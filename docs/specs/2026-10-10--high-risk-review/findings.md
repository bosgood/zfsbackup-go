# High-risk code review: findings

Date: 2026-10-10
Branch: `clean-dry-run` at `b516dfa`
Plan: `plan.md` in this directory. Nothing was fixed in this pass.

Method: items 1 to 3 each got a general-purpose agent working in a scratch copy
(`git archive HEAD | tar -x -C /tmp/hrr-itemN`) and running new tests in the
pinned `golang:1.25-bookworm` image. Items 4 to 6 were decided by reading, plus
a throwaway host-Go program for the DST arithmetic. Every agent claim below was
checked against the code before it was written here.

Summary:

| Item                                                                                            | Verdict                                                                                                                                         | Risk                                           |
| ----------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- |
| 1. `clean --cleanLocal` deletes an unreadable cached manifest of another prefix                 | CONFIRMED                                                                                                                                       | data loss                                      |
| 2. 64 MiB manifest limit rejects a large valid backup                                           | CONFIRMED (at 128,558 volumes; 24.5 TiB at the default volsize). Consequence differs from the plan: every reader fails hard, nothing is skipped | destination unusable; data loss one step later |
| 3. `--resume` trusts the cache of destination 0                                                 | REFUTED (A, B, D); C is a documented limitation                                                                                                 | none                                           |
| 4. `receive --auto` matches the local source by name only                                       | REFUTED                                                                                                                                         | none                                           |
| 5. S3 `PreDownload` re-restore                                                                  | REFUTED                                                                                                                                         | none                                           |
| 6. `plan` DST arithmetic                                                                        | REFUTED                                                                                                                                         | none                                           |
| Other 1. plain `clean` under one prefix deletes the same dataset's volumes under another prefix | CONFIRMED (seen in a test's setup step)                                                                                                         | data loss                                      |

## Item 1: `clean --cleanLocal` deletes an unreadable cached manifest of another prefix

Verdict: **CONFIRMED**. Three new tests fail on `b516dfa`.

Tests (scratch copy only, `/tmp/hrr-item1/backup/clean_hrr1_test.go`):

| Test                                                                                         | Cached p2 manifest | p2 volumes at the destination |
| -------------------------------------------------------------------------------------------- | ------------------ | ----------------------------- |
| `TestHRR1CleanLocalTruncatedOtherPrefixOtherDataset` (truncated cache file, datasets differ) | deleted            | kept                          |
| `TestHRR1CleanLocalTruncatedOtherPrefixSameDataset` (truncated cache file, same dataset)     | deleted            | deleted                       |
| `TestHRR1CleanLocalWrongKeyOtherPrefix` (p2 encrypted to another key, same dataset)          | deleted            | deleted                       |

Deciding lines in `backup/clean.go`:

- `:137` the undecodable local-only manifest is an error only when `cleanLocal` is false.
- `:149` with `--cleanLocal` it continues with `decodedManifest = nil`.
- `:151` `foreign` needs a decoded manifest, so a nil one is never foreign.
- `:153-154` a nil manifest adds no dataset, so it protects nothing.
- `:322` not foreign and not provably busy, so it is not kept as live.
- `:330` it goes to `localDeletes`; `:521-522` removes it after the remote deletes.
- `:385` the volumes survive only when no decoded manifest names their dataset.
  When the same dataset also has a set under this run's prefix, the other
  prefix's volumes pass that filter, are in no `keep` set, and are deleted as
  orphans.

With the wrong key, `readManifest` returns a `*files.KeyError`
("is encrypted to no key in the secret ring"). `readForClean` does not look at
the error type; `tryResume` does (`backup/backup.go:1285`) and refuses instead
of discarding.

Dry run: logs `Would delete local manifest <path>` for the foreign file
(`clean.go:327`) and deletes nothing. The comments at `clean.go:129-132` and
`:170-173`, the changelog line for d76ef3a and the runbook (line 233, "never
one cached under another `--manifestPrefix`") all promise what the code does
only for decodable manifests.

Also, with a nil manifest the busy check at `:322` cannot run, so the partial
manifest of a RUNNING send under another key, or one truncated mid-write, is
deleted while that send holds its lock. Reasoned from the code, not tested.

User-visible effect: a `clean --cleanLocal` under one `--manifestPrefix` or
with one key silently removes another job's cached manifest, and when both jobs
back up the same dataset, that job's volumes at the destination too.

Which rule should win: "never delete what you cannot read". An undecodable
local-only manifest cannot be attributed to this prefix, so keep it, say why,
and let the user delete the file by hand. A `KeyError` in particular is a
wrong-key signal, not a damaged-cache signal.

## Item 2: the 64 MiB manifest limit rejects a large valid backup

Verdict: **CONFIRMED**, with a different consequence than the plan assumed.

Measured (`/tmp/hrr-item2/files/manifest_hrr2_test.go`, copy in `tests/`;
two tests, both PASS, they measure rather than assert a fix):

| Quantity                                              | Value                                                                          |
| ----------------------------------------------------- | ------------------------------------------------------------------------------ |
| JSON per volume (realistic values, zone-offset times) | 517.6 B at low numbers, 522.0 B at 6-digit volume numbers                      |
| Smallest volume count `ReadManifest` rejects          | 128,558 (67,109,386 B JSON; limit 67,108,864)                                  |
| Largest count that reads back                         | 128,557                                                                        |
| Error text                                            | `manifest ... is longer than 64 MiB` (`files/manifest.go:53-54`)               |
| Dataset size at `--volsize 200` (default)             | 24.52 TiB in one full                                                          |
| At `--volsize 100` / `50` / `20`                      | 12.3 / 6.1 / 2.5 TiB (`--volsize` has no minimum, `cmd/send.go:87-93`)         |
| Compressed manifest object at the limit               | 16.1 MiB (gzip level 6, forced for manifests at `files/volumeinfo.go:640-643`) |

Where the plan's model is wrong: `downloadTo` caps the COMPRESSED object
(`backup/restore.go:623-624`), and a real over-limit manifest is 16 MiB
compressed, so the `errObjectTooLarge` skip in `syncCache`
(`backup/sync.go:198-201`) never fires for it. The manifest name is in
`atDestination` (`sync.go:147`), the download succeeds, and every reader then
calls `readCachedManifest` (`sync.go:276-313`), which fails in
`files.ReadManifest`, removes the cached copy (`:290`), downloads again,
fails again, removes again (`:305-307`) and returns `unreadableManifestError`
(`:317-324`). About 32 MiB of egress per run per reader, no progress.

Per reader, after one full send of 24.5 TiB or more:

- `clean`: `readForClean` returns the first error (`backup/clean.go:116-120`),
  `Clean` returns it. Refuses for the WHOLE destination, every dataset, whether
  or not other manifests exist. Fail-safe, but `clean` is unusable there.
- Smart `send` and `plan`: `getBackupsForTarget` (`backup/backup.go:255-261`)
  returns the error; `ProcessSmartOptions` fails. Every smart send of that
  dataset fails. Other datasets are unaffected (`manifestMayBeFor`).
- `receive --auto`: `readAndSortManifests` (`backup/list.go:149-152`) fails,
  so `--auto` of ANY snapshot of that dataset fails, old healthy backups
  included. An explicit `receive` of an older set still works; the big set
  itself cannot be restored by any means.
- `list`: reads every manifest (`backup/list.go:65-73`); fails for the whole
  destination.
- `send` write path: no check. `saveManifest` (`backup.go:1008-1045`),
  `CreateManifestVolume` and `prepareVolume` have no limit; `MaxManifestBytes`
  has zero references on the send side. `refuseExistingSet` (`backup.go:737-765`)
  compares object names only, so the next run of the same set says "already
  exists ... Delete it at the destination to send it again".
- `--resume` of the interrupted big send: the partial cache is also over the
  limit; `tryResume` (`backup.go:1288-1291`) logs "Could not read previous
  manifest file ...; starting over" and discards 24.5 TiB of uploaded state.

The step after the error is the data loss. The error text
(`sync.go:319-321`) says "If you did not write it ... delete that object from
the destination". The user did write it. Once the manifest object is deleted,
the 128,558 volumes are orphans; if the dataset has any other manifest at the
destination or in the cache, `datasets[dataset]` is true (`clean.go:127`) and
the next `clean` deletes all of them (`clean.go:375-386`, not in `keep`).
Only when it was the dataset's sole manifest do `clean.go:263` or `:383`
protect them.

Existing coverage tests only forgeries: `MaxManifestBytes+1` zero bytes
(`backup/receive3_test.go:37-50`), a 300 MiB sparse junk object
(`receive3_test.go:54-82`), `{},` x 262,145 (`files/manifest3_test.go:56-63`),
junk under another dataset's name (`e2e_receive3_test.go:77-103`). No test
writes a `CreateManifestVolume` manifest with more than a few volumes. The
comment at `files/manifest.go:32-34` justifies the limit with the e2e suite's
2.6 KB manifests. `MaxManifestVolumes` (262,144) never fails first for real
volumes; the byte limit fails at about half that.

User-visible effect: one full backup of 24.5 TiB or more at the default
volsize (6 TiB at `--volsize 50`) completes, then locks the destination out
of `clean`, `list`, `plan`, smart `send` and `receive --auto`, cannot itself be
restored, and the advice in the error leads to its volumes being deleted.

## Item 3: `--resume` trusts the cache of destination 0

Verdict: **REFUTED** for A, B and D. **C is a documented limitation**, not a
defect. Tests in `/tmp/hrr-item3/e2e_hrr3_test.go` (copy in `tests/`), all
PASS on `b516dfa`, plus the existing suite.

| Hypothesis                                                                     | Verdict               | Test                                                                                                                                               | Deciding lines (`backup/backup.go`)                                                                                                                          |
| ------------------------------------------------------------------------------ | --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| A. a plain send to one destination rewrites volumes between attempt and resume | REFUTED               | new `TestE2EHrr3ACacheOfOtherDestinationCuts` (rewritten destination first AND second); existing `TestE2EResumeDoesNotTrustOtherDestinationsCache` | `:1318` then `:1387` (`SHA256Sum` differs, `keep = n`), `:1324` "Nothing verifiable to resume; starting over."                                               |
| B. that send fails before caching anything                                     | REFUTED               | new `TestE2EHrr3BUncachedPlainSendStartsOver` (both destination orders)                                                                            | `:476-481` (`discardPartialManifests` runs before any upload), `:1282` (no cache at dest 0 discards every cache), `:1375` (no cache at dest n keeps nothing) |
| C. a send from another `--workingDirectory` or host                            | DOCUMENTED LIMITATION | new `TestE2EHrr3COtherWorkingDirectoryIsInvisible`                                                                                                 | `:1358-1361` (comment says so), `:1426-1448` (size only), `:1362-1396` (caches of this working directory only; `cacheDirFor` in `backup/sync.go:72-74`)      |
| D. other gaps in `tryResume`                                                   | REFUTED               | reading                                                                                                                                            | `:1436` (non-Sizer skip), `:1329` (final-manifest reuse)                                                                                                     |

A. The intermediate send used `FAKEZFS_STREAM_SALT` with no encryption, so
the volumes at the rewritten destination changed bytes but kept names and
sizes (asserted byte-for-byte: exactly two objects changed). `verifiedVolumes`
keeps them (present, right size); `cachedAtEveryDestination` cuts at volume 1
on `SHA256Sum`, in both orders. The log carries `The cached state for ...
differs from the one for ... at volume ...`, and `receive` from both
destinations verifies. The existing test uses PGP encryption, so attempt B's
bytes differ by session key: it passes for the right reason, but covers only
the order where the rewritten destination is first and does not assert the
Notice.

B. `discardPartialManifests` removes and fsyncs the cache of every destination
of the run before the pipeline starts. Resume with the uncached destination
first hits "No previous manifest file exists" and discards all caches; with
it second, `cachedAtEveryDestination` finds no cache for it and keeps nothing.
Both orders restore correctly.

C. From working directory W2, a salted plain send rewrote volumes 1 and 2 at
BOTH destinations and failed at 3. `--resume` from W1 logged `Resuming from
volume 3: 2 of 3 cached volumes verified at all destinations.` and published
a manifest at both; `receive` then fails at each with
`tank/data|a.zstream.vol1 does not match its manifest: got 1048576 bytes with
SHA256 203ca7..., want ... 6699ec...` (`backup/restore.go:506-521`). The code
comment at `:1358-1361` states this. The runbook (lines 258-262, 265-268, 280) promises existence checks at every destination and says the lock is per
working directory; it promises nothing about content, so no contradiction.
The restore fails closed, so the bad manifest is caught, not silently trusted.

D. `backends.Sizer` is implemented by file and ssh only; `backends/backends.go:48-50`
explains that object stores expose an object only once its upload completes,
so the `sizer == nil` skip is not a gap. The final-manifest shortcut at `:1329`
needs every cached volume to pass both cuts, so dest 0's cache is then
correct for every destination (modulo C).

Doc gap only: CHANGELOG line 4 and the runbook are silent on other working
directories and hosts. One runbook sentence ("run every `send` of a dataset
from one working directory and one host; `--resume` cannot see sends from
elsewhere") would close the gap with the comment at `backup.go:1360`.

## Item 4: `receive --auto` matches the local source by name only

Verdict: **REFUTED**. The match is name plus creation time, and that pair
changes when a snapshot is recreated.

- `files/jobinfo.go:133-138` `SnapshotInfo.Equal` compares `Name` and
  `CreationTime` (second granularity). GUID is not compared.
- `zfs/zfs.go:69` the local listing is `zfs list -o name,creation,type`, so
  local snapshots carry no GUID to compare with. Manifests record GUIDs only
  since this branch (`backup/backup.go:680`), for `--resume`.
- A `zfs rollback` followed by a new snapshot of the same name gets a new
  creation time, so `validateSnapShotExistsFromSnaps` (`backup/sync.go:349`)
  and `sourceIsLocal` (`backup/restore.go:233`) say "not here". The chain walk
  continues to the full, and `zfs receive` of a full stream into a dataset
  that has snapshots fails with zfs's own "destination has snapshots ... must
  destroy them to overwrite it", with or without `-F`.
- Received snapshots keep the source's creation time and guid (OpenZFS
  `dmu_recv_end_sync` copies `drr_creation_time` and `drr_toguid`), so the
  comparison is stable across send and receive. The ZFS integration test
  (`integration_test.go:401-422`, receive `@a`, `@b`, then `--auto`) depends
  on exactly this and passes upstream.

The only way to hit the hypothesis is a different snapshot with the same name
AND the same creation second. A rollback cannot produce that.

Error quality: the user sees zfs's message through `receiveStream`
(`backup/restore.go:610`). It does not say that the local snapshot of that
name is not the backed-up one. Safe, but it does not name the real cause.

## Item 5: S3 `PreDownload` re-restore

Verdict: **REFUTED** on all three questions, by reading
`backends/aws_s3_backend.go`.

1. A restored copy that expires within `restoreExpiryMargin` (24h) takes the
   `extend` branch (`:317-321`). It is NOT appended to `toRestore`, so the wait
   loop (`:371-395`) never polls it. No delay.
2. `RestoreAlreadyInProgress` (`:345`) is reached in two states. In the default
   branch the key was already appended to `toRestore` (`:324`) before the
   request, so it IS waited for. In the `extend` branch the copy is readable,
   so skipping the wait is right. Nothing non-readable is treated as readable.
3. `restoreExpiresBefore(nil)` returns false (`:434`), but it is only called
   when `restored(resp)` is true (`:317`), which needs a non-nil header with
   `ongoing-request="false"` (`:416-419`). A HEAD with `ongoing-request="true"`
   and no expiry goes to the default branch, where `restoreInProgress`
   (`:325`) skips the request and the key waits in the loop.

AWS documents a re-issued restore on a restored copy as a 200 that moves the
expiry, and a 409 `RestoreAlreadyInProgress` while a retrieval runs. The docs
do not say what `x-amz-restore` shows in the seconds after an extension; if
it ever showed `ongoing-request="true"`, a LATER `PreDownload` call on the
same keys (`verifyVolumesAt` then `Receive`, `backup/backup.go:899` and
`backup/restore.go:330`) would wait for a readable copy. Left as a residual
uncertainty, not a finding: there is no real S3 test.

## Item 6: `plan` DST arithmetic

Verdict: **REFUTED** on all three cases. Checked with a throwaway Go program
(`/tmp/hrr-item6/main.go`, `America/New_York`, host Go, not committed) that
copies `wallClock` and the `nextRun` arithmetic from `backup/plan.go:461-485`.

1. `Every` 48h from 2026-03-05 02:30 EST: runs on 03-07 02:30 EST, 03-09
   02:30 EDT, 03-11, 03-13. Across the 2026-11-01 fall-back from 10-29 01:30:
   10-31, 11-02 01:30 EST, 11-04. The `+12h` rounding absorbs the one-hour
   shift either way. A 24h run at 02:30 on the gap day lands on 03:30 EDT
   and returns to 02:30 the next day.
2. `wallClock(2026, 11, 1, 1, 30, ..., NY)` returns 01:30 EDT (-0400), the
   first instance; `time.Date` agrees there. In the spring gap `time.Date`
   gives 01:30 EST and `wallClock` corrects it to 03:30 EDT, as the comment
   says.
3. `boundaries` for monthly (`backup/plan_schedule.go:218-219`) starts `t` at
   the 1st of `from`'s month, so `AddDate(0, 1, 0)` walks Jan 1, Feb 1, Mar 1,
   Apr 1. The Jan 31 to Mar 3 case cannot happen.

## Other

1. **Plain `clean` under one prefix deletes the same dataset's volumes stored
   under another prefix. CONFIRMED, data loss, pre-existing.** Seen in the
   setup step of the Item 1 tests: `clean --manifestPrefix p2` (no
   `--cleanLocal`, no `--force`) logged `Deleted .../tank/data|a.zstream.gz.vol1`,
   a volume of the set under the default prefix. Cause: `syncCache`
   (`backup/sync.go:134`) lists `j.ManifestPrefix` only, so manifests under
   another prefix AT THE DESTINATION are never read. The "foreign" protection
   (`clean.go:151`) covers only manifests that happen to sit in this host's
   cache. Dataset `tank/data` is in `datasets` through the p2 manifest, so the
   default-prefix volumes pass the `:385` filter, are in no `keep` set, and are
   orphans. Manifest objects survive only because they do not parse as volumes.
   The existing `TestCleanLocalSparesOtherPrefixManifests` uses two datasets,
   so it never sees this. The runbook (line 233) and the changelog imply two
   prefixes can share a destination; today that is unsafe for every `clean`
   whenever the two jobs cover one dataset.
2. The warning at `clean.go:148` reads the same for a wrong key and a
   truncated file. A user with a rotated key cannot tell the two apart.
3. The `receive --auto` refusal in Item 4 is zfs's message, not the tool's; a
   Notice naming the local snapshot that did not match (same name, other
   creation time) would turn a confusing failure into a clear one.
4. `unreadableManifestError` (`backup/sync.go:317-324`) tells every user to
   delete the object. For Item 2 that advice destroys the only record of a
   valid backup. The error should distinguish "longer than the limit" from
   "not a manifest" and must not suggest deletion for the former.
5. Each failed read of an over-limit manifest downloads it twice and removes
   the cached copy twice (`sync.go:288-296`, `:305-307`), so retries never
   converge and cost egress.
6. The existing resume test `TestE2EResumeDoesNotTrustOtherDestinationsCache`
   covers one destination order and asserts only on the restore. The scratch
   test `TestE2EHrr3ACacheOfOtherDestinationCuts` covers both orders and the
   Notice line; worth porting.

## Artifacts

- `tests/backup_clean_hrr1_test.go.txt`: three failing regression tests for
  Item 1 (`package backup`). They copy two key helpers from `files/pgp_test.go`
  because `backup/` cannot import them.
- `tests/files_manifest_hrr2_test.go.txt`: the Item 2 measurement tests
  (`package files`).
- `tests/root_e2e_hrr3_test.go.txt`: the Item 3 tests (`package main`).
- `tests/plan_dst_check_main.go.txt`: the Item 6 throwaway program.
- Saved as `.txt` so `go vet ./...` does not compile them. Drop the suffix and
  move them into their packages for the fix pass.

Docker command used by the agents (from the napkin), with each scratch copy
mounted at `/src`:

```
docker run --rm -v /tmp/hrr-itemN:/src -v /tmp/zfsb-gocache:/root/.cache/go-build \
  -w /src golang:1.25-bookworm bash -c 'timeout 500 go test -count=1 -run "TestHRR" -v ./backup/'
```
