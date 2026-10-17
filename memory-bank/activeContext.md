# Active Context

_Last updated: 2026-10-12_

## Current focus
**Deployed and live** on the user's cluster: namespace `agent-spawner`, console at http://192.168.1.64:30880 (NodePort 30880 on any node). See deployment.md for every detail. Next steps depend on the user.

## Recent changes
### 2026-10-17: Markdown rendering in the console (0.1.22, branch `feat/render-markdown`)
- `internal/ui/static/markdown.js`: own renderer, no dependency. Parses to a tree and builds DOM (never HTML strings): raw HTML stays text; links only http(s)/mailto (one level of parentheses allowed in targets); images become links (nothing fetched); single newlines kept as line breaks (list/concat merges are one item per line). Supports headings, emphasis, strikethrough, inline/fenced code, nested lists, quotes, tables with alignment, rules, backslash escapes.
- Result panel renders Markdown with a Raw/Rendered toggle (Copy copies raw). Agent cards stream plain text and switch to rendered when the node is done (reset if it restarts).
- Tests: `node --test internal/ui/markdown.test.js` (fake DOM, escaped serialization; `internal/ui/package.json` marks ES modules for Node, not embedded). CI runs it.
- Also fixed: the Parallel agents chart drew `NaN` paths for single-agent runs (segments without timestamps), which logged console errors.
- Verified on the cluster with Playwright: the book result (15 bold, 16 list items), raw toggle, 78/79 cards rendered, a fresh table run, and hostile input (javascript: link, <img onerror>, <b>) rendered inert; no dialogs, no console errors.
- The scratchpad (old Playwright suites, fakellm, start-ui.sh) was wiped by a session restart; Playwright 1.55.0 matches the cached chromium-1187.
### 2026-10-13: LM Studio slowdown fixed by host restart; parallelism benchmark
- After the user restarted the LM Studio host, no model was loaded; loaded qwen/qwen3.5-9b via /v1/discovery/load at 32k (parallel 8 came from LM Studio's saved settings). Prefill back to ~5,200-5,500 tok/s (was ~530). Book rerun (17 files, 78 agents): 276 s, 0 failures, 4 in-job overflow retries.
- Concurrency benchmark direct to LM Studio (qwen3.5-9b Q8, 32k shared ctx, 8 slots; scratchpad parbench.py, closed loop):
  - Decode-heavy (500 in / 512 out): output 54 tok/s at N=1 -> 90 (2) -> 138 (4) -> 170 (6) -> 193 (8) -> flat 191/187 at 12/16 (queueing; latency doubles). Per-stream 55 -> 26 tok/s at 8.
  - Mixed (2k in / 256 out): 11.5 -> 19.2 -> 27.5 -> 31.9 -> 34.8 req/min at 1/2/4/6/8; flat at 12.
  - Prefill-heavy (6k in / 32 out): 35 -> 42 -> 43 -> 43 req/min at 1/2/3/4; prefill saturates at ~4k tok/s by N=2. N=6 (36k > 32k context) -> every request overflowed.
  - Knee: 6-8 for generation-heavy work (8 = slot cap; +14% from 6 to 8); 2 for reading-heavy work. Shared-context rule: N x (prompt + output) <= ~29k, so 8-way needs requests under ~3.5k tokens.
- End to end, 48 passages x ~3k tokens summarized: direct at 8 concurrent 50.3 s; spawner 58.8 s (17% overhead), avg concurrency 5.7 / peak 8. Cause: admission reserves max_tokens (default 1024) per request though outputs were ~74 tokens. Next: size the output reserve from part_max_words / observed outputs.
### 2026-10-13: shared-context overflows on book text (0.1.19, merged via PR #6)
- User's book-title run 2c280373 stalled 22+ min with LM Studio "Context size has been exceeded" (n_tokens ~14.5k per request, 32k context shared by 8 slots).
- Causes: (1) admission estimated chars/4, but book text measured 3.4 chars/token median, 1.9 worst (25/29 requests above the estimate); (2) the "retry overflow without burning an attempt" path never fired: it shared River's snooze counter (limit 30) with queue-saturation snoozes, which reached 598-715 per job, so every overflow used up an attempt.
- Fixes: pool learns real/estimated prompt-token ratio per model from response usage (`AcquireRequest`, moving mean + 1 deviation; overflow bumps ×1.25 before any observation, ×1.05 after); overflows are retried inside the job (`generateRetrying`, up to 8, re-admitting with the new ratio), never using an attempt; a repeated `node.started` tells the console to clear the abandoned attempt's partial text (also explains earlier card-text mismatches, unverified); splitting estimates chars/3.
- Lesson: taking the maximum observed ratio (0.1.18) pinned it at 2.45 and serialized the pool (1 agent at a time, 25+ min). A mean + deviation is needed.
- Structural limit: ~14k-token sections in a 32k shared context allow at most 2 concurrent; the old run's "7 running" was overflow churn.
- Verified on 0.1.19: rerun of the same 17 book files (78 agents): 78/78 done, 4 overflow retries, 0 overflow failures (1 job timeout from slowness).
- **LM Studio host slowed ~9x overnight** (prefill median 2,000-4,800 tok/s through 2026-10-13 01:29 UTC, ~520 tok/s from 13:28 UTC, also when idle: 9k-token prompt took 16 s). Cause on the host unknown (GPU offload/driver/other GPU load?); asked the user to check. Run timings after that point are not comparable with earlier ones.
### 2026-10-13: composer typing lag with no jobs running (0.1.17, merged via PR #5; user confirmed "much better")
- Cause: `GET /v1/trees?limit=100` returned every run's full prompt (document runs carry the whole document: max 364k chars; 3.1 MB total). The console refetched it every 10 s and kept ~3.1M characters in the sidebar beside the composer, so every keystroke cost ~23 ms.
- Fix: listing sends `left(prompt, 400)` plus `prompt_chars` (`store.ListPromptChars`); the sidebar also cuts to 400; the run header shows a 2,000-char preview with click-to-expand.
- Measured on the cluster (Playwright, ~25 s of steady typing, idle page): median 23 ms → 6 ms per key; listing 3.1 MB → 48 KB.
### 2026-10-13: console typing lag during live runs (0.1.16, merged via PR #4)
- Cause: the live run view repainted every 150 ms and rewrote every card (pill, meta, title, error), read each card's full prompt back via textContent (~60k chars × 32 cards), rebuilt timeline segments, and measured scroll per streaming card (forced layout each). Keystrokes in the composer waited behind it.
- Fix: `once()` memo per card (write only on change), `patchChildren()` for timeline/axis, `streamInto()` appends deltas, scroll-follow tracked by scroll events and applied after all writes, task prompt shows a 600-char preview until clicked, `.card { content-visibility: auto }`.
- Measured (Playwright, live 32-agent run on the cluster): keystroke 40 ms → 16–24 ms; locally renderCards 412 ms → ~0 per 5 s, layout 376 → 18 ms.
- Streamed card text matched final outputs 32/32 on two checks; one earlier check showed 8 mismatches whose cause is unexplained (not retries; the code only appends to strictly growing text). Unverified: possibly a replay timing issue. Watch for it.
- User's run edfcbbc3 (book titles, 10 books, 55 agents): the "Reply with one line" instruction also reached the llm merge, so the result was one title; splitting books into sections wasted agents (titles are on page 1); CBD Handbook section 1 failed with LM Studio "Context size has been exceeded". Suggested merge list + head of each file; offered a `head_tokens` option.
### 2026-10-13: per-part word cap (0.1.13, merged via PR #3)
- `part_max_words` (API, MCP run_task/analyze_files): appends "Keep your answer under N words…" after each part's text; the merge keeps the unchanged prompt. Opt-in (can drop items in extraction tasks).
- Measured on the same 29-file docs input: **112 s** with a full-length final answer (3,862 chars), against 238 s uncapped and 113 s with the cap written into the prompt (final shrank to 801 chars).
- Incident during the deploy: shared Postgres `default/postgresql-0` was OOMKilled (limit 192Mi with shared_buffers 128MB; 18 spawner connections plus large prompt rows). The API crash-looped on "connection refused" until it came back. **The user patched the StatefulSet to 512Mi limit / 256Mi request** (my patch was blocked by the permission classifier as a shared-resource change). It is Helm-managed (bitnami postgresql-16.7.4), so a `helm upgrade` would revert it unless `primary.resources` is set in values.
### 2026-10-12: large-part splitting + largest-first (0.1.12, merged via PR #2)
- Given parts over `max_part_tokens` (default **16000**; -1 off) are split at line boundaries into sections (text kept byte-exact, `splitText`), titled "<file> (section i/n)". Jobs are inserted largest first (River runs equal-priority jobs in insertion order); node idx keeps file order for the merge. >400 expanded parts → 400. analyze_files default max file size 120 KB → 400 KB.
- **Measured on the user's 29-file docs run (172k input tokens, qwen3.5-9b, 8 slots, 32k shared ctx), same inputs rebuilt from the stored prompts:** baseline 219 s; split at 6k: **343 s** (46 agents, output 15k→26k tokens, extra merge pass 88 s); split at 16k: 238 s; largest-first only: 203 s and 248 s (run-to-run noise ±20%); 16k + "at most 120 words": **113 s** (output 4.8k tokens).
- **Lesson: per-part summary work is output-bound** (each agent writes ~550 tokens regardless of input size; aggregate decode ~80–105 tok/s on the GPU). More parts = more total generation. Splitting is for context safety, not speed. Speed lever = shorter per-part outputs. My predicted 1.5–2 min from splitting at 6k was wrong.
- Open idea: a separate per-part instruction / output cap so the final merge isn't also shortened (the 120-word prompt shrank the final answer to 801 chars).
### 2026-10-12: MCP server (0.1.10, merged to main via PR #1)
- `internal/mcpserver` (official Go SDK v1.8.0): tools run_task, analyze_files (stdio only), get_task, cancel_task, list_models. Progress notifications per finished agent; auto chunk sizing (`AutoChunks`: ≤6k tokens → 1, ≤30k → 4, else ceil(t/7500) up to 16).
- `spawner mcp` = stdio server, a thin client of the API (`SPAWNER_URL`, token from `SPAWNER_TOKEN` / `SPAWNER_TOKEN_FILE` / `~/.config/agent-spawner/token`). Needs no DB.
- `/mcp` on the API: stateless Streamable HTTP (2 replicas, no affinity), protected by auth middleware; tools call the API on loopback with the caller's own Authorization header. Registered per method because a bare `/mcp` conflicts with `GET /` in the Go mux (panicked at startup; caught by the e2e run).
- API: `parts: [{title,text}]` (≤64) = caller-given split, one agent each.
- Cluster overlay sets `PUBLIC_URL` for console links. Image gets `-X main.version`.
- The user's sign-in problem was only the base64 secret value; they decoded it and it works. A base64-accepting login was tried, then dropped at the user's word.
- Verified: unit + in-memory MCP tests, Parts integration test, local e2e (stdio + HTTP, fake LLM), cluster e2e against LM Studio (4.3 s / 2.9 s / 2.8 s).
### 2026-10-12: GitHub repo, CI, authentication (0.1.9)
- Public repo https://github.com/BiodigitalJaz/go-agent-spawner (SSH remote); module renamed to `github.com/BiodigitalJaz/go-agent-spawner`.
- CI (`.github/workflows/ci.yml`, actions v7): gofmt, vet, build, race tests with Postgres service + NATS container, JS/kustomize/Python checks, image build + smoke. The first run passed (test 116 s).
- `internal/auth`: bearer tokens (`AUTH_TOKENS`), stateless HMAC session cookie (key derived from the token set; revocation by removing the token), SameSite=Strict + Origin check on cookie writes, Secure only over HTTPS. Public: /healthz, /readyz, /v1/login|logout|session, static UI. Console: sign-in overlay, sign-out button, 401 → re-login.
- Verified locally and on the cluster: 401/200 matrix, browser sign-in flow, cookie flags, cross-site write 403, bearer SSE scripted run, all earlier browser suites with login.
### 2026-10-12: shared context (0.1.7/0.1.8), preStop; experiment v2 results
- **Finding:** LM Studio's context (32k) is **one pool shared by all parallel slots**. Four 10.7k-token chunks at once → mid-stream `{"code":500,"message":"Context size has been exceeded."}` (transient). A single request > ctx → embedded `returned 400 … exceeds the available context size` (permanent).
- **Bug I introduced in 0.1.4** (keyword "context size" → 400) made the transient 500 permanent → chunked 42k/62k runs lost every part in v2 (recall 0). Fixed: `streamErrorCode` precedence is code field → "returned NNN" → nested `"code":NNN` → keywords. `llm.ContextPressure` → `JobSnooze(~2 s)`, up to 30 per job (metadata "snoozes").
- **Prevention:** `ModelConfig.SharedContext` (migration 005, UI checkbox prefilled for LM Studio). `pool.AcquireTokens` budgets estimated tokens (chars/4 + reserve; 90% of ctx) across workers (heartbeat `EndpointStatus.Tokens`, `PeerLoad{Inflight,Tokens}`). Worker estimates per role (`Worker.estimate`, `estimateRequest`).
- **Second regression caught (0.1.7):** a chars/4 estimate rejected a 28k-token single request (estimated 35k) with ErrTooLarge. 0.1.8 removed ErrTooLarge: oversized estimates are capped at the budget (they wait for an idle pool) and the server decides. **Lesson: never use an estimate for a permanent decision.**
- **Deploy availability:** rollouts briefly refused connections (seen 3×). Added `lifecycle.preStop.sleep: 5s` to api and worker (native sleep; the image is distroless). Verified: 331/331 requests OK during an API rollout restart.
- **Experiment v2 (0.1.6, ≤23k): 4 chunks + list merge = 55–65% faster than single, 15/15 perfect.** Beyond-context reruns: run3 (0.1.7, chunked valid) and run4 (0.1.8, all conditions).
- Cluster: qwen9b and qwen9b-fast have `shared_context: true`, ctx 32000, slots 8.

### 2026-10-12: code merge (0.1.5) + limiter fix (0.1.6); experiment v2
- **Merge strategies** (`settings.Merge`: llm | list | concat; per-run `merge` on POST /v1/trees; default in the Settings page; per-run select in the composer). `codeMerge` (merge.go); `Worker.reduceInCode` skips Acquire (no GPU slot), with stats model `merge:<mode>`. `complete()` accepts a nil lease. `Worker.outcome` holds the shared retry/fail logic.
- Tests: unit TestCodeMerge; integration TestCodeMergeSkipsModel (exactly N model calls for N parts; failed part visible; nothing invented). Browser: merge-ui.mjs plus all earlier suites pass locally and on the cluster.
- **Limiter fix (0.1.6):** with `slots` set, `limiter.Config.IgnoreLatency` (only errors cut the limit). Found by the burst regression: TTFT rises ~2.5× under 8-way batching, and tiny-prompt traffic lowers the baseline, so AIMD wrongly cut 8→5.3 (62.7 s, 132 tok/s). After the fix the limit holds 8 (54.8–56.8 s, ~150 tok/s) even after a deliberate tiny-prompt warm-up.
- Experiment v2 running/ran: `experiments/chunking/run2_{main,big}.json` (conditions 1, 4 (llm), 4L, 8L; same seeds as v1).

### 2026-10-12: chunking experiment results + 0.1.4
- The user asked for data on "context chunking through multi-agent spawners is more performant than single context". Results are in `experiments/chunking/RESULTS.md`: chunking wins on accuracy (>10k tokens), capacity (beyond the context) and time to first result; a single context wins on end-to-end latency while the document fits (LLM merge ≈ +8.5 s). Recommended next feature: `merge: "concat"` for list outputs.
- 0.1.4 deployed: `llm.streamErrorCode`. Harnesses are now in the repo: `experiments/chunking/chunk_experiment.py`, `experiments/stress/{stress,nest,ctx}.py`.

### 2026-10-12: next steps 1–3 + chunking experiment (released 0.1.3)
- `ModelConfig` gained `context_length` and `slots` (migration 004, registry CRUD, API, UI form prefilled from discovery: ctx = loaded instance context, slots = parallel, per-worker limit = ceil(parallel ÷ workers)).
- **Shared slots:** workers heartbeat every 500 ms; `watchPeers` (main.go) sums other workers' in-flight per model/endpoint into `pool.SetPeerInflight`; `tryAcquire` caps local + peers at `Slots` under a per-endpoint mutex. Stale peers (>3 s) are dropped.
- **Merge guard** (`internal/agent/merge.go`, `Worker.consolidate`): budget = ctx×0.9 − reserve(reducer max_tokens or min(4096, ctx/4)) − fixed prompt; items use child completion tokens; planBatches, then parallel consolidation (each batch acquires its own slot), up to 4 passes; fitItem truncates visibly; when no two items fit together, each is given an equal share. Stats: merge_passes, merge_batches, truncated, prompt_tokens.
- **Document mode** (`TreeRequest.Document/Chunks`, API `document`/`chunks`): `splitDocument` cuts on paragraphs (falling back to lines, then sentences); chunked trees skip the planner (root inserted already split); single mode puts the doc in one exec context. UI: an optional Document box in the composer.
- UI fix: stale discovery responses are ignored (sequence counter).
- Cluster config: qwen9b-fast start 4 / max 4 per worker, ctx 32000, slots 8; qwen9b start 2 / max 4, ctx 32000, slots 8. Note: slots are per routing name, so two names sharing one server model could together exceed the server's slots.
- Verified burst40: 54.4 s, 149.8 tok/s, TTFT max 0.6 s, in-flight 8/8 from about 5 s, never above 8.
- **Lesson (tests):** a Playwright full-page screenshot right before a human-speed click shifts the scroll position, so the click lands elsewhere. Take screenshots after interactions.

### 2026-10-12: model discovery & selection (released 0.1.2); cluster now runs qwen3.5-9b
- The user asked to test with qwen3.5-9b and add "model discovery and selection" to Models & capacity. The user ejected the 27B and embedding models in LM Studio themselves.
- New `internal/discovery`: LM Studio `/api/v1/models` (size, quant, loaded instances with context/parallel/TTL, reasoning options), falling back to `/v1/models`. Load and unload go through `/api/v1/models/load` `{model, context_length}` and `/api/v1/models/unload` `{instance_id}`.
- New `internal/models.Registry`: backends.json models (read-only, they win on a name clash) plus `agent_model` rows (migration 003). Reloads on NATS `agents.models.changed` and every 15 s. `backend.Pool.SetModels` applies changes (unchanged models keep their limiters). `agent.QueueFollower` adds and removes River queues at runtime for workers without `WORKER_QUEUES`.
- API: `POST/PUT/DELETE /v1/models[/{name}]`. Delete is refused while the model is a role in settings, used by a running tree's snapshot, or has unfinished jobs. Also `GET /v1/discovery[?url=]`, `POST /v1/discovery/load|unload`.
- UI: the Models page has pool cards (source badge, Edit and Remove for console models), plus a Discover section (hosts from the pool plus localStorage extras, per-model Load/Unload with a context select, and Add to pool with a reasoning select built from the model's allowed options).
- **Verified reasoning control (LM Studio, qwen3.5-9b):** `reasoning_effort: "none"` turns thinking off (0.4 s against 37.7 s). `reasoning: "off"` and `chat_template_kwargs.enable_thinking=false` are ignored; `low` is unsupported on 9B and falls back to on (LM Studio warns). The UI maps off → `"none"` and hides "on" (the default).
- **Cluster state now:** LM Studio has `qwen/qwen3.5-9b` loaded (16k ctx, 4 parallel, 11.4 GB of 16 GB VRAM). Pool: `qwen27b` (config), `qwen9b` (console, reasoning default), `qwen9b-fast` (console, reasoning off). **Settings v1: all roles = `qwen9b-fast`.**

### 2026-10-12: cancel button fix (released 0.1.1, user confirmed it works)
- Bug: "Cancel run" didn't work for real users. The live tree view rebuilt the header every 150 ms, so a human click (about 180 ms between press and release) hit a replaced button and no click fired. Playwright's instant clicks hid it. The backend cancel was fine all along.
- Fix: clickable regions (tree header with the cancel button, timeline rows, sidebar run list, result block) are now built once and updated in place (`setText`, `setPill`, `syncOrder`). Cancel also disables itself while the request is in flight.
- Verified with **human-speed clicks** (mouse down, 180 ms, mouse up) locally and on the cluster: cancel, timeline-row focus and list navigation all work, and the full UI regression passes.

### 2026-10-12: first cluster deploy
- The user asked to deploy, create a namespace, and expose the UI on a NodePort.
- The user chose: a **public** ghcr image (`ghcr.io/biodigitaljaz/agent-spawner:0.1.0`; the user flipped visibility in the GitHub UI), and a **dedicated `spawner` role and database on the shared `default/postgresql`**.
- Added `deploy/kustomize/overlays/andor/` (namespace, image tag, LM Studio backends.json, NodePort 30880 patch).
- Fixed base manifests: the NATS config moved to a ConfigMap file (the invalid `--max_mem_store` flag was removed); River UI tag is `0.21.0` (no `v`), in the base and in docker-compose. The base image is now `ghcr.io/biodigitaljaz/agent-spawner`.

### 2026-10-11: UI, editable agent settings
- **The user asked for:** "a visual on a user prompt execution and the progress of all the agent spawns" and "configuration for models used and agent directives".
- **Scope decision (Claude's, stated to the user):** the UI edits which model each role uses, its generation params and its directive. Endpoints and limits stay in `backends.json` (infrastructure) and are shown read-only with live gauges. Adding or removing backends from the UI would need River `Queues().Add` at runtime and is not built.
- New `internal/settings`: a versioned singleton in `agent_settings`, plus a per-tree snapshot in `agent_tree`. Workers read each job's tree snapshot (`settings.ForTree`), so edits never affect a running tree.
- Migration 002: `agent_settings`, `agent_tree`, and timeline columns on `agent_node` (`role, started_at, split_at, reduce_started_at, finished_at, stats`).
- Worker refactor: a per-job `task{job,node,set}`. Reduce now uses `Reducer.Model` (previously the parent's model). The planner's editable directive has fixed `planRules` appended.
- New API endpoints: `GET /v1/trees` (list with counts), `GET /v1/events` (global lifecycle SSE), `GET/PUT /v1/settings` (409 on a stale version, 422 on validation), `GET /v1/models` (config plus worker heartbeats). Overrides on `POST /v1/trees`: `model`, `max_depth`, `max_children`.
- Workers publish limiter heartbeats every second over core NATS (`agents.workers.<host>`). The API keeps the latest per worker and drops it after 6 s.
- UI: `internal/ui/static/{index.html,style.css,app.js}`, embedded and served at `/` by api mode. Vanilla JS, no build step, no CDN.
- **Bugs fixed:**
  - The LLM client accepted a stream that ended without `[DONE]` or `finish_reason` as a successful (empty or partial) answer. It now returns `llm.ErrTruncated` (retryable), and a mid-stream `{"error":...}` payload becomes a retryable `StatusError` (502).
  - Shutdown hung on open SSE connections. The HTTP server's `BaseContext` is now cancelled when shutdown starts.
  - UI: null children rendered as "null"; replayed events reset finished nodes' status; reasoning was dropped when reloading a finished run; mobile overflow; a cramped composer.

## Next steps (suggested; nothing confirmed by the user)
0. **From the stress tests (2026-10-12):** (a) reduce-context guard / hierarchical merge (silent topic loss seen); (b) per-model global concurrency: cap the sum of worker limits at the server's parallel slots (e.g. set `max_limit` = slots ÷ worker replicas, or derive it from discovery's `parallel`); (c) done: parallel 8 / ctx 32k measured (+27% burst throughput, TTFT max 6.9→1.4 s, context loss gone in the retest); (d) suggested: raise qwen9b-fast `initial_limit` 2→4 so the pool starts at the 8-slot capacity instead of ramping over about 45 s.
1. ~~Planner speed~~ solved: qwen9b-fast plans in 4–8 s (against 67–78 s on the 27B with reasoning). Possibly compare quality: worker on `qwen9b` (reasoning on) against `qwen9b-fast`.
2. ~~Auth~~ done in 0.1.9. MCP done in 0.1.10 (auto chunk sizing lives in the MCP layer). Next candidates: auto chunk sizing in the API itself, streaming list merge, overlapping chunk boundaries, structured merge, tokenizer-based admission, cross-chunk reasoning experiment, 3070 Ti emulation test.
3. Pin LM Studio's IP (DHCP reservation) or give it a DNS name; validate DNS discovery with an in-cluster backend later.
4. Prometheus metrics; limiter normalization for prompt length.
5. Possibly: manage backends from the UI; per-run directive overrides; markdown rendering of results; a cmd/fakellm dev tool committed to the repo (it currently exists only in the session scratchpad).

## Open questions for the user
- GPU inventory: in-cluster vLLM, or LM Studio workstations?
- NATS HA needed?

## Preferences and patterns learned
- The user discusses design first, then wants it built. They delegate technical choices, so recommend rather than list options.
- They prefer open source over paid add-ons.
- Idiomatic Go and the standard library where reasonable. Comments explain *why*.
- The UI must work offline (no CDN) and in both light and dark themes, and must not overflow at phone width.
