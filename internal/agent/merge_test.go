package agent

import (
	"strings"
	"testing"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

func TestMergeBudget(t *testing.T) {
	r := settings.Role{}
	if got := mergeBudget(32000, r, ""); got != 32000*9/10-4096 {
		t.Fatalf("32k budget %d", got)
	}
	// Unknown context falls back to the conservative default.
	if got := mergeBudget(0, r, ""); got != defaultContext*9/10-defaultContext/4 {
		t.Fatalf("default budget %d", got)
	}
	// Reducer max_tokens is reserved for the output.
	if got := mergeBudget(16000, settings.Role{MaxTokens: 1000}, strings.Repeat("x", 300)); got != 16000*9/10-1000-100 {
		t.Fatalf("budget with reserve %d", got)
	}
}

func TestPlanBatchesKeepsOrderAndFits(t *testing.T) {
	var items []mergeItem
	for i := range 10 {
		items = append(items, mergeItem{Title: string(rune('a' + i)), Tokens: 1000})
	}
	batches := planBatches(items, 3100) // 3 items (3*1024) per batch
	if len(batches) != 4 {
		t.Fatalf("expected 4 batches, got %d", len(batches))
	}
	var order strings.Builder
	for _, b := range batches {
		if totalTokens(b) > 3100 {
			t.Fatalf("batch over budget: %d", totalTokens(b))
		}
		for _, it := range b {
			order.WriteString(it.Title)
		}
	}
	if order.String() != "abcdefghij" {
		t.Fatalf("order changed: %s", order.String())
	}
}

func TestFitItemTruncatesVisibly(t *testing.T) {
	big := mergeItem{Title: "x", Text: strings.Repeat("A", 30000) + strings.Repeat("Z", 30000)}
	big.Tokens = estimateTokens(big.Text)
	fit, cut := fitItem(big, 2000)
	if !cut || fit.Tokens > 2000 || !strings.Contains(fit.Text, "characters omitted") {
		t.Fatalf("not fitted: cut=%v tokens=%d", cut, fit.Tokens)
	}
	if !strings.HasPrefix(fit.Text, "AAA") || !strings.HasSuffix(fit.Text, "ZZZ") {
		t.Fatal("head and tail must be kept")
	}
	small := mergeItem{Text: "ok", Tokens: 1}
	if _, cut := fitItem(small, 2000); cut {
		t.Fatal("small item must not be cut")
	}
}

func TestSplitDocument(t *testing.T) {
	var paras []string
	for i := range 40 {
		paras = append(paras, strings.Repeat("word ", 20+i%7)+".")
	}
	doc := strings.Join(paras, "\n\n")
	parts := splitDocument(doc, 4)
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(parts))
	}
	joined := strings.Join(parts, "\n\n")
	if strings.ReplaceAll(joined, "\n", "") != strings.ReplaceAll(doc, "\n", "") {
		t.Fatal("split must preserve all content in order")
	}
	for _, p := range parts {
		if len(p) < len(doc)/8 {
			t.Fatalf("unbalanced part of %d chars (doc %d)", len(p), len(doc))
		}
	}
	if got := splitDocument("one paragraph only. Two sentences here. And a third. Plus fourth. Fifth one. Sixth.", 3); len(got) != 3 {
		t.Fatalf("sentence fallback gave %d parts", len(got))
	}
	if got := splitDocument(doc, 1); len(got) != 1 {
		t.Fatal("n=1 must return the whole document")
	}
}

func node(idx int, title string, status store.Status, out, errMsg string) *store.Node {
	n := &store.Node{Idx: idx, Title: title, Status: status}
	if out != "" {
		n.Output = &out
	}
	if errMsg != "" {
		n.Error = &errMsg
	}
	return n
}

func TestCodeMerge(t *testing.T) {
	kids := []*store.Node{
		node(0, "Part 1/3", store.StatusDone, "a | 1 | x\nb | 2 | y\n", ""),
		node(1, "Part 2/3", store.StatusFailed, "", "backend 400"),
		node(2, "Part 3/3", store.StatusDone, "B  |  2 | Y\n\nc | 3 | z", ""),
	}
	list, failed := codeMerge(settings.MergeList, kids)
	want := "a | 1 | x\nb | 2 | y\n[part 2 (Part 2/3) failed: backend 400]\nc | 3 | z"
	if list != want || failed != 1 {
		t.Fatalf("list merge:\n%q\nwant\n%q (failed=%d)", list, want, failed)
	}
	concat, _ := codeMerge(settings.MergeConcat, kids)
	if !strings.Contains(concat, "a | 1 | x\nb | 2 | y") || !strings.Contains(concat, "B  |  2 | Y\n\nc | 3 | z") {
		t.Fatalf("concat must keep every output verbatim:\n%s", concat)
	}
	if strings.Index(concat, "a | 1") > strings.Index(concat, "B  |  2") {
		t.Fatal("concat must keep part order")
	}
	// Code-like duplicates survive concat (it never dedupes).
	code := []*store.Node{node(0, "p1", store.StatusDone, "}\n", ""), node(1, "p2", store.StatusDone, "}\n", "")}
	if c, _ := codeMerge(settings.MergeConcat, code); strings.Count(c, "}") != 2 {
		t.Fatalf("concat dropped a line: %q", c)
	}
}
