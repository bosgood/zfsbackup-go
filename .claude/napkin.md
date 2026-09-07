# Napkin

## Corrections
| Date | Source | What Went Wrong | What To Do Instead |
|------|--------|----------------|-------------------|
| 2026-06-30 | user | Ran `go build`/`go vet`/`go test` directly on the host | Prefer the Makefile targets for builds/tests/lint. They encode the supported (docker-based) toolchain. Build & run tests via `make test-docker` (builds the image, runs `go test ./...` inside). |
| 2026-06-30 | user | — | When a needed operation has no Makefile target, ADD/UPDATE the target rather than running ad-hoc commands, so the workflow stays captured in the Makefile. |

## User Preferences
- Use Makefile targets for repeatable operations (test, build, lint). Keep the Makefile as the source of truth; extend it when a task is missing.
- Before implementing in plan mode, the user may ask to persist intent + plan under `docs/specs/<YYYY-MM-DD>--<slug>/{intent.md,plan.md}`.

## Patterns That Work
- `make test-docker` = `docker build -t zfsbackup-test . && docker run --rm zfsbackup-test`; the Dockerfile runs `go build ./...` + tests inside golang:1.25-bookworm. Use this for verifying builds/tests.
- To run a single test against the supported toolchain without a full image rebuild:
  `make test-one RUN=TestName PKG=./pkg/`. Keep the Dockerfile image and go.mod's `go` directive in sync:
  official golang images set `GOTOOLCHAIN=local`, so an older image fails at `go mod download`.
- `Clean` (backup/clean.go) builds its backend from the destination URI via `backends.GetBackendForURI`, so backends can't be mock-injected. Use the real `file://` `FileBackend` against a temp dir for end-to-end tests (see backup/clean_test.go); set `config.WorkingDir` to a temp cache dir.

- `dev.nix` (repo root) builds/installs from a pinned git rev via `fetchFromGitHub` + `buildGoModule`; `vendorHash = null` because `vendor/` is checked in. Bump `rev`, set `hash` to `lib.fakeHash`, rebuild, paste the "got:" hash.

## Patterns That Don't Work
- (RESOLVED 2026-09-05) The go.mod `go 1.25` vs Dockerfile `golang:1.23` mismatch is fixed in the working tree;
  the Dockerfile now pins `golang:1.25-bookworm`. Keep go.mod and the Dockerfile in sync when either moves.
- Nix is NOT installed on this host (`nix`, `nix-prefetch-url` missing), so nix expressions here can't be evaluated/verified locally — the user has to run them.
- `golangci/golangci-lint:latest` rejects this repo's `.golangci.yml` (v1 config, needs `version:` for v2).
- The repo's pinned `golangci-lint v1.24.0` (per .travis.yml) cannot run under the current Go 1.23 toolchain — fails importing packages (`io.ReadAll not declared`, `debug.BuildInfo has no field Settings`). Lint via the old pinned version is effectively broken locally; rely on `go build`/tests instead, or a CI-matched setup.

## Domain Notes
- Module path is `github.com/someone1/zfsbackup-go` (not bosgood/...). Working dir is the bosgood fork.
- Flags bind either to fields on the shared global `jobInfo` (files.JobInfo, e.g. `Force`) or to standalone `cmd`-package vars (e.g. `cleanLocal`, `cleanDryRun`). Use standalone vars for command-specific flags.
- Logging: `log.AppLogger` (op/go-logging). Default level is Notice — `Debugf` is hidden by default, so user-facing previews (e.g. dry-run output) must use `Noticef`.
- Test code in this repo still uses `io/ioutil` (e.g. backup_test.go); match it for consistency within a package even though gopls flags it deprecated.
- ZFS integration tests are gated behind `//go:build integration` (`make integration`); not run by default `make test`.
- Smart-backup snapshot selection lives in `backup/backup.go`. The decision core is `selectSmartSnapshots(jobInfo, snapshots, destBackups)` — a PURE function (no ZFS/backend I/O; existence checks use `validateSnapShotExistsFromSnaps` against the passed-in snapshot list). `ProcessSmartOptions` just fetches snapshots + per-destination manifests and delegates to it. Unit-test the pure function directly (see `TestSelectSmartSnapshots`); no docker/zfs needed.
- `--fullSnapshotSuffix` / `--incrementalSnapshotSuffix` (JobInfo.FullSnapshotSuffix / IncrementalSnapshotSuffix): with sanoid-style names (`autosnap_<date>_daily|_monthly`, type is a SUFFIX), anchor fulls on `_monthly` (survives the fullIfOlderThan window) and incrementals on `_daily`. Without them, behavior = historical "newest matching prefix for everything".
- Prior `fullIfOlderThan` bug: incremental path validated the last FULL's base snapshot (`lastComparableSnapshots[0]`, the oldest/first-pruned link) instead of the actual incremental SOURCE (`lastBackup[0]`). When local snapshot retention < the full window, the full's base got pruned and every run forced a spurious full. Fixed to validate `lastBackup[0]`; a pruned source now falls back to a full from the full-candidate.
- Backup `--dry-run`/`-n` (standalone `sendDryRun` var, passed as 3rd arg to `backup.Backup(ctx, jobInfo, dryRun)` — mirrors `Clean`'s dryRun param). Short-circuits to `reportDryRun` BEFORE the lock/pipeline: validates base+incremental snapshots exist (read-only), then Noticef-logs backup type, snapshots, destinations, the `zfs send` command line, and a best-effort size estimate. Size estimate = `zfs.GetZFSSendDryRun` which runs `zfs send -n -P` and parses the `size\t<bytes>` line. The snapshot decision is already computed in PreRunE (`ProcessSmartOptions`) so dry-run just reports it.

- Go 1.26.5 IS installed on the host (`/opt/homebrew/bin/go`). Use it for THROWAWAY checks in /tmp only
  (e.g. verifying a regexp's behavior). Repo builds/tests still go through the Makefile / docker.
- A `fakeZFS(t, listing)` helper already exists in `backup/backup_test.go`. It writes a `/bin/sh` stub, points
  `zfs.ZFSPath` at it, and answers only `zfs list` (canned listing) and `zfs send` (canned `size\t123456`).
  It is the seam for ZFS-free tests; extend it rather than inventing a second stub.
- `zfs/zfs_test.go` exists but is UNTRACKED. Covers `parseSendSizeEstimate` + `sendDryRunArgs` only.
- No `testdata/` directory anywhere in the repo -- there is no golden-manifest corpus yet.
- `JobInfo.Version` is WRITTEN by cmd/send.go but never read or validated by `readManifest`. Old/new manifest
  formats are accepted silently.
- `ValidateSendFlags` is send-only. The receive path does NOT validate `Separator`.

## Known Defects (found 2026-09-05, not yet fixed)
- `files/jobinfo.go:25` `disallowedSeps = ^[\w\-:\.]+` is the wrong shape. `+` needs >=1 char, so an EMPTY
  separator passes validation. `--separator=""` then makes `ManifestObjectName`/`BackupVolumeObjectName` collide:
  incremental a->b and a full of a snapshot named `atob` both give `tank/dataatob`. The second backup silently
  overwrites the first at the destination. The pattern is also unanchored at the end, so `"|_x"` passes while
  containing ZFS-legal chars. Correct check: unanchored `[\w\-:.]` (any occurrence) PLUS a non-empty check.
- `backup/restore.go:497` returns `err` where it must return `eerr`. `err` is the outer `cmd.Start()` error and is
  nil there, so a `vol.Extract` failure (bad decompress/decrypt) returns SUCCESS from the extract goroutine.
