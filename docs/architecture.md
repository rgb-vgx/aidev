# Architecture

## What aidev is

aidev is a deterministic control plane. It sits between a planner that decides
*what* should be built and a coding agent that does the building, and it owns the
parts that must not be left to a language model: isolating the work, bounding it,
verifying it independently, and recording what happened.

```text
        Claude Code                  plans, reviews
             │
             │  MCP (stdio, JSON-RPC)
             ▼
  ┌──────────────────────┐
  │        aidev         │  orchestration, policy, persistence
  │                      │
  │  task lifecycle      │
  │  worktree isolation  │
  │  verification        │
  │  event log           │
  └───┬──────────────┬───┘
      │              │
      │ AgentBackend │ os/exec
      ▼              ▼
  OpenCode      git worktree  ◀── the only place the agent may write
      │              │
      └──────────────┘
             │
             ▼
        PostgreSQL          authoritative state + history
```

The division of responsibility that matters: **Claude Code never learns the
OpenCode command line.** That knowledge lives in one package
(`internal/agent`, Phase 2). Swapping the agent, or adding a second one, does not
touch the orchestration layer.

## Status

| Phase | Scope | State |
|---|---|---|
| 0 | Environment research | done — [docs/research.md](research.md) |
| 1 | Domain, persistence, migrations, config, logging | done |
| 2 | Git worktrees, `AgentBackend`, OpenCode backend, verification, execution | done |
| 3 | Task CLI | done |
| 4 | MCP server | not started |
| 5 | Hardening, E2E, docs | not started |

Sections below describing Phase 4+ state intent, not implementation. Anything not
yet built says so. The MCP surface does not exist yet; the CLI does, and is the
only way to drive a task today.

## Package layout

```text
cmd/aidev/            entry point: signal handling and exit codes only
internal/
  task/               domain: Task, attempts, runs, worktree record, approval,
                      the lifecycle state machine, failure classification
  event/              the event vocabulary and event construction
  store/              PostgreSQL persistence and the embedded migrator
  config/             environment configuration, loaded and validated once
  logging/            structured logging to stderr
  procexec/           one place where external processes are run, bounded,
                      deadlined and cancellable
  git/                worktree creation, diff collection, cleanup, and the path
                      checks that enforce isolation
  agent/              the Backend boundary, the OpenCode implementation, a fake
  verification/       aidev running the task's own commands
  worker/             the lifecycle: isolate, delegate, verify, record
  cli/                command line surface
migrations/           SQL, embedded into the binary
prompts/              agent instruction templates, embedded
tests/integration/    real PostgreSQL, real git, scripted agent
tests/e2e/            real PostgreSQL, real git, real OpenCode (opt-in)
docs/                 this directory
```

### Why the domain is one package, not one per table

`internal/task` holds `Task`, `TaskAttempt`, `WorkerRun`, `VerificationRun`,
`Worktree` and `Approval` together. They are one aggregate: none of them is
meaningful without the task, and they change together. Splitting them across
packages would buy directory symmetry at the price of import cycles and
`interface{}` plumbing between packages that are conceptually one thing.

`internal/task` depends on nothing but the standard library and a UUID type, so
every other package may import it freely. That is what makes it safe for
`internal/agent` to use `task.FailureKind` as the shared vocabulary for *why
something failed* without the domain knowing anything about OpenCode.

## Execution flow

`worker.Orchestrator.RunTask` is the only code that knows this order.

```text
                resolve task
                     │
            ┌────────▼────────┐   requires approval and none granted
            │ approval policy ├──────────────▶ WAITING_APPROVAL  (no attempt,
            └────────┬────────┘                                   no worktree)
                     │
              PENDING ──▶ READY
                     │
        ┌────────────▼────────────┐  one transaction
        │ claim task + open attempt│  RUNNING + attempt row + task.started
        └────────────┬────────────┘
                     │
              create worktree ──────── failure ──▶ FAILED (WORKTREE)
                     │
              run agent in it ──────── failure ──▶ FAILED (agent's own kind)
                     │                             worktree RETAINED
              collect diff from git
              persist worker run
                     │
                 VERIFYING
                     │
         aidev runs the task's commands
                     │
         ┌───────────┴───────────┐
      passed                  failed
         │                       │
  commit to aidev/<ref>     worktree RETAINED
  remove worktree           FAILED (VERIFICATION)
  SUCCEEDED
```

Three properties of this flow are worth stating separately, because they are what
the tests hold rather than what the diagram shows:

**A state change and its event are one transaction.** History cannot disagree with
state. Claiming a task and opening its attempt are likewise atomic, so a task can
never be `RUNNING` without a record of which attempt is running it.

**The writes that record an outcome run on a detached context.** Cancelling a task
would otherwise cancel the very writes that record the cancellation, leaving the
row in `RUNNING` forever. A test cancels mid-run and then re-reads the task from
the database.

**A failed task is an `Outcome`, not an error.** Delegating work to an agent that
does not succeed is the expected case, and the caller needs the record rather than
an exception. `RunTask` returns an error only when it could not conduct the run at
all.

## Cleanup policy

The policy exists because the obvious options are both wrong. Removing a
successful task's worktree destroys the deliverable, since the agent's work is
uncommitted. Keeping every worktree grows the workspace without bound.

| Outcome | What happens |
|---|---|
| succeeded | the work is committed to `aidev/<ref>`, then the worktree directory is removed |
| failed | the worktree is kept, untouched and uncommitted, and recorded as `RETAINED` |
| cancelled | same as failed |
| `WORKTREE_CLEANUP=never` | nothing is removed, but the work is still committed |

Committing is not merging: only the task's own branch is written, and the result is
reviewable with `git log aidev/<ref>` and `git diff main..aidev/<ref>`.

There is deliberately no policy that discards failed work, and `--force` is never
passed automatically. Git refuses to remove a worktree holding uncommitted
changes, so the guard is the tool's own behaviour rather than aidev's diligence
(docs/research.md §7b). `ListRetainedWorktrees` is how an operator finds abandoned
work.

## Design decisions

Each of these is a decision with a cost, recorded so it can be revisited rather
than rediscovered.

### The git worktree is the only containment boundary

Phase 0 measured OpenCode writing files in non-interactive mode with no permission
prompt, whether or not its `--auto` flag was passed (docs/research.md §2.6). There
is no agent-side sandbox to rely on.

Therefore: every task runs with the agent's working directory set to a dedicated
worktree, worktree paths are validated to resolve inside `WORKSPACE_ROOT`, and the
repository's main working tree is never a valid target. This is enforced in code
and asserted by tests, not stated as a convention.

### Only aidev's own verification can declare success

The lifecycle state machine has no edge from `RUNNING` to `SUCCEEDED`. Success is
reachable only through `VERIFYING`, so an agent's claim that tests pass cannot
affect the outcome. A test asserts that no other state can reach `SUCCEEDED`.

The corollary is that a task with no verification command is not accepted at all —
by the domain, and again by a `CHECK` constraint. aidev refuses to be in a position
where it would have to report an outcome it could not establish.

### One-shot `opencode run`, not a managed OpenCode server

Both were tried in Phase 0 (docs/research.md §2.8). The CLI gives an exit code,
both streams, NDJSON events, and — measured, not assumed — clean cancellation on
SIGTERM. Server mode's one real advantage, a deterministic `/interrupt`, is
therefore redundant, and its `/wait` endpoint is unimplemented in OpenCode 1.18.30,
so completion detection would need polling or SSE.

Cost: aidev pays OpenCode's process startup per task. Accepted, because the
alternative is owning a server's lifecycle — health, ports, crash recovery — for no
MVP capability. The `AgentBackend` interface keeps a server-mode backend addable
without touching orchestration.

### External processes are run in exactly one place

`internal/procexec` owns every subprocess: its deadline, its cancellation, its
bounded output, and the classification of how it ended. The agent backend and the
verification runner both use it, because both need the same guarantees and two
implementations would mean two places to get process cleanup wrong.

Cancellation signals the process group, not the pid. `opencode run` spawns no
children, but verification commands routinely do — `go test` starts compilers and
test binaries — so killing only the parent would leave them running. The test for
this was verified to fail when the kill is changed to pid-only.

### Configuration is read once, in one place

`config.Load` is the only reader of the environment. It validates everything and
reports *every* problem in one error rather than one per run. A misconfiguration
fails at startup instead of surfacing halfway through a task.

### Logs are JSON on stderr, always

MCP uses stdin/stdout for JSON-RPC (docs/research.md §3.3), so a single log line on
stdout corrupts the protocol. `logging` has no stdout option at all; removing the
possibility is more reliable than remembering the rule.

Correlation is by `task_id`, `attempt_id` and `worktree_path`, attached to a logger
carried in the context so deep call paths inherit them without threading a logger
through every signature.

### Enumerations are typed, and their SQL twin is tested

Every state is a named Go type with a `Valid()` method and a `CHECK` constraint in
SQL. The duplication is real, so `TestEnumsMatchMigrationConstraints` reads the
migration and fails on drift. The test was verified to fail when a value is removed
from the constraint.

### An agent's account of itself is recorded, never trusted

`worker_runs.summary` stores what the agent said it did, verbatim, because it is
useful when diagnosing a failure. It has no effect on the outcome. The diff stored
alongside it is collected from git by aidev, not reported by the agent, and the
verification rows come from aidev running the commands itself.

The integration suite contains the direct test of this:
`TestAgentClaimingSuccessWithoutDoingTheWorkFails` has the agent report "All done!
Tests pass." without touching a file, and the task ends `FAILED` with the claim on
record beside the contradiction.

### State changes are compare-and-set

`UPDATE ... WHERE id = $1 AND status = $2`. Zero rows means another writer moved
first, which is reported as `ErrConflict` and distinguished from `ErrNotFound`. The
MVP has one writer; this is what makes adding a second one a code change rather
than a redesign.

## Extension seams

These exist now and are unused, because the MVP does not need them. Each is a
place a listed future feature plugs in without a rewrite.

| Future feature | Seam that already exists |
|---|---|
| Retry | `task_attempts` is append-only with numbered attempts; `max_retries` is stored; `worker_runs.session_id` records the resumable agent session; worktree and branch names already include the attempt number so a second attempt cannot collide with the first. Only the `FAILED → READY` edge and a policy are missing. |
| Concurrent workers | `tasks_ready_claim_idx` matches a `FOR UPDATE SKIP LOCKED` claim; status changes are already compare-and-set. |
| A second agent backend | `agent.Backend`; orchestration never names OpenCode. A server-mode OpenCode backend, or a different agent entirely, is a new file in `internal/agent`. |
| Dependency DAG | `PENDING` exists as "not yet eligible"; the eligibility check is the hook. |
| Observability / event-driven features | `events.seq` gives a total order, so a consumer can resume from a cursor. |
| Approval workflows | `approvals` with one-pending-per-task, plus `WAITING_APPROVAL` in the state machine. |
| Merge | every successful task leaves a reviewable commit on `aidev/<ref>`, and `worktrees` records branch, base commit and head commit; nothing merges yet, by design. |

Deliberately **not** present: a scheduler, a DAG executor, automatic merge, a web
dashboard, authentication, Redis, Kafka, Kubernetes, or an LLM inside aidev.

## Failure classification

`task.FailureKind` is the shared vocabulary for why something failed. The values
come from failure modes actually provoked in Phase 0 (docs/research.md §2.5), not
from imagination:

| Kind | Cause |
|---|---|
| `STARTUP` | the agent process could not start or rejected its arguments |
| `AGENT_ERROR` | the agent reported an error in its own event stream |
| `AGENT_EXIT` | non-zero exit with no error event |
| `TIMEOUT` | aidev's deadline elapsed |
| `CANCELLED` | a human or a shutdown stopped it |
| `VERIFICATION` | the agent finished but verification did not pass |
| `WORKTREE` | the isolated workspace could not be prepared or inspected |
| `APPROVAL_DENIED` | policy refused |
| `INTERNAL` | aidev itself failed |
| `UNKNOWN` | unclassifiable — kept as an honest bucket rather than a plausible guess |

Phase 0 found that an unknown `--agent` makes OpenCode print a warning, fall back to
its default, and **exit 0**. Classification therefore never trusts the exit code
alone, and aidev validates agent names itself.
