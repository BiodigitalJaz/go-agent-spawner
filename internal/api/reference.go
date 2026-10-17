package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
)

// maxContextChars caps reference material. Every agent receives it, so it
// competes with each request's own input for the model's (often shared)
// context: about 8k tokens at a conservative 3 characters per token.
const maxContextChars = 24_000

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
func (s *Server) reference(ctx context.Context, req *createRequest, set settings.Settings, remote string) (string, []fetchedPage, error) {
	userCtx := strings.TrimSpace(req.Context)
	if len(userCtx) > maxContextChars {
		return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf(
			"context is %d characters; the limit is %d because every agent receives it. Send large material as document or parts", len(userCtx), maxContextChars)}
	}
	urls := promptURLs(req.Prompt)
	if len(urls) == 0 {
		return userCtx, nil, nil
	}
	if !req.FetchURLs {
		if userCtx == "" && req.Document == "" && len(req.Parts) == 0 {
			return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf(
				"the prompt links to %s, but agents cannot open web pages. Turn on fetch_urls (\"Fetch linked pages\" in the console) or pass the content as context or document", urls[0])}
		}
		return userCtx, nil, nil
	}
	if len(urls) > fetch.MaxURLs {
		return "", nil, &requestError{http.StatusBadRequest, fmt.Sprintf("the prompt links to %d pages; at most %d can be fetched per run", len(urls), fetch.MaxURLs)}
	}
	f := fetch.New(s.log.With("fetch_requested_by", remote), set.AllowInternalFetch)
	pages := make([]*fetch.Page, len(urls))
	for i, u := range urls {
		p, err := f.Fetch(ctx, u)
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
	// Share what room is left after the caller's own context between pages.
	room := (maxContextChars - len(userCtx)) / len(pages)
	var b strings.Builder
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
	if userCtx != "" {
		b.WriteString(userCtx)
	}
	return strings.TrimSpace(b.String()), report, nil
}
