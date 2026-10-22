package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// linkedPagesMarker heads the pages fetched from the task's own links; the
// search step looks for it (agent.LinkedPagesMarker).
const linkedPagesMarker = agent.LinkedPagesMarker

var urlRE = regexp.MustCompile(`https?://[^\s<>"'(){}\[\]]+`)

// promptURLs returns the distinct web links in a prompt.
func promptURLs(prompt string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range urlRE.FindAllString(prompt, -1) {
		u = strings.TrimRight(u, ".,;:!?*_")
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

type fetchedPage struct {
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	Chars     int    `json:"chars"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"` // soft fetch: why the page could not be opened
}

type requestError struct {
	status int
	msg    string
}

func (e *requestError) Error() string { return e.msg }

// reference builds a tree's reference material from the request's context
// and, when fetch_urls is on, the pages its prompt links to. A prompt that
// links to pages it neither supplies nor fetches is refused: agents cannot
// browse, so they would describe pages they never saw.
// reference caps the material at maxChars: every agent receives it, so it
// competes with each request's own input for the model's context.
func (s *Server) reference(ctx context.Context, req *createRequest, set settings.Settings, remote string, maxChars int) (string, []fetchedPage, error) {
	userCtx := strings.TrimSpace(req.Context)
	if len(userCtx) > maxChars {
		return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf(
			"context is %d characters; the limit for this model is %d (40%% of its context) because every agent receives it. Send large material as document or parts", len(userCtx), maxChars)}
	}
	// The user's documents come first: the conversation is about them.
	docs := documentsSection(req.documents)
	withDocs := func(rest string) string {
		if docs == "" {
			return rest
		}
		return strings.TrimSpace(docs + "\n\n" + rest)
	}
	maxChars -= len(docs)
	urls := promptURLs(req.Prompt)
	if len(urls) == 0 {
		return withDocs(userCtx), nil, nil
	}
	if !req.FetchURLs {
		if userCtx == "" && req.Document == "" && len(req.Parts) == 0 && !req.Search {
			return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf(
				"the prompt links to %s, but agents cannot open web pages. Turn on fetch_urls (\"Fetch linked pages\" in the console) or pass the content as context or document", urls[0])}
		}
		return withDocs(userCtx), nil, nil
	}
	if len(urls) > fetch.MaxURLs {
		return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf("the prompt links to %d pages; at most %d can be fetched per run", len(urls), fetch.MaxURLs)}
	}
	f := fetch.New(s.log.With("fetch_requested_by", remote), set.AllowInternalFetch)
	pages := make([]*fetch.Page, len(urls))
	var failed []fetchedPage
	for i, u := range urls {
		p, err := f.Fetch(ctx, u)
		if err == nil && strings.TrimSpace(p.Text) == "" {
			err = errors.New("no readable text (it may be rendered by JavaScript)")
		}
		if err != nil && req.softFetch {
			// Chat: a dead link should not stop the conversation. The agent
			// is told which links failed and why, and says so.
			failed = append(failed, fetchedPage{URL: u, Error: err.Error()})
			continue
		}
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, fetch.ErrInternal) {
				status = http.StatusForbidden
			}
			return "", nil, &requestError{status, fmt.Sprintf("could not fetch %s: %v", u, err)}
		}
		if strings.TrimSpace(p.Text) == "" {
			return "", nil, &requestError{http.StatusBadGateway, fmt.Sprintf(
				"%s has no readable text (it may be rendered by JavaScript). Paste its content as context instead", u)}
		}
		pages[i] = p
	}
	ok := pages[:0]
	for _, p := range pages {
		if p != nil {
			ok = append(ok, p)
		}
	}
	pages = ok
	// Share what room is left after the caller's own context between pages.
	// With both present, label their roles: the pages are what the task is
	// about, the supplied text is what the answer must respect (without the
	// labels, models reviewed the guidelines instead of the page).
	room := (maxChars - len(userCtx) - 200) / max(len(pages), 1)
	// Linked pages are always labelled: they are what the task is about, and
	// a later web search adds background that must not be mistaken for them.
	var b strings.Builder
	if len(pages) > 0 {
		b.WriteString(linkedPagesMarker + " (fetched from the links in the task; the task is about these) ===\n\n")
	}
	report := make([]fetchedPage, len(pages))
	for i, p := range pages {
		text, cut := p.Text, false
		if len(text) > room {
			text, cut = strings.ToValidUTF8(text[:max(room-80, 0)], ""), true
		}
		fmt.Fprintf(&b, "## Page fetched from %s\n", p.URL)
		if p.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", p.Title)
		}
		b.WriteString("\n" + text + "\n")
		if cut {
			fmt.Fprintf(&b, "[page truncated: first %d of %d characters]\n", len(text), len(p.Text))
		}
		b.WriteString("\n")
		report[i] = fetchedPage{URL: p.URL, Title: p.Title, Chars: len(text), Truncated: cut}
	}
	if len(failed) > 0 {
		b.WriteString("=== LINKS THAT COULD NOT BE OPENED (tell the user; do not guess their content) ===\n")
		for _, f := range failed {
			fmt.Fprintf(&b, "- %s: %s\n", f.URL, f.Error)
		}
		b.WriteString("\n")
		report = append(report, failed...)
	}
	if userCtx != "" {
		b.WriteString("=== GUIDELINES AND FACTS TO RESPECT (supplied with the task) ===\n\n")
		b.WriteString(userCtx)
	}
	return withDocs(strings.TrimSpace(b.String())), report, nil
}

// documentsSection formats documents the user attached for the reference.
func documentsSection(docs []store.ChatDocument) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(agent.DocumentsMarker + " (uploaded to this conversation; the user's questions are usually about these) ===\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "\n## Document: %s\n\n%s\n", d.Name, d.Text)
	}
	return b.String()
}

// fitToModel makes sure each agent's request fits the worker model's
// context on its own: the reference goes to every agent, so a document gets
// more chunks and large parts more sections when the reference is large. It
// returns a note when it changed the split, and an error when the reference
// leaves no room at all.
func fitToModel(req *createRequest, contextTokens, reserveOutput, chunkTokens int, reference string) (string, error) {
	budget := agent.PartBudget(contextTokens, reserveOutput, len(reference), len(req.Prompt))
	if budget < agent.MinPartBudget {
		return "", fmt.Errorf("the reference material (%d characters) and prompt leave no room in the worker model's context for the agents' own input; shorten the reference or use a model with a larger context", len(reference))
	}
	switch {
	case len(req.Parts) > 0:
		want := req.MaxPartTokens
		if want == 0 {
			want = agent.DefaultMaxPartTokens
		}
		if want > 0 && want > budget {
			req.MaxPartTokens = budget
			return fmt.Sprintf("parts larger than %d tokens are split so each agent fits the model's context alongside the reference", budget), nil
		}
	case req.Document != "":
		var note string
		if req.Chunks == 0 {
			req.Chunks = agent.AutoChunks(req.Document, chunkTokens)
			switch {
			case req.Chunks == 1:
				note = fmt.Sprintf("document read whole by one agent (about %d tokens; splitting only pays above about 6k)", len(req.Document)/4)
			case chunkTokens > 0 && req.Chunks > agent.AutoChunks(req.Document, 0):
				note = fmt.Sprintf("document split into %d chunks automatically (about %d tokens; the worker model's safe chunk size is %d tokens)", req.Chunks, len(req.Document)/4, chunkTokens)
			default:
				note = fmt.Sprintf("document split into %d chunks automatically (about %d tokens)", req.Chunks, len(req.Document)/4)
			}
		}
		need := (len(req.Document)/3 + budget - 1) / budget
		if need > agent.MaxChunks {
			return "", fmt.Errorf("the document needs %d chunks to fit the model's context alongside the reference; the limit is %d", need, agent.MaxChunks)
		}
		if need > max(req.Chunks, 1) {
			req.Chunks = need
			return fmt.Sprintf("document split into %d chunks so each agent fits the model's context alongside the reference", need), nil
		}
		return note, nil
	}
	return "", nil
}
