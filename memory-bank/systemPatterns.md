# System Patterns

## Architecture: control plane vs data plane
```
POST /v1/trees ──► root agent_node + plan job (one tx)        [spawner-api]
                          │
            River queue "llm_<model>" (Postgres, priority 1-4)
                          │  any worker replica fetches (LISTEN/NOTIFY or poll)
                          ▼
   agent.Worker.Work ── pool.Acquire(model, ACQUIRE_WAIT)        [spawner-worker]
      │                   └─ ErrSaturated → river.JobSnooze(jitter) → another replica
      ├─ plan:   JSON-schema call → direct | split
      │            split → store.Split + InsertManyTx(children) + JobCompleteTx (one tx)
      │            direct → exec inline if planner model == node model, else enqueue exec
      ├─ exec:   stream → NATS deltas → complete(): Finish + maybe enqueue parent reduce + JobCompleteTx
      └─ reduce: load children outputs → stream synthesis → complete() → propagates up
GET /v1/trees/{id}/events ◄── SSE ◄── NATS JetStream ordered consumer
```
- **River (control plane):** which task runs next, durability, retries, distribution, priorities.
- **Goroutines and the backend pool (data plane):** which GPU endpoint and when; streaming.
- **NATS (feedback):** high-rate, disposable events. Tokens never touch Postgres.

## Key decisions and why
- **NATS over Postgres NOTIFY for tokens.** NOTIFY takes a global lock at commit and has an 8 KB payload limit. River already uses NOTIFY to wake workers on insert, so token traffic would slow down dispatch. JetStream adds replay (Last-Event-ID) and subject wildcards for future cross-agent steering.
- **No queue abstraction over River.** Transactional enqueue and completion are the reason to use River, and an interface would hide them.
- **Open-source River replacements for Pro features:**
  - Workflows are replaced by the `agent_node` table with `pending_children`. The parent row lock serializes sibling decrements, so exactly one sibling sees zero and enqueues the reduce.
  - Global concurrency limits are replaced by per-replica AIMD plus `JobSnooze`. Snoozes ≤ River's scheduler interval (5 s) go straight back to `available` and don't count as attempts.
- **One River queue per model.** Routing tasks to the right model needs no code, and models stay loaded instead of being swapped.
- **Small TaskArgs** (`tree_id, node_id, role, model, priority`). Prompts and outputs live in `agent_node`. Uniqueness is on (tree, node, role) via `river:"unique"` + `ByArgs`, so inserting a reduce is idempotent.
- **Continuation style.** A parent completes its job when it splits, and the reduce is a new job. No worker slot is ever held waiting on children, so nested splits can't deadlock.
- **Agents are goroutines, not pods.** Pod and model startup takes seconds to minutes.
- **Model registry.** Routable models = backends.json (read-only) + `agent_model` (console). Every replica runs `models.Registry`; changes go out as NATS `agents.models.changed`, with a 15 s poll fallback. On change: `pool.SetModels` (diff: keep unchanged models and their limiters, rebuild changed ones, drop removed ones while in-flight leases finish) then `QueueFollower.Sync` (River `Queues().Add/Remove`). Model ids can be repeated under different routing names with different `extra` (e.g. `qwen9b` vs `qwen9b-fast` with `reasoning_effort: none`).
- **Settings vs infrastructure.** Agent behaviour lives in `agent_settings` (versioned and UI-edited). Each tree copies it into `agent_tree.config` at start, and every job reads its tree's snapshot. Endpoints and limits live in backends.json. Defaults come from `planner`/`default`/`MAX_DEPTH`/`MAX_CHILDREN` until the first save.
- **Planner directive + fixed rules.** The editable part is the guidance; `planRules()` (count limits, self-contained prompts, no "combine" subtask, empty subtasks when direct) is always appended because the split/join machinery depends on it.

## Merge strategies
`settings.Merge`: `llm` (reducer model; the merge size guard applies), `list` (code: distinct lines, ignoring case and spacing, in part order), `concat` (code: verbatim, never drops lines; safe for code). Code merges run before any Acquire (no GPU slot) and report failed parts inline. Chosen per tree via the settings snapshot.

## Adaptive limiter (`internal/limiter/aimd.go`)
- **When the model has `slots`, `IgnoreLatency` is on:** capacity is known and enforced pool-wide, so TTFT growth is batching load, not queueing. Only overload errors cut the limit. Latency-based AIMD remains for models without slots.
- Congestion signal: a TTFT sample > `baseline × Tolerance(2.5)` → limit × 0.8. 429/5xx/transport errors → × 0.5. Decreases have a 1 s cooldown.
- The limit grows by `+1/limit` only when it was saturated at release time, so an idle backend doesn't drift upward.
- The baseline is the minimum TTFT, drifting slowly toward recent samples (1/50).
- `Ignore` outcome for caller cancellation or client errors (4xx), and for timeouts after the first token.

## Backend pool (`internal/backend/pool.go`)
- Each endpoint has its own limiter. `pick` sorts by headroom, then by faster baseline.
- Waiters block on a broadcast channel that is replaced on every release or endpoint change.
- `dns+http://svc:port` endpoints are re-resolved every `DISCOVERY_INTERVAL`. With a headless Service there is one A record per *ready* pod, and existing endpoints are kept if a lookup fails.
- Ejection: 3 consecutive transport failures → endpoint skipped for 5 s.
- `Lease.Done(stats, err)` must be called exactly once (guarded by `sync.Once`), as soon as generation ends and before any DB work.

## Failure and cancellation paths (`internal/agent/worker.go`)
- Node already terminal at job start → return nil, or `JobCancel` if the node was cancelled.
- Retryable error with attempts left → return err (River backs off).
- Final attempt or non-retryable (4xx) → `fail()`: mark the node failed and propagate to the parent (the reduce still runs with partial results), using a fresh context because the job's may have expired. Non-retryable → `JobCancel`.
- Job timeout (`DeadlineExceeded`) is treated as an ordinary failure. Only shutdown or `context.Canceled` goes to `interrupted()`.
- Shutdown: SIGTERM → `client.Stop` (soft, up to `SHUTDOWN_TIMEOUT`) → if it times out, `ShuttingDown()` + `StopAndCancel` → interrupted jobs return `JobSnooze(0)` and are requeued without using up an attempt.
- **River is started with `context.WithoutCancel(ctx)`**, because cancelling the context passed to Start is River's *hard* stop.
- Tree cancel: `store.CancelTree` + `JobList` by `args->>'tree_id'` (partial index) + `JobCancel` each. River cancels running job contexts remotely, and any error after that becomes cancelled.

## LLM stream completeness
A stream counts as complete only on `[DONE]` or a non-empty `finish_reason`. Otherwise the client returns `llm.ErrTruncated` (retryable), and a `{"error":...}` chunk becomes a `StatusError{502}`. Never treat a dropped stream as an answer.

## HTTP shutdown with SSE
The server's `BaseContext` derives from a context that is cancelled at the start of shutdown. Without it, open SSE streams (every console tab holds `/v1/events`) block `Shutdown` until the timeout.

## Web console (`internal/ui`)
- Vanilla ES module with an `h()` element helper. Hash routes: `#/runs[/<id>]`, `#/settings`, `#/models`.
- Tree view: load `GET /v1/trees/{id}` (snapshot), then the SSE replay. Nodes that are terminal in the snapshot are `frozen`: replayed `delta` only records timing, `thinking` still appends (reasoning isn't stored in the DB), and replayed lifecycle events never regress their status.
- **Rule: never recreate an element a user can click during a live repaint.** A click needs mousedown and mouseup on the same element, and the live pump repaints every 150 ms. Build controls once and update them in place (`setText`/`setPill`/`syncOrder` in app.js); only non-interactive content (timeline segments, chips) may be rebuilt. UI tests must use human-speed clicks (down, about 180 ms, up), not Playwright's instant `click()`.
- Per-node DOM is created once and updated in place (keeps scroll and expanded state). Painting is coalesced on a 150 ms pump plus requestAnimationFrame; the timeline is rebuilt each paint while live.
- Timeline segments come from node timestamps: queued, plan, exec (with a "thinking" striped overlay until the first delta), waiting, reduce. The concurrency chart is a step function over the plan, exec and reduce segments.
- The settings page tracks dirty state by JSON comparison, warns before unload, and shows a reload banner on 409.

## Node status machine
`queued → running → (done | failed)` for exec.
`queued → running → waiting → reducing → done | failed` for a split node.
Any non-terminal status → `cancelled`. Root node id == tree id.

## Event types (`internal/events`)
`tree.created` (published by the API), `node.started, thinking, delta, node.split` (children carry id/title/role/prompt), `node.done` (stats include model and endpoint), `node.failed, tree.done, tree.failed, tree.cancelled`.
`SubscribeLifecycle` filters `agents.tree.*.*.node.>` and `agents.tree.*.*.tree.>` with DeliverNew, for the global `/v1/events`.
Worker heartbeats go over **core NATS** (`agents.workers.<host>`, no persistence) every 1 s.
Subject: `agents.tree.<tree>.<node>.<type>` in stream `AGENT_EVENTS` (memory storage, MaxAge `EVENT_RETENTION`).
Deltas go through `DeltaWriter`: first fragment sent immediately, then batched every `DELTA_INTERVAL`. `Close()` it before publishing the terminal event, to preserve ordering on the connection.
