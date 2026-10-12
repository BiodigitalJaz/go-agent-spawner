# Tech Context

## Stack (versions pinned 2026-10-10)
| Component | Version | Notes |
|---|---|---|
| Go | 1.27.2 | `go.mod` directive; Dockerfile uses `golang:1.27-alpine` → distroless static nonroot |
| River | v0.49.0 (+ `riverdriver/riverpgxv5`, `rivermigrate`) | OSS only |
| pgx | v5.11.0 | `pgxpool` |
| nats.go | v1.54.0 | new `jetstream` package API (`CreateOrUpdateStream`, `OrderedConsumer`, `PublishAsync`) |
| google/uuid | v1.6.0 | |
| golang.org/x/sync | errgroup in main | |
| Postgres | 17 used in tests; cluster version unknown | |
| NATS server | 2.11-alpine (`-js`) | |
| River UI | ghcr.io/riverqueue/riverui:v0.21.0 | |

No Prometheus, client-go or YAML dependencies by design. Discovery is DNS-based, so it needs no RBAC, and config is JSON.

## Layout
```
cmd/spawner/main.go          modes: migrate | api | worker | all
internal/agent/              args.go (TaskArgs, roles, QueueName), worker.go, tree.go (StartTree, CancelTree), prompts.go
internal/backend/pool.go     endpoints, Acquire/Lease, DNS discovery, ejection, Status()
internal/limiter/aimd.go     adaptive limit
internal/llm/client.go       OpenAI-compatible SSE client; Stream(onDelta(kind,text)), JSON(v, onReasoning)
internal/events/events.go    NATS bus, DeltaWriter, Subscribe
internal/store/              agent_node SQL, Finish/Split/CancelTree, Migrate (River + embedded migrations/*.sql)
internal/api/server.go       HTTP + SSE (per-tree and global), settings, models, worker registry
internal/models/             runtime model registry (backends.json + agent_model), CRUD, PendingJobs
internal/discovery/          LM Studio / OpenAI-compatible model discovery, load/unload
internal/agent/follow.go     QueueFollower: River queues follow the model set
internal/settings/           versioned agent settings + per-tree snapshots (Defaults, Validate, Load, Save, SnapshotTree, ForTree)
internal/ui/                 embedded web console (static/index.html, style.css, app.js); Handler() mounted at "GET /"
internal/config/config.go    env + backends JSON
deploy/kustomize/{base,keda,overlays/andor}, deploy/examples/
experiments/chunking/        chunking vs single-context experiment (harness, results.json, RESULTS.md)
experiments/stress/          load/boundary harnesses (stress.py: concurrency|fanout|depth|burst; nest.py; ctx.py)
memory-bank/                 this documentation
```

## Configuration
- `backends.json` (path from `BACKENDS_FILE`, `${ENV}` expanded): `planner`, `default`, `models[]{name, model, endpoints[], api_key?, initial_limit, max_limit, extra{}}`. Model `name` must match `^[a-z0-9][a-z0-9_-]{0,55}$`, and the queue is `llm_<name>`.
- Env vars with defaults: `DATABASE_URL` (required), `NATS_URL=nats://localhost:4222`, `HTTP_ADDR=:8080`, `MAX_DEPTH=2`, `MAX_CHILDREN=6`, `MAX_WORKERS_PER_QUEUE=32`, `ACQUIRE_WAIT=1s`, `SNOOZE=250ms`, `DELTA_INTERVAL=50ms`, `JOB_TIMEOUT=10m`, `RIVER_FETCH_COOLDOWN=20ms`, `RIVER_FETCH_POLL_INTERVAL=250ms`, `RIVER_POLL_ONLY=false`, `DISCOVERY_INTERVAL=10s`, `SHUTDOWN_TIMEOUT=2m`, `EVENT_RETENTION=15m`, `EVENT_REPLICAS=1`, `EVENT_FILE_STORE=false`, `WORKER_QUEUES` (empty = all models), `LOG_LEVEL=info`.

## Dev setup
```sh
make deps              # docker compose: postgres:5432 (pw dev, db spawner), nats:4222, riverui:8081
make test              # unit tests (integration tests skip without env)
make test-integration  # sets TEST_DATABASE_URL / TEST_NATS_URL
make run               # migrate + `spawner all`
```
- The integration tests (`internal/agent/integration_test.go`) create a **throwaway database per test** (`isolatedDB`) and drop it afterwards. Never point them at, or clear, a database a live process uses. That happened once and wiped a live run.
- A fake OpenAI server in the tests decides behaviour from the prompt: "SPLIT" → plan splits; `failPrompt` → 400; a "Results from parallel workers" prompt → synthesis text; a system prompt containing "PIRATE" → output prefixed "arr: ".
- **UI verification method (2026-10-11):** Playwright 1.55 with Chromium headless shell, installed in the session scratchpad (`npm i playwright@1.55.0 && npx playwright install chromium`; Firefox is on the system but Playwright needs its own build). A throwaway `fakellm` Go server on :19090 streamed reasoning then content with 40–100 ms delays. Neither is committed to the repo yet. Fake servers must fail **before** writing stream headers, or the failure looks like a truncated stream.

## Local LM Studio (user's machine)
- LAN IPs: 192.168.1.76 and .71 (enp5s0). LM Studio listens on 0.0.0.0:1234, and cluster pods reach it at http://192.168.1.76:1234 (verified 2026-10-12).
- `http://localhost:1234`. On 2026-10-10 `unsloth/qwen3.8-27b` was loaded (8192 context). Also installed but not loaded: `qwen/qwen3.5-9b`, `google/gemma-4-12b`, `qwen3.6-35b-a3b`, `glm-4.6v-flash`, `prism-ml/bonsai-27b`.
- About 4.4 tok/s with one stream and about 3.8 tok/s each with two; heavy silent reasoning.
- The native REST API: `/api/v0/models` (load state) and the richer **`/api/v1/models`** (size, quant, `loaded_instances[].config.{context_length,parallel}`, `remaining_ttl_seconds`, `capabilities.reasoning.allowed_options`). Load: `POST /api/v1/models/load {model, context_length}`; unload: `POST /api/v1/models/unload {instance_id}`. **Reasoning off = `reasoning_effort: "none"`** on `/v1/chat/completions` (verified 2026-10-12).
- qwen3.5-9b Q8 loads in about 16 s, uses 4 parallel slots by default, and runs at about 40 tok/s per stream with 3–4 concurrent streams.
- For the cluster to reach it, turn on "Serve on Local Network" and raise max concurrent predictions.

## Tool gotchas seen in this environment
- `pkill -f "<pattern>"` kills the invoking shell (exit 144) if the pattern appears **anywhere** in that Bash command, including in a heredoc, a printf, or a build path such as `-o .../fakellm-bin`. The `[x]pattern` trick doesn't help when the text appears elsewhere. Use `pgrep -x <exe>` from a script file written with the Write tool.
- A graceful shutdown waits up to `SHUTDOWN_TIMEOUT` for in-flight generations. A process still alive after SIGTERM is usually draining, not hung.
