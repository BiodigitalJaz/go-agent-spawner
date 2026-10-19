package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

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
	maxFollowPages   = 2 // pages a read page refers to ("See Big Game Regulations")
	searchMarker     = "=== WEB SEARCH RESULTS"
)

func searchSchema(auto bool) *llm.Schema {
	if auto {
		// The decision comes first, as its own field: small models asked to
		// "return an empty list when no search is needed" searched anyway.
		return &llm.Schema{Name: "search_queries", Schema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "search_needed": {"type": "boolean"},
    "queries": {"type": "array", "minItems": 0, "maxItems": %d, "items": {"type": "string"}}
  },
  "required": ["search_needed", "queries"],
  "additionalProperties": false
}`, maxSearchQueries))}
	}
	return &llm.Schema{Name: "search_queries", Schema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {"queries": {"type": "array", "minItems": 0, "maxItems": %d, "items": {"type": "string"}}},
  "required": ["queries"],
  "additionalProperties": false
}`, maxSearchQueries))}
}

const searchQueryDirective = `You write web search queries. Given a task, return up to 3 short keyword queries (3 to 8 words each) that would find pages with the information the task needs. Use the words such pages would contain, not the task's instructions. Do not answer the task.

If the task is about specific pages that were already fetched (listed below the task) and they are enough on their own (for example, summarizing or reviewing one page), return an empty list: searching would only add unrelated pages. Search only for information those pages do not contain.`

// searchAutoRule is added when search is on "auto": the query writer also
// decides whether searching is worth it at all.
const searchAutoRule = `

Decide first whether to search at all, and say so in search_needed. Set search_needed to false (and return no queries) only for questions that are clearly general: explanations, how-tos and techniques (cooking, smoking, butchering, repairs, crafts), tips, writing, code, recipes and opinions. Set it to true when any part of the answer depends on facts that are current, local or likely to have changed: laws and regulations, seasons, licenses and permits, prices and fees, schedules, specific products or businesses, recent events, or anything the user asks to look up. If you are unsure, set it to true.`

// lookupRE spots questions whose answers depend on rules, prices, dates or
// places. On "auto" they are always searched: the small query writer
// sometimes judged them general and answered from memory.
var lookupRE = regexp.MustCompile(`(?i)\b(laws?|legal|illegal|regulations?|regs|statutes?|ordinances?|seasons?|licen[cs]es?|permits?|tags?|bag limits?|speed limits?|prices?|costs?|fees?|how much|taxe?s?|schedules?|opening hours|deadlines?|this year|next year|today|tonight|tomorrow|this week(end)?|currently|latest|right now|near me|recalls?|(19|20)\d\d|` + usStates + `)\b`)

const usStates = `alabama|alaska|arizona|arkansas|california|colorado|connecticut|delaware|florida|georgia|hawaii|idaho|illinois|indiana|iowa|kansas|kentucky|louisiana|maine|maryland|massachusetts|michigan|minnesota|mississippi|missouri|montana|nebraska|nevada|new hampshire|new jersey|new mexico|new york|north carolina|north dakota|ohio|oklahoma|oregon|pennsylvania|rhode island|south carolina|south dakota|tennessee|texas|utah|vermont|virginia|washington|west virginia|wisconsin|wyoming`

// needsLookup reports whether a message asks about rules, prices, dates or
// places (see lookupRE).
func needsLookup(prompt string) bool { return lookupRE.MatchString(prompt) }

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
		SearchNeeded *bool    `json:"search_needed"`
		Queries      []string `json:"queries"`
	}
	linked := linkedPages(t.set.Context)
	// The date keeps "this year" and "right now" queries from naming the
	// model's training year.
	user := "Today's date: " + time.Now().Format("January 2, 2006") + "\n\nTask:\n" + conversationBlock(t.set, t.node) + t.node.Prompt
	if len(linked) > 0 {
		user += "\n\nPages from the task's links, already fetched:\n- " + strings.Join(linked, "\n- ")
	}
	// On auto, a question about rules, prices, dates or places is searched
	// without asking the model; the rest it decides.
	auto := t.set.SearchAuto && !needsLookup(t.node.Prompt)
	directive := searchQueryDirective
	if auto {
		directive += searchAutoRule
	} else if t.set.SearchAuto {
		note.Write("Searching: the question involves rules, prices, dates or places.\n")
	}
	req := request(lease, t.set.Planner, []llm.Message{
		{Role: "system", Content: directive},
		{Role: "user", Content: user},
	})
	req.JSONSchema = searchSchema(auto)
	stats, err := lease.Endpoint.Client.JSON(ctx, req, &q, nil)
	lease.Done(stats, err)
	switch {
	case len(linked) > 0 && (err != nil || len(q.Queries) == 0):
		// The task's own pages are in hand: no search beats a noisy one.
		note.Write("Search skipped: the pages linked in the task cover it.\n")
		return w.reacquire(ctx, t)
	case auto && (err != nil || len(q.Queries) == 0 || (q.SearchNeeded != nil && !*q.SearchNeeded)):
		note.Write("Search not needed for this question.\n")
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
	pages = compact(pages)
	via := make([]string, len(pages))
	read := len(pages)
	if followed, from := w.followRefs(ctx, f, pages); len(followed) > 0 {
		pages, via = append(pages, followed...), append(via, from...)
		for i, p := range followed {
			note.Write(fmt.Sprintf("Also read %s (referred to by %s).\n", p.URL, from[i]))
		}
	}

	reference := searchReference(q.Queries, results, pages, via, MaxReferenceChars(w.pool.ContextLength(t.set.Worker.Model))-len(t.set.Context))
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
func searchReference(queries []string, results []fetch.SearchResult, pages []*fetch.Page, via []string, room int) string {
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
	if len(pages) == 0 {
		return b.String()
	}
	share := max((room-b.Len()-200)/len(pages), 0)
	for i, p := range pages {
		text := p.Text
		if len(text) > share {
			text = strings.ToValidUTF8(text[:max(share-60, 0)], "") + "\n[page truncated]"
		}
		fmt.Fprintf(&b, "\n## Page: %s\nTitle: %s\n", p.URL, p.Title)
		if i < len(via) && via[i] != "" {
			fmt.Fprintf(&b, "(Read because %s refers to it; it spells out rules that page only summarizes.)\n", via[i])
		}
		fmt.Fprintf(&b, "\n%s\n", text)
	}
	return b.String()
}

// compact drops the pages that could not be read.
func compact(pages []*fetch.Page) []*fetch.Page {
	out := pages[:0]
	for _, p := range pages {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// seeTextRE finds a cross-reference in page text ("See Big Game
// Regulations"), for pages whose text came from the search provider
// without their links.
var seeTextRE = regexp.MustCompile(`\b(See|see|Refer to|refer to)(\s+the)?\s+[A-Z][a-z]+`)

// followRefs reads up to maxFollowPages pages that the read pages point to
// with "See ...": a summary page often says "archery: equipment legal for
// big game, see Big Game Regulations", and without that page the model took
// the omission as a ban. It returns the pages and, for each, the page that
// referred to it.
func (w *Worker) followRefs(ctx context.Context, f *fetch.Fetcher, pages []*fetch.Page) ([]*fetch.Page, []string) {
	have := map[string]bool{}
	for _, p := range pages {
		have[p.URL] = true
	}
	// Pages whose text came from the provider (Exa) carry no links: fetch
	// them only when their text has a cross-reference.
	var wg sync.WaitGroup
	refs := make([][]string, len(pages))
	for i, p := range pages {
		refs[i] = p.Refs
		if p.Refs != nil || !seeTextRE.MatchString(p.Text) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if full, err := f.Fetch(ctx, p.URL); err == nil {
				refs[i] = full.Refs
			}
		}()
	}
	wg.Wait()
	var want, from []string
	for i, rs := range refs {
		for _, r := range rs {
			if !have[r] && len(want) < maxFollowPages {
				have[r] = true
				want, from = append(want, r), append(from, pages[i].URL)
			}
		}
	}
	got := make([]*fetch.Page, len(want))
	for i, u := range want {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := f.Fetch(ctx, u); err == nil && strings.TrimSpace(p.Text) != "" {
				got[i] = p
			} else if err != nil {
				w.log.Info("referred page not fetched", "url", u, "err", err)
			}
		}()
	}
	wg.Wait()
	var out []*fetch.Page
	var outFrom []string
	for i, p := range got {
		if p != nil {
			out, outFrom = append(out, p), append(outFrom, from[i])
		}
	}
	return out, outFrom
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
