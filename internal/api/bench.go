package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/auth"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/discovery"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// Benchmarks replay a chat against several pool models and score them.
// Each user message of the chat is a case, replayed with the history and
// the exact reference material (search results, linked pages) its original
// reply had, so every model answers from the same sources and nothing is
// searched again. Every model answers every case at the chosen concurrency
// (timed), then a judge model scores each answer for refusals, specifics the
// sources don't support and general-knowledge slips. Optionally the runner
// swaps models on LM Studio hosts, since one GPU rarely holds two at once.

const (
	maxBenchCases  = 8
	maxBenchModels = 4
	judgeParallel  = 4
	judgeMaxTokens = 1024
)

func (s *Server) benchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/benchmarks", s.listBenchmarks)
	mux.HandleFunc("POST /v1/benchmarks", s.createBenchmark)
	mux.HandleFunc("GET /v1/benchmarks/{id}", s.getBenchmark)
	mux.HandleFunc("DELETE /v1/benchmarks/{id}", s.deleteBenchmark)
}

// benchSuite is the standard test set: one message per failure mode seen
// in chat (regulations and refusals, product facts from sources, prices,
// legal how-tos, general know-how, technical config, general knowledge,
// seasonal multi-part planning). Changing it starts a new source set.
var benchSuite = []string{
	"Provide steps to hunt with a crossbow for turkey in Oklahoma. How to process it and how many separate parts are there afterwards. Provide a smoked recipe for the separate parts.",
	"What can you tell me about the Wyze Cam Outdoor camera?",
	"How much does a Texas resident hunting license cost this year?",
	"How do I legally transport a handgun across state lines when driving through several US states?",
	"Give me a recipe for smoked pork shoulder with times and temperatures.",
	"Write a hyprland.conf snippet that makes kitty windows slightly transparent with blur, and explain each line.",
	"Explain the difference between TCP and UDP, with an example of when to use each.",
	"Plan a 3-day visit to Yellowstone in late October: what is open, likely road closures, and what to pack.",
}

type benchConfig struct {
	// Suite replays the standard test set (benchSuite). Its sources are
	// collected once and reused by later suite benchmarks, so results stay
	// comparable; Refresh collects them again.
	Suite    bool       `json:"suite,omitempty"`
	Refresh  bool       `json:"refresh,omitempty"`
	ChatID   *uuid.UUID `json:"chat_id,omitempty"`
	Message  string     `json:"message,omitempty"` // a new chat: its reply (search on auto) becomes the case
	Models   []string   `json:"models"`
	Judge    string     `json:"judge"`
	Parallel int        `json:"parallel"` // concurrent answers per model, for the speed numbers
	Swap     bool       `json:"swap"`     // load each model on its LM Studio host before its turn
}

type benchCase struct {
	Prompt     string          `json:"prompt"`
	Sources    bool            `json:"sources"` // the original reply had reference material
	SourceTree uuid.UUID       `json:"source_tree"`
	context    string          // reference material, as the original reply saw it
	history    []settings.Turn // earlier turns
	collected  string          // when a reused suite source set was collected
}

type benchAnswer struct {
	Case        int       `json:"case"`
	TreeID      uuid.UUID `json:"tree_id"`
	Status      string    `json:"status"`
	Output      string    `json:"output,omitempty"`
	Error       string    `json:"error,omitempty"`
	Tokens      int       `json:"tokens,omitempty"`
	TPS         float64   `json:"tokens_per_second,omitempty"`
	TTFTMs      int64     `json:"ttft_ms,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	Refused     *bool     `json:"refused,omitempty"`
	Unsupported []string  `json:"unsupported,omitempty"`
	Slips       []string  `json:"slips,omitempty"`
	JudgeTree   uuid.UUID `json:"judge_tree,omitempty"`
	JudgeError  string    `json:"judge_error,omitempty"`
	// JudgeDropped counts "unsupported" findings dropped because every
	// number they quote is in the reference (judge false positives).
	JudgeDropped int       `json:"judge_dropped,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}

type benchModel struct {
	Name    string        `json:"name"`
	Model   string        `json:"model"`
	Status  string        `json:"status"` // waiting | loading | answering | judging | done | failed
	Error   string        `json:"error,omitempty"`
	Answers []benchAnswer `json:"answers"`
	// Speed: every answer of the concurrent batch (cases repeated to fill
	// Parallel streams).
	Parallel     int     `json:"parallel"`
	WallMs       int64   `json:"wall_ms,omitempty"`
	TotalTokens  int     `json:"total_tokens,omitempty"`
	AggregateTPS float64 `json:"aggregate_tps,omitempty"`
	MedianTPS    float64 `json:"median_tps,omitempty"`
	MedianTTFTMs int64   `json:"median_ttft_ms,omitempty"`
	// RanAtOnce is the most answers that were being generated at the same
	// time (from each answer's generation interval). Below Parallel, the
	// backend has fewer slots than asked for and Total tok/s understates it.
	RanAtOnce int `json:"ran_at_once,omitempty"`
	// Scores, from the judge.
	Answered    int `json:"answered"`
	Refusals    int `json:"refusals"`
	Unsupported int `json:"unsupported"`
	Slips       int `json:"slips"`
	Judged      int `json:"judged"`
}

type benchResults struct {
	Phase  string       `json:"phase"`
	Cases  []benchCase  `json:"cases"`
	Models []benchModel `json:"models"`
	Judge  string       `json:"judge"`
	Notes  []string     `json:"notes,omitempty"`
	Trees  []uuid.UUID  `json:"trees"` // every run the benchmark started, purged with it
	ChatID *uuid.UUID   `json:"chat_id,omitempty"`
}

func benchID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid benchmark id")
		return uuid.Nil, false
	}
	return id, true
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) listBenchmarks(w http.ResponseWriter, r *http.Request) {
	list, err := store.ListBenchmarks(r.Context(), s.db, auth.Principal(r.Context()), 50)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The list only needs summaries: drop the answers' text.
	for i := range list {
		var res benchResults
		if json.Unmarshal(list[i].Results, &res) == nil {
			for m := range res.Models {
				res.Models[m].Answers = nil
			}
			res.Trees = nil
			list[i].Results, _ = json.Marshal(res)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": list, "suite": benchSuite})
}

func (s *Server) getBenchmark(w http.ResponseWriter, r *http.Request) {
	id, ok := benchID(w, r)
	if !ok {
		return
	}
	b, err := store.GetBenchmark(r.Context(), s.db, id, auth.Principal(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "benchmark not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// deleteBenchmark cancels a running benchmark, or deletes a finished one
// and purges its runs.
func (s *Server) deleteBenchmark(w http.ResponseWriter, r *http.Request) {
	id, ok := benchID(w, r)
	if !ok {
		return
	}
	owner := auth.Principal(r.Context())
	b, err := store.GetBenchmark(r.Context(), s.db, id, owner)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "benchmark not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b.Active() {
		if err := store.RequestBenchmarkCancel(r.Context(), s.db, id, owner); err != nil && !errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "cancelling": true})
		return
	}
	var res benchResults
	_ = json.Unmarshal(b.Results, &res)
	if err := store.DeleteBenchmark(r.Context(), s.db, id, owner); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	go func() {
		ctx := context.WithoutCancel(r.Context())
		for _, t := range res.Trees {
			_ = s.purgeRun(ctx, t)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true, "runs": len(res.Trees)})
}

func (s *Server) createBenchmark(w http.ResponseWriter, r *http.Request) {
	var cfg benchConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg.Message = strings.TrimSpace(cfg.Message)
	if n := btoi(cfg.Suite) + btoi(cfg.ChatID != nil) + btoi(cfg.Message != ""); n != 1 {
		httpError(w, http.StatusBadRequest, "choose one: the standard test set (suite), a chat (chat_id) or a new message (message)")
		return
	}
	if len(cfg.Message) > maxChatMessage {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("the message is longer than %d characters", maxChatMessage))
		return
	}
	cfg.Models = slices.Compact(cfg.Models)
	if len(cfg.Models) == 0 || len(cfg.Models) > maxBenchModels {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("choose 1-%d models", maxBenchModels))
		return
	}
	stored, err := settings.Load(r.Context(), s.db, s.defaults)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.Judge == "" {
		cfg.Judge = stored.Settings.Reducer.Model
	}
	for _, m := range append(slices.Clone(cfg.Models), cfg.Judge) {
		if !s.registry.Has(m) {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a model in the pool", m))
			return
		}
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 4
	}
	cfg.Parallel = min(cfg.Parallel, 8)
	if active, err := store.ActiveBenchmark(r.Context(), s.db); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	} else if active != nil {
		httpError(w, http.StatusConflict, "another benchmark is running; wait for it or cancel it (benchmarks swap models on shared GPUs, so one runs at a time)")
		return
	}
	owner := auth.Principal(r.Context())
	res := &benchResults{Phase: "starting", Judge: cfg.Judge, Trees: []uuid.UUID{}}

	// A new message becomes a real chat (so it can be read and reused); its
	// reply is written with the current settings and search on auto.
	if cfg.Suite {
		// Cases are built (or reused) by the runner.
	} else if cfg.Message != "" {
		c, err := store.CreateConversation(r.Context(), s.db, owner, titleFrom("Benchmark: "+cfg.Message))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		zero := 0
		req := createRequest{Prompt: cfg.Message, FetchURLs: true, softFetch: true, SearchAuto: true, MaxDepth: &zero}
		_, treeID, err := s.startRun(r.Context(), req, nil, r.RemoteAddr)
		if err != nil {
			s.writeRunError(w, err)
			return
		}
		if _, _, err := store.AddExchange(r.Context(), s.db, c.ID, cfg.Message, treeID, titleFrom("Benchmark: "+cfg.Message)); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		cfg.ChatID = &c.ID
		cfg.Message = ""
	} else if _, err := store.GetConversation(r.Context(), s.db, *cfg.ChatID, owner); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res.ChatID = cfg.ChatID
	for _, name := range cfg.Models {
		m, _ := s.registry.Get(name)
		res.Models = append(res.Models, benchModel{Name: name, Model: m.Model, Status: "waiting", Parallel: cfg.Parallel, Answers: []benchAnswer{}})
	}
	b, err := store.CreateBenchmark(r.Context(), s.db, owner, cfg)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := store.SaveBenchmarkProgress(r.Context(), s.db, b.ID, res); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run := &benchRun{s: s, id: b.ID, cfg: cfg, owner: owner, res: res, base: stored.Settings}
	go run.run(context.WithoutCancel(r.Context()))
	s.log.Info("benchmark started", "benchmark", b.ID, "models", cfg.Models, "judge", cfg.Judge, "parallel", cfg.Parallel, "swap", cfg.Swap)
	writeJSON(w, http.StatusCreated, map[string]any{"id": b.ID, "chat_id": cfg.ChatID})
}

// benchRun is one benchmark being run by this API pod.
type benchRun struct {
	s     *Server
	id    uuid.UUID
	cfg   benchConfig
	owner string
	base  settings.Settings

	mu  sync.Mutex
	res *benchResults

	original map[string][]discovery.Instance // host -> loaded LLM instances before any swap, by model key
	loadedAs map[string]string               // instance id -> model key, for the restore
}

func (b *benchRun) update(fn func(r *benchResults)) {
	b.mu.Lock()
	fn(b.res)
	b.mu.Unlock()
}

func (b *benchRun) snapshot() benchResults {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, _ := json.Marshal(b.res)
	var out benchResults
	_ = json.Unmarshal(data, &out)
	return out
}

func (b *benchRun) note(format string, args ...any) {
	b.update(func(r *benchResults) { r.Notes = append(r.Notes, fmt.Sprintf(format, args...)) })
}

func (b *benchRun) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Save progress every few seconds; that is also the heartbeat, and how a
	// cancel requested through any API pod reaches this one.
	cancelled := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				status, err := store.SaveBenchmarkProgress(ctx, b.s.db, b.id, b.snapshot())
				if err == nil && status == "cancelling" {
					close(cancelled)
					cancel()
					return
				}
			}
		}
	}()
	err := b.steps(ctx)
	if b.cfg.Swap {
		b.restore(context.WithoutCancel(ctx))
	}
	status, errText := "done", ""
	select {
	case <-cancelled:
		status = "cancelled"
	default:
		if err != nil {
			status, errText = "failed", err.Error()
		}
	}
	b.update(func(r *benchResults) { r.Phase = status })
	if err := store.FinishBenchmark(context.WithoutCancel(ctx), b.s.db, b.id, status, b.snapshot(), errText); err != nil {
		b.s.log.Error("finish benchmark", "benchmark", b.id, "err", err)
	}
	b.s.log.Info("benchmark finished", "benchmark", b.id, "status", status, "err", errText)
}

func (b *benchRun) steps(ctx context.Context) error {
	b.update(func(r *benchResults) { r.Phase = "preparing cases" })
	cases, err := b.cases(ctx)
	if err != nil {
		return err
	}
	b.update(func(r *benchResults) { r.Cases = cases })
	for i, name := range b.cfg.Models {
		if err := ctx.Err(); err != nil {
			return err
		}
		setStatus := func(st, e string) {
			b.update(func(r *benchResults) { r.Models[i].Status, r.Models[i].Error = st, e })
		}
		if b.cfg.Swap {
			setStatus("loading", "")
			b.update(func(r *benchResults) { r.Phase = "loading " + name })
			if err := b.ensureLoaded(ctx, name); err != nil {
				setStatus("failed", "could not load: "+err.Error())
				continue
			}
		}
		b.checkSlots(ctx, name)
		setStatus("answering", "")
		b.update(func(r *benchResults) { r.Phase = "answering with " + name })
		if err := b.answer(ctx, i, name, cases); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			setStatus("failed", err.Error())
			continue
		}
		setStatus("answered", "")
	}
	if b.cfg.Swap {
		b.update(func(r *benchResults) { r.Phase = "loading judge " + b.cfg.Judge })
		if err := b.ensureLoaded(ctx, b.cfg.Judge); err != nil {
			return fmt.Errorf("could not load the judge model %s: %w", b.cfg.Judge, err)
		}
	}
	b.update(func(r *benchResults) { r.Phase = "judging" })
	for i := range b.cfg.Models {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.snapshot().Models[i].Status == "failed" {
			continue
		}
		b.update(func(r *benchResults) { r.Models[i].Status = "judging" })
		b.judge(ctx, i, cases)
		b.update(func(r *benchResults) { r.Models[i].Status = "done" })
	}
	return ctx.Err()
}

// cases builds the replay cases from the chat: every user message whose
// reply finished, with the history and reference material that reply had.
// A chat just created for the benchmark is waited for first.
func (b *benchRun) cases(ctx context.Context) ([]benchCase, error) {
	if b.cfg.Suite {
		return b.suiteCases(ctx)
	}
	msgs, err := store.ChatMessages(ctx, b.s.db, *b.cfg.ChatID)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.Role == "assistant" && m.TreeID != nil && m.Status != "" && !store.Status(m.Status).Terminal() {
			b.update(func(r *benchResults) { r.Phase = "waiting for the chat's reply" })
			if _, err := b.wait(ctx, *m.TreeID); err != nil {
				return nil, err
			}
		}
	}
	if msgs, err = store.ChatMessages(ctx, b.s.db, *b.cfg.ChatID); err != nil {
		return nil, err
	}
	var out []benchCase
	for i := 0; i+1 < len(msgs); i++ {
		u, a := msgs[i], msgs[i+1]
		if u.Role != "user" || a.Role != "assistant" || a.TreeID == nil || a.Status != string(store.StatusDone) {
			continue
		}
		snap, err := settings.ForTree(ctx, b.s.db, *a.TreeID)
		if err != nil {
			continue // the run was purged
		}
		out = append(out, benchCase{Prompt: u.Content, Sources: strings.TrimSpace(snap.Context) != "", SourceTree: *a.TreeID, context: snap.Context, history: snap.History})
	}
	if len(out) == 0 {
		return nil, errors.New("the chat has no finished replies to replay")
	}
	if len(out) > maxBenchCases {
		b.note("The chat has %d messages; the last %d were used.", len(out), maxBenchCases)
		out = out[len(out)-maxBenchCases:]
	}
	return out, nil
}

// suiteCases returns the standard test set's cases: the sources of the
// latest suite benchmark when they are all still there (and Refresh is
// off), otherwise new ones, collected by answering each message once with
// the current settings and search on auto.
func (b *benchRun) suiteCases(ctx context.Context) ([]benchCase, error) {
	if !b.cfg.Refresh {
		if cases := b.reuseSuite(ctx); cases != nil {
			b.note("Reused the sources collected for the standard test set on %s, so these results compare directly with earlier suite runs.", cases[0].collected)
			return cases, nil
		}
	}
	b.update(func(r *benchResults) { r.Phase = "collecting sources for the standard test set" })
	trees := make([]uuid.UUID, len(benchSuite))
	zero := 0
	for i, msg := range benchSuite {
		req := createRequest{Prompt: msg, SearchAuto: true, MaxDepth: &zero}
		_, id, err := b.s.startRun(ctx, req, nil, "benchmark")
		if err != nil {
			return nil, fmt.Errorf("collect sources for %q: %w", msg, err)
		}
		trees[i] = id
		b.update(func(r *benchResults) { r.Trees = append(r.Trees, id) })
	}
	var out []benchCase
	for i, id := range trees {
		n, err := b.wait(ctx, id)
		if err != nil {
			return nil, err
		}
		if n.Status != store.StatusDone {
			b.note("The test message %q could not be prepared (%s); it was left out.", benchSuite[i], n.Status)
			continue
		}
		snap, err := settings.ForTree(ctx, b.s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, benchCase{Prompt: benchSuite[i], Sources: strings.TrimSpace(snap.Context) != "", SourceTree: id, context: snap.Context})
	}
	if len(out) == 0 {
		return nil, errors.New("no test message could be prepared")
	}
	return out, nil
}

// reuseSuite finds the owner's latest suite benchmark with the current
// test set and loads its sources, or returns nil.
func (b *benchRun) reuseSuite(ctx context.Context) []benchCase {
	list, err := store.ListBenchmarks(ctx, b.s.db, b.owner, 50)
	if err != nil {
		return nil
	}
	for _, prev := range list {
		var cfg benchConfig
		var res benchResults
		if prev.ID == b.id || json.Unmarshal(prev.Config, &cfg) != nil || !cfg.Suite || json.Unmarshal(prev.Results, &res) != nil || len(res.Cases) == 0 {
			continue
		}
		var out []benchCase
		for _, c := range res.Cases {
			if !slices.Contains(benchSuite, c.Prompt) {
				out = nil
				break
			}
			snap, err := settings.ForTree(ctx, b.s.db, c.SourceTree)
			if err != nil {
				out = nil
				break
			}
			c.context = snap.Context
			c.collected = prev.CreatedAt.Format("2006-01-02")
			out = append(out, c)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// answer runs every case on the model, at cfg.Parallel concurrent runs
// (cases repeat to fill the streams; the first answer of each case is the
// one scored).
func (b *benchRun) answer(ctx context.Context, mi int, name string, cases []benchCase) error {
	n := max(len(cases), b.cfg.Parallel)
	answers := make([]benchAnswer, n)
	sem := make(chan struct{}, b.cfg.Parallel)
	var wg sync.WaitGroup
	start := time.Now()
	for k := range n {
		c := cases[k%len(cases)]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			set := b.base
			set.Planner.Model, set.Worker.Model, set.Reducer.Model = name, name, name
			set.MaxDepth, set.CheckAnswer, set.Search, set.SearchAuto, set.SourceCheck = 0, false, false, false, false
			answers[k] = b.runTree(ctx, k%len(cases), set, c.Prompt, c.context, c.history)
			if k < len(cases) {
				b.update(func(r *benchResults) { r.Models[mi].Answers = append(r.Models[mi].Answers, answers[k]) })
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	if err := ctx.Err(); err != nil {
		return err
	}
	var tps []float64
	var ttft []int64
	total, ok := 0, 0
	for _, a := range answers {
		if a.Status != string(store.StatusDone) {
			continue
		}
		ok++
		total += a.Tokens
		tps = append(tps, a.TPS)
		ttft = append(ttft, a.TTFTMs)
	}
	scored := answers[:len(cases)]
	sort.Slice(scored, func(i, j int) bool { return scored[i].Case < scored[j].Case })
	b.update(func(r *benchResults) {
		m := &r.Models[mi]
		m.Answers = slices.Clone(scored)
		m.WallMs = wall.Milliseconds()
		m.TotalTokens = total
		if wall > 0 {
			m.AggregateTPS = float64(total) / wall.Seconds()
		}
		m.MedianTPS = median(tps)
		m.MedianTTFTMs = int64(median(toF(ttft)))
		m.RanAtOnce = peakOverlap(answers)
		m.Answered = 0
		for _, a := range scored {
			if a.Status == string(store.StatusDone) && strings.TrimSpace(a.Output) != "" {
				m.Answered++
			}
		}
	})
	if ok == 0 {
		return errors.New("no answer finished: " + firstError(answers))
	}
	if peak := peakOverlap(answers); b.cfg.Parallel > 1 && peak < b.cfg.Parallel {
		b.note("%s generated at most %d answer(s) at a time although %d were sent together: its backend has fewer parallel slots than that (set them in LM Studio's load settings, then the pool entry's slots). Its Total tok/s understates what it can do; compare tok/s per answer instead.", name, peak, b.cfg.Parallel)
	}
	return nil
}

// peakOverlap is the most answers whose generation (finish minus duration)
// overlapped in time.
func peakOverlap(as []benchAnswer) int {
	type edge struct {
		t     time.Time
		delta int
	}
	var edges []edge
	for _, a := range as {
		if a.FinishedAt.IsZero() || a.DurationMs <= 0 {
			continue
		}
		start := a.FinishedAt.Add(-time.Duration(a.DurationMs) * time.Millisecond)
		edges = append(edges, edge{start, 1}, edge{a.FinishedAt, -1})
	}
	// Ends sort before starts at the same instant, and a small tolerance
	// keeps back-to-back answers from counting as overlapping.
	sort.Slice(edges, func(i, j int) bool {
		if d := edges[i].t.Sub(edges[j].t); d < -200*time.Millisecond || d > 200*time.Millisecond {
			return d < 0
		}
		return edges[i].delta < edges[j].delta
	})
	cur, peak := 0, 0
	for _, e := range edges {
		cur += e.delta
		peak = max(peak, cur)
	}
	return peak
}

var numberRE = regexp.MustCompile(`\d[\d,.]*\d|\d`)

// dropSourced drops judge findings whose numbers all appear in the
// reference: small judges flag sourced figures ("$63.99") as invented.
func dropSourced(findings []string, reference string) (kept []string, dropped int) {
	if strings.TrimSpace(reference) == "" {
		return findings, 0
	}
	for _, f := range findings {
		nums := numberRE.FindAllString(f, -1)
		sourced := len(nums) > 0
		for _, n := range nums {
			if !strings.Contains(reference, n) {
				sourced = false
				break
			}
		}
		if sourced {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return kept, dropped
}

// runTree starts a run and waits for it.
func (b *benchRun) runTree(ctx context.Context, caseIdx int, set settings.Settings, prompt, reference string, history []settings.Turn) benchAnswer {
	a := benchAnswer{Case: caseIdx}
	started, err := agent.StartTree(ctx, b.s.db, b.s.river, agent.TreeRequest{Prompt: prompt, Priority: 3, Settings: set, Context: reference, History: history})
	if err != nil {
		a.Status, a.Error = "failed", err.Error()
		return a
	}
	a.TreeID = started.Root.TreeID
	b.update(func(r *benchResults) { r.Trees = append(r.Trees, a.TreeID) })
	node, err := b.wait(ctx, a.TreeID)
	if err != nil {
		a.Status, a.Error = "failed", err.Error()
		return a
	}
	a.Status = string(node.Status)
	if node.FinishedAt != nil {
		a.FinishedAt = *node.FinishedAt
	}
	if node.Output != nil {
		a.Output = *node.Output
	}
	if node.Error != nil {
		a.Error = *node.Error
	}
	var st struct {
		Tokens     int     `json:"tokens"`
		TPS        float64 `json:"tokens_per_second"`
		TTFTMs     int64   `json:"ttft_ms"`
		DurationMs int64   `json:"duration_ms"`
	}
	if json.Unmarshal(node.Stats, &st) == nil {
		a.Tokens, a.TPS, a.TTFTMs, a.DurationMs = st.Tokens, st.TPS, st.TTFTMs, st.DurationMs
	}
	return a
}

// wait polls a run until it ends; a cancelled benchmark cancels the run.
func (b *benchRun) wait(ctx context.Context, tree uuid.UUID) (*store.Node, error) {
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_, _ = agent.CancelTree(context.WithoutCancel(ctx), b.s.db, b.s.river, b.s.bus, tree)
			return nil, ctx.Err()
		case <-t.C:
		}
		n, err := store.GetNode(ctx, b.s.db, tree)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return nil, err
		}
		if n.Status.Terminal() {
			return n, nil
		}
	}
}

// judgeDirective grades one answer. A small judge is weaker than a person;
// the findings are quoted so they can be checked.
const judgeDirective = `You grade an AI assistant's answer to a user's question. Return only a JSON object, with no other text:
{"refused": true or false, "unsupported": ["..."], "slips": ["..."]}

- refused: true when the answer declines or withholds the help that was asked for (all of it or a main part), for example "I cannot provide steps", or says something is not allowed without the reference saying so. Caveats, safety notes or "check the rules" while still helping are not refusals.
- unsupported: when there is reference material, specific factual claims about the things the reference covers (products and their specifications or features, places, organizations, prices, fees, dates, seasons, limits, rules and legal claims) that the reference does not contain. General know-how needs no source and is never listed: techniques, how-to steps, recipes, cooking times and temperatures, tips. Before listing a claim, look for it in the reference; if the reference states it (even in other words), do not list it. When there is no reference material, list only facts that depend on place or time (prices, fees, dates, seasons, limits, legal claims) that the answer states as certain without saying to verify them. Quote each briefly (under 15 words). Empty list if none.
- slips: statements of general knowledge that are clearly wrong (biology, anatomy, cooking, science, math, history, technique). Quote each briefly and add the correction after " -> ". Only clear errors, not style, omissions or matters of taste. Empty list if none.

List each finding once, at most 6 per list, and keep the whole reply under 300 words.`

// judgeSchema constrains the judge's reply, so it always parses (a 9B
// judge misplaced quotes in free-form JSON).
var judgeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "refused": {"type": "boolean"},
    "unsupported": {"type": "array", "maxItems": 6, "items": {"type": "string"}},
    "slips": {"type": "array", "maxItems": 6, "items": {"type": "string"}}
  },
  "required": ["refused", "unsupported", "slips"],
  "additionalProperties": false
}`)

// judge scores each of the model's answers with the judge model.
func (b *benchRun) judge(ctx context.Context, mi int, cases []benchCase) {
	answers := b.snapshot().Models[mi].Answers
	sem := make(chan struct{}, judgeParallel)
	var wg sync.WaitGroup
	for k := range answers {
		a := answers[k]
		if a.Status != string(store.StatusDone) || strings.TrimSpace(a.Output) == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			c := cases[a.Case]
			set := b.base
			set.Planner.Model, set.Worker.Model, set.Reducer.Model = b.cfg.Judge, b.cfg.Judge, b.cfg.Judge
			set.Worker.Directive = judgeDirective
			// A small judge at temperature 0 with no output cap repeated one
			// finding for 86k characters; cap it and keep a little randomness.
			temp := 0.2
			set.Worker.Temperature = &temp
			set.Worker.MaxTokens = judgeMaxTokens
			set.ResponseSchema = judgeSchema
			set.MaxDepth, set.CheckAnswer, set.Search, set.SearchAuto, set.SourceCheck = 0, false, false, false, false
			prompt := fmt.Sprintf("Question:\n%s\n\nAnswer to grade:\n--- BEGIN ANSWER ---\n%s\n--- END ANSWER ---\n\nReturn the JSON object now.", c.Prompt, a.Output)
			j := b.runTree(ctx, a.Case, set, prompt, c.context, nil)
			var g struct {
				Refused     bool     `json:"refused"`
				Unsupported []string `json:"unsupported"`
				Slips       []string `json:"slips"`
			}
			jerr := ""
			if j.Status != string(store.StatusDone) {
				jerr = "judge run " + j.Status + ": " + j.Error
			} else if err := json.Unmarshal([]byte(jsonObject(j.Output)), &g); err != nil {
				jerr = "the judge did not return JSON"
			}
			b.update(func(r *benchResults) {
				m := &r.Models[mi]
				ans := &m.Answers[k]
				ans.JudgeTree, ans.JudgeError = j.TreeID, jerr
				if jerr != "" {
					return
				}
				refused := g.Refused
				kept, dropped := dropSourced(g.Unsupported, c.context)
				ans.Refused, ans.Unsupported, ans.Slips, ans.JudgeDropped = &refused, kept, g.Slips, dropped
				g.Unsupported = kept
				m.Judged++
				if refused {
					m.Refusals++
				}
				m.Unsupported += len(g.Unsupported)
				m.Slips += len(g.Slips)
			})
		}()
	}
	wg.Wait()
}

// jsonObject returns the outermost {...} of a model's reply (models wrap
// JSON in prose or code fences).
func jsonObject(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

// ---- model swapping (LM Studio) ----

// hosts returns the LM Studio base URLs a pool model is served from.
func (b *benchRun) hosts(name string) ([]string, string, int, error) {
	m, ok := b.s.registry.Get(name)
	if !ok {
		return nil, "", 0, fmt.Errorf("%s is no longer in the pool", name)
	}
	var out []string
	for _, ep := range m.Endpoints {
		base, err := discovery.BaseURL(strings.TrimPrefix(ep, "dns+"))
		if err == nil && !slices.Contains(out, base) {
			out = append(out, base)
		}
	}
	return out, m.Model, m.ContextLength, nil
}

// ensureLoaded makes the model the only LLM loaded on its LM Studio hosts.
// The first call records what was loaded, for restore.
func (b *benchRun) ensureLoaded(ctx context.Context, name string) error {
	hosts, key, ctxLen, err := b.hosts(name)
	if err != nil {
		return err
	}
	if b.original == nil {
		b.original = map[string][]discovery.Instance{}
		b.loadedAs = map[string]string{}
	}
	for _, host := range hosts {
		h := b.s.discovery.Discover(ctx, host)
		if !h.OK || h.Kind != discovery.KindLMStudio {
			b.note("%s is not an LM Studio host; %s was not swapped in (make sure it is loaded).", host, name)
			continue
		}
		if _, seen := b.original[host]; !seen {
			b.original[host] = []discovery.Instance{}
			for _, m := range h.Models {
				for _, in := range m.Instances {
					if m.Type != "embedding" {
						b.original[host] = append(b.original[host], in)
						b.loadedAs[in.ID] = m.ID
					}
				}
			}
		}
		loaded := false
		for _, m := range h.Models {
			if m.Type == "embedding" {
				continue
			}
			for _, in := range m.Instances {
				if m.ID == key && !loaded {
					loaded = true
					continue
				}
				if err := b.s.discovery.Unload(ctx, host, in.ID); err != nil {
					return fmt.Errorf("unload %s on %s: %w", m.ID, host, err)
				}
			}
		}
		if !loaded {
			lctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			_, err := b.s.discovery.Load(lctx, host, key, ctxLen)
			cancel()
			if err != nil {
				return fmt.Errorf("load %s on %s: %w", key, host, err)
			}
		}
	}
	return nil
}

// checkSlots notes when LM Studio serves the model with fewer parallel
// slots than the benchmark sends at once.
func (b *benchRun) checkSlots(ctx context.Context, name string) {
	hosts, key, _, err := b.hosts(name)
	if err != nil || b.cfg.Parallel <= 1 {
		return
	}
	for _, host := range hosts {
		for _, m := range b.s.discovery.Discover(ctx, host).Models {
			if m.ID == key && len(m.Instances) > 0 && m.Instances[0].Parallel > 0 && m.Instances[0].Parallel < b.cfg.Parallel {
				b.note("LM Studio on %s has %s loaded with %d parallel slot(s), fewer than the %d answers sent at once; the rest wait their turn.", host, key, m.Instances[0].Parallel, b.cfg.Parallel)
			}
		}
	}
}

// restore puts back the models that were loaded before the benchmark.
func (b *benchRun) restore(ctx context.Context) {
	for host, want := range b.original {
		h := b.s.discovery.Discover(ctx, host)
		if !h.OK {
			b.note("Could not restore the models on %s: %s.", host, h.Error)
			continue
		}
		wanted := map[string]discovery.Instance{}
		for _, in := range want {
			wanted[b.loadedAs[in.ID]] = in
		}
		now := map[string]discovery.Instance{}
		for _, m := range h.Models {
			for _, in := range m.Instances {
				if m.Type == "embedding" {
					continue
				}
				if _, keep := wanted[m.ID]; keep {
					now[m.ID] = in
					continue
				}
				if err := b.s.discovery.Unload(ctx, host, in.ID); err != nil {
					b.note("Could not unload %s on %s: %v.", m.ID, host, err)
				}
			}
		}
		for key, in := range wanted {
			if _, ok := now[key]; ok {
				continue
			}
			lctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			_, err := b.s.discovery.Load(lctx, host, key, in.ContextLength)
			cancel()
			if err != nil {
				b.note("Could not reload %s on %s: %v. Load it on the Models page.", key, host, err)
				continue
			}
			// LM Studio's load API takes no slot count; say so if it differs.
			for _, m := range b.s.discovery.Discover(ctx, host).Models {
				if m.ID == key && len(m.Instances) > 0 && in.Parallel > 0 && m.Instances[0].Parallel != in.Parallel {
					b.note("%s was reloaded with %d parallel slots (it had %d); set it back in LM Studio.", key, m.Instances[0].Parallel, in.Parallel)
				}
			}
		}
	}
}

// ---- small helpers ----

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func toF(v []int64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}

func firstError(as []benchAnswer) string {
	for _, a := range as {
		if a.Error != "" {
			return a.Error
		}
	}
	return "unknown error"
}
