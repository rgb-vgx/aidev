package task

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validInput() NewTaskInput {
	return NewTaskInput{
		ProjectID:    uuid.Must(uuid.NewV7()),
		Title:        "Add a Greet function",
		Verification: []VerificationStep{{Command: "go", Args: []string{"test", "./..."}}},
	}
}

func TestNewTaskDefaults(t *testing.T) {
	tk, err := New(validInput(), "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.Status != StatusPending {
		t.Errorf("status = %s, want PENDING: a new task is not yet eligible to run", tk.Status)
	}
	if tk.Agent != "build" {
		t.Errorf("agent = %q, want the configured default", tk.Agent)
	}
	if tk.ID == uuid.Nil {
		t.Error("New did not assign an id")
	}
	if tk.Ref != "" {
		t.Errorf("ref = %q, want empty: the database assigns it", tk.Ref)
	}
	if tk.CreatedAt.IsZero() || tk.UpdatedAt.IsZero() {
		t.Error("New did not stamp timestamps")
	}
}

func TestNewTaskTrimsTitle(t *testing.T) {
	in := validInput()
	in.Title = "  Add a Greet function\t"
	tk, err := New(in, "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.Title != "Add a Greet function" {
		t.Errorf("title = %q, want it trimmed", tk.Title)
	}
}

// Verification is mandatory. This is the "never trust the agent" rule expressed
// as a constraint on creation rather than as advice in a document.
func TestVerificationIsMandatory(t *testing.T) {
	in := validInput()
	in.Verification = nil

	_, err := New(in, "build")
	if err == nil {
		t.Fatal("a task with no verification command was accepted")
	}
	if !strings.Contains(err.Error(), "verification command is required") {
		t.Errorf("error should explain why verification is required, got: %v", err)
	}
}

func TestNewTaskRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*NewTaskInput)
		want   string
	}{
		{"no project", func(i *NewTaskInput) { i.ProjectID = uuid.Nil }, "project is required"},
		{"blank title", func(i *NewTaskInput) { i.Title = "   " }, "title is required"},
		{"long title", func(i *NewTaskInput) { i.Title = strings.Repeat("x", MaxTitleLength+1) }, "limit is 200"},
		{"long description", func(i *NewTaskInput) { i.Description = strings.Repeat("x", MaxDescriptionLength+1) }, "description is"},
		{"agent with spaces", func(i *NewTaskInput) { i.Agent = "my agent" }, "must not contain whitespace"},
		{"too many steps", func(i *NewTaskInput) {
			i.Verification = make([]VerificationStep, MaxVerificationSteps+1)
			for j := range i.Verification {
				i.Verification[j] = VerificationStep{Command: "true"}
			}
		}, "exceed the limit"},
		{"empty step command", func(i *NewTaskInput) {
			i.Verification = []VerificationStep{{Command: "  "}}
		}, "empty command"},
		{"priority too high", func(i *NewTaskInput) { i.Priority = MaxPriority + 1 }, "priority"},
		{"negative retries", func(i *NewTaskInput) { i.MaxRetries = -1 }, "max retries"},
		{"too many retries", func(i *NewTaskInput) { i.MaxRetries = MaxRetriesLimit + 1 }, "max retries"},
		{"negative timeout", func(i *NewTaskInput) { i.Timeout = -time.Second }, "timeout"},
		{"base ref with spaces", func(i *NewTaskInput) { i.BaseRef = "my branch" }, "base ref"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			_, err := New(in, "build")
			if err == nil {
				t.Fatalf("expected rejection mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewTaskReportsEveryProblemAtOnce(t *testing.T) {
	in := NewTaskInput{} // no project, no title, no verification
	_, err := New(in, "")
	if err == nil {
		t.Fatal("expected rejection")
	}
	var ve *ValidationError
	if !asValidation(err, &ve) {
		t.Fatalf("want *ValidationError, got %T", err)
	}
	if len(ve.Problems) < 3 {
		t.Errorf("got %d problems (%v), want every problem reported in one pass", len(ve.Problems), ve.Problems)
	}
}

func TestAgentFallsBackToDefaultOnly(t *testing.T) {
	in := validInput()
	in.Agent = ""
	if _, err := New(in, ""); err == nil {
		t.Fatal("with no agent and no default, creation must fail rather than guess")
	}
}

func TestEffectiveTimeout(t *testing.T) {
	def := 30 * time.Minute
	if got := (Task{}).EffectiveTimeout(def); got != def {
		t.Errorf("zero timeout = %s, want the default %s", got, def)
	}
	if got := (Task{Timeout: time.Minute}).EffectiveTimeout(def); got != time.Minute {
		t.Errorf("explicit timeout = %s, want 1m", got)
	}
}

func TestIdentifierPrefersRef(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	if got := (Task{ID: id, Ref: "TASK-000007"}).Identifier(); got != "TASK-000007" {
		t.Errorf("Identifier() = %q, want the human reference", got)
	}
	if got := (Task{ID: id}).Identifier(); got != id.String() {
		t.Errorf("Identifier() = %q, want the uuid when no ref is assigned yet", got)
	}
}

func TestFailureKindValidity(t *testing.T) {
	if !FailureNone.Valid() {
		t.Error("FailureNone must be valid: it means no failure")
	}
	for _, k := range AllFailureKinds() {
		if !k.Valid() {
			t.Errorf("%s is listed but reports itself invalid", k)
		}
	}
	if FailureKind("MADE_UP").Valid() {
		t.Error("an unknown failure kind reported itself valid")
	}
}

func TestAttemptLifecycle(t *testing.T) {
	taskID := uuid.Must(uuid.NewV7())
	a := NewAttempt(taskID, 1)
	if a.Status != AttemptRunning {
		t.Errorf("status = %s, want RUNNING", a.Status)
	}
	if a.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", a.AttemptNumber)
	}
	if a.FinishedAt != nil {
		t.Error("a new attempt must not be finished")
	}
	if a.Duration() <= 0 {
		t.Error("Duration on a running attempt should report elapsed time")
	}

	finished := a.StartedAt.Add(2 * time.Second)
	a.FinishedAt = &finished
	if got := a.Duration(); got != 2*time.Second {
		t.Errorf("Duration() = %s, want 2s", got)
	}
}

func TestParseApprovalStatus(t *testing.T) {
	if _, err := ParseApprovalStatus("GRANTED"); err != nil {
		t.Errorf("GRANTED should parse: %v", err)
	}
	if _, err := ParseApprovalStatus("MAYBE"); err == nil {
		t.Error("MAYBE should not parse")
	}
}

func asValidation(err error, target **ValidationError) bool {
	v, ok := err.(*ValidationError)
	if ok {
		*target = v
	}
	return ok
}

// A task carries the model it should run on, so that a hard task can be routed to a
// stronger model than a trivial one, and so that a model that is down can be worked
// around per task instead of by editing the configuration for everything. An empty
// model means "whatever the configuration says", the same contract the agent name and
// the timeout already have.
func TestModelIsOptionalAndTrimmed(t *testing.T) {
	in := validInput()
	in.Model = "  opencode/mimo-v2.5-free  "
	tk, err := New(in, "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tk.Model != "opencode/mimo-v2.5-free" {
		t.Errorf("model = %q, want it trimmed", tk.Model)
	}

	plain, err := New(validInput(), "build")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if plain.Model != "" {
		t.Errorf("model = %q, want empty when the task did not ask for one", plain.Model)
	}
}

// Unlike the agent name, a missing model is not an error anywhere: an empty
// configured model means "let the backend choose", which is a supported setting.
func TestEffectiveModel(t *testing.T) {
	if got := (Task{}).EffectiveModel("cfg/model"); got != "cfg/model" {
		t.Errorf("no task model = %q, want the configured one", got)
	}
	if got := (Task{Model: "task/model"}).EffectiveModel("cfg/model"); got != "task/model" {
		t.Errorf("task model = %q, want the task's own to win", got)
	}
	if got := (Task{}).EffectiveModel(""); got != "" {
		t.Errorf("nothing configured = %q, want empty so the backend chooses", got)
	}
}

// A model name is passed to the backend as one argument, so whitespace inside it is
// never a model: it is a typo, and it must be refused where the other input is.
func TestModelWithWhitespaceIsRejected(t *testing.T) {
	in := validInput()
	in.Model = "opencode/two words"
	if _, err := New(in, "build"); err == nil {
		t.Fatal("a model name containing a space was accepted")
	}
}
