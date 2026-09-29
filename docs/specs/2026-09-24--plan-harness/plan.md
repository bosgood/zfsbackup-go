# Plan harness: snapshot names in, action plans out

## Context

Smart-backup target selection is `selectSmartSnapshots(jobInfo, snapshots,
destBackups)` in `backup/backup.go:113`. It is pure: `ProcessSmartOptions`
(`backup/backup.go:58`) fetches the snapshot list with
`zfs.GetSnapshotsAndBookmarks` and the per-destination manifests with
`getBackupsForTarget` (`backup/backup.go:239`, sorted newest-first by
`BaseSnapshot.CreationTime`) and delegates. `TestSelectSmartSnapshots`
(`backup/backup_test.go:131`) exercises single decisions.

The failures we care about are multi-run failures (see the napkin's "Open
issues found 2026-09-22"):

- `hasNewerFullBase` compares against the last *full* instead of the last
  *backup*, so once the window elapses it re-sends an already-backed-up monthly
  (monthly-only mode) or takes a full that later incrementals never chain from
  (monthly/daily mode).
- The pruned-source fallback can re-send the existing full on every run until
  a newer monthly appears.
- `ErrNoOp` exits 255, which is the normal daily outcome in monthly-only mode.

None of these are visible from one `send --dry-run`. The harness below makes
the *sequence* testable, from fixtures, from the real pool listing, and through
the real pipeline.

## Design decisions

1. **The planner lives in package `backup`** (`backup/plan.go`), not a new
   package. It needs `selectSmartSnapshots`, `getBackupsForTarget`,
   `validateSnapShotExistsFromSnaps` (`backup/sync.go:148`) and
   `linkManifests` (`backup/list.go:168`). Exporting a handful of `Plan*`
   entry points is less churn than exporting those internals.
2. **Fixture-driven golden scenarios** under `backup/testdata/scenarios/`,
   not more Go table cases. Adding a scenario means dropping in a directory,
   and the same files drive the CLI and the end-to-end tier. Goldens are
   regenerated with `-update`.
3. **Decision returns data, side effects stay in the wrapper.** Refactor the
   body of `selectSmartSnapshots` into `planSmartSnapshots` returning a `Plan`
   (action, base, source, reason). `selectSmartSnapshots` applies the plan to
   `jobInfo`, logs, and maps a no-op to `ErrNoOp`, so its contract and the
   existing test are unchanged. Reasons become stable strings instead of log
   lines.
4. **End-to-end through a fake `zfs`, not a mock at the Go level.** The root
   command already has `--zfsPath` (`cmd/root.go:130`) and
   `--workingDirectory` (`config.WorkingDir`, `cmd/root.go:371`). A tiny fake
   binary lets the real `send` run against a `file://` destination on macOS or
   in Docker, writing and reading real manifests.
5. **Real ZFS is optional and last.** The host has no ZFS. The only things a
   real pool adds are tie ordering among snapshots with equal creation seconds
   and bookmarks; both are handled as explicit checks/fixtures instead.

## Layout

```
backup/plan.go                       Plan type, planSmartSnapshots, Simulate, text/JSON rendering
backup/plan_schedule.go              sanoid-style Schedule: generate + prune snapshots over time
backup/plan_checks.go                invariants over a Simulation
backup/plan_fixtures.go              LoadScenario: flags, snapshots.txt, manifests.txt, schedule, checks
backup/plan_test.go                  TestScenarios (golden), TestTieOrderIndependence, per-check tests
backup/testdata/scenarios/<name>/    flags, snapshots.txt, [manifests.txt], [schedule], expected.txt
zfs/zfs.go                           ParseSnapshotList(io.Reader) split out of GetSnapshotsAndBookmarks
cmd/plan.go                          `zfsbackup plan` subcommand
internal/fakezfs/                    fake zfs: library + TestMain hook (test binary re-executes itself as zfs)
zfs/zfs_test.go                      zfs package tests against the fake (package zfs_test)
e2e_test.go                          TestE2E*: real `send` via fake zfs against file://
Makefile, Dockerfile, README.md      targets, go 1.25 base image, docs
```

## Fixture formats

All fixture files ignore blank lines and `#` comments.

`flags`: one flag per line, exactly as passed to `zfsbackup send`.

```
--fullIfOlderThan=4320h
--fullSnapshotSuffix=_monthly
--incrementalSnapshotSuffix=_monthly
```

`snapshots.txt`: either raw `zfs list -H -p -t snapshot,bookmark -o
name,creation,type -S creation <ds>` output (dataset prefix optional), or bare
sanoid names, whose creation time is derived from the embedded timestamp
(`autosnap_2006-01-02_15:04:05_<period>`, parsed in the schedule's location,
UTC by default). Input order is preserved for raw listings so the planner sees
exactly what `ProcessSmartOptions` would see.

```
tank/data@autosnap_2026-09-01_00:00:01_monthly	1756684801	snapshot
tank/data#autosnap_2026-08-01_00:00:01_monthly	1754006401	bookmark
autosnap_2026-09-24_00:00:01_daily
```

`manifests.txt` (optional): backups already at the destination, one per line,
any order. A full is `<base>`; an incremental is `<source> to <base>`. Names
resolve against `snapshots.txt` first, then the sanoid timestamp, then an
explicit `<name>\t<epoch>`. Pruned sources therefore need no entry in
`snapshots.txt`.

```
autosnap_2026-08-01_00:00:01_monthly
autosnap_2026-08-01_00:00:01_monthly to autosnap_2026-09-01_00:00:01_monthly
```

`schedule` (optional; its presence turns a next-run scenario into a
simulation), `key=value` lines:

```
policy=hourly=36,daily=30,monthly=3   # sanoid retention counts
from=2026-09-24T01:00:00Z             # first run; default: newest snapshot + 1h
until=2027-10-01T01:00:00Z
every=24h                             # run cadence (cron)
checks=coverage:_monthly              # extra checks beyond the defaults
```

`expected.txt`: the golden rendering, identical to the CLI's text output.

```
2026-09-24T01:00:00Z  FULL  autosnap_2026-09-01_00:00:01_monthly  no-previous-full
2026-09-25T01:00:00Z..2026-09-30T01:00:00Z  NOOP x6
2026-10-01T01:00:00Z  INCR  autosnap_2026-10-01_00:00:01_monthly  from autosnap_2026-09-01_00:00:01_monthly  newer-candidate
...
2027-03-01T01:00:00Z  FULL  autosnap_2027-03-01_00:00:01_monthly  window-elapsed
checks: OK
```

Next-run scenarios (no `schedule`) render a single line labelled `next`
instead of a timestamp. Errors render as `<at>  ERROR  <message>`. Check
failures render as `checks: N violation(s)` followed by one indented line each.

Reasons are a closed set: `no-previous-full`, `window-elapsed`,
`source-pruned`, `newer-candidate`, `nothing-newer`, `explicit-full`,
`explicit-incremental`.

## Data model (`backup/plan.go`)

```go
type PlanAction string

const (
	PlanFull        PlanAction = "full"
	PlanIncremental PlanAction = "incremental"
	PlanNoop        PlanAction = "noop"
)

type Plan struct {
	Action PlanAction
	Base   files.SnapshotInfo // zero for noop
	Source files.SnapshotInfo // zero unless incremental
	Reason string
}

// planSmartSnapshots is the pure decision. selectSmartSnapshots becomes:
//   p, err := planSmartSnapshots(jobInfo, snapshots, destBackups)
//   apply p to jobInfo.BaseSnapshot/IncrementalSnapshot, log the reason,
//   return ErrNoOp when p.Action == PlanNoop.
func planSmartSnapshots(jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo) (Plan, error)

type Step struct {
	At        time.Time
	Snapshots []files.SnapshotInfo // what the pool looked like at this run
	Plan      Plan
	Err       error
}

type Simulation struct {
	Volume    string
	Window    time.Duration
	Steps     []Step
	Manifests [][]*files.JobInfo // destination state after the last step, newest-first per destination
}

type Scenario struct {
	JobInfo     files.JobInfo
	Snapshots   []files.SnapshotInfo
	DestBackups [][]*files.JobInfo
	Schedule    *Schedule       // nil => single next-run step
	From, Until time.Time
	Every       time.Duration
	Checks      []string
}

func LoadScenario(dir string) (*Scenario, error)
func (s *Scenario) Run() *Simulation
func (sim *Simulation) Check(checks []string) []Violation
func (sim *Simulation) WriteText(w io.Writer, violations []Violation) error
func (sim *Simulation) WriteJSON(w io.Writer, violations []Violation) error

// BackupsAtTarget exports getBackupsForTarget for cmd/plan.go (live mode) and e2e_test.go.
func BackupsAtTarget(ctx context.Context, volume, target string, jobInfo *files.JobInfo) ([]*files.JobInfo, error)
```

`Scenario.Run` loop, mirroring `ProcessSmartOptions` exactly:

```
snaps := s.Snapshots; dest := clone(s.DestBackups)
for at := s.From; !at.After(s.Until); at = at.Add(s.Every) {
	if s.Schedule != nil { snaps = s.Schedule.Advance(snaps, prev, at) }
	ji := s.JobInfo                                  // copy: planner reads flags only
	p, err := planSmartSnapshots(&ji, snaps, dest)
	steps = append(steps, Step{at, snaps, p, err})
	if err == nil && p.Action != PlanNoop {
		m := &files.JobInfo{VolumeName: ji.VolumeName, BaseSnapshot: p.Base}
		if p.Action == PlanIncremental { m.IncrementalSnapshot = p.Source }
		for i := range dest { dest[i] = insertNewestFirst(dest[i], m) } // same sort as getBackupsForTarget
	}
	if s.Schedule == nil { break }
}
```

## Schedule (`backup/plan_schedule.go`)

```go
type Period struct{ Name string; Keep int }       // hourly, daily, weekly, monthly, yearly
type Schedule struct {
	Periods  []Period
	Location *time.Location // default UTC; names are rendered in this zone
}
func ParseSchedule(policy string) (*Schedule, error)  // "hourly=36,daily=30,monthly=3"
// Advance adds every snapshot whose boundary falls in (from, to], prunes each
// period down to Keep newest, and returns the list newest-first.
func (s *Schedule) Advance(snaps []files.SnapshotInfo, from, to time.Time) []files.SnapshotInfo
```

Boundaries follow sanoid defaults: hourly at `:00`, daily at `00:00`, weekly
Monday `00:00`, monthly on the 1st at `00:00`, yearly on Jan 1. Names are
`autosnap_YYYY-MM-DD_HH:MM:SS_<period>`. Coincident periods (the 1st of a
month at midnight produces monthly, daily and hourly) get the *same* creation
second; the list order among them is fixed (`yearly, monthly, weekly, daily,
hourly`) but nothing may depend on it (see `TestTieOrderIndependence`). Only
`autosnap_*` names are ever pruned. Non-`autosnap` snapshots in the input are
left alone.

## Checks (`backup/plan_checks.go`)

Run on every simulation. Each returns zero or more `Violation{Check, At, Detail}`.

| Check | Fails when | Catches |
|---|---|---|
| `chain-links` | an incremental manifest's source is not the base of any manifest at the same destination (`linkManifests` leaves `ParentSnap` nil) | broken restore chains |
| `source-present` | at a step, the incremental source was not in that step's snapshot list | sending from a pruned snapshot |
| `no-duplicate-send` | two manifests share (base, source), or two fulls share a base | the re-send-existing-full bug |
| `no-orphan-full` | after a FULL step, the next non-noop step's source is not that full's base | the `hasNewerFullBase` orphaning in monthly/daily mode |
| `full-cadence` | gap between consecutive fulls is outside `[window - slack, window + slack]` where slack is the longest period in the schedule, or more than one full per window | spurious fulls, missed rolls |
| `restore-depth` | walking parents from the newest manifest to a full takes more than `ceil(window/shortest incremental period) + 1` links | chains that grow forever |
| `coverage:<suffix>` (opt-in) | a snapshot matching `<suffix>` created inside the horizon is the base of zero or more than one manifest | the monthly-only goal itself |

Every check gets one unit test with a hand-built `Simulation` that violates
it and one that passes.

## CLI (`cmd/plan.go`)

```
zfsbackup plan [smart flags] [--snapshots FILE|-] [--manifests FILE]
               [--schedule "policy=hourly=36,daily=30,monthly=3,until=2027-10-01T00:00:00Z,every=24h"]
               [--json] <volume> [<destination uri(s)>]
```

- Smart flags are the same six as `send` (`--full`, `--increment`,
  `--fullIfOlderThan`, `--snapshotPrefix`, `--fullSnapshotSuffix`,
  `--incrementalSnapshotSuffix`), bound to the shared `jobInfo` the same way
  `cmd/send.go` binds them. Extract the "only one smart option" check from
  `updateJobInfo` (`cmd/send.go`, the `onlyOneCheck` block) into
  `validateSmartFlags()` and call it from both commands.
- `--snapshots` absent: query the live pool via `zfs.GetSnapshotsAndBookmarks`
  (honours `--zfsPath`).
- Destination URIs given: read live manifests with `backup.BackupsAtTarget`
  (this syncs the local cache under `--workingDirectory`, read-only for the
  destination; PGP flags apply because manifests may be encrypted).
  `--manifests` and destination URIs are mutually exclusive.
- Exit 0 when a plan is produced, including NOOP. Exit 2 when any check fails,
  so cron/CI can gate on it. Other errors exit 1.
- Text output is byte-identical to `expected.txt` so a golden can be produced
  with `zfsbackup plan ... > expected.txt`.

## Fake zfs (`internal/fakezfs`)

A pure-Go stand-in for the `zfs` binary, so every call site in `zfs/zfs.go`
can be exercised by `go test ./...` with no ZFS, no shell script and no build
step. It is a library, not a `main` package: the test binary re-executes
*itself* as `zfs`, the same trick the Go standard library uses for
`os/exec` tests.

```go
// internal/fakezfs/fakezfs.go
package fakezfs

// Main implements the fake. Pure: takes args and streams, returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// RunIfRequested turns the current process into the fake when FAKEZFS=1.
// Call it first in TestMain; it never returns in fake mode.
func RunIfRequested() {
	if os.Getenv("FAKEZFS") != "1" {
		return
	}
	os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
```

A test package opts in with

```go
func TestMain(m *testing.M) {
	fakezfs.RunIfRequested()
	os.Exit(m.Run())
}
```

and then sets `zfs.ZFSPath = os.Args[0]` (or passes `--zfsPath os.Args[0]`)
and `t.Setenv("FAKEZFS", "1")`. Children spawned by `exec.Command` inherit the
environment, so every `zfs` invocation lands in `Main`. This also works for
the out-of-process case: the real `zfsbackup` binary started by a test with
`FAKEZFS=1` in its environment and `--zfsPath <test binary>` spawns the test
binary, which runs the fake. `Main` reads fixtures with `zfs.ParseSnapshotList`,
so `zfs/zfs_test.go` must be an external test package (`package zfs_test`) to
avoid an import cycle. Configuration is by environment:

| Variable | Meaning |
|---|---|
| `FAKEZFS_SNAPSHOTS` | path to a `snapshots.txt` fixture (same parser as the planner) |
| `FAKEZFS_STREAM_BYTES` | bytes emitted by `zfs send` (default 65536) |
| `FAKEZFS_LOG` | file to append every argv line to, for assertions |

| Invocation (as issued by `zfs/zfs.go`) | Behaviour |
|---|---|
| `list -H -d 1 -p -t snapshot,bookmark -r -o name,creation,type -S creation <ds>` (`zfs.go:56`) | print fixture rows for `<ds>`, newest-first, `name\tepoch\ttype` |
| `get -H -p -o value creation <ds@snap>` / `<ds#bm>` (`zfs.go:99`) | print the epoch; a bare dataset prints `1` |
| `send -n -P [flags] [-i\|-I src] <ds@snap>` (`zfs.go:171`) | print `size\t<n>` |
| `send [flags] [-i\|-I src] <ds@snap>` (`zfs.go:114`) | write `<n>` deterministic bytes seeded from `src+snap` to stdout |
| anything else | exit 1 with the argv on stderr, so unexpected calls fail loudly |

## Tasks

Work on branch `clean-dry-run` (or a branch off it). Every task ends green; run
`make test-docker` before each commit. Napkin rule: extend the Makefile rather
than running ad-hoc commands.

### Phase 0: prerequisites

**T0.1 Fix the test image.** `Dockerfile` line 19: `FROM golang:1.23-bookworm`
→ `FROM golang:1.25-bookworm` (go.mod says `go 1.25`; the 1.23 image sets
`GOTOOLCHAIN=local` and fails at `go mod download`). Also drop the stale
"go.mod declares go 1.18" comment. Verify: `make test-docker` passes. Commit.

### Phase 1: planner core (pure, no I/O)

**T1.1 Snapshot list parser.** `zfs/zfs.go`: extract the read loop of
`GetSnapshotsAndBookmarks` into
`func ParseSnapshotList(r io.Reader, loc *time.Location) ([]files.SnapshotInfo, error)`
that also accepts bare names (derive creation from the sanoid timestamp;
error if neither an epoch column nor a timestamp is present), strips an
optional dataset prefix, and skips blank/`#` lines. `GetSnapshotsAndBookmarks`
calls it on the command's stdout, so live behaviour is unchanged.
Tests (`zfs/zfs_test.go`, table): raw row with `@`, raw bookmark row with
`#`, bare sanoid name, bare non-sanoid name → error, comment/blank lines.
Commit.

**T1.2 Decision returns a Plan.** `backup/plan.go`: add `PlanAction`, `Plan`,
`planSmartSnapshots` (move the body of `selectSmartSnapshots`, replacing each
`jobInfo.BaseSnapshot = ...; return nil` with a `Plan{...}` return and each
`return ErrNoOp` with `Plan{Action: PlanNoop, Reason: "nothing-newer"}`).
Rewrite `selectSmartSnapshots` as the 15-line wrapper described above.
Verify: `TestSelectSmartSnapshots` passes untouched. Add
`TestPlanSmartSnapshotsReasons`: three cases asserting `Reason`
(`no-previous-full`, `window-elapsed`, `source-pruned`). Commit.

**T1.3 Fixture loader.** `backup/plan_fixtures.go`: `LoadScenario(dir)`.
`flags` is parsed by building a throwaway `pflag.FlagSet` with the six smart
flags bound to a fresh `files.JobInfo` (default `FullIfOlderThan` must be
`-1m`, the "unset" sentinel from `cmd/send.go`). `manifests.txt` per the
grammar above; `VolumeName` on every manifest is the scenario's volume
(a `volume=` line in `schedule`, default `tank/data`). Test with a tiny
scenario in `backup/testdata/scenarios/loader-smoke/`. Commit.

**T1.4 Golden test, next-run scenarios.** `backup/plan_test.go`:

```go
var update = flag.Bool("update", false, "rewrite expected.txt for every scenario")

func TestScenarios(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/scenarios/*")
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			sc, err := LoadScenario(dir)         // fatal on error
			sim := sc.Run()
			var buf bytes.Buffer
			sim.WriteText(&buf, sim.Check(sc.Checks))
			golden := filepath.Join(dir, "expected.txt")
			if *update { os.WriteFile(golden, buf.Bytes(), 0644) }
			want, _ := os.ReadFile(golden)
			if !bytes.Equal(want, buf.Bytes()) { t.Errorf("plan differs from expected.txt:\n%s", diff(want, buf.Bytes())) }
			if strings.Contains(buf.String(), "violation") { t.Errorf("checks failed") }
		})
	}
}
```

Scenarios (all next-run, goldens written by hand first, then confirmed with
`-update` and reviewed):

- `first-run-empty-dest`: monthly-only flags, hourly/daily/monthly present, no
  manifests → `FULL <newest monthly> no-previous-full`.
- `monthly-only-next-incr`: last full is last month's monthly, a new monthly
  exists → `INCR <new monthly> from <old monthly> newer-candidate`.
- `monthly-only-noop`: nothing newer than the last backup → `NOOP`.
- `legacy-prefix-incr`: no suffix flags, `--snapshotPrefix autosnap_`,
  `--fullIfOlderThan 720h` → newest snapshot of any kind.
- `out-of-sync-destinations`: two destinations with different last fulls →
  `ERROR destinations are out of sync ...`.
- `raw-listing-with-bookmarks`: raw `zfs list` rows including a bookmark; the
  bookmark must never be chosen as base.

Add to Makefile:

```make
scenarios:
	go test -count=1 -run 'TestScenarios|TestTieOrder' -v ./backup/

scenarios-update:
	go test -count=1 -run TestScenarios ./backup/ -update
```

Commit.

### Phase 2: simulation over time

**T2.1 Schedule.** `backup/plan_schedule.go` per the design. Tests
(`TestScheduleAdvance`): one day of hourlies adds 24 and prunes to `Keep`;
crossing the 1st adds monthly+daily+hourly with equal creation times; nothing
is added when `(from, to]` contains no boundary; non-`autosnap` snapshots
survive pruning. Commit.

**T2.2 Simulate + rendering.** Implement `Scenario.Run` and the NOOP-collapsing
renderer; teach `LoadScenario` the `schedule` file. Scenarios whose goldens
are correct *with the current code* (keep the horizon inside one window so the
known bugs do not fire):

- `monthly-only-5-months`: from 2026-09-24, until 2027-02-20, `every=24h`,
  `policy=hourly=36,daily=30,monthly=3`. Expected: one FULL (2026-09-01
  monthly), then INCR on each 1st, NOOP otherwise.
- `monthly-daily-5-months`: same but `--incrementalSnapshotSuffix _daily`:
  INCR every day from the previous day, with the daily on the 1st a NOOP
  (same creation second as the monthly).

Commit.

**T2.3 Checks.** `backup/plan_checks.go` per the table, wired into
`TestScenarios`; per-check unit tests. Add `checks=coverage:_monthly` to the
monthly-only scenarios. Commit.

**T2.4 Tie-order independence.** `TestTieOrderIndependence`: for the
`monthly-only-5-months` and `monthly-daily-5-months` scenarios, replace
`Schedule.Advance` ordering with every permutation of the coincident
snapshots (and a variant where they are 1 s apart in each order); assert
that the FULL steps and all checks are identical across permutations. Commit.

### Phase 3: CLI and Makefile

**T3.1 `zfsbackup plan`.** `cmd/plan.go` per the CLI section, plus
`validateSmartFlags()` extracted from `updateJobInfo` and reused by `send`.
`cmd/plan_test.go` runs `RootCmd.SetArgs([]string{"plan", "--snapshots", ...})`
in-process (pattern: `integration_test.go:219`, call `cmd.ResetSendJobInfo()`
first; add the new flags to that reset) for: fixture next-run, fixture with
`--schedule`, `--json`, and the exit-2 path via a scenario with a failing
check. Commit.

**T3.2 Makefile + README.** Add:

```make
# Usage: make plan ARGS="--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly \
#   --incrementalSnapshotSuffix _monthly --snapshots snaps.txt tank/data"
plan:
	go run . plan $(ARGS)
```

README: a "Planning a smart backup" section showing the capture command, the
`plan` invocation with and without `--schedule`, and `send -n`. Commit.

### Phase 4: fix the known issues, driven by scenarios

Each task: add the scenario with the *desired* golden, run
`make scenarios` and see it fail, fix, see it pass, commit together.

**T4.1 `hasNewerFullBase` compares against the last backup.**
`backup/plan.go`: `fullBase.CreationTime.After(lastBackup[0].CreationTime)`
instead of `lastFull`. Scenarios: `monthly-only-year` (until 2027-10-01;
expected fulls on the first run for 2026-09-01, then 2027-03-01 and
2027-09-01 monthlies, every monthly exactly once, `coverage:_monthly`) and
`monthly-daily-year` (`no-orphan-full` and `restore-depth` must pass).

**T4.2 Pruned-source fallback requires a newer full candidate.** When the
incremental source is gone locally and `fullBase` is not newer than
`lastBackup[0]`, return `Plan{Action: PlanNoop, Reason: "source-pruned"}`
(nothing new exists to send; log a warning in the wrapper). Scenario
`pruned-source-no-newer-monthly` with `no-duplicate-send`.

**T4.3 `ErrNoOp` exits 0.** `cmd/root.go:76` `Execute`: `errors.Is(err,
backup.ErrNoOp)` → `os.Exit(0)`; downgrade the `Errorf` in `updateJobInfo`
to `Noticef` for that error. Keep `RootCmd.ExecuteContext` returning
`ErrNoOp` (the integration test at `integration_test.go:292` relies on it).
Covered by the exit-code case in T5.2.

### Phase 5: the ZFS side, in pure Go

**T5.1 Fake zfs library.** `internal/fakezfs/fakezfs.go` per the table, plus
`RunIfRequested`. `fakezfs_test.go` calls `Main` directly with args and
buffers (no process spawn) for each verb: `list` prints newest-first rows
for the requested dataset only; `get creation` on a snapshot, a bookmark and
a bare dataset; `send -n -P` prints exactly one `size` line; `send` writes
`FAKEZFS_STREAM_BYTES` bytes and two different (base, source) pairs produce
different bytes; an unknown verb returns 1 and echoes argv to stderr; every
call appends to `FAKEZFS_LOG`. Commit.

**T5.2 `zfs` package tests.** `zfs/zfs_test.go` (`package zfs_test`, the
package has no tests today) with the `TestMain` hook, `zfs.ZFSPath =
os.Args[0]` and `t.Setenv("FAKEZFS", "1")`:

- `TestGetSnapshotsAndBookmarks`: fixture with snapshots and a bookmark;
  names are stripped of the dataset prefix, the bookmark is flagged, order is
  preserved, creation times match the epochs.
- `TestGetCreationDate`: snapshot and bookmark targets; unknown target
  returns an error that includes the fake's stderr text (the
  `"%s (%v)"` wrapping in `zfs.go:99`).
- `TestGetZFSSendDryRun`: returns the size the fake printed; a fixture that
  makes the fake omit the `size` line yields the "could not parse" error.
- `TestGetZFSSendCommand` (pure, no fake): table over `-i`/`-I`, `-R`, `-w`,
  `-p`, `-c` for `Compressor=zfs`, and bookmark sources rendered as
  `ds#name`.

Commit.

**T5.3 `e2e_test.go`** (package `main`, no build tag). `TestMain` installs the
hook; every case passes `--zfsPath os.Args[0]` and sets `FAKEZFS=1`,
`FAKEZFS_SNAPSHOTS` and `FAKEZFS_LOG` with `t.Setenv`. Cases:

- `TestE2EDryRunSendsNothing`: `send -n` with monthly-only flags; the
  destination dir stays empty and `FAKEZFS_LOG` has no `send` line without
  `-n`.
- `TestE2ESequenceMatchesPlanner`: for `monthly-only-year`, iterate the
  simulation's steps; for each step write `Step.Snapshots` to a fixture, point
  `FAKEZFS_SNAPSHOTS` at it, run `send --zfsPath <test binary>
  --workingDirectory <tmp> --maxParallelUploads 1 <flags> tank/data
  file://<dest>` in-process, accept `nil` or `backup.ErrNoOp`. Afterwards read
  the destination with `backup.BackupsAtTarget` and assert the (base, source)
  list equals the simulation's `Manifests[0]`. This proves the real manifest
  write/read/sort path agrees with the planner.
- `TestE2ENoopExitCode`: `go build` the real binary (the one place a
  toolchain call remains; skip with `t.Skip` if `go` is not on PATH), run a
  NOOP `send` with `FAKEZFS=1` in its environment and `--zfsPath <test
  binary>`, assert exit status 0, and that `send -n` output contains
  `Dry-run:` lines at the default log level.

Makefile: `e2e: go test -count=1 -run TestE2E -v .` and include it in
`check`. Commit.

Optional, only if manual demos on a machine without ZFS turn out useful: a
ten-line `internal/fakezfs/cmd/main.go` wrapping `Main`, so `zfsbackup send -n
--zfsPath $(go build -o /tmp/zfs ./internal/fakezfs/cmd && echo /tmp/zfs)`
works by hand.

### Phase 6: first-run runbook (docs/runbook-first-backup.md)

1. On the pool host: `zfs list -H -p -t snapshot,bookmark -o
   name,creation,type -S creation <ds> > snaps.txt`; note the sanoid retention
   counts from `/etc/sanoid/sanoid.conf`.
2. `zfsbackup plan --snapshots snaps.txt --schedule
   "policy=<counts>,until=<+400d>,every=24h" <flags> <ds>`: review the year;
   `checks: OK`.
3. `zfsbackup plan <flags> <ds> <real uri>` (live snapshots, live manifests).
   Expect `FULL <newest monthly> no-previous-full`. If the destination holds
   manifests from earlier attempts, the plan will chain from them; decide
   whether to keep or `clean` them before continuing.
4. `zfsbackup send -n <flags> <ds> <uri>`: same base, plus the size estimate.
5. Run for real. Next day, `zfsbackup send -n` should report NOOP (exit 0).
6. Commit `snaps.txt` as `backup/testdata/scenarios/prod-<ds>/snapshots.txt`
   with the production `flags` and a year-long `schedule`, so the real names
   are regression-tested from now on.

### Optional: real ZFS

Only needed to confirm how `zfs list -S creation` orders snapshots that share
a creation second. Cheapest: on the real pool on the 1st of a month, run the
capture command and look at the order of the coincident monthly/daily/hourly.
If it differs from `Schedule.Advance`, encode the observed order there. A
Linux VM (`limactl` with the ZFS module, or Docker `--privileged` on a Linux
host) can run `make integration` with `travis-setup.sh`, but that suite does
not touch the smart path today.

## Verification

- `make test-docker` green after every task (T0.1 first).
- `make scenarios` lists every scenario; `make scenarios-update` rewrites
  goldens, and the diff is reviewed by hand before committing.
- `make e2e` green on the host and in the image; `go test ./zfs/` passes
  with no `zfs` binary installed.
- The production scenario (`prod-<ds>`) shows fulls exactly every 6 months and
  every `_monthly` exactly once, with `checks: OK`.

## Assumptions to confirm

- Production flags are `--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
  --incrementalSnapshotSuffix _monthly`, one destination, daily cron.
- Sanoid retention counts (needed for the `schedule` policy and for the
  `pruned-source` scenarios).
- `plan` as a real subcommand is wanted (versus test-only tooling). It is the
  only way to run the year-ahead projection against the live destination on
  the pool host, which is the "first time" check.

## Implementation notes (2026-09-24)

All phases landed, each commit green in `make test-docker`. Where the code
differs from the plan above:

- **Pruning follows sanoid's real rule.** A snapshot is destroyed once it is
  older than count x period (hourly 1h, daily 24h, weekly 7d, monthly 31d,
  yearly 365.25d), oldest first, but never while count or fewer remain. Pools
  therefore hold count or count+1 of each period, as on the real pool
  (`hourly=48` shows 49), rather than "down to Keep newest".
- **Check slack.** `full-cadence` and `restore-depth` add slack for the wait
  until the next full candidate: one period of the full suffix (`_monthly` →
  31d), plus one run. `restore-depth` allows
  `ceil((window + slack) / spacing) + 1` incrementals, where spacing is the
  larger of the run cadence and the incremental suffix's minimum period
  (monthly 28d). Without the slack, the fixed monthly/daily year (183-link
  chains) would fail the plan's `ceil(window/1d)+1 = 181`.
  `no-orphan-full` only looks at the next incremental; `full-cadence` covers
  consecutive fulls.
- **Rendering.** NOOP lines keep their reason (`NOOP x6  nothing-newer`), so
  `source-pruned` no-ops stand out. Identical consecutive errors collapse like
  no-ops. Bookmark sources render as `from #name`. Violations tied to no run
  print without a time.
- **Fixtures.** `manifests.txt` separates destinations with `---`, and each
  name takes an optional epoch (`name epoch`), which wins over the snapshot
  list. A scenario simulates runs iff `until=` is set. A `schedule` file with
  only `volume=`, `location=` or `checks=` stays a next-run scenario.
  `--schedule` takes the same comma-separated items. Snapshot lists are
  stable-sorted newest-first, which leaves a `-S creation` capture unchanged.
- **CLI.** `plan` uses the existing global `--jsonOutput` rather than a new
  `--json`. Exit codes come from `cmd.exitCode`: 0 on success or `ErrNoOp`,
  2 on failed checks, 1 for other `plan` errors, and 255 otherwise (as
  before). The six smart flags are registered once, by
  `backup.AddSmartFlags`.
- **T2.2 correction.** With `--incrementalSnapshotSuffix _daily`, the daily of
  the 1st is sent as an ordinary incremental (it is the newest `_daily`), not
  skipped as a no-op.
- **T4.2.** The pruned-source no-op is a next-run scenario. A simulation
  through the next monthly would end in a source-pruned full one month after
  the last full, which `full-cadence` rightly flags.
- **Tests beyond the plan.** `TestTieOrderIndependence` also covers the two
  year-long scenarios, where the roll-overs fall on coincident snapshots, and
  it fails if the variants do not change the pool. The fake zfs refuses sends
  whose target or source is not in the fixture. `TestE2EExitCodes` also
  checks `plan` exiting 2.
- **Fixture epochs.** The plan's sample epochs (1756684801, ...) are for 2025,
  not 2026. The fixtures compute theirs.

After an independent review of the implementation:

- **`no-errors` default check.** A failed run is a violation, so `plan`
  exits 2 instead of printing `checks: OK`. A scenario can declare
  deliberate violations with an `expect-violations` file
  (`out-of-sync-destinations` does).
- **`no-duplicate-send`** flags any send of a snapshot already backed up at
  the destination. That catches the original monthly-only bug without the
  opt-in `coverage:` check.
- **`full-cadence`** needs a known slack, and bounds a gap above only after a
  full a run sent, so a full catching up after the initial state is fine.
  `restore-depth` is linear per run.
- **Diverged destinations.** The full roll decision compares with the
  destination furthest behind, so its outcome does not depend on the order
  of the destinations. `Plan.FullDue` lets `send` say when a full is due but
  waiting for a newer monthly. Runs only see snapshots created by their time.
- **CLI.** `Execute` removes the temporary directory of failed commands, which
  a daily `ErrNoOp` used to leak. `plan` loads keys like a smart `send`,
  rejects stdin read twice, and rejects schedules with no runs. Live
  `zfs list` output is parsed strictly (tabs only, names verbatim).
- **e2e.** Every next-run golden is replayed through the real `send` after its
  manifests are recreated with manual sends. The year replay checks each
  send. The binary test checks the default-level `Dry-run:` output and that
  no temporary directories are left.
- **Not changed:** a `source-pruned` no-op still exits 0 with a warning. An
  alternative is an incremental from an older backup still on the pool; that
  is a design decision for later.

A real capture, added afterwards:

- `testdata/zfs/snap-navidrome-2026-09-24.json` is `zfs list` output for
  `backup3/enc/user/app/navidrome` converted to JSON: oldest first, with no
  creation times. `zfs.ParseSnapshotList` reads JSON arrays of rows, and a
  scenario may hold a `snapshots.json` (here a link to the capture) instead
  of `snapshots.txt`. `prod-navidrome-first-run` and `prod-navidrome-year`
  plan from it, reading the times from the names in `America/New_York`.
- It answers part of the tie-order question above. On every 1st, sanoid
  created the monthly, then the daily, then the hourly (createtxg order), all
  with the same timestamp in their names. Loaded, they are listed in that
  order, which is also how `Schedule.Advance` lists coincident snapshots. The
  plans do not depend on the order either way (`TestTieOrderIndependence`).

Weekly cadence on a three-monthly pool (2026-09-24, from the capture):

- `prod-navidrome-weekly-3-monthlies` runs every Sunday 03:00 for a year with
  `monthly=3`. Every monthly is sent exactly once, as an incremental on the
  first Sunday after its 1st, and the fulls roll on 2027-03-07 and 2027-09-05
  (the first Sundays after the first 1st past each 4320h window); all other
  runs are `nothing-newer` no-ops. The previous monthly is always still on
  the pool: sanoid prunes a monthly only once it is older than 3 x 31 days
  with more than three present.
- `monthly-only-gap-2-months` and `monthly-only-gap-3-months` bound the
  outage the chain tolerates. With the last backup at the October monthly and
  no run since, a run at 2026-12-28 still finds October on the pool and sends
  December from it (November is skipped, not lost: it is in the December
  incremental's data). A run at 2027-01-10 finds October pruned and sends a
  `source-pruned` FULL of the January monthly, which `full-cadence` flags as
  early (`expect-violations`); the chain then continues from January.
