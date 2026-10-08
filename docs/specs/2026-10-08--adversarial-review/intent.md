# Intent: fix the 2026-10-08 adversarial review findings

**Date:** 2026-10-08. **Branch:** `clean-dry-run` at `4eee1c3` (103 commits, 216 files,
+37121/-594 against `master`).

**What happened:** four parallel reviewers (send; clean/restore/backends; plan;
security/infrastructure) reviewed the whole branch and reported 5 CRITICALs, 9 WARNINGs
and 11 NOTEs with a BLOCK verdict. Their repro tests were left in `/tmp/advrev-{send,clean,
plan,sec}`.

**What this session did:** re-ran every reviewer test against clean copies of `4eee1c3`
(only the test files differ from the working tree) and read every cited line. Results:

- All 5 CRITICALs reproduce (14 of 15 reviewer tests fail as claimed; the 15th,
  `TestAdvRevS3ReceiveMissingVolume`, passes and was never claimed as a defect).
- All 9 WARNINGs hold: 7 reproduced by a test, 2 (`cmd/plan.go` raw URI on error; the
  partial-set completion download) confirmed by reading the code.
- 10 of 11 NOTEs hold. The one that does not: "one of receive's channels is accidentally
  unbuffered" — every `make(chan ...)` in `backup/restore.go` (lines 279, 280, 286, 315)
  has a buffer. Dropped.
- "No regressions of the 2026-10-05 fixes": the full suite at `4eee1c3` is green
  (`go test ./...` in the pinned image, 7 packages ok), which is as far as that claim can
  be checked without the earlier review's tests.
- Nuance on CRITICAL 4: the 2026-09-29 review already recorded "manifest signatures never
  checked (json.Decoder stops before EOF)", so "worse than recorded then" overstates it.
  There are two defects, both open: a signature is never *required*, and when one is
  present the manifest reader never reaches the EOF where it would be *verified*.

**Goal:** fix all five CRITICALs and all nine WARNINGs, test-first, turning the `/tmp`
tests into permanent regression tests; batch the NOTEs into small commits. Order: the
`receive --auto` fix first (it is the runbook's recovery path and only shows at restore
time), then the legacy lock and the resume cache.

**Non-goals:** the still-open items of earlier plans (send-path Tasks 10–15, the
backends-bugs plan, the plan-audit leftovers: bookmarks in simulations, a full roll while
one destination is behind, `plan` during a running `send`). Where a task here overlaps
one of those, the plan says which one wins.

**Assumption to confirm with the user:** no build older than `dc37d16` (lock moved out of
`/tmp`) is installed anywhere a send could still be running. The first real offsite backup
has not run yet, so this should hold. If it does not, keep the legacy probe for one
release but make it ignore pid 1 and any file not owned by the current user.
