package task

import (
	"strings"
	"testing"
)

// Hardness is what routing reads: a task's own estimate of how much model it needs,
// stated once when the task is created and then used to pick a model without a person
// naming one every time. It is deliberately a closed, short vocabulary — a number
// would invite false precision, and a free-text label could not be routed.
func TestHardnessVocabulary(t *testing.T) {
	all := AllHardnesses()
	if len(all) < 2 {
		t.Fatalf("AllHardnesses = %v, want a vocabulary with at least two levels", all)
	}
	seen := map[Hardness]bool{}
	for _, h := range all {
		if !h.Valid() {
			t.Errorf("%q is in AllHardnesses but not Valid", h)
		}
		if seen[h] {
			t.Errorf("%q appears twice", h)
		}
		seen[h] = true
		if strings.TrimSpace(string(h)) != string(h) || string(h) == "" {
			t.Errorf("%q is not a usable identifier", h)
		}
	}
	for _, want := range []Hardness{HardnessTrivial, HardnessStandard, HardnessHard} {
		if !seen[want] {
			t.Errorf("%q is not in AllHardnesses", want)
		}
	}
	if Hardness("").Valid() {
		t.Error(`"" must not be Valid: it means the task did not state a hardness`)
	}
}

// A person types this on a command line, so it is accepted the way a person writes it
// and reported precisely when it is not one of the levels.
func TestParseHardness(t *testing.T) {
	for _, raw := range []string{"hard", "HARD", "  Hard  "} {
		got, err := ParseHardness(raw)
		if err != nil {
			t.Errorf("ParseHardness(%q): %v", raw, err)
			continue
		}
		if got != HardnessHard {
			t.Errorf("ParseHardness(%q) = %q, want %q", raw, got, HardnessHard)
		}
	}
	if _, err := ParseHardness("quite hard"); err == nil {
		t.Error("an invented hardness was accepted")
	} else if !strings.Contains(err.Error(), "quite hard") {
		t.Errorf("error = %v, want it to quote what was given", err)
	}
	// Absent is not an error: a task need not state a hardness.
	got, err := ParseHardness("  ")
	if err != nil || got != "" {
		t.Errorf("ParseHardness(blank) = (%q, %v), want (\"\", nil)", got, err)
	}
}

// A task remembers its hardness, so routing and later analysis read the same value.
func TestNewTaskKeepsHardness(t *testing.T) {
	in := validInput()
	in.Hardness = "  hard  "
	tk, err := New(in, "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.Hardness != HardnessHard {
		t.Errorf("hardness = %q, want it parsed and kept", tk.Hardness)
	}

	in = validInput()
	in.Hardness = "impossible"
	if _, err := New(in, "build"); err == nil {
		t.Fatal("an invented hardness was accepted by New")
	}

	plain, err := New(validInput(), "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if plain.Hardness != "" {
		t.Errorf("hardness = %q, want empty when the task did not state one", plain.Hardness)
	}
}
