# Napkin

## Corrections
| Date | Source | What Went Wrong | What To Do Instead |
|------|--------|----------------|-------------------|
| 2026-06-30 | user | Ran `go build`/`go vet`/`go test` directly on the host | Prefer the Makefile targets for builds/tests/lint. They encode the supported (docker-based) toolchain. Build & run tests via `make test-docker` (builds the image, runs `go test ./...` inside). |
| 2026-06-30 | user | — | When a needed operation has no Makefile target, ADD/UPDATE the target rather than running ad-hoc commands, so the workflow stays captured in the Makefile. |
| 2026-09-07 | self | Verified a 7-commit stack with ad-hoc `docker run ... go build ./...` per commit, instead of adding a Makefile target for it. | The rule above applies to VERIFICATION too, not just test/build. Add a target (e.g. `make verify-stack BASE=<rev>`) before reaching for a raw docker command. |

## User Preferences
- Use Makefile targets for repeatable operations (test, build, lint). Keep the Makefile as the source of truth; extend it when a task is missing.
- Before implementing in plan mode, the user may ask to persist intent + plan under `docs/specs/<YYYY-MM-DD>--<slug>/{intent.md,plan.md}`.

## Patterns That Work
- `make test-docker` = `docker build -t zfsbackup-test . && docker run --rm zfsbackup-test`; the Dockerfile runs `go build ./...` + tests inside golang:1.25-bookworm. Use this for verifying builds/tests.
- To run a single test against the supported toolchain without a full image rebuild:
  `make test-one RUN=TestName PKG=./pkg/`. Keep the Dockerfile image and go.mod's `go` directive in sync:
  official golang images set `GOTOOLCHAIN=local`, so an older image fails at `go mod download`.
- `Clean` (backup/clean.go) builds its backend from the destination URI via `backends.GetBackendForURI`, so backends can't be mock-injected. Use the real `file://` `FileBackend` against a temp dir for end-to-end tests (see backup/clean_test.go); set `config.WorkingDir` to a temp cache dir.

- To prove a regression test actually catches its defect: copy the fixed file to /tmp, re-introduce the old code
  with a python string replace, run the test expecting FAILURE, then copy the fixed file back. Do this every time
  a test is added for a bug -- a test that passes both before and after the fix proves nothing.
- `receiveStream` is testable without ZFS: pass `exec.Command("cat")` as the receive command. It drains stdin and
  exits 0, so the only way the function can report a problem is by propagating a real error.
- Quick full-suite run without an image rebuild:
  `docker run --rm -v "$PWD":/src -w /src golang:1.25-bookworm go test ./...` (vendor/ is checked in, so offline).
  Takes ~2 min; backends + backup are ~60s each. Finish with `make test-docker` for the sanctioned path.

- Splitting one big staged change into topic commits WITHOUT `git add -p` (interactive flags are
  unavailable here): save `git diff --cached > /tmp/staged.patch` first, `git reset`, then rebuild each
  intermediate file state with a small line-range splice script (HEAD blob + final blob + (head_start,
  head_end, final_start, final_end) tuples, applied bottom-up so line numbers stay valid). Self-check by
  applying ALL splices and diffing against the final file -- if that is identical, every range is right.
  Finish with `git diff <base> HEAD` vs the saved staged.patch; they must be byte-identical.
- To check every commit in a stack builds, `git archive <commit> | tar x -C <dir>` each one, then run
  `go build ./...` + `go vet ./...` over all of them in ONE container. `go vet` also compiles _test.go
  files, so it catches a test that references a helper landing in a later commit. Delete the checkouts
  after; they are ~30MB each because `vendor/` is checked in.

- `make fmt-check` runs `gofmt -s -l` in the pinned image and fails on any file it would rewrite.
  The pre-existing `fmt` target loops over an UNDEFINED `DIRS` variable, so it checks nothing -- do not
  trust it.
- To assert on user-facing log output in a test, use `captureLogs(t)` in `backup/backup_test.go`. It swaps
  `log.AppLogger`'s backend for a `bytes.Buffer` via `logging.MultiLogger(logging.NewLogBackend(buf, "", 0))`
  and restores stderr on cleanup. A fresh leveled backend defaults to DEBUG, so every level is captured
  (module levels set by `logging.SetLevel` live on the DEFAULT backend and do not apply).

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
- `--snapshotPrefix` (JobInfo.SnapshotPrefix, upstream commit ec89e86) is applied in EXACTLY ONE place:
  `newestMatchingSnapshot` in backup/backup.go, via `snapshotMatches(s, prefix, suffix)`. It picks the BASE
  snapshot only. It does NOT scope `getBackupsForTarget` (manifests filter on VolumeName alone), so the
  incremental SOURCE (`lastBackup[0]`/`lastComparableSnapshots[0]`) can be a non-prefixed snapshot from a
  different job writing to the same volume+destination. Verified empirically 2026-09-07.
  Also: `SnapshotPrefix` is serialized into the manifest but never read back; `usingSmartOption()` ignores it
  (prefix is silently a no-op on the explicit `vol@snap` path); `ResetSendJobInfo` does NOT reset it (leaks
  across integration-test runs, unlike Full/Incremental/*SnapshotSuffix); and there is ZERO test coverage
  (`grep SnapshotPrefix **/*_test.go` -> nothing).

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
- This repo can be edited by ANOTHER session while you work. On 2026-09-07 `backup/restore.go`,
  `files/jobinfo.go` and two new _test.go files appeared mid-task. Re-run `git status` right before
  staging, and stage files BY NAME -- never `git add -A`/`git add .`.

## Resolved Defects
- FIXED 2026-09-07: `files/jobinfo.go` `disallowedSeps` was `^[\w\-:\.]+` -- unanchored at the end and `+` needs
  >=1 char, so an EMPTY separator passed validation and `--separator=""` made object names collide (incremental
  a->b and a full of `atob` both give `tank/dataatob`; the second silently overwrote the first). Now
  `^[\w\-:./]*$`: reject a separator ONLY when every character is legal in a ZFS dataset path (or it is empty).
  A separator is safe when >=1 char cannot occur in a ZFS path, because no snapshot name can absorb it. This
  also stopped over-rejecting `"a|"` and started rejecting `"/"` (which collides ACROSS volumes:
  join(["tank/data","a","to","b"],"/") == join(["tank/data/a/to","b"],"/")). Test: `files/jobinfo_test.go`.
- FIXED 2026-09-07: `backup/restore.go` receiveStream returned `err` (the nil outer `cmd.Start()` error) instead
  of `eerr` after a failed `vol.Extract`, so a bad decompress/decrypt reported a SUCCESSFUL restore of a
  truncated stream. Test: `backup/restore_test.go` `TestReceiveStreamReportsExtractError`.

## Branch: `origin/clean-dry-run2` (compared 2026-09-07)
It is ONE squashed commit (1383460 "previous attempt") off the same merge-base (5702819). It is an EARLIER
attempt, not a newer one. `clean-dry-run` is ahead on everything else (redactURI/joinURI, separator regex,
receiveStream `eerr`, zfs helper split, `make test-one`, cbbf0e8 full-skip, 36c4c51 recovery-base anchor).
Only `backup/backup.go` holds anything we lack:
- DONE 53eb732+: full is due (`fullIfOlderThan` exceeded) but `fullBase == nil` -> our branch (backup.go:200-202)
  silently keeps doing incrementals FOREVER. Reachable when a full already exists at the destination and the
  user later sets a wrong `--fullSnapshotSuffix`. cdr2 returns a wrapped error instead.
- DONE 53eb732+: full is due but the newest full candidate is already backed up -> our branch is silent.
  cdr2 logs a Noticef that the `fullIfOlderThan` guarantee is not being met.
- JUDGMENT CALL, NOT A BUG: cdr2 adds a pruned-source fallback-to-full on the EXPLICIT `--incremental` path
  (backup.go:168-180). Today that case already fails cleanly in `Backup` ("selected incremental snapshot does
  not exist", backup.go:402-409). Auto-promoting to a full can upload TBs the user did not ask for.
- DO NOT TAKE: cdr2's `fallbackToFull` helper anchors only on `fullBase`. That REGRESSES 36c4c51 (a recovery
  full on a snapshot the destination already holds overwrites that backup in place, no forward progress).
- OPTIONAL: `ErrNoFullCandidate` / `ErrNoIncrementalCandidate` sentinels (enables `errors.Is`).

## Open Defects — `send --dry-run` audit (2026-09-07)
Ranked. None fixed yet; feature otherwise works and unit tests pass.
1. HIGH: dry-run is NOT destination-read-only. PreRunE (`cmd/send.go:316`) runs `ProcessSmartOptions` ->
   `getBackupsForTarget` -> `syncCache` -> `backend.PreDownload` (`backup/sync.go:140`). The S3 backend's
   PreDownload issues `RestoreObject` for GLACIER manifests (3-day, billable) and then BLOCKS for hours.
   Only on smart paths that read the destination; `--full` skips it (commit cbbf0e8).
2. MED: dry-run never calls `prepareBackend` (real path only, `backup/backup.go:468`), so it never validates
   destination credentials/write access. `--full` + explicit `vol@snap` dry-runs touch no backend at all.
3. MED: `--jsonOutput` prints NOTHING for a dry run. Real send writes JSON to stdout (`backup/backup.go:549`);
   dry-run returns at `:355` and only logs (go-logging default backend = stderr).
4. MED: misleading error. `validateSnapShotExists` (`backup/sync.go:162`) returns `(false, nil)` when
   `zfs list` fails, so "Selected base snapshot does not exist!" also fires for missing zfs / bad dataset.
   The `verr != nil` branches in `reportDryRun` are dead code.
5. LOW: `--resume` is ignored. `Backup` returns at `:355` before `tryResume` (`:359`), so the preview can
   differ from the resumed run (different base snapshot, skipped volumes). No warning is printed.
6. LOW: README's captured `send --help` block (README.md:187-214) has no `-n, --dry-run`. Same for `clean`.
7. NIT: `GetZFSSendCommand` logs "Enabling the X flag" at Info; reportDryRun (`:335`) and `GetZFSSendDryRun`
   (`:340`) each call it -> every line twice at `--logLevel info`.
8. GAP: no integration-test coverage for dry-run; no test asserting no backend is initialized.
