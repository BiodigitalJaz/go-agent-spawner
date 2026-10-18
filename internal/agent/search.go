package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

const (
	maxSearchQueries = 3
	maxSearchResults = 8 // listed in the reference
	maxSearchPages   = 4 // fetched and read in full
	searchMarker     = "=== WEB SEARCH RESULTS"
)

func searchSchema() *llm.Schema {
	return &llm.Schema{Name: "search_queries", Schema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {"queries": {"type": "array", "minItems": 1, "maxItems": %d, "items": {"type": "string"}}},
  "required": ["queries"],
  "additionalProperties": false
}`, maxSearchQueries))}
}

const searchQueryDirective = `You write web search queries. Given a task, return up to 3 short keyword queries (3 to 8 words each) that would find pages with the information the task needs. Use the words such pages would contain, not the task's instructions. Do not answer the task.`

// search runs the root's web search: the planner model writes queries,
// SearXNG returns results, the top pages are fetched (through the
// internal-address guard), and everything is added to the tree's reference
// material so every agent works from it. It runs once per tree; a retried
// job finds the results already stored. It returns a lease for the
// planning step that follows.
func (w *Worker) search(ctx context.Context, t *task, lease *backend.Lease) (*backend.Lease, error) {
	note := w.bus.Deltas(events.Thinking, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	defer note.Close()
	if strings.Contains(t.set.Context, searchMarker) {
		return lease, nil
	}
	if w.cfg.Searcher == nil {
		lease.Done(llm.Stats{}, nil)
		note.Write("Web search is not configured on this server (SEARCH_URL); continuing without it.\n")
		return w.reacquire(ctx, t)
	}
	w.publish(ctx, t.event(events.NodeStarted, RolePlan))
	note.Write("Searching the web: writing queries…\n")
	var q struct {
		Queries []string `json:"queries"`
	}
	req := request(lease, t.set.Planner, []llm.Message{
		{Role: "system", Content: searchQueryDirective},
		{Role: "user", Content: "Task:\n" + t.node.Prompt},
	})
	req.JSONSchema = searchSchema()
	stats, err := lease.Endpoint.Client.JSON(ctx, req, &q, nil)
	lease.Done(stats, err)
	if err != nil || len(q.Queries) == 0 {
		q.Queries = []string{t.node.Prompt} // fall back to the task itself
	}
	if len(q.Queries) > maxSearchQueries {
		q.Queries = q.Queries[:maxSearchQueries]
	}

	// Run the queries, then interleave their results so each query is
	// represented, dropping duplicate URLs.
	lists := make([][]fetch.SearchResult, len(q.Queries))
	var wg sync.WaitGroup
	for i, query := range q.Queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := w.cfg.Searcher.Search(ctx, query)
			if err != nil {
				w.log.Warn("search failed", "query", query, "err", err)
			}
			lists[i] = res
		}()
	}
	wg.Wait()
	var results []fetch.SearchResult
	seen := map[string]bool{}
	for k := 0; len(results) < maxSearchResults; k++ {
		added := false
		for _, l := range lists {
			if k < len(l) && !seen[l[k].URL] && len(results) < maxSearchResults {
				seen[l[k].URL] = true
				results = append(results, l[k])
				added = true
			}
		}
		if !added {
			break
		}
	}
	note.Write(fmt.Sprintf("Queries: %s\nFound %d results.\n", strings.Join(q.Queries, " · "), len(results)))

	// Read the top pages in full, in parallel; failures are skipped.
	f := fetch.New(w.log.With("tree", t.node.TreeID, "via", "search"), t.set.AllowInternalFetch)
	pages := make([]*fetch.Page, min(len(results), maxSearchPages))
	for i := range pages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := f.Fetch(ctx, results[i].URL); err == nil && strings.TrimSpace(p.Text) != "" {
				pages[i] = p
			} else if err != nil {
				w.log.Info("search result not fetched", "url", results[i].URL, "err", err)
			}
		}()
	}
	wg.Wait()

	reference := searchReference(q.Queries, results, pages, MaxReferenceChars(w.pool.ContextLength(t.set.Worker.Model))-len(t.set.Context))
	read := 0
	for _, p := range pages {
		if p != nil {
			read++
		}
	}
	note.Write(fmt.Sprintf("Read %d of them in full.\n", read))
	if t.set.Context != "" {
		reference = t.set.Context + "\n\n" + reference
	}
	if err := store.SetTreeContext(ctx, w.db, t.node.TreeID, reference); err != nil {
		return nil, err
	}
	t.set.Context = reference
	return w.reacquire(ctx, t)
}

// reacquire admits the job again for its own model, sized for the reference
// as it now is.
func (w *Worker) reacquire(ctx context.Context, t *task) (*backend.Lease, error) {
	prompt, output, err := w.estimate(ctx, t)
	if err != nil {
		return nil, err
	}
	return w.acquireRequest(ctx, t.job.Args.Model, prompt, output)
}

// searchReference formats search results for the reference material: the
// fetched pages in full (sharing the room left), then every result's title,
// address and snippet, so agents can cite sources.
func searchReference(queries []string, results []fetch.SearchResult, pages []*fetch.Page, room int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (searched for: %s) ===\n", searchMarker, strings.Join(queries, "; "))
	if len(results) == 0 {
		b.WriteString("\nThe web search found nothing. Answer from your own knowledge if you can, and say the search found nothing.\n")
		return b.String()
	}
	b.WriteString("\nResults may be unrelated to the task; use only the relevant ones and cite their addresses. If none are relevant, answer from your own knowledge and say the search found nothing useful.\n\nResults (title — address — snippet):\n")
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s — %s — %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	read := 0
	for _, p := range pages {
		if p != nil {
			read++
		}
	}
	if read == 0 {
		return b.String()
	}
	share := max((room-b.Len()-200)/read, 0)
	for _, p := range pages {
		if p == nil {
			continue
		}
		text := p.Text
		if len(text) > share {
			text = strings.ToValidUTF8(text[:max(share-60, 0)], "") + "\n[page truncated]"
		}
		fmt.Fprintf(&b, "\n## Page: %s\nTitle: %s\n\n%s\n", p.URL, p.Title, text)
	}
	return b.String()
}
