package agent

import (
	"strings"
	"testing"
)

func TestSplitTextKeepsTextExactly(t *testing.T) {
	var b strings.Builder
	for i := range 300 {
		b.WriteString("func f() {\n\treturn 1 // ünïcode line\n}\n")
		if i%7 == 0 {
			b.WriteString("\n")
		}
	}
	text := b.String()
	for _, n := range []int{2, 3, 5, 9} {
		secs := splitText(text, n)
		if strings.Join(secs, "") != text {
			t.Fatalf("n=%d: sections do not rejoin to the original", n)
		}
		target := (len(text) + n - 1) / n
		for i, s := range secs {
			if len(s) > target+target/4 {
				t.Fatalf("n=%d: section %d is %d bytes, over 1.25×%d", n, i, len(s), target)
			}
			if i < len(secs)-1 && !strings.HasSuffix(s, "\n") {
				t.Fatalf("n=%d: section %d does not end at a line boundary", n, i)
			}
		}
		if len(secs) < n || len(secs) > n+2 {
			t.Fatalf("n=%d: got %d sections", n, len(secs))
		}
	}
	// One huge line is cut at rune boundaries and still rejoins exactly.
	long := strings.Repeat("ä", 5000)
	secs := splitText(long, 4)
	if strings.Join(secs, "") != long || len(secs) < 4 {
		t.Fatalf("long line: %d sections", len(secs))
	}
	for _, s := range secs {
		if !strings.HasPrefix(s, "ä") {
			t.Fatal("cut inside a rune")
		}
	}
}

func TestExpandParts(t *testing.T) {
	small := Part{Title: "a.md", Text: "short"}
	big := Part{Title: "big.md", Text: strings.Repeat("line of text here\n", 9000)} // ~40.5k tokens
	out, err := expandParts([]Part{small, big}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || out[0] != small || out[1].Title != "big.md (section 1/3)" || out[3].Title != "big.md (section 3/3)" {
		t.Fatalf("got %d parts: %v", len(out), titles(out))
	}
	if out[1].Text+out[2].Text+out[3].Text != big.Text {
		t.Fatal("sections must rejoin to the original part")
	}
	if out, _ := expandParts([]Part{small, big}, -1); len(out) != 2 {
		t.Fatal("negative max keeps parts whole")
	}
	huge := Part{Title: "huge", Text: strings.Repeat("x\n", 1_000_000)}
	if _, err := expandParts([]Part{huge}, 100); err == nil {
		t.Fatal("expected ErrTooManyParts")
	}
}

func TestLargestFirst(t *testing.T) {
	parts := []Part{{Text: "bb"}, {Text: "dddd"}, {Text: "a"}, {Text: "cc"}}
	got := largestFirst(parts)
	want := []int{1, 0, 3, 2} // ties keep the given order
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func titles(ps []Part) []string {
	var t []string
	for _, p := range ps {
		t = append(t, p.Title)
	}
	return t
}
