package agent

// Sizing one agent's request to the worker model's context. Every agent
// receives the tree's reference material, so each agent's own input (a
// document chunk, a file section) gets what is left: an input that would
// not fit on its own is split across more agents instead of overflowing.
// Estimates use 3 characters per token (conservative for prose and code).

const (
	referenceSharePct = 40  // the most of a context reference material may take
	requestOverhead   = 300 // directives, framing, separators
	minPartBudget     = 500 // below this an agent has no room to work
)

func contextOrDefault(contextTokens int) int {
	if contextTokens <= 0 {
		return defaultContext
	}
	return contextTokens
}

// MaxReferenceChars is the reference size a model with contextTokens allows.
func MaxReferenceChars(contextTokens int) int {
	return contextOrDefault(contextTokens) * referenceSharePct / 100 * charsPerToken
}

// PartBudget is the tokens left for one agent's own input once the
// reference, the instruction and the reserved output are counted, within
// 90% of the model's context. Below MinPartBudget nothing useful fits.
func PartBudget(contextTokens, reserveOutput, referenceChars, instructionChars int) int {
	if reserveOutput <= 0 {
		reserveOutput = 1024
	}
	return contextOrDefault(contextTokens)*9/10 - reserveOutput - (referenceChars+instructionChars+charsPerToken-1)/charsPerToken - requestOverhead
}

// MinPartBudget is exported for callers that reject requests that cannot fit.
const MinPartBudget = minPartBudget

// AutoChunks is the measured rule of thumb for splitting a document: one
// context is as accurate and faster up to about 6k tokens (a split adds a
// merge that rewrites the answer, roughly doubling the text generated);
// above that 4 chunks were fastest and fully accurate up to 65k tokens on
// an 8-slot server, and larger documents get one chunk per ~7.5k tokens.
//
// chunkTokens is the worker model's safe chunk size (0: none known): chunks
// are never larger, since models differ a lot here (Gemma 4 12B misses
// facts above ~5k estimated tokens per chunk, Qwen 3.5 9B held to ~16k;
// experiments/chunking/RESULTS.md v3).
func AutoChunks(doc string, chunkTokens int) int {
	t := len(doc) / 4
	var n int
	switch {
	case t <= 6000:
		n = 1
	case t <= 30000:
		n = 4
	default:
		n = min(16, (t+7499)/7500)
	}
	if chunkTokens > 0 {
		n = max(n, min(MaxChunks, (t+chunkTokens-1)/chunkTokens))
	}
	return n
}

// MaxChunks is the most chunks a document is split into.
const MaxChunks = 64
