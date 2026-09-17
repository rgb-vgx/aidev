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

### 2.9 Slow first runs: a real timeout hazard, with a revised cause **[OBSERVED]**

The very first `opencode run` against a brand-new project **did not complete within 240s**,
producing zero stdout and stopping after logging `message=init`. A second attempt at 90s also
hung at `init`. Afterwards — once `opencode serve` had run once against the same directory —
every invocation completed in **5–16s**, including plain `opencode run` with no server present.

- **[OBSERVED]** the first invocations against an unseen project exceeded 240s and 90s
  producing no output; later invocations against the same project took 5–16s.

This was originally attributed to a one-time per-project initialisation, and that attribution
turned out to be **wrong, or at least unproven**. A later measurement (§7c) found the same
model taking 3.9s, 66.6s and 100.5s for an identical trivial prompt on an already-warm
repository. Free-tier latency alone can therefore account for the slow first runs, and "no
output for 240s" is equally consistent with waiting on a provider, since nothing is printed
until the model replies.

- **[OBSERVED]** a task in a repository created seconds earlier — a new root commit, so a
  project OpenCode had never seen — completed in **12 seconds** end to end using
  `muse-spark-1.3`. Whatever per-project initialisation exists therefore costs seconds, not
  minutes, and cannot explain a 656-second run.
- **[OBSERVED]** `init` is the last log line before the wait, which is suggestive but not
  conclusive about what the wait is for.

That measurement closes the question in practice: the slow first runs were provider latency,
not initialisation. The wrong explanation survived three retellings before being checked,
which is the argument for this document distinguishing observation from attribution at all.

**Implication for aidev, unchanged but for a different reason:** the default agent timeout
must be generous and configurable (`DEFAULT_TASK_TIMEOUT`). The advice that followed from the
old explanation — "warm OpenCode against the repository first" — is **not supported** and has
been removed from the documentation. The useful advice is to choose a model whose latency is
predictable (§7c).

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

**A session belongs to its directory** (measured 2026-09-17, OpenCode 1.18.31, 9router muse,
probe in `.probe/resume-dir/`). A session created with `--dir a` and continued with
`-s <id> --dir a` remembered its context ("4271"). Continued with `-s <id> --dir b` (cwd b as
well), OpenCode logged `creating instance directory=.../a`, ran the turn there — the loop
reached "exiting loop" in 7 s — yet printed no event at all and hung until `timeout` killed it
(90 s and 240 s runs). Adding `--fork` created the fork in directory a too, with the same hang.
So a retry cannot continue a session in a new worktree path: continuing the session means
running in the directory the session was created in.

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

## 7b. Phase 2 probes: git and OpenCode mechanics

Measured later than the rest, when the execution core was designed. Same rules:
these are observations, not assumptions.

### Collecting a diff without mutating the worktree **[OBSERVED]**

`git diff` ignores untracked files (§4.4), so new files need `git add -N .` first.
But a worktree belonging to a *failed* attempt is kept for a human to inspect, and
leaving intent-to-add entries in its index would change what `git status` shows
them. Pointing git at a throwaway copy of the index avoids that:

```
cp .git/worktrees/<name>/index /tmp/copy
GIT_INDEX_FILE=/tmp/copy git -C <worktree> add -N .
GIT_INDEX_FILE=/tmp/copy git -C <worktree> diff --numstat
→ 1  0  main.go
  3  0  newfile.go          ← the new file is visible
git -C <worktree> status --porcelain
→  M main.go
   ?? newfile.go            ← the real index is untouched
```

aidev therefore collects diffs against a temporary index copy.

### The cleanup policy, and why git already enforces half of it **[OBSERVED]**

| Step | Result |
|---|---|
| `git worktree remove` with uncommitted work | **rc 128**, `contains modified or untracked files, use --force`; directory left intact |
| `git -c user.name=aidev -c user.email=aidev@localhost commit` inside the worktree | **rc 0** — works with no host git identity configured |
| `git worktree remove` after committing | **rc 0**, directory gone |
| the task branch afterwards | still holds the commit and the new file |
| `git worktree remove` with *gitignored* leftovers (build artifacts) | **rc 0** — ignored files do not block removal |

This settles a question the design had to answer: on success the agent's work is
*uncommitted* in the worktree, so deleting the worktree would destroy the
deliverable. Committing to the task's own branch first makes the result durable and
reviewable in git, removes nothing from anyone's history, and is not a merge. After
that, plain `worktree remove` succeeds.

So the policy is: **commit to the task branch and remove the worktree on success;
retain the worktree untouched on failure or cancellation** — where git's own
refusal is the backstop, not aidev's diligence. Passing `-c user.name/-c
user.email` avoids depending on the host's git identity.

### `opencode run` argument separator **[OBSERVED]**

`opencode run --dir <d> --format json -m <model> -- "Reply OK only"` exits 0 and
answers normally, so `--` is accepted before the message. aidev uses it, which
removes any chance of a prompt beginning with `-` being parsed as a flag.

### Validating an agent name **[OBSERVED]**

Phase 0 found an unknown `--agent` exits 0 after silently falling back (§2.5), so
aidev validates the name itself. `opencode agent list` prints each agent at column
zero as `name (mode)`, followed by indented JSON permissions:

```
build (primary)
  [ { "permission": "*", "action": "allow", "pattern": "*" }, ... ]
compaction (primary)
explore (subagent)
general (subagent)
plan (primary)
summary (primary)
title (primary)
```

Matching `^(\S+) \((primary|subagent)\)$` extracts the names.

Incidental but confirming: the `build` agent's own permission list begins with
`{"permission": "*", "action": "allow", "pattern": "*"}`. That is OpenCode stating
in its configuration what §2.6 observed in behaviour — the default agent is allowed
everything, so the worktree really is the only boundary.

## 7c. Free model latency: measured, and highly variable **[OBSERVED]**

Measured while investigating why one task took 11 minutes. Same repository (already used by
several runs), same trivial prompt, no tools, `opencode run --dir <repo> --format json -m <model>
-- "Reply with exactly OK."`, wall clock around the whole process:

| Model | Run 1 | Run 2 | Run 3 |
|---|---|---|---|
| `opencode/muse-spark-1.3-contributor-free` | 3.2s | 3.2s | 3.9s |
| `opencode/nemotron-3.5-lightning-free` | 66.6s | 100.5s | 3.9s |

`muse-spark-1.3` is consistent. `nemotron-3.5-lightning` varies by a factor of 25 for identical
work, which is free-tier queueing rather than anything aidev or the prompt controls.

### What this explains

Two real tasks, recorded in aidev's own database:

| Task | Model | Agent time | aidev's own time | Steps | Input tokens |
|---|---|---|---|---|---|
| `TASK-000001` Add a Greet function | nemotron-3.5 | **656s** | **0.2s** | 6 | 48,266 |
| `TASK-000005` Add a Farewell function | muse-1.3 | **9s** | **0.2s** | 5 | 9,565 |

Both produced correct code that passed `go test ./...` and `go vet ./...`.

- **aidev's own contribution is 0.2s.** Worktree creation, diff collection, verification and
  every database write together are negligible. Any wait a user experiences is the agent.
- 656s over 6 steps is ~109s per step, which is consistent with the per-call latency measured
  above. The model accounts for the whole duration without needing a cold-start explanation.
- The slow run's closing summary was incoherent text mixing several scripts, while the code it
  wrote was correct. The fast run's summary was accurate English. Summary quality and work
  quality are independent, which is the clearest available argument for aidev verifying rather
  than reading the agent's report.

### Consequence

`opencode/muse-spark-1.3-contributor-free` is what the documentation now suggests for a free
setup: fast, consistent, and correct on these tasks. `OPENCODE_MODEL` remains configurable and
empty still means "let OpenCode choose".

A third task, in a repository created moments before, completed in 12s with the same model —
which is the measurement that ruled out a per-project initialisation cost (§2.9).

**[UNRESOLVED]** How either model behaves on tasks substantially larger than "write one small
function". A handful of data points on trivial tasks do not generalise, and no claim is made
that they do.

## 7d. Codex CLI: a second agent backend **[OBSERVED]**

Measured against `codex-cli 0.154.0` at `~/.local/bin/codex`, the same way OpenCode was
measured: by running it, not by reading about it.

### The invocation

```
codex exec --profile <name> --json --skip-git-repo-check \
  --sandbox workspace-write -C <worktree> -o <file> -- <prompt>
```

| Flag | Purpose | OpenCode's equivalent |
|---|---|---|
| `exec` | non-interactive subcommand | `run` |
| `-C, --cd <DIR>` | working directory — the isolation hook | `--dir` |
| `--json` | JSONL events on stdout | `--format json` |
| `-o, --output-last-message <FILE>` | final message written to a file | *(none: parsed from the stream)* |
| `-p, --profile <name>` | config profile from `$CODEX_HOME/<name>.config.toml` | *(none)* |
| `-m, --model` | model | `-m` |
| `-s, --sandbox` | `read-only`, `workspace-write`, `danger-full-access` | *(none)* |
| `--skip-git-repo-check` | allow running outside a git repository | *(none)* |
| `--` | separator before the prompt | `--` |

Both `--` before the prompt and passing the prompt on stdin were confirmed to work.

### Four behaviours that differ from OpenCode

**It blocks on stdin.** The first probe timed out after four minutes with no output and
`Reading additional input from stdin...` on stderr, even though the prompt was passed as
an argument. Closing stdin fixes it, which aidev's process runner already does
(`cmd.Stdin = nil`), but a human reproducing this by hand must add `< /dev/null`.

**`--sandbox` and `--approve-for-me` are mutually exclusive.** Passing both exits 2 with a
usage error before anything runs. `--sandbox workspace-write` alone allows the agent to
write in its working directory without prompting, which is what aidev needs.

**It has a sandbox of its own.** OpenCode has none: Phase 0 found it writes files with no
permission gate, which is why the worktree is the only boundary (§2.6). Codex adds a second
layer under aidev's. That does not change aidev's design — the worktree is still the
boundary it relies on — but it is strictly more containment, not less.

**An `error` item is not a failure.** A successful run emitted:

```json
{"type":"item.completed","item":{"id":"item_0","type":"error",
 "message":"Model metadata for `...` not found. Defaulting to fallback metadata..."}}
```

and still exited 0 with the work done. Treating an `error` item as fatal would fail every
run on this configuration. This is the same shape of trap as OpenCode's silent agent
fallback (§2.5), arriving from the opposite direction: there, a success code hid a failure;
here, a failure-looking event accompanies a success.

### Event stream

JSONL, one event per line:

| `type` | Payload | Use |
|---|---|---|
| `thread.started` | `thread_id` | the session id, for a future resume |
| `turn.started` | *(none)* | |
| `item.completed` | `item.type` = `agent_message` (`text`), `command_execution` (`command`), `error` (`message`) | the last `agent_message` is the summary; `command_execution` counts as a tool call |
| `turn.completed` | `usage`: `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens` | usage and cost accounting |
| `turn.failed` | *(not observed)* | declared by the CLI; treated as a failure when seen |

### Timings and noise

A trivial prompt took 8.6s; creating one file took 15.3s. Both exited 0.

stderr carries a large, harmless model-catalogue error on every run with this profile
(`failed to refresh available models: ... missing field 'models'`), because the router at
`web9router` returns a list shape Codex does not expect. It has no effect on the outcome,
so stderr text must not be used to classify a Codex run.

**[UNRESOLVED]** Whether `codex exec resume` can continue a session by `thread_id` the way
OpenCode's `-s` does. The id is recorded now so that retry-with-context stays possible.

## 7e. When the agent writes the judge **[OBSERVED]**

### What happened

TASK-000026 ran Codex (`AGENT_BACKEND=codex`, profile `web9router`, sandbox
`workspace-write`) on a scratch Python repository, with `python3 -m pytest -q` as
verification. pytest was not installed. Over 56 shell commands the agent tried
`pip3 install pytest` (refused by PEP 668), `pip3 install --break-system-packages`
(no network: `Name or service not known`), `sudo apt-get install -y python3-pytest`
(refused: `The "no new privileges" flag is set`), and `apt-get download` (no DNS). It
then wrote a 217-line `pytest.py` into the repository root, committed as part of the work.

aidev's verification passed in 31 ms. `python3 -c "import pytest; print(pytest.__file__)"`
in a checkout of the branch prints the agent's file: **aidev's check ran code written by
the agent it was checking.**

Two things were true and neither is a defence. The agent disclosed the file in its summary
("Added `pytest.py` fallback runner"). And the shim is not a fake: changing the test's
expected value makes it report `1 failed` and exit 1. aidev cannot know either in general;
the product rule is that an agent's account is not evidence.

### Approval prompts

`codex exec` has no `--ask-for-approval` flag (it exists only on interactive `codex`), and
the 69 `item.completed` events of this run contain no approval event. Non-interactive runs
never stop to ask. What stopped the `sudo` and network attempts was the sandbox, not a
prompt — so the sandbox setting, not the approval policy, is the control that matters for
aidev.

### How a runner is resolved from the worktree (Python 3.12.3, Go 1.26)

| Probe | Result |
|---|---|
| `exec.Command("mytool")` with `Dir` holding an executable `./mytool` | `executable file not found in $PATH` — bare names are never taken from the working directory |
| `python3 -m unittest` with `./unittest.py` | shadowed |
| `python3 -munittest` | shadowed |
| `python3 -Bm unittest`, `-Bmunittest`, `-sm unittest` | shadowed: short flags are parsed as a cluster, and `m` may end one |
| `python3 -IBm unittest`, `-BIm unittest` | not shadowed |
| `python3 -P -m unittest`, `-I`, `-IP` | not shadowed |
| `python3 -m shadowmod` with only `./shadowmod.pyc` | shadowed (sourceless import) |
| `python3 -m notinstalled` with `./notinstalled/__main__.py`, no `__init__.py` | shadowed |
| same, for an installed module (`unittest/__main__.py`) | not shadowed: a regular package later on `sys.path` wins |
| `sitecustomize.py` in the working directory, `python3 -c pass` | not loaded |
| `python3 sub/s.py` | `sys.path[0]` is `sub/`, not the working directory |
| `importlib.machinery.all_suffixes()` | `.py .pyc .cpython-312-x86_64-linux-gnu.so .abi3.so .so` — every one starts with `<module>.` |

### Git cannot be asked the obvious way

After `git add` + `git commit` inside a worktree, a diff against a copy of the index with
`add -N` is **empty**; the same diff against the base commit reports the file. So both
`git status` and the recorded diff (`worker_runs.diff`, `changed_files`) lose work the
agent committed. And files matching `.gitignore` never appear in either.

### Consequence

Before running verification, aidev lists every path that differs from the base commit —
including committed and ignored ones — and refuses to run a step whose runner can be
loaded from one of them: a command given as a relative path, the script an interpreter
(`sh`, `bash`, `python`) is handed, or the top-level module of `python -m`. The attempt
fails as `VERIFICATION` without running anything, and the worktree is kept.

Review of TASK-000027 found the first implementation checked an interpreter's arguments but
not the interpreter: `./.venv/bin/python -m pytest` was never matched against `.venv/`, though
a fresh worktree has no virtualenv, so any `.venv/` there was created during the attempt and
its python is the agent's. The same holds for `node_modules/.bin/`. A relative command is now
checked whatever it names.

One trade-off is accepted deliberately. `python -m mypkg`, where `mypkg` is the project's own
code, is refused when the agent changed `mypkg`: aidev cannot tell a project module from a
shadowing one by name. The refusal is loud and names the path, so the planner can choose a
runner the agent does not write (`python -m pytest`). A silent pass would not be visible at all.

This guards the judge, not the exam. Tests, a `Makefile`, `conftest.py`, and project code
are the work under review; changing them is what tasks are for, and whether the tests still
test anything is the reviewer's question. Not covered: code passed with `-c` (`python -c`,
`sh -c`), whose imports aidev cannot see; interpreters other than `sh`/`bash`/`python`; and
modules beside a script shadowing its imports.

## 7f. Auto mode through 9router with muse-spark **[OBSERVED]**

A Claude Code session running through 9router 0.5.75 (`ANTHROPIC_BASE_URL=http://localhost:20128`,
every model slot `oc/muse-spark-1.3-contributor-free(xhigh)`) denied 49 tool calls in auto mode
with "temporarily unavailable, so auto mode cannot determine the safety". The calls never reached
aidev: its MCP log shows the connection and no tool call. Read, Edit and Write were unaffected;
Bash that would be allowed in acceptEdits mode skipped the classifier and ran.

The router's own history is no evidence either way: `usageHistory` recorded 681 requests and all
681 as `ok`, so failures are simply not written.

Reproduced with `claude -p --permission-mode auto --debug-file` through a logging proxy:

| Request | Shape | Result |
|---|---|---|
| main loop | `stream: true`, `max_tokens` 32000 | Anthropic SSE, works |
| auto-mode classifier (`stage=xml_s1`) | no `stream`, `max_tokens` 2112 | HTTP 200 with an OpenAI `chat.completion`, empty content, no `usage` |

Claude Code then logs `Auto mode classifier (XML) error: undefined is not an object (evaluating
'xt.usage.input_tokens')` and `Auto mode classifier unavailable, denying with retry guidance (fail
closed)`. It first tries the Sonnet slot as classifier ("Got error trying Sonnet 5 as auto mode
classifier") before falling back to the main model.

Isolated directly against the router with the same request body:

| Model | `stream: false` | `stream: true` |
|---|---|---|
| `oc/muse-spark-1.3-contributor-free(xhigh)` | OpenAI JSON, empty | Anthropic SSE with text and `usage` |
| `oc/muse-spark-1.3-contributor-free` | OpenAI JSON, empty | — |
| `oc/mimo-v2.5-free` | Anthropic JSON with text and `usage` | Anthropic SSE |

The fault is muse-spark's non-streaming path, not 9router as a whole. 0.5.75 is the latest release.
Upstream also rejects `max_output_tokens` below 16, which is not what the classifier hits (2112).

Two fixes, both verified with the reproduction: `ANTHROPIC_DEFAULT_SONNET_MODEL=oc/mimo-v2.5-free`
(classifier stage 1 `outcome=ok` in 8.3 s), or `tools/anthropic-shim`, which sends non-streaming
requests upstream as streaming and assembles the SSE into an Anthropic message (stages 1 and 2
`outcome=ok`, about 15 s, muse-spark kept as classifier). The user chose the shim to keep
muse-spark.

The 15 s came from the classifier's first stage getting nothing to judge. Sampled directly
against the router, 1 of 8 streaming muse-spark replies to the same prompt was a complete
stream (`message_delta`, `message_stop`) holding only a thinking block, with `usage` 0/0;
through the shim, 1 of 3. Real replies always carried usage (input 2014). The shim retries
that exact shape once for converted requests and returns a second empty reply unchanged, so
a bad stretch cannot become a loop.

## 7g. Can a second agent review the first one's work? **[OBSERVED]**

The idea under test: pair every implementing OpenCode session with a second one whose role is to
test, debug and review it. Before building that, the reviewer half was measured on work aidev had
already delivered, where a human review had recorded what it found.

### Method

Three merged tasks, each reviewed blind in its own clone holding only that task's branch, with
`origin` removed, so the reviewer could not read main, the review commit that fixed the defects, or
any other branch:

| Case | Task | What it is |
|---|---|---|
| A | TASK-000027 | verification interception; 4 defects found by the human review, fixed in 88b3f53 |
| B | TASK-000033 | the conf.json loader; 5 defects found, fixed in d686399 |
| C | TASK-000032 | `aidev mcp` with a deferred database; **no** defect found — the control |

The ground truth and the rubric were written before any run (`.probe/review-experiment/ground_truth.md`),
including one **pre-registered false positive** for the control: a claim that tool calls fail after the
connect timeout, which the earlier probes had shown does not happen. The prompt gave the reviewer the
original task text and the diff (`HEAD~1..HEAD`), told it to find what the tests do not catch, and told
it not to change any file. Each report was then graded by reading or running the code, never by
trusting the report. Model: the free muse-spark-1.3 through OpenCode's `plan` agent, which denies
`edit:*` but allows bash — so the reviewer could compile and run probes, but not modify the clone.

### Two operational faults, both worth more than the grades

**A rejected permission ends the session and looks like success.** Rounds 0-2 told the reviewer to put
temporary files under `/tmp/opencode/<case>/`. `opencode agent list` shows `external_directory` as `ask`
with `/tmp/opencode/*` allowed, and that pattern matches **one level only**: creating the directory is
allowed, writing a file inside it is not. In headless `opencode run` the auto-rejection **ends the
session with exit 0**, no error event, and an empty final message. Three runs were lost that way, one of
them while probing the exact defect it was supposed to find. From round 2b the runs add `--auto`
(auto-approve what is not explicitly denied), which with the `plan` agent still cannot edit files.

**Concurrent runs kill each other.** Running three reviews in parallel produced
`Error: Unexpected error  database is locked`, exit 1 after one second, zero steps: OpenCode's own
store. Two runs were lost this way. Reviews must be sequential, which is why the command now lives in
`.probe/review-experiment/run-review.sh` instead of shell history.

Both faults matter to aidev directly. For an implementing agent, aidev's own verification still judges
the result, so a session cut short is caught. A reviewer produces text, and nothing checks text: a
cut-short review returns an empty report that reads exactly like "no defects found". A reviewer stage
must therefore detect it — last step reason, a rejected tool call, an empty report, a non-zero exit —
and never report "clean" from a session it cannot prove ran to the end.

### Recall of the known defects

Nine runs were valid: three per case, after five were repeated. Grades are in
`.probe/review-experiment/grades.md`, each finding recorded with the check made on it.

| Case | Known defects | Found, per run | Union of three runs | Distinct valid extras | False positives |
|---|---|---|---|---|---|
| A TASK-000027 | 4 | 0, 1, 1 | A1 only | 6 | 0 |
| B TASK-000033 | 5 | 2, 2, 2 | B1 and B2 | 4 | 0 |
| C TASK-000032 | 0 (control) | — | — | 5 | 0 |

Eight of 27 opportunities, so **30% per-run recall** of defects a human review had found, and the
union of three runs on the same commit is no better than its best single run for case B but twice
its worst for case A: the same prompt against the same diff found A1 in two runs out of three. One
run's silence is therefore not evidence of a clean diff.

What it never found is as consistent as what it did. It missed every defect that only shows when
the code is run with an odd input — a non-object conf.json (`[]`, `42`), an empty file, a step
number printed as 0 — although it ran probes freely for other claims. It missed the design-level
one (the schema stated twice, in `configSchema` and `fileConfig`, free to drift). And on case A it
missed the flag-cluster defect `python3 -Bm pytest` while finding a *different* cluster defect in
the shell parser, which says it was reading for the shape of the bug, not enumerating the inputs.
What it found, it found by reading for a mechanism: something loaded from a path that can change.

### What it found that the human review had missed

Fifteen distinct findings outside the ground truth were checked and held up, and **thirteen of them are
still present on main** — the other two the later human review had happened to fix. Each is recorded with
its check in `.probe/review-experiment/grades.md`. In severity order:

- **`ChangedPaths` ignores truncated git output** (case A). Interception decides what aidev must not run
  from that list; the list is capped at 4 MiB and never checked for truncation, so a worktree with tens
  of thousands of untracked paths produces a short list and interception fails **open** — measured on
  main with a 256-byte cap: 60 changed files came back as 7.
- **The help still configures aidev through the environment** (case B). `--agent` says
  "default: OPENCODE_AGENT", `--timeout` says "default: DEFAULT_TASK_TIMEOUT", the overview points at
  `.env.example`, and a malformed DSN prints "parse DATABASE_URL"; aidev reads none of those.
- **`make jaeger-env` / `make langfuse-env` print `export OTEL_*`** (case B, found by both valid runs).
  Nothing reads OTEL_* for configuration any more, so the documented way to turn on tracing silently
  does nothing. **Resolved:** both targets now print a `{"tracing": {...}}` object to paste into the
  conf.json that AIDEV_CONFIG names, and the docs point there instead of `eval "$(make ...)"`.
- **A malformed `database.url` is accepted** (case C, the control). The server starts and every tool
  call fails with the "make db-up" remedy, which is false advice for a typo.
- **Tracing headers are no longer trimmed** (case B). Every other tracing string is trimmed; the env
  loader this replaced trimmed both sides of every header. `" x-api-key "` is not a valid header name.
- **`python3.13t` is not recognised as python** (case A). The free-threaded build loads `-m` modules from
  the working directory exactly like the GIL build (measured with a symlink named `python3.12t`), so
  interception skips it: fail-open, the same class as the relative-interpreter defect.
- **Input is validated after the connection attempt**, and **concurrent first calls serialise** behind
  one mutex, 5 s each (case C, both reproduced against an unreachable address); a waiter cannot observe
  its own cancelled context, and then opens with an expired one.
- **`sh -s check.sh` is reported as an interception** although with `-s` the shell reads its commands
  from stdin and the operand is an argument: honest work blocked over a file the step never runs. The
  same over-matching appears in `python -m T`, which intercepts any top-level entry beginning `T.`,
  measured not to shadow the module (`unittest.extra.py` does not shadow `unittest`).
- **The MCP close reads the opened app once after Serve returns**, so an attempt that succeeds later is
  never closed: the pool stays open and traces are never flushed (case C).
- **A wrong-typed header value** is reported as "want an object of strings, got an object", naming no
  header.

Two of those are fail-open defects in the mechanism that exists to stop an agent from judging its own
work: a truncated changed-path list, and an interpreter name that skips the python rules. Both were
specified test-first and delegated back to aidev (`spec/reviewer-findings`,
`spec/reviewer-findings-2`); `make *-env` belongs to the conf.json infrastructure phase, and the
`python -m T.*` over-match is not specified yet because a fix has to keep matching the
extension-suffixed `.so` names it exists for. It is now fixed: only T/, T.py, T.pyc
and extension-suffixed names count.

### The control held

Across nine valid runs, with a false positive pre-registered as a trap, **not one run claimed it**,
and not one finding was a false positive. Seven findings were noise (a panic no production path can
reach, claimed three times; an aliased map no caller mutates, twice; a stale comment; an unreachable
absolute path), which is a low price for a human to skim. No run read a file outside its clone, ran
git over other refs, or left a clone modified — the `plan` agent's `edit:*=deny` is what guarantees
the last of those, not the prompt.

One sub-claim was wrong without the finding being wrong: round 1 said the malformed connection string
reaches the client, password and all. The finding (a malformed URL is accepted and then blamed on a
database that is not up) reproduced; the leak did not. A reviewer's severity claims need checking as
much as its conclusions.

### What this says about the two-agent idea

A reviewer cannot be a gate. 30% recall against a human review, and variance between
runs on the same commit, means "the reviewer found nothing" carries almost no information — which is
also why aidev's rule stands: only aidev's own verification commands move a task to SUCCEEDED, and a
reviewer's opinion moves nothing.

A reviewer is worth having anyway, as an advisory stage. Fifteen findings outside the ground truth
held up under checking, and **thirteen are still present on main** — including two fail-open defects
in the very mechanism that exists to stop an agent from judging its own work (a truncated changed-path
list, and an interpreter name that skips the python rules). A human review at the merge had missed
all of them. With no false positives in nine runs, the cost of reading its output is small.

So the shape the measurements support:

- The reviewer runs after verification, never instead of it, and produces findings for a human. It
  cannot fail a task, and it cannot approve one.
- Two or three runs, findings unioned, because recall varies per run on identical input.
- The stage must prove its own session ran to the end (exit status, last step reason, a non-empty
  report) and report "cut short" rather than "clean" when it cannot. A permission rejection ends a
  headless session with exit 0.
- The second session cannot share the first one's store: every OpenCode process uses one SQLite
  database (`~/.local/share/opencode/opencode.db`, 345 MB with a 33 MB WAL here), and concurrent
  `opencode run` processes killed each other with `database is locked` twice in this experiment. A
  paired monitor session needs its own data directory, or it has to be sequential.
- Findings are specifications, not patches: each one worth acting on became a failing test first
  (`spec/reviewer-findings`, `spec/reviewer-findings-2`), which is also how a false positive gets
  caught before any code changes.

## 8. Reproducing this research

Probes ran in a gitignored `.probe/` directory inside the repository (scratch repo, worktrees,
captured stdout/stderr, and a throwaway Go module for the MCP SDK). The directory is disposable;
nothing in the build depends on it. Every command in this document is runnable as written
against OpenCode 1.18.30 / Claude Code 2.1.268.

Probe-created artifacts were cleaned up afterwards: worktrees removed, probe branch deleted,
the research `opencode serve` process terminated, and both MCP config files restored to `{}`.
