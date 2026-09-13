package worker

import (
	"fmt"
	"strings"
	"text/template"

	"aidev/internal/task"
	"aidev/prompts"
)

// promptData is what the implementation template is rendered with.
type promptData struct {
	Ref                string
	Title              string
	Description        string
	AcceptanceCriteria string

	// Verification is the rendered command lines. The agent is told exactly
	// which commands will judge it, because a check it cannot see is a check it
	// cannot satisfy.
	Verification []string
}

// buildPrompt renders the instruction sent to the agent for a task.
func buildPrompt(t task.Task) (string, error) {
	tmpl, err := template.New(prompts.ImplementTask).
		Option("missingkey=error").
		ParseFS(prompts.FS, prompts.ImplementTask)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}

	data := promptData{
		Ref:                t.Identifier(),
		Title:              t.Title,
		Description:        strings.TrimSpace(t.Description),
		AcceptanceCriteria: strings.TrimSpace(t.AcceptanceCriteria),
	}
	for _, step := range t.Verification {
		data.Verification = append(data.Verification, step.String())
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render prompt for %s: %w", t.Identifier(), err)
	}

	prompt := strings.TrimSpace(out.String())
	if prompt == "" {
		return "", fmt.Errorf("rendered prompt for %s is empty", t.Identifier())
	}
	return prompt, nil
}
