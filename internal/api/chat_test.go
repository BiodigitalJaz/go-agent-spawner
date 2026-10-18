package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

func msg(role, content, status string) store.ChatMessage {
	m := store.ChatMessage{ID: uuid.New(), Role: role, Content: content, Status: status}
	return m
}

func TestChatHistory(t *testing.T) {
	msgs := []store.ChatMessage{
		msg("user", "first question", ""), msg("assistant", "first answer", "done"),
		msg("user", "this one failed", ""), msg("assistant", "", "failed"),
		msg("user", "second question", ""), msg("assistant", strings.Repeat("long ", 3000), "done"),
		msg("user", "third question", ""), msg("assistant", "third answer", "done"),
	}
	turns, dropped := chatHistory(msgs, 1_000_000)
	if dropped != 0 || len(turns) != 6 || turns[0].Content != "first question" || turns[5].Content != "third answer" {
		t.Fatalf("turns %d dropped %d: %+v", len(turns), dropped, turns)
	}
	if !strings.Contains(turns[3].Content, "part of this earlier reply was left out") || len(turns[3].Content) > maxHistoryReplyChar+100 {
		t.Fatalf("long reply not shortened: %d chars", len(turns[3].Content))
	}
	for _, tu := range turns {
		if strings.Contains(tu.Content, "failed") {
			t.Fatal("a failed exchange must be left out")
		}
	}
	// A small budget keeps the newest exchanges.
	turns, dropped = chatHistory(msgs, 60)
	if len(turns) != 2 || turns[0].Content != "third question" || dropped != 4 {
		t.Fatalf("budgeted: %+v dropped %d", turns, dropped)
	}
	if turns, _ := chatHistory(nil, 100); len(turns) != 0 {
		t.Fatal("empty history")
	}
}

func TestTitleFrom(t *testing.T) {
	if got := titleFrom("  How do I\n run   an app?  "); got != "How do I run an app?" {
		t.Fatalf("%q", got)
	}
	if got := titleFrom(strings.Repeat("x", 100)); len(got) > 64 || !strings.HasSuffix(got, "…") {
		t.Fatalf("%q", got)
	}
}
