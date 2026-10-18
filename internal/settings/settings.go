// Package settings holds the user-editable agent configuration: which model
// each role uses, its generation parameters, and its directive (system
// prompt). Settings live in Postgres so every API and worker replica sees the
// same values, and each tree stores a snapshot taken at submission.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

type Role struct {
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Directive   string   `json:"directive"`
}

type Settings struct {
	Planner     Role `json:"planner"`
	Worker      Role `json:"worker"`
	Reducer     Role `json:"reducer"`
	MaxDepth    int  `json:"max_depth"`
	MaxChildren int  `json:"max_children"`
	// Merge is how split results are combined: "llm" (default; the reducer
	// model writes the answer), "concat" (join outputs in order) or "list"
	// (union of lines without duplicates, for list-shaped answers). The code
	// merges need no model call, so they add no latency.
	Merge string `json:"merge,omitempty"`
	// AllowInternalFetch lets URL fetching reach private, loopback,
	// link-local and cluster addresses. Off by default; every attempt on
	// such an address is logged either way.
	AllowInternalFetch bool `json:"allow_internal_fetch,omitempty"`
	// CheckAnswer runs a rules check on a model-written final answer when
	// the tree has reference material: conflicting recommendations are
	// removed or fixed, and the changes are listed. On by default (no
	// omitempty, so a saved false is kept).
	CheckAnswer bool `json:"check_answer"`
	// Search runs a web search before planning (per run; set from the
	// request, never saved globally).
	Search bool `json:"search,omitempty"`

	// Context is a tree's reference material, sent to every agent. It is
	// stored with the tree (agent_tree.context), not in the settings JSON.
	Context string `json:"-"`
}

const (
	MergeLLM    = "llm"
	MergeConcat = "concat"
	MergeList   = "list"
)

// MergeMode returns the effective merge mode ("" means llm).
func (s Settings) MergeMode() string {
	if s.Merge == "" {
		return MergeLLM
	}
	return s.Merge
}

const (
	DefaultPlannerDirective = `You are the planner for a pool of AI agents that run in parallel.
Decide whether the task should be answered directly by one agent ("direct") or split into independent subtasks ("split").

Split only when:
- the subtasks are genuinely independent and can run at the same time, and
- the task is large enough that parallel work beats one focused answer.

Agents answer from their own knowledge plus any reference material provided; they cannot browse the web or open files. General questions need no reference material. Only when a task depends on specific content that was not provided (a particular web page, file, document or live data) should you answer "direct" so the missing content is reported instead of split into guesses.`

	DefaultWorkerDirective = `You are a worker agent. Complete the task thoroughly and accurately. Respond with the result only.

Answer from your own knowledge and any reference material provided; general questions need no reference material. You cannot browse the web or open files: if the task depends on specific content that was not included (a particular web page, file, document or live data), say in one line what is missing instead of guessing it. Never invent facts about a page, file or document you have not seen.`

	DefaultReducerDirective = `You synthesize results from parallel worker agents into one complete answer to the original task. Resolve overlaps, and note any gaps left by failed subtasks. Respond with the final answer only: write the answer itself, never a description of the workers, their agreement or this process.

If the results contradict each other on basic facts (for example, they describe different subjects), or report that required content was missing, do not combine them: say what is missing or contradictory, in a few lines, and stop. When reference material is provided, recommendations must not conflict with it.`
)

// Defaults builds the settings used until someone saves from the UI.
func Defaults(plannerModel, workerModel string, maxDepth, maxChildren int) Settings {
	return Settings{
		Planner:     Role{Model: plannerModel, Temperature: ptr(0.2), Directive: DefaultPlannerDirective},
		Worker:      Role{Model: workerModel, Temperature: ptr(0.7), Directive: DefaultWorkerDirective},
		Reducer:     Role{Model: workerModel, Temperature: ptr(0.3), Directive: DefaultReducerDirective},
		MaxDepth:    maxDepth,
		MaxChildren: maxChildren,
		CheckAnswer: true,
	}
}

func ptr[T any](v T) *T { return &v }

const maxDirectiveLen = 20_000

// Validate checks ranges and that every role's model is configured.
func (s Settings) Validate(knownModel func(string) bool) error {
	var errs []error
	for name, r := range map[string]Role{"planner": s.Planner, "worker": s.Worker, "reducer": s.Reducer} {
		if !knownModel(r.Model) {
			errs = append(errs, fmt.Errorf("%s: unknown model %q", name, r.Model))
		}
		if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
			errs = append(errs, fmt.Errorf("%s: temperature must be between 0 and 2", name))
		}
		if r.MaxTokens < 0 {
			errs = append(errs, fmt.Errorf("%s: max_tokens must be >= 0", name))
		}
		if strings.TrimSpace(r.Directive) == "" {
			errs = append(errs, fmt.Errorf("%s: directive is required", name))
		}
		if len(r.Directive) > maxDirectiveLen {
			errs = append(errs, fmt.Errorf("%s: directive longer than %d characters", name, maxDirectiveLen))
		}
	}
	if s.MaxDepth < 0 || s.MaxDepth > 4 {
		errs = append(errs, errors.New("max_depth must be between 0 and 4"))
	}
	if s.MaxChildren < 2 || s.MaxChildren > 16 {
		errs = append(errs, errors.New("max_children must be between 2 and 16"))
	}
	switch s.Merge {
	case "", MergeLLM, MergeConcat, MergeList:
	default:
		errs = append(errs, fmt.Errorf("merge must be %q, %q or %q", MergeLLM, MergeConcat, MergeList))
	}
	return errors.Join(errs...)
}

type Stored struct {
	Version   int       `json:"version"` // 0 means defaults, never saved
	Settings  Settings  `json:"settings"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

var ErrConflict = errors.New("settings: changed by someone else; reload and retry")

// Load returns the saved settings, or defaults at version 0.
func Load(ctx context.Context, db store.DBTX, defaults Settings) (*Stored, error) {
	var (
		st  Stored
		raw []byte
	)
	err := db.QueryRow(ctx, `SELECT version, config, updated_at FROM agent_settings WHERE id = 1`).Scan(&st.Version, &raw, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &Stored{Settings: defaults}, nil
	}
	if err != nil {
		return nil, err
	}
	// Decode over the defaults so fields added in later versions get values.
	st.Settings = defaults
	if err := json.Unmarshal(raw, &st.Settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	return &st, nil
}

// Save writes settings if the stored version still equals expectVersion.
func Save(ctx context.Context, db store.DBTX, s Settings, expectVersion int) (*Stored, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	st := Stored{Settings: s}
	if expectVersion == 0 {
		err = db.QueryRow(ctx, `INSERT INTO agent_settings (id, version, config) VALUES (1, 1, $1)
			ON CONFLICT (id) DO NOTHING RETURNING version, updated_at`, raw).Scan(&st.Version, &st.UpdatedAt)
	} else {
		err = db.QueryRow(ctx, `UPDATE agent_settings SET version = version + 1, config = $1, updated_at = now()
			WHERE id = 1 AND version = $2 RETURNING version, updated_at`, raw, expectVersion).Scan(&st.Version, &st.UpdatedAt)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SnapshotTree records the settings a tree runs with.
func SnapshotTree(ctx context.Context, db store.DBTX, treeID uuid.UUID, prompt string, s Settings) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO agent_tree (id, prompt, config, context) VALUES ($1, $2, $3, $4)`, treeID, prompt, raw, s.Context)
	return err
}

// ForTree loads a tree's snapshot.
func ForTree(ctx context.Context, db store.DBTX, treeID uuid.UUID) (*Settings, error) {
	var (
		raw       []byte
		reference string
	)
	err := db.QueryRow(ctx, `SELECT config, context FROM agent_tree WHERE id = $1`, treeID).Scan(&raw, &reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode tree settings: %w", err)
	}
	s.Context = reference
	return &s, nil
}
