# Can OpenSandbox sandbox aidev?

Question: whether OpenSandbox (https://github.com/opensandbox-group/OpenSandbox)
can be applied to aidev — to isolate the coding agent, the verification step, or
both. This note answers it from source code only, at OpenSandbox commit
`d8cfce39dc1d846e580510ca44f44c495cbe95c4`, against the aidev tree in this
worktree.

Short answer: not now. The gap OpenSandbox would close is real — the agent runs
as the operator, on the host, with the operator's network and credentials — but
closing it this way trades a small, auditable threat model for a larger one: an
Alpha Python server holding the Docker daemon socket, host-networked sandboxes
by default, images from registries aidev does not otherwise use, and a
verification path whose exit-code semantics the spec does not pin down. The
recommendation in the last section is **defer**, with the conditions that would
change it.

Style follows docs/research.md: `[OBSERVED]` marks what was read in a file,
`[UNRESOLVED]` marks what reading could not establish. Nothing was executed
(see the fifth section).

## What OpenSandbox is

OpenSandbox is a sandbox platform: a control-plane server plus per-sandbox
agents, driven over HTTP by SDKs. The server package describes itself as a
"FastAPI control plane for OpenSandbox that manages sandbox lifecycle on Docker
(ready) and Kubernetes (planned) runtimes" [OBSERVED]
(`opensandbox:server/pyproject.toml:22`). It requires Python 3.10 or newer
[OBSERVED] (`opensandbox:server/pyproject.toml:28`) and carries Docker,
FastAPI and Kubernetes client libraries in its dependency closure [OBSERVED]
(`opensandbox:server/pyproject.toml:44-50`). Its own maturity label is
"Development Status :: 3 - Alpha" [OBSERVED]
(`opensandbox:server/pyproject.toml:31`).

The moving parts, as configured:

| Part | Source of truth | What it is |
|---|---|---|
| Lifecycle server | `opensandbox:server/pyproject.toml:22`, `opensandbox:server/pyproject.toml:44-50` | Python/FastAPI control plane, Alpha |
| Docker runtime | `opensandbox:server/configuration.md:105-112` | Ready; takes an `execd_image`, optional init mode |
| Kubernetes runtime | `opensandbox:server/configuration.md:105-112` | Planned/alternative; BatchSandbox or agent-sandbox providers |
| Secure runtimes | `opensandbox:server/configuration.md:364-378` | Optional gVisor, Kata, Firecracker behind named OCI runtimes / RuntimeClasses |
| Egress sidecar + policy | `opensandbox:server/configuration.md:231-243`, `opensandbox:specs/sandbox-lifecycle.yml:1917-1926` | Per-sandbox outbound allow/deny rules, attached only when a create request carries one |
| Host storage allowlist | `opensandbox:server/configuration.md:291` | Prefix allowlist for host bind mounts; empty means every host mount is rejected |
| Go SDK | `opensandbox:sdks/sandbox/go/go.mod:1` | Lifecycle, execd and egress clients plus a `Sandbox` facade |

Runtime selection is a single required key: `runtime.type` is `docker` or
`kubernetes` [OBSERVED] (`opensandbox:server/configuration.md:105-112`). The
Docker section defaults `network_mode` to `"host"` and documents `bridge` or a
custom network as the alternatives, noting that the egress sidecar plus
`networkPolicy` requires `bridge` [OBSERVED]
(`opensandbox:server/configuration.md:115-130`). Outbound policy without the
sidecar's requirements is rejected for incompatible modes [OBSERVED]
(`opensandbox:server/configuration.md:231-243`). So the default sandbox shares
the host network namespace, and the contained-network configuration is the one
that needs asking for.

Strong isolation exists as an option, not as the default. `[secure_runtime]`
selects `gvisor`, `kata` or `firecracker` (Firecracker is Kubernetes-only),
mapped onto a Docker OCI runtime name or a Kubernetes RuntimeClass, with
validation rules for each combination [OBSERVED]
(`opensandbox:server/configuration.md:364-378`). The Docker runtime additionally
documents dropped capabilities, `no-new-privileges` and a PID limit as ordinary
server configuration [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:22-28`).

There is a Go SDK, at `sdks/sandbox/go` — an earlier draft of this assessment's
test-suite lore records a claim that there was none, and the module file refutes
it: the module path is `github.com/alibaba/OpenSandbox/sdks/sandbox/go`
[OBSERVED] (`opensandbox:sdks/sandbox/go/go.mod:1`). It covers the three APIs:
a `LifecycleClient` with create/get/list/pause/resume/delete and snapshots
[OBSERVED] (`opensandbox:sdks/sandbox/go/lifecycle.go:93`), an `ExecdClient`
with sessions, foreground/background commands, file operations and metrics
[OBSERVED] (`opensandbox:sdks/sandbox/go/execd.go:134`)
(`opensandbox:sdks/sandbox/go/execd.go:319`)
(`opensandbox:sdks/sandbox/go/execd.go:423`), and an `EgressClient` exposing
get/patch/delete of the sidecar policy plus the credential vault [OBSERVED]
(`opensandbox:sdks/sandbox/go/egress.go:42-54`). Above those sits a `Sandbox`
facade with command, session, file and egress helpers
(`opensandbox:sdks/sandbox/go/sandbox.go`)
(`opensandbox:sdks/sandbox/go/sandbox_exec.go:23-28`)
(`opensandbox:sdks/sandbox/go/sandbox_files.go:83-99`), and the shared request
types including the egress `NetworkPolicy`/`NetworkRule` shapes [OBSERVED]
(`opensandbox:sdks/sandbox/go/types.go:132-139`). Sibling SDKs exist for
Python, JavaScript, C# and Kotlin
(`opensandbox:sdks/sandbox/python/pyproject.toml`). The examples in the next
sections use the Python SDK, but the Go SDK is what aidev — a Go program —
would actually import.

The two API contracts are OpenAPI documents. The lifecycle spec defines sandbox
creation with `image` or `snapshotId`, plus `volumes`, `networkPolicy`,
platform and credential-proxy fields [OBSERVED]
(`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`)
(`opensandbox:specs/sandbox-lifecycle.yml:1761-1764`); `networkPolicy` cannot
be combined with pooled allocation [OBSERVED]
(`opensandbox:specs/sandbox-lifecycle.yml:1624-1625`). A volume entry takes a
name, a mount path, and exactly one backend — `host`, `pvc`, `ossfs` and
others [OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`). The
`host` backend maps a host directory into the container and is restricted by a
server-side allowlist [OBSERVED]
(`opensandbox:specs/sandbox-lifecycle.yml:2011-2026`). The execd spec defines
the streaming event envelope for command and code execution, with event types
`init`, `status`, `error`, `stdout`, `stderr`, `result`, `execution_complete`,
`execution_count` and `ping` [OBSERVED]
(`opensandbox:specs/execd-api.yaml:1999-2016`). Notably, that envelope carries
no exit-code property; exit codes appear in the spec only on the status objects
for background commands and code runs [OBSERVED]
(`opensandbox:specs/execd-api.yaml:1977-1986`)
(`opensandbox:specs/execd-api.yaml:2410-2414`). This asymmetry matters for
verification and is unpacked in the third section.

## How aidev isolates work today

aidev's isolation boundary is a git worktree, and only a git worktree. The
worktree path is validated before git is invoked: it must resolve inside
`WORKSPACE_ROOT` and outside the repository, which is what makes "the agent
cannot touch the main working tree" a property of the code rather than a
convention [OBSERVED] (`aidev:internal/git/git.go:160-163`), enforced by
resolving the path and checking isolation before `worktree add` runs
[OBSERVED] (`aidev:internal/git/git.go:175-181`). Phase 0 measured that
OpenCode in non-interactive `run` mode writes files without asking, so the
`--dir <worktree>` flag is a hard invariant, not a convenience [OBSERVED]
(`aidev:docs/research.md:149-152`).

Everything else about the agent's execution is deliberately ordinary. The
OpenCode backend builds `run --dir <worktree> --format json` and executes it
as a subprocess [OBSERVED] (`aidev:internal/agent/opencode.go:71`); the Codex
backend builds `exec --json --skip-git-repo-check -C <worktree>` the same way
[OBSERVED] (`aidev:internal/agent/codex.go:82`). The backend contract requires
the agent to run with the request's working directory and reports a failed
agent through `Result.Status` rather than as a Go error [OBSERVED]
(`aidev:internal/agent/agent.go:30-33`)
(`aidev:internal/agent/agent.go:150-159`). The process runner gives every
subprocess a deadline, a process group with group kill, and bounded output —
but it inherits the operator's environment, because the tools need `HOME` and
`PATH` [OBSERVED] (`aidev:internal/procexec/procexec.go:66-78`), and it puts
each child in its own process group so helpers cannot be left behind
[OBSERVED] (`aidev:internal/procexec/procexec.go:196-206`).

Concretely, the agent runs **as the user, on the host, with the user's network
and credentials**:

| Isolation axis | What holds | What does not |
|---|---|---|
| Filesystem | worktree confinement; the main tree is unreachable by construction (`aidev:internal/git/git.go:160-163`) | nothing beyond the worktree: the agent reads and writes as the UID, including `~/.local/share/opencode/auth.json`-adjacent credentials and any file the UID can reach |
| Processes | own process group, SIGTERM-then-kill, mandatory timeout (`aidev:internal/procexec/procexec.go:196-206`) | runs unsandboxed on the host; no user namespace, no seccomp, no read-only root |
| Network | Codex's own `--sandbox workspace-write` adds a layer under aidev's for that backend only | OpenCode has no network gate; free-tier inference needs the network anyway, so the agent's network access is the operator's |
| Environment | `OTEL_*` stripped; argv (never env) persisted for audit (`aidev:internal/procexec/procexec.go:66-78`) | full inherited environment, API keys included, visible to the agent process |

The second half of the design is the judge. Verification runs the task's own
commands and reports their exit codes; the agent's summary is never treated as
evidence, and the lifecycle allows `SUCCEEDED` only from `VERIFYING`, so this
result is the only thing that can produce a successful task [OBSERVED]
(`aidev:internal/verification/verification.go:1-8`). The state machine agrees:
`StatusSucceeded` is reachable only from `StatusVerifying` [OBSERVED]
(`aidev:internal/task/status.go:27-30`). Each step runs as a local subprocess
in the same worktree the agent worked in [OBSERVED]
(`aidev:internal/verification/verification.go:173-186`), and refuses to run at
all when empty [OBSERVED] (`aidev:internal/verification/verification.go:116-128`).

The interception check is the part most relevant to sandboxing. After the
TASK-000026 incident — the agent wrote a `pytest.py` shim that verification
then imported — aidev lists every path differing from the base commit
(including committed and ignored files) [OBSERVED]
(`aidev:internal/git/changed.go:14-18`) and refuses any step whose runner can
be loaded from a changed path [OBSERVED]
(`aidev:internal/verification/interception.go:21-23`). The worker enforces it
between the agent run and verification: collect changed paths, compute
interceptions, fail as `VERIFICATION` without running anything when the list
is non-empty [OBSERVED] (`aidev:internal/worker/worker.go:719-731`), after
moving the task into `VERIFYING` [OBSERVED]
(`aidev:internal/worker/worker.go:706-714`). The documented residual risks are
code passed via `python -c` / `sh -c`, interpreters other than sh/bash/python,
and a script's own imports being shadowed beside it — the check guards the
judge, not the exam.

What aidev does not protect against, stated plainly: a malicious or
compromised agent step runs with the operator's UID, network and secrets, and
can exfiltrate, mine, or attack from the host; a malicious model response can
only be kept out of the main tree, not kept from running. The worktree is a
blast wall around the repository, not a sandbox around the computer.

## Where it could fit

There are two places a sandbox could interpose — the agent phase and the
verification phase — and two ways a worktree could reach a sandbox: host
volumes or copying files through execd. They interact, and the combinations
are not equal.

A host bind mount is the only route that preserves aidev's git-native flow.
The lifecycle API offers exactly this field: per-volume entries with a `host`
backend, a container mount path, and a server-side allowlist restriction
[OBSERVED] (`opensandbox:specs/sandbox-lifecycle.yml:1792-1799`)
(`opensandbox:specs/sandbox-lifecycle.yml:2011-2026`). But the allowlist
defaults to empty, and when it is empty **every** host mount is rejected as
secure-by-default [OBSERVED] (`opensandbox:server/configuration.md:291`). So
an aidev deployment must explicitly allowlist the worktree root (or each
worktree), which punches every task checkout into a container mount namespace
— the isolation boundary then depends on the allowlist staying exactly as
narrow as intended, forever.

The alternative is copying: upload the worktree into the sandbox with the
execd file API and download the result afterwards. The Go SDK exposes both
directions (`UploadFile(s)`, `DownloadFile`) alongside directory listing and
file metadata [OBSERVED] (`opensandbox:sdks/sandbox/go/sandbox_files.go:83-99`)
(`opensandbox:sdks/sandbox/go/execd.go:319`)
(`opensandbox:sdks/sandbox/go/execd.go:423`). This keeps the host allowlist
empty, but it breaks everything aidev currently gets from git for free:

| Concern | Host mount | Copy through execd |
|---|---|---|
| Diff | `git diff` against the base commit works as today (`aidev:internal/git/git.go:285-293`) | the diff must be reconstructed from downloaded files; intent-to-add handling, binary detection and truncation all need re-implementation against a copy |
| Commit on success | commit to the task branch, then remove the worktree (`aidev:internal/worker/worker.go:862-868`) | the commit must happen either inside the sandbox (which needs git + identity + push credentials there) or by copying back into the worktree and committing locally — an extra sync step with its own failure modes |
| Failed work | worktree retained untouched; git's own refusal is the backstop (`aidev:internal/git/git.go:386-399`) | failed work lives in a sandbox or snapshot, not in a worktree; retention means snapshot lifecycle management, a second bookkeeping system beside `worktrees` |
| Judge integrity | the interception check runs against local git state (`aidev:internal/worker/worker.go:719-731`) | changed-path listing must be performed inside the sandbox or over the downloaded tree, i.e. the check itself becomes a remote operation |

Neither route is a drop-in; the mount route preserves semantics at the price
of allowlist discipline, the copy route preserves the allowlist at the price
of re-implementing the evidence chain.

Verification inside a sandbox has a sharper problem: the rule that only
aidev's own verification decides success. Today that rule is enforced by
construction — a local `wait(2)` status flows into `procexec.Result.ExitCode`
and then into the verification record [OBSERVED]
(`aidev:internal/verification/verification.go:173-186`). Over execd, the exit
code arrives as a JSON field in a stream event, i.e. as **untrusted input over
a network boundary**, produced inside a filesystem the agent just wrote to.
Moving verification into the sandbox therefore converts a local measurement
into a remote attestation, and the attester is the machine under test.

The Go SDK's exit-code handling makes this concrete. `Execution.ExitCode` is a
pointer, nil when unavailable [OBSERVED]
(`opensandbox:sdks/sandbox/go/execution.go:82-83`). An `error` event sets
`Error` and sets `ExitCode` only when the error value string parses as an
integer [OBSERVED] (`opensandbox:sdks/sandbox/go/execution.go:218-226`). An
`execution_complete` event sets `ExitCode` to 0 **only if no exit code was
already recorded and no error was seen** — the comment says "Foreground
command exit code: 0 if no error" [OBSERVED]
(`opensandbox:sdks/sandbox/go/execution.go:231-241`).

What the source does **not** establish is when execd emits an `error` event
for a failing command at all. The `/command` endpoint documents stdout,
stderr, execution status and completion events over SSE [OBSERVED]
(`opensandbox:specs/execd-api.yaml:397-406`), but the `ServerStreamEvent`
schema it points at defines no exit-code property — only the status objects
for background commands and code runs carry one [OBSERVED]
(`opensandbox:specs/execd-api.yaml:1999-2016`)
(`opensandbox:specs/execd-api.yaml:1977-1986`). The SDK nevertheless parses an
optional `exit_code` from stream events [OBSERVED]
(`opensandbox:sdks/sandbox/go/execution.go:128`) — a field the spec never
promises to send. So: a failing `go test` inside a sandbox may surface as an
`error` event (with `ExitCode` set only if the value happens to be numeric),
as `execution_complete` with a separately-delivered exit code the spec does
not document, or as `execution_complete` with no error and a synthesized 0.
[UNRESOLVED] which of these the server actually does for a foreground command
exiting nonzero — nothing was run, and the spec does not say. Adopting
sandboxed verification on top of this would mean the success decider depends
on an undocumented mapping. The safe reading of the code is that exit codes
survive the trip only by convention, not by contract.

The examples confirm the intended shape of adoption and its limits. The
opencode example creates a sandbox from the code-interpreter image, installs
the CLI over the network, and runs a prompt in a fresh `/tmp` directory
inside the sandbox [OBSERVED]
(`opensandbox:examples/opencode/main.py:52-72`); the codex-cli example does
the same, including JSONL parsing and session resume [OBSERVED]
(`opensandbox:examples/codex-cli/main.py:79-94`)
(`opensandbox:examples/codex-cli/main.py:102-118`); the claude-code example
mirrors them with `-p --output-format json` and `--resume [OBSERVED]
(`opensandbox:examples/claude-code/main.py:77-86`)
(`opensandbox:examples/claude-code/main.py:98-111`). All three default to the
same image, `code-interpreter:v1.1.0` from the Aliyun registry [OBSERVED]
(`opensandbox:examples/opencode/main.py:38-41`)
(`opensandbox:examples/codex-cli/main.py:59-62`)
(`opensandbox:examples/claude-code/main.py:56-59`). None of them mounts a host
repository — every command runs against files that already live inside the
sandbox image or were created there. There is no example of the exact
operation aidev needs: check out a host worktree inside a sandbox, run an
agent against it, and get a trustworthy diff and exit code back.

## What it would cost

The price is counted in new trust, new infrastructure, and new failure modes —
against a brief that explicitly excludes Kubernetes and prizes a local-first
MVP.

The server holds the Docker daemon socket. The example compose file mounts
`/var/run/docker.sock` into the server container [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:56-57`), alongside the
server image and its port mapping [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:48-57`). That socket is the
Docker API: whoever can write to it can create containers, including
privileged ones with arbitrary host mounts — effectively root on the host.
aidev today adds no daemon and no privileged surface; adopting OpenSandbox
adds one whose compromise is host compromise. The compose file also sets
`resolve_internal = false` with a comment explaining that the server cannot
route to sandbox bridge IPs from its own network [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:8-12`) — a reminder that
even the reference deployment is already working around its own networking.

The network default points the wrong way for an MVP. The server-wide default
is `network_mode = "host"` [OBSERVED]
(`opensandbox:server/configuration.md:119`); the example deployment overrides
it to `bridge` with a host-IP rewrite and a 20,000-port allocation range
[OBSERVED] (`opensandbox:server/docker-compose.example.yaml:22-28`). Egress
policy — the feature that would contain a network-capable agent — requires
`bridge` and is rejected otherwise [OBSERVED]
(`opensandbox:server/configuration.md:231-243`). So containment-correct
networking is available but is three configuration decisions away from the
default, and the egress image itself is a separate container version to track
[OBSERVED] (`opensandbox:server/docker-compose.example.yaml:22-28`).

Images come from registries aidev does not otherwise touch. The examples pull
the code-interpreter image from `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com`
[OBSERVED] (`opensandbox:examples/opencode/main.py:38-41`), the example server
config pins the execd image to the same Aliyun registry while the egress and
server images default to Docker Hub names [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:17-20`)
(`opensandbox:server/docker-compose.example.yaml:22-28`), and the example
client is `python:3.11-slim` [OBSERVED]
(`opensandbox:server/docker-compose.example.yaml:64-72`). Adopting means the
build's freshness, latency and provenance now depend on an Alibaba-region
registry plus Docker Hub, with version pins (`v1.1.0`, `v1.1.7`, `latest`)
scattered across example files rather than a single lockfile.

Authentication defaults to off. `server.api_key` defaults to null; when it is
empty the server skips API-key checks and instead demands an explicit
insecurity acknowledgement at startup (`OPENSANDBOX_INSECURE_SERVER=YES` or an
interactive confirmation) [OBSERVED]
(`opensandbox:server/configuration.md:69`). A local-first tool whose operator
is also the threat boundary would have to get this right right away — an
unauthenticated lifecycle API on `0.0.0.0:8080` is a container-spawning
service open to the LAN.

And much of the project is machinery the brief excludes. Kubernetes is an
explicit non-goal of aidev, alongside schedulers, dashboards, auth systems,
Kafka and Redis [OBSERVED] (`aidev:docs/architecture.md:330-331`); there is
no PersistentVolumeClaim because there is no Kubernetes, by design [OBSERVED]
(`aidev:docs/architecture.md:405-406`), and aidev is a local-first tool for
one operator [OBSERVED] (`aidev:docs/architecture.md:524`). OpenSandbox's
Kubernetes runtime (BatchSandbox vs agent-sandbox providers, pod templates,
RuntimeClasses, snapshot controllers) would be carried as dead weight, while
its Docker runtime still requires the socket, the bridge networking, and the
registries above. The server's own dependency closure (Postgres and Redis
client libraries) and its `[store]` persistence backend are further services
to operate [OBSERVED] (`opensandbox:server/pyproject.toml:44-50`).
[UNRESOLVED] how much of that persistence is mandatory for a minimal
single-operator deployment — the example compose starts only the server, and
nothing was run to find out what breaks without the rest.

| Cost | Size | Avoidable? |
|---|---|---|
| Daemon with docker.sock (host-root-equivalent) | one new privileged service | no, inherent to the Docker runtime |
| Bridge networking + egress sidecar for containment | 3+ config decisions off-default | no, the default is host networking |
| Aliyun registry + Docker Hub image supply chain | new provenance/latency dependency | partly, by mirroring — which is itself new infrastructure |
| API-key-by-default-off lifecycle API | one footgun on first boot | yes, by setting a key — if the operator does |
| Kubernetes providers, CRDs, RuntimeClasses | entire unused subsystem | yes, by ignoring it — but it stays in the dependency and upgrade path |
| Alpha server (own label) | API drift risk against pinned SDK | no; pin and re-verify on every bump |

## What was run and what was not

Nothing was run. No containers were started, no images were pulled, no
packages were installed, and the network was not used — per the task
constraints, and because nothing in the assessment needs them. Every claim
above comes from reading files: the OpenSandbox clone at
`d8cfce39dc1d846e580510ca44f44c495cbe95c4` (checked with `git cat-file` for
each cited path) and the aidev tree in this worktree.

Files read on the OpenSandbox side: `server/pyproject.toml`,
`server/configuration.md`, `server/docker-compose.example.yaml`,
`specs/sandbox-lifecycle.yml`, `specs/execd-api.yaml`,
`sdks/sandbox/go/go.mod`, `sdks/sandbox/go/execution.go`,
`sdks/sandbox/go/execd.go`, `sdks/sandbox/go/lifecycle.go`,
`sdks/sandbox/go/egress.go`, `sdks/sandbox/go/sandbox.go`,
`sdks/sandbox/go/sandbox_exec.go`, `sdks/sandbox/go/sandbox_files.go`,
`sdks/sandbox/go/types.go`, `sdks/sandbox/python/pyproject.toml`,
`examples/opencode/main.py`, `examples/codex-cli/main.py`,
`examples/claude-code/main.py`. Files read on the aidev side:
`internal/git/git.go`, `internal/git/changed.go`,
`internal/procexec/procexec.go`, `internal/agent/agent.go`,
`internal/agent/opencode.go`, `internal/agent/codex.go`,
`internal/verification/verification.go`,
`internal/verification/interception.go`, `internal/worker/worker.go`,
`internal/task/status.go`, `docs/architecture.md`, `docs/research.md`.

Consequences, stated so a reader can discount correctly:

- Any sentence about what the server *does at runtime* (socket behaviour,
  bridge routing, egress enforcement, snapshot retention) is either a
  paraphrase of configuration documentation or general Docker knowledge, not
  a measurement. It is labelled by citing the document, not a run.
- The exit-code analysis is a code reading of the Go SDK plus the two OpenAPI
  specs. The gap it identifies — the SDK synthesizing 0 where the spec
  promises nothing — is in the text whether or not any server behaves well
  today.
- [UNRESOLVED] whether a foreground command's nonzero exit reliably arrives
  as an `error` event, an `exit_code` field, or not at all.
- [UNRESOLVED] whether `allowed_host_paths` can express "exactly this
  worktree, created five seconds ago" without restarting the server, and what
  a mounted worktree's `.git` linkage does inside a container with a
  different UID.
- [UNRESOLVED] the minimal operable server deployment (persistence, Redis,
  egress image) for one operator on one machine.
- [UNRESOLVED] whether the secure runtimes (gVisor/Kata) change any of the
  above materially; they were read as configuration only.

## Recommendation

**Defer. Do not adopt OpenSandbox for aidev now — not for the agent phase,
not for verification, not for one part.**

The evidence supports this without hedging. The threat OpenSandbox addresses
— agent code running with the operator's UID, network and secrets — is real
and documented above. But the proposed remedy weakens the property aidev is
built around: success is decided by a local measurement (a `wait` status from
a subprocess in a worktree) that no agent output can influence. Sandboxed
verification replaces that measurement with a JSON field from inside the
machine under test, over a protocol whose own spec does not define it. That
is not a stronger judge; it is a weaker one wearing isolation as a costume.
And sandboxed agency, while more defensible, costs a privileged daemon, an
off-default network posture, a new image supply chain, and a rewrite of the
diff/commit/retain flow that git currently provides for free — all to contain
a single-operator tool whose agent already runs at the operator's own
privilege level. The worktree remains the honest boundary: small, audited,
and enforced by code.

What would have to be true first, in order:

1. **A pinned exit-code contract.** The execd spec (or a pinned server
   version plus a test harness aidev owns) must state exactly which event
   carries a foreground command's exit status, including nonzero exits,
   timeouts and session kills. Until then, sandboxed verification cannot
   satisfy the rule that only aidev's own verification decides success.
2. **A deployment story without a new host-root daemon on the operator
   machine.** Either the server runs somewhere the operator does not type
   their credentials (a VM, a dedicated host), or the Docker-runtime trust
   model is explicitly accepted and documented as raising aidev's privilege
   requirements. An Alpha server with docker.sock on a laptop is not a
   sandbox; it is a second, less-audited operator.
3. **Containment-correct defaults, verified.** Bridge networking, egress
   deny-by-default covering the model and package endpoints aidev needs, and
   an API key from first boot — demonstrated, not just configured.
4. **An image provenance decision.** Which registries are trusted, who
   mirrors them, and where versions are pinned in one place.
5. **A prototype of the mount flow that keeps git semantics.** One worktree,
   allowlisted, with diff-against-base-commit, commit-on-success and
   retain-on-failure all demonstrated unchanged — or an explicit admission
   that the copy flow re-implements them.

If items 1–5 were ever satisfied, the shape worth prototyping would still be
agent-phase-only sandboxing with verification kept local: the sandbox would
contain what the agent can touch, while the decider stays a local `wait`
status over files git can see. Verification-in-a-sandbox should stay off the
table even then — it contradicts the architecture, not just the schedule.
Revisit when aidev needs to run untrusted agents (multi-user, multi-tenant,
or hostile-model evaluation), rather than one operator's assistant on one
machine. Until that day, the worktree, the process group, and the local
verifier are the whole sandbox aidev needs.
