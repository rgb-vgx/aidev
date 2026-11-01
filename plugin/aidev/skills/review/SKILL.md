---
name: review
description: Review the result of an aidev task before it reaches the user's branch, then merge it and report. Use after aidev_run_task or aidev_get_task_result reports a SUCCEEDED task, or when the user asks to check, accept, apply or merge what aidev did.
---

# Review an aidev result

A SUCCEEDED task proves one thing: its verification commands passed. It does not
prove the change is right, complete, or free of shortcuts. You supply the judgment
the commands cannot. Nothing is merged until you have.

## 1. Collect the change

`aidev task diff <task>` prints the delivered change (base to head) in one go, or
the agent's undelivered change marked as such; use the git commands below to look
closer. The result names the branch in `result.worktree.branch`: `aidev/<ref>`, or
`aidev/<ref>-aN` when a retry succeeded (the delivery is then one commit on the
base; each earlier attempt's branch keeps its own unverified commit, which is not
part of what you merge). Below, `<branch>` is that
name. Compare it with the branch the task started from (its `base_ref`, usually
`spec/<name>`):

```
git log --oneline <base_ref>..<branch>
git diff --stat <base_ref> <branch>
git diff <base_ref> <branch>
```

## 2. Hard conditions: any failure means do not merge

- **No test file changed.** The tests on the base branch are the specification; an
  edit to one is the agent grading its own work. If a test really had to change,
  the specification was wrong: fix it on the spec branch and delegate again.
- **Applied database migrations are untouched**; new schema changes are new files.
- **Only files the task is about changed.** A deleted file must be one the task
  asked for.

## 3. Judgment: what the tests did not exercise

Read the whole diff, not only the summary. Start where `result.coverage` points,
when the task asked for it: changed lines no test ran are where a green
specification says nothing, and where earlier reviews found the defects. Look for:

- behaviour the tests pass by accident, or inputs they never try (empty, very
  large, odd characters, a second call, concurrent calls);
- claims in documentation or comments that do not match the code: check each
  against the source;
- a copy of existing code instead of reuse, or a change far wider than asked;
- a special case that exists only to keep an old test green: a condition on
  configuration emptiness, a value compared with a default, a branch no new test
  exercises. Find the test it serves, and fix the spec instead — the case is the
  contradiction, not the fix;
- anything that needs a real run to trust (a command the project provides, a page
  in a browser): run it yourself in a scratch worktree
  (`git worktree add <dir> <branch>`), then remove the worktree.

Read the agent's final message as well as the diff: it is where a conflict with the
specification gets reported. A test the agent says it could not satisfy is a defect
in the spec, not a remark to skim past.

Write down what you find. A finding you can fix in a few lines becomes a separate
commit after the merge, with its own test first. A larger one becomes a new task.

## 4. Merge

Only with the user's go-ahead, unless they already asked you to merge reviewed work.

1. Switch to the user's branch and make sure it is clean. Check that CI on it is
   green (for GitHub: `gh run list --branch main --limit 3`). A red CI is fixed
   first: merged into a red branch, a new failure hides among the old ones.
2. `git merge --no-ff --no-commit <branch>` and run the project's full check (for
   example `make check`), and its slower suites too when that check skips them —
   integration, database, browser: a suite nobody runs is red for as long as nobody
   looks. They run on the merged tree, which is what lands. If anything fails,
   `git merge --abort` and report.
3. Write the merge message to a file and commit with `git commit -F <file>`
   (never `-F -`). Say what changed, why, and what you checked beyond the tests.
4. Do not push unless asked.

For a user who is not a developer, or who just wants it in, `aidev task apply <task>`
does the merge and its commit in one go — it merges the task's branch into the
checked-out branch
with a merge commit, refuses a checkout with uncommitted changes, aborts and names
the files on a conflict — and records it in the task's history. Run the project's
check right after it; if that fails, `aidev task undo <task>` reverts the merge with
a new commit (safe after a push), and that is also the answer to "take it back".

## 5. Report

- **Developer**: the merge commit, what the diff does, what you checked beyond the
  tests, and any follow-up you made or propose.
- **Not a developer**: what now works differently, in their words; how it was
  checked ("the automatic checks passed, and I also tried ... myself"); anything
  still open, as a question about behaviour. No diffs, hashes or branch names unless
  they ask.

## 6. Tidy up

- A failed task keeps its worktree. Once its work is merged, superseded or not
  needed, remove it: `aidev worktree remove <ref>` (add `--force` only after you
  have looked at the uncommitted changes and know they are not needed).
- Delete a `spec/*` branch only when the user agrees.
