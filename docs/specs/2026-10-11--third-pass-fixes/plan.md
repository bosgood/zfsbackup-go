# Third high-risk pass: implementation plan

Date: 2026-10-11
Branch: `clean-dry-run` at `3814a53`
Intent: `intent.md` in this directory.

Line numbers are at `3814a53`. Check them with `cs` before you edit.

## Rules for this work

Read `.claude/napkin.md` first. Then these rules apply:

1. **Test first.** For each item, write the test, run it, and see it FAIL on the
   current code. Copy the failing output into the commit message. A test that
   passes before the fix proves nothing. If a test passes first time, read the
   code again before you touch the test (napkin, 2026-10-08).
2. **Build and test through the Makefile.**
   - One test: `make test-one RUN=TestName PKG=./backup/`
   - Fast host loop: `make test-run PKG=./backup/ RUN=TestName`
   - Root e2e tests: `make test-run PKG=. RUN=TestE2E...` and, before you commit,
     the WHOLE root package: `make test-run PKG=.` (in-process cobra runs leak
     state; a green `-run` filter is not enough).
   - Full suite before the last commit: `make test-docker`, then `make fmt-check`
     and `make lint`.
   - A missing target: add it to the Makefile. No ad-hoc `go` or `docker` runs.
3. **Find code with the `codesearch` skill.** `semble search "<concept>"` for a
   concept, `cs --color never -i go -x vendor --only-declarations <Symbol>` for a
   symbol. Read `.claude/skills/codesearch/SKILL.md` first.
4. **Pass every boolean flag on every in-process run** (`--dry-run=false`):
   `cleanDryRun` and `cleanLocal` are package globals. Assert only on Notice-level
   or higher log lines in root e2e tests.
5. **One commit per item**, test and fix together, then one commit for docs and
   `CHANGELOG.md`. Commit messages end with the attribution line from the
   session's system reminder.
6. **Napkin.** Add what you learn to `.claude/napkin.md` as you go, not at the
   end.

## Order

1, 2, 5, 3, 4, 7. Items 1 and 2 are the two likely bugs. Item 7 needs a build
of `master`, so it goes last.

---

## Item 1: explicit `receive` of a snapshot that is already local

### Why it is wrong today

- `cmd/receive.go:214-218`: when the snapshot exists on the LOCAL pool,
  `zfs.GetCreationDate` succeeds and sets `jobInfo.BaseSnapshot.CreationTime`.
- `backup/restore.go:304-312`: the "Selected base snapshot already exists,
  nothing to do!" check runs only when that time is zero, that is, only when
  the snapshot is NOT local. With a zero time, `SnapshotInfo.Equal`
  (`files/jobinfo.go:137`) never matches a real snapshot. The shortcut is dead.
- The run goes on to `zfs receive`. Without `--force` zfs refuses. With
  `--force` (`-F`, `zfs/zfs.go:468`) a full stream overwrites the dataset and
  zfs destroys the local snapshots.
- The `--auto` path is fine: it compares the manifest's snapshot (name and
  creation time) with the local listing (`restore.go:159`).

### Test first

File: `e2e_receive_exists_test.go` (root package `main`). Use `newE2EEnv`,
`env.send`, `env.receive` (`e2e_failure_test.go:94`), `guarded`, and the fake
zfs. The fake lists snapshots from `FAKEZFS_SNAPSHOTS`, answers `zfs get
creation`, and logs every `zfs receive` to `FAKEZFS_RECEIVE_LOG` (one line per
receive). `env.writeSnapshots` writes rows for ONE dataset; for two datasets
write the file yourself in the same format (`e2e_test.go:94-108`).

`TestE2EReceiveOfLocalSnapshotIsNoOp`:

1. Pool: `tank/data@a` created 2026-09-01T00:00:00Z. Send it:
   `env.sendOK(t, "--volsize", "1", "--compressor", "", "tank/data@a", dest)`.
2. Pool now also has `restored/data@a` with the SAME creation time (the
   restore happened earlier). Set `FAKEZFS_RECEIVE_LOG` to a fresh file.
3. Run `env.receive("tank/data@a", dest, "restored/data")`.
4. Assert: no error; the log has `already exists` (keep the current wording of
   the Notice, or change it and update the assertion); the receive log file
   does not exist or is empty (no `zfs receive` ran).

`TestE2EReceiveRefusesSameNameOtherSnapshot`:

1. Same send. Pool has `restored/data@a` created 2026-09-02T00:00:00Z
   (another snapshot with the backed-up name).
2. Run `env.receive("tank/data@a", dest, "restored/data")` and again with
   `--force`.
3. Assert for both: an error; the log names both creation times (reuse the
   wording of `noteLocalSnapshotMismatch`, `restore.go:255`); the receive log
   is empty.

Why they fail today: the first runs `zfs receive` (the fake logs a line) and
succeeds. The second runs `zfs receive` too and succeeds (the fake does not
model "destination exists"), so it fails on the "no receive" assertion.

Sub-tests for `--fullPath` (`-d`) and `--lastPath` (`-e`) are worth one line
each: the cmd-side lookup at `cmd/receive.go:214` uses `jobInfo.LocalVolume`
while `Receive` computes the real target (`restore.go:291-300`). After the fix
the check must use the computed target.

### Fix

In `backup/restore.go`, `Receive`:

1. Move the "already exists" decision AFTER `readCachedManifest` (`:326`), so
   it has the manifest's `BaseSnapshot` with its creation time.
2. List the target (`volume`, the computed one) with
   `zfs.GetSnapshotsAndBookmarks`. A listing error means the dataset is not
   there yet: treat it as an empty list (as `:177-180` does).
3. If `validateSnapShotExistsFromSnaps(&manifest.BaseSnapshot, local, false)`:
   log the Notice and return nil.
4. Else, if a local snapshot has the same name (and is not a bookmark): return
   an error that names both creation times. Do this with or without `--force`:
   a name collision is never what the user asked `-F` for. Reuse or extend
   `noteLocalSnapshotMismatch` so `--auto` and the explicit path share the text.
5. Delete the dead branch at `:304-312`. Keep the incremental check at
   `:315-324`, but make it use the manifest's `IncrementalSnapshot` the same way
   (a zero-time compare is the same dead pattern; today it only happens to give
   the right answer).
6. In `cmd/receive.go:214-226`, remove the `GetCreationDate` lookups if nothing
   else reads the times (check with `cs --only-usages`). If something does,
   leave them and say why in a comment.

Update `docs/context/receive.md`.

---

## Item 2: wrong-key manifests get the "delete that object" advice

### Why it is wrong today

`readCachedManifest` (`backup/sync.go:290-330`) tells only
`files.ErrManifestTooLong` apart. A `*files.KeyError` ("is encrypted to no key
in the secret ring", or signed by another key) falls through: the cached copy is
removed (`:306`), the object is downloaded again, it fails again, and
`unreadableManifestError` (`:334-357`) says "If you did not write it ... delete
that object from the destination". That advice reaches `list`, `plan`, smart
`send` and `receive --auto`. Only `clean` (`backup/clean.go:211`) and
`tryResume` (`backup/backup.go:1386`) check for a KeyError.

### Test first

File: `backup/manifest_keyerror_test.go` (package `backup`). Helpers to copy:
`prefixedKey` and `loadPrefixedRings` in `backup/clean_prefixes_test.go:180-235`
(they build a test key and load rings; `backup/` cannot import
`files/pgp_test.go`). `setupCleanTest` gives a `file://` destination and a
`JobInfo`. `prefixedSet` writes an encrypted manifest when given a key and an
email. The S3-free way to call the reader is `readCachedManifest(ctx, jobInfo,
cacheDir, backend, objectName)` with a `FileBackend` from
`prepareBackend`; see how `TestReadCachedManifestKeepsTooLongManifest`
(`backup/manifest_limit_test.go`) does it and copy its setup.

`TestReadCachedManifestWrongKeyAdvice`:

1. Key K1 writes the manifest of `tank/data@a` (`prefixedSet(..., k1,
"k1@example.com")`). Load the rings with K2 only.
2. Call `readCachedManifest` with a `JobInfo` whose `EncryptTo` is
   `k2@example.com`.
3. Assert: the error wraps `*files.KeyError` (`errors.As`); its text does NOT
   contain `delete that object`; it contains `--secretKeyRingPath`; the cached
   file under `cacheDir` still exists after the call.
4. Count the downloads: wrap the backend, or check the cache file's mtime, to
   assert the object was read ONCE.

Why it fails today: the text contains "delete that object", and the cached
copy is removed.

Add one root e2e, `TestE2EListWithWrongKeyAdvises` in `e2e_sign_test.go`'s
style: send with `--encryptTo`, run `list` with a secret ring that lacks the
key, assert exit non-zero and the advice line. This proves the text reaches a
user.

### Fix

In `readCachedManifest`:

- After the first `readManifest`, if `errors.As(err, new(*files.KeyError))`:
  return `unreadableManifestError(objectName, path, err)` at once. Do not
  remove the copy. Do not download again (the object is the same bytes).
- Do the same after the second read (a fresh download that is a key error).
- In `unreadableManifestError`, add a KeyError branch: "could not read the
  manifest X: <KeyError text>. It is a backup of yours written with another
  key: pass the secret key ring that holds it with --secretKeyRingPath (and
  --trustSigner for a rotated signing key). Do not delete it." Keep the
  generic branch for everything else.

`clean.go:211` already has its own KeyError message; make it call the shared
one if that removes duplicate text, else leave it.

---

## Item 5: a sibling prefix that starts with a symbol is read as ours

### Why it is wrong today

`isManifestObject` (`backup/sync.go:253-265`) accepts a name as ours when the
character after our prefix is one no dataset name can hold. That rule exists
so a manifest of OUR prefix written with another `--separator` is read. A
sibling job with `--manifestPrefix manifests@v2` also matches (`@` is not a
dataset character). `syncCache` downloads its manifest, `manifestNamed`
(`sync.go:370`) rejects it as "the manifest of another backup set", and `clean`
stops with the delete advice from Item 2. `list`, `plan` and smart `send` fail
the same way.

### Test first

File: `backup/clean_prefixes_test.go`, next to
`TestCleanOwnManifestOfAnotherSeparator` (`:341`).

`TestCleanSkipsSiblingPrefixStartingWithSymbol`:

1. `setupCleanTest`. Our set: `prefixedSet(t, dir, "tank/data", "a", "",
"manifests", nil, "")`. Sibling set, same dataset: `prefixedSet(t, dir,
"tank/data", "b", "", "manifests@v2", nil, "")`. Add an orphan of OUR set:
   `writeTestObject(t, dir, ourSet.BackupVolumeObjectName(2))`.
2. `Clean(ctx, jobInfo, false, false)` with `captureLogs`.
3. Assert: no error; our vol1 and the sibling's vol1 and manifest still exist;
   our orphan vol2 is gone; the log has `Skipping 1 volumes of the backup set
tank/data|b` and `under another --manifestPrefix`.

Also add rows to `TestOtherPrefixSets` (`:323`) for
`manifests@v2|tank/data|b.manifest.gz` -> `|tank/data|b`, and a unit test
`TestIsManifestObjectSibling` in `backup/sync_test.go` (create it if missing)
with the table below.

Why it fails today: `Clean` returns the "another backup set" error.

### Fix

Tighten `isManifestObject`. After the exact-separator match, derive the
separator the name implies as the run of non-dataset characters that follows
the prefix, and require that separator to appear again in the set part:

| name                                        | prefix      | sep  | ours? | why                                   |
| ------------------------------------------- | ----------- | ---- | ----- | ------------------------------------- |
| `manifests\|tank/data\|a.manifest.gz`       | `manifests` | `\|` | yes   | starts with our separator             |
| `manifests#tank/data#a.manifest.gz`         | `manifests` | `\|` | yes   | implied `#`, appears again in the set |
| `manifests@v2\|tank/data\|b.manifest.gz`    | `manifests` | `\|` | no    | implied `@`, never appears again      |
| `manifests-old/manifests\|x\|y.manifest.gz` | `manifests` | `\|` | no    | `-` is a dataset character            |
| `manifests2\|tank\|a.manifest.gz`           | `manifests` | `\|` | no    | `2` is a dataset character            |

Implementation sketch:

```go
rest, ok := strings.CutPrefix(name, manifestPrefix)
if !ok || rest == "" { return false }
if separator != "" && strings.HasPrefix(rest, separator) { return true }
n := 0
for n < len(rest) && !inDatasetName(rest[n]) { n++ }
if n == 0 { return false }
sep := rest[:n]
set := rest[n:]
idx := strings.LastIndex(set, ".manifest")
if idx < 0 { return false }
return strings.Contains(set[:idx], sep)
```

`otherPrefixSets` (`backup/clean.go:66`) calls `isManifestObject` to skip ours,
so the sibling is now indexed and its sets spared. Nothing else changes.
Update the comment at `sync.go:249-252` and `docs/context/clean.md`.

---

## Item 3: `--resume` when every byte is skipped but the cache is not final

### What is unknown

A kill between the last volume's cache write (`backup/backup.go:592`,
`saveManifest(ctx, jobInfo, false)`) and the final save (`:635`,
`saveManifest(ctx, jobInfo, true)`) leaves a cached manifest that lists every
volume and has no `EndTime`. On `--resume`, the shortcut at `:1329` needs an
`EndTime`, so the run skips the whole stream, `Peek` at `:1236` sees EOF, and
`sendStream` returns with no new volume. Whether the final manifest is then
built from the kept volumes and uploaded is untested. Tests cover the
final-manifest case (`TestE2EResumeAfterManifestUploadFailureUploadsOnlyManifest`)
and the boundary case for a fresh send
(`TestE2EStreamOnVolumeBoundaryHasNoEmptyVolume`), not this one.

### Test first

Mechanism: no kill. Add a hook next to the two at `backup/backup.go:65-68`:

```go
// TestHookBeforeFinalManifest runs before the final manifest is saved.
TestHookBeforeFinalManifest func()
```

Call it right before `:635`. In the test, the hook copies the cached partial
manifest (`partialManifestCachePath`; find the file with `env.cachedManifests`
or by listing the cache dir under `env.work`) to a side file. After the send
succeeds, delete the manifest object at the destination and put the side copy
back over the cache file. That is exactly the state a kill leaves: every volume
at the destination, a cached manifest with every volume and no `EndTime`, no
manifest at the destination.

File: `e2e_resume_test.go`. `TestE2EResumeFinalizesFullyCachedPartial`:

1. `FAKEZFS_STREAM_BYTES` = 3 MiB, `--volsize 1 --compressor ""` (three
   volumes, and the stream ends exactly at a cut, so `Peek` sees EOF).
2. Send, build the state as above. Record `destObjects` before.
3. `env.sendOK(t, "--resume", ...same args)`.
4. Assert: the log has `Resuming from volume 4: 3 of 3 cached volumes verified`
   (match the real wording); `destObjects` after = before + exactly one object
   that starts with `manifests|`; no volume changed; `newestBackup(...)` has 3
   volumes and a non-zero `EndTime`; `env.checkRestores(t, dest, "a", 3<<20)`.
5. A second sub-test with 2.5 MiB: the stream does NOT end at a cut, so the
   kept-volume state is the same but the stream has no trailing data either.
   Both must finalize.

Run it. If it FAILS, fix as below. If it PASSES on the current code, the
item is a test-only item: keep the test, say so in the commit message, and
note in the napkin why it passed (which line finalizes the set).

### Fix (only if the test fails)

Follow the failure. The likely places: `sendStream` returning at `:1238`
before it records the stream's length (`TestHookBeforeStreamBytes` at `:1343`
marks that spot), or the final save at `:635` not running when the pipeline
produced zero volumes. The set must be finalized from `j.Volumes` as kept by
`tryResume`.

---

## Item 4: Glacier restore window from the download size

### Why it is wrong today

`classifyRestores` (`backends/aws_s3_backend.go:378-381`) restores every copy
for `Days = 3`. `Receive` calls `PreDownload` for EVERY volume of a set up
front (`backup/restore.go:352`), so all downloads must finish within 3 days of
the restore. An expired copy fails the GET with `InvalidObjectState`, which is
not `NoSuchKey` (`:521`), so `receive` retries it for `--maxRetryTime` and then
fails; the re-run restores and downloads from volume 1 again. Large sets at
modest bandwidth never finish.

### Design

Decided with the user: set the window from the bytes to download.

- `classifyRestores` already HEADs every key and sums `ContentLength` for the
  keys it restores (`:364`). Also sum the size of EVERY key (readable ones
  included): the download covers all of them. Return it as `totalBytes`.
- New pure function in `backends/aws_s3_backend.go`:

  ```go
  // restoreDays is how long a restored copy must stay readable: the days to
  // download totalBytes at rate bytes per second, plus one for the wait that
  // follows the restore and one for restoreExpiryMargin. At least 3, at most 30.
  func restoreDays(totalBytes int64, rate int64) int64
  ```

  `days = clamp(ceil(totalBytes / (rate * 86400)) + 2, 3, 30)`.

- Rate: env `AWS_S3_RESTORE_DOWNLOAD_RATE`, parsed with `humanize.ParseBytes`
  (vendored), bytes per second, default `50MiB` (about 4.1 TiB a day). Env
  `AWS_S3_RESTORE_DAYS` overrides the result (an integer, 1 to 30). Read both
  once per `PreDownload`; a bad value is an error, not a silent default.
- Use the same `Days` for the `extend` branch (`:364-366`, `extend == true`).
- Log at Notice once per `PreDownload` when it restores anything: "Restoring N
  objects (X) from <class> for D days; AWS_S3_RESTORE_DAYS overrides". Keep the
  Debug lines.

### Test first

File: `backends/s3_restore_days_test.go`.

`TestRestoreDays` (table): `(0, 50MiB) -> 3`, `(4 TiB, 50MiB) -> 3`,
`(20 TiB, 50MiB) -> 7`, `(1 PiB, 50MiB) -> 30`, `(20 TiB, 500MiB) -> 3`.
Compute the expected values by hand in the test's comments.

`TestS3PreDownloadRestoreDaysFollowSize`: use `recheckS3Client`
(`backends/s3_restore_recheck_test.go`). Its `HeadObjectWithContext` answers
`ContentLength: 50` for every key; add a `size int64` field to `recheckKey`
and answer it (default 50 keeps the old tests). The mock already records
`restoreDays` per key. Two DEEP_ARCHIVE keys of 10 TiB each, no restored copy:
assert both `RestoreObject` calls carried `Days == restoreDays(20 TiB, 50MiB)`.
Then with `t.Setenv("AWS_S3_RESTORE_DAYS", "9")`: both carry 9. Then with
`AWS_S3_RESTORE_DOWNLOAD_RATE=500MiB`: both carry 3.

Why they fail today: `restoreDays` does not exist; every restore carries 3.

Also assert in an existing recheck test that the `extend` branch sends the
same computed `Days` (it has the `restoreDays` field for that).

README: the S3 section lists every environment variable; add the two new ones
next to `AWS_S3_RESTORE_POLL_INTERVAL` and `AWS_S3_GLACIER_RESTORE_TIER`. Also
`docs/context/backends.md`.

---

## Item 7: a fixture of a destination written by an older version

### Why

`manifestNamed` (`backup/sync.go:370`) rejects a manifest whose stored name
differs from the name `StoredManifestObjectName` derives from its contents
(`files/jobinfo.go:265`: `Separator`, `EncryptTo`, `SignFrom`, the manifest
compressor). No fixture written by an older version exists (`testdata/` holds
one zfs listing). If any older version derived the name another way, every old
manifest is "the manifest of another backup set", and the advice is to delete
it. `master` (`b733e96`) is the version before this branch.

### Build the fixture

1. Add a Makefile target:

   ```make
   # make build-rev REV=master  builds that revision to tmp/zfsbackup-<REV>
   build-rev:
   ```

   It runs `git archive $(REV) | tar -x -C <tmpdir>`, then
   `docker run --rm -v <tmpdir>:/src -w /src -e GOTOOLCHAIN=local golang:1.25-bookworm go build -o zfsbackup .`
   and copies the binary to `tmp/zfsbackup-$(REV)` (`tmp/` is gitignored).
   Check first whether `master` vendors its modules (`git ls-tree master
vendor | head -1`); if not, add `-e GOFLAGS=-mod=mod` and a
   `-v /tmp/zfsb-gomodcache:/go/pkg/mod` mount.

2. Add `testdata/manifests/make-fixture.sh`. It writes a `zfs` shim script to
   a temp dir and runs the old binary against it:

   ```sh
   #!/bin/sh
   # zfs shim: one dataset, two snapshots, a fixed stream. Logs every call.
   echo "$@" >> "$SHIM_LOG"
   case "$1" in
     list) printf 'tank/data@b\t1790899200\tsnapshot\ntank/data@a\t1788220800\tsnapshot\n' ;;
     get)  case "$*" in *@b) echo 1790899200 ;; *) echo 1788220800 ;; esac ;;
     send) head -c 65536 /dev/zero ;;
     *)    exit 1 ;;
   esac
   ```

   (1788220800 is 2026-09-01T00:00:00Z; 1790899200 is 2026-10-02T00:00:00Z.)
   Then, with `OLD=tmp/zfsbackup-master`:

   ```sh
   $OLD send --zfsPath $shim --workingDirectory $wd --volsize 1 tank/data@a file://$out
   $OLD send --zfsPath $shim --workingDirectory $wd --volsize 1 -i tank/data@a tank/data@b file://$out
   ```

   `master` has `--zfsPath` and `--workingDirectory` (`git show master:cmd/root.go`).
   If a call fails, read `$SHIM_LOG`: the shim must answer every invocation the
   old binary makes. Extend the `case` rather than the fixture by hand.

3. Commit the result as `testdata/manifests/master-b733e96/` (two manifests,
   two volumes, a few KiB) with a `README.md` that names the command, the
   revision and the shim's epochs. Commit the script and the Makefile target.

### Test

File: `e2e_old_destination_test.go` (root). `TestE2EOldDestinationReads`:

1. `newE2EEnv`. Copy `testdata/manifests/master-b733e96/` into `env.dest`.
   Pool rows: `tank/data@a` at 1788220800 and `tank/data@b` at 1790899200.
2. `list` in-process (there is no `env.list`; add one after `env.receive`'s
   pattern, with `cmd.ResetListJobInfo`). Assert both sets are printed.
3. `receive --auto tank/data@b dest restored/data` with
   `FAKEZFS_RECEIVE_LOG`. Assert two lines, each starting with `65536 ` (the
   fake receive logs bytes and a hash; the hash is of the shim's zeros, not of
   `fakezfs.Stream`, so do not use `checkRestores`).
4. `clean --dry-run=true --cleanLocal=false dest`: assert `would delete 0
objects` (add an `env.clean` helper the same way).
5. Smart send: `env.send("--fullIfOlderThan", "8760h", "tank/data", dest)`
   with the pool as above. Assert the no-op notice (`Nothing new to back up`
   or the current wording) and exit 0.

Why it may fail today: unknown. That is the point. If every step passes, the
item is done with the fixture and the test. If a step fails, the failure names
the incompatibility.

### Fix (only if the test fails)

Fix the READER to accept the old derivation (for example a missing `Separator`
defaulting to `files.DefaultSeparator` in `StoredManifestObjectName`, or an
older pgp-extension rule). Never regenerate the fixture to match the new code,
and never relax `manifestNamed` to accept any name.

---

## Docs and changelog (last commit)

- `CHANGELOG.md`, top block (the unreleased entries without a hash), one line
  per item in the existing style:
  - `[FIX] receive: a snapshot that is already on the target is a no-op; a local snapshot with the backed-up name but another creation time is refused, with or without --force`
  - `[FIX] errors: a manifest encrypted or signed with another key names the key problem and never advises deleting the object; its cached copy stays`
  - `[FIX] clean: a sibling --manifestPrefix that starts with a symbol (manifests@v2) is not read as ours; its sets are spared`
  - `[FIX] send: --resume finalizes a set whose every volume was cached before the kill` (only if Item 3 needed a fix; else `[ADD] test: ...`)
  - `[CHANGE] s3: the Glacier restore window follows the bytes to download (AWS_S3_RESTORE_DOWNLOAD_RATE, default 50 MiB/s; AWS_S3_RESTORE_DAYS overrides), was 3 days`
  - `[ADD] test: a destination written by master (testdata/manifests/master-b733e96) reads back in list, receive --auto, clean and smart send; make build-rev`
- `docs/context/receive.md`, `clean.md`, `backends.md`, `testing.md` (the
  fixture and `build-rev`), README S3 variables.
- `.claude/napkin.md`: what each item taught.

## Done when

- Every new test failed before its fix (output in the commit message) and
  passes after, except Items 3 and 7 where "passed first time" is an allowed,
  documented outcome.
- `make test-docker`, `make fmt-check`, `make lint` are green.
- The whole root package was run in one go (`make test-run PKG=.`).
- `git status` shows nothing unstaged that is not yours (another session may
  share this tree; stage files by name).

---

## Implementation notes (2026-10-11)

Executed on `clean-dry-run`, one commit per item: caed77d (1), f81f81c (2), 8380e26 (5),
707b9df (3), 3fbe1e1 (4), e73af4e (7), plus 97091d8 (`make fmt-write`). Deviations:

- **Item 1.** The incremental-source check runs for an explicit receive only. The fake zfs
  does not add a received snapshot to its listing, so in `--auto` the check would fail on
  step 2 of a chain. The chain walk already checks the source. `noteLocalSnapshotMismatch`
  now wraps `localSnapshotMismatch`, which returns the sentence; the explicit path logs it as
  an error. Sub-tests cover `--fullPath` and `--lastPath`.
- **Item 2.** The advice says "most likely a backup of yours written with other keys or other
  --encryptTo/--signFrom flags ... Do not delete it unless you know it is not a backup". Some
  KeyErrors are "not signed" / "not encrypted", which can also be a forgery, so "It is a
  backup of yours" was too sure. The unit test uses the `cacheBackend` mock (it counts
  downloads) rather than a `FileBackend`. `env.list` lives in `e2e_list_test.go`.
- **Item 5.** `TestSyncCacheRetriesTruncatedDownload` used the object name `manifests|x` (no
  set part, no `.manifest`); it now uses a real manifest name.
- **Item 3.** Passed first time: test-only. What finalizes the set: `tryResume` keeps all
  volumes; `sendStream` skips the stream and returns at the `volNum > 1` Peek-EOF guard; the
  forwarder's deferred `pending.done()` takes `pending` (started at 1) to zero when `startCh`
  closes; the final-manifest goroutine saves from the kept `jobInfo.Volumes`. The partial and
  the final manifest share one cache file (`partialManifestCachePath`).
- **Item 4.** `classifyRestores` HEADs every key before the first RestoreObject (the days
  depend on the total). A `restoreWindow` holds rate, override and the days the first round
  fixes. The rate is capped at 64 TiB/s so `rate * 86400` cannot overflow. Extra test:
  `TestS3PreDownloadRejectsBadRestoreEnv`. The red run used a stub `restoreDays` that returns
  3, because without the function the package does not build.
- **Item 7.** Passed first time: no incompatibility. Proven able to fail by breaking
  `manifestNamed` in place (all four sub-tests failed). `build-rev` makes a Linux binary, so
  the script runs in the same image through `make fixture-old REV=master`. `env.clean` wraps
  `cleanS3In` (it works for any URI).
- **Host.** `TestScenarioAdoptCreationTimes` and `TestE2EPlanManifestsFileAdoptsCreationTimes`
  fail on the host at HEAD too (zone EDT); they pass in Docker.

## Review follow-up (2026-10-11)

An adversarial review of 26cdb48..525b133 found these; all are fixed:

- **C1 (item 2).** Keeping the cached copy for every `KeyError` trapped the operator: a planted
  unsigned object blocked `list`, `clean` and smart `send`, the advice said to keep it, and
  after a delete the kept copy blocked `clean` as a local-only manifest. Now `KeyError` has
  `NeedsSecretKey` (only "encrypted to no key in the secret ring" and a locked key). Only those,
  and `ErrManifestTooLong`, keep the copy (`keepsCachedCopy`), and that error names the cache
  path. The others say to delete the object if you did not write it.
- **W1 (item 1).** The `--auto` skip of the source check came from a limit of the fake, with a
  comment that gave another reason. The fake `zfs receive` now adds the received snapshot to
  `FAKEZFS_SNAPSHOTS`, found by matching the stream with `StreamSalted` of each fixture pair.
  The check runs in `--auto` too. Shim streams (the master fixture) need
  `FAKEZFS_RECEIVE_UNKNOWN`. `checkRestores` puts the fixture back after each receive.
- **N2.** The collision error says to `zfs rename` the local snapshot (not destroy: it holds
  other data) or receive elsewhere.
- **N3.** The restore notice says "as AWS_S3_RESTORE_DAYS sets" for an override, and gives no
  days when only Intelligent-Tiering objects are restored.
- **N4.** `Init` checks the restore variables, so a bad value fails every s3 command at once.
  Trade-off: a bad value set for a restore also stops `send`.
- **N5.** `build-rev`: `REV` must be a commit (checked first), paths are quoted, and `/`
  becomes `-` in file names. Checked with `make build-rev REV=origin/master`.
