package limiter

import (
	"testing"
	"time"
)

func newTest(cfg Config) (*Limiter, *time.Time) {
	l := New(cfg)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	return l, &now
}

func fill(t *testing.T, l *Limiter) int {
	t.Helper()
	n := 0
	for l.TryAcquire() {
		n++
	}
	return n
}

func TestGrowsOnlyWhenSaturated(t *testing.T) {
	l, _ := newTest(Config{Initial: 2, Max: 8})

	// Unsaturated: one request at a time never grows the limit.
	for range 20 {
		if !l.TryAcquire() {
			t.Fatal("expected slot")
		}
		l.Release(Sample{TTFT: 100 * time.Millisecond})
	}
	if got := l.Snapshot().Limit; got != 2 {
		t.Fatalf("limit grew while idle: %v", got)
	}

	// Saturated with healthy latency: grows by roughly one per window.
	for range 20 {
		n := fill(t, l)
		for range n {
			l.Release(Sample{TTFT: 100 * time.Millisecond})
		}
	}
	if got := l.Snapshot().Limit; got <= 4 {
		t.Fatalf("expected growth under saturation, got %v", got)
	}
}

func TestDecreasesOnLatencyWithCooldown(t *testing.T) {
	l, now := newTest(Config{Initial: 8, Max: 8, Cooldown: time.Second})
	l.TryAcquire()
	l.Release(Sample{TTFT: 100 * time.Millisecond}) // establishes baseline

	l.TryAcquire()
	l.TryAcquire()
	l.Release(Sample{TTFT: time.Second})
	l.Release(Sample{TTFT: time.Second}) // inside cooldown: no second cut
	if got := l.Snapshot().Limit; got != 8*0.8 {
		t.Fatalf("expected one decrease to 6.4, got %v", got)
	}

	*now = now.Add(2 * time.Second)
	l.TryAcquire()
	l.Release(Sample{TTFT: time.Second})
	if got := l.Snapshot().Limit; got >= 6.4 {
		t.Fatalf("expected second decrease after cooldown, got %v", got)
	}
}

func TestOverloadHalvesAndRespectsMin(t *testing.T) {
	l, now := newTest(Config{Initial: 4, Min: 1, Max: 8})
	for range 5 {
		*now = now.Add(2 * time.Second)
		l.TryAcquire()
		l.Release(Sample{Outcome: Overload})
	}
	if got := l.Snapshot().Limit; got != 1 {
		t.Fatalf("expected floor at min=1, got %v", got)
	}
	if got := l.Snapshot().Inflight; got != 0 {
		t.Fatalf("inflight leaked: %d", got)
	}
}

func TestIgnoreDoesNotAdjust(t *testing.T) {
	l, _ := newTest(Config{Initial: 3, Max: 8})
	fill(t, l)
	l.Release(Sample{Outcome: Ignore})
	if got := l.Snapshot(); got.Limit != 3 || got.Inflight != 2 {
		t.Fatalf("unexpected state %+v", got)
	}
}

func TestIgnoreLatencyKeepsLimitUnderSlowSamples(t *testing.T) {
	l, now := newTest(Config{Initial: 4, Max: 4, IgnoreLatency: true})
	l.TryAcquire()
	l.Release(Sample{TTFT: 100 * time.Millisecond})
	for range 5 {
		*now = now.Add(2 * time.Second)
		fill(t, l)
		for range 4 {
			l.Release(Sample{TTFT: time.Second}) // 10x baseline
		}
	}
	if got := l.Snapshot().Limit; got != 4 {
		t.Fatalf("latency must not cut the limit, got %v", got)
	}
	*now = now.Add(2 * time.Second)
	l.TryAcquire()
	l.Release(Sample{Outcome: Overload})
	if got := l.Snapshot().Limit; got != 2 {
		t.Fatalf("overload must still halve the limit, got %v", got)
	}
}
