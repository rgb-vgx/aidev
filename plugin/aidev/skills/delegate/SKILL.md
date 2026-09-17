---
name: delegate
description: Hand implementation work in a git repository to aidev instead of writing the code yourself. Use when the user asks to delegate, to "give it to aidev", or to build, fix or change something in a repository where the aidev MCP tools (aidev_create_task, aidev_run_task) are available. Covers deciding what "done" means, splitting the work into small tasks, writing verification commands, running the tasks and recovering when one fails.
---

# Delegate work to aidev

aidev runs a coding agent in its own git worktree, then runs the task's verification
commands itself. Only those commands decide whether a task succeeded; what the agent
says about its work counts for nothing. Your job is everything the commands cannot do:
decide what "done" means, make it checkable, keep each task small, and judge the
result afterwards (the `review` skill).

## Talk to the user in their terms

Work out from how the user writes whether they read code.

- **Developer**: show verification commands, branch names and diffs as they are.
- **Not a developer**: say what will change and how it will be checked in plain words.
  Never ask them to read a diff or a command. Translate every choice you need from
  them into a question about behaviour ("Should a wrong password show a message, or
  just clear the field?").

If you cannot tell, ask once which they prefer.

## 1. Decide what "done" means, and confirm it

Before any task exists, write down in one to five plain sentences what will be true
when the work is finished, including what must keep working. Show it to the user and
get a yes, unless they already stated it that precisely. This is the step that
catches a misunderstanding cheaply.

## 2. Check the repository

- It must be a git repository with at least one commit. aidev never touches the
  user's working tree; the task branches from `base_ref`.
- Find how the project is tested (Makefile, package.json scripts, `go test`,
  pytest ...). Run the suite once so you know it is green before anything changes.

## 3. Make "done" checkable, test first

A verification command must fail now and pass once the work is done. A command that
already passes proves nothing.

1. Create a branch `spec/<short-name>` from the user's current branch.
2. Write the tests that express step 1. Run them and confirm they fail **for the
   right reason** (an assertion about the missing behaviour, not a compile error in
   your own test or a missing fixture).
3. Commit them on the spec branch. The user's branch never carries failing tests.
4. Use the spec branch as the task's `base_ref`.

If the change genuinely cannot be tested (pure copy edits, for example), use the
strongest command available (build, lint, an existing test suite, a `grep` for the
required text) and tell the user what is not being checked.

Verification commands run **without a shell**: no pipes, `&&`, redirects or globs.
Quote any argument containing `|`, `?`, `*`, `&` or spaces, for example
`go test ./... -run 'A|B'`. Use `env NAME=value cmd` to set a variable. For
anything more complex, commit a script on the spec branch and run that.

## 4. Keep every task small

One concern, a handful of files, one package or module. Measured on the default
model: small tasks finish in a few minutes; large multi-step tasks repeatedly ended
their turn early ("Now I will ...") with most of the work undone. If a change needs
more than about five distinct steps, split it into several tasks, each on its own
spec branch, and run them one after another, each based on the previous result.

## 5. Write the task

Call `aidev_create_task` with:

- `repo_path`: absolute path of the repository.
- `title`: one line.
- `description`: exactly what to change and where (files, functions), which tests
  specify it (name the spec commit), and the constraints. Always include:
  - "Do not modify any test file; the tests on this branch are the specification."
  - any file that must not change (applied database migrations, for example);
  - "Do not end your turn with a statement of what you will do next: make the
    change, run the verification commands, and keep going until they pass."
- `acceptance_criteria`: the plain sentences from step 1.
- `verification`: the commands from step 3. Include the whole relevant suite, not
  only the new tests, so nothing else breaks unnoticed.
- `base_ref`: the spec branch.
- `hardness`: `TRIVIAL`, `STANDARD` or `HARD` when it helps pick a model.

## 6. Run it and wait

Call `aidev_run_task`. It returns after a bounded wait; if `still_running` is true,
poll `aidev_get_task_result` (every minute or two) or follow progress with
`aidev_get_task_events`. Run one task at a time unless you know the project's tests
can run concurrently. Tell the user briefly what is running and roughly how long
tasks of this size take.

## 7. When it fails

Read `aidev_get_task_result` (with `include_logs` if the summary is not enough) and
find which it was:

- **The agent stopped early** (few or no files changed, a final message announcing
  more work): create a new task with the same spec, a smaller scope, and the
  sentence about not ending the turn early stated first. A failed task is final; do
  not try to run it again.
- **The specification was wrong** (a test cannot pass as written, or forces the
  agent to edit a test): fix the spec branch yourself, commit, and create a new task.
  Say so to the user; it is your mistake, not the agent's.
- **The work is mostly done**: the failed worktree is kept (see the result). You may
  finish or carry the partial work yourself, but check every line of it as if you
  wrote it, and never carry over an edit to a test file.
- **The environment is broken** (database down, invalid API key, agent missing):
  fix that first; retrying will fail the same way.

## 8. When it succeeds

A success leaves a commit on `aidev/<ref>` and nothing else. Use the `review` skill
before anything reaches the user's branch.

## Never

- Merge, push or delete a branch without the review, and never push unless asked.
- Treat the agent's own summary as evidence.
- Let a task edit its own tests to get to green.
