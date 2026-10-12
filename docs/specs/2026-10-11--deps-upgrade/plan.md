# Plan: upgrade the Go dependencies

Date: 2026-10-11. See `intent.md` for the goal and the scope.

Every fact marked **verified** comes from scratch runs on 2026-10-11:
copies of the tree in `/tmp`, module cache in `/tmp/zfsb-modcache`,
images `golang:1.25-bookworm` (1.25.14) and `golang:1.26-bookworm` (1.26.9).

## 1. One decision for the user: Go 1.26 or Go 1.25

**Recommended: bump `go.mod` to `go 1.26` (Path A).**

|                                      | Path A: `go 1.26`                                                                                     | Path B: stay on `go 1.25`                                                                                                                                         |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `go get -u` works                    | yes                                                                                                   | no, it aborts as a whole (**verified**). Direct deps must be pinned by hand (table in section 3)                                                                  |
| vulnerabilities left (`govulncheck`) | 1: GO-2026-5932, openpgp, no fix exists (**verified**)                                                | 9: five in `x/net` v0.58 (fixed in v0.60, needs 1.26), two ssh DoS in `x/crypto` v0.55 (fixed in v0.56, needs 1.26), one in otel sdk, plus openpgp (**verified**) |
| Go support                           | 1.26 is supported                                                                                     | 1.25 is out of support since Go 1.27 (2026-08)                                                                                                                    |
| files to touch                       | `go.mod`, `Dockerfile` (`GO_VERSION`, `GOPLS_VERSION`), `dev.nix`, `README.md` line 72, napkin recipe | none                                                                                                                                                              |
| Nix                                  | needs `pkgs.go_1_26` in the user's nixpkgs channel                                                    | works with the current channel (commit 463adda, 2026-07-18, chose 1.25 for this reason)                                                                           |

GO-2026-6610 ("HTTP/2 transport accepts malformed framing-related headers")
is a client-side bug. Our backends are HTTP clients. Path B leaves it open.

**Ask the user:** does your nixpkgs channel have `go_1_26`? If yes, Path A.
If no and the channel cannot move, Path B now and Path A later.

The rest of this plan is written for Path A. Section 3 lists the Path B pins.

## 2. What breaks, and the fix for each (all verified)

### 2.1 `backends/azure_backend.go`: three calls do not compile

`azure-storage-blob-go` v0.11.0 to v0.15.0 added arguments.

| line | call                           | add at the end                                                          |
| ---- | ------------------------------ | ----------------------------------------------------------------------- |
| 209  | `blobURL.StageBlock(...)`      | `azblob.ClientProvidedKeyOptions{}`                                     |
| 234  | `blobURL.CommitBlockList(...)` | `azblob.ClientProvidedKeyOptions{}, azblob.ImmutabilityPolicyOptions{}` |
| 257  | `blobURL.Download(...)`        | `azblob.ClientProvidedKeyOptions{}`                                     |

Line 209 then exceeds `lll` (140). Break the argument list over two lines.
Nothing else in the tree fails to compile (**verified** on both paths).

### 2.2 `make lint`: 17 new `SA1019` hits for `aws-sdk-go`

`aws-sdk-go` v1.55.8 marks every package `// Deprecated`. Add a rule to
`.golangci.yml` next to the openpgp one:

```yaml
# aws-sdk-go v1 is deprecated (end of support 2025-07-31). The v2 migration is its own spec.
- linters:
    - staticcheck
  text: 'SA1019: "github.com/aws/aws-sdk-go'
```

### 2.3 `klauspost/pgzip` v1.2.6 and v1.2.7 panic in `Reader.WriteTo`

`files.(*VolumeInfo).VerifyEnd` calls `io.Copy(io.Discard, v.r)` after a
`json.Decoder` has read part of the stream. `io.Copy` uses `pgzip.Reader.WriteTo`,
which panics with `slice bounds out of range [512:0]` when `Read` drained the
current block first. Upstream: klauspost/pgzip#74, fixed by #76, commit
`e1b5b318410c`, merged 2026-10-09, **not in any tag** (master is one commit
ahead of v1.2.7).

- `TestUnknownSignerErrorDoesNotAdviseTrust` (`files/manifest3_test.go`) fails
  on v1.2.6 and v1.2.7 and passes on v1.2.5 and on the fix commit (**verified**).
- Fix: `go get github.com/klauspost/pgzip@e1b5b318410c`. This gives
  `v1.2.8-0.20261009133145-e1b5b318410c`. A later `go get -u` moves to
  v1.2.8 when it is tagged. Note it in `go.mod` with a comment.

### 2.4 `pkg/sftp` v1.13.6 and newer: `TestSSHBackend/Close` hangs forever

Since v1.13.6, `sftp.Server.Serve` turns a clean `io.EOF` into `nil`
(`server.go`, "Check whether the connection terminated cleanly"). Our fake
sshd in `backends/ssh_backend_test.go` (`startSftpServer`) closes the
session only in its `err == io.EOF` branch, so it never closes the SSH
channel. The client's `sftpClient.Close()` then waits in `wg.Wait()` for a
read that never ends. v1.13.5 passes, v1.13.6 through v1.13.11 hang
(**verified** by bisection).

Fix in the test server, after `server.Serve()` returns:

```go
serveErr := server.Serve()
_ = server.Close()
_ = channel.Close() // what sshd does when the subsystem exits
if serveErr != nil && serveErr != io.EOF {
    t.Fatal("sftp server completed with error:", serveErr)
}
```

With this, `TestSSHBackend` passes three times in a row on v1.13.11 and the
whole `backends` package passes in 3.7 s (**verified**). Production is not
affected: OpenSSH closes the channel when `sftp-server` exits. An optional
check against a real sshd is in section 6.

### 2.5 `go get -u ./...` aborts with "ambiguous import"

`cloud.google.com/go/compute/metadata` moved to its own module. A plain
`go get -u -t ./...` stops with
`ambiguous import: found package cloud.google.com/go/compute/metadata in multiple modules`.
Run this first (**verified**):

```
go get cloud.google.com/go/storage@latest google.golang.org/api@latest cloud.google.com/go/compute@latest
```

### 2.6 `go get` rewrites the `go` line to three parts

`go get -u` writes `go 1.26.0`. The Makefile derives the image tag from that
line and would use `golang:1.26.0-bookworm`, which pins the first patch
release. Run `go mod edit -go=1.26` after every `go get`. The `mod-update`
target in section 4 does this.

## 3. Target versions

### Path A (Go 1.26), from `go get -u -t ./...` on 2026-10-11 (verified)

| module                                   | now                          | target                                             |
| ---------------------------------------- | ---------------------------- | -------------------------------------------------- |
| `cloud.google.com/go/storage`            | v1.27.0 (retracted upstream) | v1.69.0                                            |
| `github.com/Azure/azure-sdk-for-go`      | v66.0.0+incompatible         | v68.0.0+incompatible (last track-1 release)        |
| `github.com/Azure/azure-storage-blob-go` | v0.11.0                      | v0.15.0 (last release of this SDK)                 |
| `github.com/aws/aws-sdk-go`              | v1.44.104                    | v1.55.8 (last release, deprecated)                 |
| `github.com/cenkalti/backoff`            | v2.2.1+incompatible          | unchanged by `-u`; see step 5 for v4               |
| `github.com/dustin/go-humanize`          | v1.0.0                       | v1.1.0                                             |
| `github.com/juju/ratelimit`              | v1.0.2                       | unchanged (newest)                                 |
| `github.com/klauspost/pgzip`             | v1.2.5                       | v1.2.8-0.20261009133145-e1b5b318410c (section 2.3) |
| `github.com/kurin/blazer`                | v0.5.3                       | unchanged (newest, 2019)                           |
| `github.com/miolini/datacounter`         | v1.0.3                       | unchanged (newest)                                 |
| `github.com/nightlyone/lockfile`         | v1.0.0                       | unchanged (newest)                                 |
| `github.com/op/go-logging`               | 2016 pseudo-version          | unchanged (no release)                             |
| `github.com/pkg/errors`                  | v0.9.1                       | unchanged (frozen)                                 |
| `github.com/pkg/sftp`                    | v1.13.5                      | v1.13.11 (section 2.4)                             |
| `github.com/spf13/cobra`                 | v1.5.0                       | v1.10.2                                            |
| `github.com/spf13/pflag`                 | v1.0.5                       | v1.0.10                                            |
| `golang.org/x/crypto`                    | 2022-09 pseudo-version       | v0.58.0                                            |
| `golang.org/x/sync`                      | 2022-09 pseudo-version       | v0.24.0                                            |
| `golang.org/x/term`                      | 2022-09 pseudo-version       | v0.47.0                                            |
| `google.golang.org/api`                  | v0.97.0                      | v0.301.0                                           |

Indirect: `grpc` v1.49.0 to v1.84.0, `protobuf` v1.28.1 to v1.36.12,
`x/net` to v0.61.0, `x/text` v0.3.7 to v0.43.0, `go-autorest/adal` now pulls
`golang-jwt/jwt/v4` instead of `form3tech-oss/jwt-go`. OpenTelemetry comes in
through the GCS client.

`vendor/` grows from 32 MB and 1677 `.go` files to 52 MB and 2783 files
(**verified**). Mostly otel, grpc and genproto. Accept it: Nix and the Docker
test image build offline from `vendor/`.

### Path B (Go 1.25) pins, newest tagged versions that accept Go 1.25 (verified)

Pin these by hand. Everything else takes `@latest` as in Path A.

| module                        | pin      |
| ----------------------------- | -------- |
| `cloud.google.com/go/storage` | v1.68.0  |
| `google.golang.org/api`       | v0.298.0 |
| `golang.org/x/crypto`         | v0.55.0  |
| `golang.org/x/sync`           | v0.22.0  |
| `golang.org/x/term`           | v0.45.0  |

MVS then selects `x/net` v0.58.0, `x/oauth2` v0.36.0, `x/sys` v0.47.0,
`x/text` v0.41.0, `compute/metadata` v0.9.1, grpc v1.83.2, otel v1.44.0.
Do not run `go get -u ./...` on this path. Pin with `make mod-get MOD=...`.

## 4. New Makefile targets (step 1 below)

The host cannot reach `proxy.golang.org`; containers can. Mount a module
cache so the second run is fast.

```make
# Module maintenance in the pinned image (the host cannot reach proxy.golang.org).
MODCACHE_DIR?=/tmp/zfsb-modcache
GO_MOD_RUN = docker run --rm -v "$(CURDIR)":/src -w /src -v "$(GOCACHE_DIR)":/root/.cache/go-build \
	-v "$(MODCACHE_DIR)":/go/pkg/mod -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod $(GO_IMAGE)

# Upgrade every module, then keep the go line two-part so GO_IMAGE stays golang:X.Y-bookworm.
mod-update:
	$(GO_MOD_RUN) sh -c 'go get cloud.google.com/go/storage@latest google.golang.org/api@latest cloud.google.com/go/compute@latest \
		&& go get -u -t ./... && go mod tidy && go mod edit -go=$(GO_VERSION)'

# One module at a specific version, e.g. make mod-get MOD=github.com/klauspost/pgzip@e1b5b318410c
mod-get:
	@[ -n "$(MOD)" ] || { echo "mod-get: set MOD=path@version"; exit 1; }
	$(GO_MOD_RUN) sh -c 'go get $(MOD) && go mod tidy && go mod edit -go=$(GO_VERSION)'

# Rebuild vendor/ from go.mod and check the sums.
mod-vendor:
	$(GO_MOD_RUN) sh -c 'go mod tidy && go mod vendor && go mod verify'

# Known vulnerabilities reached by our code. Expected after this plan: GO-2026-5932 only.
vulncheck:
	$(GO_MOD_RUN) go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Replace the old `get` target (`go get -v -d -t ./...`; `-d` is obsolete)
with `mod-update`. Add the four targets to the table in
`docs/context/testing.md`.

`govulncheck@latest` needs Go 1.26. On Path B run `vulncheck` with
`GO_IMAGE=golang:1.26-bookworm make vulncheck`.

## 5. Steps

Each step is one commit that builds and passes `make test-docker`.
Another session edits this tree: run `git status` before staging and stage
by name, never `git add -A`.

**Step 0. Preconditions.**

- The user answers the Nix question (section 1).
- `docker pull golang:1.26-bookworm` (done once already on this host).
- `.claude/napkin.md` is already updated with the facts of section 2.

**Step 1. Makefile targets.** Section 4. Commit `Makefile`,
`docs/context/testing.md`. Check: `make vulncheck` on the unchanged tree
prints 26 vulnerabilities.

**Step 2. Toolchain bump (Path A only).**

- `go.mod`: `go 1.26` (two parts).
- `Dockerfile`: `ARG GO_VERSION=1.26`; `ARG GOPLS_VERSION=v0.23.0`
  (newest gopls, needs 1.26; v0.21.1 is the current pin).
- `dev.nix`: `go ? pkgs.go_1_26 or pkgs.go` and the comment above it.
- `README.md` line 72: "Go 1.26 or newer".
- `.claude/napkin.md`: the proxy recipe says `golang:1.25-bookworm`; make it 1.26.
- Check: `make test-docker` passes with the old modules on the new toolchain.
  `make devcontainer-build` builds (gopls install).
- Commit by name: `go.mod go.sum Dockerfile dev.nix README.md docs/context/testing.md .claude/napkin.md`.

**Step 3. Module upgrade plus the compile and lint fixes.**

- `make mod-update` (Path A) or the `mod-get` pins of section 3 (Path B).
- `make mod-get MOD=github.com/klauspost/pgzip@e1b5b318410c` and a `go.mod`
  comment: `// klauspost/pgzip#74 fix, not yet tagged; move to v1.2.8 when released`.
- `make mod-vendor`.
- Azure fix (2.1), lint rule (2.2), sftp test-server fix (2.4).
- `make fmt-write FILES=backends/azure_backend.go backends/ssh_backend_test.go`.
- Checks: `make test-docker`, `make lint`, `make fmt-check`,
  `make vulncheck` (expect 1 finding), `make e2e`.
- Commit by name: `go.mod go.sum vendor backends/azure_backend.go
backends/ssh_backend_test.go .golangci.yml`. `vendor` is one path name;
  that is allowed.

**Step 4. Help text and goldens.**

- cobra 1.5 to 1.10 may change help wording. For each command run
  `make help CMD=<cmd>` and diff against the README sections (the README
  was recaptured in the third pass). Update the README where it differs.
- `make scenarios` (golden plan output). `go-humanize` v1.1.0 may format
  sizes differently. The scratch run passed, so expect no diff.
- Commit `README.md` and any golden only if they changed.

**Step 5. Small cleanups, one commit each, optional.**

- `cenkalti/backoff` v2 to `github.com/cenkalti/backoff/v4` v4.3.0: only the
  import path changes in `backup/backup.go`, `backup/clean.go`,
  `backup/restore.go`. `NewExponentialBackOff`, `WithContext`, `Retry`,
  `Permanent` keep their signatures in v4. Do not take v5 (new API).
  `make mod-get MOD=github.com/cenkalti/backoff/v4@v4.3.0`, then `make mod-vendor`
  drops v2.
- `backends/ssh_backend_test.go`: `golang.org/x/crypto/ed25519` is a
  deprecated alias of `crypto/ed25519`. Swap the import.
- `backends/azure_backend.go`: `pkg/errors` is used for `errors.New` and
  four `errors.Wrap`. `fmt.Errorf("...: %w", err)` replaces them; then
  `make mod-vendor` drops the module.

**Step 6. Record.** Append the output of the success check (`intent.md`)
and the final `go.mod` direct block to this file under "Execution notes".
CHANGELOG entries:

```
* [CHANGE] deps: every Go module upgraded (2022 -> 2026); govulncheck goes from 26 reachable vulnerabilities to 1 (x/crypto/openpgp, replaced by the pgp-ed25519 spec); Go 1.26
* [FIX] test: the fake sshd closes its channel after the sftp session ends (pkg/sftp >= 1.13.6 no longer returns io.EOF from Serve)
* [ADD] dev: make mod-update, mod-get, mod-vendor, vulncheck run module maintenance in the pinned image
```

## 6. Extra verification, optional

The emulator tests self-skip without an endpoint. Azure is the only backend
with an API-level change, so run at least Azurite. Add a Makefile target if
you use one (`make test-emulators`), per the repo rule.

| backend                    | emulator                                                                                                 | env for the test                                                                                                                                  |
| -------------------------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Azure (`TestAzureBackend`) | `docker run --rm -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0` | `AZURE_CUSTOM_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1` (the test sets the emulator account name and key itself)                          |
| GCS (`TestGCSBackend`)     | `docker run --rm -p 4443:4443 fsouza/fake-gcs-server -scheme https`                                      | `GCS_FAKE_SERVER=https://127.0.0.1:4443`                                                                                                          |
| S3 (`TestS3Backend`)       | `docker run --rm -p 9000:9000 minio/minio server /data`                                                  | `AWS_S3_CUSTOM_ENDPOINT=http://127.0.0.1:9000 AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1`                 |
| SSH, real sshd             | `docker run --rm -p 2222:22 -e SFTP_USERS=test:password:1001 atmoz/sftp`                                 | point `ssh://test@127.0.0.1:2222/upload` at it; run a `send` to a `file://`+`ssh://` pair and confirm the process exits (the `Close` path of 2.4) |

Run the emulator containers on the host network of the test container, or
run `make test-run RUN=TestAzureBackend PKG=./backends/` on the host.

## 7. Risks

- **Repository size.** `vendor/` adds about 20 MB and 1100 files in one commit.
- **Behavior changes without a test.** `azblob` v0.15 (section 6 covers it),
  `aws-sdk-go` v1.55 (the `httptest` fakes in `backends/` cover list, upload,
  restore paths), cobra help text (step 4).
- **Host time zone.** Two tests fail on the host at HEAD (napkin). Use the
  Docker targets for the final check.
- **Unreleased pgzip commit.** If upstream rewrites history the pseudo-version
  stays valid through the module proxy and `go.sum`. If the user prefers only
  tags, pin v1.2.5 instead; the test in 2.3 guards both choices.
- **Dependabot.** `.github/dependabot.yml` opens gomod PRs daily but does not
  update `vendor/`. Add `vendor: true` under the gomod entry so its PRs
  re-vendor, or those PRs will not build.

## 8. Follow-up specs, not in this plan

1. `aws-sdk-go` v1 to v2 (deprecated, end of support 2025-07-31).
2. `azure-storage-blob-go` to `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`.
3. `x/crypto/openpgp` to ProtonMail: `docs/specs/2026-09-29--pgp-ed25519/`.
   It must re-vendor on top of this change.
4. `kurin/blazer` (2019) and `op/go-logging` (2016) have no releases; decide
   whether to keep, fork, or replace.

## Execution notes

Executed 2026-10-11 on `clean-dry-run`, Path A (the user's nixpkgs has `go_1_26`).

Commits: 0671ffd (step 1), 6229ae9 (step 2), 9d625b0 (fix of 2.6, below),
0b4956d (step 3), 94e8274 and 086b3b8 (step 5), 79a7302 (flaky test, below),
c290b77 (dependabot, section 7). Step 4 had no diff: `--help` of all six
commands matches the README, and `make scenarios` passes with no golden change.
The CHANGELOG lines landed in 1940208 (another session staged them).

Changes from the plan:

- **2.6 was wrong.** `go mod edit -go=1.26` does not hold: the next
  `go mod tidy` writes `go 1.26.0` again, because the new deps declare
  `go 1.26.0` and `1.26` sorts lower. Fix: the Makefile takes only `X.Y` of
  the `go` line for `GO_VERSION`. `go.mod` now says `go 1.26.0`; the targets
  no longer run `go mod edit`.
- **`integration/compose.yaml`** also had a `1.25` default; bumped in step 2.
- **Step 5, backoff v4: skipped.** It is not only an import path change. On a
  canceled context v4's `Retry` returns `ctx.Err()`; v2 returns the last
  operation error. After SIGTERM or a sibling failure the log would say
  "context canceled" in place of the real error. v2 has no known
  vulnerability. Do it later with a test for the error that a canceled
  retry returns.
- **Flaky test found, older than this work.** `TestManifestCostCountsHigh`
  failed 27/300 on 74b50b1 (Go 1.25) and 23/300 after the upgrade.
  `manifestCost` counted `time.Now()`, whose JSON drops trailing zeros.
  It now counts the longest time (nine digits and an offset): 0/300.
- Step 1 check: `vulncheck` on the old tree needs
  `GO_IMAGE=golang:1.26-bookworm` (govulncheck@latest needs 1.26). 26 found.
- One `mod-vendor` run read a cut go.mod right after `sed -i`
  ("unterminated block"); the rerun passed.
- One Docker e2e run stalled in `TestE2EResumeCompletesManifestAtOneDestination`
  for 10+ min. Not reproduced: the test alone and two full runs pass.

Success check (intent.md), final tree:

```
make test-docker   all packages ok (root 101 s, backup 74 s, backends 3.7 s)
make lint          0 issues.
make fmt-check     clean
make vulncheck     Vulnerability #1: GO-2026-5932 (only one)
make e2e           host: TestE2EPlanManifestsFileAdoptsCreationTimes fails (EDT, known);
                   Docker (make test-one RUN=TestE2E PKG=.): ok 98 s
make test-race     run in Docker (UTC) as make test-one TESTFLAGS="-race -timeout 45m":
                   all packages ok, root 1707 s, no DATA RACE
```

Final `go.mod` direct block:

```
go 1.26.0

require (
	cloud.google.com/go/storage v1.69.0
	github.com/Azure/azure-sdk-for-go v68.0.0+incompatible
	github.com/Azure/azure-storage-blob-go v0.15.0
	github.com/aws/aws-sdk-go v1.55.8
	github.com/cenkalti/backoff v2.2.1+incompatible
	github.com/dustin/go-humanize v1.1.0
	github.com/juju/ratelimit v1.0.2
	github.com/klauspost/pgzip v1.2.8-0.20261009133145-e1b5b318410c // klauspost/pgzip#74 fix, not yet tagged; move to v1.2.8 when released
	github.com/kurin/blazer v0.5.3
	github.com/miolini/datacounter v1.0.3
	github.com/nightlyone/lockfile v1.0.0
	github.com/op/go-logging v0.0.0-20160315200505-970db520ece7
	github.com/pkg/sftp v1.13.11
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	golang.org/x/crypto v0.58.0
	golang.org/x/sync v0.24.0
	golang.org/x/term v0.47.0
	google.golang.org/api v0.301.0
)
```

Not done: section 6 (emulators; optional). Azure has an API change and no
Azurite run yet.
