package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAutoChunks(t *testing.T) {
	for _, c := range []struct{ tokens, want int }{
		{100, 1}, {6000, 1}, {6001, 4}, {30000, 4}, {30001, 5}, {60000, 8}, {500000, 16},
	} {
		if got := AutoChunks(strings.Repeat("abcd", c.tokens)); got != c.want {
			t.Errorf("AutoChunks(%d tokens) = %d, want %d", c.tokens, got, c.want)
		}
	}
}

func write(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", []byte("package a"))
	write(t, root, "sub/b.go", []byte("package b"))
	write(t, root, "sub/c.md", []byte("# c"))
	write(t, root, "sub/bin.go", []byte("pk\x00g"))
	write(t, root, "sub/big.go", []byte(strings.Repeat("x", 200)))
	write(t, root, ".git/config", []byte("hidden"))
	write(t, root, "node_modules/x.go", []byte("skipped dir"))

	got, err := collectFiles(root, []string{"."}, []string{"go"}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, p := range got.Parts {
		titles = append(titles, p.Title)
	}
	if strings.Join(titles, ",") != "a.go,sub/b.go" {
		t.Fatalf("parts = %v", titles)
	}
	if len(got.Skipped) != 2 || !strings.Contains(strings.Join(got.Skipped, ";"), "binary") {
		t.Fatalf("skipped = %v", got.Skipped)
	}

	// Globs, duplicates and the file limit.
	got, err = collectFiles(root, []string{"sub/*.md", "a.go", "a.go", "sub/b.go"}, nil, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 2 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "file limit") {
		t.Fatalf("limit: parts=%d skipped=%v", len(got.Parts), got.Skipped)
	}
	if _, err := collectFiles(root, []string{"missing.go"}, nil, 0, 0); err == nil {
		t.Fatal("a missing path should be an error")
	}
}

// fakeAPI imitates the spawner API: one tree that splits into two parts,
// whose second part fails, and finishes with a merged result.
type fakeAPI struct {
	mu      sync.Mutex
	created []CreateTree
	auths   []string
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/trees", func(w http.ResponseWriter, r *http.Request) {
		var body CreateTree
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.created = append(f.created, body)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"authentication required"}`)
			return
		}
		fmt.Fprint(w, `{"tree_id":"t1"}`)
	})
	mux.HandleFunc("GET /v1/trees/t1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`event: node.split` + "\n" + `data: {"type":"node.split","node_id":"root","children":[{"id":"c1"},{"id":"c2"}]}`,
			`event: delta` + "\n" + `data: {"type":"delta","text":"tok"}`,
			`event: node.done` + "\n" + `data: {"type":"node.done","node_id":"c1","title":"Part 1/2"}`,
			`event: node.failed` + "\n" + `data: {"type":"node.failed","node_id":"c2","title":"Part 2/2"}`,
			`event: node.done` + "\n" + `data: {"type":"node.done","node_id":"root","role":"merge"}`,
			`event: tree.done` + "\n" + `data: {"type":"tree.done"}`,
		} {
			fmt.Fprint(w, e+"\n\n")
		}
	})
	mux.HandleFunc("GET /v1/trees/t1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tree_id":"t1","status":"done","nodes":[
			{"id":"root","parent_id":null,"status":"done","output":"merged answer"},
			{"id":"c1","parent_id":"root","idx":0,"title":"Part 1/2","status":"done","output":"one"},
			{"id":"c2","parent_id":"root","idx":1,"title":"Part 2/2","status":"failed","error":"context overflow"}]}`)
	})
	mux.HandleFunc("DELETE /v1/trees/t1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"cancelled_jobs":2}`)
	})
	return mux
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsOverStdioStyleSession(t *testing.T) {
	api := &fakeAPI{}
	ts := httptest.NewServer(api.handler())
	defer ts.Close()
	root := t.TempDir()
	write(t, root, "x.go", []byte("package x"))
	write(t, root, "y.go", []byte("package y"))

	srv := New(NewClient(ts.URL), Options{DefaultAuth: "Bearer tok", ConsoleURL: "http://console/", LocalFiles: true, Root: root, Version: "test"})
	cs := connect(t, srv)
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"run_task", "analyze_files", "get_task", "cancel_task", "list_models"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Fatalf("missing tool %s in %v", want, names)
		}
	}

	var progress []string
	var pmu sync.Mutex
	cs2ct, cs2st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, cs2st, nil); err != nil {
		t.Fatal(err)
	}
	pc, err := mcp.NewClient(&mcp.Implementation{Name: "progress"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			pmu.Lock()
			progress = append(progress, r.Params.Message)
			pmu.Unlock()
		},
	}).Connect(ctx, cs2ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	doc := strings.Repeat("word ", 10000) // ~12.5k tokens -> 4 chunks
	params := &mcp.CallToolParams{Name: "run_task", Arguments: map[string]any{"prompt": "summarize", "document": doc, "merge": "list"}}
	params.SetProgressToken("p1")
	res, err := pc.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	out := text(res)
	if res.IsError || !strings.HasPrefix(out, "merged answer") || !strings.Contains(out, "failed parts: Part 2/2") || !strings.Contains(out, "console: http://console/#/runs/t1") {
		t.Fatalf("run_task result (error=%v):\n%s", res.IsError, out)
	}
	if c := api.created[0]; c.Chunks != 4 || c.Merge != "list" || c.Document != doc || api.auths[0] != "Bearer tok" {
		t.Fatalf("request sent: chunks=%d merge=%q auth=%q", c.Chunks, c.Merge, api.auths[0])
	}
	pmu.Lock()
	if len(progress) != 4 || !strings.Contains(progress[len(progress)-1], "3/3") {
		t.Fatalf("progress = %v", progress)
	}
	pmu.Unlock()

	// analyze_files: one part per file, concat merge by default.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "analyze_files", Arguments: map[string]any{"paths": []string{"."}, "instruction": "review"}})
	if err != nil || res.IsError {
		t.Fatalf("analyze_files: %v %s", err, text(res))
	}
	if c := api.created[1]; len(c.Parts) != 2 || c.Parts[0].Title != "x.go" || c.Merge != "concat" {
		t.Fatalf("analyze_files sent %+v", c)
	}

	// Not waiting returns the id immediately.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "run_task", Arguments: map[string]any{"prompt": "hi", "wait": false}})
	if err != nil || !strings.Contains(text(res), "Started task t1") {
		t.Fatalf("no-wait: %v %s", err, text(res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"task_id": "t1", "include_parts": true}})
	if err != nil || !strings.Contains(text(res), "merged answer") {
		t.Fatalf("get_task: %v %s", err, text(res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "cancel_task", Arguments: map[string]any{"task_id": "t1"}})
	if err != nil || res.IsError {
		t.Fatalf("cancel_task: %v %s", err, text(res))
	}
}

// Over HTTP the tools act with the caller's own Authorization header.
func TestHTTPForwardsCallerToken(t *testing.T) {
	api := &fakeAPI{}
	ts := httptest.NewServer(api.handler())
	defer ts.Close()
	srv := New(NewClient(ts.URL), Options{})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	hs := httptest.NewServer(h)
	defer hs.Close()

	call := func(token string) *mcp.CallToolResult {
		hc := &http.Client{Transport: headerRT{token}}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "http"}, nil).Connect(context.Background(),
			&mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: hc}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_task", Arguments: map[string]any{"prompt": "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("good"); res.IsError {
		t.Fatalf("good token: %s", text(res))
	}
	if res := call("bad"); !res.IsError || !strings.Contains(text(res), "401") {
		t.Fatalf("bad token should surface the API's 401: %s", text(res))
	}
	if api.auths[0] != "Bearer good" || api.auths[1] != "Bearer bad" {
		t.Fatalf("forwarded auth = %v", api.auths)
	}
	// File access is never offered over HTTP: the files would be the server's.
	cs, _ := mcp.NewClient(&mcp.Implementation{Name: "http"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL}, nil)
	defer cs.Close()
	tools, _ := cs.ListTools(context.Background(), nil)
	for _, tl := range tools.Tools {
		if tl.Name == "analyze_files" {
			t.Fatal("analyze_files must not be exposed without LocalFiles")
		}
	}
}

type headerRT struct{ token string }

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}
