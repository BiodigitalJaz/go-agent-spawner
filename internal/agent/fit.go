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
