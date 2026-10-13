package agent

import (
	"fmt"
	"strings"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// mergeItem is one input to a merge: a child's result, a failure note, or an
// intermediate consolidation of several results.
type mergeItem struct {
	Title  string
	Text   string
	Tokens int
}

const (
	// defaultContext is assumed when a model's context length is unknown. It
	// is deliberately small: over-chunking costs time, overflow loses data.
	defaultContext = 8192
	// charsPerToken is a conservative estimate for prose mixed with code.
	charsPerToken = 3
	itemOverhead  = 24 // per-item heading and separators
	maxMergePass  = 4
)

func estimateTokens(s string) int { return (len(s) + charsPerToken - 1) / charsPerToken }

func totalTokens(items []mergeItem) int {
	n := 0
	for _, it := range items {
		n += it.Tokens + itemOverhead
	}
	return n
}

// mergeBudget is how many input tokens a merge may use: the context minus a
// 10% safety margin, the reserved output, and the fixed prompt (directive and
// original task).
func mergeBudget(contextLength int, reducer settings.Role, fixed string) int {
	ctx := contextLength
	if ctx <= 0 {
		ctx = defaultContext
	}
	reserve := reducer.MaxTokens
	if reserve <= 0 {
		reserve = min(4096, ctx/4)
	}
	return max(ctx*9/10-reserve-estimateTokens(fixed), 512)
}

// planBatches groups items, in order, so each group fits the budget. An item
// that alone exceeds the budget must be fitted with fitItem first.
func planBatches(items []mergeItem, budget int) [][]mergeItem {
	var batches [][]mergeItem
	var cur []mergeItem
	used := 0
	for _, it := range items {
		cost := it.Tokens + itemOverhead
		if len(cur) > 0 && used+cost > budget {
			batches = append(batches, cur)
			cur, used = nil, 0
		}
		cur = append(cur, it)
		used += cost
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// fitItem truncates an item larger than budget, keeping its head and tail and
// saying so in the text, so the loss is visible rather than silent.
func fitItem(it mergeItem, budget int) (mergeItem, bool) {
	limit := budget - itemOverhead
	if it.Tokens <= limit {
		return it, false
	}
	keep := max(limit*charsPerToken-200, 400)
	if keep >= len(it.Text) {
		return it, false
	}
	head, tail := it.Text[:keep*2/3], it.Text[len(it.Text)-keep/3:]
	omitted := len(it.Text) - len(head) - len(tail)
	it.Text = fmt.Sprintf("%s\n\n[… %d characters omitted: this result alone exceeded the merge context …]\n\n%s", head, omitted, tail)
	it.Tokens = estimateTokens(it.Text)
	return it, true
}

func itemsBlock(items []mergeItem) string {
	var b strings.Builder
	for i, it := range items {
		fmt.Fprintf(&b, "\n### %d. %s\n%s\n", i+1, it.Title, it.Text)
	}
	return b.String()
}

// splitDocument cuts a document into about n contiguous parts of similar size,
// preferring paragraph boundaries so no sentence is split between agents.
func splitDocument(doc string, n int) []string {
	doc = strings.TrimSpace(doc)
	if n <= 1 || doc == "" {
		return []string{doc}
	}
	units := strings.Split(doc, "\n\n")
	if len(units) < n*2 {
		// Too few paragraphs to balance: fall back to lines, then sentences.
		if lines := strings.Split(doc, "\n"); len(lines) >= n*2 {
			units = lines
		} else {
			units = strings.SplitAfter(doc, ". ")
		}
	}
	target := len(doc) / n
	var parts []string
	var cur strings.Builder
	for _, u := range units {
		if strings.TrimSpace(u) == "" {
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(strings.TrimSpace(u))
		if cur.Len() >= target && len(parts) < n-1 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// codeMerge combines child results without a model call. "concat" joins the
// outputs in order; "list" keeps each distinct line once (compared ignoring
// case and spacing), for list-shaped answers such as extractions. Failed or
// missing parts are reported inline so the gap is visible.
func codeMerge(mode string, children []*store.Node) (out string, failed int) {
	var b strings.Builder
	seen := map[string]bool{}
	write := func(line string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	for _, c := range children {
		if c.Status != store.StatusDone || c.Output == nil {
			failed++
			reason := string(c.Status)
			if c.Error != nil {
				reason = *c.Error
			}
			write(fmt.Sprintf("[part %d (%s) failed: %s]", c.Idx+1, c.Title, reason))
			continue
		}
		text := strings.TrimSpace(*c.Output)
		if mode == settings.MergeConcat {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			write(text)
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, " \t\r")
			key := strings.ToLower(strings.Join(strings.Fields(line), " "))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			write(line)
		}
	}
	return b.String(), failed
}

// estimateRequest approximates a request's context use: prompt characters / 4
// plus per-message overhead plus the reserved output (maxTokens, or 1024).
// It errs high for prose and low for dense code; the shared-context budget
// keeps a margin, and a transient overflow is retried.
func estimateRequest(msgs []llm.Message, maxTokens int) int {
	prompt, output := estimateParts(msgs, maxTokens)
	return prompt + output
}

// estimateParts is estimateRequest split into prompt and reserved output, so
// the pool can calibrate the prompt part against real token counts.
func estimateParts(msgs []llm.Message, maxTokens int) (prompt, output int) {
	for _, m := range msgs {
		prompt += len(m.Content)/4 + 8
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return prompt, maxTokens
}
