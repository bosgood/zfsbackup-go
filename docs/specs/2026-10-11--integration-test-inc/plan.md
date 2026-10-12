# Integration test, incremental chains and operations: implementation plan

Date: 2026-10-11
Branch: `clean-dry-run`
Source: `intent.md` in this directory.

## Goal

Five new tests in the real-ZFS + MinIO environment of `make integration-vm`
(`make integration-docker` on Linux). Each one is a `TestIntegrationS3*`
function, so the default `RUN=TestIntegrationS3` pattern of the entrypoint
runs all of them.

| Test                              | Covers                                                                                                                            |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `TestIntegrationS3Chain`          | full -> inc -> inc; restore of the middle link; restore of the whole chain                                                        |
| `TestIntegrationS3EncryptedChain` | the same chain, encrypted and signed; `list` and `receive` with the right and the wrong keys                                      |
| `TestIntegrationS3Bookmark`       | a bookmark as the incremental source, smart and explicit (`-i src#name`); restore of the chain                                    |
| `TestIntegrationS3ListClean`      | `list` filters and order; `clean` dry-run, orphan delete, unrecognized object kept, `--force` on a broken set                     |
| `TestIntegrationS3Resume`         | a `zfs send` that dies mid-stream; `--resume` continues; missing volume and missing manifest variants; `clean` protects a partial |

`TestIntegrationS3` stays as it is.

## Facts that shape the plan

Checked on 2026-10-11 in the code. Do not re-derive them.

1. **Chain walking is manifest-based.** `AutoRestore` (`backup/restore.go`)
   links manifests with `linkManifests` and walks `ParentSnap` until a
   snapshot is local or a full is reached. The target is the snapshot given
   (`receive --auto src@<name>`) or the newest backup. So a middle link
   restores with `--auto` plus a snapshot name. A plain `receive src@<mid>`
   into an empty dataset fails: the incremental source must be local
   (`docs/context/receive.md`, step 2).
2. **Bookmarks.** Smart mode: `planSmartSnapshots` (`backup/plan.go:263`)
   checks the last backup with `validateSnapShotExistsFromSnaps(&source,
snapshots, true)`. When only a bookmark of it is left, it flags
   `source.Bookmark` and plans an incremental from it; `GetZFSSendCommand`
   then passes `-i <volume>#<name>`. Explicit mode: `cmd/send.go:255` parses
   `-i src#name` and sets `Bookmark = true`. `SnapshotInfo.Bookmark` has no
   `json:"-"` tag, so the manifest records it. `zfs receive` of such a stream
   recreates the snapshot on the receiving side, so the restored dataset lists
   it as a snapshot.
3. **Keys without gpg.** `newKey(t)` and `writeRings(t, keys...)`
   (`e2e_receive_test.go:47,66`) make an RSA key and armored rings and return
   `--publicKeyRingPath`, `--secretKeyRingPath`, `--encryptTo
backup@example.com`, `--signFrom backup@example.com`. The e2e files carry
   no build tag, so they compile into the tagged binary. The napkin says: RSA,
   armored. `--signFrom` forbids `--maxFileBuffer 0` on receive.
4. **Resume.** `--resume` (`tryResume`, `backup/backup.go:1441`) logs
   `Resuming from volume %d: %d of %d cached volumes verified at all
destinations.` and, for a volume the destination lost, `Volume %s missing
at %s; it and later volumes will be re-sent.` (line 1552). It works from the
   cached partial manifest in `<work>/cache/*/*`. Without `--resume` the
   partial is discarded and the send starts over.
5. **Fault injection with real zfs.** The fake's `FAKEZFS_FAIL_AFTER_BYTES`
   does not exist here. `--zfsPath` names the binary for every zfs call
   (`zfs.ZFSPath`), so a bash wrapper can cut one `zfs send` stream and pass
   every other call through. The estimate `zfs send -n -P` must pass through.
   Both images have bash and `head`. On NixOS the wrapper finds the real `zfs`
   through `PATH` (the entrypoint prepends `ZFS_USERLAND`).
6. **clean.** Rules in `docs/context/clean.md`. What matters: a cached
   partial manifest protects its volumes unless `--cleanLocal`; objects but no
   manifests at the destination is a refusal; an object that does not parse as
   a volume logs `Skipping unrecognized object %s` and stays; dry-run logs
   `Dry-run: would delete %d objects in destination.` and `Would delete %s.`;
   `--force` deletes a manifest whose volumes are missing, with its volumes.
   Flag values leak between in-process runs: pass `--dry-run=false
--force=false --cleanLocal=false` explicitly (napkin).
7. **list.** `list --jsonOutput <uri>` prints `map[dataset][]*files.JobInfo`
   to `config.Stdout`, oldest first (`listWrapper` in `integration_test.go`).
   `--volumeName`, `--before`, `--after` filter; the time format is
   `2006-01-02T15:04:05` in the local zone (UTC in the container).
   `e2eEnv.run` captures `config.Stdout`; `itRun` throws it away today.
8. **Volume object names.** `JobInfo.BackupVolumeObjectName(n)` =
   `<volume>|<snap>[|to|<src>].zstream[.<comp>][.pgp].vol<n>`. A manifest's
   `Volumes[i].ObjectName` gives a real name; a changed `vol<n>` suffix is a
   name `clean` parses as a volume of that set.
9. **Environment limits.** The pool is a 2 GiB file; each test writes under
   15 MiB and destroys its datasets. `destroyAfter` destroys in the order
   given: clones first. Two snapshots in one second tie on creation time;
   `snapshot()` waits for a new second. The entrypoint's `-test.timeout` is
   20m; five more tests need more. In the VM a run stalls when the Mac sleeps
   (napkin): rerun before you debug.

## Assumptions

- The byte stream of `zfs send` for one snapshot is the same on every run,
  so `--resume` accepts the volumes already uploaded. If a resume on real ZFS
  fails with the changed-stream error, stop. That is a finding about `resume`
  on real ZFS, not a test defect. Report it; do not weaken the test.
- `zfs receive -F` of a chain whose source was a bookmark on the sender
  recreates the snapshot on the receiver (standard ZFS behavior). Task 3
  asserts it with `wantSnapshots`.
- MinIO is the target. `INTEGRATION_AWS=1` runs the same tests against real
  S3; each test makes its own bucket or prefix through `itBucket`.

## Rules for this repo

Read `.claude/napkin.md` first. The rules that matter here:

- Build and test through the Makefile. `make integration-image` compiles and
  vets the tagged test on this Mac without the VM: the fast loop. `make
integration-vm RUN=TestIntegrationS3Chain` runs one test.
- Find code with `semble search` (concepts) and `cs` (symbols), as described
  in `.claude/skills/codesearch/SKILL.md`. Do not use grep to locate code.
- In-process cobra runs leak flag values. Pass bool flags explicitly and
  call `RootCmd.SetArgs(nil)` and the `Reset*JobInfo` function after each run
  (`e2eEnv.run` does both).
- Two sends that must give identical volumes, or any volume-count assertion:
  `--compressor ""`. pgzip boundaries depend on timing.
- A test that passes the first time proves nothing. Each task names the
  break that must make it fail. Record the proofs in the commit message.
- Never shape production code around a test environment's limit.
- Assert on ZFS state (`wantSnapshots`, `compareDirs`) and bucket state
  before you assert on log lines. Log assertions only where the behavior
  is not visible elsewhere (`Resuming from volume`, `Would delete`).
- New tests go in new files. Another session may edit this tree: `git
status` before you stage, stage by name.
- One `CHANGELOG.md` line per task a user can see, `* [ADD] test: ...`.

## Layout

```
integration_s3_test.go           shared helpers extended (task 1); TestIntegrationS3 unchanged
integration_s3_chain_test.go     buildChain, TestIntegrationS3Chain, TestIntegrationS3EncryptedChain (tasks 2, 3)
integration_s3_bookmark_test.go  TestIntegrationS3Bookmark (task 4)
integration_s3_ops_test.go       TestIntegrationS3ListClean (task 5)
integration_s3_resume_test.go    flakyZFS, TestIntegrationS3Resume (task 6)
integration/entrypoint.sh        -test.timeout 45m (task 1)
integration/README.md            test table, RUN examples (task 7)
docs/context/testing.md          layer 4 text (task 7)
CHANGELOG.md                     one line per task (each task)
```

All new files: `//go:build integration`, `package main`.

## Task 1: shared helpers

Files: `integration_s3_test.go`, `integration/entrypoint.sh`.

Add to `integration_s3_test.go`, next to the existing helpers:

- `itRunOut(reset, command, work, zfsPath string, args ...string) (out, logs
string, err error)`: what `itRun` does, with the zfs path as a parameter and
  stdout returned. `itRun` becomes a call to it with `"zfs"`.
- `itList(t, work, args...) map[string][]*files.JobInfo`: `list --jsonOutput`
  through `itRunOut` with `cmd.ResetListJobInfo`; fails the test on an error
  or on JSON that does not decode.
- `itClean(t, work, args...) (logs string, err error)`: `clean` through
  `itRunOut` with `cmd.ResetReceiveJobInfo` (what `cleanS3Ctx` uses). It
  prepends `--dry-run=false --force=false --cleanLocal=false`; a caller that
  wants one of them passes it after, and the last value wins.
- `objectNames(t, client, bucket, prefix) []string`: sorted keys under the
  prefix. `countObjects` stays for the existing test.
- `putObject(t, client, bucket, key string, data []byte)` and
  `deleteObject(t, client, bucket, key string)`.
- `cloneOf(t, pool, src, snap, name string) string`: `zfs clone -o
mountpoint=/<pool>/<name> <src>@<snap> <pool>/<name>` and returns the
  mount path. Callers add the clone to `destroyAfter` before `src`.
- `manifestFor(t, work, src, target, snap string) *files.JobInfo`: the
  backup of `snap` among what `newestBackup`'s reader returns. Read how
  `newestBackup` (`e2e_test.go:175`) builds its `JobInfo` and reuse the same
  reader; do not copy its body. Needed because the tests look at the middle
  links, not only the newest.

`entrypoint.sh`: `-test.timeout 20m` becomes `45m`. `RUN` examples in the
README (task 7).

Check: `make integration-image` builds. `make integration-vm` still passes
`TestIntegrationS3` (nothing it uses changed in behavior).

## Task 2: chain of 3 links and the middle link

File: `integration_s3_chain_test.go`.

`buildChain(t, pool, work, target string, extra ...string) chain`:

```go
type chain struct {
    src                     string   // <pool>/src<suffix>
    monthly, daily2, daily3 string   // snapshot names
    daily2Clone             string   // mount path of a clone of src@daily2
}
```

Steps, all with `--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
--incrementalSnapshotSuffix _daily --volsize 1 --compressor ""` plus `extra`
(the key flags in task 3). The dataset name takes a suffix from the caller
so two chains can live in one pool (`src`, `srcenc`).

1. `zfs create`, `a.bin` 3 MiB, snapshots monthly, daily, hourly. Send.
   Assert the newest backup is a full of monthly.
2. `b.bin` 1 MiB, daily2, hourly. Send. Assert daily2 from monthly.
3. Clone `src@daily2` (the compare target of the middle link).
4. `c.bin` 1 MiB, daily3, hourly. Send. Assert daily3 from daily2. **This
   is the third link.**
5. Return the chain. The caller registers `destroyAfter(clone, src,
restored...)` before it calls `buildChain`, with the clone first.

`TestIntegrationS3Chain`:

1. `pool`, `itBucket(t, "zfsbackup-it-chain")`, `buildChain`.
2. **Middle link into an empty dataset, no `--auto`:** `receive -F
src@daily2 target restoredMid` must return an error (the source monthly is
   not there). Assert `err != nil`; the dataset must not exist afterwards
   (`zfs list` fails).
3. **Middle link with `--auto`:** `receive --auto -F src@daily2 target
restoredMid`. `wantSnapshots(restoredMid, monthly, daily2)`;
   `compareDirs(daily2Clone, /restoredMid)`.
4. **Continue to the newest:** `receive --auto -F src target restoredMid`.
   `wantSnapshots(restoredMid, monthly, daily2, daily3)`; `compareDirs(/src,
/restoredMid)`. One step ran; the state proves it.
5. **Whole chain into an empty dataset:** `receive --auto -F src target
restoredAll`. Three snapshots; `compareDirs(/src, /restoredAll)`.
6. **Noop:** the smart send again returns `ErrNoOp`.

Break-the-test proofs:

- Step 3: assert `wantSnapshots(restoredMid, monthly, daily2, daily3)`
  instead. Must fail: the middle-link restore stops at daily2.
- Step 4: `echo x >> /<restoredMid>/c.bin` before `compareDirs`. Must fail.

## Task 3: encrypted and signed chain

File: `integration_s3_chain_test.go`.

`TestIntegrationS3EncryptedChain`:

1. `key := newKey(t)`, `rings := writeRings(t, key)`. `buildChain(..., "enc",
target, rings...)`. Every send encrypts to and signs with the key.
2. Every object name under the prefix contains `.pgp` (volumes
   `.pgp.vol<n>`, manifests `.manifest.gz.pgp`). Assert over `objectNames`.
3. **list with the right rings:** `itList(t, work, rings..., target)`: one
   key `src`, 3 entries, order monthly, daily2, daily3.
4. **list with another key's rings:** `writeRings(t, newKey(t))`. `list`
   must fail. The log must name `--secretKeyRingPath` and must not say
   `delete that object` (as `TestE2EListWithWrongKeyAdvises` asserts).
5. **receive with no rings:** `receive --auto -F src target restoredEnc`
   must fail; the dataset must not exist.
6. **receive with the right rings:** same command plus `rings`.
   `wantSnapshots(restoredEnc, monthly, daily2, daily3)`; `compareDirs`.
7. **Middle link, encrypted:** `receive --auto -F src@daily2 target
restoredEncMid` with rings. Two snapshots; compare with the clone.

`--maxFileBuffer 0` is not used: `--signFrom` refuses it on receive.

Break-the-test proof: in step 6 pass the wrong rings of step 4. The receive
must fail. Proves the positive case needs the key, not only the flags.

## Task 4: bookmark as the incremental source

File: `integration_s3_bookmark_test.go`.

`TestIntegrationS3Bookmark`, dataset `srcbm`, same smart flags as task 2:

1. `a.bin` 3 MiB; monthly, daily, hourly. Send: full of monthly.
2. `b.bin` 1 MiB; daily2, hourly. Send: daily2 from monthly.
3. **Replace the snapshot with a bookmark:** `zfs bookmark srcbm@daily2
srcbm#daily2`, then `zfs destroy srcbm@daily2`. Now the last backup's
   snapshot is gone and only its bookmark is on the pool.
4. `c.bin` 1 MiB; daily3, hourly. **Smart send.** Assert with `manifestFor`:
   base daily3, `IncrementalSnapshot.Name == daily2`,
   `IncrementalSnapshot.Bookmark == true`. Not a full: the planner found the
   bookmark (fact 2), not `source-pruned`.
5. **Explicit form:** `zfs bookmark srcbm@daily3 srcbm#daily3`, `zfs destroy
srcbm@daily3`. `d.bin` 1 MiB; snapshot daily4 (a `_daily` name, so step 6
   sees it as the last backup). `send --volsize 1 --compressor "" -i
srcbm#daily3 srcbm@daily4 target`. Assert base daily4, source daily3,
   `Bookmark == true`.
6. **Noop:** the smart send returns `ErrNoOp` (the newest `_daily` is daily4
   and it is backed up).
7. **Restore the chain into an empty dataset:** `receive --auto -F srcbm
target restoredBm`. `wantSnapshots(restoredBm, monthly, daily2, daily3,
daily4)`: the receiver has the snapshots the sender only has bookmarks of.
   `compareDirs(/srcbm, /restoredBm)`.
8. **list** shows 4 entries in order monthly, daily2, daily3, daily4.

Break-the-test proof: skip the `zfs bookmark` in step 3 (destroy only). The
smart send in step 4 must then plan a full (`source-pruned`), and the
`IncrementalSnapshot.Name == daily2` assertion must fail.

## Task 5: list and clean

File: `integration_s3_ops_test.go`.

`TestIntegrationS3ListClean`, dataset `srcops`, `buildChain` of task 2.

**list:**

1. `itList(target)`: 1 key, 3 entries, order monthly, daily2, daily3.
2. `--volumeName <pool>/srcops`: the same. `--volumeName <pool>/s*`: the
   same. `--volumeName <pool>/nothing`: 0 keys.
3. `--after <now + 1h>`: 0 keys. `--before <now + 1h>`: 3 entries. Format
   the time with `time.Now().Format("2006-01-02T15:04:05")`; the flag parses
   it in the local zone and so does the test.

**clean, healthy destination:**

4. `itClean(--dry-run, target)`: log has `Dry-run: would delete 0 objects`.
   `objectNames` before and after are equal.

**clean, an orphan and an unrecognized object:**

5. `vol1 := manifestFor(daily3).Volumes[0].ObjectName`. `orphan :=
strings.TrimSuffix(vol1, "vol1") + "vol77"` (fact 8). `putObject(orphan,
[]byte("orphan"))`. `putObject("notes.txt", ...)`.
6. `itClean(--dry-run)`: log has `Would delete <uri>/<orphan>.` and
   `Skipping unrecognized object notes.txt`. Objects unchanged.
7. `itClean()` (no dry-run): `orphan` is gone; `notes.txt`, all 3 manifests
   and every volume the 3 manifests name are still there. `receive --auto -F
srcops target restoredOps` restores 3 snapshots; `compareDirs`.

**clean --force on a broken set:**

8. `deleteObject(manifestFor(daily3).Volumes[1].ObjectName)`.
9. `itClean()`: nothing deleted (the set is broken, `--force` is off);
   `itList` still shows 3 entries.
10. `itClean(--force)`: the daily3 manifest and its remaining volumes are
    gone; the monthly and daily2 sets are complete. `itList`: 2 entries.
    `receive --auto -F srcops target restoredOps2`: `wantSnapshots(monthly,
daily2)`.

Break-the-test proofs:

- Skip the `putObject(orphan)` in step 5: the `Would delete` assertion in
  step 6 must fail.
- `deleteObject("notes.txt")` by hand before step 7's assertion: the "still
  there" check must fail. Proves the check reads the bucket.

## Task 6: resume after a failed send

File: `integration_s3_resume_test.go`.

`flakyZFS(t, cutBytes int64) string` writes an executable bash script in
`t.TempDir()` and returns its path:

```bash
#!/bin/bash
# The first `zfs send` of a stream (not `zfs send -n`) is cut after N bytes
# and fails. Every other call goes to the real zfs.
marker=<dir>/armed
if [ "$1" = send ] && [ -e "$marker" ] && [[ " $* " != *" -n "* ]]; then
	rm -f "$marker"
	zfs "$@" | head -c <N>
	echo "flaky zfs: stream cut after <N> bytes" >&2
	exit 1
fi
exec zfs "$@"
```

The test creates `<dir>/armed` before the send that must fail. The real
`zfs` gets SIGPIPE when `head` closes; that is the intended death.

`TestIntegrationS3Resume`, dataset `srcres`, explicit snapshot names:

1. `a.bin` 4 MiB random. `snap := snapshot(t, src, "daily")`. Args: `--volsize
1 --compressor "" srcres@<snap> target` (`itRun` adds `--maxRetryTime 2m`).
2. **The send dies:** arm the marker, send with `--zfsPath flakyZFS(t,
5<<19)` (2.5 MiB: two complete volumes, then the cut). Assert `err != nil`,
   logs contain `flaky zfs`, the bucket has 0 manifests and 2 volumes, and
   `<work>/cache/*/*` holds one partial manifest with 2 volumes, each with
   `ZFSStreamBytes >= 1<<20 - 50<<10` (no truncated tail, as
   `TestE2EZFSSendFailureExits` asserts).
3. **clean protects the partial:** `itClean(work, --dry-run, target)` logs
   `would delete 0 objects`. With a fresh working directory (no cache),
   `itClean(t.TempDir(), --dry-run, target)` must fail: objects but no
   manifests (clean rule 5). Both runs leave the 2 volumes in place.
4. **Resume:** the same args plus `--resume`, `--zfsPath zfs`. `err == nil`.
   Logs contain `Resuming from volume 3: 2 of`. The bucket has 1 manifest.
   `receive -F srcres@<snap> target restoredRes`; `compareDirs(/srcres,
/restoredRes)`.
5. **Missing volume:** `deleteObject(manifest.Volumes[0].ObjectName)` and
   `deleteObject(<the manifest object>)`. Resume again. Logs contain `Volume
<vol1> missing at` and `Resuming from volume 1: 0 of`. Bucket: 1 manifest,
   all volumes. `receive -F` into `restoredRes2`; `compareDirs`.
6. **Manifest only:** `deleteObject(<the manifest object>)`. Record
   `objectNames`. Resume. The new `objectNames` equals the old plus exactly
   one manifest key. Logs do not contain `missing at`.

If step 4 fails with the changed-stream refusal, see Assumptions. Record what
differs (the stream SHA-256 in the cached manifest against the log) and stop.

Break-the-test proof: run step 4 without `--resume`. The send starts over and
succeeds, so the `Resuming from volume` assertion must fail. Second proof:
set `cutBytes` so no complete volume exists (`1<<19`): step 2's "2 volumes"
assertion must fail and the resume path is a start-over.

## Task 7: docs and napkin

Files: `integration/README.md`, `docs/context/testing.md`, `.claude/napkin.md`.

- README: a table of the six tests with one line each and the `RUN=` name;
  `RUN=TestIntegrationS3Resume` as the example; note `-test.timeout 45m`.
- `testing.md`, layer 4 and the `integration-docker` row: "six
  `TestIntegrationS3*` tests" with the list above in one sentence.
- Napkin: "Fault injection with real zfs: a bash wrapper as `--zfsPath` cuts
  one `zfs send` (`flakyZFS` in `integration_s3_resume_test.go`); `zfs send
-n -P` must pass through." And the outcome of the resume determinism
  assumption, whichever way it went.

## Order and commits

One commit per task, 1 to 7. Tasks 2 to 6 depend on task 1 only; do them in
that order, run each in the VM before the next. `make integration-image` is
the compile check between VM runs.

## Done when

- `make integration-vm` on this Mac passes all six `TestIntegrationS3*`
  tests, twice in a row.
- Every break-the-test proof failed as described and was undone; the commit
  messages say so.
- `make test-docker`, `make fmt-check` (after `git add` of the new files) and
  `make lint` pass (`.golangci.yml` lints the tag).
- The CHANGELOG lines and the docs of task 7 are in.

## Out of scope, noted for later

- `TestIntegration` and `TestEncryptionAndSign` (`integration_test.go`) still
  need `tank/data@c` and Azurite. They are not ported; the tests above cover
  their chain, bookmark, list and key cases on S3.
- Resume in pipe mode (`--maxFileBuffer 0`): a failed pipe upload cannot
  retry, and the send does not cache volumes. Nothing to resume.
- Signal-driven interruption (SIGTERM mid-upload) on real ZFS. The wrapper
  kills the stream, not the process; `e2e_signal_test.go` covers the signal.
- A chain longer than 4 links, and a `--fullIfOlderThan` window that elapses
  for real. The golden scenarios cover the planner.

## Execution notes (2026-10-11)

All six `TestIntegrationS3*` tests pass in `make integration-vm` twice in a
row (zfs 2.4.4, MinIO). Together they take about 40 s.

Where the code differs from the plan:

- **Task 1.** `--maxRetryTime` exists on `send` and `receive` only; `itRunOut`
  adds it for those two. `manifestFor` reads with `backupsAt`, the reader that
  `newestBackup` now calls (`e2e_test.go`). `buildChain` registers
  `destroyAfter(clone, src)` itself; callers register only what they restore.
- **Tasks 2, 3.** `buildChain` checks each link with `itList` (key flags
  included), not `newestBackup`: an encrypted manifest needs the rings.
- **Task 4.** Break proof: without the bookmark the smart send is a noop
  (`source-pruned`, no newer `_monthly`), not a full. The test fails either
  way: the daily3 send never happens.
- **Task 6.** Two changes.
  1. The wrapper stalls 5 s after the cut, then fails. Without the stall the
     stream died before volume 1 finished its S3 upload; the failure cancels
     uploads in flight, so nothing was cached and nothing could resume
     (seen on the first VM run). A failed `zfs send` keeps only volumes whose
     upload finished before the failure. That is production behavior, not
     changed here.
  2. Step 3 as written cannot pass: `clean` refuses a destination with
     objects but no manifests even when the cache holds a partial. The test
     first sends a small full of an earlier snapshot. Then `clean --dry-run`
     with the cache deletes 0 objects, and with a fresh working directory it
     would delete exactly the 2 partial volumes.
- **Resume assumption holds.** `--resume` on real ZFS accepted the volumes of
  the cut attempt (`Resuming from volume 3: 2 of 2`).

Break proofs, each run in a scratch copy and failing as planned: chain middle
link with daily3 expected; a changed `c.bin`; encrypted receive with another
key's rings; no bookmark; no orphan; `notes.txt` deleted by hand; resume
without `--resume`; a cut at 512 KiB (no complete volume).
