// Command spawner runs the agent pool. One image, several modes:
//
//	spawner migrate   apply River and agent_node migrations, then exit
//	spawner api       HTTP API + SSE gateway (River insert-only client)
//	spawner worker    River workers driving the GPU backends
//	spawner all       api and worker in one process, for local development
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/sync/errgroup"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/api"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/auth"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/config"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/mcpserver"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/models"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/ui"
)

// version is set at build time (-ldflags "-X main.version=…"); `go install`
// builds fall back to the module version.
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spawner migrate|api|worker|all|mcp|user")
		os.Exit(2)
	}
	if os.Args[1] == "user" {
		if err := runUser(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "spawner user:", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "mcp" {
		// stdio MCP server: a thin client of a running spawner API. Needs no
		// database or backend config, and must keep stdout for the protocol.
		if err := runMCP(); err != nil {
			fmt.Fprintln(os.Stderr, "spawner mcp:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1]); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(mode string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("mode", mode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if mode == "migrate" {
		return store.Migrate(ctx, db, log)
	}
	runAPI, runWorker := mode == "api" || mode == "all", mode == "worker" || mode == "all"
	if !runAPI && !runWorker {
		return fmt.Errorf("unknown mode %q", mode)
	}

	bus, err := events.Connect(ctx, log, events.Config{
		URL: cfg.NATSURL, Retention: cfg.EventRetention, Replicas: cfg.EventReplicas, FileStore: cfg.EventFileStore,
	})
	if err != nil {
		return err
	}
	defer bus.Close()

	// The model set is backends.json plus models added from the console; it
	// can change at runtime, so load it before building the pool.
	registry := models.NewRegistry(log, db, cfg.Backends.Models)
	if err := registry.Load(ctx); err != nil {
		return fmt.Errorf("load models: %w", err)
	}
	pool, err := backend.NewPool(log, models.Configs(registry.List()))
	if err != nil {
		return err
	}
	agentCfg := agent.Config{
		AcquireWait:   cfg.AcquireWait,
		Snooze:        cfg.Snooze,
		DeltaInterval: cfg.DeltaInterval,
		JobTimeout:    cfg.JobTimeout,
	}
	var backends []fetch.Backend
	if cfg.SearchURL != "" {
		backends = append(backends, fetch.NewSearcher(cfg.SearchURL, cfg.SearchCategories))
	}
	if cfg.ExaAPIKey != "" {
		exa := fetch.NewExa(log, cfg.ExaAPIKey)
		exa.Type = cfg.ExaType
		exa.Budget = &fetch.PGBudget{DB: db, Provider: "exa", LimitUSD: cfg.ExaMonthlyBudgetUSD}
		backends = append(backends, exa)
	}
	if len(backends) > 0 {
		agentCfg.Searcher = fetch.NewMulti(log, backends...)
		log.Info("web search enabled", "backends", agentCfg.Searcher.Names())
	}

	riverCfg := &river.Config{
		Logger:            log,
		FetchCooldown:     cfg.FetchCooldown,
		FetchPollInterval: cfg.FetchPollInterval,
		PollOnly:          cfg.RiverPollOnly,
		JobTimeout:        cfg.JobTimeout,
	}
	var (
		worker *agent.Worker
		queues = cfg.Queues
		// WORKER_QUEUES pins a worker to fixed models; otherwise it follows
		// the registry and serves every model, including ones added later.
		followAll = len(queues) == 0
	)
	if followAll {
		queues = pool.Models()
	}

	if runWorker {
		worker = agent.NewWorker(agentCfg, log, db, pool, bus)
		workers := river.NewWorkers()
		river.AddWorker(workers, worker)
		riverCfg.Workers = workers
		riverCfg.Queues = map[string]river.QueueConfig{}
		for _, m := range queues {
			riverCfg.Queues[agent.QueueName(m)] = river.QueueConfig{MaxWorkers: cfg.MaxWorkers}
		}
	}
	client, err := river.NewClient(riverpgxv5.New(db), riverCfg)
	if err != nil {
		return err
	}

	authn := auth.New(log, cfg.AuthTokens, cfg.AuthSessionTTL)
	if err := authn.WithUsers(ctx, auth.PGUsers{DB: db}); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	if authn.Enabled() {
		log.Info("authentication enabled", "tokens", len(cfg.AuthTokens))
	} else {
		log.Warn("authentication DISABLED: set AUTH_TOKENS or create an account (spawner user set <email>) to protect the API and console")
	}
	mux := http.NewServeMux()
	authn.Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /debug/backends", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pool.Status())
	})
	g, gctx := errgroup.WithContext(ctx)

	var follower *agent.QueueFollower
	if runWorker && followAll {
		follower = agent.NewQueueFollower(log, client, cfg.MaxWorkers, queues)
	}
	servedQueues := func() []string {
		if follower != nil {
			return follower.Models()
		}
		return queues
	}

	// Model changes: apply to the pool and, for workers that follow the
	// registry, add or remove River queues while running.
	registry.OnChange(func(cfgs []backend.ModelConfig) {
		if err := pool.SetModels(cfgs); err != nil {
			log.Error("apply model change", "err", err)
			return
		}
		if follower != nil {
			follower.Sync(gctx, pool.Models())
		}
	})
	if err := bus.WatchModelsChanged(gctx, registry.Trigger); err != nil {
		return err
	}
	g.Go(func() error { registry.Run(gctx, 15*time.Second); return nil })
	if runAPI {
		defaults := settings.Defaults(cfg.Backends.Planner, cfg.Backends.Default, cfg.MaxDepth, cfg.MaxChildren)
		apiSrv := api.New(log, db, client, bus, registry, defaults)
		apiSrv.SearchEnabled = cfg.SearchURL != "" || cfg.ExaAPIKey != ""
		if cfg.ExaAPIKey != "" {
			apiSrv.SearchBudget = &fetch.PGBudget{DB: db, Provider: "exa", LimitUSD: cfg.ExaMonthlyBudgetUSD}
		}
		if err := apiSrv.Start(gctx); err != nil {
			return err
		}
		apiSrv.Routes(mux)
		mux.Handle("GET /", ui.Handler())
		// MCP over Streamable HTTP. Stateless: the API runs several replicas
		// behind a Service with no session affinity. Tools call back into
		// this API on loopback with the caller's own bearer token.
		mcpSrv := mcpserver.New(mcpserver.NewClient(loopbackURL(cfg.HTTPAddr)), mcpserver.Options{
			ConsoleURL: os.Getenv("PUBLIC_URL"), Version: buildVersion(),
		})
		mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv },
			&mcp.StreamableHTTPOptions{Stateless: true, Logger: log})
		// One pattern per method: a bare "/mcp" would conflict with "GET /".
		for _, m := range []string{"GET", "POST", "DELETE"} {
			mux.Handle(m+" /mcp", mcpHandler)
		}
	}
	// SSE handlers only return when their request context ends, and Shutdown
	// waits for active connections, so request contexts derive from one that
	// is cancelled as soon as shutdown begins.
	baseCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           authn.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	g.Go(func() error {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	if runWorker {
		g.Go(func() error { pool.Discover(gctx, cfg.DiscoveryInterval); return nil })
		// Start on a context SIGTERM can't cancel: cancelling it is River's hard
		// stop. Shutdown below drains gracefully with Stop first.
		if err := client.Start(context.WithoutCancel(ctx)); err != nil {
			return err
		}
		log.Info("worker started", "queues", riverCfg.Queues)
		g.Go(func() error { heartbeat(gctx, log, bus, pool, servedQueues); return nil })
		if err := watchPeers(gctx, bus, pool); err != nil {
			return err
		}
	}

	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		cancelRequests()
		_ = srv.Shutdown(shutdownCtx)
		if !runWorker {
			return nil
		}
		// Soft stop lets in-flight generations finish; on timeout, hard stop
		// and requeue whatever is still running for another replica.
		if err := client.Stop(shutdownCtx); err != nil {
			worker.ShuttingDown()
			hardCtx, hardCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer hardCancel()
			return client.StopAndCancel(hardCtx)
		}
		return nil
	})
	return g.Wait()
}

const heartbeatEvery = 500 * time.Millisecond

// heartbeat publishes this worker's limiter state so the UI can show live GPU
// slot usage, and so peers can enforce models' shared server slots.
func heartbeat(ctx context.Context, log *slog.Logger, bus *events.Bus, pool *backend.Pool, queues func() []string) {
	id := workerID()
	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			raw, err := json.Marshal(pool.Status())
			if err != nil {
				continue
			}
			if err := bus.PublishWorkerStatus(events.WorkerStatus{Worker: id, Queues: queues(), Backends: raw}); err != nil {
				log.Debug("publish heartbeat", "err", err)
			}
		}
	}
}

func workerID() string {
	host, _ := os.Hostname()
	return strings.NewReplacer(".", "-", "*", "-", ">", "-").Replace(host)
}

// watchPeers feeds other workers' in-flight counts into the pool so a model's
// shared Slots cap holds across replicas. Heartbeats older than peerStale are
// ignored, so a crashed worker's slots free up within seconds.
func watchPeers(ctx context.Context, bus *events.Bus, pool *backend.Pool) error {
	const peerStale = 3 * time.Second
	self := workerID()
	var mu sync.Mutex
	latest := map[string]events.WorkerStatus{}
	recompute := func() {
		mu.Lock()
		defer mu.Unlock()
		peers := map[string]map[string]backend.PeerLoad{}
		for id, ws := range latest {
			if time.Since(ws.Time) > peerStale {
				delete(latest, id)
				continue
			}
			var backends map[string][]backend.EndpointStatus
			if json.Unmarshal(ws.Backends, &backends) != nil {
				continue
			}
			for model, eps := range backends {
				if peers[model] == nil {
					peers[model] = map[string]backend.PeerLoad{}
				}
				for _, ep := range eps {
					l := peers[model][ep.URL]
					l.Inflight += ep.Limiter.Inflight
					l.Tokens += ep.Tokens
					peers[model][ep.URL] = l
				}
			}
		}
		pool.SetPeerInflight(peers)
	}
	if err := bus.WatchWorkers(ctx, func(ws events.WorkerStatus) {
		if ws.Worker == self {
			return
		}
		mu.Lock()
		latest[ws.Worker] = ws
		mu.Unlock()
		recompute()
	}); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				recompute() // expire stale peers even when no heartbeats arrive
			}
		}
	}()
	return nil
}

// loopbackURL turns a listen address (":8080", "0.0.0.0:8080") into a URL
// this process can call itself on.
func loopbackURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// runMCP serves the MCP tools over stdio against a spawner API:
// SPAWNER_URL (default http://localhost:8080) and a token from SPAWNER_TOKEN,
// or the file SPAWNER_TOKEN_FILE, or ~/.config/agent-spawner/token.
func runMCP() error {
	base := os.Getenv("SPAWNER_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	token := strings.TrimSpace(os.Getenv("SPAWNER_TOKEN"))
	if token == "" {
		file := os.Getenv("SPAWNER_TOKEN_FILE")
		if file == "" {
			if home, err := os.UserHomeDir(); err == nil {
				file = filepath.Join(home, ".config", "agent-spawner", "token")
			}
		}
		if b, err := os.ReadFile(file); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	auth := ""
	if token != "" {
		auth = "Bearer " + token
	}
	console := os.Getenv("SPAWNER_CONSOLE_URL")
	if console == "" {
		console = base
	}
	srv := mcpserver.New(mcpserver.NewClient(base), mcpserver.Options{
		DefaultAuth: auth, ConsoleURL: console, LocalFiles: true, Version: buildVersion(),
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// runUser manages console accounts. The password is read from stdin so it
// never appears in process lists or shell history:
//
//	spawner user set <email>      create or reset (password on stdin; ends its sessions)
//	spawner user disable <email>  block sign-in and end its sessions
//	spawner user enable <email>
//	spawner user list
func runUser(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	users := auth.PGUsers{DB: db}
	usage := errors.New("usage: spawner user set|disable|enable <email> | spawner user list")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		list, err := users.List(ctx)
		if err != nil {
			return err
		}
		for _, u := range list {
			state := "active"
			if u.Disabled {
				state = "disabled"
			}
			fmt.Printf("%s\t%s\tcreated %s\tupdated %s\n", u.Email, state, u.CreatedAt.Format(time.RFC3339), u.UpdatedAt.Format(time.RFC3339))
		}
		return nil
	case "set", "disable", "enable":
		if len(args) != 2 || !strings.Contains(args[1], "@") {
			return usage
		}
	default:
		return usage
	}
	email := auth.NormalizeEmail(args[1])
	switch args[0] {
	case "disable", "enable":
		if err := users.SetDisabled(ctx, email, args[0] == "disable"); err != nil {
			return err
		}
		fmt.Printf("%s %sd\n", email, args[0])
		return nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("pipe the password on stdin")
	}
	hash, err := auth.HashPassword(strings.TrimRight(line, "\r\n"))
	if err != nil {
		return err
	}
	created, err := users.UpsertUser(ctx, email, hash)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created %s\n", email)
	} else {
		fmt.Printf("password reset for %s (its sessions were ended)\n", email)
	}
	return nil
}
