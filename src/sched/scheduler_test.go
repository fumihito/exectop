package sched

import (
	"context"
	"exectop/src/coordinator"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunOnce(t *testing.T) {
	var n atomic.Int32
	c := coordinator.New(nil, func(_ context.Context, s spec.Spec, m runner.Meta) runner.Result {
		n.Add(1)
		return runner.Result{ID: s.ID, Status: status.OK, Start: time.Now()}
	})
	s := New(c)
	s.Start(context.Background(), []spec.Spec{{ID: "x", Interval: time.Millisecond, RunOnce: true}})
	time.Sleep(20 * time.Millisecond)
	s.Stop()
	if n.Load() != 1 {
		t.Fatal(n.Load())
	}
	s.Start(context.Background(), []spec.Spec{{ID: "x", Interval: time.Millisecond, RunOnce: true}})
	time.Sleep(10 * time.Millisecond)
	s.Stop()
	if n.Load() != 1 {
		t.Fatalf("run once repeated: %d", n.Load())
	}
}

func TestLongRunDefersNextScheduleAfterCompletion(t *testing.T) {
	var n atomic.Int32
	c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		n.Add(1)
		start := time.Now()
		time.Sleep(35 * time.Millisecond)
		return runner.Result{ID: s.ID, Status: status.OK, Start: start, End: time.Now()}
	})
	s := New(c)
	s.SetSplay(0)
	s.Start(context.Background(), []spec.Spec{{ID: "slow", Interval: time.Millisecond}})
	time.Sleep(80 * time.Millisecond)
	s.Stop()
	if got := n.Load(); got != 1 {
		t.Fatalf("long run was rescheduled before completion correction: %d", got)
	}
}

func TestStopWaitsForScheduledExecution(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		close(started)
		<-finished
		return runner.Result{ID: s.ID, Status: status.OK, Start: time.Now(), End: time.Now()}
	})
	s := New(c)
	s.SetSplay(0)
	s.Start(context.Background(), []spec.Spec{{ID: "wait", Interval: time.Millisecond}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduled execution did not start")
	}
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Stop returned while scheduled execution was running")
	case <-time.After(20 * time.Millisecond):
	}
	close(finished)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for scheduled execution")
	}
}

func TestTimeoutOverrideReachesScheduledRunner(t *testing.T) {
	got := make(chan time.Duration, 1)
	c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		got <- s.Timeout
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.OK, Start: now, End: now}
	})
	s := New(c)
	s.SetSplay(0)
	s.SetTimeout(5 * time.Millisecond)
	s.Start(context.Background(), []spec.Spec{{ID: "timeout", Interval: time.Millisecond, Timeout: time.Second}})
	defer s.Stop()
	select {
	case timeout := <-got:
		if timeout != 5*time.Millisecond {
			t.Fatalf("timeout=%v", timeout)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduled runner did not start")
	}
}
