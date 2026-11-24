package metrics

import (
	"testing"
	"time"
)

func TestHistogramUsesFixedAtomicBuckets(t *testing.T) {
	var h Histogram
	h.Observe(time.Microsecond)
	h.Observe(time.Second)
	h.Observe(2 * time.Minute)
	s := h.Snapshot()
	if s["count"] != int64(3) {
		t.Fatalf("snapshot=%v", s)
	}
	if _, ok := s["p50_us"]; !ok {
		t.Fatalf("missing percentiles: %v", s)
	}
}

func TestCounterAtomicOperations(t *testing.T) {
	var c Counter
	c.Add(2)
	c.Add(-1)
	if got := c.Load(); got != 1 {
		t.Fatalf("counter=%d", got)
	}
}
