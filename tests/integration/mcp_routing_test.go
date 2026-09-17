package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"aidev/internal/config"
)

// Claude Code creates tasks through MCP, not the CLI. If aidev_create_task cannot
// state a hardness or a model, agent.routing is unreachable for the planner that
// aidev exists to serve, and every MCP task runs on the default model.
func TestMCPCreateTaskTakesHardnessAndModel(t *testing.T) {
	m := newMCPHarness(t)

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"verification": []string{"test -f marker.txt"},
		"hardness":     "hard",
		"model":        "opencode/mimo-v2.5-free",
	}, &created)
	if created.Task["hardness"] != "HARD" {
		t.Errorf("hardness = %v, want HARD, parsed the way the CLI parses it", created.Task["hardness"])
	}
	if created.Task["model"] != "opencode/mimo-v2.5-free" {
		t.Errorf("model = %v, want the one asked for", created.Task["model"])
	}

	// What was returned is what was stored.
	var got struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_get_task", map[string]any{"task": created.Task["ref"]}, &got)
	if got.Task["hardness"] != "HARD" || got.Task["model"] != "opencode/mimo-v2.5-free" {
		t.Errorf("get_task shows hardness %v, model %v; want what create_task stored", got.Task["hardness"], got.Task["model"])
	}
}

// A hardness that is not a level is refused, and the refusal names the levels:
// the planner has to be able to correct itself from the message alone.
func TestMCPCreateTaskRejectsAnUnknownHardness(t *testing.T) {
	m := newMCPHarness(t)

	msg := m.callExpectingError(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "x",
		"verification": []string{"true"},
		"hardness":     "impossible",
	})
	for _, want := range []string{"impossible", "TRIVIAL", "STANDARD", "HARD"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// The schema is how the planner learns the field exists and what it accepts.
func TestMCPCreateTaskSchemaDescribesHardnessAndModel(t *testing.T) {
	m := newMCPHarness(t)

	res, err := m.session.ListTools(m.ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "aidev_create_task" {
			continue
		}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatalf("decode schema: %v\n%s", err, encoded)
		}
		hardness, ok := schema.Properties["hardness"]
		if !ok {
			t.Fatalf("aidev_create_task has no hardness property:\n%s", encoded)
		}
		for _, want := range []string{"TRIVIAL", "STANDARD", "HARD", "agent.routing"} {
			if !strings.Contains(hardness.Description, want) {
				t.Errorf("hardness description %q does not mention %q", hardness.Description, want)
			}
		}
		if _, ok := schema.Properties["model"]; !ok {
			t.Errorf("aidev_create_task has no model property:\n%s", encoded)
		}
		return
	}
	t.Fatal("aidev_create_task is not exposed")
}

// End to end: a task created over MCP with a hardness runs on the model
// agent.routing names for it.
func TestMCPTaskRunsOnTheRoutedModel(t *testing.T) {
	m := newMCPHarnessWith(t, func(cfg *config.Config) {
		cfg.Routing = map[string]string{"HARD": "strong/model"}
	})
	m.backend.Work = doTheWork

	var created struct {
		Task map[string]any `json:"task"`
	}
	m.call(t, "aidev_create_task", map[string]any{
		"repo_path":    m.repoPath,
		"title":        "Create the marker file",
		"verification": []string{"test -f marker.txt"},
		"hardness":     "HARD",
	}, &created)

	m.call(t, "aidev_run_task", map[string]any{"task": created.Task["ref"]}, nil)

	call, ok := m.backend.LastCall()
	if !ok {
		t.Fatal("the backend was never called")
	}
	if call.Model != "strong/model" {
		t.Errorf("the agent ran on %q, want the model agent.routing names for HARD", call.Model)
	}
}
