---
name: doctor
description: Find out why aidev is not working and fix it. Use when an aidev tool call fails (the database is not reachable, AIDEV_CONFIG is not set, the agent is missing, migrations are pending), when the aidev MCP server does not connect, before a user's first delegation, or when the user asks to set up or check aidev.
---

# Diagnose and repair aidev

## 1. Run the checks

```
aidev doctor --json
```

It prints one result per check, in order: `config`, `git`, `agent`, `database`,
`migrations`, `workspace`. Each has a `status` (`ok`, `warn`, `fail`, `skipped`),
a `summary` of what was found and, for a problem, a `fix`. The command exits
non-zero when any check failed. A `skipped` check depends on an earlier one; fix
that first and run again.

If the `aidev` command itself is not found, aidev is not installed: say so, and
for a developer point to `make install` in the aidev repository.

## 2. Fix what you safely can, then ask about the rest

Work through the failures in order and run `aidev doctor` again after each fix.

- **database**: when the database does not answer, offer to run `aidev setup`
  (safe to run again; it starts PostgreSQL in Docker and migrates). If Docker is
  missing, do not install it yourself: tell the user what to install. If
  `database.url` points somewhere other than the local container, ask before
  changing it.
- **migrations**: run `aidev migrate`. It only adds tables and columns aidev needs.
- **workspace**: if the directory cannot be created, show the user the path and
  the error; do not change permissions on their machine without asking.
- **config**: never invent a configuration. With no configuration, ask first and
  then offer to run `aidev setup` (it writes conf.json and never overwrites an
  existing one). Its two choices are:
  - `--postgres-image <image>` when their company pulls images from a private registry.
  - `--database-url <url>` when they already have a PostgreSQL database (then no Docker is needed).
  After setup, AIDEV_CONFIG must be set: offer to add the printed
  `export AIDEV_CONFIG=...` line to their shell profile and the "AIDEV_CONFIG"
  entry to the "env" object of ~/.claude/settings.json, asking before changing
  either file.
- **agent** and **git**: tell the user what to install; installing software is
  their decision.

## 3. Report

- **Developer**: the checks that failed, what you ran, and the final
  `aidev doctor` output.
- **Not a developer**: one sentence per problem in their words ("the database
  aidev keeps its notes in was not running, so I started it"), and a clear
  statement of anything they still need to do themselves, step by step.

After changing AIDEV_CONFIG or installing aidev, the aidev tools in Claude Code
only pick it up once the MCP server restarts: ask the user to run `/mcp` and
reconnect aidev, or to start a new session.
