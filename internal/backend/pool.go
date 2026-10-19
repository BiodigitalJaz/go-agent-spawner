// Package backend routes inference requests to concrete backend endpoints.
//
// Each model has a set of endpoints (pods behind a headless Service, or static
// hosts such as LM Studio workstations). Every endpoint has its own adaptive
// limiter, and Acquire picks the endpoint with the most headroom. This is the
// reason not to go through a ClusterIP Service: kube-proxy balances blindly and
// would hide which pod is saturated.
package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/limiter"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
)

var (
	ErrSaturated    = errors.New("backend: all endpoints saturated")
	ErrUnknownModel = errors.New("backend: unknown model")
)

// sharedContextMargin keeps the admitted token total under the context to
// absorb estimation error.
const sharedContextMargin = 0.9

// ModelConfig describes one routable model. Name is the routing key used for
// River queues ("llm_<name>"); Model is the identifier sent to the API.
type ModelConfig struct {
	Name      string   `json:"name"`
	Model     string   `json:"model"`
	Endpoints []string `json:"endpoints"` // http://host:port or dns+http://headless-svc:port
	APIKey    string   `json:"api_key,omitempty"`
	// Extra is merged into every request body for this model.
	Extra        map[string]any `json:"extra,omitempty"`
	InitialLimit int            `json:"initial_limit,omitempty"`
	MaxLimit     int            `json:"max_limit,omitempty"`
	// ContextLength is the context the server loaded the model with. Merges
	// are sized to fit it; 0 means unknown (a conservative default is used).
	ContextLength int `json:"context_length,omitempty"`
	// Slots is the server's parallel capacity for this model, shared by every
	// worker replica. 0 means no shared cap (each worker's limiter decides).
	Slots int `json:"slots,omitempty"`
	// SharedContext means ContextLength is one pool shared by all concurrent
	// requests (LM Studio / llama.cpp unified KV cache), not a per-request
	// limit. Requests are then admitted only while their estimated tokens fit
	// the pool, across all workers.
	SharedContext bool `json:"shared_context,omitempty"`
	// ChunkTokens is the model's safe chunk size: the largest document
	// chunk (estimated tokens, chars/4) it reads without missing facts.
	// Automatic chunking never makes chunks larger; 0 means the default
	// rule. Measure it with experiments/chunking/chunk_size_sweep.py.
	ChunkTokens int `json:"chunk_tokens,omitempty"`
}

// NameRE constrains routing names: they become River queue names ("llm_<name>").
var NameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,55}$`)

// Validate checks a model definition without building it.
func (c ModelConfig) Validate() error {
	if !NameRE.MatchString(c.Name) {
		return fmt.Errorf("backend: model name %q must match %s", c.Name, NameRE)
	}
	if c.Model == "" || len(c.Endpoints) == 0 {
		return fmt.Errorf("backend: model %q needs a model id and at least one endpoint", c.Name)
	}
	for _, raw := range c.Endpoints {
		u, err := url.Parse(strings.TrimPrefix(raw, "dns+"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Port() == "" {
			return fmt.Errorf("backend: model %q endpoint %q must be http(s)://host:port", c.Name, raw)
		}
	}
	if c.MaxLimit < 0 || c.InitialLimit < 0 || (c.MaxLimit > 0 && c.InitialLimit > c.MaxLimit) {
		return fmt.Errorf("backend: model %q limits must satisfy 0 <= initial <= max", c.Name)
	}
	if c.ContextLength < 0 || c.Slots < 0 {
		return fmt.Errorf("backend: model %q context_length and slots must be >= 0", c.Name)
	}
	return nil
}

type Endpoint struct {
	URL    string
	Client *llm.Client
	lim    *limiter.Limiter

	slotMu sync.Mutex // serializes acquire + shared-slot/context checks
	tokens int        // estimated tokens of this worker's in-flight requests

	mu          sync.Mutex
	failures    int
	ejectedTill time.Time
}

const (
	ejectAfterFailures = 3
	ejectFor           = 5 * time.Second
)

func (e *Endpoint) available(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.After(e.ejectedTill)
}

// recordTransport ejects an endpoint for a short time after repeated
// connection-level failures (pod gone, port closed) so Acquire stops picking it.
func (e *Endpoint) recordTransport(failed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !failed {
		e.failures = 0
		return
	}
	e.failures++
	if e.failures >= ejectAfterFailures {
		e.ejectedTill = time.Now().Add(ejectFor)
		e.failures = 0
	}
}

type model struct {
	cfg ModelConfig // immutable; a changed definition gets a new model

	mu        sync.RWMutex
	spec      []string             // configured endpoints (may change at runtime)
	endpoints map[string]*Endpoint // resolved, keyed by base URL

	ratio tokenRatio // learned real/estimated prompt tokens
}

// Prompt estimates (chars/4) run low for dense text: book text measured 1.9-3.4
// chars per token, so admitted requests overflowed a shared context. Each
// response's real prompt_tokens calibrates the estimate per model. The ratio
// used is a moving mean plus one deviation: cautious about dense text without
// letting one outlier serialize the pool (taking the maximum did, at 2.45 on
// text that averaged 1.2). An overflow nudges the mean up; the rare overflow
// that remains is retried inside the job.
const (
	minTokenRatio  = 1.0
	maxTokenRatio  = 3.0
	ratioAlpha     = 0.2  // weight of each new observation
	pressureBump   = 1.05 // an overflow means the estimate was still low
	coldBump       = 1.25 // before any real count, an overflow is the only evidence
	ratioMinPrompt = 256  // tiny prompts are dominated by fixed overhead
)

type tokenRatio struct {
	mu        sync.Mutex
	n         int
	mean, dev float64 // moving mean and variance of real/estimated
}

func (r *tokenRatio) value() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return max(r.mean, minTokenRatio)
	}
	return min(max(r.mean+math.Sqrt(r.dev), minTokenRatio), maxTokenRatio)
}

func (r *tokenRatio) observe(estimated, actual int) {
	if estimated < ratioMinPrompt || actual <= 0 {
		return
	}
	x := min(max(float64(actual)/float64(estimated), 0.5), maxTokenRatio)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		r.mean, r.dev = max(r.mean, x), 0
	} else {
		d := x - r.mean
		r.mean += ratioAlpha * d
		r.dev = (1 - ratioAlpha) * (r.dev + ratioAlpha*d*d)
	}
	r.n++
}

func (r *tokenRatio) overflowed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	bump := pressureBump
	if r.n == 0 {
		bump = coldBump
	}
	r.mean = min(max(r.mean, minTokenRatio)*bump, maxTokenRatio)
}

func (m *model) tokenRatio() float64 { return m.ratio.value() }

// TokenRatio reports the learned real/estimated prompt-token ratio (1 until
// a response has been seen).
func (p *Pool) TokenRatio(name string) float64 {
	m, ok := p.get(name)
	if !ok {
		return minTokenRatio
	}
	return m.tokenRatio()
}

func (m *model) endpointSpec() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spec
}

// Pool holds every routable model and wakes waiters when capacity frees up.
// The model set can change at runtime (models added from the console).
type Pool struct {
	log *slog.Logger

	modelsMu sync.RWMutex
	models   map[string]*model

	// peers is other workers' in-flight load per model and endpoint URL, from
	// their heartbeats; used to enforce Slots and SharedContext pool-wide.
	peerMu sync.RWMutex
	peers  map[string]map[string]PeerLoad

	mu      sync.Mutex
	changed chan struct{}
}

func NewPool(log *slog.Logger, cfgs []ModelConfig) (*Pool, error) {
	p := &Pool{log: log, models: map[string]*model{}, changed: make(chan struct{})}
	if err := p.SetModels(cfgs); err != nil {
		return nil, err
	}
	return p, nil
}

// SetModels replaces the model set. Unchanged models keep their endpoints and
// learned limits; changed ones (different model id, limits, key or extra
// params) are rebuilt; removed ones stop accepting new leases while in-flight
// leases finish on the endpoints they already hold.
func (p *Pool) SetModels(cfgs []ModelConfig) error {
	seen := map[string]bool{}
	for _, c := range cfgs {
		if err := c.Validate(); err != nil {
			return err
		}
		if seen[c.Name] {
			return fmt.Errorf("backend: duplicate model %q", c.Name)
		}
		seen[c.Name] = true
	}

	p.modelsMu.Lock()
	for name := range p.models {
		if !seen[name] {
			delete(p.models, name)
			p.log.Info("backend model removed", "model", name)
		}
	}
	var refresh []*model
	for _, c := range cfgs {
		m, ok := p.models[c.Name]
		if ok && !sameExceptEndpoints(m.cfg, c) {
			ok = false
		}
		if !ok {
			m = &model{cfg: c, endpoints: map[string]*Endpoint{}}
			p.models[c.Name] = m
			p.log.Info("backend model configured", "model", c.Name, "api_model", c.Model)
		}
		m.mu.Lock()
		m.spec = slices.Clone(c.Endpoints)
		m.mu.Unlock()
		refresh = append(refresh, m)
	}
	p.modelsMu.Unlock()

	for _, m := range refresh {
		var static []string
		dynamic := false
		for _, raw := range m.endpointSpec() {
			if strings.HasPrefix(raw, "dns+") {
				dynamic = true
			} else {
				static = append(static, raw)
			}
		}
		if !dynamic {
			p.setEndpoints(m, static)
		} else {
			// Resolve now rather than waiting for the next discovery tick.
			var r net.Resolver
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			p.resolve(ctx, &r, m)
			cancel()
		}
	}
	p.signal()
	return nil
}

func sameExceptEndpoints(a, b ModelConfig) bool {
	a.Endpoints, b.Endpoints = nil, nil
	return reflect.DeepEqual(a, b)
}

func (p *Pool) get(name string) (*model, bool) {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	m, ok := p.models[name]
	return m, ok
}

func (p *Pool) snapshot() map[string]*model {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	out := make(map[string]*model, len(p.models))
	for k, v := range p.models {
		out[k] = v
	}
	return out
}

func (p *Pool) Models() []string {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	names := make([]string, 0, len(p.models))
	for n := range p.models {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func (p *Pool) Has(name string) bool { _, ok := p.get(name); return ok }

func (p *Pool) setEndpoints(m *model, urls []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, u := range urls {
		want[strings.TrimSuffix(u, "/")] = true
	}
	for u := range m.endpoints {
		if !want[u] {
			delete(m.endpoints, u) // in-flight leases keep their own pointer
			p.log.Info("backend endpoint removed", "model", m.cfg.Name, "url", u)
		}
	}
	for u := range want {
		if _, ok := m.endpoints[u]; ok {
			continue
		}
		cl := llm.New(u)
		cl.APIKey = m.cfg.APIKey
		m.endpoints[u] = &Endpoint{
			URL:    u,
			Client: cl,
			lim: limiter.New(limiter.Config{
				Initial: m.cfg.InitialLimit, Max: m.cfg.MaxLimit,
				// With known server slots (enforced across workers), latency
				// rises with batching, not queueing; only errors signal overload.
				IgnoreLatency: m.cfg.Slots > 0,
			}),
		}
		p.log.Info("backend endpoint added", "model", m.cfg.Name, "url", u)
	}
	p.signal()
}

// Lease is a reserved slot on one endpoint. Done must be called exactly once.
type Lease struct {
	Endpoint *Endpoint
	Model    string // API model identifier
	Extra    map[string]any
	tokens   int // reserved from the endpoint's shared context
	pool     *Pool
	once     sync.Once
	model    *model
	prompt   int // estimated prompt tokens before calibration, for learning
}

// PeerLoad is another worker's in-flight load on one endpoint.
type PeerLoad struct {
	Inflight int
	Tokens   int
}

// Done releases the slot and feeds the outcome into the endpoint's limiter.
func (l *Lease) Done(stats llm.Stats, err error) {
	l.once.Do(func() {
		s := limiter.Sample{TTFT: stats.TTFT}
		transport := false
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded) && stats.TTFT > 0:
			// Caller gave up, or a long generation hit the job timeout after it
			// started streaming. Neither says anything about backend load.
			s.Outcome = limiter.Ignore
		case llm.Overloaded(err):
			s.Outcome = limiter.Overload
			var se *llm.StatusError
			transport = !errors.As(err, &se)
		default:
			s.Outcome = limiter.Ignore // request-specific failure says nothing about load
		}
		if err == nil && stats.TTFT == 0 {
			s.Outcome = limiter.Ignore // empty response, no latency sample
		}
		l.Endpoint.lim.Release(s)
		if l.tokens > 0 {
			l.Endpoint.slotMu.Lock()
			l.Endpoint.tokens -= l.tokens
			l.Endpoint.slotMu.Unlock()
		}
		l.Endpoint.recordTransport(transport)
		if l.model != nil {
			if llm.ContextPressure(err) {
				l.model.ratio.overflowed()
			} else if err == nil {
				l.model.ratio.observe(l.prompt, stats.PromptTokens)
			}
		}
		l.pool.signal()
	})
}

// Acquire reserves a slot on the least-loaded endpoint for the named model,
// waiting up to wait for capacity. It returns ErrSaturated when the wait
// elapses, so the caller can hand the task back to the queue for another
// replica instead of holding a worker slot.
func (p *Pool) Acquire(ctx context.Context, name string, wait time.Duration) (*Lease, error) {
	return p.AcquireTokens(ctx, name, wait, 0)
}

// AcquireRequest is AcquireTokens for a request whose prompt is estimated at
// prompt tokens and which reserves output tokens. The prompt estimate is
// scaled by the model's learned ratio, and the response's real prompt size
// refines that ratio.
func (p *Pool) AcquireRequest(ctx context.Context, name string, wait time.Duration, prompt, output int) (*Lease, error) {
	m, ok := p.get(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, name)
	}
	tokens := int(float64(prompt)*m.tokenRatio()+0.5) + output
	lease, err := p.AcquireTokens(ctx, name, wait, tokens)
	if lease != nil {
		lease.model, lease.prompt = m, prompt
	}
	return lease, err
}

// AcquireTokens is Acquire for a request of about tokens tokens (prompt plus
// reserved output). For models with a shared context it also reserves those
// tokens from the pool; 0 skips the context check.
func (p *Pool) AcquireTokens(ctx context.Context, name string, wait time.Duration, tokens int) (*Lease, error) {
	m, ok := p.get(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, name)
	}
	if !m.cfg.SharedContext {
		tokens = 0
	}
	// Estimates are approximate, so an apparently oversized request is not
	// rejected here: it waits for the whole pool to be free and the server,
	// which counts real tokens, decides (a truly oversized request fails fast
	// with a permanent 400).
	if budget := sharedBudget(m.cfg); budget > 0 && tokens > budget {
		tokens = budget
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		ch := p.waitCh()
		if ep := p.pick(m, tokens); ep != nil {
			return &Lease{Endpoint: ep, Model: m.cfg.Model, Extra: m.cfg.Extra, tokens: tokens, pool: p}, nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil, ErrSaturated
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func sharedBudget(c ModelConfig) int {
	if !c.SharedContext || c.ContextLength <= 0 {
		return 0
	}
	return int(float64(c.ContextLength) * sharedContextMargin)
}

func (p *Pool) pick(m *model, tokens int) *Endpoint {
	m.mu.RLock()
	candidates := make([]*Endpoint, 0, len(m.endpoints))
	now := time.Now()
	for _, ep := range m.endpoints {
		if ep.available(now) {
			candidates = append(candidates, ep)
		}
	}
	m.mu.RUnlock()

	// Most free slots first; ties go to the endpoint with the faster baseline.
	slices.SortFunc(candidates, func(a, b *Endpoint) int {
		ha, hb := a.lim.Headroom(), b.lim.Headroom()
		if ha != hb {
			return hb - ha
		}
		return int(a.lim.Snapshot().Baseline - b.lim.Snapshot().Baseline)
	})
	for _, ep := range candidates {
		if p.tryAcquire(m, ep, tokens) {
			return ep
		}
	}
	return nil
}

// tryAcquire takes a slot under the endpoint's own limit and, when the model
// declares server Slots or a SharedContext, only if this worker plus its
// peers stay within them.
func (p *Pool) tryAcquire(m *model, ep *Endpoint, tokens int) bool {
	budget := sharedBudget(m.cfg)
	if m.cfg.Slots <= 0 && (budget == 0 || tokens == 0) {
		return ep.lim.TryAcquire()
	}
	ep.slotMu.Lock()
	defer ep.slotMu.Unlock()
	peer := p.peerLoad(m.cfg.Name, ep.URL)
	if m.cfg.Slots > 0 && ep.lim.Inflight()+peer.Inflight >= m.cfg.Slots {
		return false
	}
	if budget > 0 && tokens > 0 {
		used := ep.tokens + peer.Tokens
		// An idle pool always admits one request that fits the budget.
		if used > 0 && used+tokens > budget {
			return false
		}
	}
	if !ep.lim.TryAcquire() {
		return false
	}
	ep.tokens += tokens
	return true
}

func (p *Pool) peerLoad(model, url string) PeerLoad {
	p.peerMu.RLock()
	defer p.peerMu.RUnlock()
	return p.peers[model][url]
}

// SetPeerInflight replaces the view of other workers' in-flight load
// (model -> endpoint URL -> load) and wakes waiters, since capacity may have
// freed up.
func (p *Pool) SetPeerInflight(peers map[string]map[string]PeerLoad) {
	p.peerMu.Lock()
	p.peers = peers
	p.peerMu.Unlock()
	p.signal()
}

// ContextLength returns the model's configured context, or 0 if unknown.
func (p *Pool) ContextLength(name string) int {
	if m, ok := p.get(name); ok {
		return m.cfg.ContextLength
	}
	return 0
}

func (p *Pool) waitCh() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (p *Pool) signal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.changed)
	p.changed = make(chan struct{})
}

type EndpointStatus struct {
	URL     string           `json:"url"`
	Ejected bool             `json:"ejected"`
	Limiter limiter.Snapshot `json:"limiter"`
	Tokens  int              `json:"tokens_inflight"` // estimated, shared-context models only
}

// Status reports the live limiter state of every endpoint, keyed by model.
func (p *Pool) Status() map[string][]EndpointStatus {
	out := map[string][]EndpointStatus{}
	now := time.Now()
	for name, m := range p.snapshot() {
		m.mu.RLock()
		for _, ep := range m.endpoints {
			ep.slotMu.Lock()
			tokens := ep.tokens
			ep.slotMu.Unlock()
			out[name] = append(out[name], EndpointStatus{URL: ep.URL, Ejected: !ep.available(now), Limiter: ep.lim.Snapshot(), Tokens: tokens})
		}
		m.mu.RUnlock()
		slices.SortFunc(out[name], func(a, b EndpointStatus) int { return strings.Compare(a.URL, b.URL) })
	}
	return out
}

// Discover resolves dns+ endpoints every interval until ctx is done. With a
// headless Service, DNS returns one A record per ready pod, so pods that fail
// readiness (e.g. still loading the model) drop out automatically.
func (p *Pool) Discover(ctx context.Context, interval time.Duration) {
	var r net.Resolver
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		for _, m := range p.snapshot() {
			p.resolve(ctx, &r, m)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *Pool) resolve(ctx context.Context, r *net.Resolver, m *model) {
	var urls []string
	dynamic := false
	for _, raw := range m.endpointSpec() {
		rest, ok := strings.CutPrefix(raw, "dns+")
		if !ok {
			urls = append(urls, raw)
			continue
		}
		dynamic = true
		u, err := url.Parse(rest)
		if err != nil {
			p.log.Error("bad endpoint", "model", m.cfg.Name, "endpoint", raw, "err", err)
			continue
		}
		ips, err := r.LookupHost(ctx, u.Hostname())
		if err != nil {
			// Keep the current set on transient DNS failure rather than
			// dropping every endpoint.
			p.log.Warn("dns lookup failed", "model", m.cfg.Name, "host", u.Hostname(), "err", err)
			return
		}
		for _, ip := range ips {
			urls = append(urls, fmt.Sprintf("%s://%s", u.Scheme, net.JoinHostPort(ip, u.Port())))
		}
	}
	if dynamic {
		p.setEndpoints(m, urls)
	}
}
