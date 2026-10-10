# AGENTS.md

Instructions for AI coding agents that work in this repo, and for every subagent they spawn. `CLAUDE.md` is a symlink to this file.

Read this file only. Open a file in `docs/context/` when a task touches that component.

## What this program is

`zfsbackup` sends ZFS snapshots to cloud or file storage and restores them. Go CLI, cobra commands. Module path is `github.com/someone1/zfsbackup-go` (an upstream fork; the working dir is the bosgood fork).

- `send` cuts a `zfs send` stream into volumes, compresses, encrypts, uploads them to one or more destinations, then uploads a manifest.
- `receive` downloads the volumes of one manifest and pipes them into `zfs receive`. `receive --auto` walks the incremental chain.
- `list` prints the manifests at a destination.
- `clean` deletes objects at a destination that no manifest names.
- `plan` simulates what `send` would do. It never writes.

## Packages

| Package | Role | Context file |
|---|---|---|
| `cmd/` | cobra commands, flags, exit codes | [docs/context/cli.md](docs/context/cli.md) |
| `backup/` | orchestration of every command | [send](docs/context/send.md), [receive](docs/context/receive.md), [clean](docs/context/clean.md), [plan](docs/context/plan.md) |
| `files/` | data model: `JobInfo` (manifest), `VolumeInfo` (writer layers) | [docs/context/data-model.md](docs/context/data-model.md) |
| `backends/` | `Backend` interface; s3, gs, azure, b2, ssh, file, delete | [docs/context/backends.md](docs/context/backends.md) |
| `zfs/` | runs the `zfs` binary | [docs/context/data-model.md](docs/context/data-model.md) |
| `pgp/`, `config/`, `log/` | keyrings; globals `WorkingDir`, `AppLogger` | [docs/context/cli.md](docs/context/cli.md) |
| `internal/fakezfs/` | fake `zfs` binary for e2e tests | [docs/context/testing.md](docs/context/testing.md) |

Full diagrams: [docs/architecture.md](docs/architecture.md). Operator runbook: [docs/runbook-first-backup.md](docs/runbook-first-backup.md).

## Rules

1. **Find code with the `codesearch` skill.** Do not start with grep. Concept known: `semble search "<what it does>" -k 5 --max-snippet-lines 0 --format text 2>/dev/null`. Symbol known: `cs --color never -i go -x vendor --only-declarations <Symbol>` or `--only-usages`. Recipes: `.claude/skills/codesearch/SKILL.md`.
2. **Build and test through the Makefile.** See [docs/context/testing.md](docs/context/testing.md). If a target is missing, add one. Do not run ad-hoc `go` or `docker` commands.
3. **Read `.claude/napkin.md` first.** It records corrections and what works. Add to it as you work.
4. **Design notes live in `docs/specs/<date>--<slug>/`.** Put `intent.md` and `plan.md` there before a large change.
5. **Log level.** Default is Notice. User-facing output must use `Noticef`. `Debugf` is hidden.
6. **Flags.** Bind command-specific flags to standalone `cmd` vars (`cleanDryRun`). Bind shared ones to the global `jobInfo`.

## Subagents

Subagents do not inherit skills that the parent loaded. When you spawn a subagent for a task that locates code, add this line to its prompt:

> Use `semble search` for concepts and `cs` for symbols, as described in `.claude/skills/codesearch/SKILL.md`. Read that file first. Do not use grep to locate code.

If you are a subagent reading this: the rules above apply to you.

## Other skills

See `.claude/skills/README.md` for the list of repo skills.
