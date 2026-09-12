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
| 2 | Git worktrees, `AgentBackend`, OpenCode backend, verification, execution | not started |
| 3 | Task CLI | not started |
| 4 | MCP server | not started |
| 5 | Hardening, E2E, docs | not started |

Sections below describing Phase 2+ state intent, not implementation. Anything not
yet built says so.

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
  cli/                command line surface
migrations/           SQL, embedded into the binary
tests/integration/    tests that require a real PostgreSQL
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
| Retry | `task_attempts` is append-only with numbered attempts; `max_retries` is stored; `worker_runs.session_id` records the resumable agent session. Only the `FAILED → READY` edge and a policy are missing. |
| Concurrent workers | `tasks_ready_claim_idx` matches a `FOR UPDATE SKIP LOCKED` claim; status changes are already compare-and-set. |
| A second agent backend | `AgentBackend` (Phase 2); orchestration never names OpenCode. |
| Dependency DAG | `PENDING` exists as "not yet eligible"; the eligibility check is the hook. |
| Observability / event-driven features | `events.seq` gives a total order, so a consumer can resume from a cursor. |
| Approval workflows | `approvals` with one-pending-per-task, plus `WAITING_APPROVAL` in the state machine. |
| Merge | `worktrees` records branch and base commit; nothing merges yet, by design. |

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
