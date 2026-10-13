# Integration tests for `clean`: implementation plan

Date: 2026-10-12
Branch: `clean-dry-run`
Source: `intent.md` in this directory. All ten scenarios, the optional four
included.

## Goal

Four new `TestIntegrationS3Clean*` tests in the real-ZFS + MinIO environment
of `make integration-vm` (`make integration-docker` on Linux). The default
`RUN=TestIntegrationS3` pattern of the entrypoint runs them.

| Test                              | Scenarios                                                                                     | File                                 |
| --------------------------------- | --------------------------------------------------------------------------------------------- | ------------------------------------ |
| `TestIntegrationS3CleanLocal`     | 1 (`--cleanLocal`), 7 (busy dataset, a step of 1), 6 (subtest `Repair`), 9 (subtest `Denied`) | `integration_s3_cleanlocal_test.go`  |
| `TestIntegrationS3CleanEncrypted` | 2                                                                                             | `integration_s3_cleanenc_test.go`    |
| `TestIntegrationS3CleanLayout`    | 3, then subtests `Separator` (4), `Legacy` (8), `Versioned` (10)                              | `integration_s3_cleanlayout_test.go` |
| `TestIntegrationS3CleanMany`      | 5, subtest `Interrupted`                                                                      | `integration_s3_cleanmany_test.go`   |

Every scenario ends with a `receive` into an empty working directory and a
GUID check, so "clean kept what the manifests need" is proved by zfs.

## Facts that shape the plan

Checked in the code on 2026-10-12. Do not re-derive them.

1. **The `--force leaves it alone` Notice needs a missing volume.** The
   switch at `backup/clean.go:514` logs `The local manifest %s is missing
volume %s (an interrupted send lists only what it uploaded); --force
leaves it alone, --cleanLocal deletes it.` only when a volume the cached
   manifest lists is NOT at the destination. A cut send caches exactly the
   volumes it uploaded (`cutPartial` in `integration_s3_resume_test.go`
   asserts that), so `clean --force` on a fresh partial logs only `There are
1 local manifests not found in the destination, use --cleanLocal ...`
   (line 386). Scenario 1 step 2 as written in `intent.md` cannot pass. The
   plan keeps step 2 with the line 386 Notice and adds a step 6: a second cut
   send, one of its volumes deleted by hand, then `--force` gives the line
   514 Notice.
2. **What `--cleanLocal` and `--force` delete in the cache.** A local-only
   manifest goes to `localDeletes` (line 401) only with `--cleanLocal` and
   only when its dataset is not busy (`busy[local.manifest.VolumeName]`
   keeps it live, line 395). A forced set's cached copy goes to
   `localDeletes` too (line 544). Cache files are removed after every object
   delete succeeded (line 612+). The cache is per destination URI
   (`cacheDirFor(uri)`, `<work>/cache/<md5(uri)>/<md5(name)>`): `clean
--force` at `d2` cannot touch `d1`'s cached copy.
3. **Busy dataset.** `volumeLock` (`backup/backup.go:375`) is
   `<config.WorkingDir>/locks/<hex md5(dataset)>.lck`; a live pid inside
   makes `TryLock` fail and clean logs `A send of %s appears to be running
(%s: %v); leaving its volumes and its cached manifest alone.` (line 309).
   The unit test writes `os.Getppid()` into it (`backup/clean_test.go:223`).
   In the integration binary the parent is the entrypoint shell: alive.
   `config.WorkingDir` is the `--workingDirectory` of the run, so the test
   builds the path itself with `crypto/md5`.
4. **Encrypted manifest, no key.** `readCachedManifest` fails through
   `unreadableManifestError` (`backup/sync.go:356`). With a `KeyError` whose
   `NeedsSecretKey` is set the text is `could not read the manifest %s at the
destination: ... pass the secret key ring that holds that key with
--secretKeyRingPath. Do not delete it unless you know it is not a backup.
Its copy stays in the cache, %s`; `keepsCachedCopy` is true, the cached
   copy stays. `readForClean` returns that error before any delete
   (`clean.go:170`). `--force` changes nothing there: the manifests are read
   before sets are judged. The third branch of that switch (`delete that
object from the destination`) must NOT appear for an encrypted manifest:
   if it does, that is a finding (wrong advice), not a test to weaken.
5. **Refusal order.** Objects but 0 decoded manifests refuses at line 338,
   before nested destinations are computed (line 430). From the bucket root
   `run/manifests|...` does not start with `manifests|`, so the root has 0
   manifests and refuses. `validateCleanFlags` (`cmd/clean.go`) rejects a
   comma with `clean takes one destination, got %s.` before any backend
   is built.
6. **Nested destinations.** `nestedDestinations` roots are every
   `<dir>/` whose objects hold `/<manifestPrefix>...manifest`. An object
   under a root that no manifest here names logs `Skipping %s: %s holds its
own manifests, so it is another destination.` (line 451); that includes
   the nested manifest itself (it does not start with the prefix). A volume
   whose dataset no manifest here names logs `Skipping %s: no manifest at
this destination is for dataset %s.` (line 464).
7. **Separators.** `clean` takes `files.DefaultSeparator`, `jobInfo.Separator`
   and the separator of every decoded manifest (line 352). `isManifestObject`
   (`sync.go:255`) accepts a manifest of our prefix written with another
   separator, so `list`, `syncCache` and `clean` read `manifests#...` without
   a flag. `send` and `receive` have `--separator`; `clean` and `list` do not.
   `#` is not a dataset character, so it is a valid separator. If `send`
   refuses `#`, use `%`.
8. **Legacy key.** `legacyPrefixes` (`backends/backends.go:162`) gives `p`
   for `s3://bucket/p/`; `FindLegacyVolume` looks for the first key
   `p<dataset><sep>` whose rest parses as a volume name. The fake test
   `TestCleanS3Prefixes/legacy volume without a manifest` plants
   `ptank/data|<snap>.zstream.gz.vol1` and expects the Warning `looks like a
backup volume written by an older version` (`clean.go:420`). The real key
   to plant is `<prefix without slash>` + a real volume name.
9. **Listing.** `AWSS3Backend.List` pages with `ContinuationToken`
   (`backends/aws_s3_backend.go:663`); `objectNames` pages too. But the
   bucket cleanup of `setupS3Bucket` (`integration_test.go:129`) calls
   `ListObjects` once (at most 1000 keys) and `DeleteObjects` once, then
   `DeleteBucket`. With 1100 objects left it fails with `BucketNotEmpty`,
   and with versioning on it leaves versions and delete markers. Task 1
   fixes it.
10. **Deletes.** 5 workers, `backoff.Retry` with `MaxElapsedTime` 10 min
    (`clean.go:578`). `AWSS3Backend.Delete` returns the SDK error as is: an
    `AccessDenied` is retried for 10 minutes. cenkalti/backoff v2 returns
    the LAST operation error when the context ends (napkin), so a run under
    a context with a deadline fails with `AccessDenied` in its log. Scenario 9
    uses that. The 10-minute retry of a 403 is a finding to report in the
    execution notes, not to fix here.
11. **Noop.** A smart send with nothing new returns `backup.ErrNoOp` from
    `itSend` (`integration_s3_test.go:110`).
12. **Signals in process.** `e2e_signal_test.go:39-80` is the pattern:
    `cmd.SignalContext` first, the run in a goroutine under that context,
    ONE `syscall.Kill(os.Getpid(), SIGTERM)`, never a second signal.
    `cleanS3Ctx` (`clean_s3_test.go:273`) runs clean under a context;
    `itRunOut` does not take one yet.
13. **MinIO client image.** `pgsty/mc` exists on Docker Hub (checked with
    `docker manifest inspect pgsty/mc:latest` on 2026-10-12). `minio/mc`
    does not. Pin a dated tag, as `compose.yaml` pins `pgsty/minio`.
14. **Versioning.** MinIO single-drive accepts `PutBucketVersioning`
    since 2022. If the server answers `NotImplemented`, the subtest skips
    with the error in the log; it must not pass silently.
15. **In-process runs leak.** `itClean` passes `--dry-run=false
--force=false --cleanLocal=false`; a value in args wins. Rings and
    `--manifestPrefix` go after. `cmd.ResetReceiveJobInfo` is the reset for
    clean. Two sends that must give equal volumes: `--compressor ""`.

## Assumptions

- `zfs send` of one snapshot gives the same bytes each run (holds on zfs
  2.4.4, napkin), so a cut send's partial lists 2 full volumes at
  `--volsize 1 --compressor ""` with a 2.5 MiB cut (`flakyZFS(t, 5<<19, 5)`).
- MinIO deletes about 1000 small objects per second with 5 workers. The
  `Interrupted` subtest plants 3000 so the signal lands mid-run.
- A MinIO user with no `s3:DeleteObject` gets `AccessDenied` on
  `DeleteObject` and can still list and get. If `mc admin` is not in the
  `pgsty/mc` image, scenario 9 skips (its env is unset) and the execution
  notes say so.

## Rules for this repo

Read `.claude/napkin.md` first. Those that matter here:

- Makefile only. `make integration-image` compiles and vets the tagged test
  on the Mac: the fast loop. `make integration-vm RUN=TestIntegrationS3CleanLocal`
  runs one test; a subtest is `RUN=TestIntegrationS3CleanLocal/Repair`.
- Find code with `semble search` and `cs` (`.claude/skills/codesearch/SKILL.md`).
- A test that passes the first time proves nothing. Each task names its
  break proofs; run them in a scratch copy (`rsync -a --exclude .git
--exclude tmp ./ /tmp/zb-break/`, then `make -C /tmp/zb-break
integration-vm RUN=...`; the VM sees `~` only, so put the copy under
  `~/.cache/` instead of `/tmp`).
- Assert on bucket and zfs state before log lines. Log lines only where the
  behavior is not visible elsewhere (`Would delete`, the Notices of fact 1).
- Never shape production code around a test environment's limit.
- New tests in new files, `//go:build integration`, `package main`. Stage
  by name after `git status`. `make fmt-check` sees a new file after `git add`.
- One `CHANGELOG.md` line per task a user can see: `* [ADD] test: ...`.
- `t.Cleanup` is LIFO: register a cleanup that must run before
  `destroyAfter` after it.

## Layout

```
integration_test.go                  setupS3Bucket cleanup pages and deletes versions (task 1)
integration_s3_test.go               itRunCtx, itCleanCtx, cachedFiles, orphanOf, putMany, wantInLog, wantNotInLog, wantObjectsAt (task 1)
integration_s3_resume_test.go        cutSend extracted from TestIntegrationS3Resume's steps (task 1)
integration_s3_cleanlocal_test.go    TestIntegrationS3CleanLocal + Repair, Denied (task 2)
integration_s3_cleanenc_test.go      TestIntegrationS3CleanEncrypted (task 3)
integration_s3_cleanlayout_test.go   TestIntegrationS3CleanLayout + Separator, Legacy, Versioned (task 4)
integration_s3_cleanmany_test.go     TestIntegrationS3CleanMany + Interrupted (task 5)
integration/compose.yaml             mc service: user nodelete; env for the zfs service (task 6)
integration/nodelete-policy.json     the policy without s3:DeleteObject (task 6)
integration/README.md                test table, RUN examples, the mc service (task 7)
docs/context/testing.md              "fourteen" -> "eighteen", layer 4 text (task 7)
docs/context/clean.md                Tests section: the integration tests (task 7)
CHANGELOG.md                         one line per task
.claude/napkin.md                    facts 1, 9, 10 as one-liners (task 7)
```

## Task 1: shared helpers

Files: `integration_test.go`, `integration_s3_test.go`, `integration_s3_resume_test.go`.

- `setupS3Bucket` cleanup: `ListObjectVersionsPages` (versions and delete
  markers; an unversioned bucket lists each object as the null version), a
  `DeleteObjects` per page with `VersionId`, then `DeleteBucket`. Report
  each failure with `t.Errorf`, as now.
- `itRunCtx(ctx, reset, command, work, zfsPath, args...)`: `itRunOut`
  under a context. Read how `e2eEnv.sendCtx` and `cleanS3Ctx` pass the
  context to `cmd.Run` and do the same; `itRunOut` becomes a call with
  `context.Background()`. `itCleanCtx(ctx, t, work, args...)` wraps it with
  the three bool flags, as `itClean` does.
- `cachedFiles(t, work) []string`: sorted paths of `<work>/cache/*/*`.
- `orphanOf(t, client, bucket, prefix, vol1 string, n int) string`: fails
  unless `vol1` ends in `vol1`, puts `"orphan"` at `prefix + <name with
vol<n>>`, returns the name. The inline versions in
  `integration_s3_ops_test.go` and `integration_s3_prefix_test.go` stay.
- `putMany(t, client, bucket string, keys []string, data []byte)`: 16
  goroutines, `errgroup`, one `PutObject` per key.
- `wantInLog(t, logs string, want ...string)`, `wantNotInLog(t, logs
string, unwanted ...string)`, `wantObjectsAt(t, client, bucket, prefix
string, want []string)` (`t.Fatalf` with both listings). Names differ from
  the closures in the ops test on purpose.
- `cutSend(t, work, target, src, snap string, extra ...string) []string` in
  `integration_s3_resume_test.go`: `flakyZFS(t, 5<<19, 5)`, `arm()`, the
  send with `--volsize 1 --compressor ""` plus `extra`, the `flaky zfs`
  assertion, then `cutPartial` against `objectNames` before and after; fails
  unless the partial lists 2 volumes. `TestIntegrationS3Resume` keeps its
  own body (it checks `ZFSStreamBytes` of the partial, which `cutSend` need
  not).

Check: `make integration-image` builds; `make integration-vm
RUN='TestIntegrationS3(Resume|ListClean|Large)$'` still passes (the cleanup
change runs in every test).

## Task 2: `TestIntegrationS3CleanLocal` (scenarios 1, 7, 6, 9)

File: `integration_s3_cleanlocal_test.go`. Bucket `zfsbackup-it-cl`.
Datasets `srccl`, `srccl2`, `restoredCl`. One `work`.

Setup:

1. `srccl`: 64 KiB `first.bin`, snapshot `first`, `send --volsize 1
--compressor "" srccl@first target`. `srccl2`: 64 KiB, snapshot, a full
   the same way. `healthy := objectNames(...)`.
2. 4 MiB `a.bin` on `srccl`, snapshot `daily`, `partial := cutSend(...)`
   (2 volumes). `cache := cachedFiles(t, work)`; exactly one of them is
   newer than the two fulls' files: the partial's path (record it as
   `partialPath`; the simplest way is `cachedFiles` before and after the cut).

Steps (each `wantObjectsAt` is the whole bucket):

1. `clean` (no flags): no error; `wantInLog` `There are 1 local manifests
not found in the destination, use --cleanLocal`; objects unchanged;
   `cachedFiles` unchanged.
2. `clean --force`: the same Notice, the same unchanged state. `wantNotInLog`
   `Removing entire backupset`.
3. Scenario 7, busy: write `os.Getppid()` + `"\n"` to
   `<work>/locks/<hex md5(srccl)>.lck` (mode 0600, `t.Cleanup` removes it);
   `orphan2 := orphanOf(... manifestFor(srccl2's full).Volumes[0] ..., 77)`;
   `clean --cleanLocal`: no error; `wantInLog` `A send of <srccl> appears to
be running`; `partial` volumes and `partialPath` still there; `orphan2`
   gone. Remove the lock now (not at cleanup).
4. `clean --cleanLocal --dry-run`: `wantInLog` `Would delete local manifest
<partialPath>.` and `Dry-run: would delete 2 objects`; objects and
   `cachedFiles` unchanged.
5. `clean --cleanLocal`: objects == `healthy`; `partialPath` gone;
   `requireVolumes` for the two fulls.
6. `clean --dry-run`: `would delete 0 objects`. `send --volsize 1
--compressor "" -i srccl@first srccl@daily target`; `list` shows
   `first<-`, `daily<-first`; `receive --auto -F srccl target restoredCl`
   with `t.TempDir()` as work; `wantSnapshots`, `wantGUIDs`, `compareDirs`.
7. Scenario 1 step 2 as the intent meant it: 4 MiB `b.bin`, snapshot
   `weekly`, `partial2 := cutSend(...)`; `deleteObject(partial2[1])`;
   `clean --force`: no error; `wantInLog` `The local manifest <path> is
missing volume <partial2[1]>` and `--force leaves it alone, --cleanLocal
deletes it`; `partial2[0]` still there; the cache file still there.
   `clean --cleanLocal`: `partial2[0]` gone, the file gone, objects == the
   listing after step 6; `requireVolumes` for `first` and `daily`.

Subtest `Repair` (scenario 6). Bucket `zfsbackup-it-clrep`, prefixes `d1/`
and `d2/` as `TestIntegrationS3TwoDestinations`, dataset `srcclrep`,
`restoredClRep`, its own `work`. Smart flags of the two-destination test
(`_monthly` for both suffixes, `--volsize 1 --compressor ""`).

1. 3 MiB, `m1`, send to `d1,d2`. 1 MiB, `m2`, send. `wantSame` as in that
   test (copy the closure; four lines).
2. `m2Set := manifestFor(work, src, d1, m2)`; `deleteObject(p2 +
m2Set.Volumes[0].ObjectName)`. The manifest stays: a broken set is a
   manifest with a missing volume.
3. `clean --force d2`: no error; `wantInLog` `Removing entire backupset`;
   at `d2` no name contains `m2`; `d1` unchanged; `itList(work, d2)` shows
   `m1<-` only (the cached copy is gone: the cache is what `list` reads for
   a cached name, napkin).
4. `send --volsize 1 --compressor "" -i src@m1 src@m2 d2`: no error;
   `wantSame()`.
5. Smart send to `d1,d2`: `errors.Is(err, backup.ErrNoOp)`.
6. `receive --auto -F src d2 restoredClRep` with an empty work;
   `wantSnapshots` `m1`, `m2`; `wantGUIDs`; `compareDirs`.

Subtest `Denied` (scenario 9). Skips unless `ZFSBACKUP_IT_NODELETE_KEY` and
`ZFSBACKUP_IT_NODELETE_SECRET` are set (task 6 sets them in compose; a real
AWS run has them unset). Bucket `zfsbackup-it-clden`, dataset `srcclden`.

1. A full of `first`; `cutSend` of `daily` (2 volumes); `before :=
objectNames`, `files := cachedFiles`.
2. In an inner `t.Run("as nodelete", ...)` (so `t.Setenv` restores on
   return): `t.Setenv("AWS_ACCESS_KEY_ID", key)`, the secret too;
   `ctx, cancel := context.WithTimeout(ctx, 20*time.Second)`;
   `itCleanCtx(ctx, ... "--cleanLocal", target)`: `err != nil`; `wantInLog`
   `Could not delete object` and `AccessDenied`; objects == `before`;
   `cachedFiles` == `files`.
3. With the root credentials: `clean --cleanLocal`: the 2 volumes gone, the
   file gone; `requireVolumes` for `first`; `receive -F src@first target
restoredClDen` with an empty work; `wantGUIDs`.

Break proofs (scratch copy, each must fail): remove the `busy[...]` check at
`clean.go:395` (step 3 deletes the partial); make `--force` delete local-only
sets (drop `|| localOnly` at line 512; step 2 or 7 deletes); skip
`localDeletes` removal (step 5 keeps the file); `Repair` without the
`clean --force` (step 4's send fails or step 5 is not a noop); `Denied` with
the root key in the inner run (no error).

## Task 3: `TestIntegrationS3CleanEncrypted` (scenario 2)

File: `integration_s3_cleanenc_test.go`. Bucket `zfsbackup-it-clenc`,
`restoredClEnc`.

1. `rings := writeRings(t, newKey(t))`; `c := buildChain(t, pool, work,
target, "clenc", rings...)`; `orphan := orphanOf(... manifestFor(work,
c.src, target, c.daily3, rings...).Volumes[0].ObjectName, 77)`;
   `littered := objectNames`.
2. `fresh := t.TempDir()`; `clean` with `fresh` and no rings: `err != nil`;
   `wantInLog` `could not read the manifest`, `--secretKeyRingPath`,
   `Do not delete it unless you know it is not a backup`; `wantNotInLog`
   `delete that object from the destination`; objects == `littered`;
   `len(cachedFiles(fresh)) >= 1` (the copy stays).
3. `clean --force` with `fresh`, no rings: the same four checks.
4. `wrong := writeRings(t, newKey(t))`; `clean` with `wrong...`: the same.
5. `clean` with `work` and `rings...`: no error; `orphan` gone; the rest ==
   `littered` minus the orphan; `requireVolumes` for the three links
   (`manifestFor` with rings); `receive --auto -F` with rings and an empty
   work into `restoredClEnc`; `wantSnapshots` monthly, daily2, daily3;
   `wantGUIDs`; `compareDirs`.

Break proofs: let `readForClean` log and skip an unreadable manifest instead
of returning (steps 2-4 delete the orphan, and worse); `keepsCachedCopy`
returning false (the cache check of step 2); step 5 without rings.

## Task 4: `TestIntegrationS3CleanLayout` (scenarios 3, 4, 8, 10)

File: `integration_s3_cleanlayout_test.go`. Bucket `zfsbackup-it-clay`.
Datasets `srclay` (3 MiB: 3 volumes), `srclay2` (64 KiB), `restoredLay`,
`restoredLay2`. Prefixes `run/`, `run/sub/`, `other/`; targets
`s3://<bucket>/<p>`. One `work`.

1. Fulls: `srclay@m` to `run/` and to `run/sub/`; `srclay2@m` to `other/`.
   The two sets of `srclay` have the same object names (fact: names hold no
   prefix), so `name := manifestFor(work, srclay, run, m).Volumes[0]` gives
   the orphan name for every prefix: `orphanOf` at `run/`, `run/sub/`,
   `other/`, all `vol77`. `littered := objectNames(bucket, "")`.
2. `clean run/`: no error; the `run/` orphan gone; the other two there;
   `wantInLog` `Skipping sub/` and `: sub/ holds its own manifests, so it is
another destination.` (logged for every `sub/` object, its manifest
   included); `wantNotInLog` `other/`.
3. `clean run/sub/`: its orphan gone; `other/` untouched.
4. `clean other/`: no error; its `srclay` orphan stays; `wantInLog`
   `Skipping <name>: no manifest at this destination is for dataset <srclay>`.
5. `clean s3://<bucket>/`: `err != nil`; `wantInLog` `objects but no
manifests; refusing to clean`; objects unchanged.
6. `clean run/,other/`: `err != nil`; `wantInLog` `clean takes one
destination, got`; objects unchanged.
7. `requireVolumes` for the three manifests against each prefix's listing;
   `receive -F srclay@m run restoredLay` and `receive -F srclay2@m other
restoredLay2`, empty works; `wantGUIDs`.

Subtest `Separator` (scenario 4). Bucket `zfsbackup-it-clsep`, `srclsep`
(3 MiB), `restoredClSep`.

1. `send --separator '#' --volsize 1 --compressor "" srclsep@m1 target`;
   1 MiB more, `m2`; `send --separator '#' ... -i srclsep@m1 srclsep@m2`.
   Every name holds `#` and no `|`; `manifests#` is the manifest prefix.
2. `clean --dry-run`: `would delete 0 objects`; `wantNotInLog` `Skipping
unrecognized object`.
3. `set := manifestFor(work, src, target, m2).Volumes[0].ObjectName`;
   `orphanOf(set, 77)` (a `#` name); `putObject(prefix +
strings.ReplaceAll(<that name>, "#", "|"))` (a `|` name of the same set).
   `clean`: both gone; the manifests there; `requireVolumes` for `m1`, `m2`.
4. `receive --auto -F --separator '#' src target restoredClSep`, empty work;
   `wantSnapshots` `m1`, `m2`; `wantGUIDs`.

Subtest `Legacy` (scenario 8). Bucket `zfsbackup-it-clold`, prefix `p/`,
`srclold` (64 KiB).

1. A full at `s3://<bucket>/p/`. `vol1 :=` its first volume name;
   `putObject(bucket, "p"+vol1, "old")` (bucket-absolute key, outside `p/`).
2. `clean --dry-run s3://<bucket>/p/`: no error; `wantInLog` `Found p<vol1>,
which looks like a backup volume written by an older version outside of`;
   `would delete 0 objects`.
3. `clean`: the key `p<vol1>` still there (`objectNames(bucket, "")`
   contains it); the set intact; `receive` works.

Subtest `Versioned` (scenario 10). Bucket `zfsbackup-it-clver`, `srclver`
(64 KiB), `restoredClVer`.

1. `PutBucketVersioning{Status: Enabled}` right after `itBucket`; on an
   error whose code is `NotImplemented`, `t.Skipf` with it; any other error
   fails.
2. A full; `orphanOf(..., 77)`; `clean`: the orphan gone from
   `objectNames`; `ListObjectVersions` shows at least one delete marker
   (that is what proves versioning was on); `list` shows one backup;
   `clean --dry-run`: `would delete 0 objects`; `receive -F` into
   `restoredClVer`, empty work; `wantGUIDs`.

Break proofs: drop the nested-root check (step 2 deletes `sub/`'s orphan,
and the `sub/` manifest's volumes look like orphans too); drop the refusal
at `clean.go:338` (step 5); `--separator '#'` manifests not read as ours
(a change in `isManifestObject`: step 2 refuses, 0 manifests); `Legacy` with
the Warning removed; `Versioned` with the versioning call removed (no delete
marker).

## Task 5: `TestIntegrationS3CleanMany` (scenario 5)

File: `integration_s3_cleanmany_test.go`. Bucket `zfsbackup-it-clmany`,
`srclmany` (3 MiB), `restoredClMany`.

1. A full with `--volsize 1 --compressor ""`: 3 volumes, 1 manifest
   (`countObjects`). `vol1 :=` its first volume name.
2. `putMany` of 1100 keys `prefix + <vol1 without "vol1"> + "vol<100..1199>"`
   with 8 bytes each. `len(objectNames) == 1104` (the helper pages).
3. `clean --dry-run`: `wantInLog` `Dry-run: would delete 1100 objects`;
   `wantNotInLog` `missing volume`, `Pass the --force flag`.
4. `clean`: `wantInLog` `Starting to delete 1100 objects`; `objectNames` ==
   the 4 originals; `requireVolumes`; `list` one backup; `receive -F` into
   `restoredClMany` with an empty work; `wantGUIDs`; `compareDirs`.

Subtest `Interrupted` (the intent's optional step 3). Same bucket, after
step 4.

1. `putMany` 3000 keys `vol100..vol3099`.
2. `ctx, stop := cmd.SignalContext(context.Background())`; `defer stop()`;
   `itCleanCtx(ctx, ...)` in a goroutine with a result channel, as
   `e2e_signal_test.go:50-63` does.
3. Poll `objectNames` every 50 ms until the count is below 3004 (a delete
   happened), then ONE `syscall.Kill(os.Getpid(), syscall.SIGTERM)`. If the
   run returns before the signal, fail with "the deletes finished before
   the signal: raise the orphan count".
4. The result: `err != nil`; the 3 volumes and the manifest still there;
   some orphans left (count > 4).
5. `stop()`; `clean` (a fresh run, no signal): objects == the 4 originals;
   `receive -F` again into a new dataset `restoredClMany2`; `wantGUIDs`.

Break proofs: `AWSS3Backend.List` without the continuation loop (step 2's
own listing still sees 1104, but `clean --dry-run` says ~996 and warns about
a missing volume, or step 4 leaves orphans); the delete pool ignoring
`ctx.Done()` (step 4's error is nil or the orphans are all gone).

## Task 6: a MinIO user without `s3:DeleteObject` (scenario 9's environment)

Files: `integration/compose.yaml`, `integration/nodelete-policy.json`,
`integration/README.md` (task 7 writes the text).

- `nodelete-policy.json`: an IAM policy with `s3:GetObject`,
  `s3:ListBucket`, `s3:GetBucketLocation`, `s3:PutObject`,
  `s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts`,
  `s3:ListBucketMultipartUploads` on `arn:aws:s3:::*`. No delete.
- Service `mc` in `compose.yaml`: image `pgsty/mc:<dated tag>` (pin the
  newest tag Docker Hub lists; record it in the commit), `depends_on: minio
(service_healthy)`, the policy file mounted at `/nodelete-policy.json`,
  `entrypoint: ["/bin/sh", "-c"]` with one command: `mc alias set local
http://minio:9000 minioadmin minioadmin && mc admin user add local
nodelete nodelete-secret && mc admin policy create local nodelete
/nodelete-policy.json && mc admin policy attach local nodelete --user
nodelete`. Make it idempotent: `mc admin policy attach` fails when already
  attached, so `|| true` on that step only, or check with `mc admin user
info` first.
- The `zfs` service: `depends_on: mc: condition:
service_completed_successfully`; environment `ZFSBACKUP_IT_NODELETE_KEY:
nodelete`, `ZFSBACKUP_IT_NODELETE_SECRET: nodelete-secret`.
- `compose.aws.yaml` (real S3): leave the variables unset there; the subtest
  skips. Check that `make integration-vm INTEGRATION_AWS=1` still composes
  (`docker compose config` in the VM shell).

If `pgsty/mc` has no `mc admin` (a stripped build), say so in the execution
notes, keep the policy file and the subtest (it skips), and drop the service.

Check: `make integration-vm RUN=TestIntegrationS3CleanLocal/Denied` runs,
not skips (the test log shows the `AccessDenied` line).

## Task 7: docs

- `integration/README.md`: four rows in the test table (one line each, in
  the style of the others), a `RUN=` example with a subtest
  (`TestIntegrationS3CleanLayout/Separator`), a paragraph on the `mc`
  service and the `nodelete` user under "Environment".
- `docs/context/testing.md`: `fourteen` becomes `eighteen` in both places;
  add to layer 4: "`clean`: `--cleanLocal` on a cut send, a busy dataset, an
  encrypted destination without the key, nested and sibling prefixes,
  another `--separator`, the legacy key layout, a versioned bucket, 1100
  orphans and a SIGTERM mid-delete, a denied delete, repair with `--force`
  at one of two destinations".
- `docs/context/clean.md`, Tests: one bullet naming the four tests and the
  file names.
- `CHANGELOG.md`: one `* [ADD] test: ...` line per task 2-6.
- `.claude/napkin.md`: facts 1, 9 (now fixed) and 10 as one-liners under
  "Writing tests"; `docs/specs/2026-10-12--integration-test-clean/` under
  "Open plans" with its status.
- This file: an "Execution notes" section at the end, as the other plans
  have: what differed, the break proofs run, timings.

## Order and budget

1. Task 1, then `make integration-vm RUN='TestIntegrationS3(Resume|ListClean|Large)$'`.
2. Task 2 (the largest; scenario 9's subtest skips until task 6).
3. Task 3, task 4, task 5: each `make integration-image`, then
   `make integration-vm RUN=<test>`, then its break proofs in a scratch copy.
4. Task 6, then `RUN=TestIntegrationS3CleanLocal/Denied`.
5. Task 7, then the whole suite: `make integration-vm` twice in a row,
   `make lint`, `make fmt-check` (after `git add`), `make test-one PKG=.`
   (the e2e package still compiles with the helper changes).

Expected time on the VM: CleanLocal about 35 s (three cut sends, 5 s stall
each), CleanEncrypted 10 s, CleanLayout 15 s, CleanMany 25 s (4100 PUTs,
4100 DELETEs). The entrypoint's `-test.timeout 45m` holds.

Findings to report, not fix, in the execution notes: a denied delete is
retried for 10 minutes (fact 10); the advice text for an encrypted manifest
with no rings, if it is not the `--secretKeyRingPath` branch (fact 4).

## Out of scope

The bounded read rounds, an unreadable cached manifest (hand-made file; the
unit tests do it), Azure and GCS, S3 listings on real AWS past 1000 keys
(the same code path; MinIO proves the pagination).

## Execution notes

Executed 2026-10-12 in the NixOS Lima VM (zfs 2.4.4, MinIO
`RELEASE.2026-08-04T00-00-00Z`, mc `RELEASE.2026-09-16T00-00-00Z`).
`TestIntegrationS3CleanLocal` (with `Repair`, `Denied`) and
`TestIntegrationS3CleanMany` (with `Interrupted`) pass.
`TestIntegrationS3CleanEncrypted` and `TestIntegrationS3CleanLayout` are red
on purpose: findings F1 to F3 below. `Legacy` and `Versioned` pass.

Timings on the VM: CleanLocal 12 s, 29 s with `Denied` (its inner run waits
about 12 s for the denied delete to fail); CleanEncrypted 6 s; CleanLayout 4 s;
CleanMany 3 s (4100 PUTs and deletes are faster than assumed: the 3000
deletes of `Interrupted` take about 1.5 s, and the signal still lands).

Whole suite, twice in a row (`make integration-vm`): 16 tests pass; only
`CleanEncrypted` and `CleanLayout/Separator` (and its parent) fail, each only
on its finding lines. `make lint`: 0 issues. `make test-one PKG=.`: ok. gofmt:
no change.

### Findings (reported, not fixed)

- F1: `clean` with NO rings at an encrypted destination reads the
  `.manifest.gz.pgp` as plain gzip (`gzip: invalid header`) and takes the
  default branch of `unreadableManifestError`: "If you did not write it ...
  delete that object from the destination". That is the advice fact 4 says
  must not appear. The cached copy of that manifest is deleted too
  (`keepsCachedCopy` is false there). With another key's rings the advice is
  right. Nothing at the destination is deleted.
- F2: `otherPrefixSets` reads the manifest of a NESTED destination
  (`sub/manifests|zbtest/srclay|<snap>.manifest.gz`, seen from `run/`) as a
  manifest of another `--manifestPrefix` (`sub/manifests`). Volume names hold
  no prefix, so the orphan `run/zbtest/srclay|<snap>.zstream.vol77` is spared
  with "Skipping 1 volumes of the backup set ...: the manifest
  sub/manifests|... under another --manifestPrefix is for it." Safe, but an
  orphan stays and the reason is wrong. A fix: drop objects under
  `nestedDestinations` roots before `otherPrefixSets`.
- F3: `clean` reads `manifests#...` (`--separator '#'`) as its own manifests,
  but the listing loop skips by `strings.HasPrefix(obj, "manifests|")`, so
  each manifest gets "Skipping unrecognized object ... (not a backup volume
  written by this tool)". Safe; wrong Notice.
- Fact 10: a denied delete (`AccessDenied`) is retried for 10 minutes. The
  `force-local` break proof shows the same for a client-side error: a delete
  of the empty key (`InvalidParameter: 1 validation error(s)`) was retried for
  10 minutes too. clean retries errors that cannot succeed.

### Where the code differs from the plan

- `cutSend(t, client, bucket, prefix, work, src, snap, extra...)`: it needs
  the listing before and after the cut, so it takes the client and bucket.
- `itRunCtx` goes through `cmd.Run(ctx)` (main's path: every subcommand gets
  ctx), not `RootCmd.ExecuteContext`; `itRunOut` calls it with
  `context.Background()`.
- `wantInLog` exists already (`e2e_resume_other_dest_test.go`, same
  package); the integration files use it and add only `wantNotInLog`.
- CleanLocal step 7 cuts a full of `weekly`, not an incremental.
- `Denied` also checks that the root clean removes the partial's cached copy.
- CleanEncrypted: the cache check is "every manifest at the destination keeps
  its copy" (3), not ">= 1": syncCache downloads all three before the first
  fails, so ">= 1" passed with the copy deleted. `refused` also wants no
  "objects but no manifests" (see the `skip-unreadable` proof).
- CleanMany does not hard-code 3 volumes (`countObjects`: 3 or more); the
  counts are `len(sent) + 1100`. `Interrupted` also wants "Received SIGTERM;
  stopping." in the log.
- Layout and its subtests list with the run's prefix, so they work under
  `ZFSBACKUP_IT_BUCKET` too; `Versioned` skips there (it does not turn
  versioning on in a shared bucket).
- Task 6: `pgsty/mc` has no grep, so `mc admin policy attach ... || true`
  (a user with no policy fails `Denied` at its first listing).
  `--abort-on-container-exit` stopped the run when `mc` exited: the Makefile
  passes `--no-attach mc`. `compose.aws.yaml` sets the two `NODELETE`
  variables to "", so `Denied` skips on real S3.
- `snapshot()` and `snapshotTie` wait for a second AFTER the last one (`snapshot` had `==`, `snapshotTie` no check). The
  VM clock is stepped back about 1 s every few minutes (journal: "Time jumped
  backwards"). Seen twice: two snapshots with one name second (CleanLocal
  step 6, list order a tie) and "dataset already exists" (CleanEncrypted;
  `NativeEncryption` in a whole-suite run, through `snapshotTie`).
- The break proofs left 18 GB of Docker build cache in the VM (one per copy
  path); the next whole-suite run failed in `Large` with MinIO's
  `XMinioStorageFull`. `docker builder prune -f` in the VM fixed it.
- `docs/context/testing.md` no longer says `ReceiveSafety/Mounted` and
  `/Origin` are red (fixed 2026-10-12, napkin); the README row for
  `TestIntegrationS3ReceiveSafety` still says RED and is not changed here.

### Break proofs

Each in a fresh copy under `~/.cache/zb-break` (`/tmp/zb-breaks/run.sh`,
`breaks.sh`). `RUN=X/NONE` runs a parent without its subtests (make eats the
`$/` of `X$/^$`). For the two red tests a proof counts only with a failure
the baseline does not have.

| Break | Test | Result |
|---|---|---|
| no `busy[...]` check for local-only manifests | CleanLocal | fails, step 3 (cache) |
| `--force` judges local-only sets (`\|\| localOnly` dropped) | CleanLocal | fails, step 7 (after a 10-minute retry of the empty key) |
| `localDeletes` not removed | CleanLocal | fails, step 5 (cache) |
| `Repair` without `clean --force` | Repair | fails, step 3 (`m2` objects at d2, list) |
| `Denied` with the root key | Denied | fails ("succeeded", objects) |
| `readForClean` skips an unreadable manifest | CleanEncrypted | first round: only numbers changed in lines that fail anyway (the "no manifests" refusal deletes nothing too). With `refused` wanting no "objects but no manifests": fails, new: line 54 (another key's rings) |
| `keepsCachedCopy` false | CleanEncrypted | fails, new: another key's rings leave 2 copies of 3 |
| step 5 without rings | CleanEncrypted | fails, new: line 55 |
| no nested-root check | CleanLayout | fails, new: no "holds its own manifests" |
| no refusal for 0 manifests | CleanLayout | fails, new: the root clean succeeds |
| `isManifestObject` only for its own separator | Separator | fails, new: the dry-run refuses (0 manifests) |
| no legacy Warning | Legacy | fails |
| no `PutBucketVersioning` | Versioned | fails: no delete marker |
| `AWSS3Backend.List` reads one page | CleanMany | fails: dry-run count, objects left |
| delete pool ignores ctx (no `ctx.Done`, `Delete` with `Background`, no `backoff.WithContext`) | Interrupted | fails: no error after SIGTERM |

### Fixes of F1 to F3 (2026-10-12, same day)

- F1, `files.(*VolumeInfo).Extract`: with no keys, a manifest whose first byte is
  an OpenPGP packet header (`checkNotPGP`; gzip starts with 0x1f, never one) is a
  `KeyError`: an encrypted session key gives `NeedsSecretKey` ("pass --encryptTo
  and --secretKeyRingPath"; the cached copy stays), a signature a plain one.
  Unit test `TestReadCachedManifestNoKeysAdvice`.
- F2, `withoutNestedOwnManifests` (`backup/clean.go`): before `otherPrefixSets`,
  drop a manifest under a nested root x/ only when its set also has a volume
  under x/. A job whose `--manifestPrefix` really is `x/manifests` keeps its
  volumes outside x/, so its sets stay protected
  (`TestCleanSparesSetsOfPrefixWithSlash`; a naive "drop every nested
  manifest" fails it). `TestCleanNestedDestinationOfTheSameSet`.
- F3: the listing loop also skips what `isManifestObject` reads as ours.
  `TestCleanOwnManifestOfAnotherSeparator` now checks there is no
  "unrecognized object" line.
- Break proofs (scratch copy, `make test-one PKG=./backup`): each fix reverted
  fails its test; the naive F2 fails the slash-prefix test.
- `make test-one PKG=./...` ok; `make lint` 0 issues; on the VM the four clean
  tests pass, and the whole suite passes (18 of 18).
