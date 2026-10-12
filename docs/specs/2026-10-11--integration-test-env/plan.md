# Integration test env: implementation plan

Date: 2026-10-11
Branch: `clean-dry-run`
Source: `intent.md` in this directory.

## Goal

A test that runs real `zfs` in a Docker container, takes sanoid-style
snapshots, sends them through the real `s3://` backend, receives them into
a new dataset, and compares the files. One `make` target runs it.

## Facts that shape the plan

Checked on 2026-10-11. Do not re-derive them.

1. **ZFS needs the kernel module of the host that runs Docker.** A container
   cannot load it. This Mac runs OrbStack (arm64). Its kernel has no `zfs`
   module and no `/lib/modules`. The OrbStack maintainer does not plan to
   ship it (orbstack/orbstack#258, open since 2023). OrbStack 2.1 passes a
   host `/dev/zfs` through if one exists, but a module built by hand against
   the OrbStack kernel breaks on each OrbStack update. Not a base for a test.
2. So the container runs on a **Linux VM with the module**. Lima is the tool
   for that on this Mac. It is not installed (`brew install lima`). Ubuntu
   24.04 ships the module in `linux-modules-extra`; `apt install
zfsutils-linux` loads it. A Linux host or CI runner with the module works
   the same way, with no VM.
3. **Userland and module versions must match.** Debian bookworm (the Go
   image base) has `zfsutils-linux` 2.1.11 in `contrib`; Ubuntu 24.04 has
   2.2.x. The test image is `ubuntu:24.04` plus the Go toolchain copied from
   the `toolchain` stage. The VM is Ubuntu 24.04. Same version on both sides.
4. **The S3 backend already supports a local S3.** `AWS_S3_CUSTOM_ENDPOINT`
   plus `AWS_REGION` and the two key variables; path style is forced
   (`backends/aws_s3_backend.go` `Init`). `Init` lists the bucket, so the
   bucket must exist first. `integration_test.go` `setupS3Bucket` creates and
   deletes one with the SDK. MinIO is the local S3.
5. **The existing `TestIntegration` stays as it is.** It needs Azurite and
   `tank/data@c`, and skips when `AZURE_CUSTOM_ENDPOINT` is unset. It is not
   the test this plan adds.
6. **Pool names are global in the kernel.** A privileged container sees the
   host's pools. The test pool gets its own name (`zbtest`), and the
   entrypoint refuses to run when a pool of that name already exists.
7. **Sanoid names.** `autosnap_2026-09-01_00:00:00_monthly`, `_daily`,
   `_hourly`. The smart flags are `--fullIfOlderThan`, `--fullSnapshotSuffix`,
   `--incrementalSnapshotSuffix` (`backup/testdata/scenarios/*/flags`).
   `files.SnapshotInfo.Equal` compares name and creation time to the second.

## Alternative considered: an OrbStack NixOS machine

Asked 2026-10-11. It can work: laris/orbstack-nixos-openzfs builds the
OpenZFS 2.4.1 module from source against OrbStack's own headers
(`/sys/kernel/kheaders.tar.xz`), with one Linux patch and four OpenZFS
patches, and loads it with a systemd unit. Then `/dev/zfs` exists and
OrbStack 2.1+ passes it to machines and containers. Rejected as the base
for the test env: the module is tied to the exact OrbStack kernel
(`6.17.8-orbstack-...` when written), so every OrbStack update needs a
rebuild and may need new patches. A build chain of that kind has no place in
`make integration-vm`. It stays an option for a personal setup; task 2's
compose file would run unchanged against it.

## Assumptions

- "Actual S3 backend" means the real `s3://` backend code against an S3 API.
  MinIO is the default target. Real AWS S3 works through the same test when
  `AWS_S3_CUSTOM_ENDPOINT` is unset and `ZFSBACKUP_IT_BUCKET` names a bucket
  the ambient AWS credentials can write to. Task 4 adds that switch.
- The user installs Lima (`brew install lima`). The Makefile checks for it
  and prints that line when it is missing. Nothing in this plan installs
  software on the Mac.

## Rules for this repo

Read `.claude/napkin.md` first. The rules that matter here:

- Build and test through the Makefile. Add a target for every new command.
- Find code with `semble search` (concepts) and `cs` (symbols), as described
  in `.claude/skills/codesearch/SKILL.md`. Do not use grep to locate code.
- In-process cobra runs leak flag values. Pass bool flags explicitly, call
  `cmd.RootCmd.SetArgs(nil)` and the `Reset*JobInfo` function after each run.
- A test that passes the first time proves nothing. Each task names the
  break that must make it fail.
- Never shape production code around a test environment's limit.
- Another session may edit this tree. Run `git status` before you stage, and
  stage files by name.
- Add one `CHANGELOG.md` line per task that a user can see, at the top, in the
  `* [ADD] test: ...` or `* [ADD] dev: ...` style.

## Layout

```
Dockerfile              new stage `integration` (task 1)
integration/
  compose.yaml          minio + zfs services (task 2)
  entrypoint.sh         pool create, run tests, pool destroy (task 1)
  lima.yaml             Ubuntu 24.04 VM with zfs + docker (task 5)
integration_s3_test.go  the new test, build tag integration (task 3)
integration_test.go     setupS3Bucket takes a bucket name (task 3)
```

The Dockerfile stage lives in the root `Dockerfile` with the others, so
`GO_VERSION` and the `toolchain` stage are shared. Everything else lives in
`integration/`.

## Task 1: the `zfs` image and its entrypoint

Files: `Dockerfile` (new stage `integration`), `integration/entrypoint.sh`.

Stage `integration`:

```dockerfile
FROM ubuntu:24.04 AS integration
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates diffutils zfsutils-linux \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=vendor
WORKDIR /src
COPY . .
RUN go vet -tags integration . && go test -c -tags integration -o /usr/local/bin/zfsbackup-integration.test .
COPY integration/entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

`--no-install-recommends` matters: without it apt pulls `zfs-dkms` and tries
to build a module in the image. The test binary is built at image build
time, so a compile error fails `docker compose build`, not the run. The
source stays in the image because the test runs from `/src` (cwd for
`testdata/`).

`entrypoint.sh` (`set -euo pipefail`):

1. `POOL=${ZFSBACKUP_TEST_POOL:-zbtest}`. Refuse with a clear message when
   `zpool list -H -o name` already lists `$POOL`: it may be the host's pool.
   Never destroy a pool this script did not create.
2. `truncate -s 2G /var/tmp/pool.img`; `zpool create -f -O
mountpoint=/$POOL -O compression=off $POOL /var/tmp/pool.img`.
3. `trap 'zpool destroy -f "$POOL"' EXIT`. Without this, the pool stays
   imported in the host kernel after the container exits, and the next run
   hits the refusal in step 1.
4. Wait for MinIO: loop on `curl -sf "$AWS_S3_CUSTOM_ENDPOINT/minio/health/live"`
   up to 30 s, when the endpoint is set.
5. `cd /src && exec` is wrong (the trap would not run). Run
   `zfsbackup-integration.test -test.v -test.count=1 -test.run "${RUN:-TestIntegrationS3}"`
   and exit with its status.

Needs `--privileged` and `/dev/zfs` (task 2). `zpool create` in a container
also needs the mount syscalls, which `privileged` gives.

Check: `docker build --target integration -t zfsbackup-integration .` on
this Mac builds (no module needed for the build). `docker run --rm
--privileged zfsbackup-integration` on this Mac must fail at `zpool create`
with the module error, not earlier: that is the expected result here.

## Task 2: compose file

File: `integration/compose.yaml`.

```yaml
services:
  minio:
    image: quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z # pin; bump on purpose
    command: server /data
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:9000/minio/health/live"]
      interval: 2s
      retries: 30
  zfs:
    build:
      {
        context: ..,
        dockerfile: Dockerfile,
        target: integration,
        args: { GO_VERSION: "${GO_VERSION:-1.25}" },
      }
    privileged: true
    devices: ["/dev/zfs:/dev/zfs"]
    depends_on: { minio: { condition: service_healthy } }
    environment:
      AWS_S3_CUSTOM_ENDPOINT: http://minio:9000
      AWS_REGION: us-east-1
      AWS_ACCESS_KEY_ID: minioadmin
      AWS_SECRET_ACCESS_KEY: minioadmin
      ZFSBACKUP_TEST_POOL: zbtest
      RUN: "${RUN:-TestIntegrationS3}"
```

Check the MinIO image tag against quay.io before pinning; the one above is a
placeholder for "the current release on the day of the commit". Check that
the MinIO image still has `curl` for the healthcheck; if not, use `mc ready
local` or drop the healthcheck and rely on the entrypoint's wait loop.

Makefile:

```make
# ZFS + S3 integration test in Docker. Needs a Linux host or VM whose kernel
# has the zfs module (see integration/README.md); on this Mac use integration-vm.
integration-docker:
	GO_VERSION=$(GO_VERSION) docker compose -f integration/compose.yaml up \
		--build --abort-on-container-exit --exit-code-from zfs; \
	status=$$?; docker compose -f integration/compose.yaml down -v; exit $$status
```

`down -v` runs even when the test fails, and the target keeps the test's
exit status. `RUN=TestIntegrationS3/Pipe make integration-docker` selects a
subtest.

## Task 3: the test

Files: `integration_s3_test.go` (new, `//go:build integration`, `package
main`), `integration_test.go` (make `setupS3Bucket` take the bucket name;
`TestIntegration` passes `s3TestBucketName`).

Helpers, in the new file:

- `testPool(t)`: `ZFSBACKUP_TEST_POOL`, default `tank` so the old test's
  names keep working on a real host. Skip the test when `zpool list <pool>`
  fails: "no pool <name>; run through make integration-docker".
- `zfsRun(t, args...)`: run `zfs` with the given args, fail with stderr.
- `writeFile(t, dir, name, size)`: `size` bytes from `crypto/rand`, so the
  compressor cannot shrink the stream and `--volsize 1` gives several
  volumes.
- `sanoidName(t time.Time, kind string)`:
  `"autosnap_" + t.UTC().Format("2006-01-02_15:04:05") + "_" + kind`.
- `snapshot(t, dataset, kind)`: waits until the wall clock second differs
  from the last snapshot it took, then `zfs snapshot`. Two snapshots in one
  second would tie on creation time, and the napkin says selection must not
  depend on tie order. Returns the name.
- `run(t, reset, args...)`: in-process cobra run as `e2eEnv.run` does,
  `--zfsPath zfs`, `--workingDirectory <t.TempDir()>`, logs captured and
  returned. `t.Cleanup` resets. Pass `--maxRetryTime 2m` on every run so a
  broken MinIO fails the test in minutes, not the 12 h default.
- `newestBackup` (`e2e_test.go`) and `compareDirs` (`integration_test.go`)
  are in the same package and the e2e files carry no build tag, so both
  compile into the tagged binary. Reuse them; do not copy them. The e2e
  `TestMain` runs too; without `FAKEZFS=1` it is a no-op.

`TestIntegrationS3`:

1. Bucket `zfsbackup-it-<unix seconds>` via `setupS3Bucket(t, name)`; the
   returned cleanup deletes the objects and the bucket. Target
   `s3://<bucket>/`.
2. `src := pool + "/src"`, `zfs create -o mountpoint=/<pool>/src`. Write
   `a.bin` of 3 MiB. Snapshots: `_monthly`, then `_daily`, then `_hourly`.
3. **Full.** `send --fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
--incrementalSnapshotSuffix _daily --volsize 1 src s3://bucket`.
   Assert: the newest backup at the target has `BaseSnapshot.Name` ==
   the monthly and no incremental source. Assert the bucket holds at least 3
   volume objects plus one manifest (the SDK client from `setupS3Bucket`).
4. Write `b.bin` of 1 MiB. Snapshots: `_daily` (call it daily2), then
   `_hourly`.
5. **Incremental.** Same flags. Assert base == daily2, source == the monthly.
   The `_hourly` snapshots are newer than daily2 and must NOT be picked.
   This is the sanoid-suffix assertion.
6. **Noop.** Same flags again. Assert `errors.Is(err, backup.ErrNoOp)`.
7. **Receive, explicit full.** `receive -F src@<monthly> s3://bucket
<pool>/restored`. Assert `zfs list -t snapshot <pool>/restored` names the
   monthly. `compareDirs` cannot compare yet: the source has `b.bin` now.
   Compare against `/<pool>/src/.zfs/snapshot/<monthly>` instead
   (`compareDirs` excludes `.zfs` only at the top, so this path works; if
   the snapdir is hidden, `zfs set snapdir=visible` on `src` in step 2).
8. **Receive, --auto to newest.** `receive --auto -F src s3://bucket
<pool>/restored`. The chain walks monthly -> daily2; monthly is local,
   so one step runs. Assert both snapshots are on `restored`, and
   `compareDirs("/<pool>/src", "/<pool>/restored")`.
9. **Receive, --auto into a fresh dataset.** `receive --auto -F src
s3://bucket <pool>/restored2`. Two steps run (full, then incremental).
   `compareDirs` again. This is the "restore from nothing" case an operator
   cares about.
10. **Pipe mode subtest** `t.Run("Pipe")`: a new bucket, `send ... --maxFileBuffer
0 --full src s3://bucket2`, then `receive -F --maxFileBuffer 0 ...` into
    `<pool>/restored3`, `compareDirs`. This exercises S3 multipart upload
    from a pipe, which `file://` tests cannot.

Cleanup: `t.Cleanup(zfs destroy -r <pool>/src, restored, restored2,
restored3)`. The entrypoint destroys the pool anyway; the cleanup matters
when the test runs on a real host (`make integration`).

Break-the-test proof (do each, see the failure, undo):

- Step 5: run with `--incrementalSnapshotSuffix _hourly`. The assertion
  "base == daily2" must fail. Proves the suffix assertion is real.
- Step 8: after the receive, `echo x >> /<pool>/restored/a.bin` before
  `compareDirs`. Must fail. Proves the comparison is real.
- Step 3: delete one volume object from the bucket before step 7. The
  receive must fail with the missing-object error, within `--maxRetryTime`.
  Proves the test runs against the bucket, not a cache.

Record the three proofs in the commit message.

## Task 4: real AWS S3 switch

Files: `integration_s3_test.go`.

When `AWS_S3_CUSTOM_ENDPOINT` is unset and `ZFSBACKUP_IT_BUCKET` is set,
`setupS3Bucket` skips `CreateBucket` and `DeleteBucket`, uses that bucket,
and the cleanup deletes only the objects under the test's prefix. The test
uses target `s3://<bucket>/it-<unix>/` so parallel runs do not collide, and
the object count assertion lists under that prefix. Credentials come from the
SDK's usual chain. Document in `integration/README.md`: the run costs a few
cents (PUT, GET, no Glacier) and leaves nothing behind when it passes.

`docker compose` run against real AWS: `make integration-docker
AWS_S3_CUSTOM_ENDPOINT= ZFSBACKUP_IT_BUCKET=<b>` with `AWS_*` passed through.
Add `AWS_SESSION_TOKEN` and `ZFSBACKUP_IT_BUCKET` to the compose
`environment` as `${VAR:-}` passthroughs. MinIO still starts; unused.

## Task 5: the VM for this Mac

Files: `integration/lima.yaml`, Makefile targets.

```yaml
# Ubuntu 24.04 with the zfs kernel module and Docker, for make integration-vm.
images:
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
    arch: aarch64
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
    arch: x86_64
vmType: vz
cpus: 4
memory: 6GiB
disk: 30GiB
mounts:
  - location: "~"
    writable: true
provision:
  - mode: system
    script: |
      #!/bin/bash
      set -eux
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y zfsutils-linux docker.io docker-compose-v2
      modprobe zfs
      echo zfs > /etc/modules-load.d/zfs.conf
      usermod -aG docker "${LIMA_CIDATA_USER}"
```

Pin the image to a dated release URL (`releases/24.04/release-YYYYMMDD/`)
with its sha256 `digest:` once chosen, as Lima templates do. The `~` mount
means the repo path is the same inside the VM, so `$(CURDIR)` works there.

Makefile:

```make
LIMA_VM?=zfsbackup
# Lima VM with the zfs module and Docker; the only way to run integration-docker on this Mac.
integration-vm-up:
	@command -v limactl >/dev/null || { echo "integration-vm: install Lima first: brew install lima"; exit 1; }
	limactl list -q | grep -qx '$(LIMA_VM)' || limactl create --name '$(LIMA_VM)' integration/lima.yaml --tty=false
	limactl start '$(LIMA_VM)'
integration-vm: integration-vm-up
	limactl shell '$(LIMA_VM)' -- make -C '$(CURDIR)' integration-docker RUN='$(RUN)'
integration-vm-shell: integration-vm-up
	limactl shell '$(LIMA_VM)'
integration-vm-down:
	limactl stop '$(LIMA_VM)'
integration-vm-delete:
	limactl delete --force '$(LIMA_VM)'
```

Check that `docker` works in the VM without `sudo` after the first login
(`usermod` needs a new session; `limactl shell` starts one). If
`docker compose` inside the VM cannot build from the mounted repo because of
file ownership, add `--mount-type virtiofs` (vz default) notes to the README.

First run downloads the image and installs packages: about 5 minutes. Later
runs: `limactl start` in about 20 s, then the compose build.

## Task 6: docs

Files: `integration/README.md` (new), `docs/context/testing.md`,
`.claude/napkin.md`, `CHANGELOG.md`.

- `integration/README.md`: what runs, the host requirement (kernel module),
  the three ways to run (Linux host, `make integration-vm` on a Mac, CI), the
  env variables, real AWS mode, how to open a shell in the `zfs` container
  (`docker compose -f integration/compose.yaml run --rm --entrypoint bash zfs`
  needs `privileged`; say that compose honors it for `run`).
- `docs/context/testing.md`: rows for `integration-docker`, `integration-vm`;
  layer 4 text: "Integration behind the build tag: `TestIntegrationS3` runs
  real zfs in Docker against MinIO (`make integration-docker`); on a Mac,
  `make integration-vm`. `TestIntegration` (Azure + S3) still needs Azurite".
- Napkin: "OrbStack's kernel has no zfs module; ZFS tests run in the Lima VM
  (`make integration-vm`), never in plain Docker here", and "a privileged
  container sees the host's pools: the entrypoint refuses an existing pool
  name and destroys only what it made".
- CHANGELOG: `* [ADD] test: ZFS + S3 integration test in Docker
(TestIntegrationS3, make integration-docker; make integration-vm boots a
Lima VM with the zfs module on macOS); sanoid-style snapshots, full,
incremental, noop, receive --auto, pipe mode`.

## Order and commits

One commit per task, in order 1, 2, 3, 5, 4, 6. Task 5 comes before 4
because nothing in task 3 can be run on this Mac until the VM exists. Task 3
cannot be verified before task 5: do tasks 1, 2, 3 as a stack, then task 5,
then run the stack and fix.

## Done when

- `make integration-vm` on this Mac passes `TestIntegrationS3` with all
  subtests, twice in a row (the second run proves the pool cleanup).
- The three break-the-test proofs in task 3 each failed and were undone.
- `make test-docker`, `make fmt-check` (after `git add` of the new files),
  and `make lint` pass. `.golangci.yml` has no `build-tags` setting today
  (checked 2026-10-11), so the tagged files are not linted. Add
  `run: build-tags: [integration]` in task 3 and fix what it reports in
  `integration_test.go` as a separate commit, so the new test is linted.
- `docker run --rm zfsbackup-integration` on plain OrbStack prints the
  module error and exits non-zero, not a hang.
- The CHANGELOG line and the docs in task 6 are in.

## Out of scope, noted for later

- A GitHub Actions workflow. `ubuntu-24.04` runners have the module after
  `sudo apt-get install zfsutils-linux`; `make integration-docker` runs
  there as it does in the VM. There is no `.github/workflows/` yet.
- Azurite for `TestIntegration`. Add a service to the compose file when the
  Azure backend gets attention.
- Creation-time windows (`--fullIfOlderThan` elapsed, `source-pruned`). Real
  snapshots are all "now". The golden scenarios cover those paths; the
  integration test covers the real `zfs` and S3 boundaries.

## Execution notes (2026-10-11)

Commits d426bec (task 1), 93a384e (task 2), 5cfb96d (fix), f8240f2
(task 3), 01ed450 (task 5), 637e980 (task 4), then task 6. What changed
against the plan, and why:

- **NixOS VM, not Ubuntu.** The user asked for a NixOS VM. `lima.yaml` uses
  the nixos-lima v0.2.2 image; `integration/nixos/` adds zfs (LTS kernel,
  hostId), Docker and make; `vm-up.sh` applies it once with `nixos-rebuild
  boot` and restarts. Fact 3 (userland = module) is solved another way: the
  container runs the VM's own zfs from `/nix/store` (`compose.nixos.yaml`,
  `ZFS_USERLAND`). The Ubuntu userland in the image is for Ubuntu hosts.
  Docker socket group is wheel, not a docker group (Lima reuses its ssh
  session, so a new group does not apply).
- **MinIO image.** MinIO publishes no images any more; the planned tag does
  not exist. `pgsty/minio:RELEASE.2026-08-04T00-00-00Z` (community build).
- **Pool file path.** The zfs module opens a file vdev by its path in the
  host's mount namespace. The pool file is in `/var/tmp/zfsbackup-it`,
  bind-mounted at the same path.
- **Step 7** compares with a `zfs clone` of the monthly, not
  `.zfs/snapshot/<monthly>`: the kernel mounts snapshots with a helper in
  the host's mount namespace.
- **Step 3** asserts 3+ volumes only with `--compressor ""`: the volume
  counter sees compressed bytes, and pgzip holds several MiB, so with gzip
  the 3 MiB stream was one volume. The Pipe subtest keeps gzip.
- **A real bug.** The Pipe subtest hung: `receive --maxFileBuffer 0` with
  gzip volumes deadlocked (the writer's Close closed the reader's pgzip
  reader before it flushed its buffer). Fixed in 5cfb96d with a unit test
  that timed out on the old code.
- **Task 4** switches on `ZFSBACKUP_IT_BUCKET` alone, so the prefix mode was
  checked against MinIO (0 objects left). `INTEGRATION_AWS=1` adds
  `compose.aws.yaml`; not run against real AWS.
- `compareDirs` logged nothing on a difference (diff writes to stdout); it
  logs the combined output now.
- `.golangci.yml` lints the tag; no findings in `integration_test.go`.
- `make integration-image` (build only) and `make test-one TESTFLAGS=` were
  added.

Break-the-test proofs: all three failed as expected (see f8240f2).
