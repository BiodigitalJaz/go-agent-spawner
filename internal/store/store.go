// Package store persists agent task trees in Postgres alongside River's tables,
// so node state changes can commit in the same transaction as job
// completion and child insertion.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusWaiting   Status = "waiting"  // split; waiting on children
	StatusReducing  Status = "reducing" // all children finished; reduce job queued
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

var ErrNotFound = errors.New("store: not found")

type Node struct {
	ID              uuid.UUID  `json:"id"`
	TreeID          uuid.UUID  `json:"tree_id"`
	ParentID        *uuid.UUID `json:"parent_id,omitempty"`
	Idx             int        `json:"idx"`
	Depth           int        `json:"depth"`
	Title           string     `json:"title"`
	Prompt          string     `json:"prompt"`
	Model           string     `json:"model"`
	Role            string     `json:"role"`
	Status          Status     `json:"status"`
	PendingChildren int        `json:"pending_children"`
	Output          *string    `json:"output,omitempty"`
	Error           *string    `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// Timeline, for drawing phases after live events have expired.
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	SplitAt         *time.Time      `json:"split_at,omitempty"`
	ReduceStartedAt *time.Time      `json:"reduce_started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	Stats           json.RawMessage `json:"stats,omitempty"`
}

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const nodeColumns = `id, tree_id, parent_id, idx, depth, title, prompt, model, role, status,
	pending_children, output, error, created_at, updated_at,
	started_at, split_at, reduce_started_at, finished_at, stats`

func scanNode(row pgx.Row) (*Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.TreeID, &n.ParentID, &n.Idx, &n.Depth, &n.Title, &n.Prompt, &n.Model, &n.Role,
		&n.Status, &n.PendingChildren, &n.Output, &n.Error, &n.CreatedAt, &n.UpdatedAt,
		&n.StartedAt, &n.SplitAt, &n.ReduceStartedAt, &n.FinishedAt, &n.Stats)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &n, err
}

func collectNodes(rows pgx.Rows, err error) ([]*Node, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func InsertNodes(ctx context.Context, db DBTX, nodes ...*Node) error {
	for _, n := range nodes {
		_, err := db.Exec(ctx, `
			INSERT INTO agent_node (id, tree_id, parent_id, idx, depth, title, prompt, model, role, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			n.ID, n.TreeID, n.ParentID, n.Idx, n.Depth, n.Title, n.Prompt, n.Model, n.Role, n.Status)
		if err != nil {
			return fmt.Errorf("insert node: %w", err)
		}
	}
	return nil
}

func GetNode(ctx context.Context, db DBTX, id uuid.UUID) (*Node, error) {
	return scanNode(db.QueryRow(ctx, `SELECT `+nodeColumns+` FROM agent_node WHERE id = $1`, id))
}

func Children(ctx context.Context, db DBTX, parentID uuid.UUID) ([]*Node, error) {
	return collectNodes(db.Query(ctx, `SELECT `+nodeColumns+` FROM agent_node WHERE parent_id = $1 ORDER BY idx`, parentID))
}

func TreeNodes(ctx context.Context, db DBTX, treeID uuid.UUID) ([]*Node, error) {
	return collectNodes(db.Query(ctx, `SELECT `+nodeColumns+` FROM agent_node WHERE tree_id = $1 ORDER BY depth, parent_id, idx`, treeID))
}

// MarkRunning moves a queued node to running. It is informational only, so it
// runs outside any transaction and ignores nodes in other states.
func MarkRunning(ctx context.Context, db DBTX, id uuid.UUID) error {
	_, err := db.Exec(ctx, `UPDATE agent_node SET status = 'running', started_at = coalesce(started_at, now()), updated_at = now()
		WHERE id = $1 AND status = 'queued'`, id)
	return err
}

// MarkReducing stamps when a split node's reduce step began. Informational.
func MarkReducing(ctx context.Context, db DBTX, id uuid.UUID) error {
	_, err := db.Exec(ctx, `UPDATE agent_node SET reduce_started_at = coalesce(reduce_started_at, now()), updated_at = now()
		WHERE id = $1 AND status = 'reducing'`, id)
	return err
}

// Split records children for a parent and puts the parent into waiting.
func Split(ctx context.Context, tx pgx.Tx, parentID uuid.UUID, children []*Node) error {
	if err := InsertNodes(ctx, tx, children...); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_node SET status = 'waiting', pending_children = $2, split_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('queued', 'running')`, parentID, len(children))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("split: parent %s not in a splittable state", parentID)
	}
	return nil
}

type FinishResult struct {
	// Changed is false when the node was already terminal (e.g. cancelled
	// while running); callers should not propagate in that case.
	Changed bool
	TreeID  uuid.UUID
	IsRoot  bool
	// ParentReady is set when this was the parent's last pending child. The
	// caller must enqueue the parent's reduce job in the same transaction.
	ParentReady *Node
}

// Finish marks a node terminal and decrements its parent's pending count. The
// parent row lock serializes concurrent siblings, so exactly one of them sees
// the count reach zero.
func Finish(ctx context.Context, tx pgx.Tx, id uuid.UUID, status Status, output, errMsg *string, stats any) (*FinishResult, error) {
	if !status.Terminal() {
		return nil, fmt.Errorf("finish: %q is not terminal", status)
	}
	res := &FinishResult{}
	var statsJSON []byte
	if stats != nil {
		var err error
		if statsJSON, err = json.Marshal(stats); err != nil {
			return nil, err
		}
	}
	var parentID *uuid.UUID
	err := tx.QueryRow(ctx, `
		UPDATE agent_node SET status = $2, output = $3, error = $4, stats = $5, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status NOT IN ('done', 'failed', 'cancelled')
		RETURNING tree_id, parent_id`, id, status, output, errMsg, statsJSON).Scan(&res.TreeID, &parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	res.Changed = true
	if parentID == nil {
		res.IsRoot = true
		return res, nil
	}

	parent, err := scanNode(tx.QueryRow(ctx, `
		UPDATE agent_node SET
			pending_children = pending_children - 1,
			status = CASE WHEN pending_children = 1 THEN 'reducing' ELSE status END,
			updated_at = now()
		WHERE id = $1 AND status = 'waiting'
		RETURNING `+nodeColumns, *parentID))
	if errors.Is(err, ErrNotFound) {
		return res, nil // parent cancelled or failed meanwhile
	}
	if err != nil {
		return nil, err
	}
	if parent.PendingChildren == 0 {
		res.ParentReady = parent
	}
	return res, nil
}

// CancelTree marks every unfinished node in a tree cancelled.
func CancelTree(ctx context.Context, db DBTX, treeID uuid.UUID) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE agent_node SET status = 'cancelled', finished_at = now(), updated_at = now()
		WHERE tree_id = $1 AND status NOT IN ('done', 'failed', 'cancelled')`, treeID)
	return tag.RowsAffected(), err
}

// TreeSummary is one row of the tree list, with node counts by status.
// ListPromptChars caps the prompt in tree listings. A prompt can carry a
// whole document; a list only needs a preview (full prompts made the
// listing megabytes and slowed the console). GET /v1/trees/{id} has it all.
const ListPromptChars = 400

type TreeSummary struct {
	ID          uuid.UUID        `json:"id"`
	Prompt      string           `json:"prompt"`       // first ListPromptChars characters
	PromptChars int              `json:"prompt_chars"` // full length
	Status      Status           `json:"status"`
	CreatedAt   time.Time        `json:"created_at"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
	Counts      map[Status]int64 `json:"counts"`
	Total       int64            `json:"total"`
}

func ListTrees(ctx context.Context, db DBTX, limit int) ([]TreeSummary, error) {
	rows, err := db.Query(ctx, `
		SELECT t.id, left(t.prompt, $2), length(t.prompt), t.created_at, r.status, r.finished_at,
			count(n.id),
			count(n.id) FILTER (WHERE n.status = 'queued'),
			count(n.id) FILTER (WHERE n.status = 'running'),
			count(n.id) FILTER (WHERE n.status = 'waiting'),
			count(n.id) FILTER (WHERE n.status = 'reducing'),
			count(n.id) FILTER (WHERE n.status = 'done'),
			count(n.id) FILTER (WHERE n.status = 'failed'),
			count(n.id) FILTER (WHERE n.status = 'cancelled')
		FROM agent_tree t
		JOIN agent_node r ON r.id = t.id
		JOIN agent_node n ON n.tree_id = t.id
		GROUP BY t.id, r.status, r.finished_at
		ORDER BY t.created_at DESC
		LIMIT $1`, limit, ListPromptChars)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeSummary
	for rows.Next() {
		var t TreeSummary
		var c [7]int64
		if err := rows.Scan(&t.ID, &t.Prompt, &t.PromptChars, &t.CreatedAt, &t.Status, &t.FinishedAt, &t.Total,
			&c[0], &c[1], &c[2], &c[3], &c[4], &c[5], &c[6]); err != nil {
			return nil, err
		}
		t.Counts = map[Status]int64{
			StatusQueued: c[0], StatusRunning: c[1], StatusWaiting: c[2], StatusReducing: c[3],
			StatusDone: c[4], StatusFailed: c[5], StatusCancelled: c[6],
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies River's migrations and then this package's, under an
// advisory lock so concurrently starting pods don't race.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: log})
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_spawner_migrate'))`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS agent_spawner_migration (
			version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		var current int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM agent_spawner_migration`).Scan(&current); err != nil {
			return err
		}
		files, err := fs.Glob(migrationFS, "migrations/*.sql")
		if err != nil {
			return err
		}
		slices.Sort(files)
		for _, f := range files {
			base := strings.TrimPrefix(f, "migrations/")
			version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("migration %s: bad version prefix", f)
			}
			if version <= current {
				continue
			}
			sql, err := migrationFS.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", base, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agent_spawner_migration (version) VALUES ($1)`, version); err != nil {
				return err
			}
			log.Info("applied migration", "file", base)
		}
		return nil
	})
}

// ErrTreeBusy means a tree still has running jobs, so it cannot be purged.
var ErrTreeBusy = errors.New("tree still has running jobs")

// PurgeTree deletes every record of a tree: its River jobs, its nodes (with
// their prompts and outputs) and the tree row (prompt, settings snapshot,
// reference material). Call it in a transaction, on a finished tree.
func PurgeTree(ctx context.Context, db DBTX, treeID uuid.UUID) (nodes, jobs int64, err error) {
	var running int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'tree_id' = $1 AND state = 'running'`, treeID.String()).Scan(&running); err != nil {
		return 0, 0, err
	}
	if running > 0 {
		return 0, 0, ErrTreeBusy
	}
	tag, err := db.Exec(ctx, `DELETE FROM river_job WHERE args->>'tree_id' = $1`, treeID.String())
	if err != nil {
		return 0, 0, err
	}
	jobs = tag.RowsAffected()
	if tag, err = db.Exec(ctx, `DELETE FROM agent_node WHERE tree_id = $1`, treeID); err != nil {
		return 0, 0, err
	}
	nodes = tag.RowsAffected()
	if _, err = db.Exec(ctx, `DELETE FROM agent_tree WHERE id = $1`, treeID); err != nil {
		return 0, 0, err
	}
	return nodes, jobs, nil
}

// SetTreeContext replaces a tree's reference material (used by the web
// search step, which adds results before planning).
func SetTreeContext(ctx context.Context, db DBTX, treeID uuid.UUID, reference string) error {
	_, err := db.Exec(ctx, `UPDATE agent_tree SET context = $2 WHERE id = $1`, treeID, reference)
	return err
}
