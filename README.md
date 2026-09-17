# aidev

aidev is a local-first control plane for delegating implementation work to a
coding agent and **verifying the result yourself**.

A planner — Claude Code, or you at a terminal — hands aidev a task. aidev creates
an isolated git worktree, runs the agent inside it, then runs the task's own test
commands itself and decides the outcome from the exit codes. The agent's opinion
about whether it succeeded is not an input to that decision.

Everything — the task, each attempt, the captured output, the diff, the
verification results, and a full event history — is persisted in PostgreSQL.

> **Status: Phase 4 of 5.** Usable from the terminal and from Claude Code. The
> pipeline has been driven end to end against the real OpenCode, and the MCP server
> is registered and connected to the installed Claude Code. Phase 5 is hardening
> and the remaining documentation. See
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
make install                                            # puts aidev on your PATH
mkdir -p ~/.config/aidev
cp conf/conf.example.json ~/.config/aidev/conf.json     # then edit if you changed the port
export AIDEV_CONFIG="$HOME/.config/aidev/conf.json"     # put this line in your shell profile
aidev migrate
```

aidev reads nothing but the file that `AIDEV_CONFIG` names, so the export must be
present in every new terminal. Put that line in your shell profile
(`~/.bashrc`, `~/.zshrc`, or the equivalent for your shell) and this is one-time
setup. `aidev config` prints which file it used.

A conf.json holds the database password, so keep it out of git. Copy it to
`conf/conf.json` if you prefer — that path is already ignored — or just never
commit whichever path you use. `aidev config` redacts secrets, so it is safe to
read back.

`aidev migrate` is idempotent — run it as often as you like. Migrations are
embedded in the binary; there is no separate migration tool to install.

## Configuration

aidev reads its configuration from one JSON file: the conf.json that `AIDEV_CONFIG`
names. Nothing else in the environment is consulted for configuration — a variable
left over in a shell profile must not quietly win over the file someone is reading
and editing.

Every setting is optional except `database.url`. The defaults below are what the
binary uses when a key is absent; conf/conf.example.json shows them all in one file.

| Setting | Default | Purpose |
|---|---|---|
| `database.url` | *(required)* | PostgreSQL connection string |
| `workspace_root` | `~/.local/share/aidev/worktrees` | where task worktrees are created; every worktree path must resolve inside it |
| `tasks.timeout` | `30m` | bounds one agent run when the task does not set its own |
| `tasks.verification_timeout` | `10m` | bounds one verification step |
| `tasks.max_output_bytes` | `1048576` | per-stream capture limit; output beyond it is dropped and flagged as truncated |
| `tasks.worktree_cleanup` | `on-success` | `on-success` commits the work and removes the worktree; `never` keeps every worktree. Neither discards failed work |
| `agent.backend` | `opencode` | which agent implementation runs tasks: `opencode` or `codex` |
| `agent.opencode.command` | `opencode` | the OpenCode executable |
| `agent.opencode.model` | `opencode/muse-spark-1.3-contributor-free` | needs no credentials; set it to an empty string to let OpenCode choose |
| `agent.opencode.agent` | `build` | default OpenCode agent |
| `agent.codex.command` | `codex` | the Codex executable |
| `agent.codex.profile` | *(none)* | passed as `--profile` when set |
| `agent.codex.model` | *(none)* | passed as `-m` when set; empty lets Codex choose |
| `agent.codex.sandbox` | `workspace-write` | passed as `--sandbox` when set |
| `agent.routing` | *(none)* | an object mapping a task hardness (`TRIVIAL`, `STANDARD`, `HARD`) to the model that hardness deserves; a hardness with no entry leaves the choice to the backend |
| `log_level` | `info` | `debug`, `info`, `warn` or `error` |
| `tracing.endpoint` | *(none)* | base OTLP HTTP URL; tracing is off when nothing is set |
| `tracing.traces_endpoint` | *(none)* | the full URL the traces exporter posts to, taking precedence over `tracing.endpoint` |
| `tracing.headers` | *(none)* | an object of extra OTLP headers, such as `Authorization` |
| `tracing.service_name` | `aidev` | the service name the spans carry |
| `tracing.sample_ratio` | `1` | the fraction of new traces sampled, between 0 and 1 |

`agent.routing` and `tracing.headers` are objects, not scalars: their entries are
values you write, not further settings.

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
`workspace_root`, because partial work is often the most useful thing about a
failure. `git worktree remove` refuses to discard uncommitted work and aidev never
overrides that automatically, so this holds regardless of configuration. See
[the cleanup policy](docs/architecture.md#cleanup-policy).

Human-readable output goes to **stdout**; structured logs go to **stderr**. This
separation is load-bearing: when aidev runs as an MCP server, stdout carries the
JSON-RPC protocol, so nothing else may ever be written there.

## OpenCode setup

Install OpenCode and make it reachable on `PATH`, or set `agent.opencode.command`. No
API credentials are required: the public `opencode/*` models work and report zero
cost, which is how this project's end-to-end test runs without a key.

The default model is `opencode/muse-spark-1.3-contributor-free`, chosen by
measurement rather than by name. On identical trivial prompts it returned in
3.2–3.9s across repeated runs; the other free model varied between 3.9s and 100.5s
for the same work, which is the difference between a one-minute task and an
eleven-minute one. Set `agent.opencode.model` to any model you have credentials for, or
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
  `tasks.timeout` defaults to 30 minutes.
- **OpenCode writes files without asking**, even without its `--auto` flag. That
  is why every task runs in a dedicated worktree and why aidev refuses a worktree
  path that would land inside your repository.

See [docs/research.md](docs/research.md) for the measurements behind all of this.

## Claude Code MCP setup

This is what aidev is for: Claude Code plans and reviews, aidev isolates, runs and
verifies.

```bash
make install                                          # aidev on your PATH
claude mcp add --scope user -e AIDEV_CONFIG=/abs/path/conf.json aidev -- /abs/path/aidev mcp

claude mcp list
# aidev: /abs/path/aidev mcp - ✔ Connected
```

**Use `--scope user`.** It registers aidev for every project, which is the point:
you open Claude Code in whatever repository you are working on and delegate from
there. `--scope local` would confine it to one project directory, and `--scope
project` writes a shareable `.mcp.json` that each person must approve once.

No credentials go on the registration: `AIDEV_CONFIG` names the conf.json, and
aidev reads the database password out of it, so `~/.claude.json` holds no
connection string. Use the same file and path that `aidev config` reports, so the
server starts configured. Eight tools become available:

| Tool | Purpose |
|---|---|
| `aidev_create_task` | create a task; verification commands are required |
| `aidev_run_task` | isolate, delegate, verify, record |
| `aidev_get_task_result` | the outcome, with the verification evidence |
| `aidev_get_task_events` | history, and progress while a task runs |
| `aidev_list_tasks` / `aidev_get_task` | find work |
| `aidev_cancel_task` | stop a task; its worktree is kept |
| `aidev_approve_task` | a human releases a gated task |

A run takes minutes, so `aidev_run_task` waits a bounded time and then returns with
`still_running: true` while the task continues; the planner polls
`aidev_get_task_result`. The field to read is `succeeded`, which is true only when
aidev's own verification passed.

### Or install the Claude Code plugin

The repository is also a plugin marketplace. The `aidev` plugin registers the same
MCP server and adds two skills that teach Claude the workflow around it:
`aidev:delegate` (agree what "done" means, write failing tests on a `spec/*` branch,
keep tasks small, delegate, recover from a failed task) and `aidev:review` (check
the diff beyond the green tests, merge, report in the user's terms).

```bash
make install                                   # the plugin runs `aidev mcp` from PATH
export AIDEV_CONFIG="$PWD/conf/conf.json"      # in your shell profile; the plugin passes it on
claude plugin marketplace add /abs/path/to/aidev
claude plugin install aidev@aidev

claude mcp list
# plugin:aidev:aidev: aidev mcp - ✔ Connected
```

The plugin's server reads `AIDEV_CONFIG` from the environment Claude Code starts
in, so export it in your shell profile. Use either the plugin or the `claude mcp
add` registration above, not both: with both, every tool is listed twice.

Two things to know before delegating work in a real repository.

**Tell the agent which verification command fits.** In a monorepo, `go test ./...`
from the root is rarely the right check. Name the one that actually covers the
change: `--verify 'go test ./backend/...'`, or a service's own test command.

**The worktree is a clean checkout: only tracked files.** Dependencies that live
outside git are not there. `go test` is fine, because the module cache is shared,
but `npm test` or `pytest` will fail in a fresh worktree unless the task's
verification installs what it needs first — for example
`--verify 'npm --prefix web ci'` before `--verify 'npm --prefix web test'`.

Full schemas, errors and side effects: **[docs/mcp-tools.md](docs/mcp-tools.md)**.

### This has been run, not only wired up

Claude Code was given a repository with a test that did not compile, told to use
only the aidev MCP tools, and explicitly denied `Edit`, `Write`, `Read` and `Bash` —
so the work could not have come from anywhere but aidev. It created and ran the
task, then reported back:

```text
The task is TASK-000008 and it succeeded (status SUCCEEDED, first attempt, about 9 seconds).

| Command         | Exit code | Result                        |
| go test ./...   | 0         | PASSED: ok  demo (cached)     |
| go vet ./...    | 0         | PASSED, no output             |

What changed: one new file, reverse.go. reverse_test.go was not modified.
Where the work is: committed on branch aidev/TASK-000008 (commit 1aeca30).
Nothing was merged into your branch, and the task's worktree has been removed.
```

Checked afterwards without trusting any of it: the task is in PostgreSQL with both
verification steps at exit code 0, the repository's own working tree is clean and
has no reverse.go, the branch has it, and `go clean -testcache && go test` run by
hand on that branch passes `TestReverse`. The whole exchange took 35 seconds.

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

## Seeing what a task did (optional)

aidev exports OpenTelemetry traces when an OTLP endpoint is configured, and nothing
at all when one is not. One trace per task run, with the agent invocation carrying
its token usage.

```bash
make jaeger-up   # one container, UI on :16686
make jaeger-env  # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
aidev task run TASK-000001
```

For an LLM-oriented view with cost, self-hosted Langfuse works too — six services,
so it is started separately:

```bash
make langfuse-up   # six services, UI on :3000
make langfuse-env  # prints the tracing object to paste into the conf.json that AIDEV_CONFIG names
make langfuse-credentials   # the bootstrapped UI login, on :3000
```

A real run looks like this:

```text
aidev.task.run            10.97s
  aidev.worktree.create    0.02s
  aidev.agent.run         10.86s   usage {input: 9580, output: 457}
  aidev.verification       0.08s
    aidev.verification.step   0s   go test ./... → exit 0
```

Which also shows where the time goes: the agent was 10.86 of 10.97 seconds.

Two notes from getting this working. Langfuse v4 removed
`GET /api/public/traces` — use `GET /api/public/v2/observations`, and check the HTTP
status rather than reading an empty `data` field out of an error body. And aidev
deliberately does **not** pass its OTLP configuration to the agent: OpenCode is
instrumented too, and one run otherwise filed 1536 spans of its own internals into
your backend under aidev's name.

## When something goes wrong

**aidev was killed while a task was running.** The task is stuck in `RUNNING`, and
nothing will pick it up again. Cancel it:

```bash
aidev task list --status RUNNING,VERIFYING
aidev task cancel TASK-000001 --reason "aidev was killed mid-run"
```

The partial work is kept. aidev deliberately does not expire a stale `RUNNING` task
by itself — see [the reasoning](docs/architecture.md#when-a-run-is-interrupted).

**The workspace is filling up.** Failed tasks keep their worktrees on purpose:

```bash
aidev worktree list                        # with tasks, statuses and sizes
aidev worktree remove TASK-000001          # refused if work is uncommitted
aidev worktree remove TASK-000001 --force  # discard it deliberately
```

**There are Docker volumes named after tasks.** An agent ran `docker compose` inside
its worktree, where a copy of `docker-compose.yml` exists, and Compose named the
project after the directory. They are empty litter rather than data:
`docker volume prune` removes them. The project name is left unpinned on purpose —
see [the reasoning](docs/architecture.md#postgresql-data-and-why-the-compose-project-name-is-not-pinned).

**Is the database persistent?** Yes: a named volume, `aidev-pgdata`. `make db-down`
keeps it and only `make db-reset` destroys it. There is no PersistentVolumeClaim
because there is no Kubernetes.

**A task failed and you want to know why.**

```bash
aidev task result TASK-000001 --logs   # verification output, agent transcript, diff
aidev task events TASK-000001          # what happened, in order
```

**A task is slow.** Almost all of that is the model, not aidev — measured at 0.2
seconds of aidev's own work against 9 to 656 seconds of agent time. Check
`aidev task events` to see which stage it is in, and consider a faster model.

## Documentation

| | |
|---|---|
| **[docs/guide/index.html](docs/guide/index.html)** | **Start here if you are new.** A four-page guide written for a first week: what aidev is, getting started, debugging, and a full reference. Open it in a browser |
| [docs/research.md](docs/research.md) | What the installed Claude Code, OpenCode, git and PostgreSQL actually do — measured, with assumptions and open questions marked |
| [docs/architecture.md](docs/architecture.md) | Package layout, design decisions and their costs, extension seams |
| [docs/database.md](docs/database.md) | Schema, constraints, lifecycle, concurrency, migrations |
| [docs/mcp-tools.md](docs/mcp-tools.md) | Every MCP tool: input, output, errors, side effects, and what is deliberately not exposed |
| [AGENTS.md](AGENTS.md) | Conventions for agents (and people) contributing to this repository |
