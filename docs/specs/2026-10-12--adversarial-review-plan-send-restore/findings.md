# Adversarial review: plan -> send -> restore (2026-10-12)

Branch: `clean-dry-run` at `6ae8e7a` plus the uncommitted clean work. Nothing was fixed in
this pass. Four independent reviewers (plan, send, restore, zfs/S3 edges) each worked in a
scratch copy, wrote probe tests and ran them on the host (`make test-run`). Every CONFIRMED
line below was re-run or re-read by the parent session before it was written here.

Probe tests: `tests/*.go.txt` (rename to `.go`, drop into the package named in the file's
header; root-package files need the fake zfs of `e2e_test.go`). Each probe asserts the SAFE
behavior, so a FAIL confirms the finding. Scratch copies with the probes in place:
`/tmp/zb-adv-plan`, `/tmp/zb-adv-send`, `/tmp/zb-adv-restore`, `/tmp/zb-adv-edges`
(`make -C <copy> test-run RUN=TestAdv PKG=<pkg>`; the edges copy has `test-run-to ... TIMEOUT=120s`).

Verdicts: CONFIRMED = a probe fails on the current code, or the trace is unambiguous.
PLAUSIBLE = the fake zfs or MinIO cannot show it; the VM command to decide is given.

## Summary table

| #   | Finding                                                                                                      | Area    | Severity | Verdict                   |
| --- | ------------------------------------------------------------------------------------------------------------ | ------- | -------- | ------------------------- |
| 1   | Explicit `-i` from a re-created source publishes an incremental no restore can apply; exit 0                 | send    | CRITICAL | CONFIRMED                 |
| 2   | Pipe-mode receive deadlocks on any volume of 256 KiB or less                                                 | restore | CRITICAL | CONFIRMED                 |
| 3   | Smart send continues a chain whose middle link is gone at the destination; exit 0                            | plan    | CRITICAL | CONFIRMED                 |
| 4   | One future-dated snapshot backed up pins every later smart run to `nothing-newer`; `--full` does not unpin   | plan    | WARNING  | CONFIRMED                 |
| 5   | `--auto` cannot walk a chain whose links were written under different time zones                             | restore | WARNING  | CONFIRMED                 |
| 6   | `--auto --origin`: the origin is passed to every step, and counts as a snapshot of the target                | restore | WARNING  | CONFIRMED                 |
| 7   | "already exists, nothing to do" never compares the GUID the manifest carries                                 | restore | WARNING  | trace                     |
| 8   | Mount guard of a `-R` stream checks the top dataset only                                                     | restore | WARNING  | CONFIRMED (guard)         |
| 9   | Mount guard looks for the SOURCE dataset only; another dataset at the stream's mountpoint is mounted over    | restore | WARNING  | PLAUSIBLE                 |
| 10  | `-F` has no guard beyond the same-name collision                                                             | restore | WARNING  | PLAUSIBLE (zfs semantics) |
| 11  | A bare `-i` name that starts with the dataset name is mis-trimmed: another snapshot is sent                  | cmd     | WARNING  | CONFIRMED                 |
| 12  | A canceled multipart upload is never aborted: parts leak, `clean` cannot see them                            | S3      | WARNING  | CONFIRMED                 |
| 13  | `s3://user:password@bucket` puts the password into logged SDK network errors                                 | S3      | WARNING  | CONFIRMED                 |
| 14  | Bookmark source + `-R`/`-p`/`-D`/`-I`: planned, dry-run says OK, real zfs refuses every run                  | plan    | WARNING  | PLAUSIBLE                 |
| 15  | `send --dry-run` skips `refuseExistingSet`, `checkManifestFits`, `tryResume`, `refuseMixedChain`             | send    | WARNING  | CONFIRMED                 |
| 16  | `--volsize 0` (or > 10000 x chunk) accepted: one unbounded volume, S3 fails at part 10,001 then retries 12 h | send    | WARNING  | CONFIRMED                 |
| 17  | `zfs send` stderr is dropped on exit 0: `-R -s` never shows the skipped children                             | send    | WARNING  | CONFIRMED                 |
| 18  | Stale lock with a reused pid blocks every send; the message says not to remove it                            | send    | WARNING  | trace                     |
| 19  | Receive retries a local temp-file failure (full disk) for `--maxRetryTime`, re-downloading each time         | restore | WARNING  | CONFIRMED                 |
| 20  | `--fullIfOlderThan 720h` is a strict `>` against sanoid's second jitter: a month with no full                | plan    | NOTE     | CONFIRMED                 |

Notes (not in the table): the chain-walk mismatch Notice names the source dataset, not the
target (`restore.go:155,177`); `--full --raw` over a plaintext full is `already-backed-up`
without saying why; `sameStreamKind` checks `Raw` only, not `-R`/`-p`; the manifest copy of
`copyManifest`/`tryResume` uploads once without backoff (`backup.go:882`); pipe-mode receive
verifies size and SHA-256 after `zfs receive` consumed the volume (documented in the help);
the SDK session has no HTTP timeout, a stalled body hangs until TCP gives up.

## Findings

### 1. CRITICAL. Explicit `-i` from a re-created source snapshot publishes an unrestorable incremental, exit 0

- `backup/backup.go:744-776` `refuseMixedChain` is the only chain check for an explicit
  `-i`. It finds the source's backup with `BaseSnapshot.Equal` (name + creation second,
  `files/jobinfo.go:133-138`), never GUID. A snapshot destroyed and re-created under the same
  name has a new creation time: `source == nil`, an Info line "No backup of a at ...; the
  stream kind cannot be checked there", and the send goes on. `recordSnapshotGUIDs` writes
  the new GUID into the manifest; nothing compares it with the destination's.
- Scenario: full of `tank/data@a` (guid G1) at the destination. `a` is destroyed and re-taken
  (guid G2), `b` is taken. `send -i tank/data@a tank/data@b` exits 0 and publishes
  `tank/data|a|to|b`. `receive --auto tank/data@b`: "could not find parent snapshot". An
  explicit `receive -i` on real zfs: "incremental source guid mismatch". Rolling-name scripts
  (`-i @yesterday @today`) hit this on every run. Smart mode is safe (`source-pruned`).
- Probe: `TestAdvExplicitIncrementalFromRecreatedSource` (`tests/e2e_adv_send_test.go.txt`).
- Fix: when a destination holds a backup whose base has the source's name but another GUID
  (or creation time), refuse; when it holds none, say so at Notice.

### 2. CRITICAL. Pipe-mode receive deadlocks on any volume of 256 KiB or less

- `files/volumeinfo.go:486-546` `Close` takes `v.lock` (488) and then `bufw.Flush()` (543)
  into the `io.Pipe`. `files/volumeinfo.go:375-380` `Extract` needs `v.lock` before the
  reader can start. `backup/restore.go:665-690` hands the pipe volume to the orderer, copies
  the object into it and calls `Close`. When the whole object fits in `bufw`
  (`files.BufferSize`, 256 KiB), no `Write` blocks before `Close`; `Close` wins the lock,
  blocks in `Flush` on a pipe nobody reads; `Extract` blocks on the lock. No ctx cancel fires.
- Scenario: `receive --maxFileBuffer 0` of any set with a small volume: a small incremental,
  or the tail volume of most sets. `zfs receive` waits on stdin forever.
- Probes: `TestAdvPipeReceiveSmallVolumeIntact`, `...Gzip` (hang, stacks at
  `volumeinfo.go:543` and `:378`); control `TestAdvPipeReceiveLargeVolumeIntact` passes
  (`tests/adv_pipe_adv_test.go.txt`).
- Gap: no e2e test runs a pipe-mode receive to success; `TestIntegrationS3/Pipe` uses 1 MiB
  volumes.
- Fix: let the reader mark `extracted` before the writer can reach `Close`, or flush outside
  `v.lock`.

### 3. CRITICAL. Smart send continues a chain whose middle link is gone at the destination

- `backup/plan.go:266-281`: the planner checks only that the newest backup's base is on the
  pool, then `PlanIncremental ... newer-candidate`. It never checks that the newest backup's
  chain reaches a full at the destination. `plan`'s `chain-links` check (`plan_checks.go:119`)
  sees it, `send` runs no checks.
- Scenario: destination holds `F(m1)`, `I(d2<-m1)`, `I(d3<-d2)`. `clean --force` or an
  operator removes the broken set `d2<-m1` (what `TestIntegrationS3ListClean` does, then
  stops). Next smart run: `INCR d4 from d3`, exit 0. d3, d4 and every later daily are
  unrestorable until the next window-elapsed full (up to 180 days with the production flags).
- Probe: `TestAdvBrokenChainIsNotContinued` (`tests/plan_adv_test.go.txt`).
- Fix: when the newest backup's chain (via `parentOf`) does not reach a full, treat it like
  `source-pruned`.

### 4. WARNING. One future-dated snapshot that gets backed up pins every later smart run to a no-op

- `backup/plan.go:258` (`!incrBase.CreationTime.After(lastBackup[0].CreationTime)` ->
  `nothing-newer`); `backup/backup.go:275` `sortBackupsNewestFirst` orders by base creation
  time, so a 2099 backup is always the newest; `plan.go:323` passes while the bad snapshot is
  on the pool.
- Scenario: RTC in 2099 at boot, sanoid takes a `_daily`, NTP fixes the clock, the next run
  sends `INCR 2099_daily`. Every later run: `NOOP nothing-newer`, exit 0, no Warning.
  `--full` sends a full of the newest monthly, the next smart run reads the 2099 backup as
  newest again. Only destroying the 2099 snapshot turns it into `ErrLastBackupInFuture`.
- Probe: `TestAdvFutureDatedSnapshotPoisonsTheChain`.
- Fix: warn or fail when the newest backup is newer than every candidate by more than the
  window; let an explicit `--full` become the chain head.

### 5. WARNING. `--auto` cannot walk a chain whose links were written in different time zones

- `backup/list.go:186-188` and `:210-212` key parent links with `fmt.Sprintf("%s%s%v", ...)`
  of a `time.Time`: `%v` includes zone name and offset. `2026-09-01T00:00:00Z` and
  `2026-08-31T20:00:00-04:00` are two keys. Every other comparison uses `Equal`.
- Scenario: the full sent from an admin shell with `TZ` set, the incrementals from cron
  (system zone), or the job moved to a container (UTC) or another host. Smart sends keep
  chaining; the first `--auto` fails "could not find parent snapshot". The backup looks
  healthy until restore day.
- Probe: `TestAdvAutoChainAcrossTimeZones` (`tests/e2e_adv_restore2_test.go.txt`).
- Fix: key by `CreationTime.Unix()`.

### 6. WARNING. `--auto --origin`: origin on every step, and the origin counts as the target's snapshot

- `backup/restore.go:204-214` copies the per-step fields but never clears `jobInfo.Origin`;
  `zfs/zfs.go:491-494` adds `-o origin=` whenever set. Real zfs refuses a clone origin for an
  existing dataset ("Can't recv a clone into an existing fs", EINVAL): a chain longer than one
  link past the origin fails after step 1 created the clone.
- `backup/restore.go:126-133` appends the origin to the TARGET's snapshot list; `:152-153`
  then finds the target present; `:199-201` prints "already exists" and returns nil. With
  `--origin X` where X has the target's name and time: exit 0, zero receives, nothing restored.
- Probes: `TestAdvAutoOriginPassedToEveryStep`, `TestAdvAutoOriginEqualsTargetSnapshot`
  (`tests/e2e_adv_restore_test.go.txt`).
- Coverage: `ReceiveSafety/Origin` is one explicit link; `--auto --origin` runs only in the
  legacy `TestIntegration`, one link.
- Fix: pass `Origin` to the first item only; use the origin in `sourceIsLocal` and
  `backupThatApplies`, never in the "target exists" test.

### 7. WARNING. "already exists, nothing to do" accepts a local snapshot with the right name and second but another GUID

- `files/jobinfo.go:133-138`, `backup/restore.go:353-356` (explicit), `:152-153`, `:199-201`
  (`--auto`). The manifest carries the GUID (`backup/backup.go:697-704`); receive never reads
  it. `zfs/zfs.go:69` lists `name,creation,type` only.
- Scenario: two hosts run sanoid and both take `autosnap_2026-10-01_00:00:00_monthly` in the
  same second. Restore host A's backup into host B's dataset: "already exists", exit 0,
  nothing of A's data on B. A destroy and re-take within one second gives the same. The fake's
  guid is fnv(name, time), so no fake probe can show it.
- VM check: two datasets, `snapshotTie`-style same-second snapshots of the same name, send
  one, `receive --auto` into the other, expect exit 0 and no data.
- Fix: when `manifest.BaseSnapshot.GUID != ""`, compare with `zfs get guid` of the local
  match before the no-op.

### 8. WARNING. The mount guard of a `-R` stream checks the top dataset only

- `backup/restore.go:574-594` calls `zfs.GetMountpoint(ctx, manifest.VolumeName)` once.
  `zfs send -R` carries every child's local properties.
- Scenario: `tank/data` inherits its mountpoint, `tank/data/home` has `mountpoint=/home`
  (local). `send -R tank/data@a`; `receive` into `restored/data` on the same host without
  `--unmounted`: the guard passes; zfs mounts `restored/data/home` at `/home` over the live one.
- Probe: `TestAdvReplicationMountGuardChecksChildren` (`FAKEZFS_LOG` shows one `get` for the
  parent, none for the child). `TestIntegrationS3Large/Replication` creates children without
  local mountpoints on purpose.
- Fix: when `manifest.Replication`, run the check on every descendant of the source.

### 9. WARNING. The mount guard looks for the SOURCE dataset only

- `backup/restore.go:578-582`: `GetMountpoint(manifest.VolumeName)` fails on a host without
  the source -> `return nil`. The manifest records no mountpoint, so nothing can compare paths.
- Scenario (DR shape): host B has `rpool/home` at `/home`. Restore host A's `tank/home`
  (sent with `--properties`, mountpoint `/home`) into `rpool/restoredA`: mounted over B's live
  `/home`.
- VM check: `zfs create -o mountpoint=/shared tank/srcA`, snapshot, `send --properties`,
  `zfs destroy -r tank/srcA`, `zfs create -o mountpoint=/shared tank/liveB`, `touch
/shared/live.txt`, `receive tank/srcA@s <dest> tank/restoredA`; expect exit 0 and
  `live.txt` hidden.
- Fix: record the source's mountpoint in the manifest at send time; refuse when that path is a
  mounted dataset's mountpoint on the receiving host.

### 10. WARNING. `-F` has no guard beyond the same-name collision

- `backup/restore.go:357-366` is the only `-F` reasoning; `zfs/zfs.go:486-489` passes it
  through. The README example and every integration receive use `-F` as routine. With `-F`,
  zfs (a) replaces an existing target without snapshots, (b) destroys local snapshots newer
  than the incremental source plus every change since, (c) with `-R`, destroys child datasets
  not on the sending side. The fake ignores `-F` (`internal/fakezfs/fakezfs.go:326`).
- `ReceiveSafety` S1 shows only `-F` rolling back a changed file.
- VM checks: (a) `zfs create tank/x; echo live >/tank/x/f; receive -F tank/src@m1 <dest>
tank/x` -> exit 0, `f` gone. (b) restore `m1` into `tank/r`, `zfs snapshot tank/r@mine`,
  `receive --auto -F` -> `@mine` destroyed. (c) `-R` tree in `tank/rr`, `zfs create
tank/rr/scratch`, incremental `-R` with `-F` -> `scratch` destroyed.
- Fix: list what `-F` would destroy and refuse unless a second explicit flag names that loss.

### 11. WARNING. A bare `-i` name that starts with the dataset name is mis-trimmed

- `cmd/send.go:252` and `cmd/receive.go:213`: `strings.TrimPrefix(IncrementalSnapshot.Name,
VolumeName)` without requiring `@` or `#` after it.
- Scenario: pool `tank` with `tank@c`, `tank@tanknight`, `tank@night`; `send -i tanknight
tank@c dest` runs `zfs send -i night tank@c` and records `night` as the source. Without
  `tank@night` it is a loud error.
- Probe: `TestAdvSendIncrementalNameStartsWithVolume` (`tests/adv_edges_adv_test.go.txt`).
- Fix: trim only `VolumeName+"@"` and `VolumeName+"#"`.

### 12. WARNING. A canceled multipart upload is never aborted

- `vendor/github.com/aws/aws-sdk-go/service/s3/s3manager/upload.go:753-766`: `fail()` calls
  `AbortMultipartUploadWithContext(u.ctx, ...)` with the SAME context whose cancel failed the
  parts, so the abort is never sent. `backends/aws_s3_backend.go:236-239` and
  `docs/specs/2026-10-11--sigterm-handling/plan.md:43` claim the opposite.
- Scenario: SIGTERM during a volume over 5 MiB. Parts stay, billed; `ListObjectsV2` does not
  list in-progress uploads, so `clean` never sees them.
- Probe: `TestAdvS3MultipartAbortAfterCancel` (`tests/s3_abort_adv_test.go.txt`): parts seen
  1, abort received false.
- Fix: on a canceled `Upload`, abort with a fresh short-timeout context (the `UploadID` is in
  the `MultiUploadFailure` error); document the lifecycle rule.

### 13. WARNING. `s3://user:password@bucket/...` puts the password into logged SDK errors

- `backends/backends.go:132-139` cuts the bucket as a plain string, so the bucket becomes
  `user:password@bucket`; a network-level SDK error quotes the request URL with it in the path
  (`url.Error` strips userinfo only). `backup/backup.go:234,428,566` log the error raw.
- Probe: `TestAdvS3InitErrorLeaksURIPassword` (`tests/s3_edges_adv_test.go.txt`). The form is
  unsupported but mirrors the supported `ssh://user:pass@host`.
- Fix: reject a bucket name holding `:` or `@` in `Init`.

### 14. WARNING. Bookmark source with `-R`, `-p`, `-D` or `-I`: planned, dry-run says OK, real zfs refuses

- `backup/plan.go:267` passes `includeBookmarks=true`; `backup/sync.go:422` flips
  `source.Bookmark`; `zfs/zfs.go:410-423` builds `-R ... -i tank/data#name`. OpenZFS
  `zfs_do_send` exits 1 with "Unsupported flag with filesystem or bookmark." for a `#` source
  with `-R`, `-I`, `-p`, `-b`, `-h`, `-D`. `backup/backup.go:332` logs the failed estimate at
  Debug, so `send -n` exits 0.
- Related, CONFIRMED (`TestAdvPrefersSnapshotOverSameNamedBookmark`): with `@x` and `#x` both
  on the pool and the bookmark listed first in a tie, the source is marked as a bookmark
  although the snapshot exists (`sync.go:415-425`, first match wins).
- VM check: `zfs bookmark tank/x@a tank/x#a; zfs destroy tank/x@a; zfs snapshot tank/x@b; zfs
send -n -P -R -i tank/x#a tank/x@b; echo $?`; and `zfs list -H -p -t snapshot,bookmark -o
name,creation,type -S creation tank/x` with `@a` and `#a` both present.
- Fix: prefer a snapshot over a same-named bookmark; with `Replication`/`Properties`/
  `Deduplication`/`IntermediaryIncremental` and a bookmark source, take the `source-pruned`
  path; log the dry-run estimate failure at Warning.

### 15. WARNING. `send --dry-run` skips the read-only refusals

- `backup/backup.go:407-408` returns `reportDryRun` before the lock, `refuseExistingSet`
  (`:474`), `checkManifestFits` (`:478`), `tryResume`/`discardPartialManifests`,
  `refuseMixedChain`.
- Scenario: `send -n tank/data@a <uri>` of a set already there: "would perform a full backup",
  exit 0. The real send: "already exists ... refusing to overwrite", exit 255. Same for an
  oversized manifest.
- Probe: `TestAdvDryRunOfExistingSetIsRefused` (`tests/e2e_adv_plan_test.go.txt`).

### 16. WARNING. `--volsize` is not validated

- `files/jobinfo.go:215-256` `ValidateSendFlags` has no `VolumeSize` check;
  `backup/backup.go:1269` underflows for 0, so no cut happens. `backends/aws_s3_backend.go:257`
  passes a non-seekable reader, so s3manager cannot raise `PartSize`; at part 10,001
  `TotalPartsExceeded`, then `backoff.Retry` (`:1662`) for 12 h re-reading the temp file.
  Threshold with defaults: 10,000 x 10 MiB = 97.6 GiB per volume.
- Probe: `TestAdvVolsizeZeroAccepted`.
- Fix: refuse `--volsize 0` and `--volsize > 10000 * --uploadChunkSize`.

### 17. WARNING. `zfs send` stderr is dropped when zfs exits 0

- `backup/backup.go:1206` `cmd.Stderr = buf`; `:1356-1359` read it only on a non-zero exit.
  `--skip-missing` (`cmd/send.go:83`) makes zfs print "WARNING: could not send" and exit 0. A
  tree backup can lack a child for months with no trace.
- Probe: `TestAdvZFSStderrWarningIsLogged`.

### 18. WARNING. A stale lock whose pid was reused blocks every send

- `vendor/github.com/nightlyone/lockfile` `isRunning` = `Signal(0)`; `backup/backup.go:446-456`
  on `ErrBusy` says "Another send or a clean ... is running ... Run again when it is done" and
  suppresses the removal hint. Power loss mid-send, reboot, the pid lands on a daemon; cron
  runs as root so `Signal(0)` succeeds. Every nightly send exits 255 with a message that
  points away from the fix.
- Fix: store the process start time or boot id beside the pid.

### 19. WARNING. Receive retries a local temp-file failure for `--maxRetryTime`

- `backup/restore.go:659-663` (`CreateSimpleVolume` error) and `:676-688` (`io.Copy` error
  in file mode) return plain errors; only the pipe case is `backoff.Permanent`.
  `downloadSequenceVolume` retries for 12 h, re-downloading the object every time.
- Probe: `TestAdvTempFileErrorIsPermanent` (8 downloads of one object in 1.5 s).
  `make test-enospc` covers send only.

### 20. NOTE. `--fullIfOlderThan 720h` is a strict `>` against sanoid's second jitter

- `backup/plan.go:227`. After a 30-day month the age on the 1st is 720h plus or minus seconds:
  April full at :05, May monthly at :04 or :05 -> `INCR` (no full that month, a 61-day chain);
  at :06 -> `FULL`. `TestAdvWindowBoundaryFlipsOnOneSecond` records the flip.

## Refuted (where the code handles it)

- `zfs send` exits non-zero after a complete stream, both modes: `backup.go:1353-1362`
  (`Wait`, then `cout.CloseWithError(werr)`); `close(c)` only after `group.Wait()` is nil.
  Probe `TestAdvZFSExitNonZeroAfterFullStream/{Buffered,Pipe}` passes.
- `zfs receive` exits non-zero after consuming the stream: `restore.go:793-803`, "Done" only
  after `wg.Wait()==nil`. Probe `TestAdvReceiveStreamReportsNonZeroExit` passes.
- Manifest before the last volume at destination 2, out-of-order volumes with
  `MaxParallelUploads > 1`: `pendingVolumes` gates the manifest on the LAST link; `saveManifest`
  sorts. Probe with 4 workers, 2 destinations, 8 volumes passes.
- `delete://` racing a retry: chain, not fan-out; the delete link is last.
- SIGTERM between the last volume and the manifest: cached with `EndTime`, never uploaded;
  `--resume` uploads it. Probe passes.
- Hash mismatch on the last volume after earlier ones were consumed (buffered): the errgroup
  ctx SIGKILLs `zfs receive` (`exec.CommandContext`), no `-s`, zfs destroys the partial.
  Real zfs: `EncryptedResume` tampered volume -> no dataset.
- `--auto` matching the source by name only; `-d`/`-e` target names; `--origin` on another
  pool; substituted manifests; two manifests for one snapshot; Glacier `PreDownload` with ctx
  cancel; complete-partial of a partial with another source (`sameSets`); manifests of
  `tank/data2` and `tank/data/child` (`manifestMayBeFor` + `VolumeName`); `--resume` with
  another `--volsize` (harmless); `--manifestPrefix` with `|`, `/`, trailing separator; odd
  dataset names (tabs and newlines are not legal in ZFS, spaces survive); key encoding of `|`,
  `@`, `#`, `+`, space on MinIO and AWS; `tank/data` prefix listing matching `tank/data2`
  (prefix ends in `/`, volume listing is exact-match).

## Coverage gaps (what no test exercises, any zfs)

Plan: a broken chain at the destination then a smart send (3); a future-dated snapshot on the
pool (4); `@x` and `#x` both present; bookmark source with `-R`/`-p`/`-I` (14); `-R` with
smart selection at all; `--resume` + smart on real zfs; `source-pruned`, `already-backed-up`,
out-of-sync fulls, `ErrLastBackupInFuture` and `--increment` on real zfs; 3+ destinations;
zero snapshots; `send --dry-run` of an existing set (15).

Send: the S3 upload retry path against a failing S3 (no S3 fault injection exists; multipart
abort, leaked parts, `TotalPartsExceeded`); `zfs send` exit non-zero or stderr warnings on
real zfs; `-R --skip-missing`, `-I`, `-p`, `-D`, `-R` incremental, `-R --raw`, `-R` in pipe
mode; `--resume` in pipe mode or with two S3 destinations; SIGTERM on real zfs/S3; external
compressors on S3; `--maxUploadSpeed`; archive storage classes on real S3; lock pid reuse;
`--volsize` extremes.

Restore: a pipe-mode receive that SUCCEEDS on the fake (2); SIGTERM during `zfs receive`;
ENOSPC during receive (19); Glacier on real S3; `--auto` across time zones (5); a GUID
mismatch under a same-second tie (7); `-F` destroying newer snapshots, a snapshot-less
target, `-R` children (10); `--auto --origin` beyond one link, `--origin` equal to the target
(6); `-R` children with local mountpoints (8); the restore-host mountpoint collision (9);
`-R` incremental and `-R --auto`; explicit `-d`/`-e` on real zfs; `--separator` other than
`|` in the VM suite.

Fake zfs limits that hide the above: `receive` ignores `-F`, `-u`, `-o origin=`, accepts an
empty, truncated or corrupt stream, exits 0 early on nothing; `send` accepts `#` with every
flag; guid is fnv(name, creation); `get mountpoint` is always `/<dataset>`; `-S creation` ties
keep fixture order. MinIO has no Glacier semantics; the whole `PreDownload` path is covered
by in-process fakes only.

## Suggested order

1. Findings 2 and 1 (a restore that hangs, a backup that cannot be restored), then 3 and 4
   (silent no-backup in cron). All four have host-runnable probes.
2. Findings 5, 6, 11 (small fixes, probes exist).
3. VM day: 7, 8, 9, 10, 14 as integration tests before any code change.
4. 12, 13, 15 to 19.

## Second review of the fixes (2026-10-13)

An independent reviewer read the staged fixes. Fixed the same day, each with a test that fails
without the fix (break copies):

- W1. Pipe-mode receive hung when `zfs receive` exited early or `Extract` failed: the writer
  waited on the pipe, `wg.Wait()` forever. `receiveStream` now calls `vol.Abort(err)`;
  `processSequence` aborts its pipe volume when the errgroup ctx is canceled
  (`TestReview2PipeReaderFailureAbortsWriter`, `TestReview2PipeCanceledRestoreAbortsWriter`).
- W2. `receive --auto -F` of a `--replication` chain received the full, then refused step 2.
  `AutoRestore` now runs `refuseDestroy` for the whole chain before the first receive. The `-R`
  rule is narrower: refused only when the target, before the run, has snapshots other than the
  source or datasets below it (`TestE2EReview2AutoForceReplicationChain`).
- W3. `SourceMountpoint` (top only) became `SourceMountpoints` (dataset -> path), with the `-R`
  children (`TestE2EReview2MountGuardRecordsReplicatedChildren`).
- W4. `send --dry-run` (and a smart dry run, and `plan`) called `PreDownload`: an S3 Glacier
  restore. Now `backends.Archiver.NotReadable` (HEAD only); an archived manifest is skipped with
  a Notice. `getBackupsForTarget` closes its backend (`TestE2EReview2DryRunStartsNoRestore`).
- N1. A snapshot of another name in the source's second counts as newer; a target with only
  bookmarks counts as "no snapshots" for a full (`TestReview2RefuseDestroy`). Bookmarks are not
  counted as a loss: they hold no data.
- N5. `--increment` with nothing newer and a broken chain: NOOP with the broken-chain Warning
  (`TestReview2IncrementNothingNewerWarnsOfBrokenChain`).

VM check still to run: `receive --auto -F` of a real `-R` chain into a new dataset (zfs makes the
children in step 1), and a `-R -F` incremental into a target whose older snapshot the sender pruned.

## Third review (2026-10-13)

An adversarial review (restore, send, plan) of the uncommitted fixes. R1-R3 are fixed, each with a
test that fails without the fix (break copy, `backup/review3_test.go`):

- R1. `receive -F` read any `zfs list` error of the target as "does not exist", so `refuseDestroy`
  let a full replace a target with data and no snapshots (EPERM, a parse error). Now
  `refuseUnlistedTarget` refuses with `-F` unless the error is `zfs.IsNotExist` or `--allowDestroy`
  is given (`TestReview3RefuseUnlistedTarget`).
- R2. `send -n` and `plan` dropped an archived manifest that was not cached: the planner saw a hole
  in the chain and planned a `broken-chain` full that the real send would not. Now
  `syncCacheMode(noRestore)` fails with `archivedManifestsError` when the manifest may be of the
  dataset; archived manifests of other datasets are skipped (`TestReview3DryRunArchivedManifest`,
  `TestE2EReview2DryRunStartsNoRestore` now wants the error). This replaces W4's Notice.
- R3. Finding 18 (stale lock) needed `/proc/sys/kernel/random/boot_id`: no effect on FreeBSD or
  macOS. There the boot id is now `kern.boottime` (`backup/bootid_bsd.go`). illumos and a container
  restart without a host reboot still keep the old lock (`TestReview3BootLockUsesBootTime`).

Open from that review (not fixed): `--auto` runs the mount guard per step; the guards trust
`Replication`/`Properties` of an unsigned manifest; no VM run of the `-F` refusal; no test of
the `-i` GUID clause; `ValidateVolumeSize` checks `s3://` only (B2 has 10,000 parts too); the
`ValidateVolumeSize` comment says 0 is unlimited; `plan` CLI uses the real clock but goldens use
the newest time; `plan` has no `-R`/`-p`/`-D`; a broken-chain NOOP exits 0; `brokenLink` reads
`backups[0]` only, so the order of the listing can decide.
