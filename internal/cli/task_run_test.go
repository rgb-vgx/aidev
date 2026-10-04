package cli

import (
	"testing"
	"time"

	"aidev/internal/config"
	"aidev/internal/task"
)

// The deadline a run puts on itself: the agent's timeout, the verification
// budget, and the margin for the work around them — with the verification
// budget counted twice when the run also verifies the base first. This sum is
// what keeps invariant 6 true for a child no one waits on (research C2).
func TestRunTotalBudget(t *testing.T) {
	cfg := config.Config{
		DefaultTaskTimeout:       30 * time.Minute,
		VerificationTotalTimeout: 10 * time.Minute,
	}
	tests := []struct {
		name string
		tk   task.Task
		want time.Duration
	}{
		{"the task's default plus verification plus margin", task.Task{}, 50 * time.Minute},
		{"the task's own timeout replaces the default", task.Task{Timeout: 5 * time.Minute}, 25 * time.Minute},
		{"expect_fail_on_base verifies the base too", task.Task{ExpectFailOnBase: true}, 60 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runTotalBudget(cfg, tc.tk); got != tc.want {
				t.Errorf("runTotalBudget = %v, want %v", got, tc.want)
			}
		})
	}
}
