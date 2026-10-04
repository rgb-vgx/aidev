// Package prompts holds the instruction templates aidev sends to an agent.
//
// They live in files rather than in string literals so that the wording can be
// read and reviewed as prose, which is what it is: the wording materially affects
// what an agent does, and it deserves the same scrutiny as code.
package prompts

import "embed"

// FS holds every prompt template.
//
//go:embed *.tmpl
var FS embed.FS

// ImplementTask is the template used for an implementation task.
const ImplementTask = "implement_task.tmpl"

// RetryTask is the template for an automatic retry: why the previous attempt
// failed and what to do next, sent into the same agent session when it can be
// continued, after the full task when it cannot.
const RetryTask = "retry_task.tmpl"
