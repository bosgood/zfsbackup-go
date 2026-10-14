# files/ and zfs/: manifest, volumes, storage layout

Read this for any change to `files/`, `zfs/`, object names, or the local cache.

Diagrams 6 and 10 in [../architecture.md](../architecture.md).

## `files.JobInfo` (`files/jobinfo.go`) = the manifest

One `JobInfo` per backup set. It is serialized as the manifest object. Fields tagged `json:"-"` are runtime flags only. Key fields:

- `VolumeName`, `BaseSnapshot`, `IncrementalSnapshot` (`SnapshotInfo` with name, creation time, GUID).
- `Volumes []*VolumeInfo`: each has size and SHA-256 of the **stored** bytes.
- `Compressor`, `EncryptTo`, `SignFrom`, plus the resolved key fingerprints.
- `Destinations` are canonical URIs. `DestinationsAsTyped` maps them to the spelling the user typed.
- `SourceMountpoints` (`map[string]string`, `omitempty`): source dataset -> the mountpoint the stream gives it (not `none`, `legacy` or `-`), when the send has `--properties` or `--replication`. The top dataset's counts when its source is `local` or `received`; with `--replication` also each filesystem below it whose mountpoint is `local`, `received` or `inherited from` a dataset in the stream (`mountpointInStream`). `send` records them (`recordSourceMountpoints`, `zfs.GetMountpoints`). `receive` refuses to mount a restored dataset over another dataset that is mounted at that path. A manifest of an older version has no such field and restores as before.

A received snapshot keeps the GUID of its source. `receive` compares the manifest's `BaseSnapshot.GUID` with the local snapshot of the same name and creation time before it says "already exists"; a manifest with no GUID (an older version) is not compared.

`files/manifest.go`: `ReadManifest` caps a manifest at `MaxManifestBytes` (1 GiB of JSON, about 2 million volumes; `ErrManifestTooLong`); `backup.saveManifest` refuses to write one over it. `files/atomic.go`: `WriteFileAtomic` for cache writes.

## `files.VolumeInfo` (`files/volumeinfo.go`) = writer layers

`prepareVolume` and `CreateSimpleVolume` build the write stack. `Extract` reverses it on read.

Pipe mode on receive: one `VolumeInfo` has a writer (the download) and a reader (`Extract`). The download calls `WillExtract` before the volume goes to the reader. Then `Extract` does not take `v.lock`, which the writer's `Close` holds while it flushes the last bytes into the pipe. Without it, a volume of 256 KiB or less (`files.BufferSize`) deadlocked.

```
bytes → compressor (gzip internal | external cmd | none)
      → pgp encrypt+sign or sign (if --encryptTo / --signFrom)
      → byte counter → SHA-256, CRC32C, MD5, SHA-1 → bufio
      → temp file (maxFileBuffer > 0) | io.Pipe to backend.Upload (0)
```

The manifest always uses internal gzip. Hashes are of the stored bytes, after compress and pgp.

`SetReadHook(fn)`: every later `Read` that returns bytes calls `fn(n)` (after the rate limiter, so it counts what left the process). The upload progress wrapper (`backends/progress.go`) sets it for one `Upload` call and clears it with `nil`. No lock: one destination reads a volume at a time.

## Object names

```
manifests|tank/data|snap1.manifest.gz.pgp
tank/data|snap1.zstream.gz.pgp.vol1
tank/data|snap1|to|snap2.zstream.gz.pgp.vol1     (incremental)
```

- Separator default `|` (`--separator`). Incremental names are `<vol>|<from>|to|<to>`.
- `.gz` is the compressor extension. `.pgp` appears when you encrypt or sign.
- `--manifestPrefix` default `manifests`.
- For object stores, the path after the bucket is a directory. `s3://b/p` and `s3://b/p/` are the same destination. Versions before 2026-10 concatenated without `/`. `checkLegacyLayout` in `backends/` refuses such a destination.

## Local working directory (`--workingDirectory`, default `~/.zfsbackup`)

| Path | Holds |
|---|---|
| `locks/md5(dataset).lck` | send and clean lock; same host and working dir only |
| `cache/md5(canonical URI)/md5(manifest object name)` | manifest copies; `getCacheDir` migrates an old typed-URI dir |
| `temp/zfsbackupNNN/` | volume temp files |

## `zfs/` (`zfs/zfs.go`)

Runs the `zfs` binary at `zfs.ZFSPath`: `list`, `get creation`, `send`, `receive`, `send -n -P` (`GetZFSSendDryRun` parses the `size\t<bytes>` line). `ParseSnapshotList` reads the text format that `internal/fakezfs` also accepts. No other package calls `zfs` directly.

## Tests

`files/*_test.go` cover round-trips, pgp, atomic writes. `files/export_test.go` exposes internals to tests.
