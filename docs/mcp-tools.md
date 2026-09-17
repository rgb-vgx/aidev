# MCP tools

aidev exposes its orchestration to an MCP client — Claude Code in the intended
setup — as eight tools over **stdio**. The planner on the other side never learns
how OpenCode is invoked; it asks for a task to be created, run and verified, and
reads a structured result.

Implementation: [`internal/mcp`](../internal/mcp). Shapes:
[`internal/view`](../internal/view). Verified against Claude Code 2.1.268 and the
official Go SDK v1.7.0, protocol version **2025-06-18**.

## Registering the server with Claude Code

Build the binary first, then add it. aidev reads nothing but the conf.json that
`AIDEV_CONFIG` names, so the registration has to carry it: Claude Code launches the
server itself, and a variable exported in your shell is not present in that launch.

```bash
make install                                          # aidev on your PATH
claude mcp add --scope user -e AIDEV_CONFIG=/abs/path/conf.json aidev -- /abs/path/aidev mcp

claude mcp list
# aidev: /abs/path/aidev mcp - ✔ Connected
```

**`--scope user` is the one to use.** It registers aidev for every project, which
is how it is meant to be used: open Claude Code in whatever repository you are
working on, and delegate from there. `--scope local` confines the registration to
one project directory — useful only for trying it out. `--scope project` writes a
shareable `.mcp.json` in a repository root, and needs a one-time approval in the
Claude Code UI before it is connected to.

Nothing secret goes on the registration: `AIDEV_CONFIG` names the conf.json, and
aidev reads the database password out of it, so `~/.claude.json` holds no connection
string and the registration is the same on every machine. Use the same conf.json the
terminal commands use — the one `aidev config` reports.

The `.mcp.json` form, if you prefer to write it by hand:

```json
{
  "mcpServers": {
    "aidev": {
      "type": "stdio",
      "command": "/absolute/path/to/aidev",
      "args": ["mcp"],
      "env": {
        "AIDEV_CONFIG": "/abs/path/conf.json"
      }
    }
  }
}
```

Both shapes were confirmed by writing them with `claude mcp add` and reading the
files back (docs/research.md §3.2); neither was copied from documentation.

**Which settings affect the tools.** The server is the same aidev binary, so all of
conf.json applies, but the ones that shape the tools are `database.url` (every call
reads or writes PostgreSQL), `workspace_root` (where a run isolates its worktree),
`tasks.timeout` and `tasks.verification_timeout` (bound a run and its verification;
`timeout_seconds` on `aidev_run_task` overrides them for one task),
`tasks.max_output_bytes` (caps the streams a result can return), `agent.backend`
(opencode or codex), `agent.routing` (which model a task's hardness deserves), and
`tasks.worktree_cleanup` (whether a finished worktree stays).

**stdout belongs to the protocol.** aidev writes nothing to it in this mode; every
log line goes to stderr. A test asserts the logger cannot break that rule, because
one stray byte on stdout corrupts the JSON-RPC stream.

## How the tools fit together

```text
aidev_create_task ──▶ aidev_run_task ──▶ aidev_get_task_result
                          │                     ▲
                          │ still running?      │
                          └── aidev_get_task_events ──┘

aidev_list_tasks      find work
aidev_cancel_task     stop it
aidev_approve_task    release a gated task (a human decision)
```

Two things about `aidev_run_task` shape how it is used.

**It may return before the task has finished.** A run takes as long as the agent
does — measured between 9 and 656 seconds (docs/research.md §7c) — and an MCP client
will not wait indefinitely. The tool waits `wait_seconds` (120 by default) and then
returns with `still_running: true`. The run continues regardless, unaffected by the
call returning or by the client abandoning it. Poll `aidev_get_task_result`.

**`succeeded` is the field to read, not the status string.** It is true only when
the task reached `SUCCEEDED`, which requires aidev's own verification to have
passed.

---

## `aidev_create_task`

Creates a task. Does **not** run it.

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `repo_path` | string | **yes** | absolute path to the git repository |
| `title` | string | **yes** | one short line stating what to do |
| `verification` | string[] | **yes** | commands aidev runs itself to decide the outcome |
| `description` | string | no | the full instruction for the agent |
| `acceptance_criteria` | string | no | what done looks like, in prose |
| `agent` | string | no | agent to use; defaults to `build` |
| `priority` | integer | no | higher runs first; default 0 |
| `requires_approval` | boolean | no | gate the task behind a human decision |
| `base_ref` | string | no | git ref to branch from; defaults to the repository's current branch |
| `timeout_seconds` | integer | no | bound this task's agent run |
| `hardness` | string | no | how hard the task is: TRIVIAL, STANDARD or HARD; picks the model from `agent.routing` unless `model` is given |
| `model` | string | no | model to run on, overriding `agent.routing` and `agent.opencode.model` |

`verification` is required by the schema, not merely validated: a task aidev cannot
check is one it will not accept. The commands run in the task's worktree **without a
shell**, so `|`, `>` and `&&` are rejected with an explanation rather than passed
through as literal arguments.

### Output

| Field | Meaning |
|---|---|
| `task` | the created task, status `PENDING` |
| `next_step` | what to do next — running it, or requesting approval |

### Errors

- the path is not a git repository
- `verification` is empty, or a command contains a shell operator
- `base_ref` does not resolve — caught now rather than at worktree creation
- the agent name is unknown to the backend. This check exists because OpenCode
  accepts an unknown name, warns, silently uses its default and exits 0
  (docs/research.md §2.5), which would leave a record claiming an agent that never
  ran
- `title` blank or over 200 characters

### Side effects

Registers the repository as a project if it is not known yet, inserts the task, and
appends a `task.created` event — all in one transaction.

---

## `aidev_run_task`

Runs a task: creates an isolated worktree, runs the agent in it, collects the diff
from git, then runs the verification commands and records the outcome.

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `task` | string | **yes** | reference (`TASK-000001`) or UUID |
| `wait_seconds` | integer | no | how long to wait before returning; default 120, maximum 900, `0` returns at once |

### Output

| Field | Meaning |
|---|---|
| `result` | everything known so far, including the verification section |
| `still_running` | the wait elapsed and the run continues in the background |
| `succeeded` | true only if the task reached `SUCCEEDED` |
| `next_step` | review the branch, diagnose the failure, or poll |

### Errors

- no such task
- the task is already terminal, or already running
- `wait_seconds` negative

An approval gate is **not** an error: the result comes back with status
`WAITING_APPROVAL` and a `next_step` saying a human must decide. Nor is a run that
has not finished.

### Side effects

Creates a git worktree under `workspace_root` and a branch `aidev/<ref>`; runs the
agent, which writes files there; moves the task through `RUNNING`, `VERIFYING` and a
terminal state; appends events throughout. On success, commits the work to the task's
branch and removes the worktree. On failure, keeps the worktree for inspection. The
repository's own working tree is never touched.

Calling it again while a run is in flight joins that run rather than starting a
second or failing.

---

## `aidev_get_task_result`

Reads the outcome of a task's most recent attempt.

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `task` | string | **yes** | reference or UUID |
| `include_logs` | boolean | no | also return the agent transcript, the diff, and full verification output |

### Output

| Field | Meaning |
|---|---|
| `result.task` | the task and its status |
| `result.attempt` | the latest attempt: status, `failure_kind`, timing. Absent if it has never run |
| `result.worker` | what the agent did — including `summary`, which is its **claim**, and `changed_files`, which aidev counted from git |
| `result.verification` | one entry per step: command, status, exit code. **This is the evidence** |
| `result.worktree` | path, branch, and whether it was `REMOVED` or `RETAINED` |
| `result.approval` | the most recent approval record |
| `still_running` | the task is currently executing |
| `agent_stdout`, `agent_stderr`, `diff` | only with `include_logs` |

A failing step's output is included even without `include_logs`, trimmed to the last
2000 bytes, because it is the first thing anyone needs. A passing step's is not.

Absent sections are omitted rather than present and empty, so "has not run" is
distinguishable from "ran and produced nothing".

### Errors

No such task. A task that has never run is not an error.

### Side effects

None.

---

## `aidev_get_task_events`

Reads a task's history in order.

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `task` | string | **yes** | reference or UUID |
| `after_seq` | integer | no | only events after this sequence number |
| `limit` | integer | no | default 200, maximum 1000 |

### Output

| Field | Meaning |
|---|---|
| `events` | `seq`, `type`, `created_at`, `attempt_id`, `payload` |
| `count` | how many were returned |
| `last_seq` | pass it back as `after_seq` to continue |

Event types are listed in [docs/database.md](database.md#events). `seq` is a total
order independent of clock resolution, which is what makes resuming reliable.

### Errors

No such task.

### Side effects

None.

---

## `aidev_list_tasks`

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `repo_path` | string | no | only tasks for this repository |
| `statuses` | string[] | no | `PENDING`, `READY`, `RUNNING`, `VERIFYING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `WAITING_APPROVAL` |
| `limit` | integer | no | default 50, maximum 500 |

### Output

`tasks` newest first, and `count`.

### Errors

An unknown status is rejected and named. A repository with no tasks is an empty
result, not an error.

### Side effects

None.

---

## `aidev_get_task`

Input `task`; output `task`. Accepts a reference or a UUID, references being
case-insensitive. No side effects. Errors: no such task, with a pointer to
`aidev_list_tasks`.

---

## `aidev_cancel_task`

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `task` | string | **yes** | reference or UUID |
| `reason` | string | no | recorded in the task's history |

### Output

`task` and a `message` saying what happened, including that a worktree was kept.

### Errors

No such task; the task has already finished.

### Side effects

Moves the task to `CANCELLED`, closes any open attempt, marks any active worktree
`RETAINED`, appends `task.cancelled`. Work in progress is never discarded.

---

## `aidev_approve_task`

### Input

| Field | Type | Required | Meaning |
|---|---|---|---|
| `task` | string | **yes** | reference or UUID |
| `approve` | boolean | **yes** | `true` grants, `false` denies and fails the task |
| `decided_by` | string | no | who decided |
| `reason` | string | no | why |

`approve` is required rather than defaulted: approving has consequences and must not
happen because a field was omitted. **This is a human decision.** A planner should
not call it on its own initiative; the tool description says so.

### Output

`task`, the recorded `approval`, and a `message`.

### Errors

No such task; the task is not in `WAITING_APPROVAL`; there is no pending request.

### Side effects

Records the decision and moves the task to `READY` (granted) or `FAILED` (denied),
appending `task.approval_granted` or `task.approval_denied`.

---

## What is deliberately not exposed

- **Nothing that runs an arbitrary command.** Verification commands are part of a
  task and are recorded with it; there is no tool that takes a command and runs it.
- **No worktree manipulation.** No tool deletes a worktree or forces cleanup. A
  failed attempt's work is kept, and removing it is an operator's decision at a
  terminal.
- **No merge, and no push.** A successful task leaves a commit on its own branch;
  what happens to that branch is a human's call.
- **No configuration surface.** A planner cannot change `workspace_root`, the model,
  the agent command, or a timeout default.
- **No project or event writes.** Projects are registered as a side effect of
  creating a task, and the event log is append-only by construction.

## Testing it

`tests/integration/mcp_test.go` runs the real server over an in-memory transport
with a real MCP client, so the protocol, the generated schemas, the orchestrator,
git and PostgreSQL are all exercised together. Only the agent is faked.

```bash
make db-up && make test-db-create
make test-integration
```

The test worth reading is
`TestMCPReportsFailureWhenTheAgentOnlyClaimsSuccess`: an agent reports *"All done!
Tests pass."* without touching a file, and the tool returns `succeeded: false` with
that claim in `result.worker.summary` beside the failing verification step.
