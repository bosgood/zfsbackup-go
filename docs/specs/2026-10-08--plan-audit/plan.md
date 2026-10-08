# Plan audit follow-ups (2026-10-08) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the gaps found by `audit.md` (same directory): pin the untested planner
situations as goldens, give the schedule simulator the three knobs it lacks
(`snapshot-delay=`, `skip=`, `only:<suffix>`), make `plan` agree with `send` on partial
sets, explicit fulls and a held lock, and say in the runbook what the tool will do.

**Architecture:** The planner is pure (`backup/plan.go`), scenarios are directories under
`backup/testdata/scenarios/` with goldens written by `make scenarios-update`, checks live in
`backup/plan_checks.go`, the schedule spec parser in `backup/plan_fixtures.go`, the sanoid
simulator in `backup/plan_schedule.go`. The e2e tests in the root package run the real
`send` (and, after Task 7, `plan`) in-process against the fake zfs and `file://`
destinations. Every existing golden must stay byte-identical through this plan.

**Tech Stack:** Go 1.25 (vendored deps), `make test-run PKG=<pkg> RUN=<regexp>` for the
fast loop, `make scenarios` / `make scenarios-update` for goldens, `make plan ARGS=...`
to try a plan by hand, `make fmt-check`, `make test-docker` at the end.

Line numbers are as of `c60498d` on `clean-dry-run`; re-check before editing.

---

## Ground rules

- Branch `clean-dry-run`. One commit per task; messages in the existing style
  (`plan: ...`, `scenarios: ...`, `send: ...`, `docs: ...`). Stage files by name.
- A new golden is reviewed, not just generated: after `make scenarios-update`, read the
  `expected.txt` and compare it with the "Expected shape" in the task. If it differs, the
  scenario or the planner is wrong; do not commit until you know which.
- `TestScenarios` fails any scenario whose output contains "violation" unless the
  directory has an `expect-violations` file. Tasks say when to add one and why.
- Code tasks are TDD: run the new test, watch it fail, then implement.
- After any in-process cobra test, run the whole package (`make test-run PKG=. RUN=.`)
  before trusting green (napkin: leaked `SetArgs`).
- Next-run scenarios (no `until=`) are replayed by `TestE2ENextRunScenarios`
  (`make test-run PKG=. RUN=TestE2ENextRunScenarios`), so run that after adding one.

---

## Part 1: goldens (fixtures only)

### Task 1: `monthly-only-3-years`

Three years of daily runs across the 2028 leap year, with sanoid's full period set
(yearly, weekly too). Experiments C and H in `audit.md`.

**Files:**
- Create: `backup/testdata/scenarios/monthly-only-3-years/{flags,snapshots.txt,schedule}`
- Modify: `backup/plan_test.go:451-452` (`TestTieOrderIndependence`)

**Step 1: Create the fixture**

```bash
D=backup/testdata/scenarios/monthly-only-3-years
mkdir $D
cp backup/testdata/scenarios/monthly-only-year/flags $D/flags
cp backup/testdata/scenarios/monthly-only-year/snapshots.txt $D/snapshots.txt
cat > $D/schedule <<'S'
# Three years of daily runs at 01:00, across the 2028 leap year, on a pool that
# keeps every sanoid period (a yearly and a weekly join the monthly, daily and
# hourly on some 1sts). Fulls must stay on March 1st and September 1st.
policy=yearly=2,monthly=3,weekly=4,daily=30,hourly=36
from=2026-09-24T01:00:00Z
until=2029-10-02T01:00:00Z
every=24h
checks=coverage:_monthly          # every monthly backed up exactly once
S
```

**Step 2: Generate and review the golden**

Run: `make scenarios-update && cat $D/expected.txt | grep -v 'NOOP x'`
Expected shape: `FULL ... no-previous-full` on 2026-09-24, then one `INCR` per 1st,
`FULL ... window-elapsed` on 2027-03-01, 2027-09-01, 2028-03-01, 2028-09-01,
2029-03-01, 2029-09-01, `NOOP ... full-due` lines in the last days of February and
August, and `checks: OK`. Compare the first 13 months with
`monthly-only-year/expected.txt`: identical lines through 2027-10-01.

**Step 3: Make the tie-order test permute the scenario's own periods**

In `TestTieOrderIndependence` (backup/plan_test.go:451), add the new scenario to the
list and permute the periods the scenario schedules instead of the fixed three:

```go
	for _, name := range []string{"monthly-only-5-months", "monthly-daily-5-months", "monthly-only-year", "monthly-daily-year", "monthly-only-3-years"} {
		t.Run(name, func(t *testing.T) {
			var want, wantVariant string
			pools := make(map[string]bool) // distinct pools seen, to prove the variants differ
			base, err := LoadScenario(filepath.Join("testdata/scenarios", name))
			if err != nil {
				t.Fatalf("LoadScenario: %v", err)
			}
			var periods []string
			for _, p := range base.Schedule.Periods {
				periods = append(periods, p.Name)
			}
			for _, order := range permutations(periods) {
```

(the rest of the loop body is unchanged; it reloads the scenario per variant).

**Step 4: Run**

Run: `make scenarios`
Expected: PASS, including `TestTieOrderIndependence/monthly-only-3-years` (120 orders ×
2 gaps; if it takes over a minute, restrict the permutation to the periods that share a
boundary on January 1st — all five do — and note the runtime in the commit message).

**Step 5: Commit**

```bash
git add backup/testdata/scenarios/monthly-only-3-years backup/plan_test.go
git commit -m "scenarios: three years of monthly-only runs across a leap year, every sanoid period"
```

### Task 2: `monthly-only-no-hourlies`

A pool that keeps only monthlies (a syncoid target replicated with `--no-sync-snap`, or
a sanoid template without hourly/daily). The window age is then measured
monthly-to-monthly. Experiment B.

**Files:**
- Create: `backup/testdata/scenarios/monthly-only-no-hourlies/{flags,snapshots.txt,schedule}`

**Step 1: Create the fixture**

```bash
D=backup/testdata/scenarios/monthly-only-no-hourlies
mkdir $D
cp backup/testdata/scenarios/monthly-only-year/flags $D/flags
cat > $D/snapshots.txt <<'S'
# A pool with monthlies only: the newest snapshot a run sees is a monthly, so
# the age of the last full is measured from one monthly to the next.
autosnap_2026-09-01_00:00:00_monthly
autosnap_2026-08-01_00:00:00_monthly
autosnap_2026-07-01_00:00:00_monthly
S
cat > $D/schedule <<'S'
# Two years of daily runs at 01:00 on a pool that keeps only monthlies. The
# 4320h window still rolls the full onto March 1st and September 1st: the
# shortest six-month span between 1sts (September to March) is 181 days.
policy=monthly=6
from=2026-09-24T01:00:00Z
until=2028-10-02T01:00:00Z
every=24h
checks=coverage:_monthly
S
```

**Step 2: Generate and review**

Run: `make scenarios-update && grep -v 'NOOP x' $D/expected.txt`
Expected shape: FULL Sep 2026, INCR each 1st, FULL on 2027-03-01, 2027-09-01,
2028-03-01, 2028-09-01, `checks: OK`. No `full-due` lines (there is no hourly to age
the last full before the 1st).

**Step 3: Commit**

```bash
git add backup/testdata/scenarios/monthly-only-no-hourlies
git commit -m "scenarios: a pool that keeps only monthlies"
```

### Task 3: the retention floor: `monthly=2` works, `monthly=1` never chains

Experiments R2, R1, R1d.

**Files:**
- Create: `backup/testdata/scenarios/monthly-only-weekly-2-monthlies/{flags,snapshots.txt,schedule}`
- Create: `backup/testdata/scenarios/monthly-only-monthly-1-unusable/{flags,snapshots.txt,schedule,expect-violations}`

**Step 1: Create both fixtures**

```bash
S=backup/testdata/scenarios
D=$S/monthly-only-weekly-2-monthlies
mkdir $D
cp $S/monthly-only-year/flags $S/monthly-only-year/snapshots.txt $D/
cat > $D/schedule <<'X'
# A weekly cron (Sunday 03:00) on a pool that keeps only two monthlies, the
# smallest retention on which the chain holds with no missed runs: on the first
# Sunday after each 1st the previous monthly is still there (sanoid destroys a
# monthly once it is older than 2 x 31 days AND more than two remain).
policy=hourly=48,daily=30,monthly=2
from=2026-09-27T03:00:00Z
until=2027-10-10T03:00:00Z
every=168h
checks=coverage:_monthly
X
D=$S/monthly-only-monthly-1-unusable
mkdir $D
cp $S/monthly-only-year/flags $S/monthly-only-year/snapshots.txt $D/
cat > $D/schedule <<'X'
# monthly=1 cannot chain: by the first Sunday after each 1st the previous
# monthly is older than 31 days with two present, so sanoid has destroyed it,
# and every run that finds a new monthly sends a source-pruned full of it.
policy=hourly=48,daily=30,monthly=1
from=2026-09-27T03:00:00Z
until=2027-10-10T03:00:00Z
every=168h
checks=coverage:_monthly
X
cat > $D/expect-violations <<'X'
# This scenario shows a retention that is too short on purpose: every monthly
# goes out as a full, which full-cadence flags on every run that sends one.
X
```

**Step 2: Generate and review**

Run: `make scenarios-update && for d in monthly-only-weekly-2-monthlies monthly-only-monthly-1-unusable; do echo == $d; grep -v 'NOOP' $S/$d/expected.txt; done`
Expected shape:
- `weekly-2-monthlies`: one INCR on the first Sunday after each 1st, FULL on
  2027-03-07 and 2027-09-05, `checks: OK` (same lines as
  `prod-navidrome-weekly-3-monthlies` but with UTC names).
- `monthly-1-unusable`: `FULL ... no-previous-full` then `FULL ... source-pruned` on
  every first Sunday after a 1st, `checks: 13 violation(s)`, all `full-cadence ... is
  30d after FULL ...` (or 28d/31d).

**Step 3: Commit**

```bash
git add $S/monthly-only-weekly-2-monthlies $S/monthly-only-monthly-1-unusable
git commit -m "scenarios: the retention floor: monthly=2 chains, monthly=1 never does"
```

### Task 4: `switch-daily-to-monthly-only` (first scenario with manifests + schedule)

Changing the flags of a live chain. Experiment F.

**Files:**
- Create: `backup/testdata/scenarios/switch-daily-to-monthly-only/{flags,snapshots.txt,manifests.txt,schedule,expect-violations}`

**Step 1: Create the fixture**

```bash
S=backup/testdata/scenarios
D=$S/switch-daily-to-monthly-only
mkdir $D
cp $S/monthly-only-year/flags $S/monthly-only-year/snapshots.txt $D/
{ cat <<'X'
# Backups taken with --incrementalSnapshotSuffix _daily until September 23rd:
# the September full and a daily incremental every day after it. On the 24th
# the job switches to the monthly-only flags.
autosnap_2026-09-01_00:00:00_monthly
X
prev=autosnap_2026-09-01_00:00:00_monthly
for d in $(seq -w 2 23); do cur=autosnap_2026-09-${d}_00:00:00_daily; echo "$prev to $cur"; prev=$cur; done
} > $D/manifests.txt
cat > $D/schedule <<'X'
# Daily runs at 01:00 under the new flags. The first monthly after the switch
# (October) is sent as an incremental from the last daily backup, and the chain
# is monthly-only from then on; the full rolls on March 1st as usual.
policy=hourly=36,daily=30,monthly=3
from=2026-09-24T01:00:00Z
until=2027-04-02T01:00:00Z
every=24h
checks=coverage:_monthly
X
cat > $D/expect-violations <<'X'
# restore-depth flags the October incremental once: restoring it takes the 22
# daily incrementals inherited from the old flags plus one. That is the cost of
# switching mid-chain, cleared by the next full; not a planner bug.
X
```

**Step 2: Generate and review**

Run: `make scenarios-update && grep -v 'NOOP x' $D/expected.txt`
Expected shape: `2026-10-01 INCR autosnap_2026-10-01_00:00:00_monthly from
autosnap_2026-09-23_00:00:00_daily newer-candidate`, monthly INCRs through February,
`NOOP ... full-due` on 2027-02-28, `FULL ... window-elapsed` on 2027-03-01, INCR April,
then `checks: 1 violation(s)` with `restore-depth 2026-10-01T01:00:00Z restoring
autosnap_2026-10-01_00:00:00_monthly takes 23 incrementals, want at most 9`.

**Step 3: Commit**

```bash
git add $D
git commit -m "scenarios: switching a daily chain to monthly-only flags"
```

### Task 5: `monthly-only-gap-3-months-then-year`

The year after the gap-3 recovery; the cadence restarts from the recovery full.
Experiment G.

**Files:**
- Create: `backup/testdata/scenarios/monthly-only-gap-3-months-then-year/{flags,snapshots.txt,manifests.txt,schedule,expect-violations}`

**Step 1: Create the fixture**

```bash
S=backup/testdata/scenarios
D=$S/monthly-only-gap-3-months-then-year
mkdir $D
cp $S/monthly-only-gap-3-months/{flags,snapshots.txt,manifests.txt} $D/
cat > $D/schedule <<'X'
# The weekly cron resumes on 2027-01-10 after missing three months (see
# monthly-only-gap-3-months) and runs for a year. The first run sends a
# source-pruned full of January; the chain continues from it and the next
# fulls fall one window after that recovery full, not on the old March/September
# rhythm.
policy=hourly=48,daily=30,monthly=3
from=2027-01-10T03:00:00Z
until=2028-01-16T03:00:00Z
every=168h
checks=coverage:_monthly
X
cat > $D/expect-violations <<'X'
# full-cadence flags the recovery full (122 days after the September full),
# as in monthly-only-gap-3-months. Everything after it must be clean.
X
```

**Step 2: Generate and review**

Run: `make scenarios-update && grep -v 'NOOP x' $D/expected.txt`
Expected shape: `2027-01-10 FULL autosnap_2027-01-01_00:00:00_monthly source-pruned`,
INCRs Feb..Jun, `FULL ... window-elapsed` on 2027-07-04, INCRs Aug..Dec, FULL on
2028-01-02, exactly one violation: `full-cadence 2027-01-10T03:00:00Z FULL
autosnap_2027-01-01_00:00:00_monthly is 122d after FULL autosnap_2026-09-01_00:00:00_monthly,
want 180d ± 38d`.

**Step 3: Commit**

```bash
git add $D
git commit -m "scenarios: the year after a three-month outage"
```

---

## Part 2: harness knobs

### Task 6: `snapshot-delay=` (sanoid is late by a few minutes)

**Files:**
- Modify: `backup/plan_schedule.go:67-77` (Schedule struct), `:153-162` (Advance)
- Modify: `backup/plan_fixtures.go:316-380` (ParseScheduleSpec)
- Test: `backup/plan_schedule_test.go` (TestScheduleAdvance), `backup/plan_fixtures_test.go` (TestParseScheduleSpec)
- Create: `backup/testdata/scenarios/monthly-only-midnight-cron/{flags,snapshots.txt,schedule}`

**Step 1: Write the failing tests**

In `TestScheduleAdvance` (plan_schedule_test.go, after the "tie order and gap are
adjustable" case), add:

```go
	t.Run("a delay moves every snapshot and its name past the boundary", func(t *testing.T) {
		s := &Schedule{Periods: []Period{{Name: "monthly", Keep: 3}, {Name: "hourly", Keep: 36}}, Delay: 3 * time.Minute}
		at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got := s.Advance(nil, at.Add(-time.Hour), at)
		if len(got) != 2 {
			t.Fatalf("got %d snapshots, want the monthly and the hourly: %+v", len(got), got)
		}
		for _, snap := range got {
			if !snap.CreationTime.Equal(at.Add(3*time.Minute)) || !strings.HasPrefix(snap.Name, "autosnap_2026-10-01_00:03:00_") {
				t.Errorf("got %s at %v, want a 00:03:00 creation time and name", snap.Name, snap.CreationTime)
			}
		}
	})
```

In `TestParseScheduleSpec`, extend the spec with a line `snapshot-delay=3m` and assert
`s.Schedule.Delay != 3*time.Minute` is an error; add `"until=2027-01-01,snapshot-delay=-1m"`
to the bad list.

**Step 2: Run them**

Run: `make test-run PKG=./backup/ RUN='TestScheduleAdvance|TestParseScheduleSpec'`
Expected: compile error (`s.Delay` undefined).

**Step 3: Implement**

`Schedule` gets a field, documented with the real numbers:

```go
	// Delay is how long after a boundary sanoid takes the snapshot (a cron
	// at :00 plus the time zfs takes; 2-4 minutes on the captured pool). The
	// creation time and the name carry it, so a run at the boundary does not
	// see the snapshot yet.
	Delay time.Duration
```

In `Advance`, `creation := b.at.Add(s.Delay + time.Duration(len(b.periods)-1-i)*s.tieGap)`.

In `ParseScheduleSpec`, a `case "snapshot-delay":` parsing a non-negative duration into a
local `delay` and, after the schedule is built, `s.Schedule.Delay = delay` (error if set
without a policy: "snapshot-delay needs a policy"). Document the key in the
ParseScheduleSpec comment and in `cmd/plan.go`'s `--schedule` help string.

**Step 4: Run, then the whole package**

Run: `make test-run PKG=./backup/ RUN='TestScheduleAdvance|TestParseScheduleSpec'` then
`make scenarios` (every golden must still pass: the default delay is 0).

**Step 5: The scenario**

```bash
S=backup/testdata/scenarios
D=$S/monthly-only-midnight-cron
mkdir $D
cp $S/monthly-only-year/flags $S/monthly-only-year/snapshots.txt $D/
cat > $D/schedule <<'X'
# A daily cron at midnight, the same minute sanoid starts. Sanoid lands the
# monthly a few minutes later (00:02-00:03 on the captured pool), so the run on
# the 1st does not see it and the incremental goes out on the 2nd. Schedule the
# cron an hour or more after sanoid's boundary to send on the 1st.
policy=hourly=36,daily=30,monthly=3
snapshot-delay=3m
from=2026-09-24T00:00:00Z
until=2026-12-03T00:00:00Z
every=24h
checks=coverage:_monthly
X
make scenarios-update && cat $D/expected.txt
```

Expected shape: FULL `autosnap_2026-09-01_00:00:00_monthly` (from the listing) on
09-24, `NOOP x7`, `2026-10-02T00:00:00Z INCR autosnap_2026-10-01_00:03:00_monthly from
autosnap_2026-09-01_00:00:00_monthly`, likewise `2026-11-02` and `2026-12-02`,
`checks: OK`.

**Step 6: Commit**

```bash
git add backup/plan_schedule.go backup/plan_fixtures.go backup/plan_schedule_test.go backup/plan_fixtures_test.go cmd/plan.go $D
git commit -m "plan: snapshot-delay= models sanoid taking its snapshots after the boundary"
```

### Task 7: `skip=<from>..<to>` (an outage inside a long simulation)

**Files:**
- Modify: `backup/plan_fixtures.go:23-42` (Scenario struct), `:316-380` (ParseScheduleSpec)
- Modify: `backup/plan.go:296-322` (Run)
- Test: `backup/plan_fixtures_test.go` (TestParseScheduleSpec), `backup/plan_test.go` (new TestRunSkipsOutages)
- Create: `backup/testdata/scenarios/monthly-only-year-2-month-outage/{flags,snapshots.txt,schedule,expect-violations}`

**Step 1: Write the failing tests**

In `backup/plan_test.go`, next to `TestRunSeesOnlyPastSnapshots`:

```go
// TestRunSkipsOutages: no run happens inside a skip= range, inclusive.
func TestRunSkipsOutages(t *testing.T) {
	sc, err := LoadScenario("testdata/scenarios/monthly-only-5-months")
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	sc.Skips = []TimeRange{{From: from, Until: to}}
	var runs []time.Time
	for _, st := range sc.Run().Steps {
		if !st.At.Before(from) && !st.At.After(to) {
			t.Errorf("a run happened at %v, inside the outage", st.At)
		}
		runs = append(runs, st.At)
	}
	if len(runs) == 0 || !runs[0].Equal(sc.From) || runs[len(runs)-1].Before(to) {
		t.Errorf("runs outside the outage are missing: first %v, last %v", runs[0], runs[len(runs)-1])
	}
}
```

In `TestParseScheduleSpec`: add `skip=2026-12-24..2027-01-03` and
`skip=2027-02-01T00:00:00Z..2027-02-02` lines to the spec; assert `len(s.Skips) == 2` and
the first equals `{2026-12-24, 2027-01-03}` in UTC; add to the bad list
`"until=2027-01-01,skip=2026-12-24"` (no `..`) and `"until=2027-01-01,skip=2027-01-03..2026-12-24"`
(reversed).

**Step 2: Run them**

Run: `make test-run PKG=./backup/ RUN='TestRunSkipsOutages|TestParseScheduleSpec'`
Expected: compile error (`Skips`, `TimeRange` undefined).

**Step 3: Implement**

In plan_fixtures.go:

```go
// TimeRange is a closed interval of run times.
type TimeRange struct{ From, Until time.Time }
```

and on `Scenario`: `Skips []TimeRange // runs in these ranges do not happen (an outage)`.
In `ParseScheduleSpec`, `case "skip":` splits on `..`, parses both with `s.parseTime`,
rejects a missing separator or `Until.Before(From)`, appends. Document the key
(`skip=2026-12-24..2027-01-03   no runs in this range, inclusive; repeatable`) in the
comment and in `cmd/plan.go`'s help.

In `Run()` the loop becomes:

```go
	for at := s.firstRun(); !at.After(s.Until); at = s.nextRun(at) {
		if s.Schedule != nil {
			snapshots = s.Schedule.Advance(snapshots, taken, at)
			if at.After(taken) {
				taken = at
			}
		}
		if s.skipped(at) {
			continue // the host was down: sanoid still ran, the cron job did not
		}
		sim.Steps = append(sim.Steps, s.run(at, visibleAt(snapshots, at), dest))
	}
```

with

```go
func (s *Scenario) skipped(at time.Time) bool {
	for _, r := range s.Skips {
		if !at.Before(r.From) && !at.After(r.Until) {
			return true
		}
	}
	return false
}
```

Note the pool still advances through the outage (sanoid keeps pruning), which is the
point.

**Step 4: Run, then all goldens**

Run: `make test-run PKG=./backup/ RUN='TestRunSkipsOutages|TestParseScheduleSpec'` then `make scenarios`.

**Step 5: The scenario**

```bash
S=backup/testdata/scenarios
D=$S/monthly-only-year-2-month-outage
mkdir $D
cp $S/monthly-only-year/flags $S/monthly-only-year/snapshots.txt $D/
cat > $D/schedule <<'X'
# The daily cron job of monthly-only-year, with the host down from November 3rd
# to January 5th. The first run back still finds November, the last backup, on
# the pool (monthly=3 keeps it until February 2nd) and sends January from it;
# December is skipped, not lost: its data is inside January's incremental. The
# March full then comes on time.
policy=hourly=36,daily=30,monthly=3
from=2026-09-24T01:00:00Z
until=2027-10-01T01:00:00Z
every=24h
skip=2026-11-03..2027-01-05
checks=coverage:_monthly
X
cat > $D/expect-violations <<'X'
# coverage:_monthly reports the December monthly as never backed up: no run saw
# it before January replaced it as the newest candidate. That is what an outage
# costs; chain-links and source-present stay clean.
X
make scenarios-update && grep -v 'NOOP x' $D/expected.txt
```

Expected shape: like monthly-only-year up to `2026-11-01 INCR`, no lines dated
2026-11-03..2027-01-05, `2027-01-06T01:00:00Z INCR autosnap_2027-01-01_00:00:00_monthly
from autosnap_2026-11-01_00:00:00_monthly newer-candidate`, February INCR, March FULL,
and exactly one violation `coverage:_monthly autosnap_2026-12-01_00:00:00_monthly is
never backed up`.

**Step 6: Commit**

```bash
git add backup/plan.go backup/plan_fixtures.go backup/plan_test.go backup/plan_fixtures_test.go cmd/plan.go $D
git commit -m "plan: skip= leaves an outage inside a simulated schedule"
```

### Task 8: `only:<suffix>` check and the missing-suffix scenario

**Files:**
- Modify: `backup/plan_checks.go:55-83` (knownCheck, Check), new `checkOnly`
- Test: `backup/plan_checks_test.go` (TestChecks table)
- Create: `backup/testdata/scenarios/monthly-only-missing-incr-suffix/{flags,snapshots.txt,schedule,expect-violations}`

**Step 1: Write the failing test cases**

In the `TestChecks` table, after the coverage cases:

```go
		{
			name:  "only: every backup is of a monthly",
			check: "only:_monthly",
			extra: []string{"only:_monthly"},
			sim:   simulate(scenario(), chain(2)...),
		},
		{
			name:  "only: consecutive backups of other snapshots are one violation",
			check: "only:_monthly",
			extra: []string{"only:_monthly"},
			sim: simulate(scenario(), full(0, 0),
				Step{At: day(1), Snapshots: pool(0), Plan: Plan{Action: PlanIncremental, Base: snap("autosnap_day1_hourly", day(1)), Source: monthly(0)}},
				Step{At: day(2), Snapshots: pool(0), Plan: Plan{Action: PlanIncremental, Base: snap("autosnap_day2_hourly", day(2)), Source: snap("autosnap_day1_hourly", day(1))}},
				incr(30, 30, 0)),
			detail: "2 backups of snapshots not ending in _monthly, INCR autosnap_day1_hourly from autosnap_day0_monthly through 2026-09-03T00:00:00Z",
		},
```

(`snap` is the helper the file already uses for `monthly`; check its signature at the
top of plan_checks_test.go.)

**Step 2: Run**

Run: `make test-run PKG=./backup/ RUN=TestChecks`
Expected: both new cases FAIL (`only:_monthly` is unknown: `Check` ignores it, so the
violating case reports none).

**Step 3: Implement**

```go
// onlyCheck prefixes the opt-in check only:<suffix>.
const onlyCheck = "only:"
```

`knownCheck` accepts `only:` with a non-empty suffix the way it accepts `coverage:`.
`Check` dispatches `only:` to `sim.checkOnly(name, suffix)`:

```go
// checkOnly: every backup a run sends is of a snapshot ending in suffix; a
// job meant to send one period only (monthly-only) sends nothing else.
// Consecutive offending runs are one violation.
func (sim *Simulation) checkOnly(name, suffix string) []Violation {
	var found []Violation
	for i := 0; i < len(sim.Steps); {
		st := sim.Steps[i]
		if st.Err != nil || st.Plan.Action == PlanNoop || strings.HasSuffix(st.Plan.Base.Name, suffix) {
			i++
			continue
		}
		j := i + 1
		for j < len(sim.Steps) && sim.Steps[j].Err == nil && sim.Steps[j].Plan.Action != PlanNoop &&
			!strings.HasSuffix(sim.Steps[j].Plan.Base.Name, suffix) {
			j++
		}
		detail := describeBackup(st.Plan.Base, st.Plan.Source) + " is not of a snapshot ending in " + suffix
		if j-i > 1 {
			detail = fmt.Sprintf("%d backups of snapshots not ending in %s, %s through %s",
				j-i, suffix, describeBackup(st.Plan.Base, st.Plan.Source), sim.formatTime(sim.Steps[j-1].At))
		}
		found = append(found, Violation{Check: name, At: st.At, Detail: detail})
		i = j
	}
	return found
}
```

Adjust the test's `detail` substring to the exact wording you end up with.

**Step 4: Run, then the scenario**

Run: `make test-run PKG=./backup/ RUN=TestChecks` → PASS.

```bash
S=backup/testdata/scenarios
D=$S/monthly-only-missing-incr-suffix
mkdir $D
cp $S/monthly-only-year/snapshots.txt $D/
cat > $D/flags <<'X'
# The production flags with --incrementalSnapshotSuffix forgotten: fulls still
# anchor on the monthly, but every run sends an incremental to the newest
# snapshot of any period, i.e. an hourly a day.
--fullIfOlderThan=4320h --fullSnapshotSuffix=_monthly
X
cat > $D/schedule <<'X'
# The daily schedule of monthly-only-year. only:_monthly reports the 360-odd
# hourly incrementals directly; coverage:_monthly reports the monthlies that
# never became a base, which it would miss if the cron ran at the boundary and
# saw the monthly before the hourly.
policy=hourly=36,daily=30,monthly=3
from=2026-09-24T01:00:00Z
until=2027-10-01T01:00:00Z
every=24h
checks=coverage:_monthly,only:_monthly
X
cat > $D/expect-violations <<'X'
# A misconfiguration shown on purpose: both opt-in checks must report it.
X
make scenarios-update && grep -c INCR $D/expected.txt && grep -A20 '^checks' $D/expected.txt
```

Expected shape: ~370 INCR lines; violations: 11 `coverage:_monthly ... is never backed
up` plus `only:_monthly` entries, one per stretch of hourly incrementals between fulls
(three stretches: after the first full, after March, after September). If the `only:`
detail lines are longer than the rest of the golden, that is acceptable.

**Step 5: Commit**

```bash
git add backup/plan_checks.go backup/plan_checks_test.go $D
git commit -m "plan: only:<suffix> check; scenario for a forgotten incremental suffix"
```

---

## Part 3: plan/send parity

### Task 9: `plan` plans `complete-partial` the way `send` does

**Files:**
- Modify: `backup/backup.go:70-94` (ProcessSmartOptions), export a helper
- Modify: `backup/plan_fixtures.go:23-42` (Scenario.Completable), `backup/plan.go:335-352` (run)
- Modify: `backup/plan_checks.go:150-182` (checkNoDuplicateSend)
- Modify: `cmd/plan.go:157-175` (runPlan)
- Test: `e2e_test.go` (new `env.plan` helper, `TestE2EPlanCompletesPartialLikeSend`)

**Step 1: Add an in-process plan runner to `e2e_test.go`**

Next to `send`:

```go
// plan runs `zfsbackup plan` in-process and returns what it printed and logged.
func (env *e2eEnv) plan(args ...string) (out, logs string, err error) {
	cmd.ResetSendJobInfo()
	var outBuf, logBuf bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logBuf, "", 0)))
	oldStdout := config.Stdout
	config.Stdout = &outBuf
	defer func() { config.Stdout = oldStdout }()

	cmd.RootCmd.SetArgs(append([]string{"plan", "--zfsPath", env.self, "--workingDirectory", env.work}, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetSendJobInfo()
	}()
	err = cmd.RootCmd.ExecuteContext(context.Background())
	return outBuf.String(), logBuf.String(), err
}
```

**Step 2: Write the failing test** (in `e2e_resume_test.go`, after `smartCompletesPartial`)

```go
// TestE2EPlanCompletesPartialLikeSend: plan shows the complete-partial run that a
// smart send would do for a set whose manifest is missing at one destination.
func TestE2EPlanCompletesPartialLikeSend(t *testing.T) {
	env := newE2EEnv(t)
	dest2 := newDest(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	blocker := filepath.Join(dest2, "manifests|tank")
	if err := ioutil.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	dests := "file://" + env.dest + ",file://" + dest2
	common := []string{"--volsize", "1", "--maxRetryTime", "2s", "--maxBackoffTime", "1s"}
	if logs, err := guarded(t, func() (string, error) { return env.send(append(common, "tank/data@a", dests)...) }); err == nil {
		t.Fatalf("send succeeded although the manifest upload to dest2 failed:\n%s", logs)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	out, logs, err := env.plan("--fullIfOlderThan", "720h", "tank/data", dests)
	want := "next  FULL  a  complete-partial\nchecks: OK\n"
	if err != nil || out != want {
		t.Errorf("plan returned %v and printed:\n%s\nwant:\n%s\nlogs:\n%s", err, out, want, logs)
	}
}
```

**Step 3: Run it**

Run: `make test-run PKG=. RUN=TestE2EPlanCompletesPartialLikeSend`
Expected: FAIL: plan returns the "out of sync" error (exit 2 path) instead of printing
the complete-partial run.

**Step 4: Implement**

In backup.go, extract the completable computation so `send` and `plan` share it:

```go
// PartialSetCompletable reports whether the smart plan may complete a backup set that is
// missing at some destinations: with --resume, or when those destinations hold objects
// named like each of its volumes (the trace of a failed manifest upload).
func PartialSetCompletable(ctx context.Context, jobInfo *files.JobInfo, destBackups [][]*files.JobInfo) (bool, error) {
	if jobInfo.Resume {
		return true, nil
	}
	partial := partialSet(destBackups)
	if partial == nil {
		return false, nil
	}
	return partialVolumesPresent(ctx, jobInfo, partial, destBackups)
}
```

and `ProcessSmartOptions` ends with:

```go
	completable, err := PartialSetCompletable(ctx, jobInfo, destBackups)
	if err != nil {
		return err
	}
	return selectSmartSnapshots(jobInfo, snapshots, destBackups, completable)
```

`Scenario` gets `Completable bool // a partial set may be completed (see PartialSetCompletable)`
and `run()` passes `s.Completable` instead of `false`.

In `cmd/plan.go` `runPlan`, the `len(args) == 2` case sets `jobInfo.Destinations =
destinations` before the loop and, after it:

```go
		if sc.Completable, err = backup.PartialSetCompletable(cmd.Context(), &jobInfo, sc.DestBackups); err != nil {
			log.AppLogger.Errorf("Could not check the destinations for a partial backup set - %v", err)
			return err
		}
```

(`err` must be declared in that scope.)

In `checkNoDuplicateSend`, a `complete-partial` plan sends nothing: skip the violation
and the recording for it:

```go
		if st.Err != nil || st.Plan.Action == PlanNoop || st.Plan.Reason == reasonCompletePartial {
			continue
		}
```

**Step 5: Run the test, then the packages**

Run: `make test-run PKG=. RUN=TestE2EPlanCompletesPartialLikeSend` → PASS.
Run: `make test-run PKG=. RUN=.` and `make test-run PKG=./backup/ RUN=.` and
`make test-run PKG=./cmd/ RUN=.` → PASS.

**Step 6: Commit**

```bash
git add backup/backup.go backup/plan.go backup/plan_fixtures.go backup/plan_checks.go cmd/plan.go e2e_test.go e2e_resume_test.go
git commit -m "plan: complete a partial set the way send does"
```

### Task 10: an explicit `--full` is not a duplicate send

**Files:**
- Modify: `backup/plan_checks.go:150-182`
- Test: `backup/plan_checks_test.go` (TestChecks)
- Create: `backup/testdata/scenarios/explicit-full-restart/{flags,snapshots.txt,manifests.txt}`

**Step 1: Failing test case**, after "no-duplicate-send: a full of a monthly already sent as an incremental":

```go
		{
			name:  "no-duplicate-send: an explicit full restarts the chain on purpose",
			check: "no-duplicate-send",
			sim: simulate(scenario(), full(1, 0), incr(31, 30, 0),
				Step{At: day(32), Snapshots: pool(30), Plan: Plan{Action: PlanFull, Base: monthly(30), Reason: reasonExplicitFull}}),
		},
```

Run: `make test-run PKG=./backup/ RUN=TestChecks` → that case FAILS (one violation).

**Step 2: Implement**: in `checkNoDuplicateSend`, record the backup but report no
violation when `st.Plan.Reason == reasonExplicitFull`:

```go
			if earlier, dup := backedUp[d][id]; dup && st.Plan.Reason != reasonExplicitFull {
```

Update the check's comment: "... never sent there again, unless the user asked for a
full of it (`--full`), which starts a new chain on purpose."

**Step 3: Scenario**

```bash
S=backup/testdata/scenarios
D=$S/explicit-full-restart
mkdir $D
cp $S/monthly-only-noop/{snapshots.txt,manifests.txt} $D/
cat > $D/flags <<'X'
# Restarting the chain by hand: an explicit --full of the newest monthly, which
# is backed up so far only as the target of an incremental. send takes it (the
# object names differ from the incremental's); the next monthly chains from it.
--full --fullSnapshotSuffix=_monthly
X
make scenarios-update && cat $D/expected.txt
```

Expected: `next  FULL  autosnap_2026-10-01_00:00:00_monthly  explicit-full` and
`checks: OK`.

**Step 4: Run the replay**

Run: `make test-run PKG=. RUN=TestE2ENextRunScenarios` → PASS including
`explicit-full-restart` (the real send must do the full; if `refuseExistingSet` refuses
it, that is a finding to report, not to paper over).

**Step 5: Commit**

```bash
git add backup/plan_checks.go backup/plan_checks_test.go $D
git commit -m "plan: an explicit --full of a backed-up snapshot is not a duplicate send"
```

### Task 11: a run that finds another send running says so

**Files:**
- Modify: `backup/backup.go:424-437`
- Test: `e2e_failure_test.go` (new `TestE2ESendRefusesWhileAnotherSendRuns`)

**Step 1: Write the failing test**

```go
// TestE2ESendRefusesWhileAnotherSendRuns: a cron run that overlaps a send still
// uploading (a full that takes longer than the cron interval) exits with an error
// that names the running send, uploads nothing, and leaves the lock alone.
func TestE2ESendRefusesWhileAnotherSendRuns(t *testing.T) {
	env := newE2EEnv(t)
	sc, err := backup.LoadScenario(monthlyOnlyScenario)
	if err != nil {
		t.Fatal(err)
	}
	env.writeSnapshots(t, "tank/data", sc.Snapshots)

	// Another live process holds the lock: the lockfile library treats a lock held by
	// a dead pid, or by this very process, as stale.
	holder := exec.Command("sleep", "60")
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() })
	lock := env.lockFile("tank/data")
	if err = os.MkdirAll(filepath.Dir(lock), 0700); err != nil {
		t.Fatal(err)
	}
	if err = ioutil.WriteFile(lock, []byte(fmt.Sprintf("%d\n", holder.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}

	logs, err := env.send(append(scenarioFlags(t, monthlyOnlyScenario), "tank/data", "file://"+env.dest)...)
	want := fmt.Sprintf("Another send of tank/data is running (pid %d holds %s)", holder.Process.Pid, lock)
	if err == nil || !strings.Contains(logs, want) || strings.Contains(logs, "forcefully remove") {
		t.Errorf("send returned %v; want an error and %q in the logs, without the removal hint:\n%s", err, want, logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("the overlapping run uploaded %q", names)
	}
	if content, rerr := ioutil.ReadFile(lock); rerr != nil || strings.TrimSpace(string(content)) != fmt.Sprint(holder.Process.Pid) {
		t.Errorf("lock file changed: %q, %v", content, rerr)
	}
}
```

**Step 2: Run it**

Run: `make test-run PKG=. RUN=TestE2ESendRefusesWhileAnotherSendRuns`
Expected: FAIL on the message (today: "Cannot lock ... you may forcefully remove").

**Step 3: Implement** (backup.go, the `lferr != nil` branch after `TryLock`):

```go
	if lferr != nil {
		if owner, oerr := lock.GetOwner(); oerr == nil && errors.Is(lferr, lockfile.ErrBusy) {
			log.AppLogger.Errorf(
				"Another send of %s is running (pid %d holds %s); exiting. Run again when it is done.",
				jobInfo.VolumeName, owner.Pid, lockFilePath,
			)
			return lferr
		}
		log.AppLogger.Errorf(
			"Cannot lock %q, reason: %v. If no other execution of %s is working on %s, you may forcefully remove the lock file located %s.",
			lock, lferr, config.ProgramName, jobInfo.VolumeName, lockFilePath,
		)
		return lferr
	}
```

(`lockfile` is already imported for `volumeLock`; add `errors` if missing.)

**Step 4: Run the test, then the package**

Run: `make test-run PKG=. RUN=TestE2ESendRefusesWhileAnotherSendRuns` → PASS; then
`make test-run PKG=. RUN=.`.

**Step 5: Commit**

```bash
git add backup/backup.go e2e_failure_test.go
git commit -m "send: name the running send when the lock is held"
```

### Task 12: `plan $DS $URI --schedule` from a real destination (e2e)

**Files:**
- Test: `e2e_test.go` (new `TestE2EPlanFromDestinationWithSchedule`)

**Step 1: Write the test** (it should pass on the current code; it pins the runbook's
step 5b)

```go
// TestE2EPlanFromDestinationWithSchedule: plan reads the backups at a real
// destination and projects the schedule from them, so the first run is a no-op
// and the next monthly chains from the full that send uploaded.
func TestE2EPlanFromDestinationWithSchedule(t *testing.T) {
	env := newE2EEnv(t)
	sc, err := backup.LoadScenario(monthlyOnlyScenario)
	if err != nil {
		t.Fatal(err)
	}
	env.writeSnapshots(t, "tank/data", sc.Snapshots)
	flags := scenarioFlags(t, monthlyOnlyScenario)
	target := "file://" + env.dest
	env.sendOK(t, append(flags, "tank/data", target)...)

	out, logs, err := env.plan(append(flags,
		"--schedule", "policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2026-10-02T01:00:00Z,every=24h,checks=coverage:_monthly",
		"tank/data", target)...)
	want := "2026-09-24T01:00:00Z..2026-09-30T01:00:00Z  NOOP x7  nothing-newer\n" +
		"2026-10-01T01:00:00Z  INCR  autosnap_2026-10-01_00:00:00_monthly  from autosnap_2026-09-01_00:00:00_monthly  newer-candidate\n" +
		"2026-10-02T01:00:00Z  NOOP  nothing-newer\n" +
		"checks: OK\n"
	if err != nil || out != want {
		t.Errorf("plan returned %v and printed:\n%s\nwant:\n%s\nlogs:\n%s", err, out, want, logs)
	}
}
```

**Step 2: Run**: `make test-run PKG=. RUN=TestE2EPlanFromDestinationWithSchedule` → PASS.
If it fails, the difference is the finding (most likely: the first run is a FULL because
the destination was not read; then `runPlan`'s destination branch is broken).

**Step 3: Commit**

```bash
git add e2e_test.go
git commit -m "e2e: plan projects a schedule from the backups at a destination"
```

---

## Part 4: documentation

### Task 13: runbook and the use-cases report

**Files:**
- Modify: `docs/runbook-first-backup.md`
- Modify: `docs/specs/2026-09-24--plan-harness/use-cases-report.md` (append a section)

**Step 1: Runbook edits**, each anchored to a scenario or test from this plan:

1. Under the `FLAGS` block: why `4320h`: "180 days keeps every full on the 1st of
   March and September (`monthly-only-3-years`); a window over 181 days lets some gaps
   stretch to seven months, which `full-cadence` tolerates."
2. Step 1, after the sanoid grep: also grep `_hour|_min|_mday|_wday|autoprune`; the
   simulation assumes sanoid's defaults (00:00, the 1st, Monday, `autoprune=yes`).
   And: sanoid's first monthly ever is taken when it is installed, mid-month
   (`autosnap_2026-06-16_00:18:47_monthly` on the captured pool); fulls land on the
   1st once a 1st has passed. Retention floor: `monthly=2` chains with no missed runs,
   `monthly=3` survives two missed months, `monthly=1` never chains
   (`monthly-only-weekly-2-monthlies`, `monthly-only-monthly-1-unusable`).
3. Step 2: the new schedule keys: `snapshot-delay=3m` ("sanoid lands its snapshots
   2-4 minutes after the boundary; a cron at the same minute sends a day late,
   `monthly-only-midnight-cron`") and `skip=<from>..<to>` for an outage
   (`monthly-only-year-2-month-outage`). Add `only:_monthly` to the checks table:
   "a backup is sent of a snapshot not ending in `_monthly` (a forgotten
   `--incrementalSnapshotSuffix`, `monthly-only-missing-incr-suffix`)". Recommend
   `checks=coverage:_monthly,only:_monthly` in the step 2 command and in step 6.
4. Step 3: a destination with manifests in S3 Glacier: reading them issues restores
   (billable, hours); `DEEP_ARCHIVE` is not supported at all. Lifecycle rules must
   leave `manifests|*` alone (or the whole prefix). The cache hides this on the backup
   host and it surfaces on another host or at restore time.
5. Step 5: cron timing ("run at least an hour after sanoid's boundary; `from=` in step
   2 should be that time"); overlap ("a full that outlives the cron interval makes the
   next run exit non-zero with `Another send of ... is running (pid N holds ...)`; it
   uploads nothing; this is the one non-zero status that is not a failure"); restart
   recipe ("to start a new chain by hand, `send --full --fullSnapshotSuffix _monthly`;
   `plan` shows `FULL ... explicit-full`; the next monthly chains from it;
   `coverage:_monthly` will from then on report that monthly as the base of two
   backups in any simulation that starts from this destination"); step 5b: at each
   six-month mark, `zfsbackup plan $FLAGS --schedule "policy=...,until=...,every=24h,
   checks=coverage:_monthly,only:_monthly,location=..." $DS $URI` projects the next
   year from the real destination (`TestE2EPlanFromDestinationWithSchedule`).
6. Step 5, the "chain tolerates missed runs" paragraph: cite
   `monthly-only-year-2-month-outage` (December skipped, not lost) and
   `monthly-only-gap-3-months-then-year` (the cadence restarts from the recovery full).
7. Switching flags: a short paragraph pointing at `switch-daily-to-monthly-only`.

**Step 2: use-cases-report.md**: append "## 7. Added 2026-10-08" listing the new
scenarios in the table style of the file, the three schedule keys, `only:`, and the
three e2e tests; strike the "Gaps" bullets this plan closed.

**Step 3: Commit**

```bash
git add docs/runbook-first-backup.md docs/specs/2026-09-24--plan-harness/use-cases-report.md
git commit -m "docs: runbook for cron timing, lifecycle rules, outages, restarts; use cases added"
```

### Task 14: final verification

Run, in order:

```bash
make fmt-check
make scenarios
make test-run PKG=./backup/ RUN=.
make test-run PKG=./cmd/ RUN=.
make test-run PKG=. RUN=.
make test-docker
```

All must pass. Then update `.claude/napkin.md` (new schedule keys, the `only:` check,
`PartialSetCompletable`, the lock message) and the memory note
`plan-audit-2026-10-08.md` (implemented, unpushed).
