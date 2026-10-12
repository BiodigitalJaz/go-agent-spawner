package agent

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// QueueFollower keeps a worker's River queues in step with the model set, so
// a model added from the console starts being served without a restart.
type QueueFollower struct {
	log        *slog.Logger
	client     *river.Client[pgx.Tx]
	maxWorkers int

	mu    sync.Mutex
	names []string
}

func NewQueueFollower(log *slog.Logger, client *river.Client[pgx.Tx], maxWorkers int, initial []string) *QueueFollower {
	return &QueueFollower{log: log, client: client, maxWorkers: maxWorkers, names: slices.Clone(initial)}
}

// Models returns the models this worker currently has queues for.
func (f *QueueFollower) Models() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.names)
}

// Sync adds queues for new models and removes queues for models that are gone.
// Removing waits for that queue's running jobs; callers only remove models
// with no pending work (the API refuses otherwise).
func (f *QueueFollower) Sync(ctx context.Context, want []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []string
	for _, m := range f.names {
		if slices.Contains(want, m) {
			keep = append(keep, m)
			continue
		}
		if err := f.client.Queues().Remove(ctx, QueueName(m)); err != nil {
			f.log.Error("remove queue", "model", m, "err", err)
			keep = append(keep, m)
			continue
		}
		f.log.Info("queue removed", "model", m)
	}
	for _, m := range want {
		if slices.Contains(keep, m) {
			continue
		}
		if err := f.client.Queues().Add(QueueName(m), river.QueueConfig{MaxWorkers: f.maxWorkers}); err != nil {
			f.log.Error("add queue", "model", m, "err", err)
			continue
		}
		keep = append(keep, m)
		f.log.Info("queue added", "model", m)
	}
	f.names = keep
}
