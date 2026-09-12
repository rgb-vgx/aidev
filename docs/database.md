# Database

PostgreSQL holds aidev's authoritative state. Nothing important lives only in
memory: if the process dies mid-task, the task, its attempt, and everything
already observed about it are still on disk.

- Schema: [`migrations/0001_init.sql`](../migrations/0001_init.sql)
- Access layer: [`internal/store`](../internal/store)
- Domain types the tables mirror: [`internal/task`](../internal/task), [`internal/event`](../internal/event)

## Why these choices

**Application-generated UUIDv7 primary keys.** The writer knows the id before it
inserts, so it can build a whole object graph in one transaction without
round-tripping for generated keys. UUIDv7 is time-ordered, so inserts stay at the
right-hand edge of the index like a serial key, without tying identity to a
counter that a future second writer would contend on.

**A human-facing `tasks.ref` alongside the UUID.** People type task identifiers.
`TASK-000007` comes from a sequence with a column default, so it is assigned
atomically on insert. Lookups accept either form
(`Store.ResolveTask`), which keeps the "which kind of id is this?" question out of
the CLI and MCP layers.

**`TEXT` + `CHECK` for enumerations, not native enum types.** A `CHECK` list is
changed by an ordinary migration and rolls back cleanly; `ALTER TYPE ... ADD
VALUE` is more awkward to reverse and constrains how it can be combined with other
DDL. The cost is that the allowed values are written twice — once in Go, once in
SQL — so a test reads the migration and fails if the two ever disagree
(`TestEnumsMatchMigrationConstraints`). That test was verified to fail when a
value is removed from the constraint, not merely assumed to work.

**Bounded text plus a truncation flag.** aidev caps each captured stream before it
reaches the database (`MAX_OUTPUT_BYTES`). The companion `*_truncated` boolean
makes the cap visible in the record, so a reader is never misled into thinking
they are looking at complete output.

## Tables

### `projects`
A git repository aidev may run tasks against.

| Column | Notes |
|---|---|
| `id` | UUIDv7 |
| `name` | non-blank |
| `repo_path` | **unique**, must be absolute (`CHECK repo_path LIKE '/%'`) |
| `default_branch` | base for task branches when a task does not name one |

`repo_path` is unique so that registering the same repository twice is idempotent.
`EnsureProject` does insert-or-select in a single statement, so two callers racing
to register the same repo cannot make one of them fail.

### `tasks`
The unit of delegated work.

| Column | Notes |
|---|---|
| `id`, `ref` | machine and human identifiers |
| `project_id` | `ON DELETE RESTRICT` — a project with history cannot be dropped by accident |
| `title`, `description`, `acceptance_criteria` | title non-blank, ≤ 200 chars |
| `agent` | agent name passed to the backend (`build` by default) |
| `priority` | higher runs first, −1000..1000 |
| `status` | the lifecycle state, see below |
| `verification` | JSONB array of argv objects, **at least one required** |
| `max_retries` | recorded for a future retry feature; the MVP never retries |
| `requires_approval` | policy gate |
| `base_ref` | git ref to branch from; empty means the project default |
| `timeout_seconds` | 0 means "use the configured default" |

The constraint worth pointing at:

```sql
CONSTRAINT tasks_verification_is_nonempty_array CHECK (
    jsonb_typeof(verification) = 'array' AND jsonb_array_length(verification) >= 1)
```

A task aidev cannot independently verify is a task aidev cannot honestly report
on, so it is not storable. The domain rejects it first; this is the backstop for a
write path that forgets to ask.

Indexes:

```sql
CREATE INDEX tasks_project_created_idx ON tasks (project_id, created_at DESC);
CREATE INDEX tasks_ready_claim_idx     ON tasks (priority DESC, created_at)
    WHERE status = 'READY';
```

The partial index exists for a query the MVP does not yet run:

```sql
SELECT ... FROM tasks WHERE status = 'READY'
ORDER BY priority DESC, created_at
FOR UPDATE SKIP LOCKED LIMIT 1;
```

That is how a future scheduler will claim work without two workers taking the same
task. Shaping the index now means adding workers later is a code change, not a
migration under load.

### `task_attempts`
One execution of a task. Attempts are appended, never overwritten, which is what
makes history reconstructable and retry a matter of adding a row.

`UNIQUE (task_id, attempt_number)` is the real guard against two callers both
claiming attempt *n*: `NextAttemptNumber` reads the maximum, but only the unique
index decides the winner. Callers therefore run both in one transaction.

```sql
CONSTRAINT task_attempts_finished_consistent CHECK (
    (status = 'RUNNING' AND finished_at IS NULL) OR
    (status <> 'RUNNING' AND finished_at IS NOT NULL))
```

A finished attempt always has a finish time and a running one never does, so
"still running" and "finished, time not recorded" cannot be confused.
`FinishAttempt` additionally matches `WHERE status = 'RUNNING'`, so finishing an
attempt twice returns a conflict instead of quietly replacing the first outcome.

### `worktrees`
The isolated workspace an attempt ran in. `attempt_id` is **unique**: the
isolation boundary is per attempt, so a second worktree for one attempt would be a
bug rather than a feature.

```sql
CONSTRAINT worktrees_removed_consistent CHECK (
    (status = 'REMOVED' AND removed_at IS NOT NULL) OR
    (status <> 'REMOVED' AND removed_at IS NULL))
```

`RETAINED` is a deliberate state: it means the attempt failed or was cancelled and
the worktree was kept on disk for inspection. A retained worktree therefore has no
removal time. `ListRetainedWorktrees` is how an operator finds abandoned work.

### `worker_runs`
What an agent backend actually did: the argv, the captured streams, the exit code,
and the structured result where the backend reports one (`finish_reason`, `tokens`,
`cost`, `session_id`).

`session_id` is recorded but unused by the MVP. Phase 0 confirmed OpenCode can
resume a session by id, which is exactly what retry-with-context needs later
(docs/research.md §2.12); capturing it now avoids a migration then.

`diff` and `changed_files` are collected by aidev from git, not reported by the
agent. Phase 0 found that `git diff` omits untracked files, so the collection step
runs `git add -N .` first — otherwise the newly created files, which are the whole
point, would be invisible (docs/research.md §4.4).

Environment variables are deliberately **not** stored. The argv is task-defined
and safe to record; an environment can carry credentials.

### `verification_runs`
One row per verification step that aidev ran itself, with
`UNIQUE (attempt_id, step_index)` so results always map back to the step that
produced them.

`SKIPPED` is an explicit status for a step that never ran because an earlier one
failed. Without it, a missing row would be ambiguous between "skipped" and
"passed but not recorded".

### `approvals`
Human decisions gating a task.

```sql
CREATE UNIQUE INDEX approvals_one_pending_per_task ON approvals (task_id)
    WHERE status = 'PENDING';
```

At most one open request per task, enforced by the database rather than by a
read-then-write in application code. After a decision a new request may be opened,
so re-review is possible.

### `events`
The append-only history.

| Column | Notes |
|---|---|
| `seq` | `BIGSERIAL UNIQUE`, a total order independent of clock resolution |
| `task_id` | required; `ON DELETE CASCADE` |
| `attempt_id` | set for events belonging to a specific attempt |
| `type` | constrained to the known vocabulary |
| `payload` | JSONB, constrained to be an object |

`seq` exists because timestamps are not a total order: two events written in the
same millisecond would be unorderable, and history that cannot be ordered cannot
be replayed. Readers page with `seq > last_seen`.

`payload` must be a JSON object (`jsonb_typeof(payload) = 'object'`) so consumers
can always index by field name and new fields can be added without changing the
shape.

**UPDATE is rejected by a trigger. DELETE is not.**

```sql
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_reject_update();
```

Rewriting history is never legitimate — the way to correct the record is to append
a corrective event. Deleting a task together with its history *is* legitimate
(retention), and blocking `DELETE` would have made the `ON DELETE CASCADE` above a
promise the database could not keep. Both behaviours are covered by tests.

## Task lifecycle

```text
                    ┌──────────────────────────────┐
                    │                              ▼
  PENDING ──▶ READY ──▶ RUNNING ──▶ VERIFYING ──▶ SUCCEEDED
     │          │          │            │
     │          │          └────────────┼──▶ FAILED
     │          │                       │
     └──────────┴──▶ WAITING_APPROVAL ──┘
                            │
   any non-terminal state ──┴──────────▶ CANCELLED
```

The authoritative table is `transitions` in
[`internal/task/status.go`](../internal/task/status.go). Two properties are
asserted by tests rather than left to review:

- **`VERIFYING` is the only state that can reach `SUCCEEDED`.** An agent saying
  "tests pass" cannot move a task to success, because no edge exists from
  `RUNNING`.
- **Every non-terminal state can reach `CANCELLED`,** so no task can become
  unstoppable.

`FAILED → READY` (retry) is deliberately absent. Attempts are persisted so retry
can be added later; the edge is missing rather than present-and-unused, so adding
it is a deliberate change with a test to update.

### Concurrency

Status changes are compare-and-set:

```sql
UPDATE tasks SET status = $3 WHERE id = $1 AND status = $2
```

Zero rows affected means someone else moved the task first. `TransitionTask` then
re-reads to distinguish "no such task" (`ErrNotFound`) from "status moved"
(`ErrConflict`) and names the status it actually found. The domain state machine is
checked before the statement runs, so an illegal transition never reaches the
database.

## Migrations

Migrations are embedded in the binary (`migrations/embed.go`) and applied by
`aidev migrate`. There is no external migration tool.

- Files are named `NNNN_description.sql` and applied in lexical order, so the
  zero-padding is load-bearing. A test asserts lexical order equals apply order.
- Each migration runs in its own transaction **together with** its bookkeeping
  row, so a failure leaves the database at a known version rather than
  half-migrated.
- A session-level advisory lock serialises concurrent runners. Two aidev processes
  starting at once is ordinary; both trying to create the same table is not a
  useful failure.
- Applied migrations are checksummed. Editing a file that has already been applied
  is reported as an error, because a silently diverging schema is far harder to
  diagnose later than a refusal to start.

Never edit an applied migration. Add a new one.

## Operating it

```bash
make db-up            # start PostgreSQL on 127.0.0.1:5434 and wait for health
make migrate          # apply pending migrations
make test-db-create   # create aidev_test for integration tests
make test-integration # run every test, including database tests
make db-reset         # destroy the data and start clean
```

The host port is **5434**, not 5432: on the development machine both 5432 (a host
PostgreSQL service) and 5433 (another project's container) were already bound, so
a conventional default would have made the first `docker compose up` fail
(docs/research.md §5). Override with `AIDEV_DB_PORT`.

Integration tests skip themselves unless `TEST_DATABASE_URL` is set, so
`go test ./...` passes on a machine with no database rather than failing for an
environmental reason.
