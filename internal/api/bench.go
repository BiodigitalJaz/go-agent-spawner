package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
)

func (s *Server) benchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/benchmarks", s.listBenchmarks)
	mux.HandleFunc("POST /v1/benchmarks", s.createBenchmark)
	mux.HandleFunc("GET /v1/benchmarks/{id}", s.getBenchmark)
	mux.HandleFunc("DELETE /v1/benchmarks/{id}", s.deleteBenchmark)
}

type benchConfig struct {
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
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": list})
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
	if (cfg.ChatID == nil) == (cfg.Message == "") {
		httpError(w, http.StatusBadRequest, "choose a chat (chat_id) or write a new message (message)")
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
	if cfg.Message != "" {
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
	return nil
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
- unsupported: facts that depend on place or time (fees, prices, dates, seasons, limits, equipment rules, required licenses or permits, legal claims) stated in the answer that the reference material does not contain. When there is no reference material, list such facts the answer states as certain without saying to verify them. Quote each briefly (under 15 words). Empty list if none.
- slips: statements of general knowledge that are clearly wrong (biology, anatomy, cooking, science, math, history, technique). Quote each briefly and add the correction after " -> ". Only clear errors, not style or omissions. Empty list if none.`

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
			zero := 0.0
			set.Worker.Temperature = &zero
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
				ans.Refused, ans.Unsupported, ans.Slips = &refused, g.Unsupported, g.Slips
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
