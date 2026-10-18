package fetch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const cacheFor = 24 * time.Hour

// Multi searches several backends at once and interleaves their results, so
// each provider is represented. A failing backend (an exhausted free quota,
// an outage) is reported, not fatal: the others still answer. Identical
// queries are cached for a day so a re-run costs nothing.
type Multi struct {
	Backends []Backend
	log      *slog.Logger
	mu       sync.Mutex
	cache    map[string]cached
	now      func() time.Time
}

type cached struct {
	at      time.Time
	results []SearchResult
	notes   []string
}

func NewMulti(log *slog.Logger, backends ...Backend) *Multi {
	return &Multi{Backends: backends, log: log, cache: map[string]cached{}, now: time.Now}
}

func (m *Multi) Names() []string {
	n := make([]string, len(m.Backends))
	for i, b := range m.Backends {
		n[i] = b.Name()
	}
	return n
}

// Search returns merged results and notes about backends that failed.
func (m *Multi) Search(ctx context.Context, query string) ([]SearchResult, []string) {
	key := strings.ToLower(strings.Join(strings.Fields(query), " "))
	m.mu.Lock()
	if c, ok := m.cache[key]; ok && m.now().Sub(c.at) < cacheFor {
		m.mu.Unlock()
		return c.results, c.notes
	}
	m.mu.Unlock()

	lists := make([][]SearchResult, len(m.Backends))
	errs := make([]error, len(m.Backends))
	var wg sync.WaitGroup
	for i, b := range m.Backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i], errs[i] = b.Search(ctx, query)
		}()
	}
	wg.Wait()
	var notes []string
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			lists[i] = nil // results that came with an error are not trusted
			m.log.Warn("search backend failed", "backend", m.Backends[i].Name(), "query", query, "err", err)
			notes = append(notes, m.Backends[i].Name()+" unavailable: "+err.Error())
		}
	}
	var out []SearchResult
	seen := map[string]bool{}
	for k := 0; ; k++ {
		added := false
		for _, l := range lists {
			if k < len(l) && !seen[l[k].URL] {
				seen[l[k].URL] = true
				out = append(out, l[k])
				added = true
			}
		}
		if !added {
			break
		}
	}
	// Cache only complete answers: a backend that failed may work next time.
	if failed == 0 && !errors.Is(ctx.Err(), context.Canceled) {
		m.mu.Lock()
		m.cache[key] = cached{at: m.now(), results: out, notes: notes}
		m.mu.Unlock()
	}
	return out, notes
}
