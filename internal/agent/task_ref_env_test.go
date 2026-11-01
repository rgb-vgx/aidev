package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The agent runs the task's checks while it works, so it needs the same
// AIDEV_TASK_REF the verification commands get (tests/integration/
// task_ref_env_test.go); every other AIDEV_ variable stays out of its
// environment.
func TestOpenCodeRunSeesTheTaskReference(t *testing.T) {
	t.Setenv("AIDEV_CONFIG", "/secret/conf.json")
	fake := newIsolationFake(t, `[]`, 0)
	req := openCodeRequest(t)
	if _, err := NewOpenCode(fake.command, "").Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(fake.envFile)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if !strings.Contains(env, "\nAIDEV_TASK_REF="+req.TaskRef+"\n") && !strings.HasPrefix(env, "AIDEV_TASK_REF="+req.TaskRef+"\n") {
		t.Errorf("the agent's environment lacks AIDEV_TASK_REF=%s", req.TaskRef)
	}
	if strings.Contains(env, "AIDEV_CONFIG=") {
		t.Error("AIDEV_CONFIG reached the agent")
	}
}
