package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal client for the spawner's HTTP API. Every call takes the
// Authorization header to send, so the HTTP transport can act with the MCP
// caller's own token.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{}}
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("spawner API %d: %s", e.Status, e.Msg) }

func (c *Client) do(ctx context.Context, auth, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type Part struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type CreateTree struct {
	Prompt      string `json:"prompt"`
	Priority    int    `json:"priority,omitempty"`
	Model       string `json:"model,omitempty"`
	MaxDepth    *int   `json:"max_depth,omitempty"`
	MaxChildren *int   `json:"max_children,omitempty"`
	Document    string `json:"document,omitempty"`
	Chunks      int    `json:"chunks,omitempty"`
	Merge       string `json:"merge,omitempty"`
	Parts       []Part `json:"parts,omitempty"`
	// MaxPartTokens splits larger parts into sections (0 = server default).
	MaxPartTokens int    `json:"max_part_tokens,omitempty"`
	PartMaxWords  int    `json:"part_max_words,omitempty"`
	Context       string `json:"context,omitempty"`
	FetchURLs     bool   `json:"fetch_urls,omitempty"`
	Check         *bool  `json:"check,omitempty"`
}

func (c *Client) CreateTree(ctx context.Context, auth string, req CreateTree) (string, error) {
	var out struct {
		TreeID string `json:"tree_id"`
	}
	err := c.do(ctx, auth, http.MethodPost, "/v1/trees", req, &out)
	return out.TreeID, err
}

type Node struct {
	ID       string          `json:"id"`
	ParentID *string         `json:"parent_id"`
	Idx      int             `json:"idx"`
	Title    string          `json:"title"`
	Status   string          `json:"status"`
	Output   *string         `json:"output"`
	Error    *string         `json:"error"`
	Stats    json.RawMessage `json:"stats"`
}

type Tree struct {
	TreeID   string          `json:"tree_id"`
	Prompt   string          `json:"prompt"`
	Status   string          `json:"status"`
	Settings json.RawMessage `json:"settings"`
	Nodes    []Node          `json:"nodes"`
}

func (c *Client) GetTree(ctx context.Context, auth, id string) (*Tree, error) {
	var t Tree
	return &t, c.do(ctx, auth, http.MethodGet, "/v1/trees/"+id, nil, &t)
}

func (c *Client) CancelTree(ctx context.Context, auth, id string) (int, error) {
	var out struct {
		Cancelled int `json:"cancelled_jobs"`
	}
	err := c.do(ctx, auth, http.MethodDelete, "/v1/trees/"+id, nil, &out)
	return out.Cancelled, err
}

func (c *Client) Models(ctx context.Context, auth string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, auth, http.MethodGet, "/v1/models", nil, &out)
}

func (c *Client) Settings(ctx context.Context, auth string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.do(ctx, auth, http.MethodGet, "/v1/settings", nil, &out)
}

// Event is the subset of a tree event the tools use.
type Event struct {
	Type     string `json:"type"`
	NodeID   string `json:"node_id"`
	Title    string `json:"title"`
	Role     string `json:"role"`
	Text     string `json:"text"`
	Children []struct {
		ID string `json:"id"`
	} `json:"children"`
}

func (e Event) Final() bool {
	return e.Type == "tree.done" || e.Type == "tree.failed" || e.Type == "tree.cancelled"
}

// Follow streams a tree's lifecycle events (token deltas are skipped) until a
// final event, ctx ends, or fn returns false.
func (c *Client) Follow(ctx context.Context, auth, id string, fn func(Event) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/trees/"+id+"/events", nil)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{Status: resp.StatusCode, Msg: "event stream refused"}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var typ string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && typ != "delta" && typ != "thinking":
			var e Event
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &e) != nil {
				continue
			}
			if !fn(e) || e.Final() {
				return nil
			}
		}
	}
	return sc.Err()
}
