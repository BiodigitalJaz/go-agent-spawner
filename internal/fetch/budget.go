package fetch

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Budget caps a paid search provider's monthly spend so it never leaves
// its free tier. Spend is shared by every worker (Postgres).
type Budget interface {
	// Spent returns this month's recorded spend.
	Spent(ctx context.Context) (float64, error)
	// Record adds one query's cost.
	Record(ctx context.Context, cost float64) error
	Limit() float64
}

type PGBudget struct {
	DB       *pgxpool.Pool
	Provider string
	LimitUSD float64
	Now      func() time.Time
}

func (b *PGBudget) month() string {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	return now().UTC().Format("2006-01")
}

func (b *PGBudget) Limit() float64 { return b.LimitUSD }

func (b *PGBudget) Spent(ctx context.Context) (float64, error) {
	var spent float64
	err := b.DB.QueryRow(ctx, `SELECT coalesce((SELECT cost_usd::float8 FROM search_usage WHERE month = $1 AND provider = $2), 0)`, b.month(), b.Provider).Scan(&spent)
	return spent, err
}

func (b *PGBudget) Record(ctx context.Context, cost float64) error {
	_, err := b.DB.Exec(ctx, `INSERT INTO search_usage (month, provider, queries, cost_usd) VALUES ($1, $2, 1, $3)
		ON CONFLICT (month, provider) DO UPDATE SET queries = search_usage.queries + 1, cost_usd = search_usage.cost_usd + $3`,
		b.month(), b.Provider, cost)
	return err
}
