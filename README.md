# aidev

aidev is a local-first control plane for delegating implementation work to a
coding agent and **verifying the result yourself**.

A planner — Claude Code, or you at a terminal — hands aidev a task. aidev creates
an isolated git worktree, runs the agent inside it, then runs the task's own test
commands itself and decides the outcome from the exit codes. The agent's opinion
about whether it succeeded is not an input to that decision.

Everything — the task, each attempt, the captured output, the diff, the
verification results, and a full event history — is persisted in PostgreSQL.

> **Status: Phase 3 of 5.** Usable from the terminal today: create a task, run it,
> read the result. The pipeline has been driven end to end against the real
> OpenCode, not only against a test double.
>
> The MCP server that lets Claude Code delegate tasks is Phase 4. See
> [docs/architecture.md](docs/architecture.md#status).

## Why it exists

An agent that reports its own success is not a source of truth. aidev's design
makes that structural rather than advisory:

- **The only route to `SUCCEEDED` is through `VERIFYING`.** The task lifecycle has
  no edge from "the agent finished" to "it worked", so a claim of passing tests
  cannot move a task to success.
- **A task with no verification command is rejected at creation.** aidev will not
  accept work whose outcome it could not establish.
- **The agent writes only inside a dedicated git worktree.** This is not a
  nicety: OpenCode was measured writing files in non-interactive mode with no
  permission prompt, so the worktree is the only containment boundary there is
  ([docs/research.md §2.6](docs/research.md)).
- **History is append-only.** The database rejects `UPDATE` on the event log.

The integration suite contains the test that states the whole idea: an agent that
reports *"All done! Tests pass."* without touching a single file produces a
**FAILED** task — its claim recorded verbatim, next to the verification result
that contradicts it.

## Architecture

```text
 Claude Code ──MCP(stdio)──▶ aidev ──▶ OpenCode ──▶ git worktree
  (planner)                    │                     (isolation)
                               │                          │
                               │                          ▼
                               │                   independent verification
                               │                   (aidev runs the commands)
                               ▼
                          PostgreSQL
                     (state + event history)
```

See [docs/architecture.md](docs/architecture.md) for the package layout and the
reasoning behind each decision, and [docs/database.md](docs/database.md) for the
schema.

## Requirements

| | Version | Notes |
|---|---|---|
| Go | **≥ 1.25** | the MCP SDK requires it; with Go 1.24 the toolchain downloads 1.26 automatically |
| Docker + Compose | any recent | PostgreSQL for local development |
| git | ≥ 2.23 | worktree support |
| OpenCode | 1.18+ | only needed from Phase 2; verified against 1.18.30 |
| Claude Code | 2.x | only needed from Phase 4; verified against 2.1.268 |

## Installation

```bash
git clone <this repository> aidev
cd aidev
make build            # produces bin/aidev
```

Or install onto your `PATH`:

```bash
make install          # go install ./cmd/aidev
```

## PostgreSQL setup

```bash
make db-up            # starts PostgreSQL and waits until it is healthy
```

This publishes PostgreSQL on **127.0.0.1:5434**, not the usual 5432. On the machine
aidev was developed on, 5432 was a host PostgreSQL service and 5433 was another
project's container, so a conventional default would have made the first
`docker compose up` fail. Override the port if 5434 is taken too:

```bash
AIDEV_DB_PORT=5440 make db-up
```

Then configure aidev and apply the schema:

```bash
cp .env.example .env          # then edit if you changed the port
export DATABASE_URL='postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable'
make migrate
```

`make migrate` is idempotent — run it as often as you like. Migrations are embedded
in the binary; there is no separate migration tool to install.

## Configuration

aidev reads its configuration from the environment. `DATABASE_URL` is the only
required variable; everything else has a default.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | *(required)* | PostgreSQL connection string |
| `WORKSPACE_ROOT` | `$XDG_DATA_HOME/aidev/worktrees` or `~/.local/share/aidev/worktrees` | where task worktrees are created; every worktree must resolve inside it |
| `DEFAULT_TASK_TIMEOUT` | `30m` | bounds one agent run |
| `DEFAULT_VERIFICATION_TIMEOUT` | `10m` | bounds one verification step |
| `OPENCODE_COMMAND` | `opencode` | the OpenCode executable |
| `OPENCODE_MODEL` | `opencode/muse-spark-1.3-contributor-free` | needs no credentials; set it to an empty string to let OpenCode choose |
| `OPENCODE_AGENT` | `build` | default OpenCode agent |
| `MAX_OUTPUT_BYTES` | `1048576` | per-stream capture limit; output beyond it is dropped and flagged |
| `WORKTREE_CLEANUP` | `on-success` | `on-success` commits and removes; `never` keeps every worktree. Neither discards failed work |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

The task timeout default is deliberately generous: OpenCode's first run against a
repository it has not seen was measured taking over four minutes before producing
any output, then seconds afterwards. A short default would make every fresh
developer's first task fail in a way that looks like a bug in aidev.

To see exactly what aidev resolved — with the database password redacted:

```bash
aidev config
aidev config --json
```

## Your first task

With PostgreSQL running and the schema applied, from inside any git repository:

```bash
aidev task create \
  --title "Add a Greet function" \
  --description "Create greet.go with Greet(name string) string returning \"Hello, \" + name" \
  --verify 'go test ./...' \
  --verify 'go vet ./...'
# created TASK-000001  Add a Greet function
#   run it with: aidev task run TASK-000001

aidev task run TASK-000001
```

That takes minutes, not seconds — most of it is OpenCode. While it runs, the event
log shows where it is:

```bash
aidev task events TASK-000001
#   1  task.created
#   2  task.ready
#   3  task.started
#   4  task.worktree_created
#   5  task.worker_started
```

A real run of exactly this, on a repository whose test did not compile until the
work was done:

```text
TASK-000001 succeeded: 2/2 verification steps passed, work committed on aidev/TASK-000001

agent (opencode)
  outcome       SUCCEEDED
  changed files 1
  took          10m56s
  says          Tests: The=": chúng, DP eng 들어 ஆக ree? (embцион upcoming…

verification (run by aidev)
  ✓ PASSED    go test ./...
  ✓ PASSED    go vet ./...

worktree
  REMOVED  /home/you/.local/share/aidev/worktrees/TASK-000001-a1
  branch  aidev/TASK-000001
```

Note the `says` line. That run used a free model whose closing summary was
incoherent — and it did not matter. The code it wrote was correct, and what
established that was aidev running `go test` and `go vet` itself. An agent's
account of its own work is recorded because it helps diagnosis, and it is never
evidence.

The other side of the same coin, with an agent that reported success and touched
nothing:

```text
TASK-000002 FAILED (VERIFICATION): verification did not pass: 0/1 steps passed

agent (opencode)
  outcome       SUCCEEDED
  changed files 0
  says          Done! I implemented the function and all tests pass.

verification (run by aidev)
  ✗ FAILED    test -f farewell.go  (exit 1)

the work was kept for inspection
  cd /home/you/.local/share/aidev/worktrees/TASK-000002-a1
```

`aidev task run` exits non-zero when a task does not succeed, so
`aidev task run TASK-000001 && ./deploy.sh` behaves as you would expect.

## Commands

```bash
aidev version
aidev config [--json]                 # the resolved configuration, password redacted
aidev migrate [--json]                # apply pending migrations

aidev task create --title T --verify CMD [--repo .] [--description D]
                  [--acceptance A] [--agent build] [--priority N]
                  [--requires-approval] [--base-ref REF] [--timeout 30m]
aidev task list   [--status S,S] [--repo .] [--limit N] [--json]
aidev task get    <task> [--json]
aidev task run    <task> [--json]
aidev task result <task> [--logs] [--json]
aidev task events <task> [--payload] [--after SEQ] [--json]
aidev task cancel <task> [--reason R] [--json]
aidev task approve <task> [--deny] [--by WHO] [--reason R] [--json]
```

`<task>` is either the reference (`TASK-000001`, case-insensitive) or the UUID.
Every command takes `--json` for machine-readable output; human-readable is the
default. Logs always go to stderr, so `aidev task get TASK-000001 --json | jq`
works while diagnostics stay visible.

Tasks marked `--requires-approval` stop before doing anything — no worktree, no
attempt — until `aidev task approve` releases them.

The MCP server for Claude Code arrives in Phase 4.

## What happens under the hood

```text
create task ──▶ isolate in a git worktree ──▶ run the agent there
                                                      │
  record in PostgreSQL ◀── verify independently ◀──────┘
                                   │
                    passed ──▶ commit to aidev/<ref>, remove the worktree
                    failed ──▶ keep the worktree for inspection
```

A successful task leaves a commit on its own branch, so the result is reviewable
with ordinary git:

```bash
git log --oneline aidev/TASK-000001
git diff main..aidev/TASK-000001
```

Nothing is merged, and nothing is ever committed to your working branch.

A failed task leaves its worktree exactly as the agent left it, under
`WORKSPACE_ROOT`, because partial work is often the most useful thing about a
failure. `git worktree remove` refuses to discard uncommitted work and aidev never
overrides that automatically, so this holds regardless of configuration. See
[the cleanup policy](docs/architecture.md#cleanup-policy).

Human-readable output goes to **stdout**; structured logs go to **stderr**. This
separation is load-bearing: when aidev runs as an MCP server, stdout carries the
JSON-RPC protocol, so nothing else may ever be written there.

## OpenCode setup

Install OpenCode and make it reachable on `PATH`, or set `OPENCODE_COMMAND`. No
API credentials are required: the public `opencode/*` models work and report zero
cost, which is how this project's end-to-end test runs without a key.

The default model is `opencode/muse-spark-1.3-contributor-free`, chosen by
measurement rather than by name. On identical trivial prompts it returned in
3.2–3.9s across repeated runs; the other free model varied between 3.9s and 100.5s
for the same work, which is the difference between a one-minute task and an
eleven-minute one. Set `OPENCODE_MODEL` to any model you have credentials for, or
to an empty string to let OpenCode decide.

```bash
opencode --version          # verified against 1.18.30
opencode models | head      # the opencode/* entries need no credentials
```

aidev invokes `opencode run --dir <worktree> --format json -- <prompt>` and reads
the newline-delimited event stream. You never write that command yourself; knowing
it is aidev's job, not the planner's.

Two measured behaviours worth knowing before your first task:

- **A task takes as long as the model does.** aidev's own share of a run —
  worktree, diff, verification, every database write — measured 0.2 seconds
  against agent times of 9 to 656 seconds. Free-tier latency is the variable that
  matters, and it is not always predictable, which is why
  `DEFAULT_TASK_TIMEOUT` is 30 minutes.
- **OpenCode writes files without asking**, even without its `--auto` flag. That
  is why every task runs in a dedicated worktree and why aidev refuses a worktree
  path that would land inside your repository.

See [docs/research.md](docs/research.md) for the measurements behind all of this.

## Claude Code MCP setup

Not required until Phase 4. The registration mechanism was confirmed in Phase 0:
a local server uses **stdio** transport, and `.mcp.json` in the project root looks
like this:

```json
{
  "mcpServers": {
    "aidev": { "type": "stdio", "command": "/abs/path/to/aidev", "args": ["mcp"], "env": {} }
  }
}
```

Exact instructions, the tool list, and their schemas will live in
`docs/mcp-tools.md` once Phase 4 lands.

## Development and testing

```bash
make check            # the gate: gofmt, go vet, staticcheck, unit tests
make test             # unit tests only
make test-integration # everything, including tests that need PostgreSQL
make test-e2e         # the real OpenCode, end to end (slow: minutes)
```

`make check` passes on a machine with no database: integration tests skip
themselves unless `TEST_DATABASE_URL` is set, so a failure always means a real
failure rather than a missing environment.

To run the database tests:

```bash
make db-up
make test-db-create        # creates the aidev_test database
make test-integration
```

Optional but recommended static analysis:

```bash
go install honnef.co/go/tools/cmd/staticcheck@latest
```

`make lint` uses it when present and says so when it is not.

Other useful targets: `make db-reset` (destroy the data and start clean),
`make db-logs`, `make help`.

### What the tests guard

The suite is not only coverage; several tests exist to hold specific invariants:

- `VERIFYING` is the only state that can reach `SUCCEEDED`.
- Every non-terminal state can reach `CANCELLED`, so no task is unstoppable.
- Go enumerations and the SQL `CHECK` constraints cannot drift apart — verified by
  confirming the test fails when a value is removed from the constraint.
- `UPDATE` on the event log is refused by the database; deleting a task still
  cascades its history.
- A task and its creation event commit together or not at all.
- An agent cannot produce a successful task by claiming success.
- A file written in a worktree does not appear in the repository, which stays clean.
- Collecting a diff reveals new files and does not stage anything in the worktree.
- A failed attempt's worktree survives; a successful one's work is committed first.
- Cancelling a task mid-run still records the cancellation, rather than leaving the
  row in `RUNNING`.
- Cancellation kills the whole process group, so a verification command's children
  do not outlive it.

Several of these were confirmed by deliberately breaking the implementation and
checking that the test failed, rather than by assuming a green test meant a real
guarantee.

## Documentation

| | |
|---|---|
| [docs/research.md](docs/research.md) | What the installed Claude Code, OpenCode, git and PostgreSQL actually do — measured, with assumptions and open questions marked |
| [docs/architecture.md](docs/architecture.md) | Package layout, design decisions and their costs, extension seams |
| [docs/database.md](docs/database.md) | Schema, constraints, lifecycle, concurrency, migrations |
| [AGENTS.md](AGENTS.md) | Conventions for agents (and people) contributing to this repository |
