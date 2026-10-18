# Project Brief: go-agent-spawner

## What it is
A Go service that runs a pool of AI agents on goroutines. It splits work dynamically across the pool and calls local LLMs through the LM Studio OpenAI-compatible API, with other OpenAI-compatible servers such as vLLM as drop-in alternatives. It is deployed to the user's Kubernetes cluster.

## Core goals (from the user)
1. **Get output back to the client as fast as possible.** Stream progress live, and keep the time between dispatching work and seeing results short.
2. **Split work dynamically** across the pool for efficiency. A planner decides at runtime whether a task is answered directly or split into parallel subtasks.
3. **Use goroutines for concurrency** in-process, and extend it across replicas with [River](https://github.com/riverqueue/river) (Postgres-backed job queue).
4. **Run in Kubernetes** next to the cluster's existing Postgres.

## Hard requirements and decisions (user-approved)
- **Open-source River only**, not River Pro. No Pro workflows and no global concurrency limits.
- Postgres already runs in the cluster; reuse it.
- Token and event streaming goes over **NATS JetStream**. The user delegated this choice; see systemPatterns.md for the reasoning.
- The LLM client uses only the portable OpenAI-compatible subset, so backends can be swapped (LM Studio → vLLM) without code changes.

## Scope
In scope: task trees (plan → split → exec → reduce), adaptive GPU concurrency, an HTTP/SSE API, cancellation, durability across crashes and restarts, Kubernetes manifests, and a **web console** (live run visualization, editing role models and directives, a capacity view; added 2026-10-11).
Out of scope so far: auth on the API and UI, multi-tenant isolation, tool-calling agents, and managing backend endpoints from the UI.

## Repo
- Path: `/mnt/dev/go-agent-spawner`. A second working directory, `/home/aaron/Dev/go-agent-spawner`, also exists and was empty when checked.
- Module: `github.com/BiodigitalJaz/go-agent-spawner` (renamed from an early `thinkbotlabs` guess on 2026-10-12).
- **Git:** public GitHub repo https://github.com/BiodigitalJaz/go-agent-spawner, branch `main`, remote over SSH (`git@github.com:BiodigitalJaz/go-agent-spawner.git`). First commit 2026-10-12. Commit or push only when the user asks; commits end with the Co-Authored-By attribution line.
- The repo is PUBLIC: never commit secrets (the DB URL lives only in the cluster Secret `agent-spawner/spawner-db`). LAN IPs and the cluster hostname are in the overlay and memory bank by the user's choice.

## Constraints set by the user
- Web search must always stay on free tiers (Exa free credit, self-hosted SearXNG). It will never be a purchased service: keep the spend cap, never propose paid plans or paid search types.
