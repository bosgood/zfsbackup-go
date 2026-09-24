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
			if strings.Contains(buf.String(), "violation") {
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

	testCases := []struct {
		name        string
		jobInfo     files.JobInfo
		snapshots   []files.SnapshotInfo
		destBackups [][]*files.JobInfo
		want        Plan
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
			got, err := planSmartSnapshots(&ji, tc.snapshots, tc.destBackups)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Action != tc.want.Action || got.Reason != tc.want.Reason ||
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

// snapshotsIdentical is SnapshotInfo.Equal plus the bookmark flag.
func snapshotsIdentical(a, b files.SnapshotInfo) bool {
	return a.Equal(&b) && a.Bookmark == b.Bookmark
}
