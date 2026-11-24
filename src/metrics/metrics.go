package metrics

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type Histogram struct {
	once   sync.Once
	bounds []time.Duration
	counts []atomic.Int64
	total  atomic.Int64
}

func (h *Histogram) Observe(v time.Duration) {
	h.once.Do(h.init)
	if v < 0 {
		v = 0
	}
	index := len(h.bounds) - 1
	for i, bound := range h.bounds {
		if v <= bound {
			index = i
			break
		}
	}
	h.counts[index].Add(1)
	h.total.Add(1)
}
func (h *Histogram) Snapshot() map[string]any {
	h.once.Do(h.init)
	total := h.total.Load()
	if total == 0 {
		return map[string]any{"count": 0}
	}
	q := func(p float64) int64 {
		rank := int64(math.Ceil(p * float64(total)))
		if rank < 1 {
			rank = 1
		}
		var seen int64
		for i := range h.counts {
			seen += h.counts[i].Load()
			if seen >= rank {
				return h.bounds[i].Microseconds()
			}
		}
		return h.bounds[len(h.bounds)-1].Microseconds()
	}
	return map[string]any{"count": total, "p50_us": q(.5), "p95_us": q(.95), "p99_us": q(.99)}
}

func (h *Histogram) init() {
	const buckets = 16
	h.bounds = make([]time.Duration, buckets)
	h.counts = make([]atomic.Int64, buckets)
	lo := math.Log(float64(time.Microsecond))
	hi := math.Log(float64(60 * time.Second))
	for i := range h.bounds {
		x := math.Exp(lo + (hi-lo)*float64(i)/float64(buckets-1))
		h.bounds[i] = time.Duration(x)
	}
}

type Counter struct {
	v atomic.Int64
}

var registry = struct {
	sync.Mutex
	h map[string]*Histogram
}{h: map[string]*Histogram{}}

func Observe(name string, v time.Duration) {
	registry.Lock()
	h := registry.h[name]
	if h == nil {
		h = &Histogram{}
		registry.h[name] = h
	}
	registry.Unlock()
	h.Observe(v)
}
func SnapshotAll() map[string]any {
	registry.Lock()
	defer registry.Unlock()
	out := map[string]any{}
	for n, h := range registry.h {
		out[n] = h.Snapshot()
	}
	return out
}

// Reset discards one named histogram. It is primarily used by benchmark
// scenarios to isolate their measurement window from earlier work in the
// same process.
func Reset(name string) {
	registry.Lock()
	delete(registry.h, name)
	registry.Unlock()
}

func (c *Counter) Add(n int64) { c.v.Add(n) }
func (c *Counter) Load() int64 { return c.v.Load() }
