# Changelog

Each version's section is the text of its GitHub release. Install or upgrade
with:

```bash
curl -fsSL https://raw.githubusercontent.com/rgb-vgx/aidev/main/install.sh | sh
```

## v0.12.0

Upgrading from v0.11.0: install the new binary; there are no migrations. Run
`claude plugin update aidev@aidev` (plugin 0.5.7) and restart Claude Code, so the
MCP server and the skills are the new ones; `aidev doctor` now names servers that
are still running a replaced binary.

This release confines a task's shell commands when asked, gives the checks and the
agent the task's own reference, and makes a retry deliver only the verified tree.

- **A task can run under bubblewrap, so its shell commands are confined too.**
  OpenCode's permission rules cover its own file tools, not the programs they
  start: a shell command the agent runs writes wherever the person can (research
  §7p). `agent.opencode.sandbox: "bwrap"` starts each OpenCode run under
  bubblewrap with the filesystem read-only except the worktree, its git admin
  directory, the task database directory, OpenCode's state directories and the
  directories named in `agent.opencode.sandbox_writable`; `/tmp` is a fresh tmpfs
  (measured with bubblewrap 0.9.0, research §7q). A runner's build caches must be
  listed in `sandbox_writable` — for example `/home/you/.cache/go-build` and
  `/home/you/go/pkg/mod` — or its builds fail on a read-only filesystem. The
  sandbox is off by default and needs bubblewrap; `sandbox_writable` takes
  absolute directories only. A sandbox that cannot start is a startup failure, and
  bubblewrap missing from `PATH` fails at start rather than running unconfined.
  Set both settings if you want the confinement.

- **`aidev doctor` finds missing project directories and servers on an old
  binary, and its verify mode advice is followable.** Two warnings are new
  (TASK-000179). "project paths" names registered projects whose directory is
  gone and points at `aidev project add` for a repository that moved; "mcp
  servers" names this user's aidev processes still running a binary replaced on
  disk (`/proc/<pid>/exe` "(deleted)", or another `aidev` at `argv[0]`), with
  their start time, and says to restart the sessions that started them; it is
  skipped off Linux. The verify mode warning leaves out projects whose directory
  is gone, because its advice cannot be followed there, and it now gives the
  syntax the CLI accepts: `aidev project verify-mode clean --repo <path>`. Act on
  both: register a moved repository again, and restart the sessions it names.

- **Creating a clean-verification task warns when the checks will have no
  `node_modules`.** Clean verification checks out only what would be committed, so
  ignored install output is absent; a step running `node`, `npm`, `npx`, `pnpm` or
  `yarn` in a repository whose base has a Node lockfile then fails unless a setup
  step installs the packages. Creating the task still succeeds but warns, naming
  the lockfile and its install command, on `task.created`, in the MCP create
  result and as "warning:" on the CLI (TASK-000184). TASK-000159 ran `node --test`
  with no `npm ci`, which kept Axiom-Office on `in_place`. Add a setup step that
  installs the packages, or keep verifying in place.

- **Setup and verification commands, and the agent, know the task's reference.**
  `AIDEV_TASK_REF` is now set for setup and verification steps (the base check
  included) and for OpenCode and Codex runs, added after inherited `AIDEV_`
  variables are dropped so it is the only one that gets through; `AIDEV_CONFIG`
  still never reaches a step or the agent (TASK-000186). A check can keep its
  per-task resources apart with it. `make test-integration` now uses
  `aidev_test_<task>` and creates it first when `AIDEV_TASK_REF` is set and
  `TEST_DB_URL` is not, while CI and a person at a terminal keep `aidev_test`;
  TASK-000166's agent had applied an unmerged migration to the shared database
  and broken every later run. Both guides document the variable.

- **A success on a retry is delivered as one commit on the base.** After a retry
  the delivery is one commit on the task's base carrying the verified tree, with
  the task's own message, rather than being stacked on the earlier attempt's
  unverified "not verified" commit; the earlier attempt's branch keeps that
  commit (TASK-000183). Merging TASK-000172 had brought "attempt 1, not verified"
  into main. A first attempt is unchanged, and an agent's own commits still stay
  under the delivery. If a retry's final tree equals the base, the branch is moved
  back to the base so it carries no unverified commit at all. Review the delivered
  branch, not a range.

- **A pushed release tag runs the integration suite before building a release.**
  The release workflow now runs `make db-up`, `make test-db-create` and
  `make test-integration` after `make check` and before any archive is built,
  against the same compose database as CI (TASK-000180). CI's integration job had
  been red on main for nine days unnoticed; a tag pushed then would have published
  a release whose integration suite fails. Nothing to do beyond treating a red
  release gate as a stop, not a step to retry.

- **The plugin's skills catch specs that contradict old tests, workarounds and a
  red CI (plugin 0.5.7).** The delegate skill now finds the tests that pin the old
  behaviour and updates them in the spec commit, keeps `protected_paths` off files
  the tests need to change, and tells the agent to report a conflicting test
  rather than special-case the code to keep it green. The review skill looks for a
  special case that only keeps an old test green, reads the agent's final message
  for reported conflicts, runs the slower suites on the merged tree rather than
  before the merge, and checks CI before merging. These come from four spec
  contradictions and the nine-day red CI of 2026-11-01 (TASK-000181). Run
  `claude plugin update aidev@aidev` and restart Claude Code to get them.

## v0.11.0

Upgrading from v0.10.0: install the new binary, run `aidev migrate` (migrations
`0025_files_added_event` and `0026_agent_progress_event`, both only widen the
event type check: an older binary keeps working on the migrated database), and
restart Claude Code so the MCP server runs the new binary.

Most of this release comes from running aidev against OpenCode 2: what its
configuration must carry, what a run records while the agent works, and what
creation refuses before the agent starts.

- **OpenCode 2 is supported.** OpenCode 2 removed `agent list` and `run --dir`,
  and a completed run may end with a text event instead of a `step_finish`
  "stop". aidev now detects the major version, lists agents with `debug agents`
  (retrying the cold start that prints `[]`), and starts runs with
  `--standalone`; the 1.x argument form is kept. OpenCode 2 resolves the project
  directory from `PWD`, not `getcwd()`, so a probe with `cmd.Dir` set still wrote
  `OK.txt` into the launch directory; every child command now gets `PWD` set to
  its working directory. Verified against 2.2.5; 1.18 is still supported.

- **Provider keys can be passed by environment reference, and `aidev doctor`
  checks they hold a key.** Every task gets its own `OPENCODE_DB`, and OpenCode 2
  keeps keys saved with `opencode auth` in that database, so a task using an
  `opencode-go` model failed every attempt in about a second with
  `provider.no-route` (TASK-000151). `agent.opencode.provider_key_env` maps a
  provider id to the name of an environment variable, and aidev passes
  `OPENCODE_CONFIG_CONTENT` with `{env:NAME}` references, so the key never
  appears in configuration, logs or the task record; a value that is not a
  variable name is refused without being repeated in the error. `aidev doctor`
  now fails the agent check when a named variable is missing — it said "ready"
  while the MCP server ran without `OPENCODE_GO_API_KEY` (TASK-000154). Set the
  setting and the variable if your models need a key.

- **OpenCode 2 runs now apply the directory rules.** OpenCode 2 ignores
  `OPENCODE_PERMISSION`, so since OpenCode 2 support every task ran with no
  `external_directory` rule; the write tool wrote outside the worktree
  (TASK-000175, measured in research §7p). The rules are now built once and
  passed both ways: `OPENCODE_PERMISSION` for 1.x, and
  `permission.external_directory` inside `OPENCODE_CONFIG_CONTENT` for 2.x,
  merged with the provider keys and the MCP switch-off. An inherited
  `OPENCODE_CONFIG_CONTENT` is dropped for 1.x runs too. A shell command the
  agent runs is still not confined; the rules cover OpenCode's own file tools,
  not the programs they start.

- **A task's agent no longer inherits the person's MCP servers.** A task ran with
  every MCP server the person had configured, aidev's own included (approve,
  run and cancel against the live database through that server's
  `AIDEV_CONFIG`), and a web-research server wrote `data/research.db` into the
  worktree, which TASK-000143 committed. Before each OpenCode 2 run aidev lists
  the servers with `opencode debug config` in the worktree and replaces each
  with a complete disabled entry in `OPENCODE_CONFIG_CONTENT`, merged with the
  provider keys; the names land in `Result.DisabledMCP`. A listing it cannot
  read stops the run as a startup failure, while a cancellation stays a
  cancellation. 1.x runs are unchanged.

- **OpenCode 2 error events are read, and an unreachable model is not retried.**
  An error event (`{"error":{"type","message"}}`) is now recorded with its type
  and message; TASK-000149's `provider.no-route` had become only "opencode
  reported: error". `provider.no-route` is classified `FailureStartup`, which
  the worker does not retry, so an unreachable model no longer burns an attempt.
  Other errors and the 1.x shape are unchanged.

- **A running agent reports its progress as `task.agent_progress`.** The event
  scanner calls `Request.OnProgress` on each tool call with the running
  tool-call and step counts, the tool, the path it was aimed at and the elapsed
  time; the worker records a report every 15 seconds as `task.agent_progress`
  (`tool_calls`, `steps`, `last_tool`, `last_path`, `elapsed_ms`) and as an
  "agent progress" line in the run's log, so `aidev task events` and run-logs
  show what a running agent is doing (TASK-000143: 20 minutes with nothing
  recorded). Only a call's location is taken, never a bash command line. The
  event is a report; the outcome does not depend on it. Migration
  `0026_agent_progress_event`.

- **An attempt names the files it adds as `task.files_added`.** Right after the
  agent's work is snapshotted, aidev records `task.files_added` when the
  delivered tree adds files to the base: each path with its size and whether git
  reads it as binary, at most 50 listed, with the total. It is a report; an
  error working it out is a warning and the run is judged as usual. TASK-000143
  delivered `data/research.db` and the history said only `changed_files: 2`.
  Sizes are now asked of `ls-tree` only about the added paths, in batches of
  200, with `--literal-pathspecs`, so a large repository no longer silently
  skips the report. Migration `0025_files_added_event`.

- **A run records the processes an agent left behind.** `task.worker_completed`
  now carries `orphans_killed: true` when the process executor stopped
  processes an agent left running (the key is absent otherwise), and the "agent
  finished" log line says so. The outcome is unchanged.

- **A verification step that would run command substitution no shell will
  execute is refused at creation.** Without a shell, `$(...)` or a backquote is
  literal text, and TASK-000154 spent both attempts on
  `test -z '$(gofmt -l internal)'`, which is always false. `ParseVerificationStep`
  now refuses `$(` or a backquote in an argument of a command that is not
  `sh`, `bash`, `dash` or `zsh`, and the message shows the line rewritten for
  `sh -c`. A shell named after a wrapper (`env X=1 sh -c`, `timeout 60 bash -c`)
  counts as a shell. Stored steps still load. If a step needs substitution, put
  it in `sh -c`.

- **A verification step that runs a file the base does not have is refused at
  creation.** Verification refuses a runner that changed during the attempt, so
  such a step can never be judged (TASK-000159 found out after six and a half
  minutes of agent work). `CreateTask` now refuses a setup or verification step
  that would run a file the base ref lacks, naming the step and the file. Which
  missing paths a step would load is decided by `verification.Interceptions`, so
  `test -f marker.txt` naming a missing file is still accepted, and a missing
  path git ignores is left to run-time interception. Commit the file the step
  runs, or change the step.

- **The model a task will run with is pinned when it is created.** `CreateTask`
  stores the task's own model, else the routing table for its hardness, else the
  configured backend's model. A task created without a model no longer changes
  model when the configuration is edited before its run or between attempts
  (TASK-000143 ran on a model `conf.json` no longer named). The worker sends the
  pinned model; with nothing configured, nothing is pinned.

- **`aidev doctor` warns about projects that still verify in place.** A new
  "verify mode" check after the stuck-tasks check counts and names the projects
  whose new tasks verify in place and points at `aidev project verify-mode
  clean --repo <path>`; it is ok when none do, skipped when the database does not
  answer, a redacted warning when the lookup fails, and never a failure.
  Projects registered before v0.10.0 kept `in_place` silently. The
  `--verify-mode` help and both guides no longer call `in_place`
  the default. Run the command it names for each project that still warns.

## v0.10.0

Upgrading from v0.9.0: install the new binary, run `aidev migrate` (migrations
`0023_clean_verify_default` and `0024_project_coverage`, both additive: an older
binary keeps working on the migrated database), run `claude plugin update
aidev@aidev` (plugin 0.5.6) and restart Claude Code.

Three changes to how a task is judged: what the guard refuses, what the result
reports, and where verification runs.

- **A verification step that would run an edited test is refused, whatever
  runner names it.** The interception guard modelled only a command that loads
  a file directly, so `python3 tests/wps/test_install_jsaddon.py` was refused
  while `node --test tests/wps/test_addin.mjs` ran, same agent, same attempt
  (TASK-000112, Axiom-Office). It now models the runners that collect test
  files: `go test` (package patterns, `./...` the whole module, none the
  working directory), `node --test` (files or directories, none the working
  directory recursively), and `pytest`/`py.test`/`python -m pytest` (paths to
  collect, none the rootdir, with a changed `conftest.py` counting when it is
  loaded for a collected test). The refusal uses a narrower classifier than
  the tests-changed report: a fixture under `testdata/` and the `Makefile`
  never refuse. A task that edits a test and the code it covers now fails, and
  that is the accepted cost — the remedy is to verify with something that is
  not the edited test.

- **A project can turn the coverage report on for all of its tasks.** `--coverage`
  reports, once verification passes, which of the changed Go lines the tests ran —
  but it only happens when whoever creates the task remembers the flag, and the
  problem it exists for (a green specification that never executes the changed
  code) is exactly the one that goes unnoticed when nobody remembers.
  `aidev project coverage on` turns the report on for every task created from then
  on (operator only, like `verify-mode`); each task freezes its own copy at
  creation, so a later change never moves the goalposts under existing tasks.
  `aidev task create --coverage` turns it on for one task, `--no-coverage` turns
  it off for one task against a project that has it on; passing neither takes the
  project's default, and the choice is frozen into the task (MCP `coverage` is
  tri-state the same way: empty takes the default). `aidev project list` shows
  the setting. Migration
  `0024_project_coverage` (additive: one column with a default, no existing row
  is touched).

- **A new project verifies in a clean checkout unless its repository pins
  submodules.** A verification pass must not be makeable by a file git ignores
  (a `.venv` the agent built, a vendored dependency, a cached build output), so
  tasks for a newly registered repository now verify in a fresh checkout of
  the tree a commit would carry. Clean verification refuses a repository
  pinning submodules, so those projects still default to `in_place`. Projects
  already registered keep the mode they have; `aidev project verify-mode` shows
  or changes it, and `aidev project add` says which default it chose and why.
  Migration `0023_clean_verify_default` (additive: only the column default
  changes, no existing row is touched).

## v0.9.0

Upgrading from v0.8.0: install the new binary, run `aidev migrate` (migrations
`0020_run_stopped`, `0021_read_dirs` and `0022_coverage`, all additive: an older
binary keeps working on the migrated database), run `claude plugin update
aidev@aidev` (plugin 0.5.5) and restart Claude Code.

Most of this release comes from running aidev on a second repository
(Axiom-Office): three ways a correct attempt was failed (ignored files under a
protected path, an editor writing the shared git config, a denied read ending
the agent's session), a cancel that left an application running, and work taken
with plain git that never counted as taken.

- **`--coverage` shows which changed Go lines no test ran.** Every dogfood
  round found defects in code the green specification never executed. With
  `aidev task create --coverage` (MCP `coverage`), once verification passes aidev
  runs the tests of the changed packages with a coverage profile, module by
  module, and `task result` says "coverage of the changed Go lines: N of M run by
  the tests" and lists the lines not run. A report only; it never changes the
  outcome. Migration `0022_coverage`; plugin 0.5.5 has the delegate skill ask
  for it on Go changes and the review skill start from it.
- docs/compatibility.md says what 0.x means in practice: which interfaces to
  rely on, how a change is announced, and what the migrations have promised.
- **Work taken with plain git counts as taken.** A planner often merges,
  cherry-picks or rebases a task's commit instead of running `aidev task apply`,
  and such a task stayed "waiting" forever: in Axiom-Office four of the six
  tasks `task list --unapplied` named were already on main. A SUCCEEDED task
  never applied through aidev whose commits are on the repository's default
  branch (an ancestor, or an equivalent patch per `git cherry`) is now
  `landed`, shown as `[landed in main]`, and left out of `--unapplied` and MCP
  `needs_apply`.
- **Creating a branch while a task runs no longer fails it as CONTAINMENT.**
  TASK-000094 failed because VS Code wrote `branch.<name>.vscode-merge-base` to
  the shared config when the planner created a branch mid-run. A config change
  is now judged by the keys that moved: per-branch tracking and editor
  bookkeeping are a warning (`config_keys` on `task.shared_refs_changed`); any
  other key still fails the attempt, and the breach names it.

- **A denied reach outside the worktree no longer ends the agent's session.**
  OpenCode's default for a tool call outside its directory is `ask`, which a
  headless run answers by rejecting the call and ending the session —
  TASK-000091 lost its run to one read of `/opt/kingsoft`. aidev now runs
  OpenCode with that permission denied instead: the agent gets a failed tool
  call and carries on (measured, research §7m).

- **`aidev project read-dirs [DIR...] [--clear]`** lets a repository's agent
  read named directories outside it, such as an installed program's files. CLI
  only, like approval and verify-mode. It refuses the root, the repository, the
  workspace and aidev's configuration, and warns about a directory you can
  write to: it grants reading, it cannot make a directory read-only (an
  OpenCode edit rule did not stop a write). Migration `0021_read_dirs`; plugin
  0.5.4 teaches the delegate skill about it.

- **A failed attempt says where its work is.** The agent does not commit (its
  prompt tells it not to): aidev commits the work itself once it passes, so a
  failed attempt's branch still points at the base. `task result` now says the
  agent's changes are in the worktree, uncommitted, and why. The README, the
  CLI help and the reference wrongly said that `--verify-mode clean` leaves out
  untracked or uncommitted files; it leaves out only files git ignores, and a
  test now pins that clean mode verifies and commits new untracked files.

- **A cancel stops the whole run and says so.** TASK-000092 was cancelled while
  WPS, started by the agent's test command in its own session, kept running:
  stopping a run reached only the agent's process group. The agent and every
  setup and verification command now carry a per-run environment marker, and
  stopping one also stops each process carrying it, wherever it moved (Linux). `aidev task cancel`
  waits for the process running the task to stop (`--wait`, 15 s by default),
  records `task.run_stopped`, and names a runner that did not stop, with how to
  stop it; the command then exits 1. Migration `0020_run_stopped`.

- **Protected paths no longer count files git ignores.** TASK-000093 was refused
  for `__pycache__/` and `tests/wps/out/` "changing" under `tests/**`: both
  ignored, both written by the checks the agent ran, none of it work the agent
  did. A protected path, and the "tests changed" report, now judge only what a
  commit would carry. A verification runner shadowed by an ignored file (a
  `.venv` the agent made) is still intercepted.

- **A refused tool call names what was refused.** When an agent ran a command in
  its worktree that read a path outside it, the result said "a tool call for
  <the worktree> was refused", naming the one place the agent was allowed to be.
  aidev now takes the path from OpenCode's own `permission requested` line, says
  the agent reached outside its worktree and which permission was turned down,
  and that the remedy is to put what it needs into the repository.

- Plugin 0.5.3: the delegate skill tells the planner never to send the agent to a
  path outside the repository, but to gather what it needs and commit it with
  the specification.

## v0.8.0

Upgrading from v0.7.0: install the new binary, run `aidev migrate` (migration
`0019_agent_version` adds one column with a default), run `claude plugin update
aidev@aidev` and restart Claude Code.

- **Every run records which agent version and model ran it.** `task result`,
  its JSON and the MCP result now show the model and what the agent's
  `--version` printed (`agent (opencode 1.18.35)`), so a task that passes one day
  and fails the next after an agent upgrade can be told apart from a flaky one.
  The version is read alongside the agent, so it costs a run no time, and a
  failed lookup never fails the run. Migration `0019_agent_version`.

- **`aidev doctor` names the cause when Docker did not start at boot.** Its own
  `docker` command used to wake Docker through `docker.socket` and then blame
  `database.url` while PostgreSQL was still starting. It now reads systemd
  first, waits for PostgreSQL when the check itself started Docker, says so,
  and says how to start Docker at boot. Plugin 0.5.2.

- The README documents every `task create` flag, including `--setup`,
  `--protect`, `--expect-fail-on-base` and `--verify-mode`.

## v0.7.0

Upgrading from v0.6.0: install the new binary and restart Claude Code. There is no
migration and no plugin change in this release.

A task listing now answers two questions it could not before: which repository a
task is for, and whether its verified result has been taken.

- **A task listing says which repository the task is for, and whether its result
  has been taken.** `aidev task list` now shows the repository and an apply
  marker (`[applied → main]`, `[undone]`), and the JSON adds `repo_path`,
  `apply_state`, `apply_into` and `apply_commit`. `task get`, `task result` and
  the MCP tools carry the same, and the MCP list gained `needs_apply`.

- `aidev task list --unapplied` answers the question an operator actually has:
  which `SUCCEEDED` tasks are still waiting to be applied (never applied, or
  applied and then undone). It contradicts `--status`, which it fixes itself.

- The state is derived from the event log — the newest `task.applied` or
  `task.apply_undone` — rather than stored a second time, so there is one record
  of what happened.

## v0.6.0

Upgrading from v0.5.0: install the new binary and restart Claude Code. There is no
migration and no plugin change in this release.

Both features were specified test-first and implemented by aidev itself, two tasks
running side by side.

- `aidev task run-log <task>` reads the log a detached run wrote under
  `workspace_root/run-logs/` — the file that holds the reason when a run dies
  before recording an ending, and that until now only the MCP tool's error
  message named. `--path`, `--all`, `--json`.

- **A repository with no commits says so.** Creating a task in a `git init` with
  nothing committed failed with "revision does not exist: main", naming a branch
  the person never made; it now names the reason and the remedy, writes nothing
  at all, and leaves a repository whose HEAD is an unborn branch alone (that one
  has commits, on its default branch, and the task is created from there).

## v0.5.0

Upgrading from v0.4.0: install the new binary and restart Claude Code. There is no
migration and no plugin change in this release.

A hardening release: three ways a run could still write after another process had
ended its attempt are closed, and what was only asserted about failure and
recovery is now tested.

- **A run that lost its attempt writes nothing at all.** Three writes were still
  outside the lease fence: recording the worktree (and its event) while creating
  it, recording the retry's worktree and branch while handing the worktree over,
  and marking a worktree retained while recording a failure. Each now goes through
  the fence, and when it refuses, the run undoes what it did outside the database:
  the checkout no longer removed is the one it had just made, and the retry's
  branch — which a cancelled attempt never used — is deleted rather than left
  behind for the next attempt to collide with.

- **A full disk is a recorded failure.** Failing each git write a run makes
  (creating the checkout, snapshotting the agent's work, writing the commit,
  moving the branch) with "No space left on device" produces a terminal task with
  a recorded failure kind and reason, a retained worktree to inspect, nothing for
  `aidev task recover` to take back, and no stuck lease. The one case a run cannot
  record is a database that cannot be written at all — the lease and `aidev task
  recover` cover that.

- **The contract of `aidev task apply` and `task undo` is written down** in
  docs/architecture.md and held by tests: they act only on the branch you have
  checked out, refuse a dirty checkout or a task that did not succeed, deliver the
  **commit** the run recorded rather than the branch name, merge a branch that
  moved on without rewriting either side, abort a conflict with the files named,
  and never force an undo that would conflict. Two applies at once serialise.

- **The durable-commit invariant is named**: a verified commit may exist on the
  branch while the database still says `VERIFYING` (a process killed inside the
  success transaction). Recovery cancels the task and never claims the commit, and
  `apply` refuses a task that is not `SUCCEEDED`, so the commit waits for a person.

## v0.4.0

Upgrading from v0.3.0: install the new binary, run `claude plugin update
aidev@aidev` and restart Claude Code. There is no migration in this release.

- **Tasks can run side by side.** Each task now gets its own OpenCode database
  (`workspace_root/opencode-db/<ref>.sqlite`, kept for its retries): on OpenCode's
  one shared store, runs started together died at random with `database is
  locked`. Branches that sibling tasks create meanwhile are no longer reported as
  shared refs changing.

- `aidev task diff <task>` shows what a task changed: the delivered diff, or the
  undelivered change of a failed attempt, marked as such.

- After a retry, `aidev task result` — and the MCP result — list the earlier
  attempts with their status, kind and error.

- Plugin 0.5.1: the review skill starts from `aidev task diff`.

## v0.3.0

Upgrading from v0.2.0: install the new binary, then run `aidev migrate` (one
migration, for the apply and undo events) and `claude plugin update aidev@aidev`,
and restart Claude Code. Back up the database first if you like — the new
"Backing up and restoring" section in docs/architecture.md shows how.

- **Apply and undo.** `aidev task apply <task>` merges a succeeded task's branch
  into the branch you have checked out, with a merge commit, and records it;
  `aidev task undo <task>` reverts it with a new commit, safe after a push. Both
  refuse a checkout with uncommitted changes, and a conflict is aborted with the
  files named and nothing changed. Applying again after an undo works.

- **A stale run can no longer write.** Every write of a run checks, under the
  task's row lock, that its attempt is still its own; a run that was paused past
  its lease or cancelled from another process stops publishing — no commit on
  the branch, no events after the ending. Crashes at each boundary of a run are
  now tested against the real binary.

- `aidev doctor` warns when the disk holding the worktrees is nearly full, and
  tells a stopped Docker daemon, a permission problem and a stopped database
  container apart.

- The documentation says plainly what the agent can reach — it runs as you,
  without a sandbox — and how to back up and restore the database.

- Plugin 0.5.0: the review skill offers apply and undo; the doctor skill covers
  every check, including stuck tasks and the disk.

## v0.2.0

Upgrading from v0.1.0: install the new binary, then run `aidev migrate` (twelve
migrations; the CLI refuses to run against a schema with pending ones) and
`claude plugin update aidev@aidev`, and restart Claude Code. Repositories you
have only used through MCP need `aidev project add <path>` once.

- **Automatic retry.** `--max-retries N` (MCP: `max_retries`, 0 to 10) lets a
  task try again, within the same run, when its checks fail or its agent stops
  early. The retry continues in the same worktree and agent session with the
  failing output in its prompt; the failed attempt's work is kept, marked
  unverified, on its own branch, and a success lands on `aidev/<ref>-aN` — the
  result names the branch. A refused tool call, an intercepted runner or a
  timeout is never retried.

- **What is committed is what the agent delivered.** The success commit is the
  worktree as it stood when the agent finished, not as the verification commands
  left it, so a `cover.out` or a build product the checks wrote never reaches the
  branch. When in-place checks change files, `task.verification_worktree_modified`
  says which.

- **MCP runs outlive the session.** `aidev_run_task` starts a separate
  `aidev task run` process with its own deadline and a log under
  `workspace_root/run-logs/`, so closing Claude Code no longer stops a task.
  Runs hold a lease; `aidev task recover` cancels tasks whose process died.

- **MCP only creates tasks for registered repositories.** Register one with
  `aidev project add <path>` (a repository you created a task for with the CLI is
  already registered), or set `mcp.auto_register_projects`. A planner that
  guesses a path can no longer send an agent into the wrong repository.

- **Bounded logs.** `include_logs` returns each section cut to 64 KiB with its
  full size and a `next_offset` to page on; the agent's event stream comes back as
  a plain-text transcript unless the raw `stdout` section is asked for.

- **The base commit is recorded at creation.** If the base ref moved before the
  task ran, the run says so (`task.base_moved`, `result.base_moved`) and goes
  ahead on the current code.

- **Retention.** `aidev prune --logs-older-than 30d` clears old captured output
  of finished tasks and keeps their records and history; `aidev task delete`
  removes a finished task whose worktree is gone.

- Plugin 0.4.0: the delegate skill suggests `max_retries 1` and pages long logs;
  the review skill reviews the branch the result names; the doctor skill explains
  an unregistered repository.

- **The judge is harder to fool.** `--protect <glob>` (MCP: `protected_paths`)
  refuses, before any check runs, an attempt that changed a ring-fenced path; an
  attempt that edits files that look like the tests judging it is reported
  (`task.verification_tests_modified`). `--verify-mode clean` runs the checks in
  a fresh checkout of the agent's result, so a file git ignores cannot make them
  pass; `--setup` adds preparation commands. `--expect-fail-on-base` runs the
  checks on the base commit first and fails a bug-fix task whose checks already
  pass there.

- **Containment.** An attempt that writes git state shared with the main
  repository — config, hooks, attributes, other refs — fails with kind
  `CONTAINMENT`; every git command aidev runs neutralises those vectors, and child
  processes no longer inherit `AIDEV_*` variables. Task status changes are
  guarded by a database trigger as well as in Go.

- **Approval policy per repository.** `aidev project approval on` gates every
  task of a repository; the MCP approve tool is off unless `mcp.allow_approval`
  is set, so a planner cannot approve its own tasks.

- **Sturdier runs.** The verification pass has a total time bound; truncated
  output keeps its tail; the process group is reaped on every run; a cancel that
  loses a race is retried; closing the terminal records a cancellation; the
  agent list is cached; events are ordered per task so a reader following a
  cursor cannot miss one.

- **Submodules**: a project whose repository keeps its sources in git submodules
  can now be delegated to. `aidev project submodules read_only` (per repository,
  off by default) gives every task worktree a checkout of each submodule at the
  commit the repository pins, so verification commands can read those sources.
  Each task gets its own checkout — a linked worktree of the submodule's own
  repository, not a re-clone from its remote — so two tasks on one repository do
  not share a submodule HEAD, and no network or credentials are needed.

  Read-only in this version: a task still leaves one commit on one branch of one
  repository, and nothing inside a submodule is committed. Nested submodules are
  not loaded. aidev does not fetch: if the repository pins a commit no local copy
  has, it says so and names where to fetch it.

- `aidev project list` shows the repositories aidev has run tasks against, with
  each one's default branch and submodule mode.

- **A run stopped by a refused tool call now says so.** A refusal ends an
  OpenCode session outright, which looked identical in the record to a model
  that quit halfway: both were reported only as "the session ended while the
  agent was still calling tools". The failure now names the refusal, counts them,
  and gives the path the agent was reaching for — and says when that path was
  outside the worktree, which means the task is asking for something that is not
  in the checkout. The failure kind is unchanged (`AGENT_ERROR`); a bash call's
  command line is deliberately left out of the message.

## v0.1.0

The first release: prebuilt binaries for Linux and macOS (amd64 and arm64),
installed by `install.sh` after checking `SHA256SUMS`.

aidev hands implementation work to a coding agent in an isolated git worktree
and decides the outcome by running the task's own verification commands. What
the agent says about its work is recorded, never trusted.

- **Tasks**: `aidev task create/run/get/result/events/cancel/approve`. A task
  needs at least one verification command; only those commands can make it
  `SUCCEEDED`. A success leaves one commit on `aidev/<ref>`; a failure keeps its
  worktree for inspection. The agent cannot pass by changing the verification
  runner (interception is refused).
- **Claude Code**: the `aidev` plugin (`claude plugin marketplace add
  rgb-vgx/aidev`, `claude plugin install aidev@aidev`) registers the MCP server
  and three skills: `aidev:delegate`, `aidev:review` and `aidev:doctor`.
- **Setup**: `aidev setup` starts PostgreSQL in Docker (bound to 127.0.0.1;
  `--postgres-image` for a private registry, `--postgres-volume` to keep
  `make db-up` data) or uses `--database-url`, writes a private conf.json,
  migrates, and prints the lines to add. It is safe to run again.
- **Diagnosis**: `aidev doctor [--json]` checks the configuration, git, the
  agent, the database, migrations and the workspace, with a fix for each
  failure and the password never shown.
- **Models**: OpenCode by default (`opencode/muse-spark-1.3-contributor-free`,
  no credentials needed); per-task model and hardness routing; `aidev stats`
  reports outcomes by model and hardness. A Codex backend exists but is not the
  default.
- **Observability**: optional OpenTelemetry traces (Jaeger or Langfuse), with
  the agent's token usage on its span.
- **Documentation**: README and README.vi.md, and an HTML guide in English and
  Vietnamese (`docs/guide`).

aidev is released under the MIT license.

Known limits: no Windows build (aidev relies on Unix process groups); the agent
is not sandboxed beyond its worktree; a failed task is not retried
automatically.
