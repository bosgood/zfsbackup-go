# Changelog

## Unreleased

### clean

- `clean` refuses a destination that has objects but no manifests under
  `--manifestPrefix`. Cached manifests do not count now. Before, with a wrong
  prefix, every cached manifest was "local-only", and `--cleanLocal` could
  delete all of their volumes. The error now shows the prefix it looked for
  and tells you to check `--manifestPrefix` and `--separator`.
- `clean` never deletes a cached manifest of another `--manifestPrefix`. It
  keeps the volumes of that manifest too, and logs how many it found.
- `clean --force` deletes only broken sets at the destination. It does not
  delete a local-only cached manifest (the state of an interrupted send). Only
  `--cleanLocal` deletes that.

### send

- `--resume` keeps a volume only when the cache of every destination records
  it with the same number, name, size and SHA-256. Before, a send to some of
  the destinations could rewrite volumes with other bytes of the same size,
  and the resume did not see it.
- `--dry-run` redacts credentials in the destination URIs it logs.
- Smart sends read only the manifests of their own dataset. An object at the
  destination under the name of another dataset cannot stop them.
- When `send` copies a manifest between destinations, it restores the manifest
  from an archive storage class first.
- After `send` discards the cached manifests of an earlier attempt, it syncs
  the directory, so a crash cannot bring them back.

### receive and restore

- `receive --auto` reads only the manifests of the dataset it restores.
- `receive --auto` stops with an error when the manifests at the destination
  form a loop. Before, it did not stop.
- `receive --auto` applies an incremental backup when its source snapshot is
  on the local pool, also when the backup of that source is gone from the
  destination.
- A volume that is larger than its manifest records fails quickly. `receive`
  downloads at most one byte more than the recorded size.
- `--maxFileBuffer` must be 0 or more.
- The help for `--maxFileBuffer` now says correctly when the size, SHA-256 and
  signature of a volume are checked.

### Manifests and the cache

- A manifest object larger than 64 MiB is not downloaded in full and is not
  cached. Only a reader that needs that object fails.
- A manifest that lists more than `MaxManifestVolumes` volumes, or that has a
  `null` volume, is rejected before it is decoded.
- Reading a manifest uses less memory: about 2 times its size, not 5 times.
- A manifest that cannot be read is removed from the cache. The error names the
  object and tells you to delete it from the destination if it is not yours.
- `WriteFileAtomic` syncs the directory after the rename. A file system that
  cannot sync a directory is not an error.
- `syncCache` removes temporary files that an interrupted write left in the
  cache, when they are older than 24 hours.

### Signatures

- An error for a volume signed by an unknown key no longer tells you to pass
  `--trustSigner` with that key. It tells you that anyone who can write the
  destination can sign an object. Compare the full fingerprint with your own
  records first.

### S3

- `PreDownload` restores again a restored copy that expires in less than 24
  hours. S3 then extends its expiry. If the extension fails, it logs a
  warning and continues.

### plan

- `plan` adopts the creation time from a manifest only when that time is 0 to
  10 minutes after the time in the snapshot name, and all destinations record
  the same time. Otherwise it keeps the name time and logs a warning.
- Daily runs keep the wall-clock time of the first run across DST changes. A
  time in the spring-forward gap is read as after the jump, as cron does.
- In the hour when the clocks fall back, sanoid names repeat. The schedule
  makes only the first snapshot of such a name.
- `skip=` dates that end on a DST change now end at local midnight.
- `coverage` starts at the first scheduled run, also when that run is in a
  `skip=` range.
- A `from=` earlier than the last backup at a destination no longer causes a
  false "last backup newer than the pool" error. The error message now also
  names a `zfs rollback` or a stale `--snapshots` capture as possible causes.

### Development

- `make test-race` uses a 45-minute timeout.
- `make fmt-check` fails when `gofmt` fails, for example on a file that does
  not parse.
- `.devcontainer/firewall-check.sh` checks that a killed firewall re-run
  closes the network: exit code 1, the "interrupted" message, and
  loopback-only rules for IPv4 and IPv6.
- New tests for each fix above.

### Docs

- `docs/runbook-first-backup.md` explains the new `plan` adoption rules, the
  fall-back hour, the "last backup newer than the pool" error, the `clean`
  rules for cached manifests, and how to check a key before you pass
  `--trustSigner`.
