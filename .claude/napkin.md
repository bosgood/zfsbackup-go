# Napkin

Rules learned the hard way. Keep this file under 15 KB, one or two lines per entry.
Project history, review facts and defect lists go in `docs/specs/`, not here.
Old entries: `docs/specs/2026-10-11--napkin-archive/napkin-before-2026-10-11.md`.

## Bash tool and host
- Call #1 of a session is `cat .claude/napkin.md` ALONE. No chain, no separator. (Broken 9 times by chaining, last 2026-10-11, six times that day.)
- zsh expands a leading `=word`: `echo ======` aborts the whole chain. Use `echo '-----'`. Quote globs too: `--include='*.go'`.
- `grep` here is ugrep: it ignores `--` as end of options. Use `/usr/bin/grep` or `-e PATTERN` when it matters.
- macOS has no `timeout`. Put it inside the container command.
- `cd dir && ...` persists across calls; parallel calls interleave. Use absolute paths or `(cd dir && ...)`.
- Foreground `sleep N; cmd` is blocked. Wait with `until <check>; do sleep 3; done`.
- Never gate a commit on a piped test run (`make ... | grep && git commit`): the status is grep's. Run them as separate calls.
- Host zone is EDT. `TestScenarioAdoptCreationTimes` and `TestE2EPlanManifestsFileAdoptsCreationTimes` fail on the host at HEAD too. Confirm with `make test-one` (Docker, UTC).
- Host Go 1.26 is for throwaway checks in /tmp only. Nix is not installed.
- Docker containers cannot reach api.anthropic.com from this Mac; the host cannot reach proxy.golang.org. Query the proxy from a container: `docker run --rm golang:1.26-bookworm bash -c 'cd /tmp && go mod init probe >/dev/null 2>&1; go list -m -versions <module>'`.
- Host load varies a lot (e2e 7 s -> 43 s, no code change). Compare against the old tree in the same minute.
- Scratch module upgrade: rsync the tree (no .git/tmp/vendor) to /tmp, run `golang:<ver>-bookworm` with `-v /tmp/zfsb-modcache:/go/pkg/mod -e GOFLAGS=-mod=mod -e GOTOOLCHAIN=local`. Second run is fast. Delete the copies after.
- Docker here is OrbStack (arm64). Its kernel has NO zfs module and no `/lib/modules`; the maintainer will not ship one. Real `zfs` runs in the NixOS Lima VM: `make integration-vm` (see `integration/README.md`).
- OrbStack NixOS + ZFS IS possible (laris/orbstack-nixos-openzfs): build OpenZFS 2.4.1 against `/sys/kernel/kheaders.tar.xz` with 5 patches, load with a systemd unit. Rebuild after every OrbStack kernel update. Rejected for the test env 2026-10-11 (fragile); Lima VM instead.
- A privileged container sees the HOST's zpools. A test entrypoint must refuse an existing pool name and destroy only the pool it made.
- The zfs module opens a file vdev by its path in the HOST's mount namespace: a pool file only the container has gives "cannot create 'zbtest': no such pool or dataset". Bind-mount the dir at the same path. Same for `.zfs/snapshot` automount (a host helper mounts it): in a container compare with a `zfs clone` instead.
- The Lima VM sees only `~`, read-only. Scratch copies for VM runs go under `~/.cache/`, then `make -C <copy> integration-vm`.
- `limactl shell` reuses one ssh connection: a group added later does not apply. The VM's Docker socket group is wheel for that reason.
- A VM run can stall when the Mac sleeps (compile 12 s -> 34 min) and DNS can fail right after. Rerun before you debug.
- MinIO publishes no Docker images any more (quay.io and Docker Hub). `pgsty/minio` is a community build; pinned in `integration/compose.yaml`.

## Build, test, lint
- Makefile only, also for VERIFICATION. No target? Add one. Targets: `docs/context/testing.md`.
- `make test-run` = fast host loop. `make test-one` = Docker, ~10 s (mounts `/tmp/zfsb-gocache`). `make test-race` needs ~20 min (timeout 45m).
- `make fmt-check` sees TRACKED files only: a new file is not checked until `git add`. Fix with `make fmt-write FILES=...` (pinned image). Trust fmt-check, not a list.
- `make lint` runs a pinned golangci-lint v2 image. Bump the tag on purpose; a new version adds issues.
- Try a linter without editing .golangci.yml: `make lint LINT_ARGS="--enable-only X,Y --max-same-issues 0"`. 2026-10-11 probe of 31 extra linters: 1 real bug (ignored ctx), rest noise.
- `make test-docker` copies the build context in the first ~5 s; after that you can edit the tree.
- Keep the Dockerfile `GO_VERSION` default in sync with go.mod's `go` line (images set `GOTOOLCHAIN=local`).
- Newest `golang.org/x/*` and Google modules need go 1.26 (Go 1.25 is out of support since 2026-08). With `GOTOOLCHAIN=local` on 1.25, `go get -u ./...` aborts as a whole; it never picks an older version.
- `go get -u ./...` stops with "ambiguous import cloud.google.com/go/compute/metadata": first `go get cloud.google.com/go/storage@latest google.golang.org/api@latest cloud.google.com/go/compute@latest`.
- `go mod tidy` writes `go 1.26.0` when a dep declares it; `go mod edit -go=1.26` does not survive the next tidy. The Makefile takes only X.Y for the image tag.
- Right after `sed -i` on go.mod, a container can read a stale, cut file ("unterminated block"). Rerun before you debug.
- cenkalti/backoff v4 is NOT a drop-in for v2: on a canceled ctx v4's Retry returns ctx.Err(), v2 the last operation error. Kept v2 (2026-10-11).
- Module work: `make mod-update`, `mod-get MOD=p@v`, `mod-vendor`, `vulncheck` (pinned image, cache in /tmp/zfsb-modcache).
- pgzip v1.2.6/v1.2.7 panic in `Reader.WriteTo` after a `Read` (klauspost/pgzip#74). `files.VerifyEnd` hits it; `TestUnknownSignerErrorDoesNotAdviseTrust` catches it. Fix commit e1b5b318 (2026-10-09) is untagged.
- pkg/sftp >= 1.13.6: `Server.Serve` returns nil on EOF, not io.EOF. The fake sshd in `ssh_backend_test.go` must `channel.Close()` after Serve, or the client's `Close` waits forever.
- `go test` without `-v` hides stdout of passing tests.
- After a flag help change, check with `make help CMD=<cmd>`.

## Writing tests
- A test that passes the first time proves nothing. Break the code in a scratch copy (`git archive HEAD | tar -x -C /tmp/x`) and see it fail. Bitten 4 times (lock pid, cached-manifest protection, `filepath.Base` name, dry-run globals).
- In-process cobra runs leak: flag values are package globals (`cleanDryRun`, `cleanLocal`, `workingDirectory`, `zfs.ZFSPath`, `jobInfo`) and `RootCmd.SetArgs` persists. Pass bool flags explicitly (`--dry-run=false`); after each run `RootCmd.SetArgs(nil); cmd.ResetSendJobInfo()`. Run the whole package (`PKG=.`) before you trust green: a leak can `os.Exit(255)` the binary silently.
- Helpers that create volumes outside a cobra run set `config.BackupTempdir = t.TempDir()` and restore it.
- Two sends that must give identical volumes: `--compressor ""` (pgzip boundaries are timing-dependent).
- `--volsize` counts COMPRESSED bytes and pgzip holds several MiB of blocks: with gzip a 3 MiB stream is one volume at `--volsize 1`. Assert volume counts with `--compressor ""`.
- A test of `io.Copy(vol, buf)` from a `bytes.Buffer` skips the volume's bufio (WriterTo, one big Write). Copy as production does: `io.LimitReader(buf, n+1)` gives 32 KiB writes.
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
- In-process cobra runs: a subcommand keeps the context of its FIRST run (cobra sets it only when nil). `cmd.Run` resets it; use it, not `RootCmd.ExecuteContext`.
- A test that sends a real signal: `cmd.SignalContext` BEFORE `syscall.Kill`, never two signals outside `exitFunc` tests.
- The file:// backend writes a volume under its final name from the first byte. "Volume done" = a cached partial manifest (`<work>/cache/*/*`), not the object.
- Fault injection with real zfs: a bash wrapper as `--zfsPath` cuts one `zfs send` (`flakyZFS` in `integration_s3_resume_test.go`); `zfs send -n -P` must pass through. It must STALL before it fails: a failed stream cancels uploads in flight, and on S3 a stream that dies at once leaves 0 volumes to resume.
- `--resume` on real ZFS accepts the volumes of the cut attempt: `zfs send` of one snapshot gives the same bytes each run (checked 2026-10-11, zfs 2.4.4).
- `--maxRetryTime` exists on send and receive only; list and clean reject it (`itRunOut` adds it for those two).
- clean refuses a destination with objects but 0 manifests even when the cache holds a partial. To test that the partial protects its volumes, put a complete backup there first.
- Smart send, last backup's snapshot destroyed with no bookmark, no newer `_monthly`: a NOOP (`source-pruned`), not a full.
- `syncCache` downloads only manifests whose md5(name) is NOT in the cache; it never re-reads a cached name. A receive that shares `work` with the send reads the sender's cache, not the destination. To prove a restore from the destination alone, receive with `t.TempDir()` as `--workingDirectory`.
- `snapshot()` in the integration tests avoids ties on purpose. For a tie use `snapshotTie`: zfs takes ONE snapshot of a dataset per `zfs snapshot` call ("multiple snapshots of same fs not allowed") and per channel program (EAGAIN), so it is separate calls in one second, checked, retried.
- A test that restores with the sender's `work` proves nothing about the manifest at the destination. But a manifest DELETED at the destination fails both: receive takes names from the listing, only a cached name's CONTENT from the cache.
- Explicit `receive` of an incremental needs `-i <source>`, else it looks for a full set and S3 says `NotFound: Not Found`.
- `receive --origin` of an incremental is refused by `Receive` (it lists only the target). A `--properties` stream received without `--unmounted` mounts over the live source. Both red in `TestIntegrationS3ReceiveSafety`.
- Volume names hold no `--manifestPrefix`: two prefixes that send one snapshot share its objects, and clean spares orphans of a set both name.
- Smart send to out-of-sync destinations errors before it looks for anything newer (never a noop).
- `t.Cleanup` is LIFO: a cleanup that must run BEFORE `destroyAfter` is registered AFTER it.

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
- Receive pipe mode: ONE VolumeInfo has a writer (processSequence) and a reader (Extract). The writer's Close must not touch reader layers (`rw`, `cmd`); `extracted` marks them (fix 5cfb96d).
- Pipe mode (`--maxFileBuffer 0`): either side that fails must `vol.Abort(err)`. An errgroup ctx is also canceled when `Wait` succeeds, so do not abort on it. Do not `Close` a pipe volume after a failed upload.
- `sortBackupsNewestFirst` is THE manifest order. `backends.RedactURI` is THE redaction helper.
- `send` locks `<workingDirectory>/locks/<md5(dataset)>.lck`. Same host only; invisible across users and working dirs.
- On the 1st, sanoid takes monthly, daily, hourly in that order, same second. Selection must not depend on tie order.
- `tmp/snap.json` (gitignored) is native `zfs list -j`; `testdata/zfs/*.json` is a flat array of rows. `zfs.ParseSnapshotList` rejects the native shape.
- gpg's default ed25519 key fails until the pgp-ed25519 branch lands; use RSA. Ring files must be ASCII-armored.
- `dev.nix`: bump `rev`, set `hash = lib.fakeHash`, build, paste the "got:" hash. The user must run nix.

## Open plans
- `docs/specs/2026-10-11--integration-test-more/`: EXECUTED 2026-10-11 (01e927f..); thirteen TestIntegrationS3* tests; `ReceiveSafety/Mounted` and `/Origin` red on purpose (findings); notes at the end of its plan.md.
- A chain keeps one stream kind: send (smart and `-i`), plan and `receive --auto` refuse an incremental whose `--raw` differs from its source's backup (`MixedChainError`, 0c038d2). A full starts a new chain.
- `docs/specs/2026-10-11--integration-test-inc/`: EXECUTED 2026-10-11; five more TestIntegrationS3* tests; notes at the end of its plan.md.
- `docs/specs/2026-10-11--deps-upgrade/`: EXECUTED 2026-10-11 (0671ffd..), Go 1.26; notes at the end of its plan.md.
- `docs/specs/2026-10-11--integration-test-env/`: EXECUTED 2026-10-11 (d426bec..); NixOS VM instead of Ubuntu; notes at the end of its plan.md.
- `docs/specs/2026-10-11--sigterm-handling/`: EXECUTED 2026-10-11 (a2c0636..069eeba); notes at the end of its plan.md.
- `docs/specs/2026-10-11--third-pass-fixes/`: EXECUTED 2026-10-11 (caed77d..525b133); notes at the end of its plan.md.
- `docs/specs/2026-09-29--pgp-ed25519/`. Unexecuted as of 2026-09-29.
- `send --dry-run` audit (2026-09-07), items 1-5, 7, 8: status unknown, see the archive.
- Prune spec gap: deleting a destination manifest leaves a cached copy that `clean` treats as live. Prune must delete the cached copy too.
