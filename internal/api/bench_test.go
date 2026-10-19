package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestJSONObject(t *testing.T) {
	for _, in := range []string{
		`{"refused": false, "unsupported": [], "slips": ["a -> b"]}`,
		"Here is the grade:\n```json\n{\"refused\": false, \"unsupported\": [], \"slips\": [\"a -> b\"]}\n```",
	} {
		var g struct {
			Refused bool     `json:"refused"`
			Slips   []string `json:"slips"`
		}
		if err := json.Unmarshal([]byte(jsonObject(in)), &g); err != nil || len(g.Slips) != 1 {
			t.Fatalf("%q: %v %+v", in, err, g)
		}
	}
	if jsonObject("no json") != "no json" {
		t.Fatal("text without an object is returned as is")
	}
}

func TestMedian(t *testing.T) {
	if median(nil) != 0 || median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 2, 3}) != 2.5 {
		t.Fatal("median")
	}
}

func TestPeakOverlap(t *testing.T) {
	t0 := time.Now()
	at := func(endSec, durSec int) benchAnswer {
		return benchAnswer{FinishedAt: t0.Add(time.Duration(endSec) * time.Second), DurationMs: int64(durSec * 1000)}
	}
	// Served one at a time: 14 s each, back to back.
	if p := peakOverlap([]benchAnswer{at(14, 14), at(28, 14), at(42, 14), at(56, 14)}); p != 1 {
		t.Fatalf("sequential peak = %d", p)
	}
	// Four together.
	if p := peakOverlap([]benchAnswer{at(24, 24), at(25, 25), at(26, 26), at(31, 31)}); p != 4 {
		t.Fatalf("parallel peak = %d", p)
	}
}

func TestDropSourced(t *testing.T) {
	ref := "Price: $63.99–$73.99. Field of view 130°."
	kept, dropped := dropSourced([]string{"listed price of $63.99–$73.99", "8x digital zoom", "Bluetooth travel mode"}, ref)
	if dropped != 1 || len(kept) != 2 {
		t.Fatalf("kept %v dropped %d", kept, dropped)
	}
	if kept, dropped := dropSourced([]string{"$20 permit"}, ""); dropped != 0 || len(kept) != 1 {
		t.Fatal("without a reference nothing is dropped")
	}
}
