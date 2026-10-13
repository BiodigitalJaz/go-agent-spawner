package agent

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxPartTokens is the size above which a given part is split into
	// sections. It guards against parts that come close to a model's context
	// rather than chasing parallelism: on a 29-file summary run (2026-10-12),
	// splitting at 6k tokens made 46 agents whose extra summaries cost more
	// generation than the added concurrency won back (343 s against 219 s).
	// Per-part work is output-bound, so fewer, larger parts are cheaper.
	DefaultMaxPartTokens = 16000
	// MaxExpandedParts caps how many agents one request can fan out to.
	MaxExpandedParts = 400
)

// ErrTooManyParts means the input would fan out to more than
// MaxExpandedParts agents.
var ErrTooManyParts = errors.New("too many parts")

// estTokens sizes parts for splitting at a conservative 3 characters per
// token: dense text (book scans measured 1.9-3.4) at chars/4 made sections
// that alone came near a model's whole context.
func estTokens(s string) int { return len(s) / 3 }

// expandParts splits every part larger than maxTokens into sections of at
// most about maxTokens each, titled "<title> (section i/n)". maxTokens < 0
// disables splitting; 0 means DefaultMaxPartTokens.
func expandParts(parts []Part, maxTokens int) ([]Part, error) {
	if maxTokens == 0 {
		maxTokens = DefaultMaxPartTokens
	}
	if maxTokens < 0 {
		return parts, nil
	}
	var out []Part
	for _, p := range parts {
		t := estTokens(p.Text)
		if t <= maxTokens {
			out = append(out, p)
			continue
		}
		secs := splitText(p.Text, (t+maxTokens-1)/maxTokens)
		for i, s := range secs {
			out = append(out, Part{Title: fmt.Sprintf("%s (section %d/%d)", p.Title, i+1, len(secs)), Text: s})
		}
	}
	if len(out) > MaxExpandedParts {
		return nil, fmt.Errorf("%w: input splits into %d parts of at most %d tokens; the limit is %d", ErrTooManyParts, len(out), maxTokens, MaxExpandedParts)
	}
	return out, nil
}

// splitText cuts text into about n sections at line boundaries without
// changing it: joining the sections gives back the original exactly. Once a
// section reaches its target size it ends at the next blank line (a
// paragraph or function boundary), and it never grows past 1.25× the
// target: it ends before a line that would overshoot. A single line longer
// than that (minified code, a data dump) is cut at a rune boundary.
func splitText(text string, n int) []string {
	if n <= 1 || len(text) == 0 {
		return []string{text}
	}
	target := (len(text) + n - 1) / n
	limit := target + target/4
	var out []string
	start, pos := 0, 0
	for pos < len(text) {
		end := strings.IndexByte(text[pos:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += pos + 1
		}
		if end-start > limit {
			if pos > start { // end the section before this line
				out = append(out, text[start:pos])
				start = pos
				continue
			}
			cut := start + target // the line alone is too long
			for cut > start && !utf8.RuneStart(text[cut]) {
				cut--
			}
			out = append(out, text[start:cut])
			start, pos = cut, cut
			continue
		}
		line := text[pos:end]
		pos = end
		if end-start >= target && strings.TrimSpace(line) == "" {
			out = append(out, text[start:end])
			start = end
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// largestFirst returns part indexes ordered by size, largest first. Starting
// the longest work first keeps a big part from running alone at the end
// while the other slots sit idle.
func largestFirst(parts []Part) []int {
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(len(parts[b].Text), len(parts[a].Text)) })
	return order
}
