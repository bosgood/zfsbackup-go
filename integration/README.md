# ZFS + S3 integration test

`TestIntegrationS3` (`integration_s3_test.go`, build tag `integration`) runs
the real `zfs` against the real `s3://` backend:

1. It makes a dataset with random files and sanoid-style snapshots
   (`autosnap_<date>_<time>_monthly`, `_daily`, `_hourly`).
2. Smart `send` with `--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
   --incrementalSnapshotSuffix _daily`: a full of the monthly, then an
   incremental to the newest daily (not the hourly after it), then a noop.
3. `receive` of the full, `receive --auto` onto it, and `receive --auto` into
   an empty dataset. It compares the files each time.
4. Subtest `Pipe`: `send` and `receive` with `--maxFileBuffer 0` (S3 multipart
   upload from a pipe, and a download into a pipe).

The other `TestIntegrationS3*` tests run in the same environment:

| `RUN=` | Covers |
|---|---|
| `TestIntegrationS3` | the steps above (`integration_s3_test.go`) |
| `TestIntegrationS3Chain` | full, then 2 incrementals; restore of the middle link (fails without `--auto`), then of the newest; the whole chain into an empty dataset |
| `TestIntegrationS3EncryptedChain` | the same chain, encrypted and signed; volume and manifest bytes are ciphertext; `list` and `receive` with the right key, another key, no key |
| `TestIntegrationS3Bookmark` | an incremental from a bookmark, found by the smart options and named with `-i dataset#name`; restore of the chain |
| `TestIntegrationS3ListClean` | `list` filters over two datasets (a strict `--before`); `clean` dry-run, an orphan volume, an unrecognized object, `--force` on a broken set |
| `TestIntegrationS3Resume` | a `zfs send` cut mid-stream (a bash wrapper as `--zfsPath`); `clean` spares the uploaded volumes; `--resume` after the cut, after a lost volume, after a lost manifest; subtest `Gzip` with the default compressor |
| `TestIntegrationS3Rollover` | a `--fullIfOlderThan` window elapses for real; new and old chains restore; a receiver on the old chain is refused by zfs; `clean` retires the old chain |
| `TestIntegrationS3ProdShape` | production flags, monthly/daily/hourly tied on one second, a 5-part dataset name, `--snapshotPrefix`, `receive --fullPath` and `--lastPath` |
| `TestIntegrationS3EncryptedResume` | `--resume` with GPG rings; re-sent volumes get new ciphertext; a tampered volume fails the restore; an encrypted incremental from a bookmark |
| `TestIntegrationS3ReceiveSafety` | receive without `-F` onto a changed dataset; `--properties` with `--unmounted`; subtests `Mounted` (a restore that would mount over the live source is refused) and `Origin` (`--origin` of an incremental). Both were red findings, fixed 2026-10-12 (ad5b6ed, 613b22d) |
| `TestIntegrationS3TwoDestinations` | one send to two S3 prefixes; `complete-partial` without re-upload; out-of-sync destinations refused; each restores alone |
| `TestIntegrationS3ManifestPrefix` | two `--manifestPrefix` jobs in one bucket: `list` and `clean` keep to their prefix |
| `TestIntegrationS3NativeEncryption` | ZFS native encryption: a `--raw` chain with the key unloaded, a raw restore that opens only with the sender's key, a raw incremental from a bookmark, a forgotten `--raw` refused, a plaintext send, raw + GPG |
| `TestIntegrationS3CleanLocal` | `clean` on a cut send's cached partial: kept without `--cleanLocal` and with `--force`, kept for a busy dataset, deleted with its volumes by `--cleanLocal`; a partial that lost a volume; subtests `Repair` (`clean --force` at one of two destinations, then a send) and `Denied` (a user without `s3:DeleteObject`) |
| `TestIntegrationS3CleanEncrypted` | `clean` at an encrypted destination with no rings, `--force` and no rings, another key's rings: refused, nothing deleted, the advice is to pass the key (finding F1, fixed) |
| `TestIntegrationS3CleanLayout` | `clean` of nested (`run/`, `run/sub/`) and sibling (`other/`) prefixes, the bucket root, two destinations; subtests `Separator` (`--separator '#'`), `Legacy` (a volume under the old key prefix), `Versioned` (versioning on); findings F2 and F3, fixed |
| `TestIntegrationS3CleanMany` | `clean` of 1100 orphans (more than one listing page); subtest `Interrupted`: a SIGTERM in the middle of 3000 deletes, then a clean that finishes |
| `TestIntegrationS3PGPKeys` | each `testdata/pgp` fixture (`ed25519`, `ed25519-pass` with `PGP_PASSPHRASE`, `rsa`): send, list, restore from an empty cache; manifest key fingerprints; another fixture's key refused with ring advice; a wrong passphrase uploads nothing |
| `TestIntegrationS3PGPModes` | encrypt-only and sign-only sends restore; encrypted pipe-mode receive (`--maxFileBuffer 0`); a tampered signed volume fails its manifest check |
| `TestIntegrationS3PGPRotation` | a chain signed with the RSA fixture, then the Ed25519 key: smart send and `receive --auto` refuse the old signatures without `--trustSigner` and work with it |
| `TestIntegrationS3PGPInterop` | stock `gpg` (in the image) decrypts and verifies every object; the decrypted volumes feed `zfs receive`; subtests `ed25519`, `rsa`, and `runbook` (a key made with the runbook's `gpg` commands, gpg 2.4, with a passphrase). Skipped without `gpg` |
| `TestIntegrationS3Large` | 1 GiB with the default flags (`--volsize 200`, `--maxFileBuffer 5`, `--maxParallelUploads 4`, gzip): six volumes, each a multipart upload of 10 MiB parts (checked by ETag), file-buffered and in pipe mode; restores of each; subtest `Replication`: `send -R` of a parent with two children, received as a tree |
| `TestIntegrationS3ReviewGUID` | finding 7: a snapshot of one name and second on two datasets (one `zfs snapshot` call, two guids); a backup of one received into the other is refused (explicit and `--auto`, with and without `-F`); a real copy gets "already exists" |
| `TestIntegrationS3ReviewMountGuard` | findings 8 and 9: subtests `Replication` (a `-R` child with a local mountpoint, mounted, on the same host), `Shared` (`--properties`, source gone, another dataset mounted at its path) and `SharedReplication` (the same for a `-R` child): refused without `--unmounted`, the live files visible; received unmounted with it |
| `TestIntegrationS3ReviewForce` | finding 10: `-F` without `--allowDestroy` refuses a full over a dataset without snapshots, a local snapshot newer than the source (and one of the source's second), a `-R` incremental into a tree with another child; with it each is received and the data is lost only then; `--auto -F` of a 3-link `-R` chain into a new dataset needs no `--allowDestroy` (W2) |
| `TestIntegrationS3ReviewMultipartAbort` | finding 12: SIGTERM during the multipart upload of a 64 MiB volume (`--maxUploadSpeed`); afterwards the bucket has no multipart upload of it |
| `TestIntegrationS3ReviewBookmarkReplication` | finding 14: `zfs send -n -P` with `-R`, `-p` or `-I` from a bookmark exits non-zero; smart and explicit `-R` incrementals use `@x` when `#x` ties with it; a last backup left only as a bookmark gives a source-pruned full with a Warning |
| `TestIntegrationS3SendModes` | the send flags no other test runs, one subtest each: `DryRun` (`send -n` against S3 names the backup, uploads nothing, takes no lock), `Increment` (`--increment` with no suffixes: newest snapshot from the newest backup, then a noop), `Intermediary` (`-I`: the restore lists every snapshot between, with the sender's guids), `SkipMissing` (`-R` of a tree where a child lacks the snapshot: refused without `-s`, the child skipped with it), `ZfsCompressor` (`--compressor zfs` = `zfs send -c`: the objects are smaller than the data), `External` (`gzip`, `pigz`, `xz`, `zstd` at `--compressionLevel 1`: each format's magic bytes, restored by the same binary), `Dedup` (`-D`) |
| `TestIntegrationS3Plan` | `plan` against the real pool and S3 before each smart send names the run (`FULL`, `INCR ... from`, `NOOP`) that send then makes; it uploads nothing; `--jsonOutput`; `--schedule` on real input |
| `TestIntegrationS3Corrupt` | a volume with a flipped byte, a byte short and a byte long: a file-mode receive refuses it by size and SHA-256 before `zfs receive` reads it; a pipe-mode receive fails too; no dataset is left; a manifest that is not gzip fails `list` and `receive` by name |
| `TestIntegrationS3Zvol` | a zvol (mountpoint `-`) with data written through its block device (`mknod`, no udev in the container): full, incremental, explicit and `--auto` restores give the same bytes and `volsize`; `-F` over a volume with no snapshots needs `--allowDestroy` |
| `TestIntegrationS3Names` | a dataset named `na me:1` (a space and a colon, both legal): send, `list`, `receive --auto` into `re stored:1`, `clean --dry-run` with nothing unrecognized |
| `TestIntegrationS3KeyExpiry` | a key that expires in 45 s: refused without `--allowKeyExpiry`, sent with it, restored before the expiry, refused after it with `key expired` (the reason for the rule). About a minute |

Every restore of a whole chain runs with an empty working directory, so the
manifests come from S3, and every restored snapshot must keep the sender's
`guid`.

What the VM showed (zfs 2.4.4), as assumed by
`docs/specs/2026-10-11--integration-test-more/plan.md`:

- A1: a full stream into a dataset with snapshots fails (`destination has snapshots`) and destroys nothing.
- A2: `zfs receive -d` creates the parent datasets.
- A3: `zfs receive` without `-F` into a changed dataset fails (`destination ... has been modified`) and keeps the change.
- A4: a `-p` stream carries the source's local `mountpoint`. Without `--unmounted` the restored dataset would mount OVER the live one and hide its files; receive refuses that since ad5b6ed (`ReceiveSafety/Mounted`).
- A5: `receive --origin` of an incremental works since 2026-10-12 (`ReceiveSafety/Origin`); before, the program refused it before zfs ran.
- A6: `zfs load-key -L file://<key>` opens a raw-received dataset with the sender's key.
- A7: retired: `send` and `receive --auto` refuse a chain that mixes raw and plaintext.
- A8: a plaintext stream received under an encryption root inherits its encryption.
- zfs takes one snapshot of a dataset per `zfs snapshot` call (and per channel program): a tie takes separate calls in one second (`snapshotTie`).

What the `clean` tests found (2026-10-12; F1 to F3 fixed the same day):

- F1 (`CleanEncrypted`): with no rings at all, clean reads an encrypted
  manifest as plain gzip (`gzip: invalid header`) and says to "delete that
  object from the destination". It is a backup encrypted to a key this run
  does not have. It also deletes that manifest's cached copy. Nothing at the
  destination is deleted. With another key's rings the advice is right
  (`--secretKeyRingPath`). Fixed: with no keys, a manifest that starts with an
  OpenPGP packet is a `KeyError` (encrypted: needs the secret key; the copy
  stays), not a gzip error.
- F2 (`CleanLayout`): the manifest of a nested destination
  (`run/sub/manifests|...`) is read as a manifest of another
  `--manifestPrefix` (`sub/manifests`). An orphan at `run/` of a set that
  `run/sub/` also names is spared, with the Notice "under another
  --manifestPrefix". Nothing wrong is deleted. Fixed: a manifest under a
  nested root whose set also has a volume under that root is the nested
  destination's own and names no other prefix's set. A job whose
  `--manifestPrefix` is `x/manifests` (volumes outside `x/`) is still spared.
- F3 (`CleanLayout/Separator`): clean reads `manifests#...` as its own
  manifests, but its listing loop names them "unrecognized object ... (not a
  backup volume)": that check uses `manifests|`. Nothing wrong is deleted.
  Fixed: the loop skips every manifest `isManifestObject` reads as ours.
- A denied delete (`AccessDenied`) is retried for up to 10 minutes before
  clean fails (`CleanLocal/Denied` ends it with a 20 s deadline).

The S3 server is MinIO in a second container. Each test makes its own bucket
and deletes it. All tests together take a few minutes (`TestIntegrationS3Large` moves gigabytes); the test binary runs
with `-test.timeout 45m`.

## The host needs the zfs kernel module

A container cannot load a kernel module. The `zfs` container uses `/dev/zfs`
of the host that runs Docker. Docker Desktop and OrbStack on a Mac have no zfs
module. On a host with no module the container stops at once with
`entrypoint: no /dev/zfs`.

The container is privileged, so it sees the host's pools. The entrypoint makes
pool `zbtest` (set `ZFSBACKUP_TEST_POOL` to change it) on a 2 GiB file. It
refuses to start when a pool of that name exists, and it destroys only the pool
it made.

The zfs module opens the pool's file by its path on the host. So
`/var/tmp/zfsbackup-it` on the host is bind-mounted at the same path in the
container. Docker makes the directory when it is missing.

## How to run it

| Where | Command |
|---|---|
| Mac | `make integration-vm` |
| Linux host with the zfs module and Docker | `make integration-docker` |
| CI (`ubuntu-24.04` runner) | `sudo apt-get install zfsutils-linux`, then `make integration-docker` |

`RUN` selects tests (a `go test -run` pattern; the default runs all
`TestIntegrationS3*`): `make integration-vm RUN=TestIntegrationS3Resume`, or
`RUN=TestIntegrationS3/Pipe` for one subtest, or
`RUN=TestIntegrationS3PGP` for the four OpenPGP tests
(`RUN=TestIntegrationS3CleanLayout/Separator` runs the parent's steps, then
that subtest).

On a Linux host the image's zfs userland (Ubuntu 24.04, zfs 2.2) must match
the host's module. Use an Ubuntu 24.04 host, or set `ZFS_USERLAND` as the VM
does (see `compose.nixos.yaml`).

### The VM on a Mac (`make integration-vm`)

Needs Lima: `brew install lima`. The VM is NixOS (the
[nixos-lima](https://github.com/nixos-lima/nixos-lima) image, `lima.yaml`).

- First `make integration-vm`: it downloads the image, applies
  `integration/nixos/` with `nixos-rebuild` (zfs, Docker, make, the LTS kernel)
  and restarts the VM. Some minutes.
- Later runs: the VM starts in about 20 s, then the image build and the test.
- In the VM the test container runs the VM's own zfs userland: `/nix/store`
  is mounted read-only and `ZFS_USERLAND` points into it
  (`compose.nixos.yaml`). Userland and module are always one build.
- The VM sees your home directory read-only, at the same path.

| Target | Use |
|---|---|
| `make integration-vm-up` | create, start and set up the VM |
| `make integration-vm-shell` | a shell in the VM |
| `make integration-vm-down` | stop the VM |
| `make integration-vm-delete` | delete the VM |

`LIMA_VM` names the instance (default `zfsbackup`). To change the NixOS
system, edit `integration/nixos/`, then `make integration-vm-delete
integration-vm` (`vm-up.sh` applies the flake only when the VM has no
`/dev/zfs`). `integration/nixos/flake.lock` pins nixpkgs and nixos-lima; keep
nixpkgs on the branch of the nixos-lima tag in `lima.yaml`.

## Real AWS S3

```sh
export AWS_REGION=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # and AWS_SESSION_TOKEN if you have one
export ZFSBACKUP_IT_BUCKET=<a bucket these keys can write>
make integration-vm INTEGRATION_AWS=1        # or integration-docker on Linux
```

`aws configure export-credentials --format env` prints the key variables of a
profile. `INTEGRATION_AWS=1` adds `compose.aws.yaml`: no custom endpoint, and
these variables from your environment (compose stops when one is unset). For
the VM, `limactl shell --preserve-env` passes your environment in; without
`INTEGRATION_AWS` the VM gets none of it.

The test writes under `zfsbackup-it-<unix seconds>/` and
`zfsbackup-it-pipe-<unix seconds>/` in the bucket and deletes those objects
at the end, also when it fails. A killed run leaves them. A run costs a few
cents: some MiB of PUT and GET, no Glacier. MinIO still starts; nothing uses
it.

## A shell in the test container

In the VM (`make integration-vm-shell`), or on a Linux host:

```sh
cd <repo>
docker compose -f integration/compose.yaml -f integration/compose.nixos.yaml run --rm --entrypoint bash zfs
```

Leave out `-f integration/compose.nixos.yaml` on a non-NixOS host. In the VM,
first `export ZFS_USERLAND=$(dirname $(readlink -f $(command -v zfs)))`, and in
the container `export PATH=$ZFS_USERLAND:$PATH`. `compose run` keeps
`privileged` and the devices of the service. It starts MinIO too; `docker
compose -f ... down -v` stops it.

## Environment

| Variable | Default | Use |
|---|---|---|
| `RUN` | `TestIntegrationS3` | `-test.run` pattern |
| `ZFSBACKUP_TEST_POOL` | `zbtest` (`tank` outside the container) | pool the test uses |
| `ZFS_USERLAND` | unset | bin directory of a zfs userland to put first on `PATH` |
| `AWS_S3_CUSTOM_ENDPOINT` | `http://minio:9000` | S3 endpoint; empty with `INTEGRATION_AWS=1` |
| `ZFSBACKUP_IT_BUCKET` | unset | use this existing bucket, under a prefix per run, not a new bucket |
| `INTEGRATION_AWS` (make) | unset | `1`: real AWS S3, see above |
| `LIMA_VM` (make) | `zfsbackup` | Lima instance name |
| `ZFSBACKUP_IT_NODELETE_KEY`, `ZFSBACKUP_IT_NODELETE_SECRET` | `nodelete`, `nodelete-secret`; empty with `INTEGRATION_AWS=1` | keys of a user without `s3:DeleteObject`; unset, `TestIntegrationS3CleanLocal/Denied` skips |

The `mc` service of `compose.yaml` (`pgsty/mc`, pinned) runs once before the
test: it adds the MinIO user `nodelete` with the policy
`nodelete-policy.json` (list, read, write; no delete), then exits. The `zfs`
service waits for it to finish. `make integration-docker` does not attach to
it (`--no-attach mc`): with `--abort-on-container-exit` its exit would stop
the run.
