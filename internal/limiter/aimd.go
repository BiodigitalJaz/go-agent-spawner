// Package limiter implements an adaptive concurrency limit for a single
// inference backend using AIMD (additive increase, multiplicative decrease).
//
// The congestion signal is time-to-first-token. A local inference server that
// is past its parallel-slot capacity queues requests internally, which shows up
// as a TTFT spike long before throughput collapses. Independent limiters (one
// per spawner replica) that see the same signal converge on a fair share of the
// backend without coordinating, the same way TCP flows share a link.
package limiter

import (
	"math"
	"sync"
	"time"
)

type Outcome int

const (
	// OK means the request completed and its TTFT is a valid latency sample.
	OK Outcome = iota
	// Overload means the backend pushed back (429/503, connection refused,
	// timeout before first token). Treated as a strong congestion signal.
	Overload
	// Ignore releases the slot without adjusting the limit, e.g. a request
	// cancelled by the caller or rejected for a client-side reason.
	Ignore
)

type Sample struct {
	TTFT    time.Duration
	Outcome Outcome
}

type Config struct {
	Initial int
	Min     int
	Max     int
	// Tolerance is how many multiples of baseline TTFT a sample may reach
	// before it counts as congestion.
	Tolerance float64
	// LatencyBackoff and OverloadBackoff are the multiplicative decrease
	// factors for slow samples and hard failures respectively.
	LatencyBackoff  float64
	OverloadBackoff float64
	// Cooldown is the minimum time between decreases, so one burst of slow
	// responses from the same window only counts once.
	Cooldown time.Duration
	// IgnoreLatency disables the TTFT congestion signal. Use it when the
	// server's capacity is known and enforced elsewhere (shared slots): TTFT
	// then rises with batching load, not queueing, and cutting the limit
	// would only leave capacity idle. Overload errors still cut the limit.
	IgnoreLatency bool
}

func (c Config) withDefaults() Config {
	if c.Min <= 0 {
		c.Min = 1
	}
	if c.Max <= 0 {
		c.Max = 64
	}
	if c.Initial <= 0 {
		c.Initial = c.Min
	}
	c.Initial = min(max(c.Initial, c.Min), c.Max)
	if c.Tolerance <= 1 {
		c.Tolerance = 2.5
	}
	if c.LatencyBackoff <= 0 || c.LatencyBackoff >= 1 {
		c.LatencyBackoff = 0.8
	}
	if c.OverloadBackoff <= 0 || c.OverloadBackoff >= 1 {
		c.OverloadBackoff = 0.5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = time.Second
	}
	return c
}

type Limiter struct {
	cfg Config
	now func() time.Time

	mu           sync.Mutex
	limit        float64
	inflight     int
	baseline     time.Duration
	lastDecrease time.Time
}

func New(cfg Config) *Limiter {
	cfg = cfg.withDefaults()
	return &Limiter{cfg: cfg, now: time.Now, limit: float64(cfg.Initial)}
}

// TryAcquire takes a slot if one is free under the current limit.
func (l *Limiter) TryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight < l.capacity() {
		l.inflight++
		return true
	}
	return false
}

// Release returns a slot and feeds the sample into the limit.
func (l *Limiter) Release(s Sample) {
	l.mu.Lock()
	defer l.mu.Unlock()

	saturated := l.inflight >= l.capacity()
	l.inflight--

	switch s.Outcome {
	case Ignore:
		return
	case Overload:
		l.decrease(l.cfg.OverloadBackoff)
		return
	}

	if s.TTFT <= 0 {
		return
	}
	switch {
	case l.baseline == 0 || s.TTFT < l.baseline:
		l.baseline = s.TTFT
	default:
		// Drift slowly toward recent samples so the baseline follows changes in
		// prompt mix instead of pinning to one unusually fast request.
		l.baseline += (s.TTFT - l.baseline) / 50
	}

	if !l.cfg.IgnoreLatency && float64(s.TTFT) > float64(l.baseline)*l.cfg.Tolerance {
		l.decrease(l.cfg.LatencyBackoff)
		return
	}
	// Only grow when the limit was actually the constraint; otherwise an idle
	// backend would ratchet its limit up forever.
	if saturated {
		l.limit = math.Min(float64(l.cfg.Max), l.limit+1/l.limit)
	}
}

func (l *Limiter) decrease(factor float64) {
	now := l.now()
	if now.Sub(l.lastDecrease) < l.cfg.Cooldown {
		return
	}
	l.lastDecrease = now
	l.limit = math.Max(float64(l.cfg.Min), l.limit*factor)
}

func (l *Limiter) capacity() int { return int(l.limit) }

type Snapshot struct {
	Limit    float64       `json:"limit"`
	Inflight int           `json:"inflight"`
	Baseline time.Duration `json:"baseline_ttft_ns"`
}

func (l *Limiter) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Snapshot{Limit: l.limit, Inflight: l.inflight, Baseline: l.baseline}
}

// Inflight is the number of slots currently held.
func (l *Limiter) Inflight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight
}

// Headroom is the number of free slots under the current limit.
func (l *Limiter) Headroom() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.capacity() - l.inflight
}
