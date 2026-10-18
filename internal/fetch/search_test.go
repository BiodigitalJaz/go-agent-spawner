package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExa(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/search" || r.Header.Get("x-api-key") != "k123" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"results":[{"url":"https://example.com/ember","title":"Ember Knight","text":"A warrior and his tiny dragon companion."},{"url":"","title":"no url"}],"costDollars":{"total":0.004}}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	e := NewExa(slog.New(slog.NewTextHandler(&logs, nil)), "k123")
	e.BaseURL = srv.URL
	res, err := e.Search(context.Background(), "warrior tiny dragon")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Text != "A warrior and his tiny dragon companion." || res[0].Engines[0] != "exa" || res[0].CostUSD != 0.004 {
		t.Fatalf("results = %+v", res)
	}
	if got["type"] != "instant" || got["numResults"] != float64(5) || got["contents"].(map[string]any)["text"].(map[string]any)["maxCharacters"] != float64(4000) {
		t.Fatalf("request body = %v", got)
	}
	if !strings.Contains(logs.String(), "cost_usd=0.004") {
		t.Fatalf("cost not logged: %s", logs.String())
	}
	e.Key = "wrong"
	if _, err := e.Search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("auth failure should be reported: %v", err)
	}
}

type fakeBackend struct {
	name  string
	res   []SearchResult
	err   error
	calls atomic.Int32
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Search(context.Context, string) ([]SearchResult, error) {
	f.calls.Add(1)
	return f.res, f.err
}

func TestMulti(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &fakeBackend{name: "searxng", res: []SearchResult{{URL: "https://a/1"}, {URL: "https://shared"}, {URL: "https://a/2"}}}
	b := &fakeBackend{name: "exa", res: []SearchResult{{URL: "https://shared"}, {URL: "https://b/1"}}}
	m := NewMulti(log, a, b)
	res, notes := m.Search(context.Background(), "Dragon  Warrior")
	var urls []string
	for _, r := range res {
		urls = append(urls, r.URL)
	}
	if strings.Join(urls, " ") != "https://a/1 https://shared https://b/1 https://a/2" || len(notes) != 0 {
		t.Fatalf("merged = %v notes %v", urls, notes)
	}
	// Same query (case and spacing aside): served from the cache.
	m.Search(context.Background(), "dragon warrior")
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("cache missed: %d %d", a.calls.Load(), b.calls.Load())
	}
	// A failing backend is a note, not a failure, and is not cached.
	b.err = errors.New("exa returned HTTP 402: credits exhausted")
	res, notes = m.Search(context.Background(), "another query")
	if len(res) != 3 || len(notes) != 1 || !strings.Contains(notes[0], "exa unavailable") {
		t.Fatalf("with a failing backend: %d results, notes %v", len(res), notes)
	}
	m.Search(context.Background(), "another query")
	if b.calls.Load() != 3 {
		t.Fatalf("a failed search must not be cached; exa calls = %d", b.calls.Load())
	}
}

type memBudget struct {
	spent, limit float64
	records      int
}

func (m *memBudget) Spent(context.Context) (float64, error) { return m.spent, nil }
func (m *memBudget) Record(_ context.Context, c float64) error {
	m.spent += c
	m.records++
	return nil
}
func (m *memBudget) Limit() float64 { return m.limit }

func TestExaStaysInFreeTier(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"results":[{"url":"https://example.com/a","title":"A","text":"x"}]}`)) // no costDollars
	}))
	defer srv.Close()
	e := NewExa(slog.New(slog.NewTextHandler(io.Discard, nil)), "k")
	e.BaseURL = srv.URL
	b := &memBudget{limit: 0.010}
	e.Budget = b
	// Two queries fit; the missing cost is recorded at the instant estimate.
	for i := 0; i < 2; i++ {
		if _, err := e.Search(context.Background(), "q"); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if b.records != 2 || b.spent != 2*estimatedCost {
		t.Fatalf("recorded %d, spent %.4f", b.records, b.spent)
	}
	// The third would pass the cap: refused without calling Exa.
	if _, err := e.Search(context.Background(), "q"); !errors.Is(err, ErrBudget) || calls.Load() != 2 {
		t.Fatalf("over budget: err %v, calls %d", err, calls.Load())
	}
	// Paid research types are never used.
	e.Budget, e.Type = nil, "deep"
	if _, err := e.Search(context.Background(), "q"); err == nil || calls.Load() != 2 {
		t.Fatalf("deep search must be refused: %v", err)
	}
}
