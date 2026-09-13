package worker

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"aidev/internal/task"
)

func sampleTask() task.Task {
	return task.Task{
		ID:                 uuid.Must(uuid.NewV7()),
		Ref:                "TASK-000042",
		Title:              "Add a Greet function",
		Description:        "Create greet.go exposing Greet(name string) string.",
		AcceptanceCriteria: "Greet(\"world\") returns \"Hello, world\".",
		Verification: []task.VerificationStep{
			{Command: "go", Args: []string{"test", "./..."}},
			{Command: "go", Args: []string{"vet", "./..."}},
		},
	}
}

// The agent is told exactly which commands will judge it: a check it cannot see
// is a check it cannot satisfy.
func TestPromptStatesTheVerificationCommands(t *testing.T) {
	prompt, err := buildPrompt(sampleTask())
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	for _, want := range []string{"go test ./...", "go vet ./..."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q:\n%s", want, prompt)
		}
	}
}

func TestPromptIncludesTheTaskDetail(t *testing.T) {
	tk := sampleTask()
	prompt, err := buildPrompt(tk)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	for _, want := range []string{tk.Ref, tk.Title, "Create greet.go", "Hello, world"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// The prompt must tell the agent that its own claim of success has no effect, and
// that committing is not its job — otherwise it may commit, and the cleanup policy
// assumes aidev owns the commit.
//
// Assertions run against whitespace-normalised text so that reflowing a paragraph
// in the template does not break a test about its meaning.
func TestPromptSetsTheGroundRules(t *testing.T) {
	prompt, err := buildPrompt(sampleTask())
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	flat := strings.Join(strings.Fields(prompt), " ")
	for _, want := range []string{
		"isolated git worktree",
		"has no effect on the outcome",
		"only those commands do",
		"Do not run `git commit`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("prompt does not state %q:\n%s", want, prompt)
		}
	}
}

func TestPromptWorksWithoutOptionalFields(t *testing.T) {
	tk := task.Task{
		Ref:          "TASK-000001",
		Title:        "Minimal task",
		Verification: []task.VerificationStep{{Command: "true"}},
	}
	prompt, err := buildPrompt(tk)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(prompt, "Minimal task") {
		t.Errorf("prompt is missing the title:\n%s", prompt)
	}
	// No stray "Acceptance criteria" heading when there are none.
	if strings.Contains(prompt, "## Acceptance criteria") {
		t.Errorf("prompt has an empty acceptance criteria section:\n%s", prompt)
	}
}

// A task with no ref yet is identified by its uuid, so a prompt is never
// anonymous.
func TestPromptFallsBackToTheTaskID(t *testing.T) {
	tk := sampleTask()
	tk.Ref = ""
	prompt, err := buildPrompt(tk)
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(prompt, tk.ID.String()) {
		t.Errorf("prompt does not identify the task:\n%s", prompt)
	}
}

func TestWorktreeNameAndBranchName(t *testing.T) {
	tk := sampleTask()

	first := task.TaskAttempt{AttemptNumber: 1}
	if got := worktreeName(tk, first); got != "TASK-000042-a1" {
		t.Errorf("worktree name = %q", got)
	}
	// The common case keeps a clean branch name; only a retry needs a suffix.
	if got := branchName(tk, first); got != "aidev/TASK-000042" {
		t.Errorf("branch name = %q, want aidev/TASK-000042", got)
	}

	second := task.TaskAttempt{AttemptNumber: 2}
	if got := worktreeName(tk, second); got != "TASK-000042-a2" {
		t.Errorf("second worktree name = %q", got)
	}
	if got := branchName(tk, second); got != "aidev/TASK-000042-a2" {
		t.Errorf("second branch name = %q, want a distinct branch so a retry cannot collide", got)
	}
	if branchName(tk, first) == branchName(tk, second) {
		t.Error("two attempts would share a branch")
	}
	if worktreeName(tk, first) == worktreeName(tk, second) {
		t.Error("two attempts would share a worktree directory")
	}
}
