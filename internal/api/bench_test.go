package api

import (
	"encoding/json"
	"strings"
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

func TestCleanSlips(t *testing.T) {
	ref := "Resident Hunting License $25. Batteries: 2,600 mAh."
	kept, dropped := cleanSlips([]string{
		"A Resident Hunting License costs $25.",                      // no correction
		"Uses 2,600 mAh batteries -> they are 5,200 mAh",             // figure is in the reference
		"new_stack = true sets the blur stack -> not a valid option", // a real slip
	}, ref)
	if dropped != 2 || len(kept) != 1 || kept[0][:9] != "new_stack" {
		t.Fatalf("kept %v dropped %d", kept, dropped)
	}
}

func TestFigures(t *testing.T) {
	ref := "Price: $25. Batteries: 2600 mAh. Field of view 130 degrees. Operating -4°F to 122°F."
	total, uns := figures("A license costs $25. It has 2,600 mAh batteries, a 130° view, 8x zoom and lasts 3 to 6 months; it ships with a $20 permit and 1/2.9 sensor.", ref)
	// $25, 2,600, 130, 20, 1/2.9 (single digits are skipped)
	if total != 5 || len(uns) != 2 || uns[0] != "20" || uns[1] != "1/2.9" {
		t.Fatalf("total %d unsourced %v", total, uns)
	}
}

func TestAnalyze(t *testing.T) {
	no, yes := false, true
	res := benchResults{
		Judge: "a",
		Cases: []benchCase{{Prompt: benchSuite[1]}, {Prompt: "other"}},
		Models: []benchModel{
			{Name: "a", Answered: 2, Judged: 2, Parallel: 4, MedianTPS: 30, AggregateTPS: 110, MedianTTFTMs: 2000, Answers: []benchAnswer{
				{Case: 0, Status: "done", Tokens: 800, TPS: 30, Output: "costs $63.99 and has 2,600 mAh", Refused: &no, Slips: []string{"x -> y"}},
				{Case: 1, Status: "done", Tokens: 700, TPS: 29, Output: "fine", Refused: &no},
			}},
			{Name: "b", Answered: 2, Judged: 2, Parallel: 4, MedianTPS: 22, AggregateTPS: 90, MedianTTFTMs: 5000, Answers: []benchAnswer{
				{Case: 0, Status: "done", Tokens: 500, TPS: 22, Output: "costs $63.99 and has 5,200 mAh", Refused: &no},
				{Case: 1, Status: "done", Tokens: 400, TPS: 21, Output: "no", Refused: &yes},
			}},
		},
	}
	analyze(&res, []string{"Price $63.99. Battery 2600 mAh.", ""})
	if res.Cases[0].Category != "Product facts from sources" || res.Models[0].FiguresSourced != 2 || res.Models[1].FiguresSourced != 1 {
		t.Fatalf("figures: %+v %+v", res.Models[0], res.Models[1])
	}
	text := strings.Join(res.Analysis, "\n")
	for _, want := range []string{"a is fastest", "b's answers are", "a stays closer to its sources", "b refused or withheld help on: message 2", "also one of the compared models"} {
		if !strings.Contains(text, want) {
			t.Fatalf("analysis missing %q:\n%s", want, text)
		}
	}
}
