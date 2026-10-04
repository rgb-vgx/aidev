package task

import (
	"strings"
	"testing"
)

// A person writes the mode the way they say it; the stored spelling is the
// one the CHECK constraint knows. Blank must mean in_place — the behaviour
// every task had before the mode existed — so an unset column never reads
// as "verify nowhere".
func TestParseVerificationMode(t *testing.T) {
	cases := []struct {
		raw  string
		want VerificationMode
	}{
		{"", VerificationInPlace},
		{"   ", VerificationInPlace},
		{"in_place", VerificationInPlace},
		{"In-Place", VerificationInPlace},
		{" clean ", VerificationClean},
		{"CLEAN", VerificationClean},
		{"Clean", VerificationClean},
	}
	for _, tc := range cases {
		got, err := ParseVerificationMode(tc.raw)
		if err != nil {
			t.Errorf("ParseVerificationMode(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVerificationMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	if _, err := ParseVerificationMode("dirty"); err == nil {
		t.Error("an unknown mode was accepted; verification would run somewhere nobody chose")
	} else if !strings.Contains(err.Error(), "in_place, clean") {
		t.Errorf("error = %v, want it to name the valid modes", err)
	}
}

// The mode a task is created with is the mode it keeps: New stores it
// verbatim, so what the run later does is a pure function of the task row.
func TestNewFreezesTheGivenVerificationMode(t *testing.T) {
	in := validInput()
	in.VerificationMode = VerificationClean
	tk, err := New(in, "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.VerificationMode != VerificationClean {
		t.Errorf("VerificationMode = %q, want the mode the caller resolved", tk.VerificationMode)
	}

	// An unset mode is in_place, never empty: a run that reads a blank mode
	// must not have to guess where the checks go.
	tk, err = New(validInput(), "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.VerificationMode != VerificationInPlace {
		t.Errorf("VerificationMode = %q, want the in_place default", tk.VerificationMode)
	}
}

// A typo in the mode would silently change where verification runs — the
// difference between judging the agent's worktree and judging a clean
// checkout — so it is refused at creation rather than falling back quietly.
func TestNewRejectsUnknownVerificationMode(t *testing.T) {
	in := validInput()
	in.VerificationMode = VerificationMode("worktree")
	_, err := New(in, "build")
	if err == nil {
		t.Fatal("an unknown verification mode was accepted")
	}
	if !strings.Contains(err.Error(), "worktree") || !strings.Contains(err.Error(), "in_place, clean") {
		t.Errorf("error = %v, want it to name the bad value and the valid ones", err)
	}
}

// Setup steps are held to the same standard as verification — argv, no
// shell, bounded count — because they run with the same authority in the
// same place. Unlike verification they may be empty: most checkouts need
// no preparing.
func TestNewValidatesSetupSteps(t *testing.T) {
	t.Run("empty list is fine", func(t *testing.T) {
		in := validInput()
		in.SetupSteps = nil
		if _, err := New(in, "build"); err != nil {
			t.Fatalf("New: %v", err)
		}
	})

	t.Run("empty command is refused", func(t *testing.T) {
		in := validInput()
		in.SetupSteps = []VerificationStep{{Command: "  "}}
		_, err := New(in, "build")
		if err == nil {
			t.Fatal("a setup step with no command was accepted")
		}
		if !strings.Contains(err.Error(), "setup command 1") {
			t.Errorf("error = %v, want it to name the offending setup step", err)
		}
	})

	t.Run("over the count limit is refused", func(t *testing.T) {
		in := validInput()
		in.SetupSteps = make([]VerificationStep, MaxVerificationSteps+1)
		for i := range in.SetupSteps {
			in.SetupSteps[i] = VerificationStep{Command: "true"}
		}
		_, err := New(in, "build")
		if err == nil {
			t.Fatal("an unbounded setup list was accepted")
		}
		if !strings.Contains(err.Error(), "setup commands exceed the limit") {
			t.Errorf("error = %v, want it to name the setup limit", err)
		}
	})
}

// The setup list travels with the task from input to stored struct: the run
// reads it off the task row, so a step dropped here would never prepare the
// checkout the checks then judge.
func TestSetupStepsAreCarriedThrough(t *testing.T) {
	in := validInput()
	in.SetupSteps = []VerificationStep{{Command: "npm", Args: []string{"ci"}}}
	tk, err := New(in, "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(tk.SetupSteps) != 1 || tk.SetupSteps[0].Command != "npm" {
		t.Errorf("SetupSteps = %v, want the input preserved", tk.SetupSteps)
	}
}
