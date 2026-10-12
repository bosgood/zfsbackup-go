# receive: Receive and AutoRestore

Read this for any change to `backup/restore.go`, downloads, or chain walking.

Diagrams 7 and 8 in [../architecture.md](../architecture.md).

## `Receive` (`backup/restore.go`)

1. `readCachedManifest`: read the manifest from the local cache, sync from the destination if missing. A manifest that fails keeps its cached copy only when the bytes are fine for a run with other means (`keepsCachedCopy`): over `MaxManifestBytes`, or a `KeyError` with `NeedsSecretKey`. Those errors give the cache path. "Not signed", "not encrypted" and an unknown signer are what a forgery gives too: no copy is kept, and the error says to delete the object if you did not write it.
2. List the target dataset (the one `-d`/`-e` compute, not `LocalVolume`). Compare with the MANIFEST's snapshots, which carry creation times:
   - The base snapshot is there (name and creation time): Notice "already exists, nothing to do", return nil.
   - A snapshot with the base's name but another creation time is there: error that names both times (`localSnapshotMismatch`), with or without `--force`. `-F` would destroy the local snapshots. The log says to `zfs rename` it or receive into another dataset (never destroy: it holds other data).
   - The incremental source must be there, also in `--auto`: the step before received it, and the listing shows it.
   - A listing error means the dataset is not there yet: an empty list.
3. `CheckDecompressor`: run only a known decompressor, or the one given by `--compressor` (`JobInfo.TrustedCompressor`). A manifest from an untrusted writer must not pick the binary.
4. `backend.PreDownload`: S3 Glacier restore and similar.
5. `downloadChannel` has one entry per volume. `N = maxFileBuffer` workers download with backoff. Each checks size + SHA-256 against the manifest. **A mismatch is permanent.** The retry loop does not retry it.
6. Orderer emits volumes in order to `receiveStream`.
7. `VolumeInfo.Extract` reverses the writer layers: pgp verify or decrypt, then decompress. Output goes to `zfs receive` stdin.

An explicit receive names its set with the flags: `-i <source>` for an incremental, none for a full. When the backend has no manifest of that name (`fs.ErrNotExist`; the S3 backend wraps both GetObject's `NoSuchKey` and HeadObject's `NotFound`), `explainMissingManifest` syncs the cache, reads this dataset's manifests, and names each backup of the snapshot with the flags that select it, or says there is none.

`--origin <dataset@snapshot>`: `originSnapshot` lists it (name and creation time), and the incremental source check accepts it next to the target's snapshots. `zfs receive -o origin=` then clones it and applies the stream, so the target need not exist. `AutoRestore` does the same (`TestE2EReceiveIncrementalWithOrigin`, `TestIntegrationS3ReceiveSafety/Origin`).

Mount guard (`refuseMountOverSource`, before any download): a stream sent with `--properties` or `--replication` carries the source's `mountpoint`. When the source dataset is on this host with a mountpoint of its own (`zfs get -o value,source mountpoint`: `local` or `received`, not `none`, `legacy` or a zvol's `-`), the target is another dataset, and `--unmounted` is not set, the receive is refused: it would mount the restore over the live data. Only `--unmounted` lifts it; `-F` is a rollback and does not. After an `--unmounted` receive of such a stream, `noteReceivedMountpoint` (with `pctx`: the errgroup canceled `ctx`) says where the dataset mounts at the next `zfs mount -a` or import, since `-u` skips one mount and does not change the property (`TestE2EReceivePropertiesMountGuard`, `TestIntegrationS3ReceiveSafety/Mounted`).

Receive flags on `JobInfo`: `Force`, `FullPath`, `LastPath`, `NotMounted`, `Origin`, `LocalVolume`, `AutoRestore`.

## `AutoRestore` (`receive --auto`)

1. `syncCache`, read manifests of this dataset only.
2. `linkManifests` (`backup/list.go`) sets `ParentSnap` on each incremental. A full is the preferred parent.
3. Target = the given snapshot, or the newest backup.
4. `backupThatApplies` prefers an incremental whose chain reaches a local snapshot, else the full.
5. Walk `ParentSnap` until the target is local or a full is reached. A loop or a missing parent is an error. So is a link whose `Raw` differs from its parent's (`mix raw and plaintext`); nothing is received before the walk ends.
6. Call `Receive` for each item, oldest first. No item (the target is local): Notice "already exists, nothing to do".

## Tests

- `backup/restore_test.go`, `backup/receive3_test.go`.
- `receiveStream` without ZFS: pass `exec.Command("cat")` as the receive command.
- End to end: root `e2e_receive*_test.go` (`e2e_receive_exists_test.go`: a target that already holds the snapshot, or another of its name), `e2e_thaw3_test.go` (PreDownload). `FAKEZFS_RECEIVE_LOG` records bytes + SHA-256 that `zfs receive` got.
