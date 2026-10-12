"""Experiment: is chunking a large context across parallel agents more
performant than one agent reading the whole context?

Synthetic documents contain N planted facts ("needles": service, port,
owning team) at evenly spread positions in technical filler. Each condition
asks for the full list; answers are scored against ground truth.

Conditions per document size: single (whole doc in one context), chunks=4,
chunks=8. Runs execute one at a time so latency is not confounded by load.
"""
import json, os, random, re, sys, time, urllib.request, datetime

BASE = os.environ.get("SPAWNER_URL", "http://192.168.1.64:30880")
# The API requires a bearer token when auth is enabled (AUTH_TOKENS).
TOKEN = os.environ.get("SPAWNER_TOKEN", "")
AUTH = {"Authorization": f"Bearer {TOKEN}"} if TOKEN else {}
NEEDLES = 30

INSTRUCTION = ("Extract every service mentioned in the document together with its port and owning team. "
               "Output one line per service in exactly this format: service | port | team. "
               "Output only those lines, no headings or commentary. If no services are mentioned, output nothing.")

ADJ = ["amber", "basalt", "cobalt", "delta", "ember", "fjord", "garnet", "harbor", "indigo", "juniper", "krypton",
       "lumen", "marble", "nimbus", "onyx", "prairie", "quartz", "raven", "sierra", "tundra", "umber", "vertex",
       "willow", "xenon", "yarrow", "zephyr", "aspen", "birch", "cedar", "dune", "elm", "flint", "glacier", "hazel"]
NOUN = ["ledger", "gateway", "indexer", "router", "billing", "notifier", "scheduler", "catalog", "auth", "search",
        "metrics", "uploader", "renderer", "checkout", "inventory", "profile", "payments", "tracker", "mailer", "audit"]
TEAMS = ["kestrel", "osprey", "heron", "falcon", "lynx", "otter", "badger", "marten", "puffin", "ibis", "condor",
         "jackal", "viper", "walrus", "bison", "gecko", "manta", "orca", "tapir", "egret"]

# Filler: plausible engineering prose with no service/port/team patterns.
SUBJ = ["The deployment pipeline", "Our incident review", "The capacity plan", "The on-call rotation", "The migration runbook",
        "The quarterly roadmap", "The load test", "The architecture council", "The release checklist", "The postmortem",
        "The cost report", "The security review", "The observability stack", "The design document", "The platform team"]
VERB = ["recommends", "highlights", "requires", "proposes", "documents", "flags", "tracks", "summarizes", "questions", "revisits"]
OBJ = ["tighter rollback windows", "fewer manual approvals", "clearer ownership of alerts", "staged rollouts across regions",
       "better dashboards for latency percentiles", "consistent naming of environments", "budget alerts for storage growth",
       "shorter feedback loops in code review", "periodic chaos experiments", "automated certificate rotation",
       "a single source of truth for configuration", "idempotent database migrations", "explicit timeouts on every call",
       "structured logs with request identifiers", "capacity headroom before peak season"]
TAIL = ["This came up again in the last planning cycle.", "Several engineers raised the same concern independently.",
        "The trade-offs were discussed at length without a final decision.", "A follow-up is expected next sprint.",
        "Nobody objected, but nobody volunteered either.", "The change is considered low risk.",
        "It depends on work owned by another group.", "Early measurements look promising.", ""]

def filler_paragraph(rng):
    return " ".join(f"{rng.choice(SUBJ)} {rng.choice(VERB)} {rng.choice(OBJ)}. {rng.choice(TAIL)}".strip()
                    for _ in range(rng.randint(3, 6)))

def make_document(target_tokens, seed):
    rng = random.Random(seed)
    names = rng.sample([f"{a}-{n}" for a in ADJ for n in NOUN], NEEDLES)
    ports = rng.sample(range(2000, 9999), NEEDLES)
    truth = {names[i]: (str(ports[i]), rng.choice(TEAMS)) for i in range(NEEDLES)}
    paras = []
    while sum(len(p) for p in paras) < target_tokens * 4:  # ~4 chars/token for this prose
        paras.append(filler_paragraph(rng))
    # Spread needles evenly through the document, embedded mid-paragraph.
    slots = sorted(rng.sample(range(len(paras)), NEEDLES)) if len(paras) >= NEEDLES else [i * len(paras) // NEEDLES for i in range(NEEDLES)]
    for (name, (port, team)), idx in zip(truth.items(), slots):
        sentences = paras[idx].split(". ")
        pos = rng.randint(0, len(sentences))
        sentences.insert(pos, f"Note that the {name} service listens on port {port} and is owned by the {team} team")
        paras[idx] = ". ".join(sentences)
    return "\n\n".join(paras), truth

# Lenient: chunk agents sometimes write "name service | port | team team".
LINE = re.compile(r"([a-z]+-[a-z]+)(?:\s+service)?\W*\|\W*(?:port\s*)?(\d{4})\W*\|\W*(?:the\s+)?([a-z]+)", re.I)

def score(answer, truth):
    found = {}
    for m in LINE.finditer(answer or ""):
        found.setdefault(m.group(1).lower(), (m.group(2), m.group(3).lower()))
    exact = sum(1 for s, v in found.items() if truth.get(s) == v)
    named = sum(1 for s in found if s in truth)
    return {"recall": exact / len(truth), "service_recall": named / len(truth),
            "precision": exact / len(found) if found else 0.0, "extracted": len(found),
            "hallucinated": sum(1 for s in found if s not in truth)}

def post(path, body):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(), headers={"Content-Type": "application/json", **AUTH})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)

def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

def run(doc, chunks, merge=None):
    body = {"prompt": INSTRUCTION, "document": doc, "chunks": chunks, "priority": 1}
    if merge:
        body["merge"] = merge
    t0 = time.time()
    tid = post("/v1/trees", body)["tree_id"]
    evs = []
    with urllib.request.urlopen(urllib.request.Request(f"{BASE}/v1/trees/{tid}/events", headers=AUTH), timeout=1800) as r:
        etype = None
        for raw in r:
            line = raw.decode().rstrip("\n")
            if line.startswith("event:"):
                etype = line[6:].strip()
            elif line.startswith("data:") and etype not in ("delta", "thinking"):
                e = json.loads(line[5:]); evs.append(e)
                if e["type"] in ("tree.done", "tree.failed", "tree.cancelled"):
                    break
    wall = time.time() - t0
    final = evs[-1]
    done = [e for e in evs if e["type"] == "node.done" and e.get("stats")]
    execs = [e["stats"] for e in done if e.get("role") == "exec"]
    reduce = next((e["stats"] for e in done if e.get("role") == "reduce"), None)
    first_exec_done = min((ts(e["time"]) for e in done if e.get("role") == "exec"), default=None)
    created = next(ts(e["time"]) for e in evs if e["type"] == "tree.created")
    return {
        "tree_id": tid, "status": final["type"], "answer": final.get("text", "") if final["type"] == "tree.done" else "",
        "error": final.get("text", "")[:300] if final["type"] != "tree.done" else "",
        "wall_s": wall,
        "prompt_tokens": sum(s.get("prompt_tokens", 0) for s in execs) + (reduce or {}).get("prompt_tokens", 0),
        "max_exec_prompt_tokens": max((s.get("prompt_tokens", 0) for s in execs), default=0),
        "gen_tokens": sum(s.get("tokens", 0) for s in execs) + (reduce or {}).get("tokens", 0),
        "map_s": max((s["duration_ms"] for s in execs), default=0) / 1000,
        "reduce_s": (reduce or {}).get("duration_ms", 0) / 1000,
        "first_result_s": (first_exec_done - created) if first_exec_done else None,
        "merge_batches": (reduce or {}).get("merge_batches", 0),
        "failed_nodes": [e.get("text", "")[:200] for e in evs if e["type"] == "node.failed"],
    }

if __name__ == "__main__":
    sizes = [int(x) for x in sys.argv[1].split(",")]
    # Conditions: "1" = single context; "4" = 4 chunks, LLM merge;
    # "4L" = 4 chunks, list merge in code; "4C" = concat merge in code.
    conds = []
    for x in sys.argv[2].split(","):
        merge = {"L": "list", "C": "concat"}.get(x[-1])
        conds.append((int(x.rstrip("LC")), merge))
    reps = int(sys.argv[3])
    out = sys.argv[4]
    rows = []
    for size in sizes:
        for rep in range(reps):
            doc, truth = make_document(size, seed=size * 100 + rep)
            for c, merge in conds:
                r = run(doc, c, merge)
                r.update(score(r["answer"], truth))
                r.update({"size": size, "chunks": c, "merge": merge or ("llm" if c > 1 else "-"), "rep": rep, "doc_chars": len(doc)})
                rows.append(r)
                print(json.dumps({k: (round(v, 3) if isinstance(v, float) else v) for k, v in r.items() if k not in ("answer",)}), flush=True)
                with open(out, "w") as f:
                    json.dump(rows, f)
                time.sleep(2)
