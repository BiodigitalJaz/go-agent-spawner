# Chunked multi-agent context vs. one large context

**Question:** is splitting a large input across parallel agents more performant than one agent reading the whole input?

**Answer: yes, on every measure, once the merge is done in code** (experiment v2, spawner 0.1.6–0.1.8). With 4 chunks and the `list` merge:

| | Single context | **4 chunks · list merge** |
|---|---|---|
| End-to-end latency | 9.3–14.2 s | **4.1–6.9 s (51–65% faster)** |
| Accuracy (30 facts) | 9/17 perfect; recall 0.86–0.92 at 18–29k | **21/21 perfect**, 2.4k–65k tokens |
| Larger than the 32k context | fails | **works**: 65k tokens in 12.6 s |
| First partial result (median) | 11.2 s | **2.6 s** |

v1 (below) used an LLM merge, which re-generates the whole answer. That made chunking ~40% *slower* than a single context even though it was more accurate. Moving the merge into code removed the penalty entirely. For answers that can't be combined line by line (prose that needs synthesis), the LLM merge remains, with its cost.

## v2 results: code merge (`list`) vs LLM merge vs single context

Same documents and seeds as v1, runs one at a time. ≤24k tokens: 3 repetitions on 0.1.6. ≥29k: 2 repetitions on 0.1.8. Cells show mean wall time · mean recall. Raw data: [`results_v2.json`](results_v2.json).

| Doc tokens | Single | 4 · LLM merge | **4 · list merge** | 8 · list merge | 4·list vs single |
|---|---|---|---|---|---|
| 2.4k | 9.3 s · 1.000 | 13.2 s · 1.000 | **4.1 s · 1.000** | 5.5 s · 1.000 | **55% faster** |
| 5.9k | 9.8 s · 1.000 | 14.3 s · 1.000 | **4.3 s · 1.000** | 6.4 s · 1.000 | **56% faster** |
| 11.3k | 11.2 s · 1.000 | 15.3 s · 1.000 | **4.3 s · 1.000** | 7.6 s · 1.000 | **62% faster** |
| 18.6k | 11.4 s · 0.856 | 17.3 s · 1.000 | **4.6 s · 1.000** | 9.3 s · 1.000 | **60% faster** |
| 24.3k | 13.8 s · 0.922 | 19.3 s · 1.000 | **4.9 s · 1.000** | 9.8 s · 1.000 | **65% faster** |
| 29.4k | 14.2 s · 0.900 | 22.0 s · 1.000 | **6.9 s · 1.000** | 14.8 s · 1.000 | **51% faster** |
| 43.8k | fails (> context) | 26.3 s · 1.000 | **11.1 s · 1.000** | 18.8 s · 1.000 | only option |
| 65.4k | fails (> context) | 30.6 s · 1.000 | **12.6 s · 1.000** | 21.2 s · 0.967 | only option |

| Condition | Runs | Perfect | Hallucinations | First result (median) |
|---|---|---|---|---|
| Single context | 21 (17 within context) | 9 | 1 | 11.2 s |
| 4 · LLM merge | 21 | 21 | 0 | 5.3 s |
| **4 · list merge** | 21 | **21** | **0** | **2.6 s** |
| 8 · list merge | 21 | 19 | 1 | 4.9 s |

### What v2 adds

1. **Code merge turns the latency loss into a 51–65% win.** Each agent writes only its quarter of the answer, all in parallel, and nothing is re-generated afterwards. That beats v1's estimate (10–40%): wall time minus merge time had also counted the merge job's queue hop.
2. **4 chunks is the sweet spot on this GPU.** 8 chunks is slower (more repeated instructions, 8 streams sharing the GPU) and slightly less accurate.
3. **LM Studio's context is shared across parallel slots.** Four 10.7k-token chunks at once (≈43k) exceed the 32k pool and fail mid-stream with `"Context size has been exceeded"` (code 500, transient). A 0.1.4 change had made that error permanent, so a v2 run on 0.1.5 lost every chunk at 43k/65k. 0.1.7 fixed it twice: correct error classification (code before keywords; transient overflow is retried without using up an attempt) and **token-budgeted admission** for models marked `shared_context`. Those runs were discarded and re-run on 0.1.8.
4. **Never reject on an estimate.** 0.1.7's chars÷4 estimate (35k) rejected a 28k-token single-context request outright. 0.1.8 caps oversized estimates at the budget, so the request waits for an idle pool and the server, which counts real tokens, decides.

### Recommendation

- **Document mode, 4 chunks, `merge: list`** for extraction and other list-shaped answers at any size: the fastest, the most accurate, and the only option past the context.
- **`merge: concat`** for independent sections or code (never drops lines). **`merge: llm`** only when the answer needs synthesis across parts.
- Mark LM Studio / llama.cpp models `shared_context` with their loaded context, so admission prevents overflows instead of retrying them.

---

# v3 (2026-10-19): safe chunk size for Gemma 4 12B

**Question:** with `gemma-4-12b-fast` (Gemma 4 12B Q4_K_M, LM Studio, 32k context shared by 8 slots, reasoning off) on every role, how large can a chunk be before facts are missed?

**Answer: about 4k Gemma tokens (≈5k by the chars÷4 estimate, ≈20k characters) per chunk for lossless extraction.** Up to ~8k the recall is ~98% (an occasional missed fact); from ~9k it drops to 60–85%. Qwen 3.5 9B (v1/v2) stayed perfect up to ~11k in one context and at ~16k per chunk, so the safe size is per model.

Harness: [`chunk_size_sweep.py`](chunk_size_sweep.py) (same documents and scoring as v1/v2: 30 planted facts, exact match, list merge in code). 70 runs, one at a time: the full sweep with 2 documents per setting, then 4 more documents for each setting near the boundary. Raw data: [`results_v3_gemma.json`](results_v3_gemma.json), [`results_v3_gemma_confirm.json`](results_v3_gemma_confirm.json).

Gemma's tokenizer counts ~25% fewer tokens than chars÷4 on this text (a "24k" document is ~18.5k Gemma tokens). The tables use LM Studio's real prompt-token count of the largest chunk.

### Recall by chunk size (all runs, real tokens of the largest chunk)

| Real tokens per chunk | Runs | Perfect | Mean recall | Worst |
|---|---|---|---|---|
| ~3k | 4 | 4/4 | 1.000 | 1.00 |
| **~4k** | 8 | **8/8** | **1.000** | 1.00 |
| ~5k | 6 | 4/6 | 0.978 | 0.93 |
| ~6k | 12 | 8/12 | 0.981 | 0.87 |
| ~7k | 6 | 3/6 | 0.939 | 0.80 |
| ~8k | 6 | 3/6 | 0.983 | 0.97 |
| ~9k | 8 | 1/8 | 0.817 | 0.67 |
| ~11-12k | 4 | 0/4 | 0.72-0.85 | 0.60 |

No run failed and none invented a fact; misses are omissions.

### Speed against accuracy (60k-token document, list merge)

| Chunks | Tokens/chunk (est · real) | Wall s | Recall (min) | Runs |
|---|---|---|---|---|
| 4 | 15k · 11.1k | 28.4 | 0.850 (0.83) | 2 |
| 6 | 10k · 7.6k | **27.7** | 0.983 (0.97) | 6 |
| 8 (current rule) | 7.5k · 5.9k | 29.7 | 0.989 (0.97) | 6 |
| **12** | **5k · 4.1k** | 36.0 | **1.000 (1.00)** | 6 |
| 16 | 3.8k · 3.3k | 38.5 | 1.000 (1.00) | 2 |

24k document: 3 chunks 15.0 s · 0.972, 4 chunks 15.6 s · 0.978, 8 chunks 22.5 s · 1.000.

### What this means

1. **Safe chunk size for Gemma: ≤5k estimated tokens (≈4k real).** Lossless in every run, at ~20-25% more wall time than 7.5k chunks (more chunks share the GPU and repeat the instructions).
2. **The current rule (`AutoChunks`: 4 chunks up to 30k, then 7.5k per chunk) gives Gemma ~98%:** usually right, occasionally one fact short. Fine for summaries, not for exhaustive extraction.
3. **Never above ~8k real per chunk on Gemma** (≥9k loses 15-40% of facts). A single Gemma context is only safe for documents up to ~5k estimated tokens.
4. **For large documents Qwen is much faster at full accuracy**: 65k tokens in 12.6 s with 4 chunks (v2) against 36 s with 12 chunks on Gemma. Worth weighing in the Gemma trial.
5. Make the chunk size a per-model setting, measured with this sweep for each new model.

---

# v1 (first run, LLM merge only)

## Setup

- Model `qwen/qwen3.5-9b` Q8_0 on LM Studio (RTX 4080 SUPER 16 GB), reasoning off (`reasoning_effort: "none"`), context 32,000, 8 parallel slots. Spawner 0.1.3 on Kubernetes (2 workers).
- **Task:** extract every planted fact. Each synthetic document contains **30 facts** (`<service> listens on port <n> and is owned by the <team> team`) spread evenly through engineering-prose filler. Output format: `service | port | team`, one line per service.
- **Scoring:** exact match of all three fields against ground truth. Recall = found / 30; a hallucination is a listed service that does not exist. A lenient parser accepts `name service | port | team team`; no fact was missed for format reasons.
- **Conditions:** *single* (whole document in one agent's context) against *document mode* (the document is split in code on paragraph boundaries into 4, 8 or 16 parts, one exec agent each, then one LLM merge). The planner is skipped in document mode: an LLM planner would have to re-type the document.
- 69 runs, executed **one at a time** so latency is not confounded by load. 3 repetitions per cell up to 23k tokens, 2 above. Each repetition uses a different seed (different facts and positions).
- Harness: [`chunk_experiment.py`](chunk_experiment.py) (conditions such as `1,4,4L,8L,4C`). Raw v1 data: [`results.json`](results.json).

## Results

Document size is LM Studio's prompt-token count. "Extract" is the slowest chunk agent's generation time; "merge" is the reducer's.

| Doc tokens | Mode | Wall s | Recall (min) | Halluc. | Extract s | Merge s | 1st result s | Largest context |
|---|---|---|---|---|---|---|---|---|
| 2.6k | single | **8.3** | 1.000 (1.00) | 0 | 8.2 | – | 8.2 | 2.6k |
| | 4 chunks | 13.4 | 1.000 (1.00) | 0 | 4.1 | 8.3 | 2.6 | 0.9k |
| | 8 chunks | 13.6 | 1.000 (1.00) | 0 | 3.3 | 8.3 | 1.7 | 0.6k |
| 6.0k | single | **9.2** | 1.000 (1.00) | 0 | 9.1 | – | 9.1 | 6.0k |
| | 4 chunks | 14.4 | 1.000 (1.00) | 0 | 5.0 | 8.5 | 3.9 | 1.7k |
| | 8 chunks | 15.3 | 1.000 (1.00) | 0 | 4.5 | 8.7 | 2.5 | 1.0k |
| 11.1k | single | **10.4** | 0.989 (0.97) | 0 | 10.2 | – | 10.3 | 11.1k |
| | 4 chunks | 16.2 | **1.000** (1.00) | 0 | 5.6 | 8.4 | 4.2 | 3.0k |
| | 8 chunks | 17.1 | **1.000** (1.00) | 0 | 4.9 | 8.4 | 2.9 | 1.6k |
| 18.0k | single | **11.2** | 0.922 (0.90) | 1 | 10.7 | – | 11.0 | 18.0k |
| | 4 chunks | 17.6 | **1.000** (1.00) | 0 | 7.2 | 8.6 | 5.4 | 4.7k |
| | 8 chunks | 18.2 | 0.989 (0.97) | 1 | 6.0 | 8.5 | 2.9 | 2.5k |
| 23.4k | single | **12.2** | 0.922 (0.90) | 0 | 12.0 | – | 12.1 | 23.4k |
| | 4 chunks | 19.6 | **1.000** (1.00) | 0 | 7.4 | 8.5 | 5.6 | 6.0k |
| | 8 chunks | 20.4 | 0.989 (0.97) | 0 | 5.6 | 8.3 | 2.8 | 3.1k |
| 28.3k | single | **12.6** | 0.850 (0.83) | 1 | 12.3 | – | 12.5 | 28.3k |
| | 4 chunks | 19.5 | **1.000** (1.00) | 0 | 9.6 | 9.0 | 8.4 | 7.2k |
| | 8 chunks | 20.6 | **1.000** (1.00) | 0 | 9.7 | 8.9 | 3.8 | 3.8k |
| | 16 chunks | 21.9 | **1.000** (1.00) | 0 | 7.4 | 8.8 | 2.7 | 2.0k |
| 43.7k | single | **failed** | – | – | – | – | – | > 32k context |
| | 4 chunks | 27.8 | **1.000** (1.00) | 0 | 7.6 | 8.7 | 10.3 | 10.7k |
| | 8 chunks | 24.1 | **1.000** (1.00) | 0 | 9.8 | 8.5 | 4.2 | 5.5k |
| | 16 chunks | 25.9 | **1.000** (1.00) | 0 | 7.6 | 9.2 | 3.1 | 2.9k |
| 65.2k | single | **failed** | – | – | – | – | – | > 32k context |
| | 4 chunks | 29.9 | **1.000** (1.00) | 0 | 10.5 | 8.7 | 8.0 | 15.7k |
| | 8 chunks | 28.1 | 0.983 (0.97) | 1 | 9.1 | 8.4 | 5.6 | 8.0k |
| | 16 chunks | 30.0 | **1.000** (1.00) | 0 | 9.4 | 8.4 | 3.1 | 4.1k |

**Totals.** Chunked: 48 runs, 45 perfect, 2 hallucinations. Single context: 17 completed runs, 8 perfect, 2 hallucinations; the 4 runs larger than the context failed.

## What the data says

1. **Accuracy: chunking wins once inputs are long.** Single-context recall is flat to about 6k tokens, then falls steadily: 0.989 at 11k, 0.922 at 18–23k, 0.850 at 28k. It misses 2–5 of 30 facts, and the misses are in the middle of the document. Every agent in chunked mode reads at most ~16k tokens, often far less, and 4 chunks was perfect in all 21 runs.
2. **The merge is lossless here.** Recall of the union of raw chunk outputs equals final recall in every run, so the rare chunked misses come from extraction (all with 8 chunks), not from the merge.
3. **Capacity: chunking is the only option past the context.** A 65k-token document (2× the 32k context) was processed at recall 1.0. A single context fails, now in 0.9 s with a non-retryable 400 (it previously took ~100 s of pointless retries; fixed in 0.1.4).
4. **Latency: a single context is faster while the document fits.** Prefill on the GPU is fast: 28k tokens are read in a few seconds, and total single-context time grows only from 8.3 to 12.6 s. The answer's ~400 output tokens dominate. Chunking splits that output across parallel agents, but the LLM merge then **re-generates all of it serially**, a fixed ~8.5 s. End to end, chunked runs take 6–7 s longer.
5. **Feedback: chunking surfaces results sooner.** The first agent finishes in 1.7–5.6 s with 8–16 chunks, against 8–12 s for the single answer.
6. **More chunks doesn't make it faster.** 8 and 16 chunks share the same GPU slots, and the merge cost is unchanged. 4 chunks had the best accuracy; 16 chunks had the fastest first result.

## Recommendations

- **Use document mode with 4 chunks** when the input exceeds ~10k tokens or the model's context, or when accuracy matters more than ~7 s of latency.
- **Use a single context** for inputs under ~6k tokens, where accuracy is equal and it is ~35–40% faster.
- **Biggest latency lever: avoid the LLM re-writing list-shaped answers.** *Implemented in 0.1.5 as `merge: list|concat` and measured in v2 above: 51–65% faster than a single context.*

## Caveats

- One task type: needle-style extraction with structured, output-heavy answers. Tasks that need reasoning *across* distant parts of a document may lose accuracy when chunked. Summaries produce shorter merges, which changes the latency balance.
- One model and GPU. Larger models prefill more slowly, which favours chunking on latency too.
- 2–3 repetitions per cell. The accuracy trend is consistent across sizes, but individual cells have wide intervals.
- Synthetic filler is repetitive; real documents may be harder or easier for long-context attention.
