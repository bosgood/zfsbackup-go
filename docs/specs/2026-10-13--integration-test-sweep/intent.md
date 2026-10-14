# Integration test sweep before the release

Date: 2026-10-13. Branch `clean-dry-run`. Source: an audit of every flag of
`send`, `receive`, `list`, `clean` and `plan` against the twenty-seven
`TestIntegrationS3*` tests (`integration/README.md`), done at 97c3aa1.

## What the twenty-seven cover

Smart send (full, incremental, noop, window rollover, ties, prefixes,
bookmarks, broken chains), explicit `-i`, `-R`, `-p`, `-w`, `--resume`,
two destinations, `--manifestPrefix`, pipe mode, 1 GiB multipart; every
receive flag (`--auto`, `-i`, `-F` with and without `--allowDestroy`, `-d`,
`-e`, `-o`, `-u`, pipe mode); `list` filters; eleven `clean` cases; GPG in
every mode, three key fixtures, rotation, gpg interop; ZFS native encryption;
SIGTERM in a multipart upload and in a clean; the mount guards and the GUID
checks of the 2026-10-12 review.

## Holes

Flags and behaviors that no real-zfs test runs. Each is e2e-covered with the
fake zfs at most; the fake does not run the real `zfs send` flag.

| Hole | Why it matters |
|---|---|
| `send -n` against S3 | the dry-run connects to the destination and must upload nothing; only the fake saw it |
| `--increment` (no suffixes) | the simplest cron line; only smart-suffix sends run on real zfs |
| `-I` (intermediary) | the stream carries every snapshot between; receive must list them all, with their guids |
| `-s` with `-R` | a child without the snapshot; the `-R` receive guards see a child the stream does not carry |
| `--compressor zfs` (`zfs send -c`) | a compressed stream is another stream format; the restore must take it |
| external compressors (`gzip`, `pigz`, `xz`, `zstd`) and `--compressionLevel` | the external path forks a binary on both sides; only `gzip` ran, with the fake |
| `-D` | zfs 2.x deprecates it; what the program does with the warning is unknown |
| `plan` against a real pool and S3 | the planner's input on the real path (`zfs list`, `BackupsAtTarget`) never ran outside the fake |
| a corrupted plaintext volume | the SHA-256 check before `zfs receive` (file mode) and after it (pipe mode); what the target looks like after |
| a zvol | `mountpoint` is `-`; the mount guards of the 2026-10-12 review never saw a volume |
| a dataset name with a space and a colon | both are legal zfs names; object keys, mountpoints and the lock name carry them |
| `--allowKeyExpiry` | new on 2026-10-13 (128c218); the reason for it (a restore after the expiry fails) was proved by hand, not by a test |

Not in this sweep, listed so the next one sees them:

- The other backends: `azure://` (Azurite), `gs://`, `b2://`, `ssh://`. Only
  S3 runs for real. `TestIntegration` (Azurite + `tank/data@c`) is the old
  path and no target runs it. An Azurite service in `compose.yaml` is the
  cheapest next step.
- A SIGTERM during `zfs receive`. The sigterm spec says zfs discards the
  partial receive; nothing checks it. A reliable test needs a stall inside
  the receive, which a wrapper zfs gives for the wrapper, not for zfs.
- `--maxRetryTime` and `--maxBackoffTime` against a flaky S3 (needs a proxy
  that fails requests on purpose).
- The open items of `../2026-10-12--pgp-integration-tests/remaining.md`:
  the break-the-code proofs of the three review fixes.

## Goal

One new test per hole, in new files, each a `TestIntegrationS3*` function so
the default `RUN` takes it; then the whole suite, lint, vet and fmt on the
VM. No production change is planned; a hole that finds a defect gets its fix
in a separate commit.
