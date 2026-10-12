// Package models is the live set of routable models: the read-only ones from
// backends.json plus those added from the console (stored in agent_model).
// Every API and worker replica runs a Registry that reloads when notified
// over NATS, with a periodic poll as a fallback.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

const (
	SourceConfig = "config" // backends.json; read-only
	SourceUI     = "ui"     // agent_model; editable
)

type Model struct {
	backend.ModelConfig
	Source    string     `json:"source"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

var (
	ErrExists   = errors.New("models: a model with that name already exists")
	ErrNotFound = errors.New("models: no such console-managed model")
	ErrReadOnly = errors.New("models: defined in backends.json; edit the ConfigMap instead")
)

type Registry struct {
	log    *slog.Logger
	db     store.DBTX
	static []backend.ModelConfig

	mu        sync.RWMutex
	models    []Model
	fp        string
	listeners []func([]backend.ModelConfig)

	trigger chan struct{}
}

func NewRegistry(log *slog.Logger, db store.DBTX, static []backend.ModelConfig) *Registry {
	r := &Registry{log: log, db: db, static: static, trigger: make(chan struct{}, 1)}
	for _, c := range static {
		r.models = append(r.models, Model{ModelConfig: c, Source: SourceConfig})
	}
	return r
}

// OnChange registers fn to receive the full model set after every change.
// Register before calling Load so the first load is delivered too.
func (r *Registry) OnChange(fn func([]backend.ModelConfig)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, fn)
}

func (r *Registry) List() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.models)
}

func (r *Registry) Get(name string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

func (r *Registry) Has(name string) bool { _, ok := r.Get(name); return ok }

func (r *Registry) isStatic(name string) bool {
	return slices.ContainsFunc(r.static, func(c backend.ModelConfig) bool { return c.Name == name })
}

// Load reads console-managed models and notifies listeners if the set changed.
func (r *Registry) Load(ctx context.Context) error {
	rows, err := r.db.Query(ctx, `SELECT name, model, endpoints, initial_limit, max_limit, extra, context_length, slots, shared_context, updated_at
		FROM agent_model ORDER BY name`)
	if err != nil {
		return err
	}
	var dyn []Model
	for rows.Next() {
		var (
			m          Model
			eps, extra []byte
			updated    time.Time
		)
		if err := rows.Scan(&m.Name, &m.Model, &eps, &m.InitialLimit, &m.MaxLimit, &extra, &m.ContextLength, &m.Slots, &m.SharedContext, &updated); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(eps, &m.Endpoints); err != nil {
			rows.Close()
			return fmt.Errorf("model %s endpoints: %w", m.Name, err)
		}
		if len(extra) > 0 {
			if err := json.Unmarshal(extra, &m.Extra); err != nil {
				rows.Close()
				return fmt.Errorf("model %s extra: %w", m.Name, err)
			}
		}
		m.Source, m.UpdatedAt = SourceUI, &updated
		dyn = append(dyn, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	all := make([]Model, 0, len(r.static)+len(dyn))
	for _, c := range r.static {
		all = append(all, Model{ModelConfig: c, Source: SourceConfig})
	}
	for _, m := range dyn {
		if r.isStatic(m.Name) {
			r.log.Warn("console model shadowed by backends.json; ignoring", "model", m.Name)
			continue
		}
		if err := m.Validate(); err != nil {
			r.log.Warn("skipping invalid console model", "model", m.Name, "err", err)
			continue
		}
		all = append(all, m)
	}
	fp := fingerprint(all)

	r.mu.Lock()
	changed := fp != r.fp
	r.models, r.fp = all, fp
	listeners := slices.Clone(r.listeners)
	r.mu.Unlock()

	if changed {
		cfgs := Configs(all)
		for _, fn := range listeners {
			fn(cfgs)
		}
	}
	return nil
}

func fingerprint(ms []Model) string {
	b, _ := json.Marshal(Configs(ms))
	return string(b)
}

func Configs(ms []Model) []backend.ModelConfig {
	out := make([]backend.ModelConfig, len(ms))
	for i, m := range ms {
		out[i] = m.ModelConfig
	}
	return out
}

// Trigger asks Run to reload now (e.g. on a NATS change notification).
func (r *Registry) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run reloads on Trigger and every interval until ctx is done.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.trigger:
		}
		if err := r.Load(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("reload models", "err", err)
		}
	}
}

// Create stores a console-managed model.
func (r *Registry) Create(ctx context.Context, c backend.ModelConfig) error {
	if err := r.check(c); err != nil {
		return err
	}
	eps, extra := marshal(c)
	_, err := r.db.Exec(ctx, `INSERT INTO agent_model (name, model, endpoints, initial_limit, max_limit, extra, context_length, slots, shared_context)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, c.Name, c.Model, eps, c.InitialLimit, c.MaxLimit, extra, c.ContextLength, c.Slots, c.SharedContext)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return ErrExists
	}
	if err != nil {
		return err
	}
	return r.Load(ctx)
}

// Update replaces a console-managed model's definition. The name is the key.
func (r *Registry) Update(ctx context.Context, c backend.ModelConfig) error {
	if r.isStatic(c.Name) {
		return ErrReadOnly
	}
	if err := c.Validate(); err != nil {
		return err
	}
	eps, extra := marshal(c)
	tag, err := r.db.Exec(ctx, `UPDATE agent_model SET model = $2, endpoints = $3, initial_limit = $4,
		max_limit = $5, extra = $6, context_length = $7, slots = $8, shared_context = $9, updated_at = now() WHERE name = $1`,
		c.Name, c.Model, eps, c.InitialLimit, c.MaxLimit, extra, c.ContextLength, c.Slots, c.SharedContext)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return r.Load(ctx)
}

// Delete removes a console-managed model. Callers check it is unused first.
func (r *Registry) Delete(ctx context.Context, name string) error {
	if r.isStatic(name) {
		return ErrReadOnly
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM agent_model WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return r.Load(ctx)
}

func (r *Registry) check(c backend.ModelConfig) error {
	if r.isStatic(c.Name) {
		return ErrExists
	}
	return c.Validate()
}

func marshal(c backend.ModelConfig) (eps, extra []byte) {
	eps, _ = json.Marshal(c.Endpoints)
	if len(c.Extra) > 0 {
		extra, _ = json.Marshal(c.Extra)
	}
	return eps, extra
}

// PendingJobs counts unfinished jobs in a model's queue; a model with work
// still queued must not be removed or that work would never run.
func PendingJobs(ctx context.Context, db store.DBTX, queue string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE queue = $1 AND finalized_at IS NULL`, queue).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
