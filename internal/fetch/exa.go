package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Budget, when set, stops Exa before its monthly free credit runs out
	// (this project never pays for search). Unrecorded costs are estimated.
	Budget Budget
	log    *slog.Logger
	client *http.Client
}

// ErrBudget means the monthly free-tier budget is used up.
var ErrBudget = errors.New("monthly free-tier budget used up")

// freeTypes are the only search types allowed: the cheapest ones. Deep
// research types cost several times more and would drain the free credit.
var freeTypes = map[string]bool{"instant": true, "fast": true, "auto": true}

// estimatedCost is used when Exa omits costDollars: the instant price.
const estimatedCost = 0.004

func NewExa(log *slog.Logger, key string) *Exa {
	return &Exa{BaseURL: "https://api.exa.ai", Key: key, Type: "instant", NumResults: 5, MaxChars: 4000,
		log: log, client: &http.Client{Timeout: 25 * time.Second}}
}

func (e *Exa) Name() string { return "exa" }

func (e *Exa) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if !freeTypes[e.Type] {
		return nil, fmt.Errorf("exa search type %q is not allowed (free tier: instant, fast or auto)", e.Type)
	}
	if e.Budget != nil {
		spent, err := e.Budget.Spent(ctx)
		if err != nil {
			return nil, fmt.Errorf("check exa budget: %w", err)
		}
		if spent+estimatedCost > e.Budget.Limit() {
			return nil, fmt.Errorf("%w ($%.3f of $%.2f this month; resets on the 1st)", ErrBudget, spent, e.Budget.Limit())
		}
	}
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
	cost := out.CostDollars.Total
	if cost <= 0 {
		cost = estimatedCost
	}
	if e.Budget != nil {
		if err := e.Budget.Record(ctx, cost); err != nil {
			e.log.Warn("record exa spend", "err", err)
		}
	}
	e.log.Info("exa search", "query", query, "results", len(out.Results), "cost_usd", cost)
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
			Engines: []string{"exa"}, CostUSD: cost / float64(kept)})
	}
	return res, nil
}
