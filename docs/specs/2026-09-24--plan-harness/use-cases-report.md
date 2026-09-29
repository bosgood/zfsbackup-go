# Plan harness: use cases tested

Report written 2026-09-24 on branch `clean-dry-run`. It covers every golden
scenario under `backup/testdata/scenarios/` and the Go tests around them.

## How the harness tests a use case

A scenario is a directory holding:

- `flags`: the smart `send` flags, exactly as passed to `zfsbackup send`.
- `snapshots.txt` or `snapshots.json`: the pool's snapshots and bookmarks.
- `manifests.txt` (optional): the backups already at each destination.
- `schedule` (optional): a sanoid policy, a cron cadence and a date range.
- `expected.txt`: the golden output.
- `expect-violations` (optional): marks a scenario that is meant to fail a check.

Scenarios come in two kinds:

- **Next-run** scenarios have no `from`/`until` range. They ask what the next
  `send` would do. `TestE2ENextRunScenarios` also replays each one through the
  real `send` against the fake `zfs` and `file://` destinations. The real
  send must do what the planner planned.
- **Schedule** scenarios simulate a cron job over months. Sanoid takes and
  prunes snapshots between runs, following its real rule: destroy a snapshot
  once it is older than count×period AND more than count remain.

Every simulation runs seven default checks: `no-errors`, `chain-links`,
`source-present`, `no-duplicate-send`, `no-orphan-full`, `full-cadence` and
`restore-depth`. A scenario can also opt into `coverage:<suffix>`, which
requires every matching snapshot to be backed up exactly once.

Three production flag sets appear below:

| Label | Flags |
|---|---|
| **monthly-only** (production) | `--fullIfOlderThan=4320h --fullSnapshotSuffix=_monthly --incrementalSnapshotSuffix=_monthly` |
| **monthly/daily** | `--fullIfOlderThan=4320h --fullSnapshotSuffix=_monthly --incrementalSnapshotSuffix=_daily` |
| **legacy** | `--snapshotPrefix autosnap_ --fullIfOlderThan 720h` |

## 1. Production: monthly-only offsite backups

This is the configuration the user plans to deploy.

| Scenario | Situation | Expected decision |
|---|---|---|
| `first-run-empty-dest` | Nothing backed up yet. The pool has hourlies, dailies and 3 monthlies. | `FULL` of the newest monthly (Sep 1), `no-previous-full` |
| `monthly-only-next-incr` | A full of September exists and October's monthly was just taken. | `INCR` Sep→Oct, `newer-candidate` |
| `monthly-only-noop` | Mid-month, and Oct is already backed up. Only newer hourlies and dailies exist. | `NOOP`, `nothing-newer`. Hourlies and dailies are ignored. |
| `monthly-only-5-months` | Daily cron from Sep 24 to Feb 20, before the 180-day window elapses. `coverage:_monthly`. | 1 FULL, then one INCR on each 1st. The ~150 runs in between are NOOPs. No full roll-over. |
| `monthly-only-year` | Daily cron for a year. `coverage:_monthly`. | Fulls roll on 2027-03-01 and 2027-09-01 (`window-elapsed`). Each full is the first monthly after the window elapses. Every other monthly is sent as an INCR exactly once. |

**What this shows:** under production flags, the tool sends exactly one backup
per monthly and nothing else. A new full chain starts about every 6 months.

## 2. Real production data (navidrome capture)

These scenarios use `snapshots.json`, a real `zfs list` capture of
`backup3/enc/user/app/navidrome` taken 2026-09-24. They set
`location=America/New_York` because sanoid names are in local time and the
capture has no creation times.

| Scenario | Situation | Expected decision |
|---|---|---|
| `prod-navidrome-first-run` | The first backup against the real pool (next-run). | `FULL autosnap_2026-09-01_00:03:36_monthly`. The real monthly carries a real timestamp, not midnight. |
| `prod-navidrome-year` | Daily cron for a year. The real capture is pruned like `ypool` (`hourly=48,daily=30,monthly=6`), because `backup3` is an unpruned syncoid target. | Same shape as `monthly-only-year`: fulls on Mar 1 and Sep 1, one INCR per monthly. It crosses both DST transitions (offsets `-04:00`/`-05:00`) without drift. |
| `prod-navidrome-weekly-3-monthlies` | A weekly cron (Sunday 03:00) on a pool that keeps only 3 monthlies, for about 1 year. | Each monthly is sent on the first Sunday after the 1st as an INCR from the previous monthly. The runs in between are NOOPs. Fulls roll on 2027-03-07 and 2027-09-05. The chain never breaks. |

**What this shows:** production flags work on real snapshot names, across real
DST changes, and with a weekly cadence on the tightest retention considered
(`monthly=3`).

## 3. Missed runs and pruned sources (resilience)

| Scenario | Situation | Expected decision |
|---|---|---|
| `monthly-only-gap-2-months` | A weekly cron missed every run from Oct 4 to Dec 28 (`monthly=3`). September was pruned, but Oct, the last backup, is still on the pool. | `INCR` Oct→Dec. The chain continues and November is skipped. |
| `monthly-only-gap-3-months` | The same cron, missed until Jan 10. October has since been pruned, so no incremental is possible. | `FULL` of Jan 2027, `source-pruned`. The chain restarts instead of stalling. `expect-violations`: `full-cadence` flags a full 122 days after the last one (expected 180±31). That is the cost of the outage, not a planner bug. |
| `pruned-source-no-newer-monthly` | monthly/daily flags. The host was down after the 2027-03-02 run and sanoid pruned that daily. The newest monthly is the base of the current full. | `NOOP`, `source-pruned`. It does not re-send the old full (bug 044116b). |
| `out-of-sync-destinations` | Two destinations: one has the Sep full, the other only an Aug full. | `ERROR`: destinations are out of sync. `plan` reports a `no-errors` violation and exits 2, the same way `send` would fail. `expect-violations`. |

**What this shows:** the monthly-only chain survives about 2 months of missed
runs with `monthly=3`. After that, it falls back to a new full on its own.
Unsafe states produce a hard error or a no-op, never a duplicate or broken send.

## 4. Other flag configurations

| Scenario | Situation | Expected decision |
|---|---|---|
| `monthly-daily-5-months` | monthly/daily flags, daily cron for 5 months. | 1 FULL, then a daily INCR chain every run with no roll-over. On the 1st, the INCR still targets the daily, not the monthly. |
| `monthly-daily-year` | monthly/daily flags, daily cron for 1 year. | Fulls roll on 2027-03-01 and 2027-09-01. The next day's daily chains from the new full. |
| `legacy-prefix-incr` | Legacy `--snapshotPrefix`. The pool includes a non-`autosnap_` manual snapshot. | `INCR` to the newest `autosnap_` snapshot (an hourly), from the last daily backup. The manual snapshot is skipped for selection but still counts toward the full's age. This preserves historical behaviour. |

## 5. Input formats and loader

| Scenario / test | What it covers |
|---|---|
| `loader-smoke` | Mixed rows: dataset-qualified, bare sanoid names, with or without epoch or type. Flags accept both `=` and a space. The expected output is an INCR whose source is a **bookmark** (`#autosnap_2026-08-01…`). |
| `raw-listing-with-bookmarks` | Raw `zfs list -t snapshot,bookmark` output. October's monthly exists only as a bookmark, which cannot be sent as a full, so the full falls back to the September snapshot. |
| `TestParseSnapshotList` (zfs) | 20 parser cases: raw rows, bare names, time zones, JSON arrays, and rejected inputs (bad epoch, unknown type, two datasets, the native `zfs list -j` object). |
| `TestLoadScenario*`, `TestReadManifests`, `TestParseSmartFlags`, `TestParseScheduleSpec` | The scenario file formats. |

## 6. Cross-cutting tests

- **`TestPlanSmartSnapshotsReasons`**: 14 table cases, one for each planner
  reason code. Cases include: window elapsed but the newest candidate is the
  last backup; the newest candidate predates the last backup; diverged
  destinations in both orders (the destination furthest behind decides); an
  incremental source that exists only as a bookmark; explicit `--full` and
  `--increment`.
- **`TestTieOrderIndependence`**: reruns the four long schedule scenarios with
  every order of the monthly/daily/hourly snapshots sanoid takes together on
  the 1st, both in the same second and 1s apart. Each run must plan the same
  kind of backup either way, and the checks must agree.
- **`TestRunSeesOnlyPastSnapshots`**: a simulated run never sees snapshots
  created after its run time.
- **`TestChecks`**: 22 cases, with at least one passing and one failing case
  for each invariant.
- **End-to-end tests** (root package, fake `zfs` and real `send`):
  - `TestE2EDryRunSendsNothing`: `--dry-run` reports the send and uploads nothing.
  - `TestE2ESequenceMatchesPlanner`: replays `monthly-only-year` through the
    real `send`, run by run. The manifests written at the end must match the
    simulator's destination exactly.
  - `TestE2ENextRunScenarios`: all 11 next-run scenarios.
  - `TestE2EExitCodes`: a no-op exits 0 (bug 45e60b7), the first full exits 0,
    and a plan with a failing check exits 2.
- **`cmd/plan_test.go`**: tests the CLI (next-run, capture, schedule, `--json`,
  exit 2 on a failed check) and 8 flag-error cases.

## Result

All scenario, planner, check, tie-order and e2e tests passed on 2026-09-24:

- `make test-run PKG=./backup/ RUN='TestScenarios|TestTieOrder|TestPlanSmart|TestChecks'`
- `make test-run PKG=. RUN=TestE2E`

Only `out-of-sync-destinations` and `monthly-only-gap-3-months` report
violations, and both are expected.

## Gaps and possible next scenarios

- Retention of `monthly=2` or lower, where one missed month may already break
  the chain.
- A full roll-over while a destination is behind, combining the roll with the
  diverged-destination rule over a long schedule.
- Monthly-only runs with bookmarks produced by the simulation (today bookmarks
  appear only in next-run fixtures).
- Interaction with `clean` or prune: no scenario deletes manifests or volumes
  at the destination.
