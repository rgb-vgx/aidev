# anthropic-shim

A small local proxy that lets Claude Code's **auto mode** work when Claude Code talks to
[9router](https://www.npmjs.com/package/9router) with `oc/muse-spark-1.3-contributor-free`.

## Why it exists

Measured on 2026-09-13 with 9router 0.5.75 (docs/research.md 7f):

| Request to `/v1/messages` | muse-spark-1.3 | mimo-v2.5 |
|---|---|---|
| `stream: true` | Anthropic SSE, text and `usage` | Anthropic SSE |
| `stream: false` | **OpenAI `chat.completion`, empty content, no `usage`** | Anthropic JSON with `usage` |

Claude Code's auto-mode safety classifier sends non-streaming requests and reads
`usage.input_tokens`. With muse-spark it throws, and auto mode fails closed: every Bash
command or MCP call that needs classification is denied with "temporarily unavailable".

The shim sends a non-streaming `/v1/messages` request upstream as streaming and assembles
the SSE into one Anthropic message. Every other request passes through untouched and
streamed. It binds to 127.0.0.1, forwards the client's own credentials, and logs only
request shape and sizes — never content, never credentials.

## Install

```bash
make shim-install      # runs the self-test, installs, enables and (re)starts the user service
systemctl --user status anthropic-shim
journalctl --user -u anthropic-shim -f
```

The service listens on `127.0.0.1:20198` and forwards to `http://localhost:20128`. To use a
different router, edit `ExecStart` in `anthropic-shim.service` (https upstreams work too)
and run `make shim-install` again.

## Use

Point Claude Code at the shim instead of the router; the models stay as they are:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:20198
export ANTHROPIC_AUTH_TOKEN=...            # the same 9router key as before
export ANTHROPIC_MODEL='oc/muse-spark-1.3-contributor-free(xhigh)'
export ANTHROPIC_DEFAULT_HAIKU_MODEL="$ANTHROPIC_MODEL"
export ANTHROPIC_DEFAULT_SONNET_MODEL="$ANTHROPIC_MODEL"
export ANTHROPIC_DEFAULT_OPUS_MODEL="$ANTHROPIC_MODEL"
claude --permission-mode auto
```

A session started before the change keeps its old environment; start a new one.

## Limits

- About 1 in 8 muse-spark replies is a complete stream with only a thinking block and zero
  usage — measured directly against the router, so it is not the shim. For a converted
  request the shim retries that exact shape once; a second empty reply is returned as it
  is, and Claude Code then runs its second classifier stage, which costs a few seconds.
  Streaming requests, including the main conversation, are never retried.
- Only non-streaming `/v1/messages` requests are converted. If 9router fixes its
  non-streaming path, the shim becomes a pass-through and can be removed.
- The alternative without a shim — `ANTHROPIC_DEFAULT_SONNET_MODEL=oc/mimo-v2.5-free`,
  which Claude Code tries first as the classifier — also works, but makes mimo the model
  that decides what auto mode may run.

## Test and uninstall

```bash
make shim-test                                   # also part of make check
systemctl --user disable --now anthropic-shim
rm ~/.config/systemd/user/anthropic-shim.service ~/.local/share/anthropic-shim/anthropic_shim.py
```
