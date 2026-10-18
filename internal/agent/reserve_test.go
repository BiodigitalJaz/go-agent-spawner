package agent

import (
	"testing"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
	"github.com/google/uuid"
)

func TestPartOutputFollowsPartMaxWords(t *testing.T) {
	parent := uuid.New()
	part := &store.Node{ParentID: &parent}
	root := &store.Node{}
	set := &settings.Settings{PartMaxWords: 60}
	if got := partOutput(set, part, 1024); got != wordsReserve(60) || got >= 300 {
		t.Fatalf("60-word parts reserve %d tokens", got)
	}
	if got := partOutput(set, root, 1024); got != 1024 {
		t.Fatalf("the root (final answer) keeps its reserve, got %d", got)
	}
	if got := partOutput(&settings.Settings{}, part, 1024); got != 1024 {
		t.Fatalf("without part_max_words the reserve is unchanged, got %d", got)
	}
	if got := partOutput(&settings.Settings{PartMaxWords: 5000}, part, 1024); got != 1024 {
		t.Fatalf("never more than the role's reserve, got %d", got)
	}
}

func TestLinkedPages(t *testing.T) {
	ref := LinkedPagesMarker + " (fetched…) ===\n\n## Page fetched from https://antrado.com/terms.html\nTitle: Terms\n\ntext\n\n## Page fetched from https://antrado.com/\n\n=== GUIDELINES AND FACTS TO RESPECT ===\n## Page fetched from https://not-linked.example\n"
	got := linkedPages(ref)
	if len(got) != 2 || got[0] != "https://antrado.com/terms.html" || got[1] != "https://antrado.com/" {
		t.Fatalf("linked = %v", got)
	}
	if linkedPages("no linked pages here") != nil {
		t.Fatal("expected none")
	}
}
