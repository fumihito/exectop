package sched

import (
	"context"
	"exectop/src/coordinator"
	"exectop/src/metrics"
	"exectop/src/runner"
	"exectop/src/spec"
	"sync"
	"time"
)

type Scheduler struct {
	coord      *coordinator.Coordinator
	mu         sync.Mutex
	running    bool
	stopping   bool
	cancel     context.CancelFunc
	ctx        context.Context
	Overruns   map[string]int
	wg         sync.WaitGroup
	splay      time.Duration
	interval   time.Duration
	timeout    time.Duration
	inheritEnv bool
	onceDone   map[string]bool
	scheduled  map[string]bool
}

func (s *Scheduler) SetSplay(d time.Duration)    { s.mu.Lock(); s.splay = d; s.mu.Unlock() }
func (s *Scheduler) SetInterval(d time.Duration) { s.mu.Lock(); s.interval = d; s.mu.Unlock() }
func (s *Scheduler) SetTimeout(d time.Duration)  { s.mu.Lock(); s.timeout = d; s.mu.Unlock() }
func (s *Scheduler) SetInheritEnv(v bool)        { s.mu.Lock(); s.inheritEnv = v; s.mu.Unlock() }

func New(c *coordinator.Coordinator) *Scheduler {
	return &Scheduler{coord: c, Overruns: map[string]int{}, onceDone: map[string]bool{}, scheduled: map[string]bool{}}
}
func (s *Scheduler) Start(ctx context.Context, specs []spec.Spec) {
	s.mu.Lock()
	if s.running || s.stopping {
		s.mu.Unlock()
		return
	}
	s.running = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.ctx = ctx
	s.scheduled = map[string]bool{}
	for _, x := range specs {
		s.coord.Add(x)
		s.scheduled[x.ID] = true
		done := x.RunOnce && s.onceDone[x.ID]
		if done {
			continue
		}
		s.wg.Add(1)
		go s.loop(ctx, x)
	}
	s.mu.Unlock()
}

// Add starts scheduling checks discovered after the scheduler was started.
// Existing checks keep their current timers and in-flight executions.
func (s *Scheduler) Add(specs []spec.Spec) {
	s.mu.Lock()
	if !s.running || s.stopping || s.ctx == nil {
		s.mu.Unlock()
		return
	}
	ctx := s.ctx
	for _, x := range specs {
		if s.scheduled[x.ID] {
			continue
		}
		s.scheduled[x.ID] = true
		s.coord.Add(x)
		s.wg.Add(1)
		go s.loop(ctx, x)
	}
	s.mu.Unlock()
}
func (s *Scheduler) loop(ctx context.Context, x spec.Spec) {
	defer s.wg.Done()
	interval := x.Interval
	s.mu.Lock()
	if s.interval > 0 {
		interval = s.interval
	}
	s.mu.Unlock()
	if interval <= 0 {
		interval = time.Second
	}
	s.mu.Lock()
	spread := s.splay
	s.mu.Unlock()
	if spread <= 0 {
		spread = interval / 10
	}
	if spread < 0 {
		spread = 0
	}
	delay := time.Duration(0)
	if spread > 0 {
		delay = time.Duration(time.Now().UnixNano() % int64(spread+1))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	nextRun := time.Now().Add(delay)
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			fired := time.Now()
			if lag := fired.Sub(nextRun); lag > 0 {
				metrics.Observe("sched.lag", lag)
			}
			s.mu.Lock()
			inherit := s.inheritEnv
			timeout := s.timeout
			s.mu.Unlock()
			v, _ := s.coord.Request(ctx, x.ID, coordinator.Freshness{InheritEnv: inherit, Timeout: timeout}, runner.Trigger("schedule"))
			if x.RunOnce && first {
				s.mu.Lock()
				s.onceDone[x.ID] = true
				s.mu.Unlock()
				return
			}
			first = false
			nextRun = fired.Add(interval)
			if v.Cached && !v.Result.Start.IsZero() {
				nextRun = v.Result.Start.Add(interval)
			} else if !v.Result.End.IsZero() {
				end := v.Result.End.Add(100 * time.Millisecond)
				if end.After(nextRun) {
					missed := int(end.Sub(nextRun)/interval) + 1
					if missed < 1 {
						missed = 1
					}
					s.mu.Lock()
					s.Overruns[x.ID] += missed
					s.mu.Unlock()
					nextRun = end
				}
			}
			resetTimer(timer, time.Until(nextRun))
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if d < 0 {
		d = 0
	}
	t.Reset(d)
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running && s.cancel == nil {
		s.mu.Unlock()
		return
	}
	cancel := s.cancel
	s.running = false
	s.stopping = true
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	s.mu.Lock()
	s.stopping = false
	s.cancel = nil
	s.ctx = nil
	s.scheduled = map[string]bool{}
	s.mu.Unlock()
}
func (s *Scheduler) Running() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.running }

func (s *Scheduler) OverrunSnapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.Overruns))
	for id, n := range s.Overruns {
		out[id] = n
	}
	return out
}
