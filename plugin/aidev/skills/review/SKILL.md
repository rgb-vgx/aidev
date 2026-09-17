---
name: review
description: Review the result of an aidev task before it reaches the user's branch, then merge it and report. Use after aidev_run_task or aidev_get_task_result reports a SUCCEEDED task, or when the user asks to check, accept, apply or merge what aidev did.
---

# Review an aidev result

A SUCCEEDED task proves one thing: its verification commands passed. It does not
prove the change is right, complete, or free of shortcuts. You supply the judgment
the commands cannot. Nothing is merged until you have.

## 1. Collect the change

The result names the branch, `aidev/<ref>`. Compare it with the branch the task
started from (its `base_ref`, usually `spec/<name>`):

```
git log --oneline <base_ref>..aidev/<ref>
git diff --stat <base_ref> aidev/<ref>
git diff <base_ref> aidev/<ref>
```

## 2. Hard conditions: any failure means do not merge

- **No test file changed.** The tests on the base branch are the specification; an
  edit to one is the agent grading its own work. If a test really had to change,
  the specification was wrong: fix it on the spec branch and delegate again.
- **Applied database migrations are untouched**; new schema changes are new files.
- **Only files the task is about changed.** A deleted file must be one the task
  asked for.

## 3. Judgment: what the tests did not exercise

Read the whole diff, not only the summary. Look for:

- behaviour the tests pass by accident, or inputs they never try (empty, very
  large, odd characters, a second call, concurrent calls);
- claims in documentation or comments that do not match the code: check each
  against the source;
- a copy of existing code instead of reuse, or a change far wider than asked;
- anything that needs a real run to trust (a command the project provides, a page
  in a browser): run it yourself in a scratch worktree
  (`git worktree add <dir> aidev/<ref>`), then remove the worktree.

Write down what you find. A finding you can fix in a few lines becomes a separate
commit after the merge, with its own test first. A larger one becomes a new task.

## 4. Merge

Only with the user's go-ahead, unless they already asked you to merge reviewed work.

1. Switch to the user's branch and make sure it is clean.
2. `git merge --no-ff --no-commit aidev/<ref>` and run the project's full check (for
   example `make check`). If it fails, `git merge --abort` and report.
3. Write the merge message to a file and commit with `git commit -F <file>`
   (never `-F -`). Say what changed, why, and what you checked beyond the tests.
4. Do not push unless asked.

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
