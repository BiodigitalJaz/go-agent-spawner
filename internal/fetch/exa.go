package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Exa queries Exa's search API (https://exa.ai), which uses its own index
// and permits automated queries. Results come back with page text, so they
// need no separate fetch. Each query is logged with Exa's cost estimate.
type Exa struct {
	BaseURL    string
	Key        string
	Type       string // "instant" is the cheapest and fastest
	NumResults int
	MaxChars   int
	log        *slog.Logger
	client     *http.Client
}

func NewExa(log *slog.Logger, key string) *Exa {
	return &Exa{BaseURL: "https://api.exa.ai", Key: key, Type: "instant", NumResults: 5, MaxChars: 4000,
		log: log, client: &http.Client{Timeout: 25 * time.Second}}
}

func (e *Exa) Name() string { return "exa" }

func (e *Exa) Search(ctx context.Context, query string) ([]SearchResult, error) {
	body, _ := json.Marshal(map[string]any{
		"query": query, "type": e.Type, "numResults": e.NumResults,
		"contents": map[string]any{"text": map[string]any{"maxCharacters": e.MaxChars}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(e.BaseURL, "/")+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", e.Key)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("exa returned HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Results []struct {
			URL           string `json:"url"`
			Title         string `json:"title"`
			Text          string `json:"text"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
		CostDollars struct {
			Total float64 `json:"total"`
		} `json:"costDollars"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode exa results: %w", err)
	}
	e.log.Info("exa search", "query", query, "results", len(out.Results), "cost_usd", out.CostDollars.Total)
	kept := 0
	for _, r := range out.Results {
		if r.URL != "" {
			kept++
		}
	}
	res := make([]SearchResult, 0, kept)
	for _, r := range out.Results {
		if r.URL == "" {
			continue
		}
		snippet := strings.Join(strings.Fields(r.Text), " ")
		if len(snippet) > 240 {
			snippet = strings.ToValidUTF8(snippet[:240], "") + "…"
		}
		res = append(res, SearchResult{URL: r.URL, Title: strings.TrimSpace(r.Title), Snippet: snippet, Text: r.Text,
			Engines: []string{"exa"}, CostUSD: out.CostDollars.Total / float64(kept)})
	}
	return res, nil
}
