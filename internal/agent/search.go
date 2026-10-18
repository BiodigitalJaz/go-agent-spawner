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

// LinkedPagesMarker heads the reference section holding the pages fetched
// from the task's own links.
const LinkedPagesMarker = "=== PAGES LINKED IN THE TASK"

const (
	maxSearchQueries = 3
	maxSearchResults = 8 // listed in the reference
	maxSearchPages   = 4 // fetched and read in full
	searchMarker     = "=== WEB SEARCH RESULTS"
)

func searchSchema() *llm.Schema {
	return &llm.Schema{Name: "search_queries", Schema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {"queries": {"type": "array", "minItems": 0, "maxItems": %d, "items": {"type": "string"}}},
  "required": ["queries"],
  "additionalProperties": false
}`, maxSearchQueries))}
}

const searchQueryDirective = `You write web search queries. Given a task, return up to 3 short keyword queries (3 to 8 words each) that would find pages with the information the task needs. Use the words such pages would contain, not the task's instructions. Do not answer the task.

If the task is about specific pages that were already fetched (listed below the task) and they are enough on their own (for example, summarizing or reviewing one page), return an empty list: searching would only add unrelated pages. Search only for information those pages do not contain.`

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
	linked := linkedPages(t.set.Context)
	user := "Task:\n" + t.node.Prompt
	if len(linked) > 0 {
		user += "\n\nPages from the task's links, already fetched:\n- " + strings.Join(linked, "\n- ")
	}
	req := request(lease, t.set.Planner, []llm.Message{
		{Role: "system", Content: searchQueryDirective},
		{Role: "user", Content: user},
	})
	req.JSONSchema = searchSchema()
	stats, err := lease.Endpoint.Client.JSON(ctx, req, &q, nil)
	lease.Done(stats, err)
	switch {
	case len(linked) > 0 && (err != nil || len(q.Queries) == 0):
		// The task's own pages are in hand: no search beats a noisy one.
		note.Write("Search skipped: the pages linked in the task cover it.\n")
		return w.reacquire(ctx, t)
	case err != nil || len(q.Queries) == 0:
		q.Queries = []string{t.node.Prompt} // the user asked to search: use the task itself
	}
	if len(q.Queries) > maxSearchQueries {
		q.Queries = q.Queries[:maxSearchQueries]
	}

	// Run the queries, then interleave their results so each query is
	// represented, dropping duplicate URLs.
	lists := make([][]fetch.SearchResult, len(q.Queries))
	notes := make([][]string, len(q.Queries))
	var wg sync.WaitGroup
	for i, query := range q.Queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i], notes[i] = w.cfg.Searcher.Search(ctx, query)
		}()
	}
	wg.Wait()
	problems := map[string]bool{}
	for _, n := range notes {
		for _, x := range n {
			problems[x] = true
		}
	}
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
	cost := 0.0
	for _, r := range results {
		cost += r.CostUSD
	}
	msg := fmt.Sprintf("Queries (%s): %s\nFound %d results.", strings.Join(w.cfg.Searcher.Names(), " + "), strings.Join(q.Queries, " · "), len(results))
	if cost > 0 {
		msg += fmt.Sprintf(" Estimated search cost: $%.4f.", cost)
	}
	note.Write(msg + "\n")
	for p := range problems {
		note.Write("Note: " + p + "\n")
	}

	// Read the top pages in full, in parallel: providers that return page
	// text (Exa) need no fetch; the rest go through the fetcher's guard.
	f := fetch.New(w.log.With("tree", t.node.TreeID, "via", "search"), t.set.AllowInternalFetch)
	pages := make([]*fetch.Page, min(len(results), maxSearchPages))
	for i := range pages {
		if strings.TrimSpace(results[i].Text) != "" {
			pages[i] = &fetch.Page{URL: results[i].URL, Title: results[i].Title, Text: results[i].Text}
			continue
		}
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
	fmt.Fprintf(&b, "%s (searched for: %s; background only, may be unrelated to any pages linked in the task) ===\n", searchMarker, strings.Join(queries, "; "))
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

// linkedPages lists the addresses of the pages fetched from the task's own
// links, from the reference's linked-pages section.
func linkedPages(reference string) []string {
	i := strings.Index(reference, LinkedPagesMarker)
	if i < 0 {
		return nil
	}
	section := reference[i:]
	if j := strings.Index(section[len(LinkedPagesMarker):], "\n=== "); j >= 0 {
		section = section[:len(LinkedPagesMarker)+j]
	}
	var out []string
	for _, line := range strings.Split(section, "\n") {
		if u, ok := strings.CutPrefix(line, "## Page fetched from "); ok {
			out = append(out, strings.TrimSpace(u))
		}
	}
	return out
}
