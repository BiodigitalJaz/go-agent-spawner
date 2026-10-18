package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
)

func TestPromptURLs(t *testing.T) {
	got := promptURLs("Have a look at: https://antrado.com. Also (https://example.com/a?b=1) and https://antrado.com!")
	if strings.Join(got, " ") != "https://antrado.com https://example.com/a?b=1" {
		t.Fatalf("urls = %v", got)
	}
}

func status(err error) int {
	var re *requestError
	if errors.As(err, &re) {
		return re.status
	}
	return 0
}

const maxContextChars = 24_000

func TestReference(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Antrado</title><h1>We do not claim it makes money</h1>"+strings.Repeat("<p>filler words here</p>", 3000))
	}))
	defer page.Close()

	// A bare link with nothing to read is refused.
	_, _, err := s.reference(ctx, &createRequest{Prompt: "Have a look at: https://antrado.com"}, settings.Settings{}, "test", maxContextChars)
	if status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "cannot open web pages") {
		t.Fatalf("bare link: %v", err)
	}
	// Supplying the content is enough.
	ref, _, err := s.reference(ctx, &createRequest{Prompt: "Review https://antrado.com", Context: "page text"}, settings.Settings{}, "test", maxContextChars)
	if err != nil || ref != "page text" {
		t.Fatalf("with context: %q %v", ref, err)
	}
	// The test server is on loopback: blocked unless internal fetching is allowed.
	_, _, err = s.reference(ctx, &createRequest{Prompt: "Review " + page.URL, FetchURLs: true}, settings.Settings{}, "test", maxContextChars)
	if status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("internal fetch should be forbidden by default: %v", err)
	}
	ref, fetched, err := s.reference(ctx, &createRequest{Prompt: "Review " + page.URL, FetchURLs: true, Context: "BRAND RULES: no testimonials"}, settings.Settings{AllowInternalFetch: true}, "test", maxContextChars)
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || fetched[0].Title != "Antrado" || !fetched[0].Truncated {
		t.Fatalf("fetched = %+v", fetched)
	}
	for _, want := range []string{"=== PAGES LINKED IN THE TASK", "=== GUIDELINES AND FACTS TO RESPECT", "## Page fetched from " + page.URL, "# We do not claim it makes money", "[page truncated", "BRAND RULES: no testimonials"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("reference missing %q", want)
		}
	}
	if len(ref) > maxContextChars+200 {
		t.Fatalf("reference is %d chars, over the cap", len(ref))
	}
	// Oversized context is refused rather than silently cut.
	_, _, err = s.reference(ctx, &createRequest{Prompt: "x", Context: strings.Repeat("a", maxContextChars+1)}, settings.Settings{}, "test", maxContextChars)
	if status(err) != http.StatusBadRequest {
		t.Fatalf("oversized context: %v", err)
	}
}

func TestFitToModel(t *testing.T) {
	ref := strings.Repeat("r", 18_000) // ~6k tokens of reference
	// 16k context: each agent has ~7k tokens left, so 16k-token parts shrink.
	req := &createRequest{Prompt: "review", Parts: []agent.Part{{Title: "a", Text: "x"}}}
	note, err := fitToModel(req, 16384, 0, ref)
	if err != nil || req.MaxPartTokens <= 0 || req.MaxPartTokens >= 8000 || note == "" {
		t.Fatalf("parts: max %d note %q err %v", req.MaxPartTokens, note, err)
	}
	// A 60k-character document needs more than one agent next to that reference.
	req = &createRequest{Prompt: "summarize", Document: strings.Repeat("d", 60_000), Chunks: 1}
	if note, err := fitToModel(req, 16384, 0, ref); err != nil || req.Chunks != 3 || note == "" {
		t.Fatalf("document: chunks %d note %q err %v", req.Chunks, note, err)
	}
	// Enough room: nothing changes.
	req = &createRequest{Prompt: "summarize", Document: strings.Repeat("d", 6_000), Chunks: 4}
	if note, err := fitToModel(req, 32000, 0, ref); err != nil || req.Chunks != 4 || note != "" {
		t.Fatalf("roomy: chunks %d note %q err %v", req.Chunks, note, err)
	}
	// The reference alone fills an 8k model.
	if _, err := fitToModel(&createRequest{Prompt: "x"}, 8192, 0, ref); err == nil {
		t.Fatal("a reference that leaves no room must be refused")
	}
	// Auto (chunks 0): a small document is read whole; a large one is split.
	req = &createRequest{Prompt: "explain", Document: strings.Repeat("d", 9_600)} // ~2.4k tokens
	if note, err := fitToModel(req, 32000, 0, ""); err != nil || req.Chunks != 1 || !strings.Contains(note, "read whole") {
		t.Fatalf("auto small: chunks %d note %q err %v", req.Chunks, note, err)
	}
	req = &createRequest{Prompt: "summarize", Document: strings.Repeat("d", 160_000)} // ~40k tokens
	if note, err := fitToModel(req, 32000, 0, ""); err != nil || req.Chunks != 6 || !strings.Contains(note, "automatically") {
		t.Fatalf("auto large: chunks %d note %q err %v", req.Chunks, note, err)
	}
	// An explicit count is kept.
	req = &createRequest{Prompt: "explain", Document: strings.Repeat("d", 9_600), Chunks: 3}
	if note, err := fitToModel(req, 32000, 0, ""); err != nil || req.Chunks != 3 || note != "" {
		t.Fatalf("explicit: chunks %d note %q err %v", req.Chunks, note, err)
	}
	if got := agent.MaxReferenceChars(32000); got != 38_400 {
		t.Fatalf("MaxReferenceChars(32000) = %d", got)
	}
}
