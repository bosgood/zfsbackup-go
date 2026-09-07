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
- `make test-docker` = `docker build -t zfsbackup-test . && docker run --rm zfsbackup-test`; the Dockerfile runs `go build ./...` + tests inside golang:1.23-bookworm. Use this for verifying builds/tests.
- To run a single test against the supported toolchain without a full image rebuild:
  `docker run --rm -v "$PWD":/src -w /src golang:1.23-bookworm go test -run TestName -v ./pkg/`
- `Clean` (backup/clean.go) builds its backend from the destination URI via `backends.GetBackendForURI`, so backends can't be mock-injected. Use the real `file://` `FileBackend` against a temp dir for end-to-end tests (see backup/clean_test.go); set `config.WorkingDir` to a temp cache dir.

- Go version is pinned in THREE places: `go.mod`, `Dockerfile` (`FROM golang:<v>-bookworm`), and `dev.nix`. Bumping `go.mod` alone breaks `make test-docker` with "go.mod requires go >= X (running go Y; GOTOOLCHAIN=local)". Bump all three together.
- Auditing pure functions by writing a throwaway `backup/zz_audit_test.go` with `t.Logf` assertions (no `t.Fatal`), running it in docker, then deleting it — cheap way to CONFIRM suspected defects instead of asserting them from code reading. Confirmed 4/4 suspicions this way.
- `dev.nix` (repo root) builds/installs from a pinned git rev via `fetchFromGitHub` + `buildGoModule`; `vendorHash = null` because `vendor/` is checked in. Bump `rev`, set `hash` to `lib.fakeHash`, rebuild, paste the "got:" hash.

## Patterns That Don't Work
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
- Restore handles BRANCHED backup chains. `linkManifests` (list.go:168) builds a parent-pointer TREE, not a linear list: manifests are indexed by md5(volume+BaseSnapshot.Name+CreationTime), and each incremental links to whoever PRODUCED its `IncrementalSnapshot`. `AutoRestore` walks `ParentSnap` until it hits a full. So an incremental taken from an older snapshot (e.g. the last full's base) is fully restorable and skips the intervening leg. Verified in backup/restore_test.go.
- Manifest object name = `volume|base` for fulls, `volume|incr|to|base` for incrementals (`volumeNameParts`, jobinfo.go:237). Two fulls of the SAME snapshot therefore share an object name and the second OVERWRITES the first — duplicate fulls don't accumulate, they're just a wasted upload.
- `selectSmartSnapshots` error sentinels: `ErrNoOp`, `ErrNoFullCandidate`, `ErrNoIncrementalCandidate`. The test runner uses `errors.Is`, so new error cases must be sentinel-based (or wrap one with `%w`) — plain `fmt.Errorf` can't be asserted.
- When a full is DUE (`fullIfOlderThan` elapsed) the three outcomes are: no full candidate at all -> error wrapping `ErrNoFullCandidate` (catches a typo'd `--fullSnapshotSuffix`); a newer candidate exists -> roll onto it; candidate == the one already backed up -> `Noticef` and defer, continuing incrementally. Never fall through silently — an unbounded incremental chain defeats the whole point of the flag.
- Both the `--increment` path and the `fullIfOlderThan` path validate the incremental source still exists locally and degrade via the shared `fallbackToFull` helper. Keep them symmetric.
- Prior `fullIfOlderThan` bug: incremental path validated the last FULL's base snapshot (`lastComparableSnapshots[0]`, the oldest/first-pruned link) instead of the actual incremental SOURCE (`lastBackup[0]`). When local snapshot retention < the full window, the full's base got pruned and every run forced a spurious full. Fixed to validate `lastBackup[0]`; a pruned source now falls back to a full from the full-candidate.
- Bookmarks are a first-class incremental SOURCE. `selectSmartSnapshots` validates `lastBackup[0]` with `includeBookmarks=true` (backup.go:222); `validateSnapShotExistsFromSnaps` sets `snapshot.Bookmark` on match (sync.go:155); `zfs.go:150` then emits `-i <vol>#<name>`. `Equal` = name + creation time (jobinfo.go:108), and a ZFS bookmark inherits both from its snapshot, so a bookmark transparently replaces a pruned snapshot. => For sanoid setups with short daily retention, bookmarking each sent daily prevents spurious fulls. Caveat: `--intermediary` emits `-I`, which does NOT accept bookmarks (only `-i` does).
- `--snapshotPrefix` is a plain `strings.HasPrefix` (backup.go:82). With sanoid names (`autosnap_<date>_<type>`) it CANNOT distinguish hourly/daily/monthly — that's a suffix. Use the suffix flags instead.
- Backup `--dry-run`/`-n` (standalone `sendDryRun` var, passed as 3rd arg to `backup.Backup(ctx, jobInfo, dryRun)` — mirrors `Clean`'s dryRun param). Short-circuits to `reportDryRun` BEFORE the lock/pipeline: validates base+incremental snapshots exist (read-only), then Noticef-logs backup type, snapshots, destinations, the `zfs send` command line, and a best-effort size estimate. Size estimate = `zfs.GetZFSSendDryRun` which runs `zfs send -n -P` and parses the `size\t<bytes>` line. The snapshot decision is already computed in PreRunE (`ProcessSmartOptions`) so dry-run just reports it.
