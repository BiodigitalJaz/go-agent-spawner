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

func TestReference(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Antrado</title><h1>We do not claim it makes money</h1>"+strings.Repeat("<p>filler words here</p>", 3000))
	}))
	defer page.Close()

	// A bare link with nothing to read is refused.
	_, _, err := s.reference(ctx, &createRequest{Prompt: "Have a look at: https://antrado.com"}, settings.Settings{}, "test")
	if status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "cannot open web pages") {
		t.Fatalf("bare link: %v", err)
	}
	// Supplying the content is enough.
	ref, _, err := s.reference(ctx, &createRequest{Prompt: "Review https://antrado.com", Context: "page text"}, settings.Settings{}, "test")
	if err != nil || ref != "page text" {
		t.Fatalf("with context: %q %v", ref, err)
	}
	// The test server is on loopback: blocked unless internal fetching is allowed.
	_, _, err = s.reference(ctx, &createRequest{Prompt: "Review " + page.URL, FetchURLs: true}, settings.Settings{}, "test")
	if status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("internal fetch should be forbidden by default: %v", err)
	}
	ref, fetched, err := s.reference(ctx, &createRequest{Prompt: "Review " + page.URL, FetchURLs: true, Context: "BRAND RULES: no testimonials"}, settings.Settings{AllowInternalFetch: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || fetched[0].Title != "Antrado" || !fetched[0].Truncated {
		t.Fatalf("fetched = %+v", fetched)
	}
	for _, want := range []string{"## Page fetched from " + page.URL, "# We do not claim it makes money", "[page truncated", "BRAND RULES: no testimonials"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("reference missing %q", want)
		}
	}
	if len(ref) > maxContextChars+200 {
		t.Fatalf("reference is %d chars, over the cap", len(ref))
	}
	// Oversized context is refused rather than silently cut.
	_, _, err = s.reference(ctx, &createRequest{Prompt: "x", Context: strings.Repeat("a", maxContextChars+1)}, settings.Settings{}, "test")
	if status(err) != http.StatusBadRequest {
		t.Fatalf("oversized context: %v", err)
	}
}
