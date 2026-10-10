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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/zfs"
)

// coverageDetails lists the details of the coverage: violations of sim.
func coverageDetails(sim *Simulation, checks []string) []string {
	var out []string
	for _, v := range sim.Check(checks) {
		if strings.HasPrefix(v.Check, "coverage:") {
			out = append(out, v.Detail)
		}
	}
	return out
}

// An outage from the first scheduled run on makes no step for that run; the
// monthlies taken and pruned in it are still never backed up.
func TestCoverageOutageFromFirstRun(t *testing.T) {
	sc := skipScenario(t, "policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2027-04-02T01:00:00Z,"+
		"skip=2026-09-24..2027-03-04,checks=coverage:_monthly")
	sim := sc.Run()
	never := strings.Join(coverageDetails(sim, sc.Checks), "\n")
	for _, month := range []string{"2026-10", "2026-11", "2026-12"} {
		if !strings.Contains(never, "autosnap_"+month+"-01_00:00:00_monthly is never backed up") {
			t.Errorf("coverage does not name the %s monthly, taken and pruned during the outage; got:\n%s", month, never)
		}
	}
}

// adoptionScenario is a names-only capture of a monthly-only pool whose
// destinations hold a backup of the October monthly, dated as given.
func adoptionScenario(t *testing.T, dated ...time.Time) *Scenario {
	t.Helper()
	sc := &Scenario{Volume: "tank/data", Location: time.UTC, JobInfo: files.JobInfo{
		FullIfOlderThan: 4320 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly",
	}}
	listing := "tank/data@autosnap_2026-10-08_00:00:00_daily\n" +
		"tank/data@autosnap_2026-10-01_00:00:00_monthly\n" +
		"tank/data@autosnap_2026-09-01_00:00:00_monthly\n"
	if err := sc.ReadSnapshots(strings.NewReader(listing)); err != nil {
		t.Fatal(err)
	}
	for _, at := range dated {
		sc.DestBackups = append(sc.DestBackups, []*files.JobInfo{{
			VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "autosnap_2026-10-01_00:00:00_monthly", CreationTime: at},
		}})
	}
	return sc
}

// A manifest time that cannot be the snapshot's own (a 2099 backup of a 2026
// name: another host or a forged manifest) is not adopted. plan then fails
// the way send does against the live pool, instead of saying OK.
func TestAdoptionRefusesImplausibleManifestTime(t *testing.T) {
	sc := adoptionScenario(t, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	if got := sc.AdoptCreationTimes(); got != 0 {
		t.Errorf("AdoptCreationTimes = %d, want 0 for a manifest dated 2099", got)
	}
	if len(sc.AdoptionWarnings) != 1 || !strings.Contains(sc.AdoptionWarnings[0], "autosnap_2026-10-01_00:00:00_monthly") {
		t.Errorf("AdoptionWarnings = %q, want one naming the October monthly", sc.AdoptionWarnings)
	}
	sim := sc.Run()
	if err := sim.Steps[0].Err; !errors.Is(err, ErrLastBackupInFuture) {
		t.Errorf("plan from a names-only capture: %v, want ErrLastBackupInFuture as send reports", err)
	}
}

// Destinations that record different times for one name are not adopted from,
// whatever their order, and plan says so.
func TestAdoptionDisagreeingDestinations(t *testing.T) {
	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a, b := oct1.Add(5*time.Second), oct1.Add(9*time.Second)
	for _, order := range [][]time.Time{{a, b}, {b, a}} {
		sc := adoptionScenario(t, order...)
		if got := sc.AdoptCreationTimes(); got != 0 {
			t.Errorf("destinations %v: AdoptCreationTimes = %d, want 0", order, got)
		}
		if got := sc.Snapshots[1]; got.Name != "autosnap_2026-10-01_00:00:00_monthly" || !got.CreationTime.Equal(oct1) {
			t.Errorf("destinations %v: snapshot = %+v, want its name time %v", order, got, oct1)
		}
		if len(sc.AdoptionWarnings) != 1 || !strings.Contains(sc.AdoptionWarnings[0], "disagree") {
			t.Errorf("destinations %v: AdoptionWarnings = %q, want one saying they disagree", order, sc.AdoptionWarnings)
		}
	}
	// Agreeing destinations are adopted from.
	sc := adoptionScenario(t, a, a)
	if got := sc.AdoptCreationTimes(); got != 1 || len(sc.AdoptionWarnings) != 0 {
		t.Errorf("agreeing destinations: adopted %d (warnings %q), want 1 and none", got, sc.AdoptionWarnings)
	}
}

// from= earlier than the destination's last backup: the runs before that
// backup's snapshot see it (the destination proves it exists) and send
// nothing, instead of failing as if the backup came from the future.
func TestFromBeforeLastBackupIsNotAnError(t *testing.T) {
	sc := &Scenario{Volume: "tank/data", Location: time.UTC, JobInfo: files.JobInfo{
		FullIfOlderThan: 4320 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly",
	}}
	if err := sc.ParseScheduleSpec("from=2026-09-20T01:00:00Z,until=2026-10-03T01:00:00Z,every=24h"); err != nil {
		t.Fatal(err)
	}
	if err := sc.ReadSnapshots(strings.NewReader("autosnap_2026-10-01_00:00:00_monthly\nautosnap_2026-09-01_00:00:00_monthly\n")); err != nil {
		t.Fatal(err)
	}
	sc.DestBackups = [][]*files.JobInfo{{
		{VolumeName: "tank/data", BaseSnapshot: sc.Snapshots[0], IncrementalSnapshot: sc.Snapshots[1]},
		{VolumeName: "tank/data", BaseSnapshot: sc.Snapshots[1]},
	}}
	for _, st := range sc.Run().Steps {
		if st.Err != nil || st.Plan.Action != PlanNoop {
			t.Errorf("%v: %v %v, want a no-op", st.At, st.Plan, st.Err)
		}
	}
}

// The error names a pool or capture older than the destination as a cause.
func TestLastBackupInFutureNamesStalePool(t *testing.T) {
	pool := []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}}
	last := &files.SnapshotInfo{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	err := checkLastBackupsOnPool("tank/data", pool, []*files.SnapshotInfo{last})
	if err == nil || !strings.Contains(err.Error(), "is the pool or capture older than the destination") {
		t.Errorf("error %v does not name a stale pool or capture", err)
	}
}

// A daily run in the spring-forward gap (02:30 in New York on 2027-03-14)
// runs once the clock has jumped, and at 02:30 again on the days after.
func TestDailyRunKeepsWallClockAcrossSpringForward(t *testing.T) {
	sc := &Scenario{Volume: "tank/data"}
	if err := sc.ParseScheduleSpec("location=America/New_York,from=2027-03-12T02:30:00,until=2027-03-20T04:00:00,every=24h"); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for at := sc.firstRun(); !at.After(sc.Until); at = sc.nextRun(at) {
		runs = append(runs, at.In(sc.location()).Format("2006-01-02T15:04 MST"))
	}
	want := []string{
		"2027-03-12T02:30 EST", "2027-03-13T02:30 EST", "2027-03-14T03:30 EDT", "2027-03-15T02:30 EDT",
		"2027-03-16T02:30 EDT", "2027-03-17T02:30 EDT", "2027-03-18T02:30 EDT", "2027-03-19T02:30 EDT", "2027-03-20T02:30 EDT",
	}
	if strings.Join(runs, "\n") != strings.Join(want, "\n") {
		t.Errorf("runs:\n%s\nwant:\n%s", strings.Join(runs, "\n"), strings.Join(want, "\n"))
	}
}

// A time in the spring-forward gap given to from= is read as the instant the
// clock jumps past it, as cron runs it, not an hour early.
func TestScheduleTimeInSpringForwardGap(t *testing.T) {
	sc := &Scenario{}
	if err := sc.ParseScheduleSpec("location=America/New_York,from=2027-03-14T02:30:00,until=2027-03-15"); err != nil {
		t.Fatal(err)
	}
	if got, want := sc.From.UTC(), time.Date(2027, 3, 14, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("from= = %v, want %v (03:30 EDT)", got, want)
	}
}

// When the clocks fall back, the hour from 01:00 repeats. Sanoid names
// snapshots in local time, so the second 01:00 hourly would have the name of
// the first; ZFS refuses it. The simulator takes it once, at the first 01:00,
// which is also the instant the name reads as.
func TestHourlyNamesUniqueAcrossFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSchedule("hourly=48")
	if err != nil {
		t.Fatal(err)
	}
	s.Location = ny
	pool, took := s.Advance(nil, time.Date(2026, 11, 1, 0, 30, 0, 0, ny), time.Date(2026, 11, 1, 3, 30, 0, 0, ny))
	for _, list := range [][]files.SnapshotInfo{pool, took} {
		seen := make(map[string]bool)
		for _, sn := range list {
			if seen[sn.Name] {
				t.Errorf("two snapshots named %s", sn.Name)
			}
			seen[sn.Name] = true
		}
	}
	const name = "autosnap_2026-11-01_01:00:00_hourly"
	first := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC) // 01:00 EDT
	for _, sn := range took {
		if sn.Name == name && !sn.CreationTime.Equal(first) {
			t.Errorf("%s created at %v, want the first 01:00, %v", name, sn.CreationTime.UTC(), first)
		}
	}
	if got, ok := zfs.SnapshotNameTime(name, ny); !ok || !got.Equal(first) {
		t.Errorf("SnapshotNameTime(%s) = %v, %v; want %v", name, got.UTC(), ok, first)
	}
	// Advancing again over the repeated hour does not take it either.
	pool2, took2 := s.Advance(pool, time.Date(2026, 11, 1, 1, 30, 0, 0, ny), time.Date(2026, 11, 1, 1, 45, 0, 0, ny).Add(time.Hour))
	for _, sn := range took2 {
		if sn.Name == name {
			t.Errorf("a later Advance took %s again at %v", name, sn.CreationTime.UTC())
		}
	}
	_ = pool2
}
