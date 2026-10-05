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
        base check, when the task asks for it:
        commands already pass on the base
        commit ─────────────────────── FAILED (VERIFICATION)
        (agent never called; emits
         task.base_check_completed)
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

  With max_retries left, a failed check or an agent that stopped early (not a
  refused tool call) does not end the task: the attempt is FAILED, its work is
  committed (unverified) to its branch, the task goes back to READY, and the run
  opens the next attempt in the same directory on aidev/<ref>-aN
  ── see "Automatic retry" below.
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

## Running tasks side by side

Tasks run concurrently when you start them concurrently: several `aidev task run`
processes, or several `aidev_run_task` calls through MCP, each a process of its
own with its own attempt, lease, worktree and branch. Nothing in aidev picks READY
tasks up by itself; that would be a scheduler, which AGENTS.md rules out.

Two shared things had to be made safe for it (measured in docs/research.md §7k):

- **OpenCode's database.** Every OpenCode process keeps its sessions in one SQLite
  file, and runs started side by side on it die at random at startup with
  `database is locked` — one in eight in the measurement. The OpenCode backend
  sets `OPENCODE_DB` to `workspace_root/opencode-db/<ref>.sqlite`, one file per
  task, which every attempt of the task reuses: a session can only be continued
  from the database it was created in, and retry continues it. Listing agents at
  creation uses an in-memory database. `aidev task delete` removes the file.
- **The containment check.** Each run compares the repository's shared git state
  before and after its agent. With siblings running, their branches appear and
  move in the shared repository meanwhile, which used to raise
  `task.shared_refs_changed` on every run. Branches recorded for another attempt
  whose run overlapped this one (not finished, or finished after this attempt
  started) are now left out; a branch of a task that finished earlier, moved by
  the agent, is still reported (`tests/integration/parallel_test.go`).

Everything else was already per run: the compare-and-set on status, the lease
and its fence, the worktree under its own name, verification in that worktree.

## Automatic retry

Designed with the user on 2026-09-17, after a task whose agent stopped early had
to be retried by hand. A task's `max_retries` (0 to 10, default 0) is how many
more attempts one run may make when an attempt fails in a way another try can fix:

- the verification commands ran and at least one failed (`VERIFICATION`) — but not
  a refusal to run them (interception, protected paths), which another try would
  not change;
- the agent stopped early (`AGENT_EXIT`, `AGENT_ERROR`) — unless a tool call was
  refused, because a refusal ends the session and the same session would be
  refused again (`agent.ErrToolRefused`).

Cancellation, timeouts (an attempt that ran out of time would most likely do so
again, at the same cost), containment breaches, worktree and internal errors and a
passing base check are never retried.

The next attempt continues in **the same worktree directory**, because an OpenCode
session can only be continued in the directory it was created in
(docs/research.md §2.12):

1. The failed attempt's work — the snapshot taken when its agent finished, when
   there is one — is committed to that attempt's branch with a message marking it
   unverified, and the directory is reset to that commit, so whatever the failed
   checks wrote does not ride into the next commit (research A6).
2. In one transaction: the attempt is finished `FAILED` with its kind, its worktree
   record becomes `REUSED` with the partial commit as head, `task.retry_scheduled`
   is appended, and the task moves `RUNNING`/`VERIFYING → READY`. A Cancel that
   landed first wins, as everywhere.
3. The run opens the next attempt (`READY → RUNNING`, a new attempt row and
   lease — the cancel watch follows it), creates `aidev/<ref>-aN` at the partial
   commit with plumbing (`git branch`, `git symbolic-ref`; no hooks, no checkout),
   records a new worktree row for the same path, and takes a fresh containment
   baseline. The base check is not repeated; changed paths are still measured from
   the task's original base, so interception sees a runner any attempt rewrote.
4. The agent's session is continued (`-s`) with `prompts/retry_task.tmpl`: which
   attempt this is, why the last one failed, the tail of each failing check's
   output, and the commands that will judge it. With no session to continue, the
   whole task prompt comes first.

`FAILED` stays terminal: a task that retries never passes through it. The success
branch is the last attempt's, which the result names; its history holds the earlier
attempts' unverified commits. The CLI's total deadline grows with `max_retries`,
since every attempt runs inside the one `aidev task run` process.

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

What is committed is the agent's work as it stood when the agent finished, not
the worktree as verification left it (docs/research.md A6). Right after the
containment check, aidev snapshots the worktree to a tree object on a temporary
index — untracked files join, ignored files stay out, the real index is not
touched — and records it in `worktrees.agent_tree`. When verification passes, the
commit is built from that tree with `git commit-tree`, parented on the commit HEAD
was on at snapshot time (usually the base; an agent that committed its own work
moved it), and the branch is moved with `git update-ref` as a compare-and-set
against that same commit. A branch that moved in between fails the task with kind
WORKTREE instead of being overwritten. The real index is then reset to the new
HEAD so the worktree reads as clean when its files match the commit.

So a coverage file, a build product or a reformatted source that the checks wrote
never reaches the branch. When in-place checks do change the worktree, the run
records `task.verification_worktree_modified` listing what changed, because what
passed is then not exactly what was committed; the run is judged as usual, and the
worktree is usually `RETAINED` because the files left out make it dirty. Clean
verification checks out the same snapshot, so there "verified" and "committed" are
the same tree by construction.

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
| Concurrent workers | tasks already run side by side as separate `aidev task run` processes or MCP runs (see "Running tasks side by side"); a claiming worker pool would add `FOR UPDATE SKIP LOCKED` on `tasks_ready_claim_idx`. AGENTS.md rules out a scheduler, so none is built. |
| Another agent backend | `agent.Backend`; orchestration never names a backend. Codex was added exactly this way (`internal/agent/codex.go`, selected with `agent.backend` set to `"codex"`) and is **paused since 2026-09-15**: OpenCode is the backend in use. The code and its tests stay, so resuming is a configuration change. Codex took 6–8 minutes on trivial tasks and, in TASK-000029, read another project's virtualenv outside its worktree. A server-mode OpenCode backend would be another new file in `internal/agent`. |
| Sandboxed agent execution | `internal/procexec` is the one place processes start, and `agent.Request.WorkingDir` is the only path an agent is given, so containing the agent is a change to how a backend launches. Verification stays local whatever happens: a remote exit code is not aidev's own measurement (docs/opensandbox.md). **Parked as a future feature on 2026-09-15** — tasks are not yet complex enough to need it. Unmeasured: a worktree's `.git` file points into the main repository, so a container would need both mounted. |
| Dependency DAG | `PENDING` exists as "not yet eligible"; the eligibility check is the hook. |
| Observability / event-driven features | `events.seq` gives every event a position, and `AppendEvent` takes a per-task advisory lock so that within a task the sequence order is also the commit order: a consumer can resume from a cursor without missing an event. Across tasks the order is allocation order, not commit order. |
| Approval workflows | `approvals` with one-pending-per-task, plus `WAITING_APPROVAL` in the state machine. |
| Merge | every successful task leaves a reviewable commit on its branch (`aidev/<ref>`, or `aidev/<ref>-aN` after a retry), and `worktrees` records branch, base commit and head commit. Nothing merges on its own, by design; `aidev task apply` merges when a person asks, and `task undo` reverts it. |

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

### What the worktree is not

"The only boundary" is a statement about where aidev puts the work and which paths
its own git commands accept — not about what the agent can reach. The agent is a
process running as the same Unix user as aidev, with no sandbox (parked as a future
feature, docs/opensandbox.md). Whatever that user can do, the agent can do:

- read and write any file the user can: the main checkout, other tasks' worktrees,
  `~/.ssh`, the aidev binary and its `conf.json`;
- read the database password from that `conf.json` and connect to PostgreSQL
  directly — `AIDEV_*` variables are stripped from its environment, but the file is
  not hidden — so it could rewrite task rows; the transition trigger keeps an
  illegal status from being stored, but not a legal one;
- reach the network, start processes that outlive the run, and use any credential
  the user's environment holds.

What aidev adds on top is detection and refusal, not prevention: containment fails
an attempt that changed shared git state, interception and protected paths refuse
an attempt that rewrote what judges it, the lease fence stops a stale run from
writing, and every result is reviewed before it is merged. That is the right fit
for its intended use — one operator on their own machine delegating to an agent
they would otherwise run by hand — and the wrong one for an agent you do not
trust, other people's tasks, or a shared machine. Those need operating-system
isolation around the agent (a container or VM with only the worktree mounted, or
a separate user), which aidev does not provide today.

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
pick it up again: a running task is not runnable.

A run started from the MCP server is a separate `aidev task run` process in its
own session, so the cases are not symmetric: the server exiting — the client
disconnecting, a session ending — stops only the watching, never the run, which
keeps going with its own total deadline (task timeout + verification budget +
margin) and writes its stderr to a log under `workspace_root/run-logs/`. What
leaves a task `RUNNING` behind is the run process itself dying — and that is
exactly the case the lease below covers.

Every attempt therefore carries a lease: an owner (`hostname:pid:uuid`) and an
expiry, written when the attempt starts and pushed forward by the run's own
cancel poll (every 2 seconds by default, expiry 30 seconds ahead). A process that
is still working on the task keeps its lease alive; one that died stops renewing,
and the lease runs out.

The way back is to cancel the tasks whose lease has expired:

```bash
aidev task recover --dry-run                 # what would be cancelled
aidev task recover                           # cancel them
aidev task list --status RUNNING,VERIFYING   # the rest, if any
aidev worktree list                          # the work is retained
aidev worktree remove TASK-000001 --force    # once you are done with it
```

Recovery goes through the same path as a manual cancel, with the expired lease as
the recorded reason — it closes every open attempt and retains every active
worktree, so the partial work survives and the task's history stays coherent.
`TestRecoveryFromAnInterruptedRun` executes the manual sequence;
`TestRecoverCancelsTasksWithAnExpiredLease` the recovery one.

A cancel also stops the running agent and verification rather than only
changing the database: a run on the same process is stopped immediately, and a
run in another process notices the `CANCELLED` status on its next poll (every 2
seconds by default) and stops then. The stopped run adopts the ending the
cancel already recorded instead of writing a second one.

A lease is evidence, not a lock, so it is backed by a **fence**. A process that
was paused past its lease — or a run that has not yet polled — could otherwise
wake up after a recovery or a cancel and go on writing. Every write a run makes
while its attempt is open first takes `store.HoldLease` in its transaction: lock
the task row, then the attempt row (the order Cancel uses, so the two cannot
deadlock), and require the attempt to be `RUNNING` under this process. If it is
not, the transaction writes nothing and the run adopts the recorded ending.
Success and retry hold the fence across their git work too — the commit, the
branch update, the reset — so "the branch moved" and "the task succeeded" happen
under one lock, and a run that lost its attempt moves nothing. What stays
unfenced is the attempt's own audit trail, its worker-run and verification rows:
they record what really ran, which is what a reader needs most when someone else
ended the attempt. `TestARunThatLostItsAttemptPublishesNothing` holds this.

What a `kill -9` leaves at each boundary is tested against the real binary
(`tests/integration/crash_test.go`): killed while the agent runs, while the checks
run, or while waiting for the fence to record a success, the task is recoverable,
its work retained, and the branch untouched. One window remains and is
deliberate: the branch update happens inside the success transaction, so a
process that dies after it and before the commit leaves a verified commit on the
branch while the database still says `VERIFYING`; recovery then cancels the task.
The opposite order would risk the worse failure — a `SUCCEEDED` row with no
commit behind it.

Recovery is never fully automatic in the worker: a lease is evidence, and aidev
does not read it as an instruction. A human runs `aidev task recover` (doctor
warns when there is something for it to do), and the MCP server runs it once when
it first connects to the database, so a planner reconnecting after a restart does
not inherit a dead run. The alternative — declaring a task dead on a timer while
another process might still be driving it — would be worse than a task someone
must decide to cancel.

### Reclaiming disk

`aidev worktree list` reports each worktree with its task, its status and its size,
and flags a record whose directory has disappeared. Removal goes through git without
`--force`, so uncommitted work is refused rather than discarded; forcing it is
recorded in the task's history as an operator's decision.

`aidev doctor` measures the free space where worktrees are created and warns
below 5 GiB, failing below 1 GiB: every task copies the repository and its checks
may build there, and git that runs out of space fails half-way through a commit.
The space goes to retained worktrees (`aidev worktree list`, `worktree remove`),
captured output in the database (`aidev prune --logs-older-than 30d`, then
`task delete` for finished tasks nobody needs), the per-run logs under
`workspace_root/run-logs/` (plain files, safe to delete once their run ended), and
whatever the agents' builds left in Docker (`docker system df`). A run that hits a
full disk anyway fails with kind `WORKTREE` or `INTERNAL`, keeps its worktree, and
can be recovered like any other.

That is tested rather than asserted: `TestFullDiskDuringARunIsARecordedFailure`
fails each git write a run makes — creating the checkout, snapshotting the agent's
work, writing the commit, moving the branch — with `No space left on device`, and
requires a terminal task, a recorded failure kind and reason, a retained worktree,
nothing left mid-run for `aidev task recover`, and no stuck lease. The one thing a
run cannot record is a database that cannot be written at all: there is no disk on
which to record anything. The lease is what covers that case — the attempt's
renewals stop, and `aidev task recover` takes the task back.

### Backing up and restoring

The database is the record: tasks, attempts, verification evidence and the event
log. Everything else can be rebuilt or is somewhere safer — the delivered work is
commits on branches in your repositories, retained worktrees are scratch, and
`conf.json` is a file you can copy. Back up with the PostgreSQL tools of the same
major version as the server; with the container `aidev setup` starts:

```bash
docker exec aidev-postgres pg_dump -U aidev -d aidev -Fc > aidev-$(date +%F).dump
```

Do it before an upgrade that brings migrations — `aidev migrate` changes the
schema in place and nothing undoes it. To restore, into an empty database the
configuration points at:

```bash
docker exec -i aidev-postgres pg_restore -U aidev -d aidev --clean --if-exists < aidev-2026-10-05.dump
aidev migrate      # brings an older dump up to this binary's schema
aidev doctor
```

A restore rolls the record back; it does not touch git. Branches and worktrees
created after the dump stay on disk, and `aidev worktree list` will not know the
worktrees of tasks the dump does not contain — remove those by hand with
`git worktree remove`.

### A successful task's output

The work is a commit on `aidev/<ref>` (`aidev/<ref>-aN` after a retry; the result
names it). Nothing merges it, and nothing ever will without a person asking:

```bash
git log --oneline aidev/TASK-000001
git diff main..aidev/TASK-000001
aidev task apply TASK-000001     # when you want it: a merge commit into your branch
aidev task undo TASK-000001      # and back out again, with a revert commit
```

`apply` is the one command of aidev's that writes to your checkout, and only the
branch you have checked out. Its contract, held by tests in
`tests/integration/apply_test.go` and `apply_contract_test.go`:

- it acts only on the branch you have checked out, and refuses a detached HEAD;
- it refuses uncommitted changes to tracked files, naming them;
- it refuses unless the task is SUCCEEDED — `SUCCEEDED` is what verification
  found, and there is nothing else to deliver from;
- it merges the commit the run recorded, not the branch name: deleting the branch
  does not stop it, and deleting the commit does (refused, checkout untouched);
- a branch that moved on since the task ran is fine — merging brings the two sides
  together and rewrites neither — but a conflict is aborted with the files named
  and the checkout left exactly as it was;
- it refuses a second apply, and `undo` refuses when there is nothing to undo, or
  when the branch it would revert is not the one checked out;
- `undo` reverts with a new commit, never by rewriting: it works after a push, and
  a revert that would conflict is refused rather than forced;
- two applies at once serialise: one merges and the other is told the task is
  already applied.

It records `task.applied`. `undo` reverts that merge with a new commit, so it is safe after a
push; applying again after an undo reverts the revert, because merging a branch
whose merge was reverted would quietly change nothing. Neither changes the task's
status: `SUCCEEDED` is what verification found, not whether anyone took the work.

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
| A worker pool that claims tasks | tasks run side by side when you start them side by side, each with its own OpenCode database; nothing picks READY tasks up on its own — that would be a scheduler, which AGENTS.md rules out. |
| Dependency graphs | `PENDING` exists as "not yet eligible"; the eligibility check in `becomeReady` is the hook. |
| Automatic merging | a successful task leaves a reviewable commit on its own branch; `aidev task apply` merges it only when a person runs it. Nothing merges on its own, by design. |
| Automatic expiry of a stale `RUNNING` task | the lease makes staleness detectable and `aidev task recover` acts on it, but nothing cancels on a timer by itself — deliberate, see [Operating it](#when-a-run-is-interrupted). |
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
