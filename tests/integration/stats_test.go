package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"aidev/internal/config"
	"aidev/internal/worker"
)

// The last piece of the architecture is learning from what has already run. aidev
// records the model and the outcome of every attempt; stats is what turns that into
// an answer to the question routing needs: is this model getting this kind of work
// done, or is it producing code that fails verification?
//
// The distinction between a failed verification and an agent error is the whole
// point. A model that fails verification wrote the wrong code; a model that errors
// never ran. Averaging them into one "failure rate" would hide the difference that
// decides whether to route differently or to fix the plumbing.
func TestStatsReportsOutcomesPerModelAndHardness(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Routing = map[string]string{"HARD": "strong/model", "TRIVIAL": "cheap/model"}
	})

	// Two hard runs on the routed model: one finishes the work, one does not and so
	// fails aidev's own verification.
	h.backend.Work = doTheWork
	hard1 := h.createTask(func(in *worker.CreateTaskInput) { in.Hardness = "hard" })
	if _, err := h.orchestrator.RunTask(h.ctx, hard1.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	h.backend.Work = nil // the agent claims success without writing marker.txt
	hard2 := h.createTask(func(in *worker.CreateTaskInput) { in.Hardness = "hard" })
	if _, err := h.orchestrator.RunTask(h.ctx, hard2.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	// One trivial run on the cheap model, which succeeds.
	h.backend.Work = doTheWork
	triv := h.createTask(func(in *worker.CreateTaskInput) { in.Hardness = "trivial" })
	if _, err := h.orchestrator.RunTask(h.ctx, triv.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	stdout, stderr, err := h.runCLI(t, "stats", "--json")
	if err != nil {
		t.Fatalf("aidev stats --json: %v\nstderr: %s", err, stderr)
	}
	var report struct {
		Rows []struct {
			Model        string         `json:"model"`
			Hardness     string         `json:"hardness"`
			Runs         int            `json:"runs"`
			Succeeded    int            `json:"succeeded"`
			Failed       int            `json:"failed"`
			FailureKinds map[string]int `json:"failure_kinds"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stats --json is not JSON: %v\n%s", err, stdout)
	}

	byKey := map[string]int{}
	for i, r := range report.Rows {
		byKey[r.Model+"/"+r.Hardness] = i
	}
	i, ok := byKey["strong/model/HARD"]
	if !ok {
		t.Fatalf("no row for the model that ran the hard tasks:\n%s", stdout)
	}
	row := report.Rows[i]
	if row.Runs != 2 || row.Succeeded != 1 || row.Failed != 1 {
		t.Errorf("hard row = %+v, want 2 runs, 1 succeeded, 1 failed", row)
	}
	if row.FailureKinds["VERIFICATION"] != 1 {
		t.Errorf("failure kinds = %v, want the failure attributed to verification, "+
			"which is what says the model wrote code that does not pass", row.FailureKinds)
	}

	j, ok := byKey["cheap/model/TRIVIAL"]
	if !ok {
		t.Fatalf("no row for the model that ran the trivial task:\n%s", stdout)
	}
	if r := report.Rows[j]; r.Runs != 1 || r.Succeeded != 1 {
		t.Errorf("trivial row = %+v, want 1 run, 1 succeeded", r)
	}
}

// The default output is what a person reads, so it names the models and is stable
// enough to scan: one line per model and hardness, in a fixed order.
func TestStatsPrintsATableAPersonCanRead(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Routing = map[string]string{"HARD": "strong/model"}
	})
	h.backend.Work = doTheWork
	created := h.createTask(func(in *worker.CreateTaskInput) { in.Hardness = "hard" })
	if _, err := h.orchestrator.RunTask(h.ctx, created.Ref); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	stdout, stderr, err := h.runCLI(t, "stats")
	if err != nil {
		t.Fatalf("aidev stats: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{"strong/model", "HARD", "model", "runs"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stats output does not mention %q:\n%s", want, stdout)
		}
	}
}

// With nothing recorded, stats says so rather than printing an empty table that
// reads as "everything failed".
func TestStatsWithNoHistorySaysSo(t *testing.T) {
	h := newHarness(t, nil)
	stdout, _, err := h.runCLI(t, "stats")
	if err != nil {
		t.Fatalf("aidev stats: %v", err)
	}
	if !strings.Contains(strings.ToLower(stdout), "no") {
		t.Errorf("stats with no history printed %q, want it to say there is nothing recorded yet", stdout)
	}
}
