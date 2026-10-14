# Napkin

Rules learned the hard way. Keep this file under 15 KB, one or two lines per entry.
Project history, review facts and defect lists go in `docs/specs/`, not here.
Old entries: `docs/specs/2026-10-11--napkin-archive/` (`napkin-before-2026-10-11.md`, `napkin-trimmed-2026-10-14.md`).

## Bash tool and host
- Call #1 of a session is `cat .claude/napkin.md` ALONE. No chain, no separator. (Broken 13 times by chaining, last 2026-10-14.)
- zsh does NOT word-split `$var`: `for t in "test-one X=1" vet; do make $t` passed one bad arg and ran HOST tests (2026-10-13). Write each make call out.
- zsh expands a leading `=word`: `echo ======` aborts the whole chain (again 2026-10-14, 3rd time). Use `echo '-----'`. Quote globs too: `--include='*.go'`.
- `grep` here is ugrep: it ignores `--` as end of options. Use `/usr/bin/grep` or `-e PATTERN` when it matters.
- macOS has no `timeout`. Put it inside the container command.
- `cd dir && ...` persists across calls; parallel calls interleave. Use absolute paths or `(cd dir && ...)`.
- Foreground `sleep N; cmd` is blocked. Wait with `until <check>; do sleep 3; done`.
- Never gate a commit on a piped test run (`make ... | grep && git commit`): the status is grep's. Run them as separate calls.
- Host zone is EDT. `TestScenarioAdoptCreationTimes` and `TestE2EPlanManifestsFileAdoptsCreationTimes` fail on the host at HEAD too. Confirm with `make test-one` (Docker, UTC).
- Host Go 1.26 is for throwaway checks in /tmp only. Nix is not installed.
- Host load varies a lot (e2e 7 s -> 43 s, no code change). Compare against the old tree in the same minute.
- Docker here is OrbStack (arm64). Its kernel has NO zfs module and no `/lib/modules`; the maintainer will not ship one. Real `zfs` runs in the NixOS Lima VM: `make integration-vm` (see `integration/README.md`).
- A privileged container sees the HOST's zpools. A test entrypoint must refuse an existing pool name and destroy only the pool it made.
- Two sessions share ONE compose project in the VM (`zfsbackup-integration`): a second `make integration-vm` recreates the running zfs container (exit 143), both logs interleave, and `down -v` kills the other run. Check `limactl shell zfsbackup -- docker ps` first (bitten 2026-10-12). `docker ps` empty is not enough: a run is invisible while its image builds. Ask the busy zfsbackup sessions first (ListAgents, SendMessage) and wait for their "done" (worked 2026-10-12).
- The Lima VM sees only `~`, read-only. Scratch copies for VM runs go under `~/.cache/`, then `make -C <copy> integration-vm`.
- A VM run can stall when the Mac sleeps (compile 12 s -> 34 min) and DNS can fail right after. Rerun before you debug.

## Build, test, lint
- Makefile only, also for VERIFICATION. No target? Add one. Targets: `docs/context/testing.md`. (Broke it with an ad-hoc `go vet -tags integration` 2026-10-12, and again with `go test` in a /tmp scratch copy the same day; `make vet`, `make -C /tmp/copy test-run`.)
- `make test-run` = fast host loop. `make test-one` = Docker, ~10 s (mounts `/tmp/zfsb-gocache`). `make test-race` needs ~20 min (timeout 45m).
- `make fmt-check` sees TRACKED files only: a new file is not checked until `git add`. Fix with `make fmt-write FILES=...` (pinned image). Trust fmt-check, not a list.
- `make lint` runs a pinned golangci-lint v2 image. Bump the tag on purpose; a new version adds issues.
- `make test-docker` copies the build context in the first ~5 s; after that you can edit the tree.
- Keep the Dockerfile `GO_VERSION` default in sync with go.mod's `go` line (images set `GOTOOLCHAIN=local`).
- Module work: `make mod-update`, `mod-get MOD=p@v`, `mod-vendor`, `vulncheck` (pinned image, cache in /tmp/zfsb-modcache).
- `go test` without `-v` hides stdout of passing tests.
- Break proofs in `~/.cache` copies fill the VM disk with build cache (18 GB after 15); MinIO then fails with `XMinioStorageFull`. Run `limactl shell zfsbackup -- docker builder prune -f` after them. Ordinary runs fill it too (15 GB after one day, 2026-10-13): check `limactl shell zfsbackup -- df -h /` before a FULL suite; a cascade of 0.00 s FAILs after one test = this.
- `make ... RUN='X$/^$'` breaks: make eats `$/`. To run a parent test without its subtests: `RUN=X/NONE`.
- After a flag help change, check with `make help CMD=<cmd>`. It also proves the vendor dir is consistent (`go run .` uses `-mod=vendor`); a cheap build check on the host.

## Writing tests
- A test that passes the first time proves nothing. Break the code in a scratch copy (`git archive HEAD | tar -x -C /tmp/x`) and see it fail. Bitten 4 times (lock pid, cached-manifest protection, `filepath.Base` name, dry-run globals).
- `postRunCleanup` wipes the run temp dir after EVERY run: "temp empty after send" never proves `delete://` ran. Check `config.BackupTempdir` in `TestHookBeforeFinalManifest` (2026-10-14).
- `postRunCleanup` wipes the run temp dir after EVERY run: "temp empty after send" never proves `delete://` ran. Check `config.BackupTempdir` in `TestHookBeforeFinalManifest` (2026-10-14).
- In-process cobra runs leak: flag values are package globals (`cleanDryRun`, `cleanLocal`, `workingDirectory`, `zfs.ZFSPath`, `jobInfo`) and `RootCmd.SetArgs` persists. Pass bool flags explicitly (`--dry-run=false`); after each run `RootCmd.SetArgs(nil); cmd.ResetSendJobInfo()`. Run the whole package (`PKG=.`) before you trust green: a leak can `os.Exit(255)` the binary silently.
- Helpers that create volumes outside a cobra run set `config.BackupTempdir = t.TempDir()` and restore it.
- Two sends that must give identical volumes: `--compressor ""` (pgzip boundaries are timing-dependent).
- `--volsize` counts COMPRESSED bytes and pgzip holds several MiB of blocks: with gzip a 3 MiB stream is one volume at `--volsize 1`. Assert volume counts with `--compressor ""`.
- No `,` in a subtest name when `t.TempDir()` is a destination: destinations are comma-separated.
- Root-package e2e: assert only on Notice+ log lines. go-logging checks the DEFAULT backend level, which `processFlags` sets to NOTICE.
- Lock tests: a live holder must be a pid this user can signal (`os.Getppid()` or a `sleep` child). `lockfile.isRunning` treats EPERM (pid 1 as uid 501) as dead.
- Tests set `backup.HostZone`, never `time.Local` (data race with runtime timers).
- Dry-run assertions on object names: build them with `strings.TrimPrefix(path, targetDir+"/")`, not `filepath.Base`.
- `backup/` tests cannot import `files/` test helpers: copy them, and set `EncryptTo` so `StoredManifestObjectName` matches.
- `receiveStream` runs without ZFS: pass `exec.Command("cat")`.
- Fake `zfs receive` adds the received snapshot to `FAKEZFS_SNAPSHOTS`. Receiving one snapshot twice into one target: put the fixture back between (`checkRestores`). Shim streams: `FAKEZFS_RECEIVE_UNKNOWN`.
- Never shape production code around a fake's limit. Fix the fake.
- Break-the-code check without touching the tree: `rsync -a --exclude .git --exclude tmp ./ /tmp/zb-break/`, edit there, `make -C /tmp/zb-break test-one ...`.
- zfs tests are `package zfs_test` (the fake imports `zfs`).
- gpg in tests on macOS: GNUPGHOME under `t.TempDir()` is too long for gpg-agent's socket ("File name too long"); use `os.MkdirTemp("", "gpg")` and `gpgconf --kill gpg-agent` on cleanup. `gpg --verify` rejects encrypted messages; check `GOODSIG` in `--decrypt --status-file`.
- Never put backticks in a cobra/pflag usage string: pflag shows the span as the flag TYPE.
- In-process cobra runs: a subcommand keeps the context of its FIRST run (cobra sets it only when nil). `cmd.Run` resets it; use it, not `RootCmd.ExecuteContext`.
- `runInProcess(ctx, reset, args...)` in `e2e_test.go` is THE in-process runner (e2e `run`/`sendCtx`, integration `itRunCtx` call it). Do not copy its body again (three copies drifted, 2026-10-12).
- `setupS3Bucket` refuses an existing bucket (also `BucketAlreadyOwnedByYou`): its cleanup wipes every version and deletes the bucket, so it must own it.
- A test goroutine that runs a cobra command must be stopped and waited for in `t.Cleanup` (`stop(); <-finished`): its deferred flag reset lands in the next test otherwise.
- A test that sends a real signal: `cmd.SignalContext` BEFORE `syscall.Kill`, never two signals outside `exitFunc` tests.
- The file:// backend writes a volume under its final name from the first byte. "Volume done" = a cached partial manifest (`<work>/cache/*/*`), not the object.
- `--maxRetryTime` exists on send and receive only; list and clean reject it (`itRunOut` adds it for those two).
- `syncCache` downloads only manifests whose md5(name) is NOT in the cache; it never re-reads a cached name. A receive that shares `work` with the send reads the sender's cache, not the destination. To prove a restore from the destination alone, receive with `t.TempDir()` as `--workingDirectory`.
- `snapshot()` in the integration tests avoids ties on purpose. For a tie use `snapshotTie`: zfs takes ONE snapshot of a dataset per `zfs snapshot` call ("multiple snapshots of same fs not allowed") and per channel program (EAGAIN), so it is separate calls in one second, checked, retried.
- A test that restores with the sender's `work` proves nothing about the manifest at the destination. But a manifest DELETED at the destination fails both: receive takes names from the listing, only a cached name's CONTENT from the cache.
- Dry-run and `plan` must never call `PreDownload` (S3 Glacier restore = money): `syncCacheMode(..., noRestore)` and `backends.Archiver` (2026-10-13). An archived, uncached manifest of the dataset makes them FAIL (`archivedManifestsError`): dropping it gave a false broken-chain full.
- The VM wall clock is stepped BACK now and then (journal: "Time jumped backwards", ~1 s). `snapshot()` and `snapshotTie` wait for a second AFTER the last; a helper that names things by the second must do the same.
- Never split `zfs list -H` output with `strings.Fields`: dataset names may hold spaces (`TestIntegrationS3Names`). Split on lines.
- `zfs send -I` logs `-I <bare snap> ds@snap`. A flipped byte in pipe mode is caught by zfs's stream checksum, not ours.
- `wantInLog` and `wantNotInLog` are in `log_assert_test.go` (untagged, one package with the integration tests): do not redeclare them.
- Volume names hold no `--manifestPrefix`: two prefixes that send one snapshot share its objects, and clean spares orphans of a set both name.
- `t.Cleanup` is LIFO: a cleanup that must run BEFORE `destroyAfter` is registered AFTER it.
- `--maxUploadSpeed` sets `config.BackupUploadBucket` and nothing resets it: an in-process run with it must restore the old value in `t.Cleanup`.
- `VolumeInfo.Counter()` panics after `Close` (the counter is nil). Read it first.
- `make fmt-check` fails in a git worktree (the `.git` link does not resolve in the container). Use `make fmt-write FILES=...` and check `git diff`, or rsync to /tmp, `git init && git add -A`, then `make fmt-check` there.
- `PGP_PASSPHRASE` is read in `init()` AND `resetRootFlags()`: `t.Setenv` + a `Reset*JobInfo` run sees it. Unset with an encrypted key and no TTY panics the test binary.

## Git and a shared tree
- Another session can edit this tree at the same time. Run `git status` right before staging; stage BY NAME, never `git add -A`.
- A shared file already shows `M`? Stage only your hunk: `git show HEAD:<f> > /tmp/f`, add your change, `git update-index --cacheinfo 100644,$(git hash-object -w /tmp/f),<f>`.
- 1Password ssh commit signing fails in a non-interactive session ("failed to fill whole buffer"): commit with `-c commit.gpgsign=false`, re-sign after.

## Subagents
- `isolation: worktree` starts at an OLD base, not HEAD, and `make fmt-check` in a worktree saw no files. Prefer: each agent works in a `git archive HEAD` copy, owns a disjoint file set, puts new tests in NEW files, and returns a patch; apply with `git apply --3way`.
- Reviewer copies in /tmp: `diff -rq --exclude=vendor --exclude=.git` against the tree first, then run with the gocache mount.
- An independent read-only reviewer finds real issues after goldens are green. Worth it before "done".
- Verify a reviewer's NOTEs with the same grep/read as CRITICALs; wrong claims hide there.

## Docs
- Verify every behavior claim with `cs` or a read before you write it in a context doc. Agents read docs as fact.
- Check json tags before you design on a manifest field: `Destinations`, `VolumeSize`, `ManifestPrefix`, keys and smart flags are `json:"-"`.

## Code facts that are easy to get wrong
- `files.SnapshotInfo.Equal` = name + creation time (to the second), never GUID. `zfs list` has no GUID.
- cobra `PersistentPostRun` does not run when PreRunE/RunE fail; `Execute` cleans the temp dir on error.
- Pipe mode (`--maxFileBuffer 0`): either side that fails must `vol.Abort(err)`. An errgroup ctx is also canceled when `Wait` succeeds, so do not abort on it. Do not `Close` a pipe volume after a failed upload.
- `sortBackupsNewestFirst` is THE manifest order. `backends.RedactURI` is THE redaction helper.
- `send` locks `<workingDirectory>/locks/<md5(dataset)>.lck`. Same host only; invisible across users and working dirs.
- On the 1st, sanoid takes monthly, daily, hourly in that order, same second. Selection must not depend on tie order.
- `tmp/snap.json` (gitignored) is native `zfs list -j`; `testdata/zfs/*.json` is a flat array of rows. `zfs.ParseSnapshotList` rejects the native shape.
- PGP is ProtonMail/go-crypto v1 `openpgp` (not `openpgp/v2`): ed25519/cv25519 and RFC 9580 v6 keys load. Keyring fixtures in `testdata/pgp` (`make pgp-fixtures` regenerates). Ring files must be ASCII-armored.
- `dev.nix`: bump `rev`, set `hash = lib.fakeHash`, build, paste the "got:" hash. The user must run nix.

- The smart planner reads `backup.Now`. A root e2e test that sends at simulated or future dates must `pinPlanClock(t, sc.Clock(at))`.
- Smart-flag help (`--fullIfOlderThan`...) lives in `backup/plan_fixtures.go` `AddSmartFlags`; README embeds copies.
- Fake guid = fnv(name, creation), NOT dataset: a received copy keeps its guid. Mismatch: `FAKEZFS_GUIDS=ds@snap=N`. `FAKEZFS_DATASETS` = datasets with no snapshots; `FAKEZFS_MOUNTPOINTS` takes `ds=local:/path`.
- A pipe volume on receive must get `WillExtract()` before it goes to the reader.
- `-F` guards must fail closed: a `zfs list` error is "does not exist" only when `zfs.IsNotExist` says so (`refuseUnlistedTarget`, 2026-10-13).
- Prove a test of a NEW API fails on base: a compile-only shim in the base copy (same names, base behavior), or break the fix in a copy of the fixed tree.
- A test that guards against a double action passes on base by design; prove it with a break copy.

## Open plans
- `docs/specs/2026-10-14--integration-file-ssh/`: PLANNED 2026-10-14, not started. `file://` and `ssh://` on real zfs, sshd in the test container; expected defect `ssh_backend.go:239`.
- `docs/specs/2026-10-13--integration-test-sweep/`: EXECUTED 2026-10-14; six new tests (33 total), all green. Open: per-test break proofs; Azure/gs/b2/ssh, SIGTERM in receive, flaky-S3 retry (see intent.md).
- `send --dry-run` audit (2026-09-07), items 1-5, 7, 8: status unknown, see the archive.
- Prune spec gap: deleting a destination manifest leaves a cached copy that `clean` treats as live. Prune must delete the cached copy too.
