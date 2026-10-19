package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BenchStale is how long a running benchmark may go without a heartbeat
// before it counts as interrupted (its API pod went away).
const BenchStale = 3 * time.Minute

type Benchmark struct {
	ID         uuid.UUID       `json:"id"`
	Owner      string          `json:"-"`
	Status     string          `json:"status"`
	Config     json.RawMessage `json:"config"`
	Results    json.RawMessage `json:"results"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	Heartbeat  time.Time       `json:"heartbeat_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Active reports whether the benchmark is still being run by a live runner.
func (b *Benchmark) Active() bool {
	return (b.Status == "running" || b.Status == "cancelling") && time.Since(b.Heartbeat) < BenchStale
}

// normalize reports a running benchmark whose runner stopped as interrupted.
func (b *Benchmark) normalize() {
	if (b.Status == "running" || b.Status == "cancelling") && time.Since(b.Heartbeat) >= BenchStale {
		b.Status = "interrupted"
	}
}

const benchCols = `id, owner, status, config, results, error, created_at, heartbeat_at, finished_at`

func scanBench(row pgx.Row) (*Benchmark, error) {
	var b Benchmark
	if err := row.Scan(&b.ID, &b.Owner, &b.Status, &b.Config, &b.Results, &b.Error, &b.CreatedAt, &b.Heartbeat, &b.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.normalize()
	return &b, nil
}

func CreateBenchmark(ctx context.Context, db DBTX, owner string, config any) (*Benchmark, error) {
	cfg, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return scanBench(db.QueryRow(ctx, `INSERT INTO benchmark (id, owner, status, config) VALUES ($1, $2, 'running', $3) RETURNING `+benchCols,
		uuid.New(), owner, cfg))
}

// ActiveBenchmark returns a benchmark still being run, if any (one runs at a
// time: they swap models on shared GPUs).
func ActiveBenchmark(ctx context.Context, db DBTX) (*Benchmark, error) {
	b, err := scanBench(db.QueryRow(ctx, `SELECT `+benchCols+` FROM benchmark WHERE status IN ('running', 'cancelling') AND heartbeat_at > now() - $1::interval ORDER BY created_at DESC LIMIT 1`,
		BenchStale.String()))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return b, err
}

func GetBenchmark(ctx context.Context, db DBTX, id uuid.UUID, owner string) (*Benchmark, error) {
	return scanBench(db.QueryRow(ctx, `SELECT `+benchCols+` FROM benchmark WHERE id = $1 AND owner = $2`, id, owner))
}

func ListBenchmarks(ctx context.Context, db DBTX, owner string, limit int) ([]Benchmark, error) {
	rows, err := db.Query(ctx, `SELECT `+benchCols+` FROM benchmark WHERE owner = $1 ORDER BY created_at DESC LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Benchmark{}
	for rows.Next() {
		b, err := scanBench(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// SaveBenchmarkProgress stores the results so far and refreshes the
// heartbeat; it returns the current status, so the runner sees a
// cancellation requested through another API pod.
func SaveBenchmarkProgress(ctx context.Context, db DBTX, id uuid.UUID, results any) (string, error) {
	res, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	var status string
	err = db.QueryRow(ctx, `UPDATE benchmark SET results = $2, heartbeat_at = now() WHERE id = $1 RETURNING status`, id, res).Scan(&status)
	return status, err
}

func FinishBenchmark(ctx context.Context, db DBTX, id uuid.UUID, status string, results any, errText string) error {
	res, err := json.Marshal(results)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE benchmark SET status = $2, results = $3, error = $4, finished_at = now(), heartbeat_at = now() WHERE id = $1`, id, status, res, errText)
	return err
}

// RequestBenchmarkCancel marks a running benchmark for cancellation; the
// runner stops at its next check.
func RequestBenchmarkCancel(ctx context.Context, db DBTX, id uuid.UUID, owner string) error {
	tag, err := db.Exec(ctx, `UPDATE benchmark SET status = 'cancelling' WHERE id = $1 AND owner = $2 AND status = 'running'`, id, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func DeleteBenchmark(ctx context.Context, db DBTX, id uuid.UUID, owner string) error {
	tag, err := db.Exec(ctx, `DELETE FROM benchmark WHERE id = $1 AND owner = $2`, id, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
