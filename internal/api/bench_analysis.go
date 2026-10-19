package api

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// analysisVersion is bumped when analyze changes, so stored benchmarks are
// analyzed again when next opened.
const analysisVersion = 2

// suiteCategory names what each standard test message probes.
var suiteCategory = map[string]string{
	benchSuite[0]: "Regulations & refusals",
	benchSuite[1]: "Product facts from sources",
	benchSuite[2]: "Prices",
	benchSuite[3]: "Legal how-to",
	benchSuite[4]: "Recipe know-how",
	benchSuite[5]: "Technical config",
	benchSuite[6]: "General knowledge",
	benchSuite[7]: "Seasonal planning",
}

// figureRE matches figures worth checking against the sources: numbers with
// two or more digits, or with a decimal point, comma, slash or currency
// sign ("$25", "2,600", "1/2.9", "130"). Single digits (list numbers, "3
// days") are left out.
var figureRE = regexp.MustCompile(`\$?\d+(?:[.,/]\d+)+|\$\d+|\d{2,}`)

// figures returns the distinct figures in an answer and those the reference
// does not contain. Thousands separators are ignored when comparing.
func figures(answer, reference string) (total int, unsourced []string) {
	ref := strings.ReplaceAll(reference, ",", "")
	seen := map[string]bool{}
	for _, f := range figureRE.FindAllString(answer, -1) {
		f = strings.TrimPrefix(f, "$")
		key := strings.ReplaceAll(f, ",", "")
		if seen[key] {
			continue
		}
		seen[key] = true
		total++
		if !strings.Contains(ref, key) {
			unsourced = append(unsourced, f)
		}
	}
	return total, unsourced
}

// analyze computes the judge-independent numbers (figures checked against
// the sources, speed spread, answer length) and the written analysis.
// contexts holds each case's reference material.
func analyze(res *benchResults, contexts []string) {
	for ci := range res.Cases {
		if c := suiteCategory[res.Cases[ci].Prompt]; c != "" {
			res.Cases[ci].Category = c
		}
	}
	for mi := range res.Models {
		m := &res.Models[mi]
		m.Figures, m.FiguresSourced, m.AvgTokens = 0, 0, 0
		var tps []float64
		var ttft []float64
		tokens, n := 0, 0
		for ai := range m.Answers {
			a := &m.Answers[ai]
			a.Figures, a.FiguresSourced, a.Unsourced = 0, 0, nil
			if a.Status != string(store.StatusDone) {
				continue
			}
			n++
			tokens += a.Tokens
			if a.TPS > 0 {
				tps = append(tps, a.TPS)
			}
			if a.TTFTMs > 0 {
				ttft = append(ttft, float64(a.TTFTMs))
			}
			if a.Case < len(contexts) && strings.TrimSpace(contexts[a.Case]) != "" {
				total, uns := figures(a.Output, contexts[a.Case])
				a.Figures, a.FiguresSourced = total, total-len(uns)
				if len(uns) > 12 {
					uns = uns[:12]
				}
				a.Unsourced = uns
				m.Figures += total
				m.FiguresSourced += a.FiguresSourced
			}
		}
		if n > 0 {
			m.AvgTokens = tokens / n
		}
		if len(tps) > 0 {
			m.MinTPS, m.MaxTPS = slices.Min(tps), slices.Max(tps)
		}
		m.P90TTFTMs = int64(percentile(ttft, 0.9))
	}
	// Concurrency notes from before the half-the-streams rule (a peak of 6-7
	// of 8 is staggered starts, not missing slots) are dropped.
	notes := res.Notes[:0]
	for _, n := range res.Notes {
		stale := false
		for _, m := range res.Models {
			if strings.HasPrefix(n, m.Name+" generated at most ") && m.RanAtOnce*2 > m.Parallel {
				stale = true
			}
		}
		if !stale {
			notes = append(notes, n)
		}
	}
	res.Notes = notes
	res.Analysis = writeAnalysis(res)
	res.AnalysisVersion = analysisVersion
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	i := int(float64(len(s)-1)*p + 0.5)
	return s[min(i, len(s)-1)]
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// writeAnalysis states what the numbers say, plainly, with the caveats
// that matter (small samples, judge noise, a judge grading itself).
func writeAnalysis(res *benchResults) []string {
	var ms []*benchModel
	for i := range res.Models {
		if res.Models[i].Answered > 0 {
			ms = append(ms, &res.Models[i])
		}
	}
	if len(ms) == 0 {
		return []string{"No model finished an answer, so there is nothing to compare."}
	}
	var out []string
	cases := len(res.Cases)
	if cases < 4 {
		out = append(out, fmt.Sprintf("Only %d message(s) were replayed: treat the differences below as anecdotes. The standard test set (8 messages) gives a broader picture.", cases))
	}

	// Speed.
	if len(ms) > 1 {
		byTPS := slices.Clone(ms)
		sort.Slice(byTPS, func(i, j int) bool { return byTPS[i].MedianTPS > byTPS[j].MedianTPS })
		f, s := byTPS[0], byTPS[1]
		line := fmt.Sprintf("Speed with %d answers at once: %s is fastest at %.1f tok/s per answer (%s: %.1f), %.0f tok/s in total (%s: %.0f); first token after %s (%s: %s).",
			f.Parallel, f.Name, f.MedianTPS, s.Name, s.MedianTPS, f.AggregateTPS, s.Name, s.AggregateTPS, secs(f.MedianTTFTMs), s.Name, secs(s.MedianTTFTMs))
		if f.MedianTPS > 0 && s.MedianTPS/f.MedianTPS > 0.9 {
			line += " That is within 10%: about the same speed."
		}
		out = append(out, line)
	} else {
		m := ms[0]
		out = append(out, fmt.Sprintf("Speed with %d answers at once: %.1f tok/s per answer (%.1f-%.1f), %.0f tok/s in total, first token after %s (slowest 10%%: %s).",
			m.Parallel, m.MedianTPS, m.MinTPS, m.MaxTPS, m.AggregateTPS, secs(m.MedianTTFTMs), secs(m.P90TTFTMs)))
	}
	for _, m := range ms {
		if m.RanAtOnce > 0 && m.RanAtOnce*2 <= m.Parallel {
			out = append(out, fmt.Sprintf("%s ran only %d answer(s) at a time of the %d sent: its total speed reflects too few parallel slots, not the model.", m.Name, m.RanAtOnce, m.Parallel))
		}
	}

	// Length.
	if len(ms) > 1 {
		byLen := slices.Clone(ms)
		sort.Slice(byLen, func(i, j int) bool { return byLen[i].AvgTokens < byLen[j].AvgTokens })
		short, long := byLen[0], byLen[len(byLen)-1]
		if long.AvgTokens > 0 && float64(short.AvgTokens) < 0.85*float64(long.AvgTokens) {
			out = append(out, fmt.Sprintf("Length: %s's answers are %.0f%% shorter on average (%d vs %d tokens for %s). Shorter is faster to read, but check nothing asked for was left out.",
				short.Name, 100-pct(short.AvgTokens, long.AvgTokens), short.AvgTokens, long.AvgTokens, long.Name))
		} else {
			out = append(out, "Length: the answers are about as long as each other.")
		}
	}

	// Figures checked against the sources (no judge involved).
	withFig := 0
	for _, m := range ms {
		if m.Figures > 0 {
			withFig++
		}
	}
	if withFig > 0 {
		parts := []string{}
		for _, m := range ms {
			if m.Figures > 0 {
				parts = append(parts, fmt.Sprintf("%s %.0f%% (%d of %d)", m.Name, pct(m.FiguresSourced, m.Figures), m.FiguresSourced, m.Figures))
			}
		}
		line := "Sourced figures (prices, specs, dates and other numbers in answers to messages with sources, found in those sources; checked in code, no judge): " + strings.Join(parts, ", ") + "."
		if withFig > 1 {
			byFig := slices.Clone(ms)
			sort.Slice(byFig, func(i, j int) bool {
				return pct(byFig[i].FiguresSourced, byFig[i].Figures) > pct(byFig[j].FiguresSourced, byFig[j].Figures)
			})
			if d := pct(byFig[0].FiguresSourced, byFig[0].Figures) - pct(byFig[1].FiguresSourced, byFig[1].Figures); d >= 5 {
				line += fmt.Sprintf(" %s stays closer to its sources.", byFig[0].Name)
			} else {
				line += " No real difference."
			}
		}
		line += " A figure not in the sources is not always wrong (general knowledge, unit conversions), but it is where invented details hide."
		out = append(out, line)
	}

	// Refusals.
	for _, m := range ms {
		var refused []string
		for _, a := range m.Answers {
			if a.Refused != nil && *a.Refused {
				refused = append(refused, caseLabel(res, a.Case))
			}
		}
		if len(refused) > 0 {
			out = append(out, fmt.Sprintf("%s refused or withheld help on: %s.", m.Name, strings.Join(refused, "; ")))
		}
	}
	if !slices.ContainsFunc(ms, func(m *benchModel) bool { return m.Refusals > 0 }) {
		out = append(out, "Refusals: none.")
	}

	// Judge findings, per message.
	judged := slices.ContainsFunc(ms, func(m *benchModel) bool { return m.Judged > 0 })
	if judged && len(ms) > 1 {
		wins := map[string][]string{}
		ties := 0
		for ci := range res.Cases {
			best, bestScore, tie := "", 1<<30, false
			for _, m := range ms {
				a := answerFor(m, ci)
				if a == nil || a.Refused == nil {
					tie = true // not judged: no winner
					break
				}
				score := len(a.Unsupported) + len(a.Slips)
				if *a.Refused {
					score += 5
				}
				switch {
				case score < bestScore:
					best, bestScore, tie = m.Name, score, false
				case score == bestScore:
					tie = true
				}
			}
			if tie || best == "" {
				ties++
				continue
			}
			wins[best] = append(wins[best], caseLabel(res, ci))
		}
		parts := []string{}
		for _, m := range ms {
			if w := wins[m.Name]; len(w) > 0 {
				parts = append(parts, fmt.Sprintf("%s had fewer findings on %d (%s)", m.Name, len(w), strings.Join(w, "; ")))
			}
		}
		if ties > 0 {
			parts = append(parts, fmt.Sprintf("%d tied or were not judged", ties))
		}
		out = append(out, "By message (judge findings): "+strings.Join(parts, ", ")+".")
	}
	if judged {
		caveat := fmt.Sprintf("Judge: %s. Small judges miss invented details (especially in code and config) and flag correct ones; across repeat runs of the same answers the slip count varied from 2 to 9. Treat differences of a few findings as noise and open the answers to check.", res.Judge)
		if slices.ContainsFunc(ms, func(m *benchModel) bool { return m.Name == res.Judge }) {
			caveat += fmt.Sprintf(" %s is also one of the compared models and may favor its own answers; run again with another judge for a second opinion.", res.Judge)
		}
		out = append(out, caveat)
	}
	return out
}

func answerFor(m *benchModel, ci int) *benchAnswer {
	for i := range m.Answers {
		if m.Answers[i].Case == ci {
			return &m.Answers[i]
		}
	}
	return nil
}

func caseLabel(res *benchResults, ci int) string {
	if ci < len(res.Cases) {
		if c := res.Cases[ci].Category; c != "" {
			return fmt.Sprintf("%d %s", ci+1, c)
		}
	}
	return fmt.Sprintf("message %d", ci+1)
}

func secs(ms int64) string {
	if ms <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}

// caseContexts loads each case's reference material from its source run.
func (s *Server) caseContexts(ctx context.Context, cases []benchCase) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		if c.context != "" {
			out[i] = c.context
			continue
		}
		if snap, err := settings.ForTree(ctx, s.db, c.SourceTree); err == nil {
			out[i] = snap.Context
		}
	}
	return out
}
