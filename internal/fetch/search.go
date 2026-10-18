package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SearchResult is one hit from a search engine.
type SearchResult struct {
	URL     string
	Title   string
	Snippet string
	Engines []string
}

// Searcher queries a SearXNG instance's JSON API. Its address is admin
// configuration (SEARCH_URL), typically an in-cluster service, so it is not
// subject to the internal-address guard; the result pages are, when fetched.
type Searcher struct {
	BaseURL    string
	Categories string // comma-separated SearXNG categories
	client     *http.Client
}

func NewSearcher(baseURL, categories string) *Searcher {
	if categories == "" {
		// The instance's default category may have no working engine, so
		// ask for several explicitly; SearXNG merges them. Science is left
		// out: academic engines swamp everyday queries with papers.
		categories = "general,it,q&a,news"
	}
	return &Searcher{BaseURL: strings.TrimSuffix(baseURL, "/"), Categories: categories, client: &http.Client{Timeout: 25 * time.Second}}
}

func (s *Searcher) Search(ctx context.Context, query string) ([]SearchResult, error) {
	q := url.Values{"q": {query}, "format": {"json"}, "categories": {s.Categories}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned HTTP %d (is format=json enabled in SearXNG?)", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			URL     string   `json:"url"`
			Title   string   `json:"title"`
			Content string   `json:"content"`
			Engines []string `json:"engines"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	out := make([]SearchResult, 0, len(body.Results))
	for _, r := range body.Results {
		if r.URL != "" {
			out = append(out, SearchResult{URL: r.URL, Title: strings.TrimSpace(r.Title), Snippet: strings.TrimSpace(r.Content), Engines: r.Engines})
		}
	}
	return out, nil
}
