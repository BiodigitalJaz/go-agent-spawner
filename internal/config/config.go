// Package config loads settings from environment variables plus a JSON
// backends file (mounted from a ConfigMap in Kubernetes).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
)

type Backends struct {
	// Planner is the model that decides how to split work; Default is the
	// executor model used when a request doesn't name one.
	Planner string                `json:"planner"`
	Default string                `json:"default"`
	Models  []backend.ModelConfig `json:"models"`
}

type Config struct {
	DatabaseURL string
	NATSURL     string
	HTTPAddr    string
	LogLevel    string

	Backends Backends

	// Queues limits which models this worker serves; empty means all.
	Queues []string

	MaxDepth          int
	MaxChildren       int
	MaxWorkers        int
	AcquireWait       time.Duration
	Snooze            time.Duration
	DeltaInterval     time.Duration
	JobTimeout        time.Duration
	FetchCooldown     time.Duration
	FetchPollInterval time.Duration
	RiverPollOnly     bool
	DiscoveryInterval time.Duration
	ShutdownTimeout   time.Duration

	EventRetention time.Duration
	EventReplicas  int
	EventFileStore bool

	// AuthTokens are accepted bearer tokens (comma-separated AUTH_TOKENS,
	// from a Secret). Empty disables authentication.
	AuthTokens     []string
	AuthSessionTTL time.Duration
	// SearchURL is a SearXNG instance for runs with search on (JSON API);
	// SearchCategories overrides the categories queried.
	SearchURL        string
	SearchCategories string
	// ExaAPIKey enables Exa web search (official API, its own index).
	ExaAPIKey string
	ExaType   string
	// ExaMonthlyBudgetUSD caps Exa spend per calendar month, under the free
	// credit: search is never paid for.
	ExaMonthlyBudgetUSD float64
}

var modelName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,55}$`)

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		NATSURL:             env("NATS_URL", "nats://localhost:4222"),
		HTTPAddr:            env("HTTP_ADDR", ":8080"),
		LogLevel:            env("LOG_LEVEL", "info"),
		Queues:              list(os.Getenv("WORKER_QUEUES")),
		MaxDepth:            envInt("MAX_DEPTH", 2),
		MaxChildren:         envInt("MAX_CHILDREN", 6),
		MaxWorkers:          envInt("MAX_WORKERS_PER_QUEUE", 32),
		AcquireWait:         envDur("ACQUIRE_WAIT", time.Second),
		Snooze:              envDur("SNOOZE", 250*time.Millisecond),
		DeltaInterval:       envDur("DELTA_INTERVAL", 50*time.Millisecond),
		JobTimeout:          envDur("JOB_TIMEOUT", 10*time.Minute),
		FetchCooldown:       envDur("RIVER_FETCH_COOLDOWN", 20*time.Millisecond),
		FetchPollInterval:   envDur("RIVER_FETCH_POLL_INTERVAL", 250*time.Millisecond),
		RiverPollOnly:       envBool("RIVER_POLL_ONLY", false),
		DiscoveryInterval:   envDur("DISCOVERY_INTERVAL", 10*time.Second),
		ShutdownTimeout:     envDur("SHUTDOWN_TIMEOUT", 2*time.Minute),
		EventRetention:      envDur("EVENT_RETENTION", 15*time.Minute),
		EventReplicas:       envInt("EVENT_REPLICAS", 1),
		EventFileStore:      envBool("EVENT_FILE_STORE", false),
		AuthTokens:          list(os.Getenv("AUTH_TOKENS")),
		AuthSessionTTL:      envDur("AUTH_SESSION_TTL", 7*24*time.Hour),
		SearchURL:           env("SEARCH_URL", ""),
		SearchCategories:    env("SEARCH_CATEGORIES", ""),
		ExaAPIKey:           env("EXA_API_KEY", ""),
		ExaType:             env("EXA_SEARCH_TYPE", "instant"),
		ExaMonthlyBudgetUSD: envFloat("EXA_MONTHLY_BUDGET_USD", 9.5),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	path := env("BACKENDS_FILE", "backends.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read backends file: %w", err)
	}
	// Allow ${VAR} references so API keys can come from Secrets.
	if err := json.Unmarshal([]byte(os.ExpandEnv(string(raw))), &c.Backends); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var names []string
	for _, m := range c.Backends.Models {
		if !modelName.MatchString(m.Name) {
			return fmt.Errorf("model name %q must match %s", m.Name, modelName)
		}
		names = append(names, m.Name)
	}
	if !slices.Contains(names, c.Backends.Planner) {
		return fmt.Errorf("planner model %q is not defined", c.Backends.Planner)
	}
	if !slices.Contains(names, c.Backends.Default) {
		return fmt.Errorf("default model %q is not defined", c.Backends.Default)
	}
	for _, q := range c.Queues {
		if !slices.Contains(names, q) {
			return fmt.Errorf("WORKER_QUEUES references unknown model %q", q)
		}
	}
	if c.MaxDepth < 0 || c.MaxChildren < 2 {
		return fmt.Errorf("MAX_DEPTH must be >= 0 and MAX_CHILDREN >= 2")
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
