package agent

import (
	"strings"
	"testing"
)

func TestAutoChunks(t *testing.T) {
	for _, c := range []struct{ tokens, safe, want int }{
		// Default rule.
		{100, 0, 1}, {6000, 0, 1}, {6001, 0, 4}, {30000, 0, 4}, {30001, 0, 5}, {60000, 0, 8}, {500000, 0, 16},
		// A safe chunk size never lets chunks grow past it (Gemma: 5000).
		{4000, 5000, 1}, {5500, 5000, 2}, {24000, 5000, 5}, {60000, 5000, 12}, {500000, 5000, MaxChunks},
		// A larger safe size keeps the default rule (Qwen: 16000).
		{24000, 16000, 4}, {60000, 16000, 8},
	} {
		if got := AutoChunks(strings.Repeat("abcd", c.tokens), c.safe); got != c.want {
			t.Errorf("AutoChunks(%d tokens, safe %d) = %d, want %d", c.tokens, c.safe, got, c.want)
		}
	}
}
