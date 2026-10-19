package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"golang.org/x/sync/errgroup"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/backend"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/fetch"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

var errCancelled = errors.New("agent: tree cancelled")

// Events is the subset of the event bus the worker needs.
type Events interface {
	Publish(ctx context.Context, e events.Event) error
	Deltas(typ events.Type, treeID, nodeID uuid.UUID, interval time.Duration) *events.DeltaWriter
}

// Config holds operational knobs. What the agents do (models, directives,
// split limits) comes from each tree's settings snapshot instead.
type Config struct {
	// AcquireWait is how long a job waits for a free GPU slot before
	// snoozing back to the queue, where another replica may have capacity.
	AcquireWait   time.Duration
	Snooze        time.Duration
	DeltaInterval time.Duration
	JobTimeout    time.Duration
	// Searcher runs web searches for runs with search on (nil: not configured).
	Searcher *fetch.Multi
}

type Worker struct {
	river.WorkerDefaults[TaskArgs]

	cfg  Config
	log  *slog.Logger
	db   *pgxpool.Pool
	pool *backend.Pool
	bus  Events

	shuttingDown atomic.Bool
}

func NewWorker(cfg Config, log *slog.Logger, db *pgxpool.Pool, pool *backend.Pool, bus Events) *Worker {
	return &Worker{cfg: cfg, log: log, db: db, pool: pool, bus: bus}
}

// ShuttingDown makes jobs interrupted by a hard stop return to the queue
// immediately instead of burning a retry attempt.
func (w *Worker) ShuttingDown() { w.shuttingDown.Store(true) }

func (w *Worker) Timeout(*river.Job[TaskArgs]) time.Duration { return w.cfg.JobTimeout }

// task is the per-job state threaded through the role handlers.
type task struct {
	job   *river.Job[TaskArgs]
	node  *store.Node
	set   *settings.Settings
	merge struct{ passes, batches, truncated int }
	// quiet keeps a generation's text off the live stream: the source
	// check's rewrite replaces the answer once, when it is done, instead of
	// streaming a second copy for the reader to read again.
	quiet bool
}

func (t *task) event(typ events.Type, role Role) events.Event {
	return events.Event{Type: typ, TreeID: t.node.TreeID, NodeID: t.node.ID, ParentID: t.node.ParentID, Role: string(role), Title: t.node.Title}
}

func (w *Worker) Work(ctx context.Context, job *river.Job[TaskArgs]) error {
	a := job.Args
	node, err := store.GetNode(ctx, w.db, a.NodeID)
	if errors.Is(err, store.ErrNotFound) {
		return river.JobCancel(err)
	}
	if err != nil {
		return err
	}
	if node.Status == store.StatusCancelled {
		return river.JobCancel(errCancelled)
	}
	if node.Status.Terminal() {
		return nil // already finished by an earlier attempt
	}
	set, err := settings.ForTree(ctx, w.db, a.TreeID)
	if errors.Is(err, store.ErrNotFound) {
		return river.JobCancel(fmt.Errorf("tree %s has no settings snapshot", a.TreeID))
	}
	if err != nil {
		return err
	}
	t := &task{job: job, node: node, set: set}

	// Code merges need no model, so they don't wait for a GPU slot.
	if a.Role == RoleReduce && set.MergeMode() != settings.MergeLLM {
		if err := store.MarkReducing(ctx, w.db, node.ID); err != nil {
			w.log.Warn("mark node phase", "node", node.ID, "err", err)
		}
		return w.outcome(ctx, t, w.reduceInCode(ctx, t))
	}

	prompt, output, err := w.estimate(ctx, t)
	if err != nil {
		return err
	}
	lease, err := w.pool.AcquireRequest(ctx, a.Model, w.cfg.AcquireWait, prompt, output)
	switch {
	case errors.Is(err, backend.ErrSaturated):
		return river.JobSnooze(jitter(w.cfg.Snooze))
	case errors.Is(err, backend.ErrUnknownModel):
		return w.fail(ctx, t, err, false)
	case err != nil:
		return w.interrupted(err)
	}

	if a.Role == RoleReduce {
		err = store.MarkReducing(ctx, w.db, node.ID)
	} else {
		err = store.MarkRunning(ctx, w.db, node.ID)
	}
	if err != nil {
		w.log.Warn("mark node phase", "node", node.ID, "err", err)
	}

	// A root with search on searches first (planner job, or a depth-0
	// worker job); the search uses this lease for its query call, then
	// re-admits the job with the larger reference.
	if node.ParentID == nil && set.Search && (a.Role == RolePlan || a.Role == RoleExec) {
		if lease, err = w.search(ctx, t, lease); err != nil {
			return w.outcome(ctx, t, err)
		}
	}
	switch a.Role {
	case RolePlan:
		if node.Depth >= set.MaxDepth {
			lease.Done(llm.Stats{}, nil)
			err = w.direct(ctx, t)
		} else {
			err = w.plan(ctx, t, lease)
		}
	case RoleExec:
		err = w.exec(ctx, t, lease)
	case RoleReduce:
		err = w.reduce(ctx, t, lease)
	default:
		lease.Done(llm.Stats{}, nil)
		return river.JobCancel(fmt.Errorf("unknown role %q", a.Role))
	}
	return w.outcome(ctx, t, err)
}

// outcome turns a role handler's error into the job result: retry, snooze,
// cancel, or (on the final attempt) mark the node failed so its parent can
// still reduce.
func (w *Worker) outcome(ctx context.Context, t *task, err error) error {
	job, node, a := t.job, t.node, t.job.Args
	if err == nil {
		return nil
	}
	// Shutdown or remote cancellation: hand back or let River cancel. A job
	// timeout (DeadlineExceeded) is an ordinary failure and must reach the
	// final-attempt check, or the node would never finish and its parent
	// would never reduce.
	if ctx.Err() != nil && (w.shuttingDown.Load() || errors.Is(ctx.Err(), context.Canceled)) {
		return w.interrupted(err)
	}
	var snooze *river.JobSnoozeError
	var cancel *river.JobCancelError
	if errors.As(err, &snooze) || errors.As(err, &cancel) {
		return err
	}
	// A shared context that was momentarily full is not this task's fault:
	// wait for other requests to finish instead of burning an attempt.
	if llm.ContextPressure(err) && snoozes(job.Metadata) < maxPressureSnoozes {
		w.log.Info("context full, retrying shortly", "node", node.ID, "role", a.Role)
		return river.JobSnooze(jitter(2 * time.Second))
	}
	final := !llm.Retryable(err) || job.Attempt >= job.MaxAttempts
	if !final {
		w.log.Warn("task attempt failed", "node", node.ID, "role", a.Role, "attempt", job.Attempt, "err", err)
		return err
	}
	return w.fail(ctx, t, err, llm.Retryable(err))
}

const maxPressureSnoozes = 30

func snoozes(metadata []byte) int {
	var m struct {
		Snoozes int `json:"snoozes"`
	}
	_ = json.Unmarshal(metadata, &m)
	return m.Snoozes
}

// estimate sizes this job's model request (prompt plus reserved output) for
// shared-context admission. A merge estimate is capped at what the merge
// guard will actually send.
func (w *Worker) estimate(ctx context.Context, t *task) (prompt, output int, err error) {
	set, node := t.set, t.node
	switch t.job.Args.Role {
	case RolePlan:
		prompt, output = estimateParts(planMessages(set, node, node.Depth+1 < set.MaxDepth), max(set.Planner.MaxTokens, 1024))
		return prompt, output, nil
	case RoleReduce:
		children, err := store.Children(ctx, w.db, node.ID)
		if err != nil {
			return 0, 0, err
		}
		prompt, output = estimateParts(reduceMessages(set, node, childItems(children)), set.Reducer.MaxTokens)
		if ctxLen := w.pool.ContextLength(t.job.Args.Model); ctxLen > 0 {
			prompt = max(min(prompt, ctxLen*9/10-output), 0)
		}
		return prompt, output, nil
	default:
		prompt, output = estimateParts(execMessages(set, node), set.Worker.MaxTokens)
		return prompt, partOutput(set, node, output), nil
	}
}

func (w *Worker) interrupted(err error) error {
	if w.shuttingDown.Load() {
		return river.JobSnooze(0)
	}
	return err
}

func request(lease *backend.Lease, role settings.Role, msgs []llm.Message) llm.Request {
	return llm.Request{
		Model:       lease.Model,
		Messages:    msgs,
		Temperature: role.Temperature,
		MaxTokens:   role.MaxTokens,
		Extra:       lease.Extra,
	}
}

func (w *Worker) plan(ctx context.Context, t *task, lease *backend.Lease) error {
	w.publish(ctx, t.event(events.NodeStarted, RolePlan))

	set, node := t.set, t.node
	canNest := node.Depth+1 < set.MaxDepth
	req := request(lease, set.Planner, planMessages(set, node, canNest))
	req.JSONSchema = planSchema(set.MaxChildren)

	var p plan
	thinking := w.bus.Deltas(events.Thinking, node.TreeID, node.ID, w.cfg.DeltaInterval)
	stats, err := lease.Endpoint.Client.JSON(ctx, req, &p, thinking.Write)
	thinking.Close()
	lease.Done(stats, err)
	if err != nil {
		return err
	}

	if p.Strategy != "split" || len(p.Subtasks) < 2 {
		return w.direct(ctx, t)
	}
	if len(p.Subtasks) > set.MaxChildren {
		p.Subtasks = p.Subtasks[:set.MaxChildren]
	}

	children := make([]*store.Node, len(p.Subtasks))
	params := make([]river.InsertManyParams, len(p.Subtasks))
	evChildren := make([]events.Child, len(p.Subtasks))
	for i, st := range p.Subtasks {
		args := TaskArgs{TreeID: node.TreeID, NodeID: uuid.New(), Role: RoleExec, Model: set.Worker.Model, Priority: t.job.Args.Priority}
		if st.Complex && canNest {
			args.Role, args.Model = RolePlan, set.Planner.Model
		}
		children[i] = &store.Node{
			ID: args.NodeID, TreeID: node.TreeID, ParentID: &node.ID, Idx: i, Depth: node.Depth + 1,
			Title: st.Title, Prompt: st.Prompt, Model: set.Worker.Model, Role: string(args.Role), Status: store.StatusQueued,
		}
		params[i] = river.InsertManyParams{Args: args}
		evChildren[i] = events.Child{ID: args.NodeID, Title: st.Title, Role: string(args.Role), Prompt: st.Prompt}
	}

	err = w.commit(ctx, t.job, func(tx pgx.Tx, client *river.Client[pgx.Tx]) error {
		if err := store.Split(ctx, tx, node.ID, children); err != nil {
			return err
		}
		_, err := client.InsertManyTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return err
	}
	ev := t.event(events.NodeSplit, RolePlan)
	ev.Text, ev.Children = p.Reason, evChildren
	w.publish(ctx, ev)
	return nil
}

// direct answers a node without splitting. When the planner and worker are
// the same model, it runs inline to skip a queue hop; otherwise it hands off
// to the worker model's queue.
func (w *Worker) direct(ctx context.Context, t *task) error {
	model := t.set.Worker.Model
	if t.job.Args.Model == model {
		prompt, output := estimateParts(execMessages(t.set, t.node), t.set.Worker.MaxTokens)
		lease, err := w.pool.AcquireRequest(ctx, model, w.cfg.AcquireWait, prompt, output)
		if err == nil {
			return w.exec(ctx, t, lease)
		}
		if !errors.Is(err, backend.ErrSaturated) {
			return err
		}
	}
	return w.commit(ctx, t.job, func(tx pgx.Tx, client *river.Client[pgx.Tx]) error {
		_, err := client.InsertTx(ctx, tx, TaskArgs{
			TreeID: t.node.TreeID, NodeID: t.node.ID, Role: RoleExec, Model: model, Priority: t.job.Args.Priority,
		}, nil)
		return err
	})
}

func (w *Worker) exec(ctx context.Context, t *task, lease *backend.Lease) error {
	w.publish(ctx, t.event(events.NodeStarted, RoleExec))
	msgs := execMessages(t.set, t.node)
	out, stats, lease, err := w.generateRetrying(ctx, t, RoleExec, t.job.Args.Model, t.set.Worker, msgs, lease)
	if err != nil {
		return err
	}
	return w.complete(ctx, t, RoleExec, out, lease, stats)
}

// checkAnswer runs the rules check on a root's final answer with the
// reducer model. The repeated node.started makes viewers replace the shown
// answer with the checked one as it streams.
func (w *Worker) checkAnswer(ctx context.Context, t *task, role Role, answer string) (string, error) {
	return w.review(ctx, t, role, answer, checkMessages(t.set, t.node, answer), "Rules check: reviewing the answer against the reference material.\n", false)
}

// review rewrites a root's final answer with the reducer model (the rules
// check and the source check).
// A quiet review is not streamed (see task.quiet).
func (w *Worker) review(ctx context.Context, t *task, role Role, answer string, msgs []llm.Message, status string, quiet bool) (string, error) {
	model := t.set.Reducer.Model
	r := t.set.Reducer
	if r.MaxTokens > 0 { // the checked answer is about as long as the answer
		r.MaxTokens = max(r.MaxTokens, estimateTokens(answer)+800)
	}
	prompt, output := estimateParts(msgs, r.MaxTokens)
	output = max(output, estimateTokens(answer)+800)
	lease, err := w.acquireRequest(ctx, model, prompt, output)
	if err != nil {
		return "", err
	}
	if !quiet {
		w.publish(ctx, t.event(events.NodeStarted, role)) // viewers replace the shown answer as the check streams
	}
	note := w.bus.Deltas(events.Thinking, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	note.Write(status)
	note.Close()
	t.quiet = quiet
	out, _, _, err := w.generateRetrying(ctx, t, role, model, r, msgs, lease)
	t.quiet = false
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", errors.New("the check returned nothing")
	}
	return out, nil
}

// maxPressureRetries bounds in-job retries after a shared-context overflow.
// They run inside the job, so they never use up an attempt or count against
// River's snooze total (which queue waits also raise).
const maxPressureRetries = 8

// generateRetrying runs generate and, when the shared context overflowed,
// waits briefly and re-admits the request: the overflow raised the model's
// token ratio, so the retry reserves more and waits for real room.
func (w *Worker) generateRetrying(ctx context.Context, t *task, role Role, model string, r settings.Role, msgs []llm.Message, lease *backend.Lease) (string, llm.Stats, *backend.Lease, error) {
	for retry := 0; ; retry++ {
		if retry > 0 && !t.quiet {
			w.publish(ctx, t.event(events.NodeStarted, role)) // a repeated start tells viewers to clear the partial text
		}
		out, stats, err := w.generate(ctx, t, lease, request(lease, r, msgs))
		if err == nil || !llm.ContextPressure(err) || retry >= maxPressureRetries {
			return out, stats, lease, err
		}
		w.log.Info("context full, retrying", "node", t.node.ID, "role", role, "retry", retry+1, "token_ratio", w.pool.TokenRatio(model))
		select {
		case <-time.After(jitter(time.Duration(retry+1) * 2 * time.Second)):
		case <-ctx.Done():
			return "", llm.Stats{}, lease, ctx.Err()
		}
		prompt, output := estimateParts(msgs, r.MaxTokens)
		if role == RoleExec {
			output = partOutput(t.set, t.node, output)
		}
		if lease, err = w.acquireRequest(ctx, model, prompt, output); err != nil {
			return "", llm.Stats{}, lease, err
		}
	}
}

// reduce merges a node's children. If their results would not fit the
// reducer model's context, they are first consolidated in batches that run in
// parallel (repeating if needed), so nothing is silently cut off by the server.
func (w *Worker) reduce(ctx context.Context, t *task, lease *backend.Lease) error {
	children, err := store.Children(ctx, w.db, t.node.ID)
	if err != nil {
		lease.Done(llm.Stats{}, nil)
		return err
	}
	w.publish(ctx, t.event(events.NodeStarted, RoleReduce))

	model := t.job.Args.Model
	items := childItems(children)
	budget := mergeBudget(w.pool.ContextLength(model), t.set.Reducer, t.set.Reducer.Directive+t.node.Prompt+t.set.Context)
	if totalTokens(items) > budget {
		lease.Done(llm.Stats{}, nil) // batches take their own slots
		if items, err = w.consolidate(ctx, t, model, items, budget); err != nil {
			return err
		}
		prompt, output := estimateParts(reduceMessages(t.set, t.node, items), t.set.Reducer.MaxTokens)
		if lease, err = w.acquireRequest(ctx, model, prompt, output); err != nil {
			return err
		}
	}
	out, stats, lease, err := w.generateRetrying(ctx, t, RoleReduce, model, t.set.Reducer, reduceMessages(t.set, t.node, items), lease)
	if err != nil {
		return err
	}
	return w.complete(ctx, t, RoleReduce, out, lease, stats)
}

// consolidate condenses items in parallel batches until they fit the budget.
func (w *Worker) consolidate(ctx context.Context, t *task, model string, items []mergeItem, budget int) ([]mergeItem, error) {
	note := w.bus.Deltas(events.Thinking, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	defer note.Close()
	for pass := 1; totalTokens(items) > budget; pass++ {
		if pass > maxMergePass {
			return nil, fmt.Errorf("merge: results still exceed the %d-token budget after %d passes", budget, pass-1)
		}
		for i := range items {
			var cut bool
			if items[i], cut = fitItem(items[i], budget); cut {
				t.merge.truncated++
				w.log.Warn("merge input truncated to fit context", "node", t.node.ID, "item", items[i].Title)
			}
		}
		batches := planBatches(items, budget)
		if len(batches) == len(items) && len(items) > 1 {
			// No two results fit together: give each an equal share instead,
			// with visible truncation markers.
			share := budget / len(items)
			for i := range items {
				var cut bool
				if items[i], cut = fitItem(items[i], share); cut {
					t.merge.truncated++
				}
			}
			continue
		}
		if len(batches) == 1 {
			break // fits after truncation
		}
		t.merge.passes, t.merge.batches = pass, t.merge.batches+len(batches)
		note.Write(fmt.Sprintf("Results exceed the merge context (≈%d tokens > %d). Pass %d: consolidating %d results in %d parallel batches.\n",
			totalTokens(items), budget, pass, len(items), len(batches)))

		next := make([]mergeItem, len(batches))
		g, gctx := errgroup.WithContext(ctx)
		for i, batch := range batches {
			g.Go(func() error {
				if len(batch) == 1 {
					next[i] = batch[0]
					return nil
				}
				prompt, output := estimateParts(consolidateMessages(t.node, batch), t.set.Reducer.MaxTokens)
				lease, err := w.acquireRequest(gctx, model, prompt, output)
				if err != nil {
					return err
				}
				out, stats, err := lease.Endpoint.Client.Stream(gctx, request(lease, t.set.Reducer, consolidateMessages(t.node, batch)), nil)
				lease.Done(stats, err)
				if err != nil {
					return err
				}
				tokens := stats.CompletionTokens
				if tokens == 0 {
					tokens = estimateTokens(out)
				}
				next[i] = mergeItem{Title: fmt.Sprintf("Consolidated: %s … %s", batch[0].Title, batch[len(batch)-1].Title), Text: out, Tokens: tokens}
				note.Write(fmt.Sprintf("Batch %d/%d consolidated (%d results → %d tokens).\n", i+1, len(batches), len(batch), tokens))
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		items = next
	}
	return items, nil
}

// acquire waits for a slot (and context tokens) for as long as the job
// allows; merges run inside one job, so snoozing back to the queue is not an
// option here.
func (w *Worker) acquireRequest(ctx context.Context, model string, prompt, output int) (*backend.Lease, error) {
	for {
		lease, err := w.pool.AcquireRequest(ctx, model, 30*time.Second, prompt, output)
		if !errors.Is(err, backend.ErrSaturated) {
			return lease, err
		}
	}
}

// reduceInCode merges children without a model call (merge "concat"/"list").
func (w *Worker) reduceInCode(ctx context.Context, t *task) error {
	children, err := store.Children(ctx, w.db, t.node.ID)
	if err != nil {
		return err
	}
	w.publish(ctx, t.event(events.NodeStarted, RoleReduce))
	start := time.Now()
	out, failed := codeMerge(t.set.MergeMode(), children)
	if failed > 0 {
		w.log.Warn("code merge with failed parts", "node", t.node.ID, "failed", failed)
	}
	content := w.bus.Deltas(events.Delta, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	content.Write(out)
	content.Close()
	return w.complete(ctx, t, RoleReduce, out, nil, llm.Stats{Duration: time.Since(start)})
}

// generate streams a completion, forwarding deltas to the bus, and releases
// the lease as soon as generation ends so the slot frees before any DB work.
func (w *Worker) generate(ctx context.Context, t *task, lease *backend.Lease, req llm.Request) (string, llm.Stats, error) {
	content := w.bus.Deltas(events.Delta, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	thinking := w.bus.Deltas(events.Thinking, t.node.TreeID, t.node.ID, w.cfg.DeltaInterval)
	out, stats, err := lease.Endpoint.Client.Stream(ctx, req, func(kind llm.DeltaKind, text string) {
		switch {
		case t.quiet:
		case kind == llm.Reasoning:
			thinking.Write(text)
		default:
			content.Write(text)
		}
	})
	thinking.Close()
	content.Close()
	lease.Done(stats, err)
	return out, stats, err
}

// complete marks the node done, queues the parent's reduce if this was the
// last child, and completes the job, all in one transaction.
func (w *Worker) complete(ctx context.Context, t *task, role Role, out string, lease *backend.Lease, stats llm.Stats) error {
	ev := &events.Stats{
		TTFTMillis:      stats.TTFT.Milliseconds(),
		DurationMillis:  stats.Duration.Milliseconds(),
		Tokens:          stats.CompletionTokens,
		PromptTokens:    stats.PromptTokens,
		TokensPerSecond: stats.TokensPerSecond(),
		MergePasses:     t.merge.passes,
		MergeBatches:    t.merge.batches,
		Truncated:       t.merge.truncated,
	}
	if lease != nil {
		ev.Endpoint, ev.Model = lease.Endpoint.URL, lease.Model
	} else {
		ev.Model = "merge:" + t.set.MergeMode() // merged in code, no model call
	}
	// A model-written final answer is checked against the reference (code
	// merges only combine per-item outputs, so they are left as they are).
	if t.node.ParentID == nil && lease != nil && t.set.CheckAnswer && strings.TrimSpace(t.set.Context) != "" && strings.TrimSpace(out) != "" {
		checked, err := w.checkAnswer(ctx, t, role, out)
		switch {
		case err == nil:
			out, ev.Checked = checked, true
		case ctx.Err() != nil:
			return err
		default:
			w.log.Warn("rules check failed; keeping the unchecked answer", "node", t.node.ID, "err", err)
			ev.CheckError = err.Error()
			out += "\n\n---\n_The rules check against the reference could not run (" + err.Error() + "), so this answer is unchecked._"
		}
	} else if t.node.ParentID == nil && lease != nil && t.set.SourceCheck && strings.Contains(t.set.Context, searchMarker) && strings.TrimSpace(out) != "" {
		checked, err := w.review(ctx, t, role, out, sourceCheckMessages(t.set, t.node, out), "Checking the answer against its sources…\n", true)
		switch {
		case err == nil:
			out, ev.SourceChecked = checked, true
		case ctx.Err() != nil:
			return err
		default:
			// The unchecked answer is still useful: keep it.
			w.log.Warn("source check failed; keeping the unchecked answer", "node", t.node.ID, "err", err)
		}
	}
	var res *store.FinishResult
	err := w.commit(ctx, t.job, func(tx pgx.Tx, client *river.Client[pgx.Tx]) error {
		var err error
		res, err = store.Finish(ctx, tx, t.node.ID, store.StatusDone, &out, nil, ev)
		if err != nil {
			return err
		}
		if !res.Changed {
			return errCancelled
		}
		return w.enqueueReduce(ctx, tx, client, t, res.ParentReady)
	})
	if errors.Is(err, errCancelled) {
		return river.JobCancel(err)
	}
	if err != nil {
		return err
	}

	done := t.event(events.NodeDone, role)
	done.Stats = ev
	w.publish(ctx, done)
	if res.IsRoot {
		w.publish(ctx, events.Event{Type: events.TreeDone, TreeID: t.node.TreeID, NodeID: t.node.ID, Text: out})
	}
	return nil
}

// fail marks the node failed and lets the parent proceed to reduce with
// partial results. A non-retryable error cancels the job outright; otherwise
// the error is returned and River discards the job as out of attempts.
func (w *Worker) fail(ctx context.Context, t *task, cause error, retryable bool) error {
	msg := cause.Error()
	// The job context may already be past its deadline; recording the
	// failure must still succeed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var res *store.FinishResult
	err := pgx.BeginFunc(ctx, w.db, func(tx pgx.Tx) error {
		var err error
		res, err = store.Finish(ctx, tx, t.node.ID, store.StatusFailed, nil, &msg, nil)
		if err != nil {
			return err
		}
		return w.enqueueReduce(ctx, tx, river.ClientFromContext[pgx.Tx](ctx), t, res.ParentReady)
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	if res.Changed {
		w.log.Error("task failed", "node", t.node.ID, "role", t.job.Args.Role, "err", cause)
		ev := t.event(events.NodeFailed, t.job.Args.Role)
		ev.Text = msg
		w.publish(ctx, ev)
		if res.IsRoot {
			w.publish(ctx, events.Event{Type: events.TreeFailed, TreeID: t.node.TreeID, NodeID: t.node.ID, Text: msg})
		}
	}
	if !retryable {
		return river.JobCancel(cause)
	}
	return cause
}

func (w *Worker) enqueueReduce(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], t *task, parent *store.Node) error {
	if parent == nil {
		return nil
	}
	_, err := client.InsertTx(ctx, tx, TaskArgs{
		TreeID: parent.TreeID, NodeID: parent.ID, Role: RoleReduce, Model: t.set.Reducer.Model, Priority: t.job.Args.Priority,
	}, nil)
	return err
}

// commit runs fn and completes the job in the same transaction, so node state,
// new jobs, and job completion become visible atomically.
func (w *Worker) commit(ctx context.Context, job *river.Job[TaskArgs], fn func(pgx.Tx, *river.Client[pgx.Tx]) error) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	return pgx.BeginFunc(ctx, w.db, func(tx pgx.Tx) error {
		if err := fn(tx, client); err != nil {
			return err
		}
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

// publish is best effort: a lost event degrades the live view but must not
// fail a task whose result is already committed.
func (w *Worker) publish(ctx context.Context, e events.Event) {
	if err := w.bus.Publish(ctx, e); err != nil {
		w.log.Warn("publish event", "type", e.Type, "tree", e.TreeID, "err", err)
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d)
}
