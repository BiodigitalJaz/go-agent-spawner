// Package api exposes task trees over HTTP: submit, inspect, cancel, live
// Server-Sent Events backed by the NATS event bus, and the editable agent
// settings the UI manages.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/discovery"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/models"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

type Server struct {
	log       *slog.Logger
	db        *pgxpool.Pool
	river     *river.Client[pgx.Tx]
	bus       *events.Bus
	registry  *models.Registry
	discovery *discovery.Client
	defaults  settings.Settings
	workers   *workerRegistry
	// SearchEnabled: the workers have a search backend (SEARCH_URL).
	SearchEnabled bool
	// SearchBudget reports the paid provider's spend this month (Exa).
	SearchBudget fetch.Budget
}

func New(log *slog.Logger, db *pgxpool.Pool, client *river.Client[pgx.Tx], bus *events.Bus, registry *models.Registry, defaults settings.Settings) *Server {
	return &Server{log: log, db: db, river: client, bus: bus, registry: registry, discovery: discovery.New(), defaults: defaults, workers: newWorkerRegistry()}
}

// Start begins tracking worker heartbeats for the models view.
func (s *Server) Start(ctx context.Context) error {
	return s.bus.WatchWorkers(ctx, s.workers.update)
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/trees", s.createTree)
	mux.HandleFunc("GET /v1/trees", s.listTrees)
	mux.HandleFunc("GET /v1/trees/{id}", s.getTree)
	mux.HandleFunc("DELETE /v1/trees/{id}", s.cancelTree)
	s.chatRoutes(mux)
	s.benchRoutes(mux)
	mux.HandleFunc("GET /v1/trees/{id}/events", s.streamTree)
	mux.HandleFunc("GET /v1/events", s.streamAll)
	mux.HandleFunc("GET /v1/settings", s.getSettings)
	mux.HandleFunc("PUT /v1/settings", s.putSettings)
	mux.HandleFunc("GET /v1/models", s.getModels)
	mux.HandleFunc("POST /v1/models", s.createModel)
	mux.HandleFunc("PUT /v1/models/{name}", s.updateModel)
	mux.HandleFunc("DELETE /v1/models/{name}", s.deleteModel)
	mux.HandleFunc("GET /v1/discovery", s.discover)
	mux.HandleFunc("POST /v1/discovery/load", s.loadModel)
	mux.HandleFunc("POST /v1/discovery/unload", s.unloadModel)
}

func (s *Server) knownModel(name string) bool { return s.registry.Has(name) }

type createRequest struct {
	Prompt   string `json:"prompt"`
	Priority int    `json:"priority,omitempty"`
	// Optional per-request overrides of the saved settings.
	Model       string `json:"model,omitempty"` // worker model
	MaxDepth    *int   `json:"max_depth,omitempty"`
	MaxChildren *int   `json:"max_children,omitempty"`
	// Document mode: the prompt is the instruction, the document is split in
	// code into Chunks parts. 0 (or omitted) chooses automatically: one
	// agent up to about 6k tokens, more chunks above; 1 = one agent.
	Document string `json:"document,omitempty"`
	Chunks   int    `json:"chunks,omitempty"`
	// Merge overrides how split results are combined: llm, concat or list.
	Merge string `json:"merge,omitempty"`
	// Parts are given pieces of input, one agent each (e.g. files).
	Parts []agent.Part `json:"parts,omitempty"`
	// MaxPartTokens splits parts larger than this into sections (default
	// 6000; -1 keeps every part whole).
	MaxPartTokens int `json:"max_part_tokens,omitempty"`
	// PartMaxWords asks each part's agent for a short answer; the merge
	// keeps the full prompt.
	PartMaxWords int `json:"part_max_words,omitempty"`
	// Context is reference material sent to every agent (planner, workers
	// and merge), e.g. a page under review or rules the answer must respect.
	Context string `json:"context,omitempty"`
	// FetchURLs downloads the web pages the prompt links to and adds their
	// text to the context. Without it (or content), a linked page is refused.
	FetchURLs bool `json:"fetch_urls,omitempty"`
	// Check overrides the check_answer setting for this run: a rules check
	// of the final answer against the reference material.
	Check *bool `json:"check,omitempty"`
	// Search runs a web search before planning and gives every agent the
	// results (needs SEARCH_URL on the server).
	Search bool `json:"search,omitempty"`
	// SearchAuto searches only when the question needs current or local
	// facts (the local model decides). Ignored without a search backend.
	SearchAuto bool `json:"search_auto,omitempty"`
	// SourceCheck reviews an answer written from search results against
	// them (see settings.SourceCheck).
	SourceCheck bool `json:"source_check,omitempty"`

	// softFetch (chat): a link that cannot be fetched becomes a note for
	// the agent instead of failing the request.
	softFetch bool
}

func (s *Server) createTree(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, _, err := s.startRun(r.Context(), req, nil, r.RemoteAddr)
	if err != nil {
		s.writeRunError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// writeRunError answers a startRun error: request errors with their status,
// anything else as a 500.
func (s *Server) writeRunError(w http.ResponseWriter, err error) {
	var re *requestError
	if errors.As(err, &re) {
		httpError(w, re.status, re.msg)
		return
	}
	httpError(w, http.StatusInternalServerError, err.Error())
}

func badRequest(format string, a ...any) error {
	return &requestError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

// startRun validates a request and starts its tree: the shared path for
// POST /v1/trees and chat messages (which add the conversation history).
func (s *Server) startRun(ctx context.Context, req createRequest, history []settings.Turn, remote string) (map[string]any, uuid.UUID, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, uuid.Nil, badRequest("prompt is required")
	}
	if req.Chunks < 0 || req.Chunks > 64 {
		return nil, uuid.Nil, badRequest("chunks must be between 0 and 64")
	}
	if req.PartMaxWords < 0 || req.PartMaxWords > 5000 {
		return nil, uuid.Nil, badRequest("part_max_words must be between 0 and 5000")
	}
	if len(req.Parts) > 64 {
		return nil, uuid.Nil, badRequest("at most 64 parts")
	}
	for i, p := range req.Parts {
		if strings.TrimSpace(p.Title) == "" || len(p.Title) > 300 {
			return nil, uuid.Nil, badRequest("part %d needs a title of 1-300 characters", i+1)
		}
	}
	stored, err := settings.Load(ctx, s.db, s.defaults)
	if err != nil {
		s.log.Error("load settings", "err", err)
		return nil, uuid.Nil, errors.New("could not load settings")
	}
	set := stored.Settings
	if req.Model != "" {
		set.Worker.Model = req.Model
	}
	if req.MaxDepth != nil {
		set.MaxDepth = *req.MaxDepth
	}
	if req.MaxChildren != nil {
		set.MaxChildren = *req.MaxChildren
	}
	if req.Merge != "" {
		set.Merge = req.Merge
	}
	if err := set.Validate(s.knownModel); err != nil {
		return nil, uuid.Nil, &requestError{http.StatusUnprocessableEntity, err.Error()}
	}
	if req.Priority == 0 {
		req.Priority = 1
	}
	ctxLen := s.contextLength(set.Worker.Model)
	reference, fetched, err := s.reference(ctx, &req, set, remote, agent.MaxReferenceChars(ctxLen))
	if err != nil {
		return nil, uuid.Nil, err
	}
	chunkTokens := 0
	if m, ok := s.registry.Get(set.Worker.Model); ok {
		chunkTokens = m.ChunkTokens
	}
	sizing, err := fitToModel(&req, ctxLen, set.Worker.MaxTokens, chunkTokens, reference)
	if err != nil {
		return nil, uuid.Nil, badRequest("%s", err.Error())
	}
	if req.Check != nil {
		set.CheckAnswer = *req.Check
	}
	// The rules check is for rules the requester supplies; fetched pages and
	// search results are material, not rules.
	if strings.TrimSpace(req.Context) == "" {
		set.CheckAnswer = false
	}
	set.SourceCheck = req.SourceCheck
	if req.Search {
		if !s.SearchEnabled {
			return nil, uuid.Nil, badRequest("web search is not configured on this server (set SEARCH_URL or EXA_API_KEY)")
		}
		set.Search = true
	} else if req.SearchAuto && s.SearchEnabled {
		set.Search, set.SearchAuto = true, true
	}

	started, err := agent.StartTree(ctx, s.db, s.river, agent.TreeRequest{
		Prompt: req.Prompt, Priority: req.Priority, Settings: set, Document: req.Document, Chunks: req.Chunks, Parts: req.Parts,
		MaxPartTokens: req.MaxPartTokens, PartMaxWords: req.PartMaxWords, Context: reference, History: history,
	})
	if errors.Is(err, agent.ErrTooManyParts) {
		return nil, uuid.Nil, badRequest("%s", err.Error())
	}
	if err != nil {
		s.log.Error("start tree", "err", err)
		return nil, uuid.Nil, errors.New("could not start tree")
	}
	root := started.Root
	if err := s.bus.Publish(ctx, events.Event{Type: events.TreeCreated, TreeID: root.TreeID, NodeID: root.ID, Text: truncate(req.Prompt, 200)}); err != nil {
		s.log.Warn("publish tree.created", "err", err)
	}
	if len(started.Children) > 0 {
		kids := make([]events.Child, len(started.Children))
		for i, c := range started.Children {
			kids[i] = events.Child{ID: c.ID, Title: c.Title, Role: c.Role, Prompt: truncate(c.Prompt, 400)}
		}
		_ = s.bus.Publish(ctx, events.Event{Type: events.NodeSplit, TreeID: root.TreeID, NodeID: root.ID, Role: string(agent.RolePlan),
			Text: splitReason(req, len(kids)), Children: kids})
	}
	resp := map[string]any{
		"tree_id":    root.TreeID,
		"events_url": fmt.Sprintf("/v1/trees/%s/events", root.TreeID),
	}
	if sizing != "" {
		resp["sizing"] = sizing
	}
	if req.Document != "" && len(req.Parts) == 0 {
		resp["chunks"] = max(req.Chunks, 1)
	}
	if len(fetched) > 0 {
		resp["fetched"] = fetched
		s.log.Info("tree started with fetched pages", "tree", root.TreeID, "pages", len(fetched), "context_chars", len(reference))
	}
	return resp, root.TreeID, nil
}

func (s *Server) contextLength(model string) int {
	if m, ok := s.registry.Get(model); ok {
		return m.ContextLength
	}
	return 0
}

func (s *Server) listTrees(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	trees, err := store.ListTrees(r.Context(), s.db, limit)
	if err != nil {
		s.log.Error("list trees", "err", err)
		httpError(w, http.StatusInternalServerError, "could not list trees")
		return
	}
	if trees == nil {
		trees = []store.TreeSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"trees": trees})
}

func (s *Server) getTree(w http.ResponseWriter, r *http.Request) {
	id, ok := treeID(w, r)
	if !ok {
		return
	}
	nodes, err := store.TreeNodes(r.Context(), s.db, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(nodes) == 0 {
		httpError(w, http.StatusNotFound, "tree not found")
		return
	}
	set, err := settings.ForTree(r.Context(), s.db, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var root *store.Node
	for _, n := range nodes {
		if n.ID == id {
			root = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tree_id":    id,
		"prompt":     root.Prompt,
		"status":     root.Status,
		"created_at": root.CreatedAt,
		"settings":   set,
		"context":    treeContext(set),
		"nodes":      nodes,
	})
}

func (s *Server) cancelTree(w http.ResponseWriter, r *http.Request) {
	id, ok := treeID(w, r)
	if !ok {
		return
	}
	if p := r.URL.Query().Get("purge"); p == "1" || p == "true" {
		s.purgeTree(w, r, id)
		return
	}
	n, err := agent.CancelTree(r.Context(), s.db, s.river, s.bus, id)
	if err != nil {
		s.log.Error("cancel tree", "tree", id, "err", err)
		httpError(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tree_id": id, "cancelled_jobs": n})
}

// purgeTree deletes every record of a finished tree (DELETE ?purge=1): its
// nodes, prompts, outputs, reference material, River jobs and live events.
// An active tree must be cancelled first, so no agent writes after the purge.
func (s *Server) purgeTree(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	root, err := store.GetNode(r.Context(), s.db, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "tree not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !root.Status.Terminal() {
		httpError(w, http.StatusConflict, "the run is still active; cancel it first, then delete")
		return
	}
	var nodes, jobs int64
	err = pgx.BeginFunc(r.Context(), s.db, func(tx pgx.Tx) error {
		nodes, jobs, err = store.PurgeTree(r.Context(), tx, id)
		return err
	})
	if errors.Is(err, store.ErrTreeBusy) {
		httpError(w, http.StatusConflict, "agents are still stopping; try again in a few seconds")
		return
	}
	if err != nil {
		s.log.Error("purge tree", "tree", id, "err", err)
		httpError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if err := s.bus.PurgeTree(r.Context(), id); err != nil {
		s.log.Warn("purge tree events", "tree", id, "err", err) // they expire with the replay window anyway
	}
	s.log.Info("tree deleted", "tree", id, "nodes", nodes, "jobs", jobs, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"tree_id": id, "deleted": true, "nodes": nodes, "jobs": jobs})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	st, err := settings.Load(r.Context(), s.db, s.defaults)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        st.Version,
		"settings":       st.Settings,
		"updated_at":     st.UpdatedAt,
		"defaults":       s.defaults,
		"search_enabled": s.SearchEnabled,
		"search_budget":  s.searchBudget(r.Context()),
	})
}

type putSettingsRequest struct {
	Version  int               `json:"version"`
	Settings settings.Settings `json:"settings"`
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req putSettingsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := req.Settings.Validate(s.knownModel); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	st, err := settings.Save(r.Context(), s.db, req.Settings, req.Version)
	if errors.Is(err, settings.ErrConflict) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.log.Error("save settings", "err", err)
		httpError(w, http.StatusInternalServerError, "could not save settings")
		return
	}
	s.log.Info("settings saved", "version", st.Version)
	writeJSON(w, http.StatusOK, map[string]any{"version": st.Version, "settings": st.Settings, "updated_at": st.UpdatedAt})
}

type modelView struct {
	Name      string         `json:"name"`
	Model     string         `json:"model"`
	Endpoints []string       `json:"endpoints"`
	Initial   int            `json:"initial_limit,omitempty"`
	Max       int            `json:"max_limit,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
	Context   int            `json:"context_length,omitempty"`
	Slots     int            `json:"slots,omitempty"`
	Shared    bool           `json:"shared_context,omitempty"`
	Chunk     int            `json:"chunk_tokens,omitempty"`
	Source    string         `json:"source"` // config (backends.json) or ui
}

// getModels returns the routable models (API keys omitted) plus each live
// worker's per-endpoint limiter state.
func (s *Server) getModels(w http.ResponseWriter, _ *http.Request) {
	list := s.registry.List()
	out := make([]modelView, len(list))
	for i, m := range list {
		out[i] = modelView{Name: m.Name, Model: m.Model, Endpoints: m.Endpoints, Initial: m.InitialLimit, Max: m.MaxLimit, Extra: m.Extra,
			Context: m.ContextLength, Slots: m.Slots, Shared: m.SharedContext, Chunk: m.ChunkTokens, Source: m.Source}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "workers": s.workers.list()})
}

type modelRequest struct {
	Name         string         `json:"name"`
	Model        string         `json:"model"`
	Endpoints    []string       `json:"endpoints"`
	InitialLimit int            `json:"initial_limit"`
	MaxLimit     int            `json:"max_limit"`
	Extra        map[string]any `json:"extra"`
	Context      int            `json:"context_length"`
	Slots        int            `json:"slots"`
	Shared       bool           `json:"shared_context"`
	ChunkTokens  int            `json:"chunk_tokens"`
}

func (r modelRequest) config() backend.ModelConfig {
	return backend.ModelConfig{Name: strings.TrimSpace(r.Name), Model: strings.TrimSpace(r.Model), Endpoints: r.Endpoints,
		InitialLimit: r.InitialLimit, MaxLimit: r.MaxLimit, Extra: r.Extra, ContextLength: r.Context, Slots: r.Slots, SharedContext: r.Shared, ChunkTokens: r.ChunkTokens}
}

func (s *Server) modelChanged(action, name string) {
	s.log.Info("model "+action, "model", name)
	if err := s.bus.PublishModelsChanged(); err != nil {
		s.log.Warn("publish models changed", "err", err)
	}
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	var req modelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg := req.config()
	switch err := s.registry.Create(r.Context(), cfg); {
	case errors.Is(err, models.ErrExists):
		httpError(w, http.StatusConflict, fmt.Sprintf("a model named %q already exists", cfg.Name))
		return
	case err != nil && strings.HasPrefix(err.Error(), "backend:"):
		httpError(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), "backend: "))
		return
	case err != nil:
		s.log.Error("create model", "err", err)
		httpError(w, http.StatusInternalServerError, "could not create model")
		return
	}
	s.modelChanged("added", cfg.Name)
	writeJSON(w, http.StatusCreated, cfg)
}

func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	var req modelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = r.PathValue("name")
	cfg := req.config()
	switch err := s.registry.Update(r.Context(), cfg); {
	case errors.Is(err, models.ErrReadOnly):
		httpError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, models.ErrNotFound):
		httpError(w, http.StatusNotFound, err.Error())
		return
	case err != nil && strings.HasPrefix(err.Error(), "backend:"):
		httpError(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), "backend: "))
		return
	case err != nil:
		s.log.Error("update model", "err", err)
		httpError(w, http.StatusInternalServerError, "could not update model")
		return
	}
	s.modelChanged("updated", cfg.Name)
	writeJSON(w, http.StatusOK, cfg)
}

// deleteModel refuses while anything could still need the model: a role in
// the saved settings, an unfinished tree whose snapshot uses it, or queued jobs.
func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx := r.Context()
	if st, err := settings.Load(ctx, s.db, s.defaults); err == nil {
		var roles []string
		for role, m := range map[string]string{"planner": st.Settings.Planner.Model, "worker": st.Settings.Worker.Model, "reducer": st.Settings.Reducer.Model} {
			if m == name {
				roles = append(roles, role)
			}
		}
		if len(roles) > 0 {
			slices.Sort(roles)
			httpError(w, http.StatusConflict, fmt.Sprintf("%q is the %s model in Agents & directives; choose another model there first", name, strings.Join(roles, " and ")))
			return
		}
	}
	var active int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM agent_tree t JOIN agent_node r ON r.id = t.id
		WHERE r.status NOT IN ('done', 'failed', 'cancelled')
		AND $1 IN (t.config->'planner'->>'model', t.config->'worker'->>'model', t.config->'reducer'->>'model')`, name).Scan(&active); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pending, err := models.PendingJobs(ctx, s.db, agent.QueueName(name))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if active > 0 || pending > 0 {
		httpError(w, http.StatusConflict, fmt.Sprintf("%q is still in use by %d running tree(s) and %d queued job(s)", name, active, pending))
		return
	}
	switch err := s.registry.Delete(ctx, name); {
	case errors.Is(err, models.ErrReadOnly):
		httpError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, models.ErrNotFound):
		httpError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.modelChanged("removed", name)
	writeJSON(w, http.StatusOK, map[string]string{"removed": name})
}

// discover lists models on every host the pool uses, plus any ?url= hosts.
func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	var hosts []string
	add := func(raw string) error {
		base, err := discovery.BaseURL(raw)
		if err != nil {
			return err
		}
		if !seen[base] {
			seen[base] = true
			hosts = append(hosts, base)
		}
		return nil
	}
	for _, m := range s.registry.List() {
		for _, ep := range m.Endpoints {
			_ = add(ep)
		}
	}
	for _, raw := range r.URL.Query()["url"] {
		if err := add(raw); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	out := make([]discovery.Host, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() { out[i] = s.discovery.Discover(r.Context(), h) })
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

type loadRequest struct {
	URL           string `json:"url"`
	Model         string `json:"model"`
	InstanceID    string `json:"instance_id"`
	ContextLength int    `json:"context_length"`
}

func (s *Server) loadModel(w http.ResponseWriter, r *http.Request) {
	var req loadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.Model == "" {
		httpError(w, http.StatusBadRequest, "url and model are required")
		return
	}
	base, err := discovery.BaseURL(req.URL)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	s.log.Info("loading model", "host", base, "model", req.Model, "context_length", req.ContextLength)
	res, err := s.discovery.Load(ctx, base, req.Model, req.ContextLength)
	if err != nil {
		httpError(w, http.StatusBadGateway, "load failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) unloadModel(w http.ResponseWriter, r *http.Request) {
	var req loadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.InstanceID == "" {
		httpError(w, http.StatusBadRequest, "url and instance_id are required")
		return
	}
	base, err := discovery.BaseURL(req.URL)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("unloading model", "host", base, "instance", req.InstanceID)
	if err := s.discovery.Unload(r.Context(), base, req.InstanceID); err != nil {
		httpError(w, http.StatusBadGateway, "unload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"unloaded": req.InstanceID})
}

// streamTree replays the tree's events from the JetStream window and then
// follows live. Clients reconnect with Last-Event-ID to resume without gaps.
func (s *Server) streamTree(w http.ResponseWriter, r *http.Request) {
	id, ok := treeID(w, r)
	if !ok {
		return
	}
	root, err := store.GetNode(r.Context(), s.db, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "tree not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var after uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	msgs, err := s.bus.Subscribe(ctx, id, after)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "event stream unavailable")
		return
	}
	sse, ok := startSSE(w)
	if !ok {
		return
	}

	// A tree that finished before its events aged out of the replay window
	// would otherwise leave the client waiting forever.
	if root.Status.Terminal() && after == 0 {
		idle := time.NewTimer(2 * time.Second)
		defer idle.Stop()
		select {
		case m, ok := <-msgs:
			if ok {
				sse.event(m)
				if m.Event.Type.Final() {
					return
				}
			}
		case <-idle.C:
			sse.snapshot(root)
			return
		}
	}
	sse.follow(ctx, msgs, func(m events.Message) bool { return m.Event.Type.Final() })
}

// streamAll sends lifecycle events for every tree, for the activity view.
func (s *Server) streamAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	msgs, err := s.bus.SubscribeLifecycle(ctx)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "event stream unavailable")
		return
	}
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	sse.follow(ctx, msgs, func(events.Message) bool { return false })
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable ingress-nginx buffering
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, true
}

func (s *sseWriter) event(m events.Message) {
	data, _ := json.Marshal(m.Event)
	fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", m.Seq, m.Event.Type, data)
	s.f.Flush()
}

func (s *sseWriter) snapshot(root *store.Node) {
	typ := events.TreeDone
	text := root.Output
	switch root.Status {
	case store.StatusFailed:
		typ, text = events.TreeFailed, root.Error
	case store.StatusCancelled:
		typ = events.TreeCancelled
	}
	e := events.Event{Type: typ, TreeID: root.TreeID, NodeID: root.ID, Time: root.UpdatedAt}
	if text != nil {
		e.Text = *text
	}
	data, _ := json.Marshal(e)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, data)
	s.f.Flush()
}

// follow relays messages until stop returns true, the stream ends, or the
// client goes away, with keepalives so proxies don't drop idle streams.
func (s *sseWriter) follow(ctx context.Context, msgs <-chan events.Message, stop func(events.Message) bool) {
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return
			}
			s.event(m)
			if stop(m) {
				return
			}
		case <-heartbeat.C:
			fmt.Fprint(s.w, ": keepalive\n\n")
			s.f.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// workerRegistry keeps the latest heartbeat from each worker replica.
type workerRegistry struct {
	mu      sync.Mutex
	workers map[string]events.WorkerStatus
}

const workerStale = 6 * time.Second

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{workers: map[string]events.WorkerStatus{}}
}

func (r *workerRegistry) update(ws events.WorkerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[ws.Worker] = ws
}

func (r *workerRegistry) list() []events.WorkerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []events.WorkerStatus{}
	for id, ws := range r.workers {
		if time.Since(ws.Time) > workerStale {
			delete(r.workers, id)
			continue
		}
		out = append(out, ws)
	}
	slices.SortFunc(out, func(a, b events.WorkerStatus) int { return strings.Compare(a.Worker, b.Worker) })
	return out
}

func treeID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid tree id")
		return uuid.Nil, false
	}
	return id, true
}

func splitReason(req createRequest, n int) string {
	if len(req.Parts) > 0 {
		if n > len(req.Parts) {
			return fmt.Sprintf("%d given parts as %d agents (large parts split into sections), largest first", len(req.Parts), n)
		}
		return fmt.Sprintf("%d given parts, one agent each, largest first", n)
	}
	return fmt.Sprintf("document split in code into %d parts of about %d characters", n, len(req.Document)/n)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func treeContext(set *settings.Settings) string {
	if set == nil {
		return ""
	}
	return set.Context
}

// searchBudget reports this month's Exa spend against its free-tier cap.
func (s *Server) searchBudget(ctx context.Context) map[string]any {
	if s.SearchBudget == nil {
		return nil
	}
	spent, err := s.SearchBudget.Spent(ctx)
	if err != nil {
		return nil
	}
	return map[string]any{"provider": "exa", "spent_usd": spent, "limit_usd": s.SearchBudget.Limit(), "month": time.Now().UTC().Format("2006-01")}
}
