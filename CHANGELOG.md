# Changelog

Each version's section is the text of its GitHub release. Install or upgrade
with:

```bash
curl -fsSL https://raw.githubusercontent.com/rgb-vgx/aidev/main/install.sh | sh
```

## Unreleased

- **Tasks can run side by side.** Each task now gets its own OpenCode database
  (`workspace_root/opencode-db/<ref>.sqlite`, kept for its retries): on OpenCode's
  one shared store, runs started together died at random with `database is
  locked`. Branches that sibling tasks create meanwhile are no longer reported as
  shared refs changing.

- `aidev task diff <task>` shows what a task changed: the delivered diff, or the
  undelivered change of a failed attempt, marked as such.

- After a retry, `aidev task result` — and the MCP result — list the earlier
  attempts with their status, kind and error.

- Plugin 0.5.1: the review skill starts from `aidev task diff`.

## v0.3.0

Upgrading from v0.2.0: install the new binary, then run `aidev migrate` (one
migration, for the apply and undo events) and `claude plugin update aidev@aidev`,
and restart Claude Code. Back up the database first if you like — the new
"Backing up and restoring" section in docs/architecture.md shows how.

- **Apply and undo.** `aidev task apply <task>` merges a succeeded task's branch
  into the branch you have checked out, with a merge commit, and records it;
  `aidev task undo <task>` reverts it with a new commit, safe after a push. Both
  refuse a checkout with uncommitted changes, and a conflict is aborted with the
  files named and nothing changed. Applying again after an undo works.

- **A stale run can no longer write.** Every write of a run checks, under the
  task's row lock, that its attempt is still its own; a run that was paused past
  its lease or cancelled from another process stops publishing — no commit on
  the branch, no events after the ending. Crashes at each boundary of a run are
  now tested against the real binary.

- `aidev doctor` warns when the disk holding the worktrees is nearly full, and
  tells a stopped Docker daemon, a permission problem and a stopped database
  container apart.

- The documentation says plainly what the agent can reach — it runs as you,
  without a sandbox — and how to back up and restore the database.

- Plugin 0.5.0: the review skill offers apply and undo; the doctor skill covers
  every check, including stuck tasks and the disk.

## v0.2.0

Upgrading from v0.1.0: install the new binary, then run `aidev migrate` (twelve
migrations; the CLI refuses to run against a schema with pending ones) and
`claude plugin update aidev@aidev`, and restart Claude Code. Repositories you
have only used through MCP need `aidev project add <path>` once.

- **Automatic retry.** `--max-retries N` (MCP: `max_retries`, 0 to 10) lets a
  task try again, within the same run, when its checks fail or its agent stops
  early. The retry continues in the same worktree and agent session with the
  failing output in its prompt; the failed attempt's work is kept, marked
  unverified, on its own branch, and a success lands on `aidev/<ref>-aN` — the
  result names the branch. A refused tool call, an intercepted runner or a
  timeout is never retried.

- **What is committed is what the agent delivered.** The success commit is the
  worktree as it stood when the agent finished, not as the verification commands
  left it, so a `cover.out` or a build product the checks wrote never reaches the
  branch. When in-place checks change files, `task.verification_worktree_modified`
  says which.

- **MCP runs outlive the session.** `aidev_run_task` starts a separate
  `aidev task run` process with its own deadline and a log under
  `workspace_root/run-logs/`, so closing Claude Code no longer stops a task.
  Runs hold a lease; `aidev task recover` cancels tasks whose process died.

- **MCP only creates tasks for registered repositories.** Register one with
  `aidev project add <path>` (a repository you created a task for with the CLI is
  already registered), or set `mcp.auto_register_projects`. A planner that
  guesses a path can no longer send an agent into the wrong repository.

- **Bounded logs.** `include_logs` returns each section cut to 64 KiB with its
  full size and a `next_offset` to page on; the agent's event stream comes back as
  a plain-text transcript unless the raw `stdout` section is asked for.

- **The base commit is recorded at creation.** If the base ref moved before the
  task ran, the run says so (`task.base_moved`, `result.base_moved`) and goes
  ahead on the current code.

- **Retention.** `aidev prune --logs-older-than 30d` clears old captured output
  of finished tasks and keeps their records and history; `aidev task delete`
  removes a finished task whose worktree is gone.

- Plugin 0.4.0: the delegate skill suggests `max_retries 1` and pages long logs;
  the review skill reviews the branch the result names; the doctor skill explains
  an unregistered repository.

- **The judge is harder to fool.** `--protect <glob>` (MCP: `protected_paths`)
  refuses, before any check runs, an attempt that changed a ring-fenced path; an
  attempt that edits files that look like the tests judging it is reported
  (`task.verification_tests_modified`). `--verify-mode clean` runs the checks in
  a fresh checkout of the agent's result, so a file git ignores cannot make them
  pass; `--setup` adds preparation commands. `--expect-fail-on-base` runs the
  checks on the base commit first and fails a bug-fix task whose checks already
  pass there.

- **Containment.** An attempt that writes git state shared with the main
  repository — config, hooks, attributes, other refs — fails with kind
  `CONTAINMENT`; every git command aidev runs neutralises those vectors, and child
  processes no longer inherit `AIDEV_*` variables. Task status changes are
  guarded by a database trigger as well as in Go.

- **Approval policy per repository.** `aidev project approval on` gates every
  task of a repository; the MCP approve tool is off unless `mcp.allow_approval`
  is set, so a planner cannot approve its own tasks.

- **Sturdier runs.** The verification pass has a total time bound; truncated
  output keeps its tail; the process group is reaped on every run; a cancel that
  loses a race is retried; closing the terminal records a cancellation; the
  agent list is cached; events are ordered per task so a reader following a
  cursor cannot miss one.

- **Submodules**: a project whose repository keeps its sources in git submodules
  can now be delegated to. `aidev project submodules read_only` (per repository,
  off by default) gives every task worktree a checkout of each submodule at the
  commit the repository pins, so verification commands can read those sources.
  Each task gets its own checkout — a linked worktree of the submodule's own
  repository, not a re-clone from its remote — so two tasks on one repository do
  not share a submodule HEAD, and no network or credentials are needed.

  Read-only in this version: a task still leaves one commit on one branch of one
  repository, and nothing inside a submodule is committed. Nested submodules are
  not loaded. aidev does not fetch: if the repository pins a commit no local copy
  has, it says so and names where to fetch it.

- `aidev project list` shows the repositories aidev has run tasks against, with
  each one's default branch and submodule mode.

- **A run stopped by a refused tool call now says so.** A refusal ends an
  OpenCode session outright, which looked identical in the record to a model
  that quit halfway: both were reported only as "the session ended while the
  agent was still calling tools". The failure now names the refusal, counts them,
  and gives the path the agent was reaching for — and says when that path was
  outside the worktree, which means the task is asking for something that is not
  in the checkout. The failure kind is unchanged (`AGENT_ERROR`); a bash call's
  command line is deliberately left out of the message.

## v0.1.0

The first release: prebuilt binaries for Linux and macOS (amd64 and arm64),
installed by `install.sh` after checking `SHA256SUMS`.

aidev hands implementation work to a coding agent in an isolated git worktree
and decides the outcome by running the task's own verification commands. What
the agent says about its work is recorded, never trusted.

- **Tasks**: `aidev task create/run/get/result/events/cancel/approve`. A task
  needs at least one verification command; only those commands can make it
  `SUCCEEDED`. A success leaves one commit on `aidev/<ref>`; a failure keeps its
  worktree for inspection. The agent cannot pass by changing the verification
  runner (interception is refused).
- **Claude Code**: the `aidev` plugin (`claude plugin marketplace add
  rgb-vgx/aidev`, `claude plugin install aidev@aidev`) registers the MCP server
  and three skills: `aidev:delegate`, `aidev:review` and `aidev:doctor`.
- **Setup**: `aidev setup` starts PostgreSQL in Docker (bound to 127.0.0.1;
  `--postgres-image` for a private registry, `--postgres-volume` to keep
  `make db-up` data) or uses `--database-url`, writes a private conf.json,
  migrates, and prints the lines to add. It is safe to run again.
- **Diagnosis**: `aidev doctor [--json]` checks the configuration, git, the
  agent, the database, migrations and the workspace, with a fix for each
  failure and the password never shown.
- **Models**: OpenCode by default (`opencode/muse-spark-1.3-contributor-free`,
  no credentials needed); per-task model and hardness routing; `aidev stats`
  reports outcomes by model and hardness. A Codex backend exists but is not the
  default.
- **Observability**: optional OpenTelemetry traces (Jaeger or Langfuse), with
  the agent's token usage on its span.
- **Documentation**: README and README.vi.md, and an HTML guide in English and
  Vietnamese (`docs/guide`).

aidev is released under the MIT license.

Known limits: no Windows build (aidev relies on Unix process groups); the agent
is not sandboxed beyond its worktree; a failed task is not retried
automatically.
