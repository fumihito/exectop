package coordinator

import (
	"context"
	"exectop/src/metrics"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"strings"
	"sync"
	"time"
)

type Freshness struct {
	Force      bool
	MaxAge     time.Duration
	InheritEnv bool
	Timeout    time.Duration
	Interval   time.Duration
}
type Served struct {
	ID           string        `json:"id,omitempty"`
	State        string        `json:"state,omitempty"`
	Result       runner.Result `json:"result"`
	Cached       bool          `json:"cached"`
	Joined       bool          `json:"joined"`
	Restored     bool          `json:"restored"`
	Age          time.Duration `json:"age_ms"`
	NextEligible time.Time     `json:"next_eligible_at"`
	Spec         *spec.Spec    `json:"spec,omitempty"`
}
type Entry struct {
	Spec      spec.Spec
	Latest    *runner.Result
	History   []HistoryPoint
	Running   bool
	RunStart  time.Time
	Iter      int
	Restored  bool
	Changed   bool
	ChangedAt time.Time
}
type HistoryPoint struct {
	Status   status.Status
	Duration time.Duration
	Start    time.Time
	ExitCode int
	Summary  string
}
type Store struct {
	mu           sync.RWMutex
	entries      map[string]*Entry
	historyLimit int
}

func NewStore() *Store { return &Store{entries: map[string]*Entry{}, historyLimit: 60} }
func (s *Store) SetHistoryLimit(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.historyLimit = n
	for _, e := range s.entries {
		if len(e.History) > n {
			e.History = e.History[len(e.History)-n:]
		}
	}
	s.mu.Unlock()
}
func (s *Store) Upsert(x spec.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[x.ID]; e != nil {
		e.Spec = x
	} else {
		s.entries[x.ID] = &Entry{Spec: x}
	}
}
func (s *Store) Remove(id string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.entries, id) }
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return Entry{}, false
	}
	x := *e
	if e.Latest != nil {
		r := *e.Latest
		x.Latest = &r
	}
	x.History = append([]HistoryPoint(nil), e.History...)
	return x, true
}
func (s *Store) Snapshot() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		x := *e
		if e.Latest != nil {
			r := *e.Latest
			x.Latest = &r
		}
		x.History = append([]HistoryPoint(nil), e.History...)
		out = append(out, x)
	}
	return out
}
func (s *Store) Restore(id string, r runner.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		r.Runbook = e.Spec.Runbook
		e.Latest = &r
		e.Iter = r.Iter
		e.Restored = true
		e.History = appendHistory(e.History, r, s.historyLimit)
	}
}
func (s *Store) SetHistory(id string, points []HistoryPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		e.History = append([]HistoryPoint(nil), points...)
		if len(e.History) > s.historyLimit {
			e.History = e.History[len(e.History)-s.historyLimit:]
		}
	}
}
func (s *Store) Start(id string, t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || e.Running {
		return false
	}
	e.Running = true
	e.RunStart = t
	e.Iter++
	return true
}
func (s *Store) Apply(r runner.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[r.ID]; e != nil {
		r.Runbook = e.Spec.Runbook
		r.Changed = false
		r.ChangedAt = time.Time{}
		e.Running = false
		e.Latest = &r
		e.Restored = false
		e.History = appendHistory(e.History, r, s.historyLimit)
	}
}
func appendHistory(h []HistoryPoint, r runner.Result, limit int) []HistoryPoint {
	h = append(h, HistoryPoint{Status: r.Status, Duration: r.Duration, Start: r.Start, ExitCode: r.ExitCode, Summary: r.Summary})
	if limit < 1 {
		limit = 60
	}
	if len(h) > limit {
		h = h[len(h)-limit:]
	}
	return h
}
func (s *Store) Abort(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		e.Running = false
	}
}

type Executor func(context.Context, spec.Spec, runner.Meta) runner.Result
type Coordinator struct {
	store    *Store
	exec     Executor
	mu       sync.Mutex
	waits    map[string][]chan Served
	subs     map[int]chan Served
	nextSub  int
	hook     func(runner.Result)
	metaHook func(*runner.Meta)
	sem      chan struct{}
}

func New(st *Store, fn Executor) *Coordinator {
	if st == nil {
		st = NewStore()
	}
	if fn == nil {
		fn = runner.Run
	}
	return &Coordinator{store: st, exec: fn, waits: map[string][]chan Served{}, subs: map[int]chan Served{}, sem: make(chan struct{}, 32)}
}
func (c *Coordinator) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	c.mu.Lock()
	c.sem = make(chan struct{}, n)
	c.mu.Unlock()
}
func (c *Coordinator) Add(s spec.Spec)                      { c.store.Upsert(s) }
func (c *Coordinator) Remove(id string)                     { c.store.Remove(id) }
func (c *Coordinator) SetResultHook(fn func(runner.Result)) { c.mu.Lock(); c.hook = fn; c.mu.Unlock() }
func (c *Coordinator) SetMetaHook(fn func(*runner.Meta))    { c.mu.Lock(); c.metaHook = fn; c.mu.Unlock() }
func (c *Coordinator) Snapshot() []Entry                    { return c.store.Snapshot() }
func (c *Coordinator) Subscribe(buffer int) (int, <-chan Served, func()) {
	if buffer < 1 {
		buffer = 32
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSub++
	id := c.nextSub
	ch := make(chan Served, buffer)
	c.subs[id] = ch
	return id, ch, func() {
		c.mu.Lock()
		if x, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(x)
		}
		c.mu.Unlock()
	}
}

func (c *Coordinator) PublishState(state, id string) {
	c.publish(Served{ID: id, State: state})
}

func (c *Coordinator) PublishSpec(state, id string, s spec.Spec) {
	c.publish(Served{ID: id, State: state, Spec: &s})
}

func (c *Coordinator) publish(v Served) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- v:
		default:
		}
	}
}
func (c *Coordinator) Request(ctx context.Context, id string, f Freshness, trig runner.Trigger) (Served, error) {
	started := time.Now()
	defer func() { metrics.Observe("coord.request", time.Since(started)) }()
	e, ok := c.store.Get(id)
	if !ok {
		return Served{}, statusError("unknown_check")
	}
	now := time.Now()
	if e.Spec.InvalidReason != "" {
		r := runner.Result{ID: id, Name: e.Spec.Name, Runbook: e.Spec.Runbook, Group: groupOf(e.Spec.ID), Tags: append([]string(nil), e.Spec.Tags...), Status: status.Error, ExitCode: -1, Reason: e.Spec.InvalidReason, Start: now, End: now, SpecHash: e.Spec.Hash}
		return Served{Result: r}, nil
	}
	if e.Running {
		ch := make(chan Served, 1)
		c.mu.Lock()
		// Completion also drains waits while holding c.mu. Re-check the
		// store while holding the same lock so a run cannot complete between
		// the initial snapshot and waiter registration.
		current, stillRunning := c.store.Get(id)
		if !stillRunning || !current.Running {
			c.mu.Unlock()
			if stillRunning && current.Latest != nil && !e.RunStart.IsZero() && !current.Latest.Start.Before(e.RunStart) {
				r := *current.Latest
				r.Joined = true
				return Served{Result: r, Joined: true}, nil
			}
			return c.Request(ctx, id, f, trig)
		}
		c.waits[id] = append(c.waits[id], ch)
		c.mu.Unlock()
		select {
		case v := <-ch:
			v.Joined = true
			return v, nil
		case <-ctx.Done():
			c.mu.Lock()
			ws := c.waits[id]
			for i, w := range ws {
				if w == ch {
					c.waits[id] = append(ws[:i], ws[i+1:]...)
					break
				}
			}
			if len(c.waits[id]) == 0 {
				delete(c.waits, id)
			}
			c.mu.Unlock()
			return Served{}, ctx.Err()
		}
	}
	if e.Latest != nil && !f.Force {
		effective := e.Spec.MinInterval
		if status.IsFailure(e.Latest.Status) {
			effective = e.Spec.MinIntervalFail
		}
		if effective > 0 && now.Sub(e.Latest.Start) < effective {
			r := *e.Latest
			r.Name = e.Spec.Name
			r.Group = groupOf(e.Spec.ID)
			r.Tags = append([]string(nil), e.Spec.Tags...)
			r.Cached = true
			r.Restored = e.Restored
			r.Age = now.Sub(r.Start)
			r.NextEligible = r.Start.Add(effective)
			if f.MaxAge > 0 && r.Age > f.MaxAge {
				r.Status = status.Unknown
				r.Reason = "cached result exceeded max age"
			}
			return Served{Result: r, Cached: true, Restored: e.Restored, Age: now.Sub(r.Start), NextEligible: r.Start.Add(effective)}, nil
		}
	}
	if !c.store.Start(id, now) {
		return c.Request(ctx, id, f, trig)
	}
	c.mu.Lock()
	for _, ch := range c.subs {
		select {
		case ch <- Served{ID: id, State: "running"}:
		default:
		}
	}
	c.mu.Unlock()
	e, _ = c.store.Get(id)
	runSpec := e.Spec
	if f.Timeout > 0 {
		runSpec.Timeout = f.Timeout
	}
	if f.Interval > 0 {
		runSpec.Interval = f.Interval
	}
	mode := "run"
	if trig == runner.Trigger("schedule") {
		mode = "watch"
	}
	m := runner.Meta{Trigger: trig, Mode: mode, Iter: e.Iter, PrevStatus: status.Status("NONE")}
	m.InheritEnv = f.InheritEnv || e.Spec.InheritEnv
	if e.Latest != nil {
		m.PrevStatus = e.Latest.Status
		m.PrevTS = e.Latest.Start
	}
	c.mu.Lock()
	mh := c.metaHook
	sem := c.sem
	c.mu.Unlock()
	if mh != nil {
		mh(&m)
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		c.store.Abort(id)
		c.mu.Lock()
		ws := c.waits[id]
		delete(c.waits, id)
		c.mu.Unlock()
		if len(ws) > 0 {
			now := time.Now()
			r := runner.Result{ID: id, Runbook: runSpec.Runbook, Status: status.Error, ExitCode: -1, Reason: "cancelled", Start: now, End: now}
			for _, w := range ws {
				w <- Served{Result: r, Joined: true}
			}
		}
		return Served{}, ctx.Err()
	}
	r := c.exec(ctx, runSpec, m)
	c.store.Apply(r)
	c.mu.Lock()
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	v := Served{Result: r}
	c.mu.Lock()
	for _, ch := range c.subs {
		select {
		case ch <- v:
		default:
		}
	}
	c.mu.Unlock()
	c.mu.Lock()
	ws := c.waits[id]
	delete(c.waits, id)
	c.mu.Unlock()
	for _, w := range ws {
		x := r
		x.Joined = true
		w <- Served{Result: x, Joined: true}
	}
	return v, nil
}

type statusError string

func (e statusError) Error() string { return string(e) }

func groupOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return ""
}
