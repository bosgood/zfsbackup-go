# Testing and build

Read this before you run anything. All repeatable work goes through the Makefile. If a target is missing, add one.

## Targets

| Target | Use |
|---|---|
| `make test-docker` | the supported path: build the `test` image and run `go build ./...` + `go test ./...` inside it |
| `make test-one RUN=TestName PKG=./backup/` | one test in the pinned Go image, no image rebuild |
| `make test-run RUN='TestPlan' PKG=./backup/` | one test on the host |
| `make e2e` | root-package end-to-end tests with `internal/fakezfs` |
| `make scenarios` / `make scenarios-update` | golden plan scenarios, see [plan.md](plan.md) |
| `make test-enospc` | full-disk tests on a 40 KiB tmpfs |
| `make lint` | golangci-lint v2, pinned image |
| `make fmt-check` | `gofmt -s -l` in the pinned image. `make fmt` rewrites every tracked file with the host's gofmt. |
| `make fmt-write FILES="a.go b.go"` | `gofmt -s -w` on those files in the pinned image. `fmt-check` sees tracked files only: check a new file after `git add`. |
| `make build-rev REV=master` | build an older revision (its `vendor/`, pinned image, Linux) to `tmp/zfsbackup-<REV>` (a `/` in REV becomes `-`) |
| `make fixture-old REV=master` | `build-rev`, then write a destination with it to `testdata/manifests/<REV>-<hash>/` (`testdata/manifests/make-fixture.sh`, a `zfs` shim). Never regenerate a committed fixture to match new code |
| `make integration` | needs a real ZFS host and `tank/data@c`; build tag `integration` |
| `make integration-docker` | the eighteen `TestIntegrationS3*` tests: real zfs + MinIO in Docker (`integration/compose.yaml`). Needs a Linux host with the zfs module. `RUN=TestIntegrationS3Resume` picks one, `RUN=TestIntegrationS3/Pipe` a subtest |
| `make integration-vm` | the same in a NixOS Lima VM: the way on a Mac. `integration-vm-up`, `-shell`, `-down`, `-delete`. See `integration/README.md` |
| `make integration-image` | only build the `integration` image (compiles and vets the tagged test) |
| `make plan ARGS="..."` | run `plan` with the working tree |
| `make mod-update` | upgrade every module in the pinned image (the host cannot reach `proxy.golang.org`). Then `make mod-vendor` |
| `make mod-get MOD=path@version` | one module at one version, in the pinned image |
| `make mod-vendor` | `go mod tidy`, `go mod vendor`, `go mod verify` in the pinned image |
| `make vulncheck` | `govulncheck@latest ./...` in the pinned image |

Deps are vendored. Go version comes from `go.mod` (`X.Y` only: `go 1.26.0` gives `golang:1.26-bookworm`) and must match the Dockerfile default. Nix is not on this host.

## Test layers

1. **Pure unit tests.** `planSmartSnapshots` and friends in `backup/plan*_test.go`. No ZFS, no network.
2. **Package tests with `file://`.** `backup/clean_test.go`, `restore_test.go`. Set `config.WorkingDir` to a temp dir.
3. **E2E** in the root package (`e2e_*_test.go`). `newE2EEnv(t)` builds a `file://` destination and a working dir. `env.send(...)`, `env.plan(...)`. The test binary is the `zfs` binary.
4. **Integration** behind the build tag: eighteen `TestIntegrationS3*` tests run real zfs in Docker against MinIO (`make integration-docker`); on a Mac, `make integration-vm`. Plaintext volumes: send/receive with pipe mode, 1 GiB with the default flags (multipart uploads, checked by ETag) and a `-R` tree, a 3-link chain, bookmarks, `list` and `clean`, `--resume` after a cut `zfs send`, a window-elapsed full, the production flags with creation-time ties and `--fullPath`/`--lastPath`, receive safety (`-F`, `--unmounted`, `--origin`), two destinations, two `--manifestPrefix` jobs. GPG volumes: the chain encrypted and signed, `--resume` with rings; the GnuPG-exported fixtures in `testdata/pgp` (Ed25519, Ed25519 with `PGP_PASSPHRASE`, RSA) through send, list and receive, with another key and a wrong passphrase refused; encrypt-only, sign-only (a tampered volume refused) and encrypted pipe-mode receive; an RSA to Ed25519 signing-key rotation with `--trustSigner`; stock `gpg` decrypts the uploaded objects and its output feeds `zfs receive`, also with a key the runbook's `gpg` commands made (gpg 2.4) (`RUN=TestIntegrationS3PGP`). ZFS native encryption: a `--raw` chain, the key unloaded, raw restores, a forgotten `--raw` refused, raw + GPG. Whole-chain restores use an empty working directory and check each snapshot's `guid`. `TestIntegrationS3ReceiveSafety/Mounted` and `/Origin` are red on purpose (findings; see `integration/README.md`). `TestIntegration` (Azure + S3) still needs Azurite and `tank/data@c`. Lint covers the tag (`.golangci.yml` `build-tags`).

## `internal/fakezfs`

A pure-Go `zfs`. The test binary becomes the fake when `FAKEZFS=1` (`RunIfRequested` in `TestMain`). Point `--zfsPath` at `os.Args[0]`.

| Env | Effect |
|---|---|
| `FAKEZFS_SNAPSHOTS` | snapshot fixture, text format of `zfs.ParseSnapshotList` |
| `FAKEZFS_STREAM_BYTES` | bytes `zfs send` writes (default 65536) |
| `FAKEZFS_FAIL_AFTER_BYTES` | send dies mid-stream after N bytes |
| `FAKEZFS_STREAM_SALT` | change stream bytes, keep GUIDs |
| `FAKEZFS_RECEIVE_LOG` | file that records `<bytes> <sha256>` of each receive |
| `FAKEZFS_RECEIVE_UNKNOWN` | file of `dataset@name` lines; a received stream that matches no fixture snapshot (a shim's) takes the first line |
| `FAKEZFS_DRYRUN_OUTPUT` | replaces `zfs send -n -P` output |
| `FAKEZFS_LOG` | one line per invocation |
| `FAKEZFS_MOUNTPOINTS` | `dataset=source` entries, comma-separated: the source `zfs get mountpoint` reports (`local`, `received`); others are `default`. The value is always `/<dataset>` |

`zfs receive` adds the snapshot it received to `FAKEZFS_SNAPSHOTS` (as `-d`/`-e` map the name, with the sent creation time), as real zfs lists it afterwards. It finds the snapshot by comparing the stream with what `zfs send` writes for each fixture snapshot, with the current `FAKEZFS_STREAM_SALT`. A second receive of the same snapshot is an error. A test that receives one snapshot again must put the fixture back (see `checkRestores`).

## Habits that work

- Prove a regression test fails on the old code before you trust it.
- Capture logs with `captureLogs(t)` (`backup/backup_test.go`).
- Scratch experiments: `git archive HEAD | tar -x -C /tmp/scratch`, mount that dir instead of the tree.
- Hangs: run `env.send` in a goroutine, `select` on a timeout, dump `runtime.Stack` filtered to `zfsbackup-go/backup.`.
- Test files still use `io/ioutil`. Match the package you edit.
- Many details in `.claude/napkin.md`.
