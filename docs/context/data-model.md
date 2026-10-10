# files/ and zfs/: manifest, volumes, storage layout

Read this for any change to `files/`, `zfs/`, object names, or the local cache.

Diagrams 6 and 10 in [../architecture.md](../architecture.md).

## `files.JobInfo` (`files/jobinfo.go`) = the manifest

One `JobInfo` per backup set. It is serialized as the manifest object. Fields tagged `json:"-"` are runtime flags only. Key fields:

- `VolumeName`, `BaseSnapshot`, `IncrementalSnapshot` (`SnapshotInfo` with name, creation time, GUID).
- `Volumes []*VolumeInfo`: each has size and SHA-256 of the **stored** bytes.
- `Compressor`, `EncryptTo`, `SignFrom`, plus the resolved key fingerprints.
- `Destinations` are canonical URIs. `DestinationsAsTyped` maps them to the spelling the user typed.

`files/manifest.go`: `ReadManifest` caps a manifest at `MaxManifestBytes` (1 GiB of JSON, about 2 million volumes; `ErrManifestTooLong`); `backup.saveManifest` refuses to write one over it. `files/atomic.go`: `WriteFileAtomic` for cache writes.

## `files.VolumeInfo` (`files/volumeinfo.go`) = writer layers

`prepareVolume` and `CreateSimpleVolume` build the write stack. `Extract` reverses it on read.

```
bytes → compressor (gzip internal | external cmd | none)
      → pgp encrypt+sign or sign (if --encryptTo / --signFrom)
      → byte counter → SHA-256, CRC32C, MD5, SHA-1 → bufio
      → temp file (maxFileBuffer > 0) | io.Pipe to backend.Upload (0)
```

The manifest always uses internal gzip. Hashes are of the stored bytes, after compress and pgp.

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
