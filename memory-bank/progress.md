# Progress

## Status: v0 and web console DEPLOYED to the andor cluster (2026-10-12), console on NodePort 30880


## What works (verified 2026-10-18, 0.1.51): grounded chat answers
- Crossbow/turkey prompt: no refusal (single and pool), crossbows legal in archery fall turkey with dates and specs from ODWC pages, no invented permit fee, full recipes. "See X" link followed. Retry endpoint replaces the reply and purges the old run (404). Phone footer: no overflow.
- Known limit: the 9B model still slips on general knowledge it is not given (anatomy terms, part counts); grounding only covers place/time facts.

## What works (verified 2026-10-18, 0.1.49): chat search auto and mobile
- Search auto, five live chat questions: three how-tos (smoking turkey legs, field dressing, crossbow triggers) searched nothing; two place/time questions (OK turkey season, TX license price) searched. With today's date given to the query writer, queries say 2026 (previously 2024) and the OK season answer cited wildlifedepartment.com.
- Directive v9 retest of the turkey chat: practical steps, no invented prohibition, advice to confirm with ODWC.
- Mobile audit at 390/360 px, all pages: no overflow, no fonts under 16 px in inputs; the only flagged targets are 20 px checkboxes wrapped in their labels.
## Chunking experiment v2 (2026-10-12, code merge): see experiments/chunking/RESULTS.md (data: results_v2.json, 84 valid runs)
- **The theory holds:** 4 chunks + `merge: list` is **51–65% faster** than a single context (4.1–6.9 s against 9.3–14.2 s), **21/21 perfect** recall, first result after 2.6 s (single: 11.2 s), and works past the context (65k tokens in 12.6 s). 8 chunks is slower than 4. The LLM merge stays ~40% slower than single (it re-generates the answer).
- Bugs found by "perform all tests" and fixed: (1) 0.1.4 classified LM Studio's transient shared-context overflow (500) as permanent → v2 lost chunks at 43k/65k → fixed in 0.1.7 (error precedence + ContextPressure snooze + token-budgeted admission via `shared_context`); (2) 0.1.7's estimate-based ErrTooLarge rejected a 28k request → removed in 0.1.8; (3) the AIMD latency signal under-used known slots → `IgnoreLatency` when slots are set (0.1.6); (4) rollouts refused connections → preStop sleep 5 s (331/331 OK during restart).
- Test inventory at 0.1.8: Go unit + integration (race), including TestCodeMergeSkipsModel, TestSharedContextPressure (unbudgeted: recovers from overflow; budgeted: 0 overflows), TestSharedContextBudget, TestSharedSlotsCapAcrossWorkers, TestIgnoreLatency…, TestContextPressureIsTransient. Browser (local + cluster): runs, human-speed clicks, cancel, models (discover/add/edit/remove + shared-context prefill), merge UI. Cluster: burst40 (8/8 slots, ~143–151 tok/s), rollout availability probe.

## Chunking experiment v1 (2026-10-12): LLM merge only (data: results.json, 69 runs)
- Single context against document mode (4/8/16 chunks), 30 planted facts, 2.6k–65k tokens, qwen9b-fast, runs one at a time.
- **Accuracy:** single-context recall 1.0 to 6k, 0.989 at 11k, 0.922 at 18–23k, 0.850 at 28k; fails above the 32k context. **4 chunks: 21/21 perfect**, including 65k tokens. Chunked overall: 45/48 perfect. The merge was lossless (chunk-union recall = final recall).
- **Latency:** single is ~35–40% faster while it fits (8.3→12.6 s); chunked adds a fixed ~8.5 s LLM merge that re-generates the ~400-token answer. First partial result arrives 2–4× sooner with chunking.
- Proposed `merge: "concat"`: built in 0.1.5 as `list`/`concat`; measured in v2 at 51–65% faster than single.
- Scoring lesson: chunk agents sometimes write `name service | port | team team`; the lenient parser fixed apparent 8/16-chunk dips. Always check for scorer artifacts.
- Fixed in **0.1.4**: in-stream errors keep their real status (context overflow = non-retryable 400): fails in 0.9 s instead of ~100 s of retries.

## Stress / boundary tests (2026-10-12, cluster, qwen9b-fast, LM Studio parallel=4, ctx 16k)
Harness: `scratchpad/stress/stress.py` (session-only; not in repo): submits trees, follows SSE, samples limiter state every 1 s.
- **Concurrency ladder** (N direct runs at once, ~150 words each): aggregate 49→76→107 tok/s at N=1→2→4, then a **plateau of about 110 tok/s** (N=6–16). Per-stream drops 54→32 tok/s. N>4 adds pure queueing: tree latency grows linearly (16 runs: p50 19 s, max 30 s). No failures.
- **Boundary 1 = LM Studio's 4 parallel slots.** 2 worker replicas × qwen9b-fast `max_limit` 4 let 5–6 requests through; the extras queue *inside* LM Studio → TTFT max 5–7 s. AIMD reacts per replica but doesn't see the global picture.
- **Fan-out ladder** (forced split 4/8/12/16): the planner complied exactly. Planning 11→32 s and merge 4→41 s grow with fan-out; workers took only 8→24 s. **Boundary 2 = the single-stream planner and reduce** (73 of 97 s at fan-out 16); aggregate drops to about 50 tok/s.
- **Depth:** at depth 1–3 the planner never nests on its own (complex=false). Forced nesting works: depth 2 → 13 agents in 41 s; **depth 3 → 40 agents (27 exec, 13 reduce) in 131 s**, every join correct, TTFT max 11 s.
- **Boundary 3 = merge context (silent).** 12 children × about 800 words → about 14k tokens into a reduce with a 16k context: **no error**, but the merged output (10.7k chars from 69k) **dropped one topic entirely**. Silent quality loss near the context limit. Needs a guard (estimate reduce input against the loaded context; hierarchical or batched reduce, or fail loudly).
- **Burst:** 40 runs at once: all ok in 76 s at 109 tok/s; p50 42 s, max 76 s. River, Postgres and NATS were stable.
- **Rerun after the user set LM Studio parallel=8, ctx 32k (2026-10-12):** VRAM 11.4→12.2 GB.
  - Ladder: TTFT max fell from 5–6 s to under 1 s at every N (no more queueing inside LM Studio). Aggregate only about 110→118 tok/s, because peak in-flight stayed at 5–6: the spawner's AIMD limits (start 2/worker) hadn't ramped to the max of 4/worker within the short phases.
  - **burst40: 59.5 s (was 76.1, −22%), 138.6 tok/s (was 108.9, +27%), TTFT max 1.4 s (was 6.9), peak in-flight 8**. The limit sum ramped 5.6→8 over about 45 s. Per-stream speed 24 tok/s (was 32).
  - **Context retest 12×800 words at 32k: all 12 topics present** (at 16k one was dropped). The merge wrote 2.8k tokens (was 2.1k). This supports 16k-context truncation as the cause (n=1 each).
  - GPU ceiling for qwen3.5-9b Q8 on the RTX 4080 SUPER: about 140 tok/s aggregate at 8 streams.

## Live tests with qwen3.5-9b, reasoning off (2026-10-12, all roles qwen9b-fast)
- Forced 3-way split (same prompt as the 27B run below): **8.5 s total** against 170 s. Plan 4.6 s; 3 parallel agents at about 40 tok/s, TTFT about 0.3 s, about 1 s each; reduce 2.5 s.
- Larger task (4-area onboarding guide, not forced, fan-out 6): the planner **chose to split into 4** on its own; 4 parallel agents (= LM Studio's 4 slots) about 8 s each at 36 tok/s; reduce 24.7 s (1,308 tokens); **43 s total**. The reduce is now the longest phase.
- Hot add on the cluster: both worker replicas picked up console-added models with no restart. Locally, a worker served a new model 0.8 s after Add.

## Live multi-agent test on the cluster (2026-10-12, qwen3.8-27b on LM Studio, depth 1, fan-out 3)
- **Run 1**, a small task (3 one-sentence definitions): the planner chose **direct** after 67 s, reasoning "task is tiny; parallel work doesn't beat one focused answer". One exec took 31 s; 98 s in total. Correct behaviour per the directive.
- **Run 2**, the same task with "split into 3 parallel subtasks" in the prompt: the planner split after 78 s. All 3 agents started in the same second across both worker replicas (slots 2/2 + 1/2). Each agent took 38–50 s at 3.6–4.0 tok/s, with TTFT 3.4–6.0 s (against 1.3 s alone), an aggregate of about 11.6 tok/s against 4.7 for a single stream. The reduce started in the same second the last child finished and took 41 s. 170 s in total with 5 nodes.
- AIMD in action: w9x8s cut its limit 2.0→1.6 on a 6 s TTFT sample, then grew back to 2.2 while saturated.
- **Takeaways:** (1) planning (67–78 s of reasoning) is the biggest single cost; a small non-reasoning planner is the top optimisation. (2) For small tasks, split + reduce is slower end to end than direct, even though parallel execution itself is about 2.5× more throughput. (3) There is no per-run "force split" control yet; the prompt wording works as a workaround.

## What works (verified 2026-10-12): cancel in the UI (0.1.1)
- Human-speed clicks on Cancel run, timeline rows and the run list work locally and on the cluster. Cancelled runs show node and job `cancelled`, and the worker logs "job cancelled remotely" (generation stopped). The user confirmed cancel works.

## What works (verified 2026-10-12): cluster deploy
- All pods are Ready in `agent-spawner` (api×2, worker×2, nats, riverui). Migrations created every table in the `spawner` database.
- The UI returns 200 on 192.168.1.63/.64/.65:30880. `/v1/models` shows both workers' heartbeats against LM Studio 192.168.1.76:1234.
- A live run from a cluster pod against LM Studio `qwen3.8-27b` finished in 19.7 s (TTFT 2.0 s, 5.4 tok/s). Playwright on the NodePort showed the run, live status and 2 workers, with no errors.

## What works (verified 2026-10-11): web console and settings
- Playwright (Chromium) drove the real UI against `spawner all` plus a fake streaming LLM:
  - submitted a split prompt and saw 4 parallel agents (peak 4 in the concurrency chart) with one failing (400); the merge still produced the Result
  - the timeline phases rendered correctly
  - reloading a finished run rebuilt it from the DB snapshot plus replay with no duplicated text and with reasoning kept
  - a settings edit saved and the version incremented
  - the models page showed live gauges from worker heartbeats
  - dark mode worked; 390 px width had 0 px horizontal overflow; no console errors
- Integration tests added for: worker directive applied, per-role model routing (exec on large, reduce on small), timeline columns and stats persisted, snapshot immutability after a settings save, settings version conflicts.
- Unit tests added for: settings validation; truncated stream (`ErrTruncated`); `finish_reason` without `[DONE]`; error payload mid-stream.

## What works (verified 2026-10-10)
- **Unit tests:** limiter (growth only when saturated, latency and overload decreases, cooldown, min floor), LLM client (SSE parsing, reasoning split, usage, error classification, fence stripping, `extra` merge), backend pool (spread across endpoints, saturation, waiter wake-up, ejection, config validation).
- **End-to-end tests** (real Postgres 17 and NATS 2.11, fake LLM, `-race`):
  - split → 3 parallel execs → reduce → `tree.done`
  - direct answer run inline (exactly 2 LLM requests: plan + exec)
  - a failed child (400) is marked failed, and the reduce still merges the partial results
  - cancelling a tree mid-stream: every node cancelled, jobs cancelled or completed, generation stopped
- **Live run against LM Studio** (`qwen3.8-27b`, MAX_DEPTH=1, MAX_CHILDREN=3; 3 haiku prompt):
  - The planner split into 3, and 2 ran at once (initial limit 2). The third started the moment a slot freed, and the reduce was queued in the same second as the last child.
  - The limiter grew from 2 to 2.9. Aggregate throughput was about 7.7 tok/s against 4.4 single-stream.
  - 1,394 thinking events against 37 delta events: reasoning dominates. 575 s in total. Zero warnings or errors in the logs.
- Graceful shutdown drained in-flight jobs on SIGTERM (seen live).
- `kubectl kustomize` renders `base` and `keda` without errors.

## What's left to build
- [ ] **Auth for the console and API.** None exists, and settings/directives are editable by anyone who can reach the service
- [x] Manage models from the UI (discovery, add/edit/remove, LM Studio load/unload). backends.json models stay read-only
- [ ] Discovery hosts beyond the pool's endpoints are kept only in browser localStorage (per viewer)
- [ ] Per-run directive overrides, markdown rendering of results, search/filter in the run list
- [x] Deploy to the cluster (andor overlay; 2026-10-12)
- [ ] Validate DNS discovery against a real in-cluster headless backend Service
- [ ] Prometheus metrics endpoint
- [ ] Planner tuning / skipping it for short prompts
- [ ] Limiter normalization for prompt length
- [ ] Test that drives the job-timeout failure through all of River's retries (backoff makes it slow; the fix is verified only by reading the code)
- [ ] Test for snooze-based handoff between two River clients
- [ ] Unit tests for `internal/api` handlers (covered only by the browser drive, which is not in the repo)
- [ ] Commit a fake LLM dev tool and the Playwright UI check to the repo (both exist only in the session scratchpad)
- [ ] Persistent NATS / HA (single in-memory node in the base manifests)
- [ ] CI pipeline (image publishing is manual: docker build/push to ghcr), git repo

## Known issues and limitations
- The UI run list polls every 10 s, with refreshes debounced on lifecycle events. Fine for tens of trees; the list is capped at 100.
- Reasoning text is only in NATS (15 min window). After it expires, a finished run shows output and timeline from the DB but no reasoning or thinking stripes.
- The per-node "started" time for a direct-inline plan+exec comes from events; from the DB alone the two phases appear as one bar.
- TTFT includes prompt prefill, so very long prompts (the reduce step) can trigger false latency decreases.
- Tokens-per-second in stats includes reasoning time (`Duration - TTFT`), and `CompletionTokens` falls back to a chunk count when the backend doesn't report usage.
- If an exec run inline inside the planner's job fails retryably, the retry re-runs the planner as well (one extra planner call).
- `Pool.Discover` signals waiters on every tick even when nothing changed. This is harmless.
- `CancelTree` caps the job listing at 10,000 jobs per call.
- The KEDA query counts all `llm_%` queues together. Use per-model triggers to scale GPU backends separately.

## Evolution of decisions
1. Started as a pure in-process goroutine pool with a custom priority queue, split/join and an adaptive semaphore.
2. Kubernetes added: agents stay goroutines (not pods). Per-pod limiters via headless Services. NATS or Redis were considered for multi-replica state.
3. River adopted: replaced the custom queue, the durability layer and the need for NATS as a *queue*. River Pro was considered and rejected in favour of OSS (user choice).
4. Event transport: NATS JetStream chosen over Postgres NOTIFY (Claude's call, delegated by the user).
5. After the live test: added thinking events and per-model `extra`, because reasoning models made the feedback loop look dead.
6. 2026-10-11: agent behaviour (role models, params, directives, depth, fan-out) moved from env/config into versioned Postgres settings edited in a web console, with per-tree snapshots. Infrastructure (endpoints, limits) stays in backends.json.
