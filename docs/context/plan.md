# plan: simulator and checks

Read this for any change to `cmd/plan.go`, `backup/plan*.go`, or the golden scenarios.

Diagram 11 in [../architecture.md](../architecture.md). Design: [../specs/2026-09-24--plan-harness/plan.md](../specs/2026-09-24--plan-harness/plan.md).

## Parts

| File | Holds |
|---|---|
| `cmd/plan.go` (`runPlan`) | flags, input loading, text or JSON output, exit code |
| `backup/plan.go` | `planSmartSnapshots` (shared with `send`), `Scenario`, `Scenario.Run` |
| `backup/plan_schedule.go` | `Schedule`: sanoid policy, `until=`, `skip=`, `location=`, `snapshot-delay=`, `Advance` takes and prunes snapshots |
| `backup/plan_checks.go` | `Simulation.Check`: no-errors, chain-links, source-present, no-duplicate-send, no-orphan-full, full-cadence, restore-depth, coverage, only |
| `backup/plan_fixtures.go` | parsers for `--snapshots` and `--manifests` files |

## Planner rules that are easy to miss

`planSmartAt` (`backup/plan.go`) is pure; `planSmartSnapshots` calls it with `Now()` and logs `Plan.Warnings` at Warning. `plan` prints each warning under its run (`    warning: ...`) and in `--json` (`warnings`).

- **Future dates.** Snapshots and backups dated more than a day (`futureSlack`) after now are ignored, with a warning. A backup is dated by its base snapshot. `ErrLastBackupInFuture` is left for a backup after every snapshot but not in the future (a rollback, another host).
- **Clock.** `backup.Now` (tests set it, like `HostZone`). A simulated run uses `Scenario.Clock(at)`: the later of `at` and `Scenario.Now` (zero: `backup.Now()`). `LoadScenario` sets `Now` to the newest time in its files, so goldens do not depend on the day. Root e2e tests that send at simulated times call `pinPlanClock(t, sc.Clock(at))`.
- **Broken chain** (reason `broken-chain`). When the newest backup at a destination does not reach a full there (`brokenLink`, links as `parentOf` finds them), smart mode does what it does for `source-pruned`: a full of a full candidate newer than the last backup, else a no-op until one exists. The warning names the missing link. A window-elapsed full still wins, with the warning. `--increment` refuses with an error; when there is nothing newer to send, it is a no-op `nothing-newer` with the same Warning, so the next run's refusal is no surprise.
- **Bookmark sources.** A snapshot wins over a bookmark of the same name and time (`validateSnapShotExistsFromSnaps`), whatever the listing order. With `-R` or `-I` (`needsSnapshotSource`), a last backup left only as a bookmark is a pruned source (`source-pruned`, with a warning): zfs send refuses a bookmark source with those flags. It accepts one with `-p` and `-D` (zfs 2.4.4, `TestIntegrationS3ReviewBookmarkReplication`). `source-present` checks the same.
- **Window slack.** A full is due when the last full is older than `--fullIfOlderThan` minus 10 minutes (`fullWindowSlack`); a window of 1h or less has no slack. sanoid's seconds then cannot move a full by a month.

## Inputs

- Snapshots from `zfs list`, or a `--snapshots` text file.
- Manifests from the destination, a `--manifests` file, or none. In a manifests file a trailing word `raw` marks a backup sent with `--raw`.
- `--raw` on `plan` (and in a scenario's `flags`): plan as `send --raw` would. An incremental onto a chain of the other kind is an error.
- `--schedule` spec. Without `until=` the plan is one step. With it, each cron run advances the schedule, plans, and adds a fake manifest.

`plan` never writes to a destination. Violations exit 2.

## Golden scenarios

`backup/testdata/scenarios/<name>/` has `flags`, `snapshots.txt`, `manifests.txt`, `schedule`, `expected.txt`.

```
make scenarios          # run
make scenarios-update   # rewrite every expected.txt; review the diff
make plan ARGS="..."    # run plan with the working tree
```

Root `e2e_plan_test.go` checks that `send` and `plan` agree (`TestE2ESequenceMatchesPlanner`).
