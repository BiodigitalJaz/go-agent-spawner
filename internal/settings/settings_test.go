package settings

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	known := func(m string) bool { return m == "a" || m == "b" }
	ok := Defaults("a", "b", 2, 6)
	if err := ok.Validate(known); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}

	bad := ok
	bad.Worker.Model = "nope"
	bad.Planner.Temperature = ptr(3.0)
	bad.Reducer.Directive = "  "
	bad.MaxChildren = 1
	bad.Merge = "magic"
	err := bad.Validate(known)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{`worker: unknown model "nope"`, "planner: temperature", "reducer: directive is required", "max_children", "merge must be"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
