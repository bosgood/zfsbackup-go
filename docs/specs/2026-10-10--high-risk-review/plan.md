# High-risk code review: investigation plan

Date: 2026-10-10
Branch: `clean-dry-run` (144 commits ahead of `master`)
Source: triage of `CHANGELOG.md` "Unreleased" against the code.

## Goal

Confirm or refute six suspected defects in the unreleased changes. Items 1 to 3
can delete a backup or reject a valid one. Do them first, in order. Items 4 to 6
are lower risk.

For each item, report one of:

- **CONFIRMED**: a test or a trace shows the defect. Give the test name and the
  exact line that is wrong.
- **REFUTED**: the code handles the case. Give the line that handles it.
- **OPEN**: you could not decide. Say what blocked you.

Do not fix anything in this pass. Write the report to
`docs/specs/2026-10-10--high-risk-review/findings.md`.

## Rules for this repo

Read `.claude/napkin.md` first. The rules that matter here:

- Build and test through the Makefile. Do not run `go test` on the host.
  - One test: `make test-one RUN=TestName PKG=./backup/`
  - Full suite: `make test-docker`
  - Format check: `make fmt-check`
- A new regression test must FAIL on the old code. Prove it: copy the file to
  `/tmp`, re-insert the old code, run the test, see it fail, then put the fixed
  file back. A test that passes before and after proves nothing.
- `clean` builds its backend from the URI. Use a real `file://` destination in
  a temp dir. See `backup/clean_test.go` for the pattern.
- Package globals such as `cleanDryRun` and `cleanLocal` survive between
  in-process cobra runs. Pass every boolean flag on every run.
- Do not put a comma in a subtest name that uses `t.TempDir()` as a destination.
- In root-package e2e tests, assert only on Notice-level or higher log lines.
- Another session may edit this repo. Run `git status` before you stage, and
  stage files by name.

## Item 1: `clean --cleanLocal` deletes an unreadable cached manifest of another prefix

Risk: data loss. Priority: highest.

Files:

- `backup/clean.go:120-160` (`readForClean`, local-only manifests)
- `backup/clean.go:292-330` (the `foreign` count and `localDeletes`)
- `backup/clean.go:360-440` (orphan and `--force` decisions)

What the changelog claims:

- `clean` never deletes a cached manifest of another `--manifestPrefix`, and
  keeps its volumes.

What the code does:

- Line 137: an undecodable local-only manifest is an error only when
  `cleanLocal` is false. With `--cleanLocal` it continues with
  `decodedManifest = nil`.
- Line 152: `foreign` is computed from the decoded manifest. A nil manifest is
  never foreign.
- Line 321: a manifest that is not foreign, not busy and not decodable goes to
  `localDeletes`.
- Its volumes are known to no manifest, so the orphan pass deletes them.

Hypothesis: a cached manifest that belongs to another prefix AND cannot be
decoded by this run (wrong key, truncated file, old format) is deleted by
`clean --cleanLocal`, and so are all its volumes at the destination.

How to test:

1. Send dataset A with `--manifestPrefix p1` and key K1 to a `file://` dest.
2. Send dataset B with `--manifestPrefix p2` and key K2 to the same dest, so
   that the cache holds a manifest only K2 can read.
3. Remove the `p2` manifest object from the destination, so that the cached one
   is local-only.
4. Run `clean --manifestPrefix p1 --cleanLocal` with key K1.
5. Check: are the volumes of B still at the destination? Is the cached `p2`
   manifest still in the cache?

Also check the simpler case: no keys, but the cached file is truncated to half
its size.

If CONFIRMED, say which rule should win: "never delete another prefix" or
"`--cleanLocal` deletes what it cannot read".

## Item 2: the 64 MiB manifest limit rejects a large valid backup

Risk: a valid backup becomes invisible. Priority: high.

Files:

- `files/manifest.go` (`MaxManifestBytes`, `MaxManifestVolumes`, `readAtMost`)
- `files/volumeinfo.go:69-84` (the JSON fields of one volume)
- `backup/sync.go:195-205` (`syncCache` skips a too-large manifest)
- `backup/restore.go:619-665` (`downloadAtMost`)
- `backup/clean.go:263` (the "no manifests" refusal)

The numbers:

- One volume is about 500 to 550 bytes of JSON: two 64-hex sums, an MD5, two
  RFC3339 times, the object name and the counters.
- 64 MiB / 520 B is about 129,000 volumes.
- The default `--volsize` is 200 MiB (`cmd/send.go:176`). So one send of
  about 25 TiB hits the limit.

Hypothesis: a full send of a dataset of 25 TiB or more writes a manifest this
version cannot read. `syncCache` skips it, `receive --auto` does not see it,
and `clean` sees no manifest for that dataset. The code comment justifies the
limit with the 2.6 KB manifests of the e2e suite.

How to test:

1. Write a unit test in `files` that builds a `JobInfo` with 130,000 volumes
   of realistic field values, writes it with `CreateManifestVolume`, and reads
   it with `ReadManifest`. Measure the bytes. Report the volume count where it
   first fails.
2. Trace what `clean` does with the volumes of a skipped manifest. Are they
   orphans? Does the line 263 refusal protect them only when it is the ONLY
   manifest at the destination?
3. Check whether `send` refuses to write a manifest it cannot read back. If it
   does not, that is a second finding.

Report the smallest dataset size, with the default volsize, that triggers this.

## Item 3: `--resume` trusts the cache of destination 0

Risk: a published manifest names a volume with other bytes. Priority: high.

Files:

- `backup/backup.go:1267-1360` (`tryResume`)
- `backup/backup.go:1361-1410` (`cachedAtEveryDestination`)
- `backup/backup.go:1411-...` (`verifiedVolumes`)
- `backup/backup.go:978-1010` (`discardPartialManifests`)
- Tests: `e2e_resume_test.go`, `e2e_resume3_test.go`

What the changelog claims:

- A volume is kept only when the cache of every destination records it with
  the same number, name, size and SHA-256.

What the code does:

- `cachedAtEveryDestination` compares the caches of destinations 1..n to the
  cache of destination 0. It never compares a cache to the bytes at the
  destination.
- `verifiedVolumes` lists objects at each destination. Confirm whether it
  compares size only, or size and hash. Line 924 (`size != vol.Size || sum !=
vol.SHA256Sum`) is in the manifest-copy path, not the resume path.

Hypotheses, in order:

A. A send to destination 0 alone, from this host, between the interrupted
attempt and the resume, rewrites the same-named volumes. Then the cache of
destination 0 describes the new bytes, the cache of destination 1 the old
bytes, and the comparison catches it. Confirm with a test.

B. The same send, but it fails after it uploads volume 3 and before it caches
the manifest. Does `discardPartialManifests` run before the upload, so that
the cache of destination 0 is empty and the resume starts over? Confirm the
order of operations at `backup/backup.go:481`.

C. A send from another working directory (`--workingDirectory`) to the same
destinations. The code comment says the lock does not cover this. Confirm
that `verifiedVolumes` does not catch it either, and say so in the report as
a documented limitation, not a defect, unless the runbook claims otherwise.

Check `docs/runbook-first-backup.md` for what it promises about resume.

## Item 4: `receive --auto` matches the local source by name only

Risk: low. It fails safely, but the error may be confusing.

Files:

- `backup/restore.go:160-175` (the `sourceIsLocal` exit in the chain walk)
- `backup/restore.go:218-250` (`backupThatApplies`, `sourceIsLocal`)
- `backup/sync.go:349` (`validateSnapShotExistsFromSnaps`)

Hypothesis: a local snapshot with the same name as the incremental source but a
different GUID (after `zfs rollback` and a new snapshot of the same name) makes
`AutoRestore` skip the parent. `zfs receive` then refuses the stream.

How to test:

1. Read `validateSnapShotExistsFromSnaps`. Does it compare GUID when both
   sides have one?
2. If name only: use `internal/fakezfs` to present a snapshot with the right
   name and a wrong GUID, and check which error the user sees.

Report whether the error names the real cause.

## Item 5: S3 `PreDownload` re-restore

Risk: cost and a hang. No real S3 test exists.

Files:

- `backends/aws_s3_backend.go:289-395` (`PreDownload`)
- `backends/aws_s3_backend.go:395-460` (`needsRestore`, `restoreInProgress`,
  `restoreDone`, `restoreExpiresBefore`)
- Tests: `backends/s3_restore_expiry3_test.go`, `backends/aws_s3_backend_test.go`

Read by hand. Check:

1. A restored copy that expires soon is restored again. Is the key then added
   to the wait loop? If yes, does the loop wait for a copy that is already
   readable? That is a delay of up to the full restore time for no reason.
2. The "RestoreAlreadyInProgress" branch at line 345. Does it treat the copy as
   readable when it is not?
3. `restoreExpiresBefore` parses the `expiry-date` field. What does it return
   for a header with no `expiry-date`? The comment says "does not expire".
   Confirm that a HEAD with `ongoing-request="true"` and no expiry is handled
   by `restoreInProgress` first.

Use the `httptest.Server` pattern from the napkin for HEAD and RestoreObject
if you need a test.

## Item 6: `plan` DST arithmetic

Risk: low. `plan` deletes nothing.

Files:

- `backup/plan.go:449-490` (`firstRun`, `nextRun`, `wallClock`)
- `backup/plan_schedule.go:137-235` (`Advance`, `boundaries`)
- Tests: `backup/plan_schedule_test.go`, `backup/plan3_test.go`

Check only these cases:

1. `nextRun` with `Every` of 48h over a spring-forward day. The `days`
   arithmetic rounds with a 12h offset. Does a 48h step land on the right day?
2. `wallClock` on a fall-back day at 01:30. Does it return the first instance?
   The comment says yes. Confirm with `time.LoadLocation("America/New_York")`.
3. `boundaries` for "monthly" when `from` is the 31st. `AddDate(0, 1, 0)` from
   Jan 31 gives Mar 3. Confirm `t` starts at the 1st so that this cannot
   happen.

## Order of work

1. Items 1 and 2 are independent. Do them in parallel if you spawn agents.
   Each agent works in its own `git archive HEAD | tar -x -C /tmp/...` copy.
   See the napkin, "Parallel fix agents without worktrees".
2. Item 3 next. It needs the e2e helpers in `e2e_test.go`.
3. Items 4 to 6 last. Reading is enough for most of them.

## Report format

For each item: the verdict, the test or line that decides it, and one sentence
on the user-visible effect. Put CONFIRMED items first. Put any new finding you
meet on the way in a final section "Other".
