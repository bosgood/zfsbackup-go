# receive: Receive and AutoRestore

Read this for any change to `backup/restore.go`, downloads, or chain walking.

Diagrams 7 and 8 in [../architecture.md](../architecture.md).

## `Receive` (`backup/restore.go`)

1. `readCachedManifest`: read the manifest from the local cache, sync from the destination if missing.
2. `CheckDecompressor`: run only a known decompressor, or the one given by `--compressor` (`JobInfo.TrustedCompressor`). A manifest from an untrusted writer must not pick the binary.
3. `backend.PreDownload`: S3 Glacier restore and similar.
4. `downloadChannel` has one entry per volume. `N = maxFileBuffer` workers download with backoff. Each checks size + SHA-256 against the manifest. **A mismatch is permanent.** The retry loop does not retry it.
5. Orderer emits volumes in order to `receiveStream`.
6. `VolumeInfo.Extract` reverses the writer layers: pgp verify or decrypt, then decompress. Output goes to `zfs receive` stdin.

Receive flags on `JobInfo`: `Force`, `FullPath`, `LastPath`, `NotMounted`, `Origin`, `LocalVolume`, `AutoRestore`.

## `AutoRestore` (`receive --auto`)

1. `syncCache`, read manifests of this dataset only.
2. `linkManifests` (`backup/list.go`) sets `ParentSnap` on each incremental. A full is the preferred parent.
3. Target = the given snapshot, or the newest backup.
4. `backupThatApplies` prefers an incremental whose chain reaches a local snapshot, else the full.
5. Walk `ParentSnap` until the target is local or a full is reached. A loop or a missing parent is an error.
6. Call `Receive` for each item, oldest first.

## Tests

- `backup/restore_test.go`, `backup/receive3_test.go`.
- `receiveStream` without ZFS: pass `exec.Command("cat")` as the receive command.
- End to end: root `e2e_receive*_test.go`, `e2e_thaw3_test.go` (PreDownload). `FAKEZFS_RECEIVE_LOG` records bytes + SHA-256 that `zfs receive` got.
