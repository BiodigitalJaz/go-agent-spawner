package backend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
)

func testPool(t *testing.T) *Pool {
	t.Helper()
	p, err := NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)), []ModelConfig{{
		Name: "m", Model: "api-m", InitialLimit: 1, MaxLimit: 1,
		Endpoints: []string{"http://10.0.0.1:1234", "http://10.0.0.2:1234"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAcquireSpreadsAcrossEndpointsThenSaturates(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	a, err := p.Acquire(ctx, "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Acquire(ctx, "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Endpoint == b.Endpoint {
		t.Fatal("expected leases on different endpoints")
	}
	if a.Model != "api-m" {
		t.Fatalf("lease model %q", a.Model)
	}
	if _, err := p.Acquire(ctx, "m", 20*time.Millisecond); !errors.Is(err, ErrSaturated) {
		t.Fatalf("expected ErrSaturated, got %v", err)
	}

	// A waiter wakes as soon as a slot frees.
	got := make(chan *Lease)
	go func() {
		l, _ := p.Acquire(ctx, "m", time.Second)
		got <- l
	}()
	time.Sleep(20 * time.Millisecond)
	a.Done(llm.Stats{TTFT: 50 * time.Millisecond}, nil)
	select {
	case l := <-got:
		if l == nil || l.Endpoint != a.Endpoint {
			t.Fatal("waiter should get the freed endpoint")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter not woken")
	}
}

func TestTransportFailuresEjectEndpoint(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	var bad *Endpoint
	for range ejectAfterFailures {
		l, err := p.Acquire(ctx, "m", 0)
		if err != nil {
			t.Fatal(err)
		}
		if bad == nil {
			bad = l.Endpoint
		}
		if l.Endpoint != bad {
			l.Done(llm.Stats{TTFT: time.Millisecond}, nil)
			l, _ = p.Acquire(ctx, "m", 0)
		}
		l.Done(llm.Stats{}, errors.New("dial tcp: connection refused"))
	}
	if !p.Status()["m"][0].Ejected && !p.Status()["m"][1].Ejected {
		t.Fatal("expected an endpoint to be ejected")
	}
	for range 3 {
		l, err := p.Acquire(ctx, "m", 0)
		if err != nil {
			t.Fatal(err)
		}
		if l.Endpoint == bad {
			t.Fatal("ejected endpoint was picked")
		}
		l.Done(llm.Stats{TTFT: time.Millisecond}, nil)
	}
}

func TestUnknownModelAndBadEndpoint(t *testing.T) {
	p := testPool(t)
	if _, err := p.Acquire(context.Background(), "nope", 0); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("got %v", err)
	}
	_, err := NewPool(slog.Default(), []ModelConfig{{Name: "x", Model: "x", Endpoints: []string{"dns+http://svc"}}})
	if err == nil {
		t.Fatal("expected error for endpoint without port")
	}
}

func TestSetModelsAddsRemovesAndKeepsLimiters(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	l, err := p.Acquire(ctx, "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	keep := l.Endpoint
	l.Done(llm.Stats{TTFT: 10 * time.Millisecond}, nil)

	// Same model with one endpoint dropped plus a new model: the surviving
	// endpoint keeps its limiter; the new model becomes routable.
	err = p.SetModels([]ModelConfig{
		{Name: "m", Model: "api-m", InitialLimit: 1, MaxLimit: 1, Endpoints: []string{keep.URL}},
		{Name: "new", Model: "api-new", Endpoints: []string{"http://10.0.0.9:1234"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Models(); len(got) != 2 || got[0] != "m" || got[1] != "new" {
		t.Fatalf("models %v", got)
	}
	l, err = p.Acquire(ctx, "m", 0)
	if err != nil || l.Endpoint != keep {
		t.Fatalf("expected surviving endpoint to be reused: %v", err)
	}
	l.Done(llm.Stats{}, nil)
	if l, err := p.Acquire(ctx, "new", 0); err != nil || l.Model != "api-new" {
		t.Fatalf("new model not routable: %v", err)
	}

	// Changing a model's definition rebuilds it; removing it makes it unknown.
	if err := p.SetModels([]ModelConfig{{Name: "m", Model: "api-m2", Endpoints: []string{keep.URL}}}); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.Acquire(ctx, "m", 0); l == nil || l.Endpoint == keep || l.Model != "api-m2" {
		t.Fatal("changed model should get a fresh endpoint and new API model")
	}
	if _, err := p.Acquire(ctx, "new", 0); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("removed model should be unknown, got %v", err)
	}
	if err := p.SetModels([]ModelConfig{{Name: "Bad Name", Model: "x", Endpoints: []string{"http://h:1"}}}); err == nil {
		t.Fatal("invalid name should be rejected")
	}
}

func TestSharedSlotsCapAcrossWorkers(t *testing.T) {
	p, err := NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)), []ModelConfig{{
		Name: "m", Model: "api-m", InitialLimit: 8, MaxLimit: 8, Slots: 4,
		Endpoints: []string{"http://10.0.0.1:1234"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Another worker already holds 3 of the server's 4 slots.
	p.SetPeerInflight(map[string]map[string]PeerLoad{"m": {"http://10.0.0.1:1234": {Inflight: 3}}})
	l, err := p.Acquire(ctx, "m", 0)
	if err != nil {
		t.Fatalf("one slot should remain: %v", err)
	}
	if _, err := p.Acquire(ctx, "m", 20*time.Millisecond); !errors.Is(err, ErrSaturated) {
		t.Fatalf("shared cap should block a 5th request despite local limit 8, got %v", err)
	}
	// The peer frees a slot: a waiter gets through.
	got := make(chan error, 1)
	go func() { _, err := p.Acquire(ctx, "m", time.Second); got <- err }()
	time.Sleep(20 * time.Millisecond)
	p.SetPeerInflight(map[string]map[string]PeerLoad{"m": {"http://10.0.0.1:1234": {Inflight: 2}}})
	if err := <-got; err != nil {
		t.Fatalf("waiter should acquire after peer frees a slot: %v", err)
	}
	l.Done(llm.Stats{}, nil)
}

func TestSharedContextBudget(t *testing.T) {
	p, err := NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)), []ModelConfig{{
		Name: "m", Model: "api-m", InitialLimit: 8, MaxLimit: 8, Slots: 8,
		ContextLength: 32000, SharedContext: true, // budget 28,800
		Endpoints: []string{"http://10.0.0.1:1234"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Two 12k requests fit; a third would overflow the shared pool.
	a, err := p.AcquireTokens(ctx, "m", 0, 12000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.AcquireTokens(ctx, "m", 0, 12000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AcquireTokens(ctx, "m", 20*time.Millisecond, 12000); !errors.Is(err, ErrSaturated) {
		t.Fatalf("third 12k request must wait for context, got %v", err)
	}
	// Small requests still fit alongside.
	small, err := p.AcquireTokens(ctx, "m", 0, 2000)
	if err != nil {
		t.Fatalf("2k request should fit: %v", err)
	}
	// Releasing frees tokens; a waiter gets in.
	got := make(chan *Lease, 1)
	go func() {
		l, err := p.AcquireTokens(ctx, "m", time.Second, 12000)
		if err != nil {
			t.Errorf("waiter should be admitted after release: %v", err)
		}
		got <- l
	}()
	time.Sleep(20 * time.Millisecond)
	a.Done(llm.Stats{}, nil)
	waiter := <-got
	if waiter == nil {
		t.FailNow()
	}
	defer waiter.Done(llm.Stats{}, nil)
	// Another worker's tokens count too.
	b.Done(llm.Stats{}, nil)
	small.Done(llm.Stats{}, nil)
	p.SetPeerInflight(map[string]map[string]PeerLoad{"m": {"http://10.0.0.1:1234": {Inflight: 1, Tokens: 20000}}})
	if _, err := p.AcquireTokens(ctx, "m", 20*time.Millisecond, 12000); !errors.Is(err, ErrSaturated) {
		t.Fatalf("peer tokens must count against the pool, got %v", err)
	}
	// An estimate above the whole budget waits for an idle pool rather than
	// being rejected: estimates can be high, and the server has the final say.
	p.SetPeerInflight(nil)
	waiter.Done(llm.Stats{}, nil)
	big, err := p.AcquireTokens(ctx, "m", 0, 40000)
	if err != nil {
		t.Fatalf("oversized estimate should be admitted on an idle pool: %v", err)
	}
	if _, err := p.AcquireTokens(ctx, "m", 20*time.Millisecond, 1000); !errors.Is(err, ErrSaturated) {
		t.Fatalf("an oversized request holds the whole pool, got %v", err)
	}
	big.Done(llm.Stats{}, nil)
	// Models without a shared context ignore token estimates.
	q, _ := NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)), []ModelConfig{{Name: "v", Model: "x", ContextLength: 8000, Endpoints: []string{"http://10.0.0.2:1"}}})
	if _, err := q.AcquireTokens(ctx, "v", 0, 100000); err != nil {
		t.Fatalf("per-request context models must not be budgeted: %v", err)
	}
}
