# go-agent-spawner

A pool of AI agents on goroutines that splits work dynamically, backed by
[River](https://github.com/riverqueue/river) (Postgres job queue) for
durability and distribution, and NATS JetStream for live token streaming.
Inference backends are any OpenAI-compatible server: LM Studio, vLLM or
llama.cpp.

## How it works

```
POST /v1/trees ──► root node + plan job (one tx)
                          │
            River queue llm_<model> (Postgres, priority 1-4)
                          │  fetched by any worker replica
                          ▼
   Worker.Work ── Acquire GPU slot (per-endpoint AIMD limiter)
      │              └─ saturated? JobSnooze → another replica picks it up
      ├─ plan:   planner model returns JSON {direct | split[subtasks]}
      │            split → children + their jobs + JobCompleteTx (one tx)
      │            direct → exec inline (same model) or hand off
      ├─ exec:   stream answer ─► NATS (delta/thinking events)
      │            finish node; last sibling enqueues parent reduce (one tx)
      └─ reduce: synthesize children's outputs ─► finish node ─► … ─► tree.done

GET /v1/trees/{id}/events ◄── SSE from NATS (replay + Last-Event-ID resume)
```

- **Control plane: River.** Task lifecycle, retries, priorities, distribution
  across replicas, and atomic split/join. Every state change commits in the
  same transaction as job completion, so a crash never orphans children or
  loses a reduce.
- **Data plane: goroutines and the backend pool.** Each backend endpoint has an
  adaptive concurrency limit (AIMD on time-to-first-token). The limiter finds
  how many parallel requests a GPU can take before requests start queueing
  inside the server. Independent replicas converge on a fair share without
  coordinating.
- **Feedback: NATS JetStream.** Token deltas are batched at 50 ms (the first
  one is sent immediately) and never touch Postgres. A short replay window lets
  SSE clients reconnect without gaps.

## Web console

### Chat

The **Chat** tab is a conversational agent. Each reply is a run whose root agent sees the conversation so far (as chat turns), so chat uses the same pool, models and live streaming as runs.
- **Links** in your message are always read; a link that can't be opened is reported in the reply instead of failing the message.
- **Search** (shown when a search backend is configured): **Auto** (the default) searches when the answer depends on current or local facts such as regulations, seasons, licenses, prices or schedules: questions that mention those (or a US state, a year, "near me") are always searched, and the model decides the rest, searching when unsure. **On** always searches; **Off** never does. Auto keeps general questions (how-tos, explanations, code) off the search quota.
- **Grounded answers:** when a page found by search says "See Big Game Regulations" and links it, that page is read too (up to 2 such pages). An answer written from search results gets a **source check**: fees, dates, limits and legal claims the pages don't support are removed or marked unverified, and "not mentioned" is never treated as "not allowed". General know-how (steps, recipes, tips) is left alone.
- **Each reply** says whether it searched the web; the last one has **Search and answer again**, which rewrites it in place with search on.
- **Agent pool** lets the planner split the reply across parallel agents; one agent answers by default, which is fastest. The Search and Agent pool choices are remembered in the browser.
- **Each reply** links to its run ("View run") for the agents, timeline and timings.
- **Conversations** are stored in Postgres and belong to the account (or token) that created them. Long conversations keep the newest turns that fit about 30% of the worker model's context; older ones are left out and the reply says so. Deleting a conversation also purges its runs.

The API serves a web console at `/`. It's embedded in the binary and needs no build step or CDN, so it works in air-gapped clusters.

- **Runs:** submit prompts, with per-run overrides for the worker model, split depth, fan-out and priority. Watch every run live:
  - a timeline of each agent's phases (queued, planning, thinking, working, waiting on children, merging) with a "parallel agents" chart underneath
  - a card for each spawned agent, with streaming output, collapsible reasoning, TTFT, tok/s, tokens and endpoint
  - the merged result
  - a sidebar that updates live, with per-status progress bars for every run
- **Agents & directives:** choose the model, temperature and max tokens for each role (planner, worker, reducer), edit each role's directive (system prompt), and set split depth and fan-out. Settings are versioned, so concurrent edits are detected. Each run snapshots the settings when it starts, so edits never change a run in progress.
- **Models & capacity:** every model in the pool, with each worker's live limiter state per endpoint (slots in use, learned limit, baseline TTFT, ejected). Below that, **Discover** lists every model your backends offer. You can load or unload models on LM Studio hosts and **add any of them to the pool** with a routing name, concurrency limits and a reasoning setting ("off" sends `reasoning_effort: "none"`). Workers start serving new models within seconds, with no restart. Models from `backends.json` are read-only; console-added ones can be edited or removed.

### Authentication

Everything under `/v1`, `/debug` and `/mcp` needs authentication once either accounts or tokens exist. Health checks and the console's static shell stay public.

**Accounts (people).** Sign in to the console with an email and password.
- Passwords are stored as **argon2id** hashes (19 MiB, 2 passes, 1 lane, 16-byte salt), never in plain text, and must be at least 12 characters.
- Manage accounts with the binary; the password is read from stdin, so it never lands in shell history or process lists:
  ```sh
  kubectl -n agent-spawner exec -i deploy/spawner-api -c api -- /spawner user set someone@example.com < password-file
  kubectl -n agent-spawner exec deploy/spawner-api -c api -- /spawner user list
  kubectl -n agent-spawner exec deploy/spawner-api -c api -- /spawner user disable someone@example.com
  kubectl -n agent-spawner exec deploy/spawner-api -c api -- /spawner user delete someone@example.com
  ```
  `delete` removes the account (its sessions end within 15 seconds). It refuses to delete the last active account when no `AUTH_TOKENS` are set, since that would leave the API without authentication.
  `set` creates the account or resets its password.
- **Change password:** click your email in the console's top bar. Changing a password, resetting it with `user set`, or disabling the account signs out every session of that account. Other API replicas notice within 15 seconds.
- **Brute force:** each failed sign-in waits 0.5 s, and an email is locked for 15 minutes after 5 failures. Failures, logins and password changes are logged.

**Tokens (scripts, agents, administrators).** Set `AUTH_TOKENS` (comma-separated; in Kubernetes, the `spawner-auth` Secret).
- Send `Authorization: Bearer <token>` on every request, including SSE streams and `/mcp`.
- The console's sign-in screen also accepts a token ("Use an access token instead").
- **Revoking:** remove a token from the Secret and restart the pods. Every session created with that token stops working.

**Sessions.** The console keeps a signed session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` behind HTTPS), valid for `AUTH_SESSION_TTL` (default 7 days).
- Account sessions are signed with a key generated once and stored in Postgres, so every API replica accepts them.
- Cookie-authenticated writes from another origin are refused.

Without accounts or `AUTH_TOKENS`, the API is open and logs a warning at startup.

### Merge strategies

| Mode | How | Use for |
|---|---|---|
| `llm` (default) | The reducer model synthesizes one answer | Prose, anything that needs reconciliation across parts |
| `list` | Code: every distinct line once (ignoring case and spacing), in part order | Extraction and other list-shaped answers |
| `concat` | Code: each part's output verbatim, in order | Independent sections; code (never drops a line) |

Set a default in *Agents & directives*, or per run in the run form or with `merge` on `POST /v1/trees`. See [experiments/chunking/RESULTS.md](experiments/chunking/RESULTS.md) for measured trade-offs.

### Merge size guard

Before a reducer merges its children, it estimates the input size from each child's recorded completion tokens (or characters ÷ 3) against the reducer model's `context_length`. Inputs that don't fit are first **consolidated in parallel batches** (a lossless condensation step), repeating up to 4 passes, then merged. A single result too large on its own is truncated with a visible `[… N characters omitted …]` marker instead of being silently cut by the server. The reducer's stats report `merge_batches`, `merge_passes` and `truncated`, and the console shows them on the merge card.

## Run locally

```sh
cp backends.example.json backends.local.json   # point at your LM Studio
make deps                                       # postgres and nats
DATABASE_URL=postgres://postgres:dev@localhost:5432/spawner?sslmode=disable \
BACKENDS_FILE=backends.local.json go run ./cmd/spawner migrate
DATABASE_URL=postgres://postgres:dev@localhost:5432/spawner?sslmode=disable \
BACKENDS_FILE=backends.local.json go run ./cmd/spawner all

curl -s -X POST localhost:8080/v1/trees -d '{"prompt":"..."}'
curl -N localhost:8080/v1/trees/<tree_id>/events
curl -s localhost:8080/debug/backends     # live limiter state per endpoint
```

In LM Studio, enable **Serve on Local Network** for access from the cluster,
and raise the max concurrent predictions setting. The limiter only finds
parallelism the server allows.

## API

| Method | Path | |
|---|---|---|
| POST | `/v1/trees` | `{"prompt", "priority"? (1-4), "model"?, "max_depth"?, "max_children"?, "document"?, "chunks"?}` → `{tree_id, events_url}`. The optional fields override the saved settings for this run only. **Document mode:** `prompt` is the instruction and `document` is split *in code* into `chunks` parts (paragraph boundaries), each handled by its own agent and merged by the reducer, with no planner. `chunks` 0 or omitted chooses automatically (one agent up to about 6k tokens, since a split adds a merge that roughly doubles the text generated; 4 chunks up to about 30k; then one per ~7.5k tokens, at most 16); `chunks: 1` puts the whole document into one agent's context. **`merge`** overrides how split results are combined: `llm` (default; the reducer model writes the answer), `list` (in code: union of lines, duplicates dropped; for list-shaped answers) or `concat` (in code: outputs joined verbatim). Code merges take no GPU slot and add no model latency; failed parts appear as `[part N failed: …]`. **`parts`** (`[{"title", "text"}]`, up to 64) gives the split yourself, one agent per part, e.g. one per file. Parts over **`max_part_tokens`** (default 16000; -1 disables) are split at line boundaries into sections, one agent each, so no part comes near the model's context; jobs start largest first. **`part_max_words`** asks each part's agent for an answer under that many words while the merge keeps the full prompt, and admission reserves room for about that many words per part instead of a full `max_tokens`, so more parts run at once: per-part work is output-bound, so this is the main speed lever for summaries (a 29-file run went from about 230 s to about 110 s at 120 words). Avoid it for extraction, where it can drop items **Grounding:** agents cannot browse. **`context`** is reference material every agent receives (planner, workers and merge), up to 24,000 characters, e.g. a page under review or rules the answer must respect. **`fetch_urls`** downloads the web pages linked in the prompt (up to 5) and adds their text to the context. A prompt that links to pages without `fetch_urls` or content is refused (400). Fetching never reaches private, loopback, link-local or cluster addresses unless `allow_internal_fetch` is on in the settings (403 otherwise); every attempt on such an address is logged. The reference may take up to 40% of the worker model's context, and each agent's own input is sized to fit alongside it: parts get smaller sections and documents more chunks when the reference is large (the response's `sizing` says when). **`check`** (default: the `check_answer` setting, on) runs a rules check when the request supplies its own `context` (fetched pages and search results alone don't trigger it): the model-written final answer is reviewed against it, conflicting recommendations are removed or fixed, and a "Rules check" section lists the changes. **`search`** runs a web search before planning. Backends (results are merged; one failing, for example an exhausted free quota, does not stop the others): **SearXNG** via `SEARCH_URL` (JSON format enabled; `SEARCH_CATEGORIES` overrides the categories) and **[Exa](https://exa.ai)** via `EXA_API_KEY` (official API with its own index; `EXA_SEARCH_TYPE`, default `instant`, about $0.004 per query). **Search is never paid for:** Exa is capped at `EXA_MONTHLY_BUDGET_USD` (default $9.50, under the $10 monthly free credit), tracked per calendar month in Postgres across all workers; at the cap Exa stops and search continues on SearXNG until the 1st. Only the cheapest search types (`instant`, `fast`, `auto`) are allowed. Settings shows this month's spend. Exa returns page text, so its results need no fetch; every Exa query logs its cost, and identical queries are cached for a day. In Kubernetes the key comes from the optional `spawner-search` Secret (`kubectl -n agent-spawner create secret generic spawner-search --from-literal=exa_api_key=...`). The search: the planner model writes up to 3 queries, the top results are fetched (through the same internal-address guard) and added to the reference with their addresses, so every agent can cite them |
| GET | `/v1/trees?limit=` | Recent trees with node counts by status. `prompt` is a preview (first 400 characters); `prompt_chars` is its full length |
| GET | `/v1/trees/{id}` | Tree prompt, settings snapshot, and every node with outputs, timeline timestamps and stats |
| GET | `/v1/trees/{id}/events` | SSE stream: `node.started`, `thinking`, `delta`, `node.split`, `node.done`, `node.failed`, `tree.done`/`tree.failed`/`tree.cancelled`. An event carries at most 64 KiB of text (NATS's message limit is 1 MB): longer streamed text arrives as several `delta` events, and a longer final result arrives cut in `tree.done` with `text_truncated: true`; the full result is in `GET /v1/trees/{id}` |
| DELETE | `/v1/trees/{id}` | Cancel the tree; running generations stop. With `?purge=1`, delete every record of a finished tree instead (nodes, prompts, outputs, reference material, River jobs and live events); 409 while it is still active |
| GET | `/v1/events` | SSE lifecycle events for every tree (no tokens), including `tree.created` |
| GET/PUT | `/v1/settings` | Role models, temperature, max tokens, directives, depth and fan-out. PUT takes `{"version", "settings"}`; returns 409 if the version is stale and 422 if validation fails |
| GET | `/v1/models` | Routable models (backends.json and console-added, with `source`; API keys omitted) and live worker heartbeats |
| POST / PUT / DELETE | `/v1/models[/{name}]` | Add, edit or remove console-managed models. Workers pick changes up within seconds. Delete is refused while the model is in use (by settings, a running tree, or queued jobs) |
| GET / POST | `/v1/chats` | List your conversations / create one (`{"title"?}`) |
| GET / PATCH / DELETE | `/v1/chats/{id}` | A conversation with its messages (a reply's text and status come from its run) / rename (`{"title"}`) / delete it and purge its runs |
| POST | `/v1/chats/{id}/retry` | `{"search_mode"?, "deep"?, "model"?}` → like `messages`: writes the last reply again (in place; its old run is purged) |
| POST | `/v1/chats/{id}/messages` | `{"content", "search_mode"? ("auto" default, "on", "off"), "fetch_urls"? (default true), "deep"?, "model"?}` (`"search": true` still means "on") → `{tree_id, events_url, user_message_id, assistant_message_id, history_note?, fetched?}`. Starts the reply's run with the conversation so far; stream it from `events_url`. 409 while the previous reply is still being written |
| GET | `/v1/discovery[?url=]` | Models offered by each backend host (LM Studio: size, quantization, load state, parallel slots, reasoning options) |
| POST | `/v1/discovery/load`, `/v1/discovery/unload` | LM Studio only: `{url, model, context_length?}` / `{url, instance_id}` |

## MCP server

The spawner is also an [MCP](https://modelcontextprotocol.io) server, so a
coding agent (Claude Code, Cursor, any MCP client) can hand work to the local
models. Tools:

| Tool | |
|---|---|
| `run_task` | Run a prompt, optionally over a `document` (chunked automatically: one context up to ~6k tokens, otherwise 4+ parts) or your own `parts`. Waits for the result and reports progress (agents finished / planned); `wait: false` returns the task id at once |
| `analyze_files` | stdio only. Reads local files, directories or globs (skipping hidden, vendor and binary files) and runs the instruction on each file in its own agent; results are concatenated per file by default |
| `get_task` | Status and result of a task, optionally with each part's output |
| `cancel_task` | Cancel a task |
| `list_models` | Models, role assignments and online workers |

**stdio** (runs on your machine, talks to the API, can read local files):

```sh
go install github.com/BiodigitalJaz/go-agent-spawner/cmd/spawner@latest
claude mcp add spawner -e SPAWNER_URL=http://192.168.1.64:30880 -- spawner mcp
```

The token comes from `SPAWNER_TOKEN`, the file named by `SPAWNER_TOKEN_FILE`,
or `~/.config/agent-spawner/token`. `SPAWNER_CONSOLE_URL` overrides the
console link in results.

**Streamable HTTP** at `/mcp` on the API (stateless, so any replica serves
any request). Tools act with the caller's own bearer token; `analyze_files`
is not offered because the files would be the server's:

```sh
claude mcp add --transport http spawner http://192.168.1.64:30880/mcp \
  --header "Authorization: Bearer $(cat ~/.config/agent-spawner/token)"
```

## Configuration

What the agents do (role models, directives, temperature, depth, fan-out) is configured in the console or through `/v1/settings`. Settings are stored in Postgres; until someone saves, defaults come from `planner`/`default` and `MAX_DEPTH`/`MAX_CHILDREN`.

`backends.json` defines the models the pool can use:

| Field | Meaning |
|---|---|
| `planner` / `default` | Default planner and worker models, used until settings are saved |
| `models[].name` | Routing key; River queue is `llm_<name>` |
| `models[].model` | Model id sent to the API |
| `models[].endpoints` | `http://host:port` (static) or `dns+http://headless-svc:port` (one endpoint per ready pod, re-resolved every `DISCOVERY_INTERVAL`) |
| `models[].initial_limit` / `max_limit` | AIMD bounds per endpoint |
| `models[].extra` | Merged into each request body, e.g. `{"reasoning_effort":"none"}` (LM Studio: turns reasoning off) |
| `models[].context_length` | Context the server loaded the model with. Merges are sized to fit it; 0 = unknown (assume 8k) |
| `models[].shared_context` | The context is **one pool shared by all concurrent requests** (LM Studio, llama.cpp unified KV), not a per-request limit. Workers estimate each request's tokens (prompt chars ÷ 4 + reserved output) and admit requests only while the total across all workers fits 90% of `context_length`. An estimate over budget waits for an idle pool (the server makes the final call). If the pool still overflows, the server's transient "Context size has been exceeded" is retried shortly without using up an attempt |
| `models[].slots` | Server parallel slots shared by **all** worker replicas; workers exchange in-flight counts every 500 ms and never exceed it. With slots set, the limiter stops treating rising TTFT as congestion (it's batching load within known capacity) and only backs off on errors. 0 = no shared cap (latency-based AIMD) |

The values are expanded with `${ENV}`, so API keys can come from Secrets.

Environment: `DATABASE_URL` (required), `NATS_URL`, `HTTP_ADDR`, `MAX_DEPTH`
(split nesting, 0 disables planning), `MAX_CHILDREN`, `MAX_WORKERS_PER_QUEUE`,
`ACQUIRE_WAIT`, `SNOOZE`, `JOB_TIMEOUT`, `WORKER_QUEUES` (serve a subset of
models), `RIVER_POLL_ONLY` (needed behind PgBouncer transaction pooling),
`RIVER_FETCH_COOLDOWN`, `RIVER_FETCH_POLL_INTERVAL`, `EVENT_RETENTION`,
`EVENT_REPLICAS`, `SHUTDOWN_TIMEOUT`, `AUTH_TOKENS`, `AUTH_SESSION_TTL`, `PUBLIC_URL` (console address used in MCP
results). See
[internal/config/config.go](internal/config/config.go).

## Deploy

```sh
kubectl -n agents create secret generic spawner-db --from-literal=url='postgres://...'
kubectl -n agents create secret generic spawner-auth --from-literal=tokens="$(openssl rand -hex 32)"
kubectl apply -k deploy/kustomize/base      # or deploy/kustomize/keda with KEDA installed
```

- `spawner-api` runs migrations in an init container, serves the API and SSE,
  and only inserts jobs.
- `spawner-worker` runs the River workers. On SIGTERM it stops fetching and
  lets in-flight generations finish. Anything still running after
  `SHUTDOWN_TIMEOUT` is returned to the queue without using up a retry.
- [deploy/examples](deploy/examples) has an in-cluster vLLM backend behind a
  headless Service, and ingress-nginx settings for SSE.

## Tests

```sh
make test               # unit tests
make test-integration   # end-to-end against real Postgres and NATS (docker compose)
```
