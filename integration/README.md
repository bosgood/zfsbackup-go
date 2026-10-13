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
| `TestIntegrationS3ReceiveSafety` | receive without `-F` onto a changed dataset; `--properties` with `--unmounted`; subtests `Mounted` and `Origin` are RED on purpose (findings, below) |
| `TestIntegrationS3TwoDestinations` | one send to two S3 prefixes; `complete-partial` without re-upload; out-of-sync destinations refused; each restores alone |
| `TestIntegrationS3ManifestPrefix` | two `--manifestPrefix` jobs in one bucket: `list` and `clean` keep to their prefix |
| `TestIntegrationS3NativeEncryption` | ZFS native encryption: a `--raw` chain with the key unloaded, a raw restore that opens only with the sender's key, a raw incremental from a bookmark, a forgotten `--raw` refused, a plaintext send, raw + GPG |
| `TestIntegrationS3PGPKeys` | each `testdata/pgp` fixture (`ed25519`, `ed25519-pass` with `PGP_PASSPHRASE`, `rsa`): send, list, restore from an empty cache; manifest key fingerprints; another fixture's key refused with ring advice; a wrong passphrase uploads nothing |
| `TestIntegrationS3PGPModes` | encrypt-only and sign-only sends restore; encrypted pipe-mode receive (`--maxFileBuffer 0`); a tampered signed volume fails its manifest check |
| `TestIntegrationS3PGPRotation` | a chain signed with the RSA fixture, then the Ed25519 key: smart send and `receive --auto` refuse the old signatures without `--trustSigner` and work with it |
| `TestIntegrationS3PGPInterop` | stock `gpg` (in the image) decrypts and verifies every object; the decrypted volumes feed `zfs receive`; subtests `ed25519`, `rsa`, and `runbook` (a key made with the runbook's `gpg` commands, gpg 2.4, with a passphrase). Skipped without `gpg` |
| `TestIntegrationS3Large` | 1 GiB with the default flags (`--volsize 200`, `--maxFileBuffer 5`, `--maxParallelUploads 4`, gzip): six volumes, each a multipart upload of 10 MiB parts (checked by ETag), file-buffered and in pipe mode; restores of each; subtest `Replication`: `send -R` of a parent with two children, received as a tree |

Every restore of a whole chain runs with an empty working directory, so the
manifests come from S3, and every restored snapshot must keep the sender's
`guid`.

What the VM showed (zfs 2.4.4), as assumed by
`docs/specs/2026-10-11--integration-test-more/plan.md`:

- A1: a full stream into a dataset with snapshots fails (`destination has snapshots`) and destroys nothing.
- A2: `zfs receive -d` creates the parent datasets.
- A3: `zfs receive` without `-F` into a changed dataset fails (`destination ... has been modified`) and keeps the change.
- A4: a `-p` stream carries the source's local `mountpoint`. Without `--unmounted` the restored dataset mounts OVER the live one and hides its files (`ReceiveSafety/Mounted`, red).
- A5: not checked: `receive --origin` of an incremental is refused by the program before zfs runs (`ReceiveSafety/Origin`, red).
- A6: `zfs load-key -L file://<key>` opens a raw-received dataset with the sender's key.
- A7: retired: `send` and `receive --auto` refuse a chain that mixes raw and plaintext.
- A8: a plaintext stream received under an encryption root inherits its encryption.
- zfs takes one snapshot of a dataset per `zfs snapshot` call (and per channel program): a tie takes separate calls in one second (`snapshotTie`).

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
`RUN=TestIntegrationS3/Pipe` for one subtest, or `RUN=TestIntegrationS3PGP` for
the four OpenPGP tests.

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
