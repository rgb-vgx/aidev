# Compatibility in 0.x

aidev is at 0.x. This page says what that means in practice: which parts of it
other programs and people may rely on, what can change between releases, and how
a change is announced. It describes how releases have been made so far, not a
promise beyond it; a 1.0 promise will be defined from the interfaces people
outside this project actually use.

## What counts as an interface

These are the surfaces a script, a planner or a person builds on:

- **The CLI**: command names, flags, positional arguments, and exit codes
  (`aidev task run` exits non-zero when the task does not succeed; `aidev doctor`
  exits 1 when something is broken; `aidev task cancel` exits 1 when the runner
  did not stop).
- **`--json` output** of every command, and the MCP tools' names, inputs and
  structured results. The two are the same shapes.
- **`conf.json` keys** and their meaning.
- **Event types and their payload keys** in the event log.
- **The plugin's skills** (their names and what they do), versioned separately as
  the plugin's own 0.x.

The database schema is not an interface. Read it if you like, but write to it
only through aidev, and expect columns to be added.

## What can change, and how you find out

A release may add, rename or remove any of the above. What has been true of
every release so far:

- **Additions are the normal change.** New commands, flags, JSON fields, event
  types and payload keys appear without notice beyond the changelog; a new JSON
  field is omitted when empty, so a reader that ignores unknown fields keeps
  working.
- **Every release has an upgrade note** at the top of its CHANGELOG section,
  saying whether to run `aidev migrate`, whether the plugin changed, and what to
  restart.
- **A rename or a removal is called out in the changelog** in the section of the
  release that makes it, with what to use instead. None has been needed yet.

## The database

- Migrations are embedded in the binary, forward-only, and applied with
  `aidev migrate`. There are no down migrations: back up before migrating
  (README, "Backing up and restoring").
- Every migration so far is additive: a column with a default, a wider `CHECK`
  constraint. An older binary keeps working on a database migrated by a newer
  one, because it checks only the migrations it knows; this was checked when
  0020 and 0021 were applied while v0.8.0-2 was still installed (`aidev doctor`,
  `task list` and `project list` all worked).
- A migration that is not additive will say so in its release's upgrade note.
- A project registered from now on defaults to `clean` verification (migration
  `0023_clean_verify_default`); a repository pinning submodules still defaults
  to `in_place`, and projects already registered keep the mode they have.

## Versions

- A release is tagged `vMAJOR.MINOR.PATCH`. Every release so far has been a
  minor version, and in 0.x a minor version may contain any change listed above.
- `aidev version` prints the version a binary was built from, and the MCP server
  reports it in its handshake, so a running server can be told apart from the
  installed binary.
- The plugin's version moves when its skills change, independently of aidev's.
