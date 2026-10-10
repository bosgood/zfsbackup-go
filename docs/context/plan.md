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

## Inputs

- Snapshots from `zfs list`, or a `--snapshots` text file.
- Manifests from the destination, a `--manifests` file, or none.
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
