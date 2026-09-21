# Changelog

Each version's section is the text of its GitHub release. Install or upgrade
with:

```bash
curl -fsSL https://raw.githubusercontent.com/rgb-vgx/aidev/main/install.sh | sh
```

## Unreleased

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
