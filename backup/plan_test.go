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
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

var update = flag.Bool("update", false, "rewrite expected.txt for every scenario")

// TestScenarios runs every scenario under testdata/scenarios and compares the
// rendered plan with its expected.txt. `make scenarios-update` rewrites them.
func TestScenarios(t *testing.T) {
	entries, err := ioutil.ReadDir("testdata/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join("testdata/scenarios", entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			sc, err := LoadScenario(dir)
			if err != nil {
				t.Fatalf("LoadScenario: %v", err)
			}
			sim := sc.Run()
			var buf bytes.Buffer
			if err = sim.WriteText(&buf, sim.Check(sc.Checks)); err != nil {
				t.Fatal(err)
			}

			golden := filepath.Join(dir, "expected.txt")
			if *update {
				if err = ioutil.WriteFile(golden, buf.Bytes(), 0644); err != nil { // nolint:gosec // not secret
					t.Fatal(err)
				}
			}
			want, err := ioutil.ReadFile(golden)
			if os.IsNotExist(err) {
				t.Fatalf("%s is missing; create it with `make scenarios-update`:\n%s", golden, buf.String())
			} else if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want, buf.Bytes()) {
				t.Errorf("plan differs from %s (- expected, + got):\n%s", golden, lineDiff(string(want), buf.String()))
			}
			if len(sim.Steps) == 0 {
				t.Errorf("the scenario has no runs")
			}
			// A scenario may show a failure on purpose; it says so with an
			// expect-violations file, so that `make scenarios-update` cannot
			// quietly bless a violation in any other scenario.
			if _, err = os.Stat(filepath.Join(dir, "expect-violations")); os.IsNotExist(err) &&
				strings.Contains(buf.String(), "violation") {
				t.Errorf("checks failed:\n%s", buf.String())
			}
		})
	}
}

// lineDiff lists the lines only in want ("- ") or only in got ("+ "), in order.
func lineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	// common[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	common := make([][]int, len(a)+1)
	for i := range common {
		common[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				common[i][j] = common[i+1][j+1] + 1
			case common[i+1][j] >= common[i][j+1]:
				common[i][j] = common[i+1][j]
			default:
				common[i][j] = common[i][j+1]
			}
		}
	}
	var out strings.Builder
	for i, j := 0, 0; i < len(a) || j < len(b); {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || common[i][j+1] >= common[i+1][j]):
			out.WriteString("+ " + b[j] + "\n")
			j++
		default:
			out.WriteString("- " + a[i] + "\n")
			i++
		}
	}
	return out.String()
}

// nolint:funlen // table-driven test
func TestPlanSmartSnapshotsReasons(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }
	const window = 720 * time.Hour // 30 days
	const off = -1 * time.Minute   // "unset" fullIfOlderThan
	monthlyDaily := files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily"}
	monthlyOnly := files.JobInfo{FullIfOlderThan: window, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly"}
	// Destinations that share a full but disagree on the last incremental.
	ahead := []*files.JobInfo{
		incrManifest(snap("d60_monthly", day(60)), snap("d30_monthly", day(30))),
		incrManifest(snap("d30_monthly", day(30)), snap("d1_monthly", day(1))),
		fullManifest(snap("d1_monthly", day(1))),
	}
	behind := []*files.JobInfo{
		incrManifest(snap("d30_monthly", day(30)), snap("d1_monthly", day(1))),
		fullManifest(snap("d1_monthly", day(1))),
	}

	testCases := []struct {
		name        string
		jobInfo     files.JobInfo
		snapshots   []files.SnapshotInfo
		destBackups [][]*files.JobInfo
		completable bool // the lagging destination holds the partial set's volumes
		want        Plan
		wantErr     string
	}{
		{
			name:        "no previous full",
			jobInfo:     monthlyDaily,
			snapshots:   []files.SnapshotInfo{snap("d2_daily", day(2)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{{}},
			want:        Plan{Action: PlanFull, Base: snap("d1_monthly", day(1)), Reason: "no-previous-full"},
		},
		{
			name:    "window elapsed",
			jobInfo: monthlyDaily,
			snapshots: []files.SnapshotInfo{
				snap("d32_daily", day(32)), snap("d32_monthly", day(32)), snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("d1_monthly", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("d32_monthly", day(32)), Reason: "window-elapsed"},
		},
		{
			name:    "window elapsed, but the newest candidate was the last backup",
			jobInfo: monthlyOnly,
			snapshots: []files.SnapshotInfo{
				snap("d40_hourly", day(40)), snap("d31_monthly", day(31)), snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d31_monthly", day(31)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{Action: PlanNoop, Reason: "nothing-newer", FullDue: true},
		},
		{
			name:    "window elapsed, but the newest candidate predates the last backup",
			jobInfo: monthlyDaily,
			snapshots: []files.SnapshotInfo{
				snap("d40_daily", day(40)), snap("d39_daily", day(39)), snap("d31_monthly", day(31)), snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d39_daily", day(39)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{
				Action: PlanIncremental, Base: snap("d40_daily", day(40)), Source: snap("d39_daily", day(39)),
				Reason: "newer-candidate", FullDue: true,
			},
		},
		{
			name:        "diverged destinations: the one behind decides",
			jobInfo:     monthlyOnly,
			snapshots:   []files.SnapshotInfo{snap("d60_monthly", day(60)), snap("d30_monthly", day(30)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{ahead, behind},
			want:        Plan{Action: PlanFull, Base: snap("d60_monthly", day(60)), Reason: "window-elapsed"},
		},
		{
			name:        "diverged destinations in the other order",
			jobInfo:     monthlyOnly,
			snapshots:   []files.SnapshotInfo{snap("d60_monthly", day(60)), snap("d30_monthly", day(30)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{behind, ahead},
			want:        Plan{Action: PlanFull, Base: snap("d60_monthly", day(60)), Reason: "window-elapsed"},
		},
		{
			name:    "source pruned",
			jobInfo: monthlyDaily,
			snapshots: []files.SnapshotInfo{
				snap("d10_monthly", day(10)), snap("d9_daily", day(9)), snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d5_daily", day(5)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{Action: PlanFull, Base: snap("d10_monthly", day(10)), Source: snap("d5_daily", day(5)), Reason: "source-pruned"},
		},
		{
			name:    "source pruned and no newer full candidate",
			jobInfo: monthlyDaily,
			snapshots: []files.SnapshotInfo{
				snap("d12_daily", day(12)), snap("d6_daily", day(6)), snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d2_daily", day(2)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{Action: PlanNoop, Source: snap("d2_daily", day(2)), Reason: "source-pruned"},
		},
		{
			name:        "newer candidate",
			jobInfo:     monthlyDaily,
			snapshots:   []files.SnapshotInfo{snap("d2_daily", day(2)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("d1_monthly", day(1)))}},
			want: Plan{
				Action: PlanIncremental, Base: snap("d2_daily", day(2)), Source: snap("d1_monthly", day(1)), Reason: "newer-candidate",
			},
		},
		{
			name:    "incremental source only left as a bookmark",
			jobInfo: monthlyDaily,
			snapshots: []files.SnapshotInfo{
				snap("d3_daily", day(3)), {Name: "d2_daily", CreationTime: day(2), Bookmark: true}, snap("d1_monthly", day(1)),
			},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d2_daily", day(2)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{
				Action: PlanIncremental, Base: snap("d3_daily", day(3)),
				Source: files.SnapshotInfo{Name: "d2_daily", CreationTime: day(2), Bookmark: true}, Reason: "newer-candidate",
			},
		},
		{
			name:      "nothing newer",
			jobInfo:   monthlyDaily,
			snapshots: []files.SnapshotInfo{snap("d2_daily", day(2)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{{
				incrManifest(snap("d2_daily", day(2)), snap("d1_monthly", day(1))),
				fullManifest(snap("d1_monthly", day(1))),
			}},
			want: Plan{Action: PlanNoop, Reason: "nothing-newer"},
		},
		{
			name:        "explicit full",
			jobInfo:     files.JobInfo{Full: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("s2", day(2)), Reason: "explicit-full"},
		},
		{
			name:      "explicit full already at every destination",
			jobInfo:   files.JobInfo{Full: true, FullIfOlderThan: window, FullSnapshotSuffix: "_monthly"},
			snapshots: []files.SnapshotInfo{snap("d2_daily", day(2)), snap("d1_monthly", day(1))},
			destBackups: [][]*files.JobInfo{
				{fullManifest(snap("d1_monthly", day(1)))},
				{incrManifest(snap("d2_daily", day(2)), snap("d1_monthly", day(1))), fullManifest(snap("d1_monthly", day(1)))},
			},
			want: Plan{Action: PlanNoop, Reason: "already-backed-up"},
		},
		{
			// Only an incremental TO the candidate exists: a full of it is still new.
			name:        "explicit full, candidate only backed up as an incremental",
			jobInfo:     files.JobInfo{Full: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{incrManifest(snap("s2", day(2)), snap("s1", day(1))), fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("s2", day(2)), Reason: "explicit-full"},
		},
		{
			name:        "explicit full at no destination",
			jobInfo:     files.JobInfo{Full: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{}, {fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("s2", day(2)), Reason: "explicit-full"},
		},
		{
			name:        "explicit full already at one destination only",
			jobInfo:     files.JobInfo{Full: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s2", day(2)))}, {}},
			wantErr:     "destinations are out of sync: destination #1 already has a full of s2, destination #2 does not; run again with --resume to complete it there",
		},
		{
			name:        "explicit full already at one destination only, resuming",
			jobInfo:     files.JobInfo{Full: true, Resume: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s2", day(2)))}, {}},
			want:        Plan{Action: PlanFull, Base: snap("s2", day(2)), Reason: "explicit-full"},
		},
		{
			name:        "smart, full missing at one destination, resuming",
			jobInfo:     files.JobInfo{Resume: true, FullIfOlderThan: 720 * time.Hour},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{}, {fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("s1", day(1)), Reason: "complete-partial"},
		},
		{
			name:      "smart, incremental missing at one destination, resuming",
			jobInfo:   files.JobInfo{Resume: true, FullIfOlderThan: 720 * time.Hour},
			snapshots: []files.SnapshotInfo{snap("s3", day(3)), snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{
				{incrManifest(snap("s2", day(2)), snap("s1", day(1))), fullManifest(snap("s1", day(1)))},
				{fullManifest(snap("s1", day(1)))},
			},
			want: Plan{Action: PlanIncremental, Base: snap("s2", day(2)), Source: snap("s1", day(1)), Reason: "complete-partial"},
		},
		{
			// Completing copies a manifest and sends nothing, so it needs no --resume (Decision 4),
			// once the lagging destination is known to hold the set's volumes.
			name:        "smart, full missing at one destination, its volumes there",
			jobInfo:     files.JobInfo{FullIfOlderThan: 720 * time.Hour},
			completable: true,
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{}, {fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanFull, Base: snap("s1", day(1)), Reason: "complete-partial"},
		},
		{
			name:        "smart, full missing at one destination, not resuming",
			jobInfo:     files.JobInfo{FullIfOlderThan: 720 * time.Hour},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{}, {fullManifest(snap("s1", day(1)))}},
			wantErr: "destinations are out of sync, cannot continue with smart option: destination #1's last full backup is none, " +
				"destination #2's is s1",
		},
		{
			// Not one set behind: two different fulls. The error says which destination holds what.
			name:        "smart, destinations hold different fulls",
			jobInfo:     files.JobInfo{FullIfOlderThan: 720 * time.Hour},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s2", day(2)))}, {fullManifest(snap("s1", day(1)))}},
			wantErr: "destinations are out of sync, cannot continue with smart option: destination #1's last full backup is s2, " +
				"destination #2's is s1",
		},
		{
			name:        "smart, in sync, resuming",
			jobInfo:     files.JobInfo{Resume: true, FullIfOlderThan: 720 * time.Hour},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}, {fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanIncremental, Base: snap("s2", day(2)), Source: snap("s1", day(1)), Reason: "newer-candidate"},
		},
		{
			name:        "explicit incremental",
			jobInfo:     files.JobInfo{Incremental: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s2", day(2)), snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanIncremental, Base: snap("s2", day(2)), Source: snap("s1", day(1)), Reason: "explicit-incremental"},
		},
		{
			name:        "explicit incremental with nothing newer",
			jobInfo:     files.JobInfo{Incremental: true, FullIfOlderThan: off},
			snapshots:   []files.SnapshotInfo{snap("s1", day(1))},
			destBackups: [][]*files.JobInfo{{fullManifest(snap("s1", day(1)))}},
			want:        Plan{Action: PlanNoop, Reason: "nothing-newer"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ji := tc.jobInfo
			got, err := planSmartSnapshots(&ji, tc.snapshots, tc.destBackups, ji.Resume || tc.completable)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("got %+v, %v; want error %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Action != tc.want.Action || got.Reason != tc.want.Reason || got.FullDue != tc.want.FullDue ||
				!snapshotsIdentical(got.Base, tc.want.Base) || !snapshotsIdentical(got.Source, tc.want.Source) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if ji.BaseSnapshot != (files.SnapshotInfo{}) || ji.IncrementalSnapshot != (files.SnapshotInfo{}) {
				t.Errorf("planSmartSnapshots set snapshots on jobInfo: %+v", ji)
			}
			for _, dest := range tc.destBackups {
				for _, m := range dest {
					if m.BaseSnapshot.Bookmark || m.IncrementalSnapshot.Bookmark {
						t.Errorf("planSmartSnapshots modified destination manifest %+v", m)
					}
				}
			}
		})
	}
}

// A run cannot see snapshots created after it, even when a simulation starts
// before the newest snapshot it was given.
func TestRunSeesOnlyPastSnapshots(t *testing.T) {
	aug1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sc := &Scenario{
		Volume:      "tank/data",
		JobInfo:     files.JobInfo{FullIfOlderThan: 4320 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly"},
		Snapshots:   []files.SnapshotInfo{snap("autosnap_2026-09-01_00:00:00_monthly", sep1), snap("autosnap_2026-08-01_00:00:00_monthly", aug1)},
		DestBackups: [][]*files.JobInfo{{}},
		From:        aug1.AddDate(0, 0, 14),
		Until:       sep1.AddDate(0, 0, 1),
	}
	sim := sc.Run()
	if first := sim.Steps[0]; len(first.Snapshots) != 1 || first.Plan.Base.Name != "autosnap_2026-08-01_00:00:00_monthly" {
		t.Errorf("first run saw %v and planned %v, want only the August monthly", first.Snapshots, first.Plan)
	}
	for _, st := range sim.Steps[1:] {
		if st.Plan.Action == PlanNoop {
			continue
		}
		if !st.At.Equal(sep1) || st.Plan.Action != PlanIncremental || st.Plan.Base.Name != "autosnap_2026-09-01_00:00:00_monthly" {
			t.Errorf("%v planned %v, want only the September monthly, from August's, on September 1st", st.At, st.Plan)
		}
	}
}

// snapshotsIdentical is SnapshotInfo.Equal plus the bookmark flag.
func snapshotsIdentical(a, b files.SnapshotInfo) bool {
	return a.Equal(&b) && a.Bookmark == b.Bookmark
}

// TestRunSkipsOutages: no run happens inside a skip= range, inclusive, while
// the pool keeps changing.
func TestRunSkipsOutages(t *testing.T) {
	sc, err := LoadScenario("testdata/scenarios/monthly-only-5-months")
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	// Until is exclusive; through the run at `to` is what parseSkip makes of a timed end.
	sc.Skips = []TimeRange{{From: from, Until: to.Add(time.Nanosecond)}}
	steps := sc.Run().Steps
	for _, st := range steps {
		if !st.At.Before(from) && !st.At.After(to) {
			t.Errorf("a run happened at %v, inside the outage", st.At)
		}
	}
	if len(steps) == 0 || !steps[0].At.Equal(sc.From) || steps[len(steps)-1].At.Before(to) {
		t.Fatalf("runs outside the outage are missing: first %v, last %v", steps[0].At, steps[len(steps)-1].At)
	}
	// The first run back sees the snapshots taken during the outage.
	var back Step
	for _, st := range steps {
		if st.At.After(to) {
			back = st
			break
		}
	}
	if want := "autosnap_2026-10-21_00:00:00_daily"; len(back.Snapshots) == 0 || !strings.Contains(fmt.Sprint(snapshotNames(back.Snapshots)), want) {
		t.Errorf("the run at %v does not see %s: %v", back.At, want, snapshotNames(back.Snapshots))
	}
}

// TestTieOrderIndependence reruns scenarios with every order of the snapshots
// sanoid takes at the same boundary (the 1st at midnight brings a monthly, a
// daily and an hourly), in the same second and 1s apart. `zfs list -S
// creation` guarantees neither, so every run must plan the same kind of
// backup either way, and the checks must agree.
func TestTieOrderIndependence(t *testing.T) {
	for _, name := range []string{"monthly-only-5-months", "monthly-daily-5-months", "monthly-only-year", "monthly-daily-year", "monthly-only-3-years"} {
		t.Run(name, func(t *testing.T) {
			var want, wantVariant string
			pools := make(map[string]bool) // distinct pools seen, to prove the variants differ
			base, err := LoadScenario(filepath.Join("testdata/scenarios", name))
			if err != nil {
				t.Fatalf("LoadScenario: %v", err)
			}
			var periods []string // every period the scenario schedules shares a boundary on January 1st
			for _, p := range base.Schedule.Periods {
				periods = append(periods, p.Name)
			}
			for _, order := range permutations(periods) {
				for _, gap := range []time.Duration{0, time.Second} {
					sc, err := LoadScenario(filepath.Join("testdata/scenarios", name))
					if err != nil {
						t.Fatalf("LoadScenario: %v", err)
					}
					sc.Schedule.tieOrder, sc.Schedule.tieGap = order, gap
					sim := sc.Run()
					pools[fmt.Sprint(sim.Steps[len(sim.Steps)-1].Snapshots)] = true
					got := tieIndependentSummary(sim, sim.Check(sc.Checks))
					variant := fmt.Sprintf("order %v, %v apart", order, gap)
					if want == "" {
						want, wantVariant = got, variant
					} else if got != want {
						t.Errorf("%s plans differently from %s (- %[2]s, + %[1]s):\n%s", variant, wantVariant, lineDiff(want, got))
					}
				}
			}
			if len(pools) < 2 {
				t.Errorf("every variant produced the same pool; the tie order hooks had no effect")
			}
		})
	}
}

// tieIndependentSummary renders every run and violation, naming snapshots by
// period only: with coincident snapshots spaced apart their names change.
func tieIndependentSummary(sim *Simulation, violations []Violation) string {
	period := func(s files.SnapshotInfo) string {
		if s.Name == "" {
			return "-"
		}
		return s.Name[strings.LastIndex(s.Name, "_")+1:]
	}
	var b strings.Builder
	for _, st := range sim.Steps {
		fmt.Fprintf(&b, "%s %s %s %s %s %v\n", sim.formatTime(st.At), st.Plan.Action, st.Plan.Reason, period(st.Plan.Base), period(st.Plan.Source), st.Err)
	}
	for _, v := range violations {
		fmt.Fprintf(&b, "violation %s %s\n", v.Check, sim.formatTime(v.At))
	}
	return b.String()
}

func permutations(items []string) [][]string {
	if len(items) <= 1 {
		return [][]string{append([]string(nil), items...)}
	}
	var out [][]string
	for i := range items {
		rest := append(append([]string(nil), items[:i]...), items[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{items[i]}, p...))
		}
	}
	return out
}
