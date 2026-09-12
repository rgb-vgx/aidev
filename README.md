# aidev

aidev is a local-first control plane for delegating implementation work to a
coding agent and **verifying the result yourself**.

A planner — Claude Code, or you at a terminal — hands aidev a task. aidev creates
an isolated git worktree, runs the agent inside it, then runs the task's own test
commands itself and decides the outcome from the exit codes. The agent's opinion
about whether it succeeded is not an input to that decision.

Everything — the task, each attempt, the captured output, the diff, the
verification results, and a full event history — is persisted in PostgreSQL.

> **Status: Phase 1 of 5.** The domain model, PostgreSQL persistence, migrations,
> configuration and logging are built and tested. Worktree isolation, the OpenCode
> backend, verification, the task CLI and the MCP server are not yet implemented —
> see [docs/architecture.md](docs/architecture.md#status). The commands documented
> below are the ones that exist today.

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
| `OPENCODE_MODEL` | *(empty)* | empty lets OpenCode choose, which works with no credentials |
| `OPENCODE_AGENT` | `build` | default OpenCode agent |
| `MAX_OUTPUT_BYTES` | `1048576` | per-stream capture limit; output beyond it is dropped and flagged |
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

## Commands available today

```bash
aidev version
aidev config [--json]     # print the resolved configuration
aidev migrate [--json]    # apply pending migrations
```

Task commands (`aidev task create|get|list|run|cancel|result`) arrive in Phase 3,
and the MCP server for Claude Code in Phase 4.

Human-readable output goes to **stdout**; structured logs go to **stderr**. This
separation is load-bearing: when aidev runs as an MCP server, stdout carries the
JSON-RPC protocol, so nothing else may ever be written there.

## OpenCode setup

Not required until Phase 2. What Phase 0 established about the installed version:

- aidev will invoke `opencode run --dir <worktree> --format json`, capturing
  newline-delimited JSON events.
- OpenCode works **without any API credentials** using its public
  `opencode/*` models, which report zero cost. `opencode models` lists them.
- The first run against an unfamiliar repository can take minutes; later runs take
  seconds.

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

## Documentation

| | |
|---|---|
| [docs/research.md](docs/research.md) | What the installed Claude Code, OpenCode, git and PostgreSQL actually do — measured, with assumptions and open questions marked |
| [docs/architecture.md](docs/architecture.md) | Package layout, design decisions and their costs, extension seams |
| [docs/database.md](docs/database.md) | Schema, constraints, lifecycle, concurrency, migrations |
| [AGENTS.md](AGENTS.md) | Conventions for agents (and people) contributing to this repository |
