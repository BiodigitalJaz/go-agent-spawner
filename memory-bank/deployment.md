# Deployment Procedures

## LIVE deployment (since 2026-10-12): cluster `andor.empire.byrd.guru`
- kubectl context `kubernetes-admin@andor.empire.byrd.guru`: 3 nodes (cpl01 192.168.1.63, wn01 .64, wn02 .65), k8s v1.33, containerd. **No** ingress controller, KEDA, in-cluster registry or SSH to nodes. Storage classes: `local` (openebs, default) and `plex-store` (nfs).
- Namespace **`agent-spawner`**, deployed with `kubectl apply -k deploy/kustomize/overlays/andor`.
- **Console: NodePort 30880 on any node**, e.g. http://192.168.1.64:30880. River UI is ClusterIP only (`kubectl -n agent-spawner port-forward svc/riverui 8081:80`).
- Image: **`ghcr.io/biodigitaljaz/agent-spawner:0.1.9`** (live since 2026-10-12. 0.1.9: token auth + console sign-in; 0.1.8: estimates never reject; 0.1.7: shared-context token budget + stream error precedence; 0.1.6: limiter ignores latency when slots are set; 0.1.5: code merge modes; 0.1.4: in-stream error codes; 0.1.3: context/slots, merge guard, document mode; 0.1.2: model discovery; 0.1.1: cancel fix; 0.1.0: first deploy), a public ghcr package. New tags of an already-public package are public automatically. Docker on the dev box is logged into ghcr as `BiodigitalJaz`. New ghcr packages start **private**, and visibility can only be changed in the GitHub UI: https://github.com/users/BiodigitalJaz/packages/container/agent-spawner/settings → Danger Zone. The API returns 404.
- Database: dedicated role and database **`spawner`** on the shared Bitnami Postgres 17.5 at `postgresql.default.svc.cluster.local:5432` (StatefulSet `default/postgresql-0`; the admin password is read from `$POSTGRES_PASSWORD_FILE` inside the pod). The app connection URL is only in Secret `agent-spawner/spawner-db` (key `url`). Read it with `kubectl -n agent-spawner get secret spawner-db -o jsonpath='{.data.url}' | base64 -d`.
- Models added in the console live in the `agent_model` table (they survive redeploys). Current: `qwen9b`, `qwen9b-fast` → `qwen/qwen3.5-9b`. Settings v1 assigns all roles to `qwen9b-fast`.
- LM Studio qwen3.5-9b is now loaded with **parallel 8, context 32000** (user-set 2026-10-12; 12.2 GB VRAM). Matches the pool: qwen9b-fast `max_limit` 4 × 2 workers = 8.
- GPU: RTX 4080 SUPER 16 GB. The 27B IQ3_S (13 GB) and the 9B Q8 (10.4 GB) cannot be resident together; swap them with Load/Unload on the Models page.
- Backend: LM Studio on the dev box at **http://192.168.1.76:1234** (it also has .71 on the same NIC, and .76 may be DHCP: if it changes, edit `overlays/andor/backends.json` and re-apply). Model `unsloth/qwen3.8-27b` is used for all roles.
- Replicas: api 2, worker 2, NATS 1 (memory JetStream, capped at 1 GB via a config file), riverui 1.
- Verified 2026-10-12: the UI returns 200 on all 3 nodes; both workers send heartbeats; a live run (depth 0) from a cluster pod to LM Studio finished in 19.7 s with 2.0 s TTFT; Playwright on the NodePort showed no errors.

### Release procedure
```sh
docker build -t ghcr.io/biodigitaljaz/agent-spawner:<ver> . && docker push ghcr.io/biodigitaljaz/agent-spawner:<ver>
# bump newTag in deploy/kustomize/overlays/andor/kustomization.yaml (and base)
kubectl apply -k deploy/kustomize/overlays/andor
```
### Shared Postgres memory (2026-10-13)
`default/postgresql-0` (Helm, bitnami postgresql-16.7.4) was OOMKilled at its 192Mi limit (shared_buffers 128MB). The user patched the StatefulSet to limits.memory 512Mi / requests 256Mi. Not in Helm values: a `helm upgrade` reverts it unless `primary.resources` is set. Changing this shared DB needs the user's go-ahead (the permission classifier blocks it).

### MCP
Live image 0.1.13. HTTP: `http://192.168.1.64:30880/mcp` with `Authorization: Bearer <token>`. stdio: `SPAWNER_URL=http://192.168.1.64:30880 spawner mcp` (reads ~/.config/agent-spawner/token).

### Zero-downtime rollouts
api and worker have `lifecycle.preStop.sleep.seconds: 5` (the endpoint leaves Service routing before the process stops listening). Verified 331/331 OK during `rollout restart`. When probing right after a deploy, wait for `/readyz` regardless.

### Lessons from the first deploy
- `nats --max_mem_store` is not a CLI flag (NATS printed its usage and crash-looped). Memory caps must go in a config file.
- River UI image tags have **no `v` prefix** (`ghcr.io/riverqueue/riverui:0.21.0`).
- A StatefulSet won't roll over a pod that has never been Ready: delete the pod. Deployments stuck in crash-loop backoff: delete their pods instead of waiting up to 5 minutes.
- The app exits fatally if NATS is unreachable at startup (it then recovers by restarting). Bring NATS up first.

## Kubernetes topology (`deploy/kustomize/base`, namespace `agents`)
| Resource | Purpose |
|---|---|
| `spawner-api` Deployment (2) + Service + PDB | Init container runs `spawner migrate` (advisory-locked, safe to run concurrently), then `spawner api`: HTTP/SSE, River insert-only client |
| `spawner-worker` Deployment (2) | `spawner worker`: River queues `llm_<model>`, backend pool, DNS discovery. `terminationGracePeriodSeconds: 660` |
| `nats` StatefulSet (1) | JetStream in memory, `--max_mem_store 1GB`. For HA, use the official Helm chart with 3 replicas + `EVENT_REPLICAS=3` |
| `riverui` Deployment | Internal job inspector (`kubectl -n agents port-forward svc/riverui 8081:80`) |
| ConfigMaps `spawner-backends` (from `backends.json`) and `spawner-env` | Generated by kustomize |
| Secret `spawner-db` (key `url`) | **Created manually**; not in the repo |

Optional overlay `deploy/kustomize/keda`: a KEDA `postgresql` trigger that scales `spawner-worker` on available `llm_%` jobs (target 20, 1–6 replicas).

Examples in `deploy/examples/`: an in-cluster vLLM backend behind a **headless** Service (`dns+http://llm-large.agents.svc.cluster.local:8000`, with startup and readiness probes on `/v1/models`), and an ingress-nginx config for SSE (buffering off, 3600 s timeouts).

## Procedure
```sh
make image IMAGE=ghcr.io/<org>/agent-spawner:<tag> && docker push ...
# set images.newName/newTag in deploy/kustomize/base/kustomization.yaml
kubectl create namespace agents
kubectl -n agents create secret generic spawner-db --from-literal=url='postgres://USER:PASS@HOST:5432/DB'
# edit deploy/kustomize/base/backends.json (endpoints reachable from pods)
kubectl apply -k deploy/kustomize/base        # or deploy/kustomize/keda
kubectl -n agents port-forward svc/spawner-worker 8080:8080 && curl localhost:8080/debug/backends
```

## Checks and gotchas
- **PgBouncer in transaction mode breaks LISTEN** → set `RIVER_POLL_ONLY=true`, or point `DATABASE_URL` at Postgres directly (e.g. the CloudNativePG `-rw` Service).
- The worker's grace period (660 s) must exceed `SHUTDOWN_TIMEOUT` (set to 10m in the configmap) plus about 10 s for the hard stop.
- LM Studio outside the cluster: it must listen on the LAN, and pods must have a route to it. Its max concurrent predictions caps what the limiter can find.
- Backends discovered by DNS must use a *headless* Service. A ClusterIP Service collapses to one endpoint and defeats the per-pod limiting.
- **Auth (since 0.1.9):** `/v1/*` and `/debug/*` need a token. Secret `agent-spawner/spawner-auth` (key `tokens`, comma-separated). The user's token is also saved at `~/.config/agent-spawner/token` (0600) on the dev box; never print it. Scripts: `SPAWNER_TOKEN="$(tr -d '\n' < ~/.config/agent-spawner/token)"` for the experiment harnesses; Playwright suites log in via `authpage.mjs`. River UI (ClusterIP, port-forward only) has no auth.
- The console is served by `spawner-api` at `/`. The ingress example already routes `/` there and disables buffering, which the SSE streams need.
- The generic base (`deploy/kustomize/base`, namespace `agents`) is still the template. The andor overlay is what's live.
