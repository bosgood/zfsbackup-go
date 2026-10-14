# receive: Receive and AutoRestore

Read this for any change to `backup/restore.go`, downloads, or chain walking.

Diagrams 7 and 8 in [../architecture.md](../architecture.md).

## `Receive` (`backup/restore.go`)

1. `readCachedManifest`: read the manifest from the local cache, sync from the destination if missing. A manifest that fails keeps its cached copy only when the bytes are fine for a run with other means (`keepsCachedCopy`): over `MaxManifestBytes`, or a `KeyError` with `NeedsSecretKey`. Those errors give the cache path. "Not signed", "not encrypted" and an unknown signer are what a forgery gives too: no copy is kept, and the error says to delete the object if you did not write it.
2. List the target dataset (the one `-d`/`-e` compute, not `LocalVolume`). Compare with the MANIFEST's snapshots, which carry creation times:
   - The base snapshot is there (name and creation time): Notice "already exists, nothing to do", return nil. When the manifest records the snapshot's GUID, `refuseOtherGUID` first compares it with `zfs get guid` of the local snapshot: another GUID is another snapshot (two hosts that took a snapshot of one name in one second), and the receive is refused, with or without `--force`. An older manifest has no GUID and is not compared.
   - A snapshot with the base's name but another creation time is there: error that names both times (`localSnapshotMismatch`), with or without `--force`. `-F` would destroy the local snapshots. The log says to `zfs rename` it or receive into another dataset (never destroy: it holds other data).
   - The incremental source must be there, also in `--auto`: the step before received it, and the listing shows it.
   - A listing error means the dataset is not there yet: an empty list.
   - `refuseDestroy`: with `--force` (`-F`), refuse when zfs would destroy data, unless `--allowDestroy` (`JobInfo.AllowDestroy`) is set too: a full stream into a target that exists with no snapshots (bookmarks do not count: zfs replaces it), local snapshots newer than the source of an incremental (a snapshot of another name in the same second counts too: it may be newer), and a `--replication` incremental into a target with other snapshots than the source, or with datasets below it (`zfs.Descendants`): zfs destroys each one the sending side did not have, and the manifest does not say which. The rollback of changes since the source snapshot stays allowed: that is what `-F` is for. An explicit receive checks its one backup; `AutoRestore` checks the whole chain before its first receive (`receive(..., checkDestroy=false)` for the steps).
3. `CheckDecompressor`: run only a known decompressor, or the one given by `--compressor` (`JobInfo.TrustedCompressor`). A manifest from an untrusted writer must not pick the binary.
4. `backend.PreDownload`: S3 Glacier restore and similar.
5. `downloadChannel` has one entry per volume. `N = maxFileBuffer` workers download with backoff. Each checks size + SHA-256 against the manifest. **A mismatch is permanent.** The retry loop does not retry it. Nor a failure of the local temp file (no temp dir, a full disk at the copy or at `Close`): `readErrReader` tells a failed read (retried) from a failed write (permanent). In pipe mode the download calls `vol.WillExtract()` before it hands the volume on (see [data-model.md](data-model.md)). The workers download through `backends.WithDownloadProgress`; its total is the manifest's volume sizes. Everything else keeps the raw `backend`.
6. Orderer emits volumes in order to `receiveStream`. Pipe mode: when the reader fails (`Extract`, or the copy into a `zfs receive` that exited), it calls `vol.Abort(err)`; and `processSequence` aborts its volume when the errgroup ctx is canceled (`context.AfterFunc`, stopped when it returns, so a restore that works never aborts). Else the writer waits on the pipe forever and `wg.Wait()` hangs.
7. `VolumeInfo.Extract` reverses the writer layers: pgp verify or decrypt, then decompress. Output goes to `zfs receive` stdin.

An explicit receive names its set with the flags: `-i <source>` for an incremental, none for a full. When the backend has no manifest of that name (`fs.ErrNotExist`; the S3 backend wraps both GetObject's `NoSuchKey` and HeadObject's `NotFound`), `explainMissingManifest` syncs the cache, reads this dataset's manifests, and names each backup of the snapshot with the flags that select it, or says there is none.

`--origin <dataset@snapshot>`: `originSnapshot` lists it (name and creation time), and the incremental source check accepts it next to the target's snapshots. `zfs receive -o origin=` then clones it and applies the stream, so the target need not exist. `AutoRestore` does the same (`TestE2EReceiveIncrementalWithOrigin`, `TestIntegrationS3ReceiveSafety/Origin`).

Mount guard (`refuseMountOverSource`, before any download): a stream sent with `--properties` or `--replication` carries the source's `mountpoint`. When the source dataset is on this host with a mountpoint of its own (`zfs.GetMountpoints`: `local` or `received`, not `none`, `legacy` or a zvol's `-`), the target is another dataset, and `--unmounted` is not set, the receive is refused: it would mount the restore over the live data. With `--replication` it checks each filesystem below the source too (`zfs get -r -t filesystem`): the stream carries their mountpoints. Then `refuseMountOverRecordedPath`: when the manifest records `SourceMountpoints` (source dataset -> path, see [data-model.md](data-model.md)) and `zfs.MountedFilesystems` shows a dataset other than the restored one (`restoredName`) mounted at one of those paths on this host (a restore on another host), the receive is refused too, also into a dataset of the source's name. Only `--unmounted` lifts it; `-F` is a rollback and does not. After an `--unmounted` receive of such a stream, `noteReceivedMountpoint` (with `pctx`: the errgroup canceled `ctx`) says where the dataset mounts at the next `zfs mount -a` or import, since `-u` skips one mount and does not change the property (`TestE2EReceivePropertiesMountGuard`, `TestIntegrationS3ReceiveSafety/Mounted`).

Receive flags on `JobInfo`: `Force`, `AllowDestroy`, `FullPath`, `LastPath`, `NotMounted`, `Origin`, `LocalVolume`, `AutoRestore`.

## `AutoRestore` (`receive --auto`)

1. `syncCache`, read manifests of this dataset only.
2. `linkManifests` (`backup/list.go`) sets `ParentSnap` on each incremental. A full is the preferred parent. It keys a parent by volume, name and creation time in Unix seconds: links written by senders in other time zones still match.
3. Target = the given snapshot, or the newest backup.
4. `backupThatApplies` prefers an incremental whose chain reaches a local snapshot, else the full. With `--origin`, the origin snapshot counts as a source here and in the walk, but never as a snapshot of the target.
5. Walk `ParentSnap` until the target is local or a full is reached. A local snapshot that matches by name and time is compared by GUID as in `Receive`. A loop or a missing parent is an error. So is a link whose `Raw` differs from its parent's (`mix raw and plaintext`); nothing is received before the walk ends.
6. `refuseDestroy` for the whole chain, with the target as it is before the first step. Then call `receive` for each item, oldest first, without the per-step `-F` check: a `--replication` chain into a new dataset must not stop at step 2 because step 1 made children. Only the first item gets `--origin` (`-o origin=`): zfs refuses a clone origin for a dataset that exists. No item (the target is local): Notice "already exists, nothing to do".

## Tests

- `backup/restore_test.go`, `backup/receive3_test.go`, `backup/restore_review_test.go`.
- `backup/restore_review2_test.go` (pipe writer aborts, `refuseDestroy` cases, `mountpointInStream`), `e2e_review2_test.go` (`--auto -F` of a `-R` chain, children's recorded mountpoints, `send --dry-run` starts no S3 restore).
- `e2e_review_restore_test.go`: pipe mode with a small volume, `--auto` across time zones, `--auto --origin`, the GUID check, the mount guards of `-R` children and of `SourceMountpoints`, `--force` and `--allowDestroy`, and a full disk on receive (`make test-enospc`).
- `receiveStream` without ZFS: pass `exec.Command("cat")` as the receive command.
- End to end: root `e2e_receive*_test.go` (`e2e_receive_exists_test.go`: a target that already holds the snapshot, or another of its name), `e2e_thaw3_test.go` (PreDownload). `FAKEZFS_RECEIVE_LOG` records bytes + SHA-256 that `zfs receive` got.
