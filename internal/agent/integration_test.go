package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/models"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// fakeLLM is an OpenAI-compatible backend. Planner requests (those carrying a
// response_format) get a split plan; other requests stream canned text.
type fakeLLM struct {
	split      []string // subtask prompts the planner returns
	failPrompt string   // exec requests containing this get a 400
	wordDelay  time.Duration
	requests   atomic.Int64
	// sharedChars simulates LM Studio's context shared by all slots: when the
	// prompt characters of concurrent requests exceed it, the request fails
	// mid-stream with "Context size has been exceeded" (code 500).
	sharedChars int64
	inflight    atomic.Int64
	pressure    atomic.Int64
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages       []struct{ Role, Content string } `json:"messages"`
		ResponseFormat json.RawMessage                  `json:"response_format"`
	}
	_ = json.Unmarshal(body, &req)
	user := req.Messages[len(req.Messages)-1].Content
	system := req.Messages[0].Content
	if f.sharedChars > 0 {
		size := int64(len(user) + len(system))
		defer f.inflight.Add(-size)
		if f.inflight.Add(size) > f.sharedChars {
			f.pressure.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"error":{"message":"Engine protocol predict stream returned an error: {\"code\":500,\"message\":\"Context size has been exceeded.\",\"type\":\"server_error\"}"}}`+"\n\n")
			return
		}
	}

	var content string
	switch {
	case req.ResponseFormat != nil:
		p := map[string]any{"strategy": "direct", "reason": "small task", "subtasks": []any{}}
		if strings.Contains(user, "SPLIT") {
			subs := []map[string]any{}
			for i, s := range f.split {
				subs = append(subs, map[string]any{"title": fmt.Sprintf("part %d", i+1), "prompt": s, "complex": false})
			}
			p = map[string]any{"strategy": "split", "reason": "independent parts", "subtasks": subs}
		}
		b, _ := json.Marshal(p)
		content = string(b)
	case f.failPrompt != "" && strings.Contains(user, f.failPrompt):
		http.Error(w, `{"error":"context length exceeded"}`, http.StatusBadRequest)
		return
	case strings.Contains(user, "Partial results to consolidate"):
		content = "CONSOLIDATED " + fmt.Sprint(strings.Count(user, "### ")) + " results"
	case strings.Contains(user, "Results from parallel workers"):
		content = "SYNTHESIS of " + fmt.Sprint(strings.Count(user, "### ")) + " results"
	case strings.Contains(system, "PIRATE"):
		content = "arr: " + user
	default:
		content = "answer to: " + user
	}

	w.Header().Set("Content-Type", "text/event-stream")
	for _, word := range strings.SplitAfter(content, " ") {
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": word}}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(max(f.wordDelay, 2*time.Millisecond)):
		case <-r.Context().Done():
			return
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// isolatedDB creates a throwaway database for one test so tests never touch
// data in the database TEST_DATABASE_URL points at.
func isolatedDB(t *testing.T, baseURL string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "spawner_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	return db
}

type harness struct {
	db     *pgxpool.Pool
	client *river.Client[pgx.Tx]
	bus    *events.Bus
	pool   *backend.Pool
	llmURL string
	set    settings.Settings
}

func setup(t *testing.T, llm *fakeLLM) *harness {
	t.Helper()
	dbURL, natsURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_NATS_URL")
	if dbURL == "" || natsURL == "" {
		t.Skip("TEST_DATABASE_URL and TEST_NATS_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}

	db := isolatedDB(t, dbURL)
	if err := store.Migrate(ctx, db, log); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(llm)
	t.Cleanup(srv.Close)
	pool, err := backend.NewPool(log, []backend.ModelConfig{
		{Name: "small", Model: "fake-small", Endpoints: []string{srv.URL}, InitialLimit: 2, MaxLimit: 4},
		{Name: "large", Model: "fake-large", Endpoints: []string{srv.URL}, InitialLimit: 2, MaxLimit: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	bus, err := events.Connect(ctx, log, events.Config{URL: natsURL, Retention: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bus.Close)

	cfg := agent.Config{
		AcquireWait: 200 * time.Millisecond, Snooze: 50 * time.Millisecond,
		DeltaInterval: 20 * time.Millisecond, JobTimeout: 30 * time.Second,
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, agent.NewWorker(cfg, log, db, pool, bus))
	client, err := river.NewClient(riverpgxv5.New(db), &river.Config{
		Logger:            log,
		Workers:           workers,
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
		Queues: map[string]river.QueueConfig{
			agent.QueueName("small"): {MaxWorkers: 8},
			agent.QueueName("large"): {MaxWorkers: 8},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})
	return &harness{db: db, client: client, bus: bus, pool: pool, llmURL: srv.URL, set: settings.Defaults("small", "large", 2, 4)}
}

// run starts a tree and collects its events until a final one arrives.
func (h *harness) run(t *testing.T, prompt string) (uuid.UUID, []events.Event) {
	t.Helper()
	return h.runWith(t, prompt, h.set)
}

func (h *harness) runWith(t *testing.T, prompt string, set settings.Settings) (uuid.UUID, []events.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started, err := agent.StartTree(ctx, h.db, h.client, agent.TreeRequest{Prompt: prompt, Priority: 1, Settings: set})
	if err != nil {
		t.Fatal(err)
	}
	root := started.Root
	msgs, err := h.bus.Subscribe(ctx, root.TreeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []events.Event
	for m := range msgs {
		got = append(got, m.Event)
		if m.Event.Type.Final() {
			return root.TreeID, got
		}
	}
	t.Fatalf("tree %s did not finish; events: %v", root.TreeID, types(got))
	return uuid.Nil, nil
}

func types(evs []events.Event) []events.Type {
	out := make([]events.Type, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func count(evs []events.Event, typ events.Type) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestSplitExecReduce(t *testing.T) {
	h := setup(t, &fakeLLM{split: []string{"alpha", "beta", "gamma"}})
	treeID, evs := h.run(t, "SPLIT this task")

	last := evs[len(evs)-1]
	if last.Type != events.TreeDone || last.Text != "SYNTHESIS of 3 results" {
		t.Fatalf("unexpected final event %+v", last)
	}
	if n := count(evs, events.NodeSplit); n != 1 {
		t.Fatalf("expected 1 split, got %d", n)
	}
	// 3 children + the reduce on the root.
	if n := count(evs, events.NodeDone); n != 4 {
		t.Fatalf("expected 4 node.done, got %d: %v", n, types(evs))
	}
	if count(evs, events.Delta) == 0 {
		t.Fatal("expected streamed deltas")
	}

	nodes, err := store.TreeNodes(context.Background(), h.db, treeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}
	for _, n := range nodes {
		if n.Status != store.StatusDone {
			t.Errorf("node %s (%q) status %s", n.ID, n.Title, n.Status)
		}
	}
}

func TestDirectInline(t *testing.T) {
	llm := &fakeLLM{}
	h := setup(t, llm)
	_, evs := h.run(t, "what is 2+2")
	last := evs[len(evs)-1]
	if last.Type != events.TreeDone || last.Text != "answer to: what is 2+2" {
		t.Fatalf("unexpected final event %+v", last)
	}
	if got := llm.requests.Load(); got != 2 {
		t.Fatalf("expected plan + exec = 2 requests, got %d", got)
	}
}

func TestFailedChildStillReduces(t *testing.T) {
	h := setup(t, &fakeLLM{split: []string{"ok one", "BROKEN", "ok two"}, failPrompt: "BROKEN"})
	treeID, evs := h.run(t, "SPLIT with a failing part")

	last := evs[len(evs)-1]
	if last.Type != events.TreeDone || last.Text != "SYNTHESIS of 3 results" {
		t.Fatalf("unexpected final event %+v", last)
	}
	if n := count(evs, events.NodeFailed); n != 1 {
		t.Fatalf("expected 1 failed node, got %d", n)
	}
	nodes, _ := store.TreeNodes(context.Background(), h.db, treeID)
	failed := 0
	for _, n := range nodes {
		if n.Status == store.StatusFailed {
			failed++
			if n.Error == nil || !strings.Contains(*n.Error, "400") {
				t.Errorf("failed node missing error: %+v", n)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("expected 1 failed node in db, got %d", failed)
	}
}

func TestCancelTreeStopsRunningWork(t *testing.T) {
	llm := &fakeLLM{split: []string{"slow one", "slow two"}, wordDelay: 300 * time.Millisecond}
	h := setup(t, llm)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	started, err := agent.StartTree(ctx, h.db, h.client, agent.TreeRequest{Prompt: "SPLIT slowly with many words to stream", Priority: 1, Settings: h.set})
	if err != nil {
		t.Fatal(err)
	}
	root := started.Root
	msgs, err := h.bus.Subscribe(ctx, root.TreeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until both children are streaming, then cancel.
	streaming := map[uuid.UUID]bool{}
	for m := range msgs {
		if m.Event.Type == events.Delta && m.Event.NodeID != root.ID {
			streaming[m.Event.NodeID] = true
		}
		if len(streaming) == 2 {
			break
		}
	}
	if _, err := agent.CancelTree(ctx, h.db, h.client, h.bus, root.TreeID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var unfinished int
		err := h.db.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'tree_id' = $1 AND finalized_at IS NULL`, root.TreeID.String()).Scan(&unfinished)
		if err != nil {
			t.Fatal(err)
		}
		if unfinished == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	nodes, _ := store.TreeNodes(ctx, h.db, root.TreeID)
	for _, n := range nodes {
		if n.Status != store.StatusCancelled {
			t.Errorf("node %q status %s, want cancelled", n.Title, n.Status)
		}
	}
	var states []string
	rows, _ := h.db.Query(ctx, `SELECT state FROM river_job WHERE args->>'tree_id' = $1`, root.TreeID.String())
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	for _, s := range states {
		if s != "cancelled" && s != "completed" {
			t.Errorf("job state %s after cancel; all states %v", s, states)
		}
	}
}

func TestSettingsSnapshotDrivesDirectivesModelsAndTimeline(t *testing.T) {
	h := setup(t, &fakeLLM{split: []string{"one", "two"}})
	set := h.set
	set.Worker.Directive = "You are a PIRATE worker."
	set.Reducer.Model = "small" // planner and reducer on small, workers on large
	treeID, evs := h.runWith(t, "SPLIT into pirate parts", set)

	if last := evs[len(evs)-1]; last.Type != events.TreeDone {
		t.Fatalf("unexpected final event %+v", last)
	}
	models := map[string]string{} // role -> API model that served it
	for _, e := range evs {
		if e.Type == events.NodeDone && e.Stats != nil {
			models[e.Role] = e.Stats.Model
		}
	}
	if models["exec"] != "fake-large" || models["reduce"] != "fake-small" {
		t.Fatalf("role routing wrong: %v", models)
	}

	nodes, err := store.TreeNodes(context.Background(), h.db, treeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == treeID {
			if n.SplitAt == nil || n.ReduceStartedAt == nil || n.FinishedAt == nil || n.StartedAt == nil {
				t.Errorf("root timeline incomplete: %+v", n)
			}
			continue
		}
		if n.Output == nil || !strings.HasPrefix(*n.Output, "arr: ") {
			t.Errorf("worker directive not applied to child %q: %v", n.Title, n.Output)
		}
		if n.StartedAt == nil || n.FinishedAt == nil || len(n.Stats) == 0 || n.Role != "exec" {
			t.Errorf("child timeline/stats missing: %+v", n)
		}
	}

	// The snapshot is immutable: saving new settings doesn't touch it.
	if _, err := settings.Save(context.Background(), h.db, h.set, 0); err != nil {
		t.Fatal(err)
	}
	snap, err := settings.ForTree(context.Background(), h.db, treeID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Worker.Directive != set.Worker.Directive {
		t.Fatalf("snapshot changed: %q", snap.Worker.Directive)
	}
}

func TestSettingsVersioning(t *testing.T) {
	h := setup(t, &fakeLLM{})
	ctx := context.Background()
	st, err := settings.Load(ctx, h.db, h.set)
	if err != nil || st.Version != 0 {
		t.Fatalf("expected defaults at version 0: %+v %v", st, err)
	}
	v1, err := settings.Save(ctx, h.db, h.set, 0)
	if err != nil || v1.Version != 1 {
		t.Fatalf("first save: %+v %v", v1, err)
	}
	if _, err := settings.Save(ctx, h.db, h.set, 0); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale insert should conflict, got %v", err)
	}
	if _, err := settings.Save(ctx, h.db, h.set, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Save(ctx, h.db, h.set, 1); !errors.Is(err, settings.ErrConflict) {
		t.Fatalf("stale update should conflict, got %v", err)
	}
	trees, err := store.ListTrees(ctx, h.db, 10)
	if err != nil || len(trees) != 0 {
		t.Fatalf("list trees on empty db: %v %v", trees, err)
	}
}

func TestModelAddedAtRuntimeIsServed(t *testing.T) {
	h := setup(t, &fakeLLM{})
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	static := []backend.ModelConfig{
		{Name: "small", Model: "fake-small", Endpoints: []string{h.llmURL}},
		{Name: "large", Model: "fake-large", Endpoints: []string{h.llmURL}},
	}
	reg := models.NewRegistry(log, h.db, static)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	follower := agent.NewQueueFollower(log, h.client, 4, []string{"small", "large"})
	reg.OnChange(func(cfgs []backend.ModelConfig) {
		if err := h.pool.SetModels(cfgs); err != nil {
			t.Errorf("set models: %v", err)
		}
		follower.Sync(ctx, h.pool.Models())
	})

	// A name clash with backends.json is refused; a new name is served.
	if err := reg.Create(ctx, backend.ModelConfig{Name: "large", Model: "x", Endpoints: []string{h.llmURL}}); !errors.Is(err, models.ErrExists) {
		t.Fatalf("expected clash with static model, got %v", err)
	}
	if err := reg.Create(ctx, backend.ModelConfig{Name: "dyn", Model: "fake-dyn", Endpoints: []string{h.llmURL}, MaxLimit: 2}); err != nil {
		t.Fatal(err)
	}
	if got := follower.Models(); !slices.Contains(got, "dyn") {
		t.Fatalf("queue not added: %v", got)
	}

	set := h.set
	set.MaxDepth = 0
	set.Worker.Model = "dyn"
	_, evs := h.runWith(t, "hello dynamic", set)
	var served string
	for _, e := range evs {
		if e.Type == events.NodeDone && e.Stats != nil {
			served = e.Stats.Model
		}
	}
	if last := evs[len(evs)-1]; last.Type != events.TreeDone || served != "fake-dyn" {
		t.Fatalf("run on runtime model: final=%v served by %q", last.Type, served)
	}

	// Updating rebuilds it; deleting removes the queue and the route.
	if err := reg.Update(ctx, backend.ModelConfig{Name: "dyn", Model: "fake-dyn2", Endpoints: []string{h.llmURL}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(ctx, backend.ModelConfig{Name: "small", Model: "x", Endpoints: []string{h.llmURL}}); !errors.Is(err, models.ErrReadOnly) {
		t.Fatalf("static model must be read-only, got %v", err)
	}
	if err := reg.Delete(ctx, "dyn"); err != nil {
		t.Fatal(err)
	}
	if got := follower.Models(); slices.Contains(got, "dyn") || h.pool.Has("dyn") {
		t.Fatalf("model not removed: queues=%v pool=%v", got, h.pool.Has("dyn"))
	}
	if n, err := models.PendingJobs(ctx, h.db, agent.QueueName("dyn")); err != nil || n != 0 {
		t.Fatalf("pending jobs %d %v", n, err)
	}
}

// collect follows a tree's events until a final one.
func (h *harness) collect(t *testing.T, treeID uuid.UUID) []events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := h.bus.Subscribe(ctx, treeID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []events.Event
	for m := range msgs {
		got = append(got, m.Event)
		if m.Event.Type.Final() {
			return got
		}
	}
	t.Fatalf("tree did not finish: %v", types(got))
	return nil
}

func testDocument(paragraphs int) string {
	var b strings.Builder
	for i := range paragraphs {
		fmt.Fprintf(&b, "Paragraph %d mentions service svc-%03d on port %d. %s\n\n", i, i, 7000+i, strings.Repeat("filler text ", 12))
	}
	return b.String()
}

func TestDocumentModeChunksInCode(t *testing.T) {
	h := setup(t, &fakeLLM{})
	doc := testDocument(40)
	started, err := agent.StartTree(context.Background(), h.db, h.client, agent.TreeRequest{
		Prompt: "List every service and port.", Priority: 1, Settings: h.set, Document: doc, Chunks: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Children) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(started.Children))
	}
	evs := h.collect(t, started.Root.TreeID)
	if last := evs[len(evs)-1]; last.Type != events.TreeDone || last.Text != "SYNTHESIS of 4 results" {
		t.Fatalf("unexpected final %+v", last)
	}
	if count(evs, events.NodeStarted) != 5 { // 4 parts + reduce, no planner
		t.Fatalf("expected 4 workers + 1 reduce and no planner: %v", types(evs))
	}
	// Every paragraph went to exactly one agent.
	nodes, _ := store.TreeNodes(context.Background(), h.db, started.Root.TreeID)
	seen := 0
	for _, n := range nodes {
		if n.ParentID != nil {
			seen += strings.Count(n.Prompt, "Paragraph ")
		}
	}
	if seen != 40 {
		t.Fatalf("paragraphs across chunks = %d, want 40", seen)
	}
}

func TestMergeGuardConsolidatesInParallelBatches(t *testing.T) {
	h := setup(t, &fakeLLM{})
	// Shrink the reducer model's context so 8 chunk results cannot fit one merge.
	err := h.pool.SetModels([]backend.ModelConfig{
		{Name: "small", Model: "fake-small", Endpoints: []string{h.llmURL}, InitialLimit: 2, MaxLimit: 4},
		{Name: "large", Model: "fake-large", Endpoints: []string{h.llmURL}, InitialLimit: 3, MaxLimit: 4, ContextLength: 900},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := agent.StartTree(context.Background(), h.db, h.client, agent.TreeRequest{
		Prompt: "List every service and port.", Priority: 1, Settings: h.set, Document: testDocument(48), Chunks: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := h.collect(t, started.Root.TreeID)
	last := evs[len(evs)-1]
	if last.Type != events.TreeDone {
		t.Fatalf("tree failed: %+v", last)
	}
	var reduce *events.Stats
	for _, e := range evs {
		if e.Type == events.NodeDone && e.Role == "reduce" {
			reduce = e.Stats
		}
	}
	if reduce == nil || reduce.MergePasses < 1 || reduce.MergeBatches < 2 {
		t.Fatalf("expected batched consolidation, got %+v", reduce)
	}
	// The final merge saw consolidated groups, not the 8 raw results.
	if !strings.HasPrefix(last.Text, "SYNTHESIS of ") || last.Text == "SYNTHESIS of 8 results" {
		t.Fatalf("final merge should see consolidated groups: %q", last.Text)
	}
}

func TestCodeMergeSkipsModel(t *testing.T) {
	for _, mode := range []string{settings.MergeList, settings.MergeConcat} {
		t.Run(mode, func(t *testing.T) {
			// Paragraph 13 lands in part 2 of 4; that part's request fails (400).
			llm := &fakeLLM{failPrompt: "svc-013"}
			h := setup(t, llm)
			set := h.set
			set.Merge = mode
			started, err := agent.StartTree(context.Background(), h.db, h.client, agent.TreeRequest{
				Prompt: "List every service and port.", Priority: 1, Settings: set, Document: testDocument(40), Chunks: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			evs := h.collect(t, started.Root.TreeID)
			last := evs[len(evs)-1]
			if last.Type != events.TreeDone {
				t.Fatalf("tree should finish despite a failed part: %+v", last)
			}
			if got := llm.requests.Load(); got != 4 {
				t.Fatalf("expected exactly 4 model calls (one per part, none for the merge), got %d", got)
			}
			var reduce *events.Stats
			for _, e := range evs {
				if e.Type == events.NodeDone && e.Role == "reduce" {
					reduce = e.Stats
				}
			}
			if reduce == nil || reduce.Model != "merge:"+mode || reduce.Endpoint != "" {
				t.Fatalf("reduce should be a code merge: %+v", reduce)
			}
			if !strings.Contains(last.Text, "[part 2 (Part 2/4) failed:") {
				t.Fatalf("failed part must be visible in the result:\n%s", last.Text)
			}
			// Every service from every successful part is in the result; the
			// failed part's services are not (no model invented them).
			nodes, _ := store.TreeNodes(context.Background(), h.db, started.Root.TreeID)
			svc := regexp.MustCompile(`svc-\d{3}`)
			kept, failedParts := 0, 0
			for _, n := range nodes {
				if n.ParentID == nil {
					continue
				}
				for _, name := range svc.FindAllString(n.Prompt, -1) {
					in := strings.Contains(last.Text, name)
					if n.Status == store.StatusDone && !in {
						t.Fatalf("%s from successful %s missing from result", name, n.Title)
					}
					if n.Status == store.StatusFailed && in {
						t.Fatalf("%s from failed %s should not appear", name, n.Title)
					}
					if in {
						kept++
					}
				}
				if n.Status == store.StatusFailed {
					failedParts++
				}
			}
			if failedParts != 1 || kept < 25 {
				t.Fatalf("expected 1 failed part and most services kept: failed=%d kept=%d", failedParts, kept)
			}
		})
	}
}

func TestSharedContextPressure(t *testing.T) {
	doc := testDocument(40) // 4 parts of ~2.4k chars each
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_context=%v", shared), func(t *testing.T) {
			llm := &fakeLLM{sharedChars: 6000, wordDelay: 15 * time.Millisecond} // ~2 parts fit at once
			h := setup(t, llm)
			large := backend.ModelConfig{Name: "large", Model: "fake-large", Endpoints: []string{h.llmURL}, InitialLimit: 4, MaxLimit: 4}
			if shared {
				// ~1.7k estimated tokens per part; budget 0.9 x 4000 admits two.
				large.ContextLength, large.SharedContext = 4000, true
			}
			if err := h.pool.SetModels([]backend.ModelConfig{
				{Name: "small", Model: "fake-small", Endpoints: []string{h.llmURL}, InitialLimit: 2, MaxLimit: 4},
				large,
			}); err != nil {
				t.Fatal(err)
			}
			set := h.set
			set.Merge = settings.MergeList
			started, err := agent.StartTree(context.Background(), h.db, h.client, agent.TreeRequest{
				Prompt: "List every service and port.", Priority: 1, Settings: set, Document: doc, Chunks: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			evs := h.collect(t, started.Root.TreeID)
			if last := evs[len(evs)-1]; last.Type != events.TreeDone || strings.Contains(last.Text, "failed:") {
				t.Fatalf("all parts must succeed: %+v", last)
			}
			nodes, _ := store.TreeNodes(context.Background(), h.db, started.Root.TreeID)
			for _, n := range nodes {
				if n.Status != store.StatusDone {
					t.Fatalf("part %q ended %s: %v", n.Title, n.Status, n.Error)
				}
			}
			p := llm.pressure.Load()
			if shared && p != 0 {
				t.Fatalf("token admission should prevent overflow, got %d pressure errors", p)
			}
			if !shared && p == 0 {
				t.Fatalf("expected the unbudgeted run to hit context pressure (and recover)")
			}
			t.Logf("pressure errors: %d", p)
		})
	}
}
