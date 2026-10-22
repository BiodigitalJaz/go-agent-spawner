package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

func TestReferenceWithDocuments(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	docs := []store.ChatDocument{{Name: "regs.pdf", Text: "Archery season opens Oct 1."}, {Name: "notes.md", Text: "Bring a crossbow."}}
	ref, _, err := s.reference(context.Background(), &createRequest{Prompt: "What opens Oct 1?", Context: "be brief", documents: docs}, settings.Settings{}, "test", maxContextChars)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{agent.DocumentsMarker, "## Document: regs.pdf", "Archery season opens Oct 1.", "## Document: notes.md", "be brief"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("reference missing %q:\n%s", want, ref)
		}
	}
	if !strings.HasPrefix(ref, agent.DocumentsMarker) {
		t.Fatal("the user's documents come first")
	}
}

func TestDocumentHelpers(t *testing.T) {
	for in, want := range map[string]string{`C:\Users\me\Report Q3.pdf`: "Report Q3.pdf", "../../etc/passwd": "passwd", " notes.md ": "notes.md", "": "document"} {
		if got := documentName(in); got != want {
			t.Errorf("documentName(%q) = %q, want %q", in, got, want)
		}
	}
	h := []settings.Turn{{Role: "user", Content: "first"}, {Role: "assistant", Content: strings.Repeat("x", 1000)}, {Role: "user", Content: "second"}, {Role: "assistant", Content: "ok"}, {Role: "user", Content: "third"}, {Role: "assistant", Content: "fine"}}
	got := recentTurns(h, 2, 600)
	if strings.Contains(got, "first") || !strings.Contains(got, "User: second") || !strings.Contains(got, "Assistant: fine") {
		t.Fatalf("recentTurns:\n%s", got)
	}
}
