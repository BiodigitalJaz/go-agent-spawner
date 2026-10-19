"""Find the safe chunk size for the current worker model.

Reuses chunk_experiment.py's documents (30 planted facts in engineering
filler) and scoring, but runs explicit (document size, chunks) pairs so the
tokens per chunk vary at a fixed document size:

- single context at growing sizes: the largest document one agent reads
  without missing facts (the ceiling for one chunk);
- one large document split into more and more chunks: speed and accuracy
  per chunk size, with the list merge in code.

Runs execute one at a time. Usage:
  SPAWNER_TOKEN=... python3 chunk_size_sweep.py out.json [reps] [first_rep] [size:chunks,...]
first_rep picks new documents (seeds) for confirmation runs; the pairs
default to the full sweep below.
"""
import json, os, sys, time, urllib.request

sys.path.insert(0, os.path.dirname(__file__))
from chunk_experiment import AUTH, BASE, make_document, run, score  # noqa: E402

# (document tokens, chunks). chunks=1 is a single context.
SINGLE = [(4000, 1), (8000, 1), (12000, 1), (16000, 1), (20000, 1), (24000, 1)]
MID = [(24000, 2), (24000, 3), (24000, 4), (24000, 8)]
LARGE = [(60000, 4), (60000, 6), (60000, 8), (60000, 12), (60000, 16)]


def worker_model():
    req = urllib.request.Request(BASE + "/v1/settings", headers=AUTH)
    with urllib.request.urlopen(req, timeout=30) as r:
        s = json.load(r)["settings"]
    return s["worker"]["model"]


if __name__ == "__main__":
    out = sys.argv[1]
    reps = int(sys.argv[2]) if len(sys.argv) > 2 else 2
    first = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    pairs = SINGLE + MID + LARGE
    if len(sys.argv) > 4:
        pairs = [tuple(int(v) for v in p.split(":")) for p in sys.argv[4].split(",")]
    model = worker_model()
    rows = []
    for size, chunks in pairs:
        for rep in range(first, first + reps):
            doc, truth = make_document(size, seed=size * 100 + rep)
            r = run(doc, chunks, "list" if chunks > 1 else None)
            r.update(score(r["answer"], truth))
            r.update({"model": model, "size": size, "chunks": chunks, "merge": "list" if chunks > 1 else "-", "rep": rep,
                      "doc_chars": len(doc), "chunk_tokens_est": size // chunks})
            r.pop("answer", None)
            rows.append(r)
            print(json.dumps({k: (round(v, 3) if isinstance(v, float) else v) for k, v in r.items()}), flush=True)
            with open(out, "w") as f:
                json.dump(rows, f)
            time.sleep(2)
