# Working in this repository

Guidance for an agent or a person making changes here. It is short on purpose;
everything in it is a rule that a reviewer would otherwise have to repeat.

## Before you finish

```bash
make check
```

`gofmt`, `go vet`, `staticcheck`, and the unit tests. If you touched anything
involving persistence, also:

```bash
make db-up && make test-db-create && make test-integration
```

A change is not done until `make check` passes.

## Invariants — do not break these without changing the documented decision

1. **The agent writes only inside a task's own git worktree.** Never run an agent
   with the repository's main working tree as its working directory. Worktree paths
   must resolve inside `WORKSPACE_ROOT`. Rationale: OpenCode writes files with no
   permission prompt, so this is the only containment there is
   (docs/research.md §2.6).

2. **Only verification may declare success.** Do not add a transition from
   `RUNNING` to `SUCCEEDED`. There is a test asserting no state other than
   `VERIFYING` can reach it.

3. **Nothing is written to stdout except command output.** All logging goes to
   stderr. In MCP mode stdout carries JSON-RPC, and one stray line corrupts the
   protocol (docs/research.md §3.3).

4. **The event log is append-only.** Do not add an update path. Correct the record
   by appending a corrective event. The database enforces this.

5. **Never log secrets.** Connection strings are redacted via
   `config.RedactURL`. Do not persist environments; argv is task-defined and safe,
   an environment is not.

6. **Every external process gets a timeout, cancellation, and bounded output.**
   No exceptions, including verification commands.

7. **Never edit an applied migration.** Add a new one. The migrator checksums
   applied files and will refuse to start if one changed.

8. **Status values live in two places.** Adding one means updating both the Go
   enumeration and the SQL `CHECK` constraint in a new migration.
   `TestEnumsMatchMigrationConstraints` will fail if you update only one.

## Conventions

- **Configuration is read only by `internal/config`.** Do not call `os.Getenv`
  anywhere else.
- **Domain validation lives in `internal/task`.** Database constraints are a
  backstop, not the primary check — but keep them in agreement.
- **Errors say what was being done and to what.** `fmt.Errorf("create attempt %d
  for task %s: %w", ...)`. Wrap with `%w`; callers distinguish `store.ErrNotFound`,
  `store.ErrConflict`, `store.ErrAlreadyExists` and `*task.TransitionError`.
- **Prefer the standard library.** The dependency list is `pgx`, `uuid`, and (from
  Phase 4) the official MCP SDK. Adding to it needs a reason in the commit message.
- **Comments explain *why*.** The code already says what it does. Comments that
  restate it will be removed in review.
- **Tests state the property they protect,** in the test name or a short comment.
  A test whose purpose is unclear cannot be maintained.

## Do not add

Kafka, Redis, Kubernetes, a web dashboard, an authentication system, a scheduler, a
DAG executor, automatic merge, or an LLM inside aidev. These are explicit
non-goals. Extension seams for several of them already exist and are documented in
docs/architecture.md; use them rather than inventing infrastructure.

## Commits

Small and focused, with a subject line in the imperative
(`feat: add git worktree manager`). Explain *why* in the body when the change is
not obvious. Do not bundle unrelated work into one commit.

## Research before guessing at external tools

docs/research.md records what the installed OpenCode, Claude Code, git and
PostgreSQL actually do, and distinguishes measurements from assumptions. If you
need behaviour that is not recorded there, **measure it and add it to that
document** rather than inferring it from documentation or from this brief. Several
entries exist precisely because the obvious assumption was wrong.
