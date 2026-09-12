# Phase 0 — Environment Research

**Date of investigation:** 2026-09-13
**Host:** Ubuntu 24.04.5 LTS (Noble), kernel 6.8.0-139-generic, x86_64

Every claim below is tagged:

| Tag | Meaning |
|---|---|
| **[OBSERVED]** | Reproduced by running a command on this machine during Phase 0. Output captured. |
| **[DOCUMENTED]** | Stated by the tool's own `--help` / OpenAPI schema, but not exercised. |
| **[ASSUMPTION]** | Design decision taken in absence of proof. Must be revisited if it breaks. |
| **[UNRESOLVED]** | Open question deliberately deferred. |

No flag, endpoint, or config key in this document was copied from the task brief.
Everything was read off the installed software.

---

## 1. Installed versions

| Component | Version | How determined |
|---|---|---|
| Claude Code | `2.1.268` | `claude --version` **[OBSERVED]** |
| OpenCode | `1.18.30` | `opencode --version` **[OBSERVED]** |
| Go (system) | `go1.24.12 linux/amd64` | `go version` **[OBSERVED]** |
| Go (effective) | `go1.26.8` (auto-downloaded toolchain) | see §7.1 **[OBSERVED]** |
| git | `2.43.0` | `git --version` **[OBSERVED]** |
| Docker | `29.8.0` | `docker --version` **[OBSERVED]** |
| Docker Compose | `v5.5.1` | `docker compose version` **[OBSERVED]** |
| psql client | `18.6` | `psql --version` **[OBSERVED]** |

OpenCode binary lives at `/home/thuyetmt/.opencode/bin/opencode` (native ELF executable,
not a Node wrapper) **[OBSERVED]**.

---

## 2. OpenCode: how it is actually invoked

### 2.1 Relevant subcommands **[OBSERVED]** (`opencode --help`)

```
opencode run [message..]     run opencode with a message
opencode serve               starts a headless opencode server
opencode attach <url>        attach to a running opencode server
opencode agent list          list all available agents
opencode models [provider]   list all available models
opencode export [sessionID]  export session data as JSON
```

### 2.2 Flags of `opencode run` that aidev relies on **[OBSERVED]** (`opencode run --help`)

| Flag | Purpose | Used by aidev |
|---|---|---|
| `--dir <path>` | directory to run in | **yes** — this is the worktree isolation hook |
| `--format json` | raw JSON events instead of formatted text | **yes** — structured result |
| `-m, --model provider/model` | model selection | yes, optional |
| `--agent <name>` | agent selection | yes, optional |
| `-s, --session <id>` | continue an existing session | recorded now, used by future retry |
| `-c, --continue` | continue the last session | no (non-deterministic) |
| `--attach <url>` | attach to a running server | no (see §2.8) |
| `--auto` | auto-approve permissions not explicitly denied | **no** — see §2.6, it is not needed |
| `--print-logs`, `--log-level` | diagnostics to stderr | yes, on demand |

There is **no** `--output-format`, `--json`, `--cwd`, `--timeout`, or `--max-turns` flag.
The correct spellings are `--format json` and `--dir`.

### 2.3 Working directory **[OBSERVED]**

`--dir` is honoured and is the only mechanism needed. Proof:

```
opencode run --dir <worktree> --format json \
  "Create a file named greet.go ... with Greet(name string) string"
→ exit 0 in 16s
→ <worktree>/greet.go created
→ main repo: `git status --porcelain` EMPTY, no greet.go
```

An invalid `--dir` fails fast: exit `1`, stderr `Error: Failed to change directory to <path>`,
**zero bytes on stdout** **[OBSERVED]**.

### 2.4 Structured output: NDJSON event stream **[OBSERVED]**

`--format json` emits **newline-delimited JSON**, one event per line (not a single JSON
document). Envelope:

```json
{"type":"<event>","timestamp":1789233980042,"sessionID":"ses_...","part":{...}}
```

Event types observed in real runs:

| `type` | Meaning | Notable `part` fields |
|---|---|---|
| `step_start` | model turn begins | `snapshot` (git-like hash) |
| `tool_use` | a tool ran | `tool`, `callID`, `state.status`, `state.input`, `state.output`, `state.title`, `state.time` |
| `text` | assistant prose | `text`, `time.start/end` |
| `step_finish` | model turn ends | `reason` (`stop` \| `tool-calls`), `tokens{input,output,reasoning,cache}`, `cost`, `snapshot` |
| `error` | run failed | `error.name`, `error.data.message`, `error.data.ref` |

A complete successful file-creating run was 6 lines. Real example of `tool_use`:

```json
{"type":"tool_use","part":{"type":"tool","tool":"write",
 "callID":"call-8d365897-...",
 "state":{"status":"completed",
   "input":{"filePath":"<worktree>/greet.go","content":"package main\n..."},
   "output":"Wrote file successfully.","title":"greet.go",
   "time":{"start":1789233980024,"end":1789233980039}}}}
```

**Implication for aidev:** parse line-by-line, tolerate unknown `type` values (forward
compatibility), take the last `text` event as the agent's summary, take `step_finish.reason`
+ `tokens` + `cost` as the structured result, and treat any `error` event as failure.

### 2.5 Exit codes and failure classification **[OBSERVED]**

| Scenario | Exit | stdout | stderr |
|---|---|---|---|
| success | `0` | NDJSON events, ends with `step_finish reason=stop` | empty |
| invalid model (`-m bogus/nope-123`) | `1` | one `{"type":"error",...}` NDJSON line | empty |
| invalid `--dir` | `1` | **empty** | `Error: Failed to change directory to ...` |
| unknown `--agent` | **`0`** | normal events | warning: `agent "x" not found. Falling back to default agent` |
| SIGTERM during run | `143` | partial NDJSON | empty |

Two classification lessons:

1. **Exit code alone is insufficient.** An unknown agent name yields **exit 0** and silently
   runs the default agent. aidev must validate the agent name itself rather than trust
   OpenCode to reject it.
2. Failure information arrives on **two different channels** — an `error` event on stdout for
   in-session failures, plain text on stderr for startup failures. aidev must capture and
   classify both.

### 2.6 Permissions: `--auto` is not required, and that is a security fact **[OBSERVED]**

Running a file-creating prompt **without** `--auto`:

```
opencode run --dir <worktree> --format json "Create a file noauth.txt containing hello"
→ exit 0, noauth.txt created, tool_use write status=completed, no prompt, no block
```

In non-interactive `run` mode OpenCode **writes files without asking**. `--auto` changes
nothing for this path, so aidev does **not** pass it (passing a flag named "dangerous!" with
no benefit would be pure downside).

**Consequence:** there is no agent-side containment. The git worktree is the *only* boundary
between the agent and the user's real working tree. This is the single most important
security finding of Phase 0 and it is why `--dir` pointing at a worktree is a hard invariant
in aidev, not a convenience.

### 2.7 Cancellation **[OBSERVED]**

Tested carefully, because a first measurement was misleading.

```
opencode run ... &            # direct child, no setsid
ps --ppid <pid>               → NO descendants; single process, own pgid
kill -TERM <pid>              → exit 143
file count frozen at 2 for the following 15s
remaining opencode processes  → none belonging to this run
```

**`opencode run` is a single process with no children, and SIGTERM stops it and its work
promptly.** So `exec.CommandContext` + process-group kill is sufficient for aidev.

> **Correction of an earlier measurement, recorded for honesty:** an initial test appeared to
> show an orphan `opencode -s ses_...` process that kept creating files after the kill. It was
> traced (`/proc/<pid>/cmdline`, PPID chain) to the **machine owner's own interactive OpenCode
> TUI session** running in a Konsole window with cwd `~/Documents/paper` — entirely unrelated
> to the probes. There is no orphan problem. aidev must nevertheless only ever signal process
> groups it created itself, never match on process name.

### 2.8 Server mode **[OBSERVED]** — available, and deliberately not used in the MVP

`opencode serve --port <p>` starts a headless HTTP server in ~2s and exposes a large
OpenAPI surface (`GET /doc`, 479KB spec). Endpoints exercised during research:

| Endpoint | Result |
|---|---|
| `GET /api/health` | `{"healthy":true}` |
| `POST /api/session` body `{"location":{"directory":...},"model":{"providerID","id"}}` | creates session, returns `data.id` = `ses_...` |
| `POST /api/session/{id}/prompt` body `{"prompt":{"text":...}}` | **async**, returns immediately with `admittedSeq` + message id |
| `GET /api/session/{id}/message` | full message history incl. assistant `content[]`, `finish`, `tokens`, `cost`, `snapshot` |
| `POST /api/session/{id}/interrupt` | `204`, and the agent's work verifiably stops |
| `POST /api/session/{id}/wait` | **`503 ServiceUnavailableError: "Session wait is not available yet"`** — not implemented in 1.18.30 |
| `opencode run --attach <url>` | works, 6s, same NDJSON on stdout, exit 0 |

`POST /api/session` returned `projectID: b2d43534d0aedd28df35746ab0c64343220fb9e9`, which is
exactly the **initial commit SHA** of the probe repository **[OBSERVED]**. OpenCode therefore
identifies a "project" by git root commit, which has a useful consequence: **all worktrees of
one repository share a project identity** (see §2.9).

**Decision: the MVP uses one-shot `opencode run`, not server mode.** Justification:
- `run` already provides everything the MVP contract needs: exit code, stdout, stderr,
  structured NDJSON, cancellation (§2.7).
- Server mode would add process-lifecycle ownership (start, health-wait, port allocation,
  shutdown, crash recovery) for no MVP capability gain, and `/wait` is unimplemented so
  completion detection would require SSE or polling — strictly more code and more failure modes.
- Server mode's one genuine advantage is `/interrupt`, and §2.7 shows SIGTERM already achieves
  deterministic cancellation.

The `AgentBackend` interface keeps this reversible: an `OpenCodeServerBackend` can be added
later without touching orchestration. Recorded as a future option, not a limitation.

### 2.9 Cold start: a real timeout hazard **[OBSERVED]**

The very first `opencode run` against a brand-new project **did not complete within 240s**,
producing zero stdout and stopping after logging `message=init`. A second attempt at 90s also
hung at `init`. Afterwards — once `opencode serve` had run once against the same directory —
every invocation completed in **5–16s**, including plain `opencode run` with no server present.

- **[OBSERVED]** first-ever invocation for an unseen project can exceed 4 minutes; subsequent
  invocations take seconds.
- **[OBSERVED]** worktrees of an already-known repository are *not* cold: the 16s worktree run
  was the first ever in that worktree directory, and it was fast — consistent with project
  identity being the git root commit (§2.8).
- **[ASSUMPTION]** the cold cost is one-time per *project* (repository), not per directory.
- **[UNRESOLVED]** exactly what `init` does. Candidates: SQLite schema/migration work on the
  293MB `~/.local/share/opencode/opencode.db` (+33MB WAL observed), or first-time index build.

**Implication for aidev:** the default agent timeout must be generous and configurable
(`DEFAULT_TASK_TIMEOUT`), and documentation must tell a fresh developer to warm OpenCode once
against the target repository before relying on short timeouts.

### 2.10 Model availability without credentials **[OBSERVED]**

`opencode providers list` reports `0 credentials` (`~/.local/share/opencode/auth.json`), and
no provider API keys exist in the environment. **Inference still works**, because the
`opencode/*` zen models are public:

```
GET /api/model → ... "api":{"url":"https://opencode.ai/zen/v1"},
                     "request":{"body":{"apiKey":"public"}} ...
```

Verified end-to-end with `opencode/nemotron-3.5-lightning-free`: prompt "reply PONG" returned
`text: "PONG"`, `finish: "stop"`, `cost: 0`.

Free models listed **[OBSERVED]**: `big-pickle`, `ling-3.0-flash-fin-free`, `mimo-v2.5-free`,
`muse-spark-1.2/1.3-contributor-free`, `nemotron-3-ultra-free`, `nemotron-3.5-lightning-free`.

Omitting `-m` entirely also works — OpenCode auto-selects **[OBSERVED]** — so
`OPENCODE_MODEL` is optional in aidev's configuration.

**Consequence:** aidev's E2E test can exercise a real OpenCode run on this machine with no
API key and no cost. Tests still default to a fake backend for determinism (§6).

### 2.11 Agents **[OBSERVED]** (`GET /agent`)

| Agent | Mode | Relevance |
|---|---|---|
| `build` | primary | the default; executes tools → **the implementation agent** |
| `plan` | primary | "Disallows all edit tools" → useful read-only/analysis mode |
| `explore`, `general` | subagent | not invoked directly |
| `compaction`, `summary`, `title` | primary | internal housekeeping |

aidev maps its `Task.Agent` field onto this namespace, defaulting to `build`.

### 2.12 Session continuation **[OBSERVED]**

```
run 1: "Remember the secret number 4271. Reply OK."  → sessionID ses_f69534640ffeZtuI27AghGGUpT
run 2: opencode run -s ses_f69534640ffeZtuI27AghGGUpT "What was the secret number?"
       → "4271", same sessionID
```

Context survives across separate process invocations. aidev persists the session id on every
`TaskAttempt` now, so retry-with-context can be built later without a schema change. The MVP
does not resume sessions.

---

## 3. Claude Code: MCP registration

### 3.1 Mechanism **[OBSERVED]** (`claude mcp --help`, `claude mcp add --help`)

```
claude mcp add [options] <name> <commandOrUrl> [args...]
  -s, --scope <scope>          local | user | project   (default: local)
  -t, --transport <transport>  stdio | sse | http       (default: stdio)
  -e, --env <env...>           e.g. -e KEY=value
```

Supported transports are exactly `stdio`, `sse`, `http`. **For a local server, stdio is the
correct and default transport** — no port, no URL, no auth.

### 3.2 Exact config file shapes **[OBSERVED]**

Verified by adding throwaway entries, reading the files, then removing them
(both files confirmed returned to `{}` afterwards).

`--scope project` writes **`.mcp.json` in the project root** — this is the committable,
share-with-the-team form:

```json
{
  "mcpServers": {
    "aidev": {
      "type": "stdio",
      "command": "/abs/path/to/aidev",
      "args": ["mcp"],
      "env": {}
    }
  }
}
```

`--scope local` writes **`~/.claude.json`** under `projects["<absolute project path>"].mcpServers`,
with the identical inner object shape (`type`/`command`/`args`/`env`). `-e FOO=bar` lands in `env`.

Other verified facts: `claude mcp list` health-checks each server and prints
`✔ Connected` / `✘ Failed to connect — <reason>`; unapproved `.mcp.json` servers show as
`⏸ Pending approval` **[DOCUMENTED]** (from `claude mcp get`/`list` help text), i.e. a
project-scope server requires a one-time user approval in the UI.

### 3.3 Hard constraint on the aidev MCP server **[OBSERVED, inferred from mechanism]**

stdio transport means **stdout is the JSON-RPC channel**. Any stray byte written to stdout by
aidev — a log line, a fmt.Println, a library warning — corrupts the protocol stream.

**Rule enforced in aidev:** in `aidev mcp` mode, *all* logging goes to **stderr**. This is an
architectural constraint, not a style preference.

### 3.4 MCP protocol version and Go SDK **[OBSERVED]**

The official SDK exists and works. `github.com/modelcontextprotocol/go-sdk`, latest stable
**`v1.7.0`** (`v1.8.0-pre.2` is a prerelease; not used).

A minimal stdio server was built and driven with hand-written JSON-RPC to prove the whole path
before committing to it:

```
→ initialize (protocolVersion 2025-06-18)
← {"result":{"capabilities":{"logging":{},"tools":{"listChanged":true}},
             "protocolVersion":"2025-06-18","serverInfo":{"name":"probe","version":"0.0.1"}}}
→ tools/list
← tools[0] = {name:"probe_create",
              inputSchema:{type:object, properties:{title:{type:string,description:"task title"}},
                           required:["title"], additionalProperties:false},
              outputSchema:{type:object, properties:{id:...}, required:["id"], ...}}
→ tools/call {name:"probe_create", arguments:{title:"demo"}}
← {"content":[{"type":"text","text":"{\"id\":\"T-demo\"}"}],
   "structuredContent":{"id":"T-demo"}}
```

Confirmed capabilities:
- negotiated protocol version **`2025-06-18`**;
- `mcp.AddTool[In, Out]` **infers both input and output JSON Schema from Go types**, with
  property descriptions taken from `jsonschema:"..."` struct tags — so aidev's tool schemas are
  generated from the domain types rather than hand-written and drifting;
- responses carry both human-readable `content` and machine-readable `structuredContent`.

Gotcha found while testing **[OBSERVED]**: if stdin reaches EOF immediately the server exits
with `server is closing: EOF` before replying. Real clients hold stdin open; test harnesses
must too.

### 3.5 Go version requirement **[OBSERVED]**

```
go: github.com/modelcontextprotocol/go-sdk@v1.7.0 requires go >= 1.25.0; switching to go1.26.8
```

System Go is 1.24.12, so the module graph forces a **toolchain download to go1.26.8**, which
succeeded and built an 8.6MB binary. aidev's `go.mod` will declare `go 1.25.0`. A fresh
developer needs either Go ≥ 1.25 or network access for the toolchain switch — documented in
the README.

---

## 4. Git worktree behaviour

All **[OBSERVED]** on git 2.43.0 against a scratch repository.

### 4.1 Isolation holds

```
git worktree add -b aidev/probe-1 <path>
→ <path>/.git is a FILE: "gitdir: <repo>/.git/worktrees/feature-1"
opencode run --dir <path> "create greet.go"
→ worktree:  ?? greet.go
→ main repo: clean, no greet.go
```

### 4.2 Error cases and their exit codes

| Case | Exit | stderr |
|---|---|---|
| branch name already exists | **255** | `fatal: a branch named 'x' already exists` |
| target path already exists | **255** | (hits the branch check first) |
| branch already used by another worktree | **128** | `fatal: 'x' is already used by worktree at '<path>'` |
| `worktree remove` with modified/untracked files | **128** | `fatal: '<path>' contains modified or untracked files, use --force to delete it` |

**Exit codes are inconsistent (128 vs 255), so the git abstraction classifies errors by
matching stderr text, with the exit code only as a fallback.**

Methodological note: these codes were initially misread as `0` because the command was piped
into `head`, which returns *head's* status. True codes require capturing without a pipe — a
trap worth recording.

### 4.3 Cleanup policy is backed by git's own refusal

`git worktree remove` (no `--force`) **refuses to delete a worktree containing uncommitted or
untracked work** and leaves the directory intact. This maps exactly onto the required policy:

- task **succeeded** → plain `worktree remove` is safe and sufficient;
- task **failed / cancelled** → keep the worktree; the agent's work is preserved for inspection;
- `--force` is reserved for explicit, operator-requested cleanup.

### 4.4 Diff collection requires intent-to-add — a real trap

OpenCode's primary output is **new files**, and `git diff` ignores untracked files entirely:

```
status --porcelain →  M main.go / ?? greet.go / ?? f1.txt ...
git diff --stat    →  main.go | 1 +          ← new files INVISIBLE
git add -N . && git diff --stat
                   →  10 files changed, 14 insertions(+)   ← correct
```

**aidev must run `git add -N .` (intent-to-add) before diffing**, or it would report "no
changes" for exactly the work it was built to capture. This happens inside the worktree and
stages nothing's content, so it is non-destructive.

---

## 5. PostgreSQL and ports

**[OBSERVED]** Both conventional Postgres ports on this machine are already taken:

| Port | Occupant |
|---|---|
| 5432 | host `postgresql` service, `systemctl is-active` → **active** |
| 5433 | container `hnspl-live-postgres` (`postgres:16-alpine`, healthy, another project) |
| 5435 | busy (unrelated) |
| **5434** | **free** |
| 5436 | free |

**Decision:** aidev's `docker-compose.yml` publishes Postgres on host port **5434**
(container-internal 5432), overridable via an env var. Defaulting to 5432 or 5433 would make
`docker compose up` fail on this machine.

`postgres:16-alpine` is already cached locally **[OBSERVED]**, so compose startup needs no pull.
Host `psql` client is 18.6, which talks to a 16 server fine **[ASSUMPTION]** (wire protocol is
stable; only client-side feature messages differ).

Module availability **[OBSERVED]**: `proxy.golang.org` reachable (HTTP 200, 0.24s);
`github.com/jackc/pgx/v5` latest **`v5.11.0`**.

---

## 6. Consequences for the aidev design

Each item is forced by a finding above, not by preference.

1. **Worktree is the only containment** (§2.6) → `--dir <worktree>` is an invariant the code
   enforces and tests assert; the main working tree must be unreachable by construction.
2. **One-shot `opencode run`, no server lifecycle** (§2.8) behind `AgentBackend` so server
   mode remains addable.
3. **NDJSON parser must be tolerant** (§2.4) → ignore unknown event types; never fail a task
   because OpenCode added an event kind.
4. **Never trust exit code alone** (§2.5) → classify with exit code + stderr + `error` events;
   validate agent names in aidev because OpenCode silently falls back (exit 0).
5. **Generous, configurable timeouts** (§2.9) → cold start can exceed 4 minutes.
6. **Process-group kill, self-spawned only** (§2.7) → never signal by process name.
7. **stderr-only logging in MCP mode** (§3.3) → stdout belongs to JSON-RPC.
8. **Schemas generated from Go types** (§3.4) → `jsonschema` tags on the MCP DTOs.
9. **`git add -N .` before diff** (§4.4).
10. **Classify git errors by stderr text** (§4.2); **no `--force` on the success path** (§4.3).
11. **Host port 5434** for Postgres (§5).
12. **Fake backend is the default in tests**; a real-OpenCode E2E is possible here at zero cost
    (§2.10) and will be opt-in via an env guard so CI without OpenCode still passes.

---

## 7. Unresolved questions (deliberately deferred)

1. **[UNRESOLVED]** What the multi-minute `init` actually does (§2.9), and whether it can be
   triggered deliberately as a warm-up step rather than discovered as a timeout.
2. **[UNRESOLVED]** Whether `opencode run` enforces any internal turn/time ceiling of its own.
   No such flag exists; aidev imposes its own timeout regardless.
3. **[UNRESOLVED]** Precise taxonomy of `error.name` values (only `UnknownError` was
   provoked). aidev's classifier keeps an `unknown` bucket rather than guessing.
4. **[UNRESOLVED]** Whether `step_finish.reason` has values beyond `stop` and `tool-calls`
   (e.g. a length/abort reason). Treated as: anything that is not `stop` and is not followed by
   a successful finish is non-conclusive, and verification decides the outcome anyway.
5. **[UNRESOLVED]** Behaviour under concurrent `opencode run` invocations against worktrees of
   the *same* repository, given the shared SQLite store and shared project identity. The MVP is
   single-flight per task and does not depend on this; it must be settled before parallel workers.
6. **[ASSUMPTION]** `.mcp.json` project-scope approval is a one-time interactive step; a
   fully headless first-run may need `--scope user` or `--scope local` instead.

---

## 8. Reproducing this research

Probes ran in a gitignored `.probe/` directory inside the repository (scratch repo, worktrees,
captured stdout/stderr, and a throwaway Go module for the MCP SDK). The directory is disposable;
nothing in the build depends on it. Every command in this document is runnable as written
against OpenCode 1.18.30 / Claude Code 2.1.268.

Probe-created artifacts were cleaned up afterwards: worktrees removed, probe branch deleted,
the research `opencode serve` process terminated, and both MCP config files restored to `{}`.
