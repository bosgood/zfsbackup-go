# Napkin

Rules learned the hard way. Keep this file under 15 KB, one or two lines per entry.
Project history, review facts and defect lists go in `docs/specs/`, not here.
Old entries: `docs/specs/2026-10-11--napkin-archive/napkin-before-2026-10-11.md`.

## Bash tool and host
- Call #1 of a session is `cat .claude/napkin.md` ALONE. No chain, no separator. (Broken 3 times by chaining.)
- zsh expands a leading `=word`: `echo ======` aborts the whole chain. Use `echo '-----'`. Quote globs too: `--include='*.go'`.
- `grep` here is ugrep: it ignores `--` as end of options. Use `/usr/bin/grep` or `-e PATTERN` when it matters.
- macOS has no `timeout`. Put it inside the container command.
- `cd dir && ...` persists across calls; parallel calls interleave. Use absolute paths or `(cd dir && ...)`.
- Foreground `sleep N; cmd` is blocked. Wait with `until <check>; do sleep 3; done`.
- Never gate a commit on a piped test run (`make ... | grep && git commit`): the status is grep's. Run them as separate calls.
- Host zone is EDT. `TestScenarioAdoptCreationTimes` and `TestE2EPlanManifestsFileAdoptsCreationTimes` fail on the host at HEAD too. Confirm with `make test-one` (Docker, UTC).
- Host Go 1.26 is for throwaway checks in /tmp only. Nix is not installed.
- Docker containers cannot reach api.anthropic.com from this Mac; the host cannot reach proxy.golang.org. Query the proxy from a container: `docker run --rm golang:1.25-bookworm bash -c 'cd /tmp && go mod init probe >/dev/null 2>&1; go list -m -versions <module>'`.
- Host load varies a lot (e2e 7 s -> 43 s, no code change). Compare against the old tree in the same minute.

## Build, test, lint
- Makefile only, also for VERIFICATION. No target? Add one. Targets: `docs/context/testing.md`.
- `make test-run` = fast host loop. `make test-one` = Docker, ~10 s (mounts `/tmp/zfsb-gocache`). `make test-race` needs ~20 min (timeout 45m).
- `make fmt-check` sees TRACKED files only: a new file is not checked until `git add`. Fix with `make fmt-write FILES=...` (pinned image). Trust fmt-check, not a list.
- `make lint` runs a pinned golangci-lint v2 image. Bump the tag on purpose; a new version adds issues.
- Try a linter without editing .golangci.yml: `make lint LINT_ARGS="--enable-only X,Y --max-same-issues 0"`. 2026-10-11 probe of 31 extra linters: 1 real bug (ignored ctx), rest noise.
- `make test-docker` copies the build context in the first ~5 s; after that you can edit the tree.
- Keep the Dockerfile `GO_VERSION` default in sync with go.mod's `go` line (images set `GOTOOLCHAIN=local`).
- `go test` without `-v` hides stdout of passing tests.
- After a flag help change, check with `make help CMD=<cmd>`.

## Writing tests
- A test that passes the first time proves nothing. Break the code in a scratch copy (`git archive HEAD | tar -x -C /tmp/x`) and see it fail. Bitten 4 times (lock pid, cached-manifest protection, `filepath.Base` name, dry-run globals).
- In-process cobra runs leak: flag values are package globals (`cleanDryRun`, `cleanLocal`, `workingDirectory`, `zfs.ZFSPath`, `jobInfo`) and `RootCmd.SetArgs` persists. Pass bool flags explicitly (`--dry-run=false`); after each run `RootCmd.SetArgs(nil); cmd.ResetSendJobInfo()`. Run the whole package (`PKG=.`) before you trust green: a leak can `os.Exit(255)` the binary silently.
- Helpers that create volumes outside a cobra run set `config.BackupTempdir = t.TempDir()` and restore it.
- Two sends that must give identical volumes: `--compressor ""` (pgzip boundaries are timing-dependent).
- No `,` in a subtest name when `t.TempDir()` is a destination: destinations are comma-separated.
- Root-package e2e: assert only on Notice+ log lines. go-logging checks the DEFAULT backend level, which `processFlags` sets to NOTICE.
- Lock tests: a live holder must be a pid this user can signal (`os.Getppid()` or a `sleep` child). `lockfile.isRunning` treats EPERM (pid 1 as uid 501) as dead.
- Tests set `backup.HostZone`, never `time.Local` (data race with runtime timers).
- Dry-run assertions on object names: build them with `strings.TrimPrefix(path, targetDir+"/")`, not `filepath.Base`.
- `backup/` tests cannot import `files/` test helpers: copy them, and set `EncryptTo` so `StoredManifestObjectName` matches.
- `files.MaxManifestBytes` is a var: set it small with `setManifestLimit`. `writeTestManifest` uses compression level 0; set `CompressionLevel: 6` when the object must be smaller than its JSON.
- `receiveStream` runs without ZFS: pass `exec.Command("cat")`.
- Fake `zfs receive` adds the received snapshot to `FAKEZFS_SNAPSHOTS`. Receiving one snapshot twice into one target: put the fixture back between (`checkRestores`). Shim streams: `FAKEZFS_RECEIVE_UNKNOWN`.
- Never shape production code around a fake's limit. Fix the fake.
- Break-the-code check without touching the tree: `rsync -a --exclude .git --exclude tmp ./ /tmp/zb-break/`, edit there, `make -C /tmp/zb-break test-one ...`.
- Fault injection: upload failure on `file://` = a regular FILE at `<dest>/tank`. ENOSPC = `make test-enospc`. S3 without network = `httptest.Server` + `AWS_S3_CUSTOM_ENDPOINT`, `AWS_REGION`, keys (fake in `clean_s3_test.go`).
- zfs tests are `package zfs_test` (the fake imports `zfs`).
- `fmt.Sprintf(w)` with no verb appends `%!(EXTRA ...)`. `fmt.Sprint("a", 1)` is `a1` (space only between two non-strings).
- Never put backticks in a cobra/pflag usage string: pflag shows the span as the flag TYPE.

## Git and a shared tree
- Another session can edit this tree at the same time. Run `git status` right before staging; stage BY NAME, never `git add -A`.
- A shared file already shows `M`? Stage only your hunk: `git show HEAD:<f> > /tmp/f`, add your change, `git update-index --cacheinfo 100644,$(git hash-object -w /tmp/f),<f>`.
- No `git add -p` here. To split a staged change: save `git diff --cached`, reset, rebuild each step's file from HEAD + line ranges; the final diff must be byte-identical to the saved patch.
- Check a commit stack: `git archive` each commit to /tmp, `go vet ./...` all of them in ONE container (vet also compiles tests). Delete the copies (~30 MB each).

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
- gpg's default ed25519 key fails until the pgp-ed25519 branch lands; use RSA. Ring files must be ASCII-armored.
- `dev.nix`: bump `rev`, set `hash = lib.fakeHash`, build, paste the "got:" hash. The user must run nix.

## Open plans
- `docs/specs/2026-10-11--third-pass-fixes/`: EXECUTED 2026-10-11 (caed77d..525b133); notes at the end of its plan.md.
- `docs/specs/2026-09-29--pgp-ed25519/`. Unexecuted as of 2026-09-29.
- `send --dry-run` audit (2026-09-07), items 1-5, 7, 8: status unknown, see the archive.
- Prune spec gap: deleting a destination manifest leaves a cached copy that `clean` treats as live. Prune must delete the cached copy too.
