package events

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunksAndCap(t *testing.T) {
	s := strings.Repeat("ä", MaxEventText) // 2 bytes each: 128 KiB
	parts := chunks(s)
	if strings.Join(parts, "") != s || len(parts) != 2 {
		t.Fatalf("%d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > MaxEventText || !utf8.ValidString(p) {
			t.Fatalf("bad chunk: %d bytes, valid %v", len(p), utf8.ValidString(p))
		}
	}
	if got := chunks("small"); len(got) != 1 || got[0] != "small" {
		t.Fatalf("small text: %v", got)
	}
	e := Event{Text: s}
	capText(&e)
	if !e.TextTruncated || len(e.Text) > MaxEventText || !utf8.ValidString(e.Text) {
		t.Fatalf("cap: truncated %v len %d", e.TextTruncated, len(e.Text))
	}
	e = Event{Text: "short"}
	capText(&e)
	if e.TextTruncated || e.Text != "short" {
		t.Fatal("short text must be untouched")
	}
}
