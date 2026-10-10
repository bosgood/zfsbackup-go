# send: Backup and the upload pipeline

Read this for any change to `backup/backup.go`, smart snapshot selection, resume, or upload.

Diagrams 3, 4, 5 in [../architecture.md](../architecture.md).

## Control flow

1. `cmd/send.go` validates flags, loads keys.
2. If a smart flag is set, `ProcessSmartOptions` (`backup/backup.go`) fetches snapshots and manifests, then calls `selectSmartSnapshots` → `planSmartSnapshots` (`backup/plan.go`). `plan` uses the same function, so both pick the same action. A noop returns `ErrNoOp` (exit 0).
3. `Backup` (`backup/backup.go`):
   - `--dry-run` → `reportDryRun` logs the decision and a `zfs send -n -P` size estimate. Stops before the lock.
   - `prepareBackend` + `getCacheDir` per destination.
   - Take the lock `workdir/locks/md5(dataset).lck`. Busy means another `send` or `clean` runs on this host.
   - `refuseExistingSet`: manifest already at all destinations → error. At some → needs `--resume` or `CompletePartial`, then `copyManifest` verifies volume sizes + SHA-256 and uploads only the manifest.
   - `checkManifestFits`: `zfs send -n -P` estimate ÷ volsize → manifest bytes (`manifestCost`). Over `files.MaxManifestBytes` → error that names the smallest `--volsize` that fits, before any upload. No estimate → warning, go on (`saveManifest` still refuses at the limit).
   - `--resume` → `tryResume` keeps volumes already at every destination. Else `discardPartialManifests`.
   - `validateSnapShotExists`, then run the pipeline.

## Smart selection (`planSmartSnapshots`)

Pure function. No ZFS or backend I/O. Unit-test it directly (`TestSelectSmartSnapshots`, `backup/plan*_test.go`).

Outcomes, in order of checks:

| Outcome | Reason label |
|---|---|
| `--full` and newest full exists at all destinations | noop `already-backed-up` |
| `--full` exists at some, no `--resume` | error: destinations out of sync |
| `--full` | full `explicit-full` |
| partial set at some destinations, completable | `complete-partial` (Backup copies manifest only) |
| newest backup newer than pool | `ErrLastBackupInFuture` (exit 2) |
| no full yet | full `no-previous-full` |
| last full older than `--fullIfOlderThan` | full `window-elapsed` |
| incremental source pruned from pool | full `source-pruned` |
| newer `*IncrSuffix` snapshot | incremental `newer-candidate` |
| nothing newer | noop `nothing-newer` |

Snapshot suffixes: `--fullSnapshotSuffix _monthly` anchors fulls; `--incrementalSnapshotSuffix _daily` anchors incrementals. Sanoid names put the type at the end. `--snapshotPrefix` filters only the base snapshot in `newestMatchingSnapshot`. It does not scope manifests.

The lagging destination decides. The full window compares with the destination furthest behind.

## Pipeline (`sendStream`, `backup/backup.go`)

All stages are goroutines in one `errgroup`. Any failure cancels `ctx`. No final manifest is written after a failure.

- `zfs send` stdout → `sendStream`: SHA-256 of the whole stream (for resume), cut at `volsize − 50 KiB` with `CreateBackupVolume`.
- Forwarder → `retryUploadChainer` per destination. **Chain, not fan-out**: dest 2 gets a volume after dest 1 uploads it. `MaxParallelUploads` workers, exponential backoff.
- `delete://` backend is the last link. It removes the temp file when every real destination has the volume. Only when `--maxFileBuffer > 0`.
- Finisher appends to `jobInfo.Volumes`, saves a partial manifest to the local cache, releases a `fileBuffer` token. Each save encodes the WHOLE manifest, so the cost per volume grows with the volume count.
- The final manifest goes through the same chain last. A set is complete only when its manifest is at the destination.
- `--maxFileBuffer 0` uses pipes. Then only one destination is allowed and a failed upload cannot retry.

## Tests

- Pure planner: `backup/plan_test.go`, golden scenarios in `backup/testdata/scenarios/` (`make scenarios`).
- Pipeline end to end: root `e2e_test.go`, `e2e_failure_test.go`, `e2e_resume*_test.go` with `internal/fakezfs` and a `file://` destination. See [testing.md](testing.md).
- Log assertions: `captureLogs(t)` in `backup/backup_test.go`.
