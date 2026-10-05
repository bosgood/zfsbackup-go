# Send-path adversarial review (2026-10-05) Fix Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix what an adversarial review of the `send` path found. Five bugs can publish a
backup that is wrong or that someone else deletes, while `send` exits 0 or the planner
treats the set as complete. A second group wedges every later run until someone deletes a
file by hand. The rest are lifecycle and validation gaps.

**How the findings were established:** a read-only sweep produced 13 leads. Five agents
then reproduced each lead against `e9ef349` in scratch copies, using the fake zfs and the
in-process e2e helpers, with docker `--tmpfs` for ENOSPC and subprocess runs for signals.
Every verdict below comes from a run, not from reading. Where a race window was widened
by injecting a sleep, the finding says so. The scratch copies (`/tmp/sendrev-{A,B,C,D,E}`)
hold the repro tests. They are in `/tmp`, so they can disappear; each task below describes
its test well enough to rewrite it.

**Tech Stack:** Go 1.25, vendored deps, in-process e2e tests (`e2e_*_test.go`, fake zfs in
`internal/fakezfs`), the fake S3 in `clean_s3_test.go`.

Line numbers are as of `e9ef349` on `clean-dry-run`; re-check them before editing.

---

## Ground rules

- Branch `clean-dry-run`. One commit per task. Commit messages follow the existing style
  (`send: ...`, `clean: ...`, `receive: ...`, `files: ...`).
- TDD: each task names a test that must fail on the unfixed code. Run it and watch it
  fail before fixing anything. A test that passes on the first run is wrong (napkin,
  2026-10-02).
- Fast loop: `make test-run PKG=./backup/ RUN='TestName'` (also `./files/`, and `PKG=.`
  for the root e2e tests). After an in-process cobra test, run the whole package.
- Before declaring the plan done: `make test-docker`.
- Tests that need docker `--tmpfs` (ENOSPC) get their own Makefile target. Gate them on an
  env var so plain `go test` skips them (napkin: extend the Makefile, never ad-hoc docker).

## Findings

Severity: **CRITICAL** means the tool can publish a wrong or unrestorable backup while
reporting success, or delete live data. **WARNING** means it wedges runs, leaks storage,
or hides a CRITICAL. **NOTE** is cosmetic or not reachable today.

| #   | Lead           | Verdict                | Sev            | Summary                                                                                                                                                                                                                                                                                                                                                                                                                          | Task |
| --- | -------------- | ---------------------- | -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---- |
| F1  | 1              | CONFIRMED              | CRITICAL       | A splitter failure (ENOSPC on a volume write) while `zfs send` is still running publishes a final manifest of the volumes done so far. 17 of 300 ENOSPC runs did so. After that, `refuseExistingSet` protects the set, smart mode says "Nothing new to back up", and `receive` feeds zfs a short stream. Exit code is non-zero.                                                                                                  | 2    |
| F2  | 2              | CONFIRMED              | CRITICAL       | `VolumeInfo.Close` drops the `bufw.Flush()` error. On ENOSPC at the last flush, a short volume is uploaded under a manifest that describes the full one. Worse: a manifest under 256 KiB (almost every manifest) is written only by that flush, so a **0-byte manifest** is published, send exits 0, re-send is refused, and `clean` aborts with EOF for the whole destination.                                                  | 1    |
| F3  | 3a             | CONFIRMED              | CRITICAL       | `--resume` skips N bytes of the new stream without checking them. A snapshot destroyed and recreated under the same name gives a set made of the old stream's prefix followed by the new stream's suffix. Send exits 0. The manifest records the new snapshot's creation time even though vol1..k hold old data. The same mechanism applies to `-R` children or properties changed between attempts, and to a ZFS upgrade.       | 4    |
| F4  | 8b             | CONFIRMED              | CRITICAL       | `copyManifest` (the `--resume` completion of a set whose manifest is missing at some destinations) checks only that the volume names exist at the lagging destination. Leftovers from an earlier attempt with a different `--volsize` have the same names, so the copied manifest describes bytes that are not there. Send exits 0, and a fresh-host receive gets SHA256 mismatches. Same-size volumes defeat a size-only check. | 5    |
| F5  | 9a             | CONFIRMED              | CRITICAL       | The send lock lives in `os.TempDir()`. When `send` and `clean` see different `TMPDIR`s (systemd `PrivateTmp`, cron vs shell, macOS per-user TMPDIR), `clean` deletes the in-flight volumes. Send then exits 0 and publishes a manifest that points at them.                                                                                                                                                                      | 3    |
| F6  | 5 + E-NEW-1    | CONFIRMED              | WARNING        | Cached manifests are written in place (`CopyTo`, `downloadTo`), and `syncCache` trusts any file by name. A kill, a failed download (no kill needed), or two concurrent sends of _different_ datasets to one destination leave a truncated cache file. Every later smart send, `list`, `clean` and `receive` for that destination then fails with EOF, permanently. A truncated partial wedges `--resume` and `clean`.            | 6    |
| F7  | C-NEW, B-NEW-1 | CONFIRMED              | WARNING        | `receive` hangs on **any** pipeline error, not just a missing volume. `case orderedVolumes <- <-c` blocks on `<-c` outside the select (`restore.go:365`). Separately, a SHA256 mismatch is retried as if transient, for up to `--maxRetryTime` (12h). Together they make every corrupt-backup finding above look like a hang instead of an error.                                                                                | 7    |
| F8  | 3b             | CONFIRMED              | WARNING        | Resume compares `EncryptTo`/`SignFrom` strings, not keys. After a key is rotated under the same address, one set mixes the old and new keys (vol1-2 to k1, vol3-5 to k2).                                                                                                                                                                                                                                                        | 4    |
| F9  | 4              | PARTIAL                | WARNING        | Object names ignore `-R -p -w -D -s`, `-i` vs `-I`, bookmark vs snapshot source, and `""` vs `zfs` compressor. Every reachable pair is **refused** (by `refuseExistingSet` or the resume argv compare), never mixed, so that much is fine. The gap: `copyManifest` compares no options, so `--resume` without `-R -p` completes a `-R -p` set and exits 0.                                                                       | 5    |
| F10 | 8a             | CONFIRMED              | WARNING        | A manifest missing at one destination wedges every smart send without `--resume` ("destinations are out of sync"), and the message does not mention `--resume`.                                                                                                                                                                                                                                                                  | 8    |
| F11 | 6              | CONFIRMED              | WARNING / NOTE | (a) When a stream ends exactly on a volume boundary, an empty trailing volume is uploaded (NOTE: restore copes). (b) If only the final manifest upload fails, `--resume` re-reads the **entire** snapshot through `zfs send`, discards it, and uploads an empty volume N+1 (WARNING: the full read cost on a real dataset).                                                                                                      | 9    |
| F12 | 13a            | PARTIAL                | WARNING        | A broken external compressor (exits 0 without reading, or drains and outputs nothing) publishes 0-byte volumes and send succeeds, if the volume fits in the pipe buffer (small incrementals). Larger volumes get EPIPE. `--compressor` is not validated.                                                                                                                                                                         | 10   |
| F13 | 13b            | CONFIRMED              | WARNING        | `--volsize 0` underflows the threshold at `backup.go:757`, producing one unbounded volume. A huge `--volsize` overflows the same way. All other numeric flags are validated.                                                                                                                                                                                                                                                     | 10   |
| F14 | B-NEW-2        | CONFIRMED              | NOTE           | An external compressor's path goes verbatim into object names (`...zstream./tmp/x/pigz.vol1`). A `.` in the path makes `ParseBackupVolumeObjectName` reject the name, so `clean` never attributes those volumes.                                                                                                                                                                                                                 | 10   |
| F15 | 12             | CONFIRMED              | WARNING        | No signal handler. SIGTERM/SIGINT skip every defer. A killed run leaves its temp dir (up to `maxFileBuffer × volsize`), and nothing ever sweeps it. It also leaves a stale lock (recovered by pid liveness) and S3 multipart uploads (billable without a lifecycle rule). The `zfs send` child lives until its next write. `--resume` works afterwards, unless the kill landed in a cache write (F6).                            | 11   |
| F16 | 9b + E-NEW-2   | CONFIRMED              | WARNING        | The lockfile is pid-based. Two containers that share the lock dir are both pid 1, so both take the lock. A stale lock's takeover (`GetOwner` → remove → create) is not atomic, so two contenders can both take it. Since F15 makes stale locks routine, a send and a clean that start together after a crash can both get in, which reopens F5.                                                                                  | 12   |
| F17 | 7              | CONFIRMED              | WARNING        | A failed attempt at snapshot X followed by a smart send of Y leaves X's volumes forever: its local-only partial counts as live. `--cleanLocal` reclaims them unless X was the dataset's only set. In that case it deletes the cache file but skips the volumes, and every later `clean` refuses ("objects but no manifests").                                                                                                    | 13   |
| F18 | D-NEW-1        | CONFIRMED              | WARNING        | `copyManifest` does not refresh the lagging destination's cache, so a stale partial stays under the final name. A same-host restore check passes while a fresh host fails.                                                                                                                                                                                                                                                       | 5    |
| F19 | 10             | CONFIRMED              | NOTE           | `ZFSStreamBytes` is 0 in about 1% of successful manifests (ordering, not a data race). It is only displayed.                                                                                                                                                                                                                                                                                                                     | 2    |
| F20 | 9c             | REFUTED (harmful case) | —              | Planning before the lock is harmless for the same dataset: the loser is refused under the lock. The real harm is F6's concurrent cache writes across datasets. `adoptCacheDir` has no harmful interleaving.                                                                                                                                                                                                                      | 6    |
| F21 | 11             | PARTIAL                | NOTE           | The early `return berr` after the goroutines start is unreachable: `DeleteBackend.Init` cannot fail. With an injected failure, the lock is released while `sendStream` and `zfs send` still run. The `Destinations` append is not observable.                                                                                                                                                                                    | 14   |
| F22 | A-NEW-1        | CONFIRMED              | NOTE           | Data race in pipe mode on `VolumeInfo.isOpened` (`volumeinfo.go:294` vs `:177`). No functional failure seen in 30 runs.                                                                                                                                                                                                                                                                                                          | 14   |
| F23 | misc           | CONFIRMED              | NOTE           | `Close` sets `isClosed` before steps that can fail, so a second `Close` returns nil. "Total Files Uploaded" counts resumed volumes. `--zfsPath zfs` vs `/sbin/zfs` refuses resume. Volumes overshoot `--volsize` by up to 50% with gzip.                                                                                                                                                                                         | 14   |

Already planned elsewhere, not repeated here: atomic file/SSH uploads (a kill mid-upload
leaves a truncated manifest that `refuseExistingSet` trusts) and the SSH backend's local
`os.Remove`. Both are in `docs/specs/2026-10-02--backends-bugs/plan.md` (Task 7 and the
SSH cleanup task).

## Decisions made in this plan (review these first)

1. **Resume proves stream identity by hashing the skipped prefix.** The partial manifest
   gets a cumulative SHA-256 of the raw zfs stream after each volume (`StreamSHA256` per
   volume). Resume hashes the bytes it currently discards and refuses on mismatch. A
   partial manifest written before this change has no hashes, so resume refuses it with
   "start over without --resume" rather than trusting it. Snapshot `guid` is also
   recorded and compared, so a mismatch fails before zfs send even starts.
2. **`copyManifest` hashes the lagging destination's volumes before publishing.** Size
   alone is not enough (F4 repro 2 has equal sizes). It downloads each volume and compares
   it with the manifest's SHA-256. This path only runs to repair a partial set, so the
   cost is acceptable. On any mismatch it refuses and tells the user to delete the
   leftovers at that destination.
3. **The send lock moves to `<workingDirectory>/locks/` and uses `flock(2)`.** The working
   directory holds the cache that the lock really protects, and `TMPDIR` does not change
   it. `flock` is released by the kernel on any death, so stale locks, pid recycling and
   pid namespaces no longer matter. Residual risk, documented in the runbook: a `send` and
   a `clean` with _different_ `--workingDirectory` on the same host, or on different
   hosts, are still unprotected. During the transition, `clean` also probes the old
   `os.TempDir()` path, so a send from the old binary that is still running is respected.
4. **Smart mode completes a partial set without `--resume`** once Task 5 makes completion
   verify content. Completion copies a manifest and sends nothing, and it unwedges cron.
5. **Object names stay as they are.** Name collisions between send variants are refused
   rather than mixed (F9). Putting flags into names is a format change that `clean` and
   `ParseBackupVolumeObjectName` would have to learn, and nothing needs it yet.
6. **A partial is superseded, and `clean` reclaims it, when the destination has a final
   manifest for the same dataset newer than the partial, or when the partial's snapshot
   no longer exists locally.** Otherwise it stays live, as it is today.

---

### Task 1: `VolumeInfo.Close` reports flush errors (F2, part of F23)

**Files:** `files/volumeinfo.go:285-345`, tests in `files/volumeinfo_test.go`, root e2e.

- Capture `err := v.bufw.Flush()`. Still close `fw` and `pw`, then return the first error.
  On error, do not fill in `Size` or the sums. Set `isClosed` only on success.
- For file-backed volumes, after closing, `os.Stat(v.filename).Size()` must equal
  `v.Size`. Otherwise return an error, which catches a late ENOSPC/EDQUOT.
- `CopyTo`: check the error from `out.Close()` (folded into Task 6's atomic write).

Tests:

- `TestVolumeCloseReportsFlushError` (files pkg, no docker): swap the writer under `bufw`
  for one that fails after N bytes, through an `export_test.go` hook. Write 100 KiB.
  `Close` must return an error.
- `TestE2EENOSPCAtFinalFlushFails` (root, env-gated, new Makefile target `test-enospc`
  running docker with `--tmpfs /work/temp:size=40k`). Case A: a 200 KiB stream with
  `--compressor ""`, where the volume flush hits ENOSPC. Case B: `--maxFileBuffer 0` with a
  pre-filled tmpfs, where the manifest flush hits ENOSPC. Both must make `send` return an
  error and leave no `manifests|` object at the destination.

### Task 2: no final manifest after a failed stream (F1, F19)

**Files:** `backup/backup.go:399-402` (forwarder), `:492-511` (finalizer), `:727-870`
(`sendStream`), `:1040-1065` (`retryUploadChainer`).

- The splitter must stop closing `c` (`defer close(c)` at `:729`). Instead, `sendStream`
  closes it after `group.Wait()` succeeds and after storing `j.ZFSStreamBytes` (`:870`).
  On error, `c` stays open and the forwarder leaves through `ctx.Done`. By the time the
  outer ctx is cancelled, `pending.zero` can no longer be reached with a live ctx.
- Belt and braces: `retryUploadChainer` checks `ctx.Err()` before starting the upload of
  an `IsManifest` volume. The backoff operation runs once even after cancel (cenkalti v2).
- Add a test hook `testHookStreamFailed func()`, called on `sendStream`'s error path just
  before it returns.

Tests:

- `TestE2EFailedStreamPublishesNoManifest`: the hook sleeps 200ms, with
  `FAKEZFS_FAIL_AFTER_BYTES=2.5MiB --volsize 1 --compressor ""`. Assert that
  `manifestNames(destObjects(dest))` is empty and that no cached manifest has a non-zero
  `EndTime`. It fails 10 of 10 on current code.
- `TestSendRecordsStreamBytes`: a hook sleeps just before `:870`. Assert
  `ZFSStreamBytes == streamBytes` in the published manifest.

### Task 3: lock under the working directory (F5)

**Files:** `backup/backup.go:270-277` (`volumeLock`), `backup/clean.go` (lock probing),
`cmd/root.go` (make sure `<workingDirectory>/locks` exists).

- `volumeLock` puts its file at `filepath.Join(config.WorkingDir, "locks", "<md5>.lck")`.
- `clean` takes the new lock. For one release it also tries the legacy
  `os.TempDir()` path, and a legacy lock held by a live pid counts as held (Decision 3).
- `flock` comes in Task 12. This task only moves the lock, so that each fix lands on its
  own.

Test: `TestCleanHonoursSendLockAcrossTMPDIR`. Write a live-pid lock the way
`TestCleanSkipsDatasetOfRunningSend` does, then `t.Setenv("TMPDIR", t.TempDir())` before
`Clean`. The in-flight volume must survive.

### Task 4: resume proves the stream and keys are unchanged (F3, F8)

**Files:** `files/jobinfo.go` (new manifest fields), `files/volumeinfo.go` (per-volume
stream hash), `backup/backup.go:734-753` (skip), `:895-956` (`tryResume`), `zfs/` (guid).

- New fields: `VolumeInfo.StreamSHA256`, the cumulative SHA-256 of raw stream bytes
  through the end of this volume, computed by a hasher on the `counter` side of the
  splitter. Also `SnapshotInfo.GUID` (filled from `zfs get -Hp guid`; teach the fake zfs
  this property), plus `EncryptKeyFingerprint` and `SignKeyFingerprint` on `JobInfo`
  (json-tagged; `EncryptKey`/`SignKey` are `json:"-"`).
- `tryResume` refuses with "option mismatch" when any of these hold:
  - the base or incremental GUID differs;
  - a fingerprint differs;
  - the cached manifest has volumes but no `StreamSHA256` (written by an older version;
    Decision 1).
- `sendStream`'s skip hashes the bytes instead of discarding them to `ioutil.Discard`, and
  compares the result with the last kept volume's `StreamSHA256`. On mismatch it fails
  with "stream differs from the interrupted attempt; run without --resume". This fails
  before any new volume is uploaded.

Tests (need a `FAKEZFS_STREAM_SALT` knob that salts the fake's stream bytes, plus
`fakezfs.StreamSalted`):

- `TestE2EResumeRefusesChangedStream`: run `interruptedSend`, delete vol3+, recreate the
  snapshot with a salt and a new creation time, then `--resume`. It must error, and no
  manifest may be published. Subtest: salt only (same creation, same guid), which only the
  prefix hash catches.
- `TestE2EResumeRefusesRotatedKey`: two RSA entities with the same email (set
  `PreferredHash` to SHA256). Interrupt with ring `[k1]`, then resume with `[k2, k1]`. It
  must error with "option mismatch". Key rings are package globals, so pass both ring
  paths on every run.

### Task 5: `copyManifest` verifies what it publishes (F4, F9, F18)

**Files:** `backup/backup.go:601-656` (`copyManifest`), with checks shared with
`tryResume`.

- Before uploading, run the same option comparison as `tryResume` (compressor, argv, GUID,
  fingerprints) between the current job and the source manifest.
- For each volume, the lagging destination must have the right size (`backends.Sizer`) and
  the right content: download it and compare the SHA-256 with the manifest (Decision 2).
  On mismatch, refuse and name the leftovers to delete.
- After uploading, write the copied manifest into `cacheDirFor(dest.uri)` with Task 6's
  atomic writer, replacing any stale partial.

Tests:

- `TestE2EResumeRefusesToCompleteOverForeignVolumes`. Case 1: truncate vol2 at dest2.
  Case 2: an interrupted `--volsize 1` attempt at dest2, then a full `--volsize 2` send to
  dest1 alone. Both: `--resume d1,d2` must fail, and dest2 must have no manifest.
- `TestE2EResumeCompleteRefusesOtherVariant`: send `-R -p --compressor zfs` to A and B,
  then delete B's manifest. `--resume --compressor ""` without `-R -p` must error.
- `TestE2ECompletedSetRefreshesLaggingCache`: after a successful completion, the cached
  copy for dest2 is byte-identical to the uploaded manifest.

### Task 6: atomic cache writes, self-healing reads (F6, F20)

**Files:** `files/volumeinfo.go:401-417` (`CopyTo`), `backup/restore.go:535-557`
(`downloadTo`), `backup/sync.go:142-161`, readers at `backup/backup.go:170-175`,
`:899-904`, `backup/clean.go:132-153`, `cmd/list.go`/`backup/list.go`,
`backup/restore.go:254-271`.

- Add one helper, `writeFileAtomic(dir, name, r)`: `os.CreateTemp(dir, ".tmp-*")`, copy,
  `Sync`, check `Close`, `Rename`, and remove the temp file on any error. Use it in
  `CopyTo` and `downloadTo`. `syncCache` and `clean` must ignore `.tmp-*` files.
- When a cached manifest that exists at the destination fails to decode, delete the cached
  copy, download it once more, and fail only if the fresh copy also fails to decode. Do
  this in one shared reader used by the planner, `Receive`, `Clean` and `List`.
- `tryResume`: an unreadable partial is treated like a missing one, with a warning
  ("starting over").

Tests:

- `TestDownloadToLeavesNoPartialFileOnError` (backup pkg): a fake backend whose reader
  errors halfway. Afterwards no file exists under the final name.
- `TestSyncCacheRetriesTruncatedDownload`: the second `syncCache` downloads again.
- `TestE2ESmartSendHealsTruncatedCache`: send a, truncate the cache file to 0 and to half,
  then `send --increment` must succeed.
- `TestE2EResumeIgnoresTruncatedPartial`: `interruptedSend`, truncate the partial, then
  `--resume` must succeed and pass `checkRestores`.

### Task 7: `receive` fails instead of hanging (F7)

**Files:** `backup/restore.go:320-380` (downloaders and orderer), `:333-348` and
`:426-440` (retry).

- Orderer: `select { case v, ok := <-c: ...; case <-ctx.Done(): return ctx.Err() }`, then
  send `v` under its own select. Close each `sequence.c` when its downloader is done with
  it, instead of with a `defer` that only runs when the goroutine returns.
- Hash and size mismatches return `backoff.Permanent(err)`.
- This also fixes the open bug "receive hangs on missing volume" (napkin 2026-10-02), so
  update the napkin and the memory file once it lands.

Tests, all inside `guarded`:

- `TestE2EReceiveFailsOnMissingVolume`: delete vol2.
- `TestE2EReceiveFailsOnUndecryptableVolume`: publish encrypted to k1, receive with ring
  `[k2]`.
- `TestE2EReceiveFailsFastOnHashMismatch`: flip a byte in vol1 and keep the default
  `--maxRetryTime`. It must error within the guard.

### Task 8: smart mode completes a partial set without `--resume` (F10)

**Files:** `backup/plan.go:128-165`.

- Drop the `&& jobInfo.Resume` at `:132` (Decision 4; safe after Task 5).
- Keep the generic out-of-sync error, but when `partialSet` is nil and the destinations
  still disagree, say which destination is behind.
- Update the scenario goldens that this changes (`make scenarios-update`, then review the
  diff).

Test: `TestE2ESmartCompletesPartialWithoutResume`. Use two destinations, with a
`manifests|tank` blocker at dest2 for the first run, then remove it. A smart send without
`--resume` must complete the set at dest2.

### Task 9: no empty trailing volume; resume of a finished set uploads only its manifest (F11)

**Files:** `backup/backup.go:755-800` (splitter), `:506`/`:685-693`, and `tryResume`.

- Splitter: wrap `counter` in a `bufio.Reader`. Before creating a volume, call `Peek(1)`;
  on EOF, finish without a new volume. A zero-length stream still gets one volume, so the
  set is never empty; check what `zfs send` of an empty incremental emits and keep that
  restorable.
- Record completion: a final manifest has a non-zero `EndTime`. When `tryResume` finds a
  complete cached manifest whose volumes all verify, it uploads only the manifest (sharing
  code with `copyManifest`), and `zfs send` is never started.

Tests:

- `TestE2EStreamOnVolumeBoundaryHasNoEmptyVolume`: 1 MiB and 3 MiB streams with
  `--volsize 1 --compressor ""`. Assert 1 and 3 volumes, and `checkRestores` passes.
- `TestE2EResumeAfterManifestUploadFailureUploadsOnlyManifest`: use the blocker, then
  `--resume`. The only new object is the manifest, and the fake zfs log shows no `send`.

### Task 10: validate `--volsize` and `--compressor` (F12, F13, F14)

**Files:** `files/jobinfo.go:177-217` (`ValidateSendFlags`), `:301-329` (names),
`files/volumeinfo.go:490-525`, `cmd/send.go` PreRunE.

- `--volsize` must be at least 1 and at most `math.MaxUint64/MiB`. Compute the threshold
  without the subtraction underflow.
- External compressor: in PreRunE, round-trip about 1 MiB of a known payload through
  `<c> -c -N` and then `<c> -c -d`, which is more than the pipe buffer. The output must
  equal the input. Also, at `Close`, a volume with raw bytes > 0 and `Size == 0` is an
  error.
- Object-name extension: use `filepath.Base(compressor)`, and reject a base name that
  contains `.` or `/` (F14). Existing sets with odd names are not renamed; the runbook
  notes it.

Tests:

- `TestValidateSendFlagsVolumeSize`, table-driven: 0 and 2^44 fail, 1 and 200 pass.
- `TestE2EExternalCompressorMustRoundTrip`: scripts that drain and output nothing, or
  sleep and exit 0, with a 1000 B stream. Send must fail.
- `TestVolumeNameUsesCompressorBase`: `/opt/bin/pigz` gives `.pigz.`.

### Task 11: signals, process group, temp sweep (F15)

**Files:** `main.go`, `cmd/root.go` (`Execute`, `postRunCleanup`, temp dir creation at
`:388`), `zfs/zfs.go` (send command).

- `main.go` uses `signal.NotifyContext(ctx, SIGINT, SIGTERM)` and passes the context to
  `Execute`, so commands run with it. The pipeline already selects on ctx, so a signal
  unwinds: the lock is released, the zfs child is killed, and `postRunCleanup` runs. A
  second signal exits at once.
- `zfs send` runs with `SysProcAttr{Setpgid: true}`, and cancelling kills the group.
- At startup, delete `workingDir/temp/zfsbackup*` directories older than the oldest live
  run. Simplest rule: each run writes its pid into its temp dir, and a dir whose pid is
  dead or missing is removed. Do this under the Task 12 lock directory's protection or
  with a separate flock.
- Runbook: S3 buckets need an `AbortIncompleteMultipartUpload` lifecycle rule.

Test: `TestSendSIGTERMLeavesNoLockOrTemp`. Build the binary in `TestMain`, start a slow
send as a subprocess (a new fake knob `FAKEZFS_SLOW_MS`), and SIGTERM it. The exit must be
non-zero, the temp dir must be empty, the zfs pid must be gone, and a following
`--resume` must succeed.

### Task 12: `flock`-based lock (F16)

**Files:** `backup/backup.go` (`volumeLock` and its callers), `backup/clean.go`, and drop
`nightlyone/lockfile` from `vendor/` if nothing else uses it.

- Open `<workingDirectory>/locks/<md5>.lck` and `syscall.Flock(fd, LOCK_EX|LOCK_NB)`. Hold
  the fd for the whole run. The file content (pid, hostname, start time) is for
  diagnostics only.
- `clean`'s "held?" probe takes `LOCK_EX|LOCK_NB` and releases it at once if it got it.
- The legacy `os.TempDir()` probe from Task 3 keeps using pid semantics.

Tests:

- `TestVolumeLockIsExclusiveWithinProcess`: two handles in one process; the second fails.
  On current code it returns nil.
- `TestVolumeLockReleasedOnProcessDeath`: a subprocess takes the lock and is SIGKILLed;
  the parent then takes it.

### Task 13: `clean` reclaims superseded partials (F17)

**Files:** `backup/clean.go:128-170`, `:262`.

- A local-only partial is superseded (Decision 6) when the destination has a final
  manifest for the same dataset with a newer snapshot, or when the partial's base snapshot
  no longer exists locally. A superseded partial's volumes are deleted along with the
  cache file, under the dataset lock `clean` already takes. Honour `--dry-run`.
- With `--cleanLocal`, add the deleted manifests' datasets to the attributable set, so
  that their volumes go even when no final manifest exists for that dataset.

Tests:

- `TestE2ECleanReclaimsSupersededPartial`: a failed attempt at a, then a smart send of b,
  then plain `clean` deletes `a.zstream.*`, and b still restores.
- `TestE2ECleanLocalReclaimsOrphanOnlyDataset`: `interruptedSend`, then
  `clean --cleanLocal` leaves the destination empty, and a second `clean` succeeds.

### Task 14: small hardening (F21, F22, F23)

- Prepare the `delete://` backend in the destination loop at `backup.go:313-324`, before
  the lock and any goroutine, and keep it out of `jobInfo.Destinations`. Rule, stated in a
  comment: no goroutine starts before everything that can fail has run.
- `VolumeInfo.isOpened`: read and write it under the volume's mutex, or make it an
  `atomic.Bool`. Check with `go test -race` on a pipe-mode e2e test.
- "Total Files Uploaded" counts this run's uploads.
- Resume compares argv without `zfs.ZFSPath`, by comparing `args[1:]`.

Test: `TestE2EPipeModeRaceFree`, the pipe-mode send run under `make test-race`.

### Task 15: docs

- `docs/runbook-first-backup.md`:
  - the new lock location and its same-host, same-`--workingDirectory` scope;
  - the S3 lifecycle rule;
  - what `--resume` now refuses, and why;
  - that smart mode completes a partial set by itself;
  - that `clean` reclaims superseded partials.
- Napkin: close the "receive hangs" entry; note the new fake knobs (`FAKEZFS_STREAM_SALT`,
  `FAKEZFS_SLOW_MS`), the `test-enospc` target, and that cache files are written
  atomically.

---

## Suggested order

Tasks 1, 2, 3, 4 and 5 are the CRITICALs; do them first, in that order. Task 6 should land
before Task 5 if Task 5's cache refresh is to use the atomic writer; otherwise add the
writer in Task 5 and reuse it in Task 6. Do Task 7 early in practice, because every
CRITICAL repro is easier to check when `receive` errors instead of hanging. Task 8 depends
on Task 5. Task 12 replaces Task 3's lock mechanism but keeps its location.
