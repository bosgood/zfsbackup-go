// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package backup

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// simulate builds a Simulation from hand-written steps, recording their
// backups at the scenario's destinations the way Run does.
func simulate(sc *Scenario, steps ...Step) *Simulation {
	sim := &Simulation{Scenario: sc, Steps: steps, Initial: cloneDestinations(sc.DestBackups)}
	dest := cloneDestinations(sc.DestBackups)
	for _, st := range steps {
		if st.Err == nil && st.Plan.Action != PlanNoop {
			for i := range dest {
				dest[i] = addManifest(dest[i], manifestFor(sc.Volume, st.Plan))
			}
		}
	}
	sim.Manifests = dest
	return sim
}

// nolint:funlen // one passing and one violating case per check
func TestChecks(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n) }
	monthly := func(n int) files.SnapshotInfo { return snap(fmt.Sprintf("autosnap_day%d_monthly", n), day(n)) }
	pool := func(days ...int) []files.SnapshotInfo { // newest-first
		var out []files.SnapshotInfo
		for i := len(days) - 1; i >= 0; i-- {
			out = append(out, monthly(days[i]))
		}
		return out
	}
	full := func(at, base int) Step {
		return Step{At: day(at), Snapshots: pool(base), Plan: Plan{Action: PlanFull, Base: monthly(base)}}
	}
	incr := func(at, base, source int) Step {
		return Step{At: day(at), Snapshots: pool(source, base), Plan: Plan{Action: PlanIncremental, Base: monthly(base), Source: monthly(source)}}
	}
	noop := func(at int, snaps ...int) Step {
		return Step{At: day(at), Snapshots: pool(snaps...), Plan: Plan{Action: PlanNoop}}
	}
	// Monthly-only production flags, daily runs.
	scenario := func(initial ...*files.JobInfo) *Scenario {
		return &Scenario{
			Volume:      "tank/data",
			JobInfo:     files.JobInfo{FullIfOlderThan: 4320 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly"},
			DestBackups: [][]*files.JobInfo{initial},
			Location:    time.UTC,
			From:        day(0),
			Until:       day(1000),
		}
	}
	// A chain of n monthly incrementals after a full, 30 days apart.
	chain := func(n int) []Step {
		steps := []Step{full(0, 0)}
		for i := 1; i <= n; i++ {
			steps = append(steps, incr(30*i, 30*i, 30*(i-1)))
		}
		return steps
	}

	testCases := []struct {
		name   string
		check  string
		sim    *Simulation
		extra  []string
		detail string // substring of the single expected violation; empty for none
	}{
		{
			name:  "no-errors: every run decides",
			check: "no-errors",
			sim:   simulate(scenario(), full(0, 0), noop(1, 0)),
		},
		{
			name:  "no-errors: runs failing the same way are one violation",
			check: "no-errors",
			sim: simulate(scenario(), full(0, 0),
				Step{At: day(1), Err: errors.New("destinations are out of sync")},
				Step{At: day(2), Err: errors.New("destinations are out of sync")}),
			detail: "destinations are out of sync (2 runs, through 2026-09-03T00:00:00Z)",
		},
		{
			name:  "chain-links: incremental with its full",
			check: "chain-links",
			sim:   simulate(scenario(incrManifest(monthly(30), monthly(0)), fullManifest(monthly(0)))),
		},
		{
			name:   "chain-links: incremental whose source was never backed up",
			check:  "chain-links",
			sim:    simulate(scenario(incrManifest(monthly(30), monthly(0)))),
			detail: "INCR autosnap_day30_monthly from autosnap_day0_monthly cannot be restored: no backup of autosnap_day0_monthly",
		},
		{
			name:  "source-present: source still on the pool",
			check: "source-present",
			sim:   simulate(scenario(), full(0, 0), incr(30, 30, 0)),
		},
		{
			name:  "source-present: source pruned",
			check: "source-present",
			sim: simulate(scenario(), full(0, 0),
				Step{At: day(30), Snapshots: pool(30), Plan: Plan{Action: PlanIncremental, Base: monthly(30), Source: monthly(0)}}),
			detail: "autosnap_day0_monthly is not on the pool",
		},
		{
			name:  "no-duplicate-send: every backup once",
			check: "no-duplicate-send",
			sim:   simulate(scenario(), full(0, 0), incr(30, 30, 0)),
		},
		{
			name:   "no-duplicate-send: the same full twice",
			check:  "no-duplicate-send",
			sim:    simulate(scenario(), full(0, 0), full(1, 0)),
			detail: "FULL autosnap_day0_monthly re-sends autosnap_day0_monthly, already backed up by FULL autosnap_day0_monthly sent 2026-09-01T00:00:00Z",
		},
		{
			name:   "no-duplicate-send: a full already at the destination",
			check:  "no-duplicate-send",
			sim:    simulate(scenario(fullManifest(monthly(0))), full(1, 0)),
			detail: "FULL autosnap_day0_monthly re-sends autosnap_day0_monthly, already backed up by FULL autosnap_day0_monthly before the first run",
		},
		{
			name:   "no-duplicate-send: a full of a monthly already sent as an incremental",
			check:  "no-duplicate-send",
			sim:    simulate(scenario(), full(0, 0), incr(30, 30, 0), full(180, 30)),
			detail: "FULL autosnap_day30_monthly re-sends autosnap_day30_monthly, already backed up by INCR autosnap_day30_monthly from autosnap_day0_monthly",
		},
		{
			name:  "no-duplicate-send: an explicit full restarts the chain on purpose",
			check: "no-duplicate-send",
			sim: simulate(scenario(), full(1, 0), incr(31, 30, 0),
				Step{At: day(32), Snapshots: pool(30), Plan: Plan{Action: PlanFull, Base: monthly(30), Reason: reasonExplicitFull}}),
		},
		{
			name:  "no-orphan-full: the next incremental chains from the full",
			check: "no-orphan-full",
			sim:   simulate(scenario(), full(0, 0), noop(1, 0), incr(30, 30, 0)),
		},
		{
			name:   "no-orphan-full: the next incremental continues an older chain",
			check:  "no-orphan-full",
			sim:    simulate(scenario(fullManifest(monthly(0))), full(180, 180), incr(210, 210, 0)),
			detail: "INCR autosnap_day210_monthly from autosnap_day0_monthly skips FULL autosnap_day180_monthly",
		},
		{
			name:  "full-cadence: fulls one window apart",
			check: "full-cadence",
			sim:   simulate(scenario(), full(0, 0), full(181, 181)),
		},
		{
			name:   "full-cadence: a full well inside the window",
			check:  "full-cadence",
			sim:    simulate(scenario(), full(0, 0), full(30, 30)),
			detail: "FULL autosnap_day30_monthly is 30d after FULL autosnap_day0_monthly, want 180d ± 32d",
		},
		{
			name:  "full-cadence: a late full catching up after the initial state",
			check: "full-cadence",
			sim:   simulate(scenario(fullManifest(monthly(0))), full(400, 400)),
		},
		{
			name:  "full-cadence: nothing to judge without a snapshot period",
			check: "full-cadence",
			sim: simulate(&Scenario{JobInfo: files.JobInfo{FullIfOlderThan: 720 * time.Hour}, DestBackups: [][]*files.JobInfo{{fullManifest(monthly(0))}}},
				full(0, 1)),
		},
		{
			name:   "full-cadence: a full overdue",
			check:  "full-cadence",
			sim:    simulate(scenario(), full(0, 0), noop(200, 0, 200), noop(250, 0, 250), noop(260, 0, 260)),
			detail: "no full since autosnap_day0_monthly, 250d ago",
		},
		{
			name:  "restore-depth: chains reset within a window",
			check: "restore-depth",
			sim:   simulate(scenario(), chain(6)...),
		},
		{
			name:   "restore-depth: a chain that keeps growing",
			check:  "restore-depth",
			sim:    simulate(scenario(), chain(12)...),
			detail: "restoring autosnap_day300_monthly takes 10 incrementals, want at most 9",
		},
		{
			name:  "coverage: every monthly from the first run on backed up once",
			check: "coverage:_monthly",
			extra: []string{"coverage:_monthly"},
			sim:   simulate(scenario(), full(1, 0), incr(31, 30, 0)),
		},
		{
			name:   "coverage: a monthly never backed up",
			check:  "coverage:_monthly",
			extra:  []string{"coverage:_monthly"},
			sim:    simulate(scenario(), full(1, 0), noop(31, 0, 30)),
			detail: "autosnap_day30_monthly is never backed up",
		},
		{
			name:   "coverage: a monthly backed up twice",
			check:  "coverage:_monthly",
			extra:  []string{"coverage:_monthly"},
			sim:    simulate(scenario(), full(1, 0), incr(31, 30, 0), full(32, 30)),
			detail: "autosnap_day30_monthly is the base of 2 backups",
		},
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var got []Violation
			for _, v := range tc.sim.Check(tc.extra) {
				if v.Check == tc.check {
					got = append(got, v)
				}
			}
			switch {
			case tc.detail == "" && len(got) != 0:
				t.Errorf("got violations %+v, want none", got)
			case tc.detail != "" && (len(got) != 1 || !strings.Contains(got[0].Detail, tc.detail)):
				t.Errorf("got violations %+v, want one containing %q", got, tc.detail)
			}
		})
	}
}

func TestWriteTextViolations(t *testing.T) {
	sc := &Scenario{Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Location: time.UTC}
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	sim := &Simulation{Scenario: sc, Steps: []Step{{At: at, Plan: Plan{Action: PlanNoop, Reason: reasonNothingNewer}}}}
	var buf bytes.Buffer
	if err := sim.WriteText(&buf, []Violation{
		{Check: "no-duplicate-send", At: at, Detail: "FULL x again"},
		{Check: "coverage:_monthly", Detail: "y is never backed up"},
	}); err != nil {
		t.Fatal(err)
	}
	want := "2026-10-01T01:00:00Z  NOOP  nothing-newer\n" +
		"checks: 2 violation(s)\n" +
		"  no-duplicate-send  2026-10-01T01:00:00Z  FULL x again\n" +
		"  coverage:_monthly  y is never backed up\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// A monthly taken and pruned while the host was down was never seen by any
// run; coverage must still report that it was never backed up.
func TestCoverageNamesMonthlyPrunedDuringOutage(t *testing.T) {
	sc := skipScenario(t, "policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2027-04-02T01:00:00Z,"+
		"skip=2026-11-03T01:00:00Z..2027-03-05T01:00:00Z,checks=coverage:_monthly")
	sim := sc.Run()
	var never []string
	for _, v := range sim.Check(sc.Checks) {
		if v.Check == "coverage:_monthly" && strings.Contains(v.Detail, "is never backed up") {
			never = append(never, v.Detail)
		}
	}
	joined := strings.Join(never, "\n")
	for _, month := range []string{"2026-12-01", "2027-01-01", "2027-02-01"} {
		if !strings.Contains(joined, "autosnap_"+month+"_00:00:00_monthly") {
			t.Errorf("coverage does not name the %s monthly, which no run ever sent; got:\n%s", month, joined)
		}
	}
	if len(never) != 3 {
		t.Errorf("got %d never-backed-up violations, want 3:\n%s", len(never), joined)
	}
}

func TestFormatDays(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{29*24*time.Hour + 23*time.Hour + 30*time.Minute, "30d"},
		{29*24*time.Hour + 23*time.Hour + 29*time.Minute, "29d23h"},
		{30 * 24 * time.Hour, "30d"},
		{-(2*24*time.Hour + 5*time.Hour), "-2d5h"},
		{0, "0d"},
	} {
		if got := formatDays(tc.d); got != tc.want {
			t.Errorf("formatDays(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
