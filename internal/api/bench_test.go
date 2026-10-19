package api

import (
	"encoding/json"
	"testing"
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
