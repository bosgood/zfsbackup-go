# Plan

Four parallel agents, each in a `git archive HEAD` copy under `/tmp/zb-fix-<area>`, each
owns a file set and returns a patch (`git diff`). New tests go in new files.

| Agent | Findings | Owns |
|---|---|---|
| restore | 2, 5, 6, 7, 8, 9, 10, 19 | `files/volumeinfo.go`, `backup/list.go`, `backup/restore.go`, `zfs/`, `internal/fakezfs/`; one hunk each in `files/jobinfo.go` (mountpoint field), `backup/backup.go` (record it), `cmd/receive.go` (`--allowDestroy`) |
| plan | 3, 4, 14 (selection), 20 | `backup/plan.go`, `backup/sync.go`, `backup/plan_checks.go`, scenario goldens |
| send | 1, 11, 14 (Warning), 15, 16, 17, 18 | `backup/backup.go`, `files/jobinfo.go` (`ValidateSendFlags`), `cmd/send.go`, `cmd/receive.go` (`-i` trim) |
| edges | 12, 13 | `backends/` |

Then: apply the patches with `git apply --3way`, `make test-docker`, `make lint`,
`make fmt-check`, one independent reviewer, docs/context updates, napkin.

VM checks still to run (`make integration-vm`): 7, 8, 9, 10, 14 (commands in `findings.md`).

## Status (2026-10-13)

EXECUTED, committed 2026-10-13. The four patches applied with no conflicts. An independent review
found W1-W4, N1, N5 (see "Second review" in `findings.md`); all fixed. `make test-one`
(all packages), `test-enospc`, `vet`, `lint`, `fmt-check` pass.

Decision changes: 3 uses a new reason `broken-chain` (acts like `source-pruned`); 16 uses
9,900 parts, not 10,000 (a volume can pass `--volsize` a little); 10's `-R` rule refuses only
when the target has children or snapshots other than the source; `--auto` checks `-F` for
the whole chain before step 1.
