# cmd/: CLI, flags, exit codes

Read this when a task adds a flag, a command, or changes exit behavior.

## Entry points

| Command | File | Calls |
|---|---|---|
| `send` | `cmd/send.go` | `backup.Backup` (`backup/backup.go`) |
| `receive` | `cmd/receive.go` | `backup.Receive`, or `backup.AutoRestore` with `--auto` |
| `list` | `cmd/list.go` | `backup.List` |
| `clean` | `cmd/clean.go` | `backup.Clean` |
| `plan` | `cmd/plan.go` (`runPlan`) | `backup.Scenario.Run` |

`cmd/root.go` holds global flags, keyring loading, the working directory, and the exit-code mapping. `main.go` only calls `cmd.Execute`.

## Exit codes (`cmd/root.go`)

| Code | Meaning |
|---|---|
| `0` | success, or nothing new to send (`ErrNoOp`) |
| `2` | plan checks failed, or the last backup is newer than the pool (`ErrLastBackupInFuture`) |
| `1` | other plan error |
| `255` | any other error |

## Flags

- Shared flags bind to fields of the global `jobInfo` (`files.JobInfo`), for example `Force`, `Compressor`, `EncryptTo`.
- Command-specific flags bind to standalone vars in `cmd/` (`cleanLocal`, `cleanDryRun`, `sendDryRun`). Use this style for new command-specific flags.
- `send` smart flags: `--full`, `--incremental`, `--fullIfOlderThan`, `--fullSnapshotSuffix`, `--incrementalSnapshotSuffix`, `--snapshotPrefix`. `PreRunE` runs `ProcessSmartOptions` before `Backup`. See [send.md](send.md).
- `receive`, `list`, `clean` reject the smart flags.
- `--dry-run` / `-n` on `send` is a 3rd argument to `backup.Backup(ctx, jobInfo, dryRun)`. Same shape as `Clean`.

## Globals

- `config.WorkingDir`: default `~/.zfsbackup`. Holds `locks/`, `cache/`, `temp/`. See [data-model.md](data-model.md).
- `config.BackupTempdir`: temp file dir for volumes.
- `log.AppLogger`: op/go-logging. Default level Notice. User-facing text must use `Noticef`.
- `pgp/`: loads public and secret keyrings from `--publicKeyRingPath` and `--secretKeyRingPath`. Passphrase from `PGP_PASSPHRASE`.
- `zfs.ZFSPath` / `--zfsPath`: the `zfs` binary. Tests point it at the test binary (see [testing.md](testing.md)).

## Tests

`cmd/*_test.go` test flag validation and exit codes. Root-package `e2e_*_test.go` run the real commands end to end.
