# Product Context

## Why it exists
Local LLMs on a few GPUs are slow and a scarce resource. Sending one big prompt to one model wastes the parallelism the server can offer: LM Studio and vLLM batch concurrent requests. The user also gets no feedback until the whole answer is done. This project:
- breaks large tasks into independent subtasks and runs them in parallel on available GPU slots,
- finds out on its own how much parallelism each backend can take,
- streams every agent's progress to the client live,
- survives pod restarts and crashes without losing or duplicating work.

## Problems it solves
| Problem | Solution |
|---|---|
| GPU saturation is invisible; requests queue inside the inference server | AIMD limiter per endpoint, using time-to-first-token (TTFT) as the congestion signal |
| kube-proxy balances blindly across GPU pods | Headless Service plus DNS discovery; the spawner picks the endpoint with the most headroom |
| Multiple spawner replicas overload the same GPUs | Independent AIMD per replica converges on a fair share (like TCP), and saturated jobs snooze back to the queue |
| Orphaned subtasks or lost joins after a crash | Split, finish and join commit in the same Postgres transaction as River job completion |
| Slow feedback | SSE stream of `thinking`/`delta`/lifecycle events over NATS; first token flushed immediately |
| Reasoning models are silent for 20–50 s | Reasoning tokens are forwarded as `thinking` events |

## How it should work (user flow)
1. Client `POST /v1/trees {"prompt": "...", "model"?, "priority"?}` → `{tree_id, events_url}`.
2. Client opens `GET /v1/trees/{id}/events` (SSE) and watches the tree: `node.started` → `thinking` → `node.split` (children listed) → children stream in parallel → `node.done` with stats → the reduce step streams → `tree.done` with the final text.
3. `GET /v1/trees/{id}` returns every node with its outputs, and `DELETE` cancels the tree, stopping running generations.
4. **Web console** (`/`): the Runs page shows the live timeline, agent cards with streaming output and reasoning, and the result. The Agents & directives page edits role models, params and directives. The Models & capacity page shows live slot gauges per worker and endpoint.
5. Operators can also use `/debug/backends` per worker, and River UI for the jobs.

## UX goals
- Show the first sign of life in under a second, even when the model is thinking.
- Never hang a client: terminal trees whose events have aged out get a snapshot event.
- A failed subtask shouldn't kill the tree. The reduce step merges partial results and notes the gaps.
- Make parallelism visible: the user wants to *see* agents being spawned and running concurrently (timeline plus "parallel agents" chart).
- Configuration changes are safe: versioned, conflicts detected, and never applied to in-flight runs.
