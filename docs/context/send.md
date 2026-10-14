# send: Backup and the upload pipeline

Read this for any change to `backup/backup.go`, smart snapshot selection, resume, or upload.

Diagrams 3, 4, 5 in [../architecture.md](../architecture.md).

## Control flow

1. `cmd/send.go` validates flags, loads keys. `ValidateVolumeSize` (`files/jobinfo.go`) refuses `--volsize 0` and, with an `s3://` destination, a `--volsize` over `S3MaxVolumeParts` (9,900) x `--uploadChunkSize`: S3 takes 10,000 parts and the backend cannot make them larger. A bare `-i` name loses the dataset name only when `@` or `#` follows it (`-i tanknight` of `tank` is `tank@tanknight`); `receive -i` does the same.
2. If a smart flag is set, `ProcessSmartOptions` (`backup/backup.go`) fetches snapshots and manifests, then calls `selectSmartSnapshots` → `planSmartSnapshots` (`backup/plan.go`). `plan` uses the same function, so both pick the same action. A noop returns `ErrNoOp` (exit 0).
3. `Backup` (`backup/backup.go`):
   - `--dry-run` → `reportDryRun`: validates the snapshots, then `dryRunRefusals` runs the checks that only read (`refuseExistingSet` with `dryRun`, `checkManifestFits`, `refuseMixedChain`), then logs the decision and a `zfs send -n -P` size estimate (a failed estimate is a Warning). It connects to the destinations, writes nothing there, takes no lock, and does not run `tryResume`; like a smart send, it can add manifests to the local cache. It never calls `PreDownload` (a smart `--dry-run` too: `ProcessSmartOptions(ctx, jobInfo, dryRun)`): `syncCacheMode` with `noRestore` asks a `backends.Archiver` which manifests a GET cannot read (S3 Glacier), starts no restore, and fails (`archivedManifestsError`) when one of them may be a manifest of the dataset and is not cached: without it the plan would see a hole in the chain and plan a wrong full. Cold manifests of other datasets are skipped. `plan` (`BackupsAtTarget`) reads the same way. A set at some destinations is "would complete" (no copy).
   - `prepareBackend` + `getCacheDir` per destination.
   - Take the lock `workdir/locks/md5(dataset).lck` (`bootLock`). Busy means another `send` or `clean` runs on this host. On Linux the holder also writes `<lock>.boot` (`<pid> <boot id>`, from `/proc/sys/kernel/random/boot_id`). A busy lock whose `.boot` file names the lock's pid and another boot is from before a restart: it is removed with a Warning. No boot id (not Linux) or no `.boot` file: busy as before; the error then says to remove the lock if the pid is not zfsbackup.
   - `refuseExistingSet`: manifest already at all destinations → error. At some → needs `--resume` or `CompletePartial`, then `copyManifest` verifies volume sizes + SHA-256 and uploads only the manifest.
   - `checkManifestFits`: `zfs send -n -P` estimate ÷ volsize → manifest bytes (`manifestCost`). Over `files.MaxManifestBytes` → error that names the smallest `--volsize` that fits, before any upload. No estimate → warning, go on (`saveManifest` still refuses at the limit).
   - `--resume` → `tryResume` keeps volumes already at every destination. Else `discardPartialManifests`.
   - `validateSnapShotExists`, then run the pipeline.

## Smart selection (`planSmartSnapshots`)

Pure function. It reads the smart options, `Raw` and `VolumeName` from `jobInfo`. No ZFS or backend I/O. Unit-test it directly (`TestSelectSmartSnapshots`, `backup/plan*_test.go`).

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
| an incremental (explicit or `newer-candidate`) whose `--raw` differs from the newest backup at a destination | error: would mix raw and plaintext streams (`sameStreamKind`) |
| nothing newer | noop `nothing-newer` |

Snapshot suffixes: `--fullSnapshotSuffix _monthly` anchors fulls; `--incrementalSnapshotSuffix _daily` anchors incrementals. Sanoid names put the type at the end. `--snapshotPrefix` filters only the base snapshot in `newestMatchingSnapshot`. It does not scope manifests.

The lagging destination decides. The full window compares with the destination furthest behind.

A chain keeps one stream kind: an incremental must carry the same `--raw` as the backup it continues from; a full starts a new chain and may change it. Explicit `-i` sends check it too (`refuseMixedChain` in `Backup`, after the incremental snapshot is validated; a destination with no backup of the source is skipped with a Notice). `--dry-run` checks it too.

An incremental must continue a backup of the same snapshot: `sourceBackup` matches the source by name, creation time and, when both record one, guid. When a destination holds a backup of a snapshot with the source's name but another creation time or guid (destroyed and taken again), `refuseMixedChain` refuses the send: no restore could apply it. `complete-partial` and `--resume` are covered by `sameStream`. The error text holds `mix raw and plaintext` (`MixedChainError`).

## Pipeline (`sendStream`, `backup/backup.go`)

All stages are goroutines in one `errgroup`. Any failure cancels `ctx`. No final manifest is written after a failure.

- `zfs send` stdout → `sendStream`: SHA-256 of the whole stream (for resume), cut at `volsize − 50 KiB` with `CreateBackupVolume`. Stderr of a `zfs send` that exits 0 is logged at Warning (`-R -s` reports skipped children there); on a non-zero exit it is in the error.
- Forwarder → `retryUploadChainer` per destination. **Chain, not fan-out**: dest 2 gets a volume after dest 1 uploads it. `MaxParallelUploads` workers, exponential backoff.
- `delete://` backend is the last link. It removes the temp file when every real destination has the volume. Only when `--maxFileBuffer > 0`.
- Finisher appends to `jobInfo.Volumes`, saves a partial manifest to the local cache, releases a `fileBuffer` token. Each save encodes the WHOLE manifest, so the cost per volume grows with the volume count.
- The final manifest goes through the same chain last. A set is complete only when its manifest is at the destination.
- `--maxFileBuffer 0` uses pipes. Then only one destination is allowed and a failed upload cannot retry.

## Tests

- Pure planner: `backup/plan_test.go`, golden scenarios in `backup/testdata/scenarios/` (`make scenarios`).
- Pipeline end to end: root `e2e_test.go`, `e2e_failure_test.go`, `e2e_resume*_test.go`, `e2e_delete_backend_test.go` (the `delete://` link frees temp files) with `internal/fakezfs` and a `file://` destination. See [testing.md](testing.md).
- Log assertions: `captureLogs(t)` in `backup/backup_test.go`.
