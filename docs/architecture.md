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
| 4 | MCP server | done |
| 5 | Hardening, E2E, docs | done |

All five phases are implemented. Both surfaces exist — the CLI, and the MCP server,
which has been registered with and connected to the installed Claude Code and used
by it to create and run a task. Everything described below is built; the limitations
are listed at the end rather than implied.

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
  view/               the external JSON shapes, shared by the CLI and MCP
  mcp/                the MCP server and its eight tools
  cli/                command line surface
migrations/           SQL, embedded into the binary
prompts/              agent instruction templates, embedded
tests/integration/    real PostgreSQL, real git, scripted agent
tests/e2e/            real PostgreSQL, real git, real OpenCode (opt-in)
docs/                 this directory
```

### Deviations from the specified layout, and why

Three departures from the layout in the brief, each a decision rather than an
oversight.

**No `Dockerfile`, and no `deployments/` for aidev itself** (`deployments/langfuse/` holds only the optional observability stack's compose file). aidev must run on the host. It operates on
the developer's own repositories, creates git worktrees next to them, and executes the
`opencode` binary and the task's verification commands — `go test`, a linter, whatever
the project uses. A containerised aidev would need the repository bind-mounted, git
and its identity available inside, the OpenCode install and its credentials mounted,
and the project's whole toolchain present in the image. That is a large amount of
fragile plumbing in exchange for nothing: the thing being isolated is the *agent*, and
a git worktree already does that. Docker Compose is used for PostgreSQL, which is a
service and belongs in one.

**No `internal/approval/`.** Approvals are part of the task aggregate: the record
lives in `internal/task` beside the task it gates, and the policy that enforces it
lives in `internal/worker` with the rest of the lifecycle. A package containing one
struct and no behaviour would be directory symmetry, not structure.

**Four packages the brief did not list.** `internal/procexec` exists so that
subprocess handling is written once rather than in both the agent backend and the
verification runner. `internal/view` exists so the CLI's JSON and the MCP schemas
cannot drift apart. `internal/cli` keeps commands out of `main` so they can be tested
without spawning a binary. `internal/logging` is small but is the single place that
enforces stderr-only output.

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
              (+ submodules, if the
               project asks for them)
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
  (fails ──▶ FAILED          FAILED (VERIFICATION)
   (WORKTREE), RETAINED)
  VERIFYING ──▶ SUCCEEDED
  remove worktree
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
| `tasks.worktree_cleanup` set to `never` | nothing is removed, but the work is still committed |

Committing is not merging: only the task's own branch is written, and the result is
reviewable with `git log aidev/<ref>` and `git diff main..aidev/<ref>`.

There is deliberately no policy that discards failed work, and `--force` is never
passed automatically. Git refuses to remove a worktree holding uncommitted
changes, so the guard is the tool's own behaviour rather than aidev's diligence
(docs/research.md §7b). `ListRetainedWorktrees` is how an operator finds abandoned
work.

A project with submodules removes its submodule checkouts first, because git will
not remove a working tree that contains them. The empty directories are put back,
so the `--force` guard above still behaves exactly as it does without submodules.

## Design decisions

Each of these is a decision with a cost, recorded so it can be revisited rather
than rediscovered.

### The git worktree is the only containment boundary

Phase 0 measured OpenCode writing files in non-interactive mode with no permission
prompt, whether or not its `--auto` flag was passed (docs/research.md §2.6). There
is no agent-side sandbox to rely on.

Therefore: every task runs with the agent's working directory set to a dedicated
worktree, worktree paths are validated to resolve inside `workspace_root`, and the
repository's main working tree is never a valid target. This is enforced in code
and asserted by tests, not stated as a convention.

What the boundary contains is the working *tree*, not git state. Linked worktrees
share refs, config and the stash stack with the main repository: measured, an
agent inside its worktree can `update-ref` a branch the operator's checkout is on,
and a `core.fsmonitor` set from there runs on the operator's next `git status`
(docs/research.md §7i). Ref and config writes are outside what a worktree can
contain, so aidev adds two layers on top of the tree boundary: every git command
it runs itself is neutralised (no fsmonitor, no hooks path, no ext-diff), and
shared state is snapshotted around the agent's run — a write to config, hooks,
attributes or HEAD fails the attempt as `CONTAINMENT` before verification even
starts, while a foreign ref moving only warns (docs/research.md §7i.2). This is
detection after the fact, not a sandbox: the security model states the trust that
follows from that.

### Submodules are a worktree each, read-only, and off by default

A repository that keeps its sources in git submodules gets a task worktree whose
submodule directories are **empty**, because `git worktree add` does not populate
them. Every verification command that reads those sources then fails — and since
aidev only accepts work it can check by command, the whole class of task cannot be
delegated at all.

With `projects.submodules` set to `READ_ONLY`, each submodule the base commit pins
becomes a **linked worktree of the submodule's own repository**, checked out at the
pinned commit and placed at the gitlink. The task's checkout is then complete, and
its submodule HEADs are its own.

The obvious alternative, `git submodule update --init` inside the worktree, was
measured and rejected. On git 2.43 it does isolate HEAD — it gives a linked worktree
its own submodule git directory under `.git/worktrees/<wt>/modules/<name>` — but it
does so by **re-cloning every submodule from its remote, for every task**, with no
alternates: a network fetch and a full copy of the object store each time, needing
whatever credentials that remote wants, inside a step that is meant to be local and
fast. A linked worktree needs neither (docs/research.md §7h).

Three consequences worth knowing:

- **Removal is ordered.** Git refuses outright to remove a working tree containing
  submodules, so `Remove` takes the submodule checkouts back first — and then
  recreates the empty directories, because a missing gitlink reads as ` D <path>`
  and would make git demand `--force`. That refusal is the guard on a failed
  attempt's work, so it has to keep working without it.
- **Status ignores dirty submodules.** Content edited inside a submodule made
  `git status` report work to commit while `git add --all` staged nothing for an
  unmoved gitlink, and the commit then failed. `--ignore-submodules=dirty` makes the
  two agree; a moved gitlink is still reported, and a repository without submodules
  is unaffected.
- **aidev never fetches.** A pinned commit no local repository holds is reported as
  such, naming the repository to fetch it in, rather than surfacing as an unknown
  revision from a directory the operator would have to go and find.

Off by default because populating submodules registers a worktree in the
submodule's own repository, and no single-repository project should pay for a
feature it cannot use. With the mode at `NONE`, worktree creation runs exactly the
git commands it ran before.

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

### One JSON contract, two surfaces

The CLI's `--json` output and the MCP tools' structured results are the same shapes,
defined once in `internal/view`. The `jsonschema` tags there are what the MCP SDK
reads to generate each tool's output schema, so the schema a planner sees is
generated from the same struct the CLI serialises and cannot drift from it.

They stay separate types from the domain structs: this is an interface other things
depend on, and deriving it from `task.Task` would make an internal rename a silent
breaking change.

### A long task cannot be a long tool call

A run takes as long as the agent does — 9 to 656 seconds, measured — while an MCP
client will not wait indefinitely. `aidev_run_task` therefore starts the run on the
server's own context, waits a bounded time, and returns with `still_running` set if
it has not finished. The run is unaffected by the call returning or by the client
abandoning it, and the server drains in-flight runs on shutdown so that quitting
mid-task records a cancellation rather than leaving a row in `RUNNING`.

A second `aidev_run_task` joins the run in flight rather than failing: the
orchestrator would reject a concurrent run anyway, and reporting progress is more
useful than an error.

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

### A task states how hard it is; configuration picks the model

A task carries an optional `model` and an optional `hardness` (`TRIVIAL`, `STANDARD`,
`HARD` — a closed vocabulary, because a number invites false precision and free text
cannot be routed). The worker resolves the model for a run in one place: the task's own
model, then `agent.routing[hardness]` from conf.json, then nothing at all. "Nothing" is
not a gap — it hands the choice to the backend, which was built from configuration and
holds its own model, so `agent.opencode.model` and `agent.codex.model` each apply to
their own backend rather than one standing in for the other.

The run record does not repeat that calculation. `agent.Result.Model` is what the
backend reports it actually used, and that is what `worker_runs.model` stores, next to
`worker_runs.agent`. The distinction matters as soon as two backends disagree: before
`Result.Model` existed, the worker recomputed the fallback and would have filed a Codex
run under an OpenCode model — a history that reads plausibly and is wrong.

This is the routing half of "task hardness → agent/model routing → isolated execution →
independent verification". Nothing here decides hardness on a task's behalf: a person
states it, and the recorded history is what makes the policy checkable.

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
| Another agent backend | `agent.Backend`; orchestration never names a backend. Codex was added exactly this way (`internal/agent/codex.go`, selected with `agent.backend` set to `"codex"`) and is **paused since 2026-09-15**: OpenCode is the backend in use. The code and its tests stay, so resuming is a configuration change. Codex took 6–8 minutes on trivial tasks and, in TASK-000029, read another project's virtualenv outside its worktree. A server-mode OpenCode backend would be another new file in `internal/agent`. |
| Sandboxed agent execution | `internal/procexec` is the one place processes start, and `agent.Request.WorkingDir` is the only path an agent is given, so containing the agent is a change to how a backend launches. Verification stays local whatever happens: a remote exit code is not aidev's own measurement (docs/opensandbox.md). **Parked as a future feature on 2026-09-15** — tasks are not yet complex enough to need it. Unmeasured: a worktree's `.git` file points into the main repository, so a container would need both mounted. |
| Dependency DAG | `PENDING` exists as "not yet eligible"; the eligibility check is the hook. |
| Observability / event-driven features | `events.seq` gives every event a position, and `AppendEvent` takes a per-task advisory lock so that within a task the sequence order is also the commit order: a consumer can resume from a cursor without missing an event. Across tasks the order is allocation order, not commit order. |
| Approval workflows | `approvals` with one-pending-per-task, plus `WAITING_APPROVAL` in the state machine. |
| Merge | every successful task leaves a reviewable commit on `aidev/<ref>`, and `worktrees` records branch, base commit and head commit; nothing merges yet, by design. |

Deliberately **not** present: a scheduler, a DAG executor, automatic merge, a web
dashboard, authentication, Redis, Kafka, Kubernetes, or an LLM inside aidev.

## Security model

### What aidev assumes about the agent

The agent is not trusted to be correct, and it is not contained. Phase 0 measured
OpenCode writing files in non-interactive mode with no permission prompt, and
OpenCode's own configuration lists the default agent's permission as
`{"permission": "*", "action": "allow"}` (docs/research.md §2.6, §7b). It runs with
the privileges of whoever started aidev.

The consequence is stated plainly because it decides everything else: **the git
worktree is the only boundary.** It is enforced by two checks before git is ever
invoked — the path must resolve inside `workspace_root`, and it must lie outside the
repository — both comparing physically resolved paths so a planted symlink cannot
satisfy a textual prefix. Worktree names are an allowlist of `[A-Za-z0-9._-]` rather
than a blocklist, because a name arrives from user input and an allowlist cannot be
defeated by an escape nobody anticipated.

The boundary is over *files*. It does not extend to git state, which every linked
worktree shares with the main repository (docs/research.md §7i): from inside its
worktree an agent can move a branch the operator's checkout is on, push into the
shared stash, or set `core.fsmonitor`/`core.sshCommand` so the operator's next plain
git command runs something the task chose. aidev does not sandbox those writes —
it *neutralises* them for its own commands (no fsmonitor, no hooks path, no
ext-diff on any git command aidev runs) and *detects* them after the agent: shared
config, hooks, attributes or HEAD changed means the attempt fails as
`CONTAINMENT` before verification runs; a foreign ref moving only emits a warning
(docs/research.md §7i.2). Detection is after the fact, so it widens the trust
rather than changing it — anyone who can create a task can have a verification
command run anything anyway, which is the next section.

### Verification commands are arbitrary code, deliberately

A task defines the commands aidev runs to verify it, and aidev runs them. That is
the design: the whole product rests on aidev executing real checks rather than
reading an agent's report.

It follows that **anyone who can create a task can run a command on this machine**,
including through MCP. That is not a loophole, it is the feature, and it bounds who
should be given access: aidev is a local-first tool for an operator working on their
own machine, and exposing its MCP server or its database to anyone you would not
give a shell to would be a mistake.

What is *not* available:

- **No shell.** Commands are argv, executed directly. `|`, `>`, `&&`, backticks and
  `$` are rejected at parse time with an explanation. This is not injection defence —
  there is no shell to inject into — but it removes a class of surprise where a
  pipe silently becomes a literal argument.
- **No command endpoint.** There is no tool or flag that takes a command and runs
  it. Commands exist only as part of a task, recorded with it and visible in its
  history.
- **No privilege of aidev's own.** A verification command can do nothing the person
  who started aidev could not already do.

### Every external process

`internal/procexec` is the only place a subprocess is created, and it gives all of
them the same guarantees: a required deadline, cancellation that signals the process
group rather than the pid, output bounded per stream with a truncation flag, and a
classified outcome that distinguishes "exited non-zero" from "we killed it at the
deadline" from "it never started".

A verification pass is bounded per step rather than in total, so its worst case is
`MaxVerificationSteps` (20) × the step timeout. A task that needs a tighter bound
should set per-step `timeout_seconds`.

### Secrets

The connection string is redacted wherever configuration is printed or logged
(`config.RedactURL`), and a test asserts a password does not survive it. Agent
and verification subprocesses drop `AIDEV_*` from the inherited environment —
`AIDEV_CONFIG` names the file holding the database URL, and the agent executes
instructions we do not control — while `PG*` and whatever database variables
the project exports for its own tests deliberately stay. The rest of the
environment is inherited (OpenCode needs `HOME` for its credentials) and is
never recorded: `worker_runs` stores the argv, which is task-defined, and not
the environment.

Dropping the variable is not the whole story: the agent runs with the
operator's privileges, so it can read the config file itself. That path is
closed only by the sandbox work (report item F). Until then, two things hold
the line: the environment drop above, and a guard in the database —
`tasks_transition_guard` (migration 0007) rejects any status transition the Go
state machine forbids — because the agent's inherited environment still
contains `PG*` credentials and could reach psql directly. The trigger is
load-bearing for exactly that reason, not a decorative backstop.

### MCP transport

stdio means stdout is the JSON-RPC channel. The logger has no stdout option at all,
and a test redirects `os.Stdout` to assert nothing reaches it, because this is the
rule a future change is most likely to break by accident.

### Who may decide that a task may run

Two gates, OR'd at run time in `run.enforceApproval` and deliberately never
merged into one stored flag: `tasks.requires_approval` is the *creator's*
explicit ask, `projects.requires_approval` (migration 0008) is the *operator's*
policy. Copying the project flag into tasks at creation would make the two
indistinguishable and would survive the policy being switched off again;
`task.approval_required` records `required_by` (`task`, `project`,
`task+project`) so the audit keeps them apart, and every decision event
records `via` (`cli` or `mcp`).

The surfaces are deliberately not equal. The project flag is writable only
from `aidev project approval` — there is no MCP tool that touches it — because
the party creating tasks must not decide whether its own work is gated. And
`mcp.allow_approval` is off by default: the MCP client may be the planner that
created the task, so `aidev_approve_task` refuses both granting and denying
until the operator turns it on, sending the reader to `aidev task approve`.
A gate that cannot read its own policy (the project lookup fails) blocks
rather than proceeds.

## Operating it

### PostgreSQL data, and why the compose project name is not pinned

There is no PersistentVolumeClaim, because there is no Kubernetes — that is an
explicit non-goal. The equivalent is a named Docker volume, `aidev-pgdata`, mounted
at `/var/lib/postgresql/data`. `make db-down` removes the container and keeps it;
`make db-reset` is the only thing that destroys data, and it says so. Verified by
stopping the stack and confirming every task survived the restart.

The compose project name is deliberately **not** pinned with a `name:` field, even
though leaving it unpinned has a visible cost: a `docker-compose.yml` exists in every
task worktree, so an agent that runs `docker compose up` there creates a second stack
named after the worktree directory and leaves an orphan volume behind
(`task-000013-a1_aidev-pgdata` was found this way, 0 bytes).

Pinning the name would stop the litter and introduce something far worse. An agent
runs shell commands inside its worktree; with a pinned project name, a
`docker compose down -v` in that worktree would target the operator's real database
and destroy it. The directory-derived name is accidental isolation, and accidental
isolation worth keeping is still worth keeping.

The litter is cleaned with `docker volume prune`, or by removing the named volume
directly. This is written down because the orphan looks like an oversight, and the
obvious fix for it is a hazard.

### When a run is interrupted

If aidev is killed mid-run — `kill -9`, a closed laptop, a container stopped — the
task is left `RUNNING` with an open attempt and a worktree on disk. Nothing will
pick it up again: a running task is not runnable, and aidev cannot know whether
another process is still working on it.

The way back is to cancel it:

```bash
aidev task list --status RUNNING,VERIFYING   # find them
aidev task cancel TASK-000001 --reason "aidev was killed mid-run"
aidev worktree list                          # the work is retained
aidev worktree remove TASK-000001 --force    # once you are done with it
```

Cancelling closes every open attempt and retains every active worktree, so the
partial work survives and the task's history stays coherent.
`TestRecoveryFromAnInterruptedRun` executes exactly this sequence.

A cancel also stops the running agent and verification rather than only
changing the database: a run on the same process is stopped immediately, and a
run in another process notices the `CANCELLED` status on its next poll (every 2
seconds by default) and stops then. The stopped run adopts the ending the
cancel already recorded instead of writing a second one.

aidev does **not** time out a stale `RUNNING` task on its own. A timeout that
declared a task dead while another process was still driving it would be worse than
a task an operator has to cancel deliberately, and there is no reliable way to tell
those apart without a lease mechanism — which the MVP does not have and which
concurrent workers will need.

### Reclaiming disk

`aidev worktree list` reports each worktree with its task, its status and its size,
and flags a record whose directory has disappeared. Removal goes through git without
`--force`, so uncommitted work is refused rather than discarded; forcing it is
recorded in the task's history as an operator's decision.

### A successful task's output

The work is a commit on `aidev/<ref>`. Nothing merges it, and nothing ever will
without a person asking:

```bash
git log --oneline aidev/TASK-000001
git diff main..aidev/TASK-000001
```

## Tracing

`internal/tracing` exports OpenTelemetry spans over OTLP HTTP. It is off unless an
endpoint is configured, and the disabled path returns a no-op tracer so instrumented
code never branches on whether tracing is on.

OTLP rather than a vendor SDK, for a reason that proved itself within an hour of
being written: when traces stopped appearing in Langfuse, pointing the same code at
Jaeger — one container, no credentials — established in two minutes that the
instrumentation was correct and the fault was in the backend's ingestion pipeline.
A vendor client would have left no way to make that distinction.

```bash
make jaeger-up     # one container, UI on :16686
make jaeger-env    # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
make langfuse-up   # six services, UI on :3000
make langfuse-env  # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
AIDEV_TEST_OTLP=1 go test ./internal/tracing/ -run TestExport -count=1
```

### What has been verified

**Against Jaeger and against self-hosted Langfuse.** A real task run produces one
trace: `aidev.task.run` over `aidev.worktree.create`, `aidev.agent.run` carrying the
agent's token usage as `gen_ai.usage.*`, and `aidev.verification` with a span per
step and its exit code. Langfuse maps the agent span to a generation and shows its
usage.

### Two things that made this look broken, recorded because both are easy to repeat

**Querying a removed endpoint.** Langfuse v4 in `events_only` mode has no
`GET /api/public/traces`; it answers **404** with a message saying so. The v4 read
path is `GET /api/public/v2/observations`. Worse than the wrong URL was the wrong
probe: a script that did `json.get("data", [])` turned that 404 body into "0 traces"
and produced a confident, false conclusion that ingestion was broken. Check the
status code.

**Leaking aidev's exporter configuration into the agent.** `procexec` inherited the
environment, so OpenCode — itself instrumented — received aidev's OTLP endpoint,
credentials and service name, and published its own internals to the operator's
backend as if they were aidev's. One run produced 1536 foreign spans against 8 of
aidev's. `OTEL_*` is now stripped from subprocess environments; `ExtraEnv` remains
the deliberate way to configure a child.

## What the MVP does not do

Stated plainly so that nobody has to infer it from absence.

| Not implemented | Where the seam is |
|---|---|
| Automatic retry | attempts are numbered and appended, `max_retries` is stored, the agent session id is recorded, and branch and worktree names already carry the attempt number. Only the `FAILED → READY` edge and a policy are missing. |
| Concurrent workers | status changes are compare-and-set and `tasks_ready_claim_idx` matches a `FOR UPDATE SKIP LOCKED` claim. A lease would also be needed, so that a crashed worker's task could be reclaimed without a human cancelling it. |
| Dependency graphs | `PENDING` exists as "not yet eligible"; the eligibility check in `becomeReady` is the hook. |
| Merging | a successful task leaves a reviewable commit on its own branch. Nothing merges it, by design. |
| Expiring a stale `RUNNING` task | deliberate, see [Operating it](#when-a-run-is-interrupted). |
| A second agent backend | `agent.Backend`, plus a server-mode OpenCode option evaluated and documented in docs/research.md §2.8. |
| A web surface, auth, multi-tenancy | explicit non-goals. aidev is a local-first tool for one operator. |

Known limitations that are not missing features:

- **Behaviour under concurrent `opencode run` invocations against worktrees of the
  same repository is unverified** (docs/research.md §5 of the unresolved list). The
  MVP is single-flight per task and does not depend on it; it must be settled before
  concurrent workers. The git half of the question is now measured for submodules:
  six concurrent `worktree add` invocations against one submodule repository all
  succeeded, and the resulting checkouts do not share a HEAD (docs/research.md §7h).
  What remains unverified is the agent, not git.
- **Submodules are read-only.** A task leaves one commit on one branch of one
  repository, so nothing inside a submodule is collected: content edited there is
  ignored, and the submodule's own history is never committed to or pushed. Only
  top-level submodules are loaded, not nested ones. An agent that moves a gitlink
  does produce a parent-repository change that the task's commit records, and the
  commit it points at may exist only in the local object store — review that as you
  would any submodule bump.
- **A verification pass is bounded per step, not in total.** Worst case is 20 steps ×
  the step timeout.
- **Migrations are forward-only.** There are no down migrations.
- **`aidev_run_task`'s default 120-second wait is a judgement, not a measurement.**
  The MCP client's own tool timeout was never measured.

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
| `CONTAINMENT` | the agent modified state shared with the main repository (config, hooks, attributes, HEAD, or broke base-commit ancestry) — checked after the agent, before verification |
| `INTERNAL` | aidev itself failed |
| `UNKNOWN` | unclassifiable — kept as an honest bucket rather than a plausible guess |

Phase 0 found that an unknown `--agent` makes OpenCode print a warning, fall back to
its default, and **exit 0**. Classification therefore never trusts the exit code
alone, and aidev validates agent names itself.
