"""Load harness for the agent spawner: submits trees, follows their SSE
streams, and records per-node and per-tree metrics plus limiter samples."""
import json, sys, threading, time, urllib.request, statistics, datetime
from concurrent.futures import ThreadPoolExecutor

BASE = "http://192.168.1.64:30880"

def post(path, body):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)

def get(path):
    with urllib.request.urlopen(BASE + path, timeout=10) as r:
        return json.load(r)

def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

def run_tree(prompt, depth, children, timeout=1800):
    t0 = time.time()
    tid = post("/v1/trees", {"prompt": prompt, "max_depth": depth, "max_children": children, "priority": 1})["tree_id"]
    evs = []
    with urllib.request.urlopen(f"{BASE}/v1/trees/{tid}/events", timeout=timeout) as r:
        etype = None
        for raw in r:
            line = raw.decode().rstrip("\n")
            if line.startswith("event:"):
                etype = line[6:].strip()
            elif line.startswith("data:") and etype not in ("delta", "thinking"):
                e = json.loads(line[5:])
                evs.append(e)
                if e["type"] in ("tree.done", "tree.failed", "tree.cancelled"):
                    break
    t1 = time.time()
    created = next((ts(e["time"]) for e in evs if e["type"] == "tree.created"), None)
    nodes = [e for e in evs if e["type"] in ("node.done", "node.failed")]
    split = next((e for e in evs if e["type"] == "node.split"), None)
    final = evs[-1] if evs else {}
    return {
        "tree_id": tid,
        "status": final.get("type"),
        "wall_s": t1 - t0,
        "agents": len({e["node_id"] for e in evs if e["type"] == "node.started"}),
        "children": len(split["children"]) if split else 0,
        "done": sum(1 for e in nodes if e["type"] == "node.done"),
        "failed": [e.get("text", "")[:160] for e in nodes if e["type"] == "node.failed"],
        "final_error": final.get("text", "")[:200] if final.get("type") == "tree.failed" else "",
        "final_chars": len(final.get("text", "")) if final.get("type") == "tree.done" else 0,
        "stats": [dict(e["stats"], role=e.get("role"), title=e.get("title")) for e in nodes if e.get("stats")],
        "plan_s": (ts(split["time"]) - created) if split and created else None,
    }

class Sampler(threading.Thread):
    """Samples worker limiter state every second."""
    def __init__(self, model):
        super().__init__(daemon=True)
        self.model, self.samples, self.stop = model, [], threading.Event()
    def run(self):
        while not self.stop.is_set():
            try:
                d = get("/v1/models")
                inflight = limit = 0
                for w in d["workers"]:
                    for ep in (w["backends"] or {}).get(self.model, []):
                        inflight += ep["limiter"]["inflight"]; limit += ep["limiter"]["limit"]
                self.samples.append((time.time(), inflight, limit))
            except Exception:
                pass
            self.stop.wait(1)

def summarize(name, results, sampler, wall):
    gen = [s for r in results for s in r["stats"] if s["role"] == "exec"]
    tokens = sum(s["tokens"] for r in results for s in r["stats"])
    out = {
        "phase": name, "trees": len(results), "wall_s": round(wall, 1),
        "ok": sum(r["status"] == "tree.done" for r in results),
        "failed_trees": [r["final_error"] or r["status"] for r in results if r["status"] != "tree.done"],
        "failed_nodes": sum(len(r["failed"]) for r in results),
        "node_errors": sorted({e for r in results for e in r["failed"]})[:4],
        "agents": sum(r["agents"] for r in results),
        "tokens": tokens,
        "agg_tok_s": round(tokens / wall, 1) if wall else 0,
    }
    if gen:
        out.update({
            "exec_ttft_p50_ms": int(statistics.median(s["ttft_ms"] for s in gen)),
            "exec_ttft_max_ms": max(s["ttft_ms"] for s in gen),
            "exec_tok_s_p50": round(statistics.median(s["tokens_per_second"] for s in gen), 1),
        })
    lat = [r["wall_s"] for r in results]
    out.update({"tree_wall_p50_s": round(statistics.median(lat), 1), "tree_wall_max_s": round(max(lat), 1)})
    if sampler.samples:
        out["peak_inflight"] = max(s[1] for s in sampler.samples)
        out["limit_range"] = [round(min(s[2] for s in sampler.samples), 1), round(max(s[2] for s in sampler.samples), 1)]
        out["limit_end"] = round(sampler.samples[-1][2], 1)
    for r in results:
        if r["plan_s"] is not None:
            out.setdefault("plan_s", []).append(round(r["plan_s"], 1))
            out.setdefault("children", []).append(r["children"])
            red = [s for s in r["stats"] if s["role"] == "reduce"]
            if red:
                out.setdefault("reduce_s", []).append(round(red[0]["duration_ms"] / 1000, 1))
        if r["final_chars"]:
            out.setdefault("final_chars", []).append(r["final_chars"])
    return out

def phase(name, jobs, model="qwen9b-fast"):
    """jobs: list of (prompt, depth, children); all submitted at once."""
    s = Sampler(model); s.start()
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=max(1, len(jobs))) as ex:
        results = list(ex.map(lambda j: _safe(run_tree, *j), jobs))
    wall = time.time() - t0
    time.sleep(1.5); s.stop.set()
    summary = summarize(name, results, s, wall)
    print(json.dumps(summary), flush=True)
    with open(f"results_{name}.json", "w") as f:
        json.dump({"summary": summary, "results": results, "samples": s.samples}, f)
    return summary

def _safe(fn, *a):
    try:
        return fn(*a)
    except Exception as e:
        return {"tree_id": None, "status": f"harness error: {e}", "wall_s": 0, "agents": 0, "children": 0, "done": 0,
                "failed": [], "final_error": str(e)[:200], "final_chars": 0, "stats": [], "plan_s": None}

TOPICS = ["goroutines", "channels", "mutexes", "context cancellation", "select statements", "worker pools",
          "rate limiting", "backpressure", "sync.Once", "atomic counters", "errgroup", "semaphores",
          "fan-in", "fan-out", "pipelines", "deadlocks", "race detection", "timeouts", "heartbeats", "leases",
          "graceful shutdown", "retries", "circuit breakers", "bulkheads", "idempotency", "sharding",
          "consistent hashing", "leader election", "write-ahead logs", "snapshots", "gossip", "vector clocks",
          "CRDTs", "two-phase commit", "sagas", "outbox pattern", "queues", "pub/sub", "streams", "batching"]

def direct_prompt(i):
    return f"In about 150 words, explain {TOPICS[i % len(TOPICS)]} in Go to an intermediate developer. (run {i})"

if __name__ == "__main__":
    which = sys.argv[1]
    if which == "concurrency":
        for n in [int(x) for x in sys.argv[2].split(",")]:
            phase(f"conc{n}", [(direct_prompt(i + n * 100), 0, 2) for i in range(n)])
            time.sleep(3)
    elif which == "fanout":
        for k in [int(x) for x in sys.argv[2].split(",")]:
            items = ", ".join(TOPICS[:k])
            prompt = (f"Split this into exactly {k} parallel subtasks, one per topic (this is a parallelism test, so split even if small). "
                      f"For each topic write a 120-word explanation for Go developers. Topics: {items}.")
            phase(f"fanout{k}", [(prompt, 1, k)])
            time.sleep(3)
    elif which == "depth":
        for d in [int(x) for x in sys.argv[2].split(",")]:
            prompt = ("Write a concise handbook on building reliable distributed services in Go with four major parts: "
                      "(1) concurrency primitives, (2) failure handling, (3) data consistency, (4) operations and deployment. "
                      "Each part is large and has several independent sections that should each be written separately.")
            phase(f"depth{d}", [(prompt, d, 4)])
            time.sleep(3)
    elif which == "burst":
        n = int(sys.argv[2])
        phase(f"burst{n}", [(direct_prompt(i + 5000), 0, 2) for i in range(n)])
