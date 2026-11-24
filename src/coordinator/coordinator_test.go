package coordinator

import (
	"context"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"testing"
	"time"
)

func TestFreshnessAndForce(t *testing.T) {
	s := spec.Spec{ID: "x", Name: "x", MinInterval: time.Hour, MinIntervalFail: time.Minute}
	n := 0
	c := New(nil, func(_ context.Context, s spec.Spec, m runner.Meta) runner.Result {
		n++
		return runner.Result{ID: s.ID, Status: status.OK, Start: time.Now()}
	})
	c.Add(s)
	a, e := c.Request(context.Background(), "x", Freshness{}, "request")
	if e != nil || a.Cached || n != 1 {
		t.Fatal(a, e, n)
	}
	b, e := c.Request(context.Background(), "x", Freshness{}, "request")
	if e != nil || !b.Cached || n != 1 {
		t.Fatal(b, e, n)
	}
	_, _ = c.Request(context.Background(), "x", Freshness{Force: true}, "request")
	if n != 2 {
		t.Fatal(n)
	}
}

func TestUnknownUsesFailureMinimumInterval(t *testing.T) {
	c := New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.Unknown, Start: now, End: now}
	})
	c.Add(spec.Spec{ID: "unknown", Name: "unknown", MinInterval: time.Hour, MinIntervalFail: time.Minute})
	if _, err := c.Request(context.Background(), "unknown", Freshness{Force: true}, "request"); err != nil {
		t.Fatal(err)
	}
	v, err := c.Request(context.Background(), "unknown", Freshness{}, "request")
	if err != nil || !v.Cached || v.NextEligible.Sub(v.Result.Start) != time.Minute {
		t.Fatalf("unknown freshness=%+v err=%v", v, err)
	}
}

func TestInvalidSpecDoesNotReturnOldCachedResult(t *testing.T) {
	c := New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.OK, Start: now, End: now}
	})
	c.Add(spec.Spec{ID: "x", Name: "x"})
	if _, err := c.Request(context.Background(), "x", Freshness{Force: true}, "request"); err != nil {
		t.Fatal(err)
	}
	c.Add(spec.Spec{ID: "x", Name: "x", InvalidReason: "bad config", Hash: "new"})
	v, err := c.Request(context.Background(), "x", Freshness{}, "request")
	if err != nil || v.Result.Status != status.Error || v.Result.Reason != "bad config" {
		t.Fatalf("result=%+v err=%v", v, err)
	}
}

func TestConcurrentJoinCannotMissCompletion(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c := New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		close(started)
		<-release
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.OK, Start: now, End: now}
	})
	c.Add(spec.Spec{ID: "x", Name: "x", MinInterval: time.Hour})
	first := make(chan error, 1)
	go func() {
		_, e := c.Request(context.Background(), "x", Freshness{Force: true}, "request")
		first <- e
	}()
	<-started
	const joins = 100
	done := make(chan error, joins)
	for i := 0; i < joins; i++ {
		go func() {
			_, e := c.Request(context.Background(), "x", Freshness{}, "request")
			done <- e
		}()
	}
	close(release)
	if e := <-first; e != nil {
		t.Fatal(e)
	}
	for i := 0; i < joins; i++ {
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(time.Second):
			t.Fatal("join waiter hung")
		}
	}
}

func TestSubscribeReceivesRunningAndCompletion(t *testing.T) {
	c := New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.OK, Start: now, End: now}
	})
	c.Add(spec.Spec{ID: "x", Name: "x"})
	_, ch, closeSub := c.Subscribe(4)
	defer closeSub()
	if _, err := c.Request(context.Background(), "x", Freshness{Force: true}, "request"); err != nil {
		t.Fatal(err)
	}
	var running, complete bool
	for i := 0; i < 2; i++ {
		select {
		case v := <-ch:
			running = running || v.State == "running"
			complete = complete || v.Result.Status == status.OK
		case <-time.After(time.Second):
			t.Fatal("subscription timed out")
		}
	}
	if !running || !complete {
		t.Fatalf("running=%v complete=%v", running, complete)
	}
}

func TestStoreLifecycleAndHistoryRing(t *testing.T) {
	s := NewStore()
	s.SetHistoryLimit(2)
	x := spec.Spec{ID: "x", Name: "one"}
	s.Upsert(x)
	x.Name = "two"
	s.Upsert(x)
	if got, ok := s.Get("x"); !ok || got.Spec.Name != "two" {
		t.Fatalf("get=%+v ok=%v", got, ok)
	}
	if _, ok := s.Get("missing"); ok || len(s.Snapshot()) != 1 {
		t.Fatal("store lookup")
	}
	if s.Start("missing", time.Now()) || !s.Start("x", time.Now()) || s.Start("x", time.Now()) {
		t.Fatal("start state")
	}
	s.Abort("x")
	for i := 0; i < 65; i++ {
		s.Apply(runner.Result{ID: "x", Status: status.OK, Start: time.Now(), Iter: i})
	}
	if got, _ := s.Get("x"); got.Running || len(got.History) != 2 {
		t.Fatalf("entry=%+v", got)
	}
	s.Restore("x", runner.Result{ID: "x", Status: status.Warn, Iter: 66, Start: time.Now()})
	if got, _ := s.Get("x"); !got.Restored || got.Iter != 66 {
		t.Fatalf("restore=%+v", got)
	}
	points := make([]HistoryPoint, 70)
	s.SetHistory("x", points)
	if got, _ := s.Get("x"); len(got.History) != 2 {
		t.Fatalf("history=%d", len(got.History))
	}
	s.SetHistory("missing", nil)
	s.Apply(runner.Result{ID: "missing"})
	s.Abort("missing")
	s.Remove("x")
	if _, ok := s.Get("x"); ok {
		t.Fatal("remove")
	}
}

func TestStoreHistoryLimitClampAndPublishState(t *testing.T) {
	s := NewStore()
	s.SetHistoryLimit(0)
	s.Upsert(spec.Spec{ID: "x"})
	for i := 0; i < 2; i++ {
		s.Apply(runner.Result{ID: "x", Status: status.OK, Iter: i})
	}
	if got, _ := s.Get("x"); len(got.History) != 1 {
		t.Fatalf("clamped history=%d", len(got.History))
	}
	c := New(s, nil)
	_, ch, closeSub := c.Subscribe(1)
	defer closeSub()
	c.PublishState("running", "x")
	select {
	case v := <-ch:
		if v.State != "running" || v.ID != "x" {
			t.Fatalf("state=%+v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("state notification timed out")
	}
}

func TestStatusErrorMessage(t *testing.T) {
	if got := statusError("bad").Error(); got != "bad" {
		t.Fatalf("error=%q", got)
	}
}

func TestCoordinatorHooksFreshnessMaxAgeAndCancellation(t *testing.T) {
	var meta runner.Meta
	var hooked runner.Result
	c := New(nil, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result {
		meta = m
		select {
		case <-ctx.Done():
			return runner.Result{ID: s.ID, Status: status.Error, Reason: "cancelled"}
		default:
		}
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.Fail, Start: now.Add(-time.Second), End: now, Iter: m.Iter}
	})
	c.SetConcurrency(0)
	c.SetMetaHook(func(m *runner.Meta) { meta = *m })
	c.SetResultHook(func(r runner.Result) { hooked = r })
	c.Add(spec.Spec{ID: "x", Name: "x", MinInterval: time.Hour, MinIntervalFail: time.Minute, InheritEnv: true})
	if _, err := c.Request(context.Background(), "missing", Freshness{}, "request"); err == nil {
		t.Fatal("unknown check")
	}
	v, err := c.Request(context.Background(), "x", Freshness{Force: true, InheritEnv: true}, "schedule")
	if err != nil || v.Result.Status != status.Fail || hooked.ID != "x" || meta.Mode != "watch" || !meta.InheritEnv {
		t.Fatalf("v=%+v meta=%+v hook=%+v", v, meta, hooked)
	}
	v, err = c.Request(context.Background(), "x", Freshness{MaxAge: time.Nanosecond}, "request")
	if err != nil || !v.Cached || v.Result.Status != status.Unknown {
		t.Fatalf("max age=%+v err=%v", v, err)
	}
	_, ch, cleanup := c.Subscribe(1)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	c.store.Start("x", time.Now())
	done := make(chan error, 1)
	go func() { _, e := c.Request(ctx, "x", Freshness{}, "request"); done <- e }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancel did not return")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel timeout")
	}
	if len(c.waits["x"]) != 0 {
		t.Fatal("cancelled waiter retained")
	}
	_ = ch
}
