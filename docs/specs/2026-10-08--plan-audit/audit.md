# Audit: runbook-first-backup and the plan scenarios

Written 2026-10-08 on `clean-dry-run` (c60498d). Every claim below was
checked by reading `backup/plan*.go`, `cmd/plan.go`, the e2e tests and the 20
scenarios, and by running `make plan ARGS=...` experiments (commands at the
end). Nothing here is a planner bug in the production configuration
(`--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
--incrementalSnapshotSuffix _monthly`, daily or weekly cron, `monthly>=3`):
that path is covered from several angles and behaved in every variation
tried. The findings are places where `plan` can say something `send` or cron
will not do, situations the runbook describes without a golden behind them,
and operational facts the runbook omits.

## A. Where `plan` can mislead

### A1. The simulated sanoid is punctual; the real one is 2-4 minutes late

`Schedule.Advance` takes every snapshot exactly at its boundary (00:00:00).
The real capture shows sanoid landing monthlies at `00:02:35`, `00:02:46`,
`00:03:36`. A run simulated at `from=...T00:00:00` therefore sees the monthly
taken "at" that instant (boundaries are `(from, to]`, and `visibleAt` keeps
equal times), while a cron at `0 0 1 * *` on the real host runs before sanoid
and sends the monthly a day (daily cron) or a week (weekly cron) later.
Experiment A shows `INCR` on `2026-10-01T00:00:00Z` for a midnight cron.

Every committed schedule runs at 01:00 or 03:00, so the goldens are right; the
runbook is what lets a user plan at the wrong time. Fix both:

- schedule key `snapshot-delay=5m` (default 0 keeps every golden byte-identical)
  that `Advance` adds to each boundary's creation time; one scenario with a
  midnight cron and the delay set, showing the monthly going out on the 2nd;
- runbook: "schedule the cron at least an hour after sanoid's boundary and
  put that wall-clock time in `from=`".

### A2. `plan` never plans `complete-partial`

`Scenario.run` calls `planSmartSnapshots(..., completable=false)`;
`ProcessSmartOptions` computes `completable` from `--resume` or from the
volumes present at the lagging destination. With two destinations where one
lacks the newest manifest (a failed manifest upload), `plan $DS $URI1,$URI2`
reports `ERROR want to do an incremental backup but last incremental backup at
destinations do not match` while `send` finishes the set (`complete-partial`).
`TestE2ESmartCompletesPartialWithoutResume` covers `send`; nothing compares it
with `plan`. Single-destination runs (the runbook) are unaffected.

Fix: `runPlan` with destination URIs computes `completable` the way
`ProcessSmartOptions` does (the backends are already open), and a next-run
e2e asserts plan == send for a partial set.

### A3. An explicit `--full` restart is a violation to `plan` but fine for `send`

Experiment I: with October backed up as `INCR Sep→Oct`, `plan --full
--fullSnapshotSuffix _monthly` prints `FULL ... explicit-full` and exits 2
(`no-duplicate-send`); `send --full` proceeds. Afterwards every simulation
starting from that destination reports `coverage:_monthly ... is the base of 2
backups`. The runbook has no "restart the chain by hand" recipe, so a user
doing this sees plan fail and send succeed with no explanation.

Fix: exempt `explicit-full` plans from `no-duplicate-send` (the user asked),
keep the coverage report, and add a runbook recipe: when to force a full, what
`plan` prints, and that the next monthly chains from it (experiment I2).

### A4. A full that outlives the cron interval makes the next run "fail"

`Backup` takes `<workingDirectory>/locks/<md5(volume)>.lck` with `TryLock`; a
second run while a multi-day full is still uploading logs `Cannot lock ...
you may forcefully remove the lock file` and exits non-zero. The runbook says
"any other status is a real failure" and the message invites deleting the lock
of a live send. No e2e test exercises a send against a held lock.

Fix: e2e test (take the lock, run `send`, expect the error and no upload); a
message that says "another send of X is running" without the removal hint
when the holder's pid is alive; runbook paragraph on overlap.

## B. Situations without a golden (cheap: fixtures only)

Each of these ran cleanly as an experiment and should be pinned so a planner
change cannot move them silently.

| Scenario to add | Shows | Result today |
|---|---|---|
| `monthly-only-3-years` (daily, 2026-09-24..2029-10-02, policy with `yearly=2,weekly=4` too) | cadence across the 2028 leap year and extra sanoid periods | fulls on Mar 1 / Sep 1 every year; adding yearly+weekly leaves the year golden byte-identical (experiments C, H) |
| `monthly-only-no-hourlies` (`policy=monthly=6`) | a pool with only monthlies (syncoid target with `--no-sync-snap`, or a monthly-only template): the window age is then measured monthly-to-monthly, not against an hourly | same fulls (experiment B); note 4344h would already slip a month here |
| `monthly-only-weekly-2-monthlies` | the retention floor that still works | one upload per monthly, fulls Mar 7 / Sep 5, `checks: OK` (R2) |
| `monthly-only-monthly-1-unusable` (`expect-violations`) | `monthly=1` | weekly: a `source-pruned` FULL every month; daily: FULL/INCR alternating (R1, R1d) |
| `switch-daily-to-monthly-only` (manifests: Sep full + 22 daily incrementals; `expect-violations`) | changing flags on a live chain | `INCR Oct from the Sep-23 daily`, then monthly chain, full on Mar 1; `restore-depth` flags the inherited 23-deep chain once (F). After a 40-day outage: `source-pruned FULL Nov` (F2) |
| `monthly-only-gap-3-months-then-year` (gap-3 manifests + weekly schedule from 2027-01-10) | recovery and re-synchronisation | FULL Jan (source-pruned), incrementals, FULL Jul 4 and Jan 2: the cadence restarts from the recovery full (G) |
| `monthly-only-missing-incr-suffix` (`expect-violations`) | `--incrementalSnapshotSuffix` forgotten | 370 hourly incrementals a year; `coverage:_monthly` reports 11 monthlies never backed up (E) |

Structural gap behind two of these: **no committed scenario combines
`manifests.txt` with a `schedule`**. Every long simulation starts from an
empty destination, yet the runbook's real need after step 5 is "a year of
runs from the destination as it is now". `cmd/plan.go` supports `plan $FLAGS
--schedule ... $DS $URI` (live snapshots + real manifests + simulation) but no
test runs a destination together with `--schedule` (`TestPlanSchedule` uses
`--snapshots`). Add the cmd test and a runbook step 5b: run that command at
each six-month mark.

## C. Harness and check additions (small code)

- **`skip=<from>..<to>`** (repeatable) in the schedule spec: runs in the
  range do not happen. The gap scenarios are next-run only because an outage
  cannot be placed inside a long simulation; with `skip=` one golden can show
  a year with a two-month outage, the recovery and the checks over the whole
  timeline.
- **`only:<suffix>`** check: every backup's base matches the suffix. Today
  the forgotten-suffix case (E) is caught by `coverage:_monthly` only because
  the 01:00 run sees an hourly newer than the 00:00 monthly; with a cron at the
  boundary and sanoid's real tie order (monthly first) every monthly would be
  "covered" and 360 extra uploads a year would pass `checks: OK`.
- **`snapshot-delay=`**: see A1.
- `TestTieOrderIndependence` permutes monthly/daily/hourly only; once a
  scenario carries `yearly`, January 1st has four coincident snapshots.
  Include `yearly` in the permutation for that scenario.

## D. Runbook statements without a test, and omissions

1. **S3 lifecycle rules.** `AWSS3Backend.PreDownload` restores only storage
   class `GLACIER` (billable, "could take several hours", polled every N
   minutes); `DEEP_ARCHIVE` and `GLACIER_IR` are not recognised, so the
   download fails outright. The cache hides this on the sending host (cached
   manifests are never re-downloaded), so it surfaces on a new host, after
   `clean --cleanLocal`, or at restore time, when it matters most. The
   runbook never mentions lifecycle rules. Add: exclude `manifests|*` (or the
   whole prefix) from transitions, or expect every restore to start with a
   restore of all manifests; Deep Archive is unsupported.
2. "The planner ignores a cached manifest that is gone from the destination"
   has no planner-side test (`clean_test.go` covers clean's view). An e2e
   that deletes the manifest at a `file://` destination and runs `plan` and
   `send` would pin what happens next: with no manifest left the plan is
   `no-previous-full` and `send` rewrites the full's volumes in place, since
   `refuseExistingSet` keys on the manifest.
3. "`fulls land every 181-184 days, always on the 1st`" holds once sanoid has
   run for a month. Sanoid's first monthly is taken when it is installed
   (`autosnap_2026-06-16_00:18:47_monthly` in the capture); if that is the
   newest monthly at the first run, the first full and its window start
   mid-month. Harmless; say so.
4. The window value. `4320h` (180 d) and `4344h` (181 d) keep fulls on Mar 1 /
   Sep 1; `4392h` (183 d) drifts to Apr/Oct and then May, a 212-day gap that
   `full-cadence` still accepts (slack is 31 d + one run). Say in the runbook
   why 4320h and that anything over 181 d stretches some gaps to seven months.
5. Retention floor: `monthly>=2` works with no missed runs; `monthly=3` is
   the smallest that survives two missed months; `monthly=1` never chains.
   The runbook says only "the chain tolerates missed runs for about two
   months", which is true for `monthly=3`.
6. Step 1 captures only the retention counts. The harness assumes sanoid's
   default times (`*_hour`, `*_min`, `monthly_mday`, `weekly_wday`) and
   `autoprune=yes`; a template that changes any of these makes the simulation
   wrong. Grep for those keys too and say the simulation assumes the
   defaults.
7. `plan` during a running `send` on the same host (both touch the cache
   directory) is neither tested nor mentioned. Cache writes are atomic since
   ed46293, so it is probably safe; test it before saying so.
8. Multiple destinations: the runbook uses one. A2 above is the one way two
   destinations make `plan` and `send` disagree.
9. `date -d '+400 days'` is GNU `date` (fine on the Linux pool host; macOS
   needs `-v+400d`).

## E. What is covered well (no action)

Monthly-only daily and weekly cadence for a year with real names and both DST
transitions; tie-order independence of coincident snapshots; two-month and
three-month outages (next-run); `source-pruned` no-op; due full with no
candidate (error) and with a candidate a day later; out-of-sync destinations;
explicit full already backed up; the year replay through the real `send`
(`TestE2ESequenceMatchesPlanner`); every next-run golden replayed through
`send` with its manifests recreated by real sends; exit codes 0/0/2;
resume, refuse-overwrite, partial-set completion, failed-stream and ENOSPC
paths in the e2e suite.

## Experiments (reproduce with `make plan ARGS=...`)

```
F="--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly"
S=backup/testdata/scenarios/monthly-only-year/snapshots.txt
G=backup/testdata/scenarios/monthly-only-gap-3-months
N=backup/testdata/scenarios/monthly-only-noop
A   $F --snapshots $S --schedule 'policy=hourly=36,daily=30,monthly=3,from=2026-09-24T00:00:00Z,until=2026-11-02T00:00:00Z,every=24h,checks=coverage:_monthly' tank/data
B   $F --snapshots $S --schedule 'policy=monthly=6,from=2026-09-24T01:00:00Z,until=2028-10-02T01:00:00Z,every=24h,checks=coverage:_monthly' tank/data
C   $F --snapshots $S --schedule 'policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2029-10-02T01:00:00Z,every=24h,checks=coverage:_monthly' tank/data
D   as C with --fullIfOlderThan 4344h | 4392h | 4000h
E   --fullIfOlderThan 4320h --fullSnapshotSuffix _monthly --snapshots $S --schedule 'policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2027-10-01T01:00:00Z,every=24h,checks=coverage:_monthly' tank/data
F   $F --snapshots $S --manifests <Sep full + daily chain to Sep 23> --schedule 'policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2027-04-02T01:00:00Z,every=24h,checks=coverage:_monthly' tank/data
F2  as F with from=2026-11-03T01:00:00Z
G   $F --snapshots $G/snapshots.txt --manifests $G/manifests.txt --schedule 'policy=hourly=48,daily=30,monthly=3,from=2027-01-10T03:00:00Z,until=2028-01-16T03:00:00Z,every=168h,checks=coverage:_monthly' tank/data
H   as monthly-only-year with policy=yearly=2,monthly=3,weekly=4,daily=30,hourly=36 (diff against expected.txt: identical)
I   --full --fullSnapshotSuffix _monthly --snapshots $N/snapshots.txt --manifests $N/manifests.txt tank/data
I2  $F --snapshots $N/snapshots.txt --manifests <noop manifests + FULL Oct> --schedule 'policy=hourly=36,daily=30,monthly=3,until=2026-12-02T01:00:00Z,every=24h,checks=coverage:_monthly' tank/data
R   $F --snapshots $S --schedule 'policy=hourly=48,daily=30,monthly=2|1,from=2026-09-27T03:00:00Z,until=2027-10-10T03:00:00Z,every=168h,checks=coverage:_monthly' tank/data
```

## Status (2026-10-08, same day)

Every item in sections A-D was implemented on `clean-dry-run` per `plan.md`
(commits `de29089..6bbcdb7`), except the three noted as still open in
`use-cases-report.md` section 7. Two defects surfaced while replaying the new
goldens through `send`: the "full backup is due" error printed a
zone-dependent time, and a full and an incremental of the same snapshot had no
defined order at a destination. Both are fixed in that range.
