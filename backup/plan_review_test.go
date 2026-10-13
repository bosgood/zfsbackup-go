package backup

import (
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// Regression tests for the findings 3, 4, 14 and 20 of the 2026-10-12 adversarial review
// (docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md).

var reviewBase = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func reviewDay(n int) time.Time { return reviewBase.Add(time.Duration(n) * 24 * time.Hour) }

func reviewMonthlyDaily() files.JobInfo {
	return files.JobInfo{
		VolumeName: "tank/data", FullIfOlderThan: 720 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_daily",
	}
}

// setPlanClock sets Now for the test.
func setPlanClock(t *testing.T, now time.Time) {
	t.Helper()
	old := Now
	Now = func() time.Time { return now }
	t.Cleanup(func() { Now = old })
}

func hasWarning(p Plan, substrings ...string) bool {
	for _, w := range p.Warnings {
		all := true
		for _, s := range substrings {
			all = all && strings.Contains(w, s)
		}
		if all {
			return true
		}
	}
	return false
}

// Finding 3: the set d2<-m1 was deleted from the middle of the chain. The planner must not
// continue from d3, whose chain no longer reaches a full: no incremental, a warning that names
// the missing link, and a full as soon as a newer full candidate exists.
func TestReviewBrokenChainIsNotContinued(t *testing.T) {
	m1, d2, d3, d4 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2)), snap("d3_daily", reviewDay(3)), snap("d4_daily", reviewDay(4))
	broken := []*files.JobInfo{incrManifest(d3, d2), fullManifest(m1)} // incrManifest(d2, m1) is gone
	now := reviewDay(5)

	t.Run("no newer monthly", func(t *testing.T) {
		ji := reviewMonthlyDaily()
		got, err := planSmartAt(now, &ji, []files.SnapshotInfo{d4, d3, d2, m1}, [][]*files.JobInfo{broken}, false)
		if err != nil || got.Action != PlanNoop || got.Reason != reasonBrokenChain {
			t.Fatalf("got %s, %v; want NOOP %s", got, err, reasonBrokenChain)
		}
		if !hasWarning(got, "broken", "no backup of d2_daily", "d3_daily") {
			t.Errorf("warnings %q do not name the missing link d2_daily", got.Warnings)
		}
	})

	t.Run("newer monthly", func(t *testing.T) {
		m4 := snap("d4_monthly", reviewDay(4).Add(time.Second))
		ji := reviewMonthlyDaily()
		got, err := planSmartAt(now, &ji, []files.SnapshotInfo{m4, d4, d3, d2, m1}, [][]*files.JobInfo{broken}, false)
		if err != nil || got.Action != PlanFull || got.Base.Name != m4.Name || got.Reason != reasonBrokenChain {
			t.Fatalf("got %s, %v; want FULL d4_monthly %s", got, err, reasonBrokenChain)
		}
		if !hasWarning(got, "no backup of d2_daily", "starts a new chain") {
			t.Errorf("warnings %q do not name the missing link d2_daily", got.Warnings)
		}
	})

	t.Run("second destination broken", func(t *testing.T) {
		whole := []*files.JobInfo{incrManifest(d3, d2), incrManifest(d2, m1), fullManifest(m1)}
		ji := reviewMonthlyDaily()
		got, err := planSmartAt(now, &ji, []files.SnapshotInfo{d4, d3, d2, m1}, [][]*files.JobInfo{whole, broken}, false)
		if err != nil || got.Action == PlanIncremental {
			t.Fatalf("got %s, %v; want no incremental", got, err)
		}
		if !hasWarning(got, "destination #2", "no backup of d2_daily") {
			t.Errorf("warnings %q do not name destination #2 and d2_daily", got.Warnings)
		}
	})

	t.Run("window elapsed", func(t *testing.T) {
		m40 := snap("d40_monthly", reviewDay(40))
		ji := reviewMonthlyDaily()
		got, err := planSmartAt(reviewDay(41), &ji, []files.SnapshotInfo{m40, d3, d2, m1}, [][]*files.JobInfo{broken}, false)
		if err != nil || got.Action != PlanFull || got.Reason != reasonWindowElapsed || !hasWarning(got, "no backup of d2_daily") {
			t.Fatalf("got %s, %v, warnings %q; want FULL %s with a warning", got, err, got.Warnings, reasonWindowElapsed)
		}
	})

	t.Run("increment", func(t *testing.T) {
		ji := files.JobInfo{VolumeName: "tank/data", Incremental: true, FullIfOlderThan: -1 * time.Minute}
		got, err := planSmartAt(now, &ji, []files.SnapshotInfo{d4, d3, d2, m1}, [][]*files.JobInfo{broken}, false)
		if err == nil || !strings.Contains(err.Error(), "no backup of d2_daily") {
			t.Fatalf("got %s, %v; want an error that names d2_daily", got, err)
		}
	})

	t.Run("whole chain", func(t *testing.T) {
		whole := []*files.JobInfo{incrManifest(d3, d2), incrManifest(d2, m1), fullManifest(m1)}
		ji := reviewMonthlyDaily()
		got, err := planSmartAt(now, &ji, []files.SnapshotInfo{d4, d3, d2, m1}, [][]*files.JobInfo{whole}, false)
		if err != nil || got.Action != PlanIncremental || got.Base.Name != d4.Name || len(got.Warnings) != 0 {
			t.Fatalf("got %s, %v, warnings %q; want INCR d4_daily and no warning", got, err, got.Warnings)
		}
	})
}

// Finding 14: a snapshot and a bookmark of the same name and time are both on the pool. The
// planner uses the snapshot whatever the listing order.
func TestReviewPrefersSnapshotOverSameNamedBookmark(t *testing.T) {
	m1, d2, d3 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2)), snap("d3_daily", reviewDay(3))
	bm := files.SnapshotInfo{Name: d2.Name, CreationTime: d2.CreationTime, Bookmark: true}
	for _, order := range []string{"bookmark first", "snapshot first"} {
		pool := []files.SnapshotInfo{d3, d2, bm, m1}
		if order == "bookmark first" {
			pool = []files.SnapshotInfo{d3, bm, d2, m1}
		}
		ji := reviewMonthlyDaily()
		ji.Replication = true
		got, err := planSmartAt(reviewDay(4), &ji, pool, [][]*files.JobInfo{{incrManifest(d2, m1), fullManifest(m1)}}, false)
		if err != nil || got.Action != PlanIncremental || got.Source.Name != d2.Name {
			t.Fatalf("%s: got %s, %v; want an incremental from d2_daily", order, got, err)
		}
		if got.Source.Bookmark {
			t.Errorf("%s: the source is a bookmark although the snapshot d2_daily is on the pool", order)
		}

		probe := d2
		probe.Bookmark = true // the result must not depend on what the caller passes in
		if !validateSnapShotExistsFromSnaps(&probe, pool, true) || probe.Bookmark {
			t.Errorf("%s: validateSnapShotExistsFromSnaps flags the snapshot as a bookmark", order)
		}
	}

	// Only the bookmark: found with bookmarks, flagged as one; not found without.
	only := []files.SnapshotInfo{d3, bm, m1}
	probe := d2
	if !validateSnapShotExistsFromSnaps(&probe, only, true) || !probe.Bookmark {
		t.Errorf("a bookmark alone is not found as a bookmark: %+v", probe)
	}
	probe = d2
	if validateSnapShotExistsFromSnaps(&probe, only, false) {
		t.Errorf("a bookmark is found although bookmarks are not included")
	}
}

// Finding 14: zfs send refuses a bookmark source with -R, -p, -D and -I. With one of those
// flags, a last backup left only as a bookmark is a pruned source: a full of a newer full
// candidate, or nothing until one exists, with a warning. Without them the bookmark is used.
func TestReviewBookmarkSourceWithSnapshotOnlyFlags(t *testing.T) {
	m1, d2, d3 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2)), snap("d3_daily", reviewDay(3))
	bm := files.SnapshotInfo{Name: d2.Name, CreationTime: d2.CreationTime, Bookmark: true}
	dest := func() [][]*files.JobInfo { return [][]*files.JobInfo{{incrManifest(d2, m1), fullManifest(m1)}} }
	flags := map[string]func(*files.JobInfo){
		"-R": func(j *files.JobInfo) { j.Replication = true },
		"-p": func(j *files.JobInfo) { j.Properties = true },
		"-D": func(j *files.JobInfo) { j.Deduplication = true },
		"-I": func(j *files.JobInfo) { j.IntermediaryIncremental = true },
	}
	for name, set := range flags {
		ji := reviewMonthlyDaily()
		set(&ji)
		got, err := planSmartAt(reviewDay(4), &ji, []files.SnapshotInfo{d3, bm, m1}, dest(), false)
		if err != nil || got.Action != PlanNoop || got.Reason != reasonSourcePruned {
			t.Errorf("%s: got %s, %v; want NOOP %s", name, got, err, reasonSourcePruned)
		}
		if !hasWarning(got, "Only a bookmark of d2_daily") {
			t.Errorf("%s: warnings %q do not say only a bookmark is left", name, got.Warnings)
		}

		m3 := snap("d3_monthly", reviewDay(3).Add(time.Second))
		got, err = planSmartAt(reviewDay(4), &ji, []files.SnapshotInfo{m3, d3, bm, m1}, dest(), false)
		if err != nil || got.Action != PlanFull || got.Base.Name != m3.Name || got.Reason != reasonSourcePruned {
			t.Errorf("%s with a newer monthly: got %s, %v; want FULL d3_monthly %s", name, got, err, reasonSourcePruned)
		}
	}

	ji := reviewMonthlyDaily()
	got, err := planSmartAt(reviewDay(4), &ji, []files.SnapshotInfo{d3, bm, m1}, dest(), false)
	if err != nil || got.Action != PlanIncremental || !got.Source.Bookmark || len(got.Warnings) != 0 {
		t.Errorf("without those flags: got %s, %v (source %+v); want an incremental from the bookmark #d2_daily", got, err, got.Source)
	}
}

// Finding 4: a snapshot taken while the clock read 2099. It is never sent, a backup of it (from
// an older version) is ignored, an explicit --full starts a chain the next smart run continues,
// and every one of these says so at Warning.
func TestReviewFutureDatedSnapshotIsIgnored(t *testing.T) {
	bad := snap("autosnap_2099-01-01_00:00:00_daily", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	m1, d2 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2))
	ji := reviewMonthlyDaily()

	// Run 1: the bad snapshot is on the pool; the run sends the real daily.
	got, err := planSmartAt(reviewDay(2).Add(time.Hour), &ji, []files.SnapshotInfo{bad, d2, m1}, [][]*files.JobInfo{{fullManifest(m1)}}, false)
	if err != nil || got.Action != PlanIncremental || got.Base.Name != d2.Name {
		t.Fatalf("run 1: got %s, %v; want INCR d2_daily", got, err)
	}
	if !hasWarning(got, "Ignoring tank/data@"+bad.Name, "2099-01-01T00:00:00Z") {
		t.Errorf("run 1: warnings %q do not name the 2099 snapshot", got.Warnings)
	}

	// Run 2, 200 days later: an older version sent the bad snapshot. Its backup is ignored and
	// the window has elapsed.
	dest := []*files.JobInfo{incrManifest(bad, m1), fullManifest(m1)}
	m190, d200 := snap("d190_monthly", reviewDay(190)), snap("d200_daily", reviewDay(200))
	pool := []files.SnapshotInfo{bad, d200, m190, m1}
	now := reviewDay(200).Add(time.Hour)
	got, err = planSmartAt(now, &ji, pool, [][]*files.JobInfo{dest}, false)
	if err != nil || got.Action != PlanFull || got.Base.Name != m190.Name {
		t.Fatalf("run 2: got %s, %v; want FULL d190_monthly", got, err)
	}
	if !hasWarning(got, "Ignoring the backup INCR "+bad.Name) {
		t.Errorf("run 2: warnings %q do not name the 2099 backup", got.Warnings)
	}

	// An explicit --full, then a smart run: the 2099 backup does not take the chain back.
	full := ji
	full.Full, full.FullIfOlderThan = true, -1*time.Minute
	got, err = planSmartAt(now, &full, pool, [][]*files.JobInfo{dest}, false)
	if err != nil || got.Action != PlanFull || got.Base.Name != m190.Name {
		t.Fatalf("--full: got %s, %v; want FULL d190_monthly", got, err)
	}
	dest = append([]*files.JobInfo{fullManifest(m190)}, dest...)
	sortBackupsNewestFirst(dest)
	got, err = planSmartAt(now, &ji, pool, [][]*files.JobInfo{dest}, false)
	if err != nil || got.Action != PlanIncremental || got.Base.Name != d200.Name || got.Source.Name != m190.Name {
		t.Fatalf("smart run after --full: got %s, %v; want INCR d200_daily from d190_monthly", got, err)
	}

	// The bad snapshot destroyed: its backup is still ignored, not an error.
	got, err = planSmartAt(now, &ji, pool[1:], [][]*files.JobInfo{dest}, false)
	if err != nil || got.Action != PlanIncremental || !hasWarning(got, "Ignoring the backup") {
		t.Fatalf("bad snapshot destroyed: got %s, %v, warnings %q", got, err, got.Warnings)
	}

	// Every snapshot in the future (the clock is wrong now): an error, and the warnings.
	got, err = planSmartAt(reviewDay(-1), &ji, []files.SnapshotInfo{d2, m1}, [][]*files.JobInfo{{}}, false)
	if err == nil || len(got.Warnings) != 2 {
		t.Fatalf("clock before every snapshot: got %s, %v, warnings %q; want an error and 2 warnings", got, err, got.Warnings)
	}

	// A day of slack: a snapshot dated 23 hours after now is used.
	soon := snap("d3_daily", reviewDay(3))
	got, err = planSmartAt(reviewDay(3).Add(-23*time.Hour), &ji, []files.SnapshotInfo{soon, d2, m1}, [][]*files.JobInfo{{incrManifest(d2, m1), fullManifest(m1)}}, false)
	if err != nil || got.Action != PlanIncremental || got.Base.Name != soon.Name || len(got.Warnings) != 0 {
		t.Fatalf("23 hours ahead: got %s, %v, warnings %q; want INCR d3_daily", got, err, got.Warnings)
	}
}

// Finding 4: send reads the clock Now, and logs the planner's warnings at Warning.
func TestReviewPlanSmartSnapshotsLogsWarnings(t *testing.T) {
	logs := captureLogs(t)
	setPlanClock(t, reviewDay(2).Add(time.Hour))
	bad := snap("autosnap_2099-01-01_00:00:00_daily", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	m1, d2 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2))
	ji := reviewMonthlyDaily()
	got, err := planSmartSnapshots(&ji, []files.SnapshotInfo{bad, d2, m1}, [][]*files.JobInfo{{fullManifest(m1)}}, false)
	if err != nil || got.Base.Name != d2.Name {
		t.Fatalf("got %s, %v; want INCR d2_daily", got, err)
	}
	if !strings.Contains(logs.String(), "Ignoring tank/data@"+bad.Name) {
		t.Errorf("the warning is not logged:\n%s", logs)
	}
}

// Finding 20: --fullIfOlderThan 720h after a 30-day month. sanoid's monthly lands a few seconds
// before or after the full's second; the full must not depend on those seconds.
func TestReviewFullWindowSlack(t *testing.T) {
	ji := files.JobInfo{
		VolumeName: "tank/data", FullIfOlderThan: 720 * time.Hour, FullSnapshotSuffix: "_monthly", IncrementalSnapshotSuffix: "_monthly",
	}
	apr := snap("autosnap_2026-04-01_00:00:00_monthly", time.Date(2026, 4, 1, 0, 0, 5, 0, time.UTC))
	may := time.Date(2026, 5, 1, 0, 0, 5, 0, time.UTC)
	for _, tc := range []struct {
		offset time.Duration
		want   PlanAction
	}{
		{-11 * time.Minute, PlanIncremental},
		{-9 * time.Minute, PlanFull},
		{-1 * time.Second, PlanFull},
		{0, PlanFull},
		{time.Second, PlanFull},
	} {
		m := snap("autosnap_2026-05-01_00:00:00_monthly", may.Add(tc.offset))
		got, err := planSmartAt(may, &ji, []files.SnapshotInfo{m, apr}, [][]*files.JobInfo{{fullManifest(apr)}}, false)
		if err != nil || got.Action != tc.want {
			t.Errorf("May monthly %v off April's second: got %s, %v; want %s", tc.offset, got, err, tc.want)
		}
	}

	// A window of an hour or less has no slack.
	hourly := files.JobInfo{VolumeName: "tank/data", FullIfOlderThan: time.Hour}
	a := snap("a", reviewDay(1))
	for _, tc := range []struct {
		age  time.Duration
		want PlanAction
	}{{time.Hour - time.Second, PlanIncremental}, {time.Hour, PlanIncremental}, {time.Hour + time.Second, PlanFull}} {
		b := snap("b", reviewDay(1).Add(tc.age))
		got, err := planSmartAt(reviewDay(2), &hourly, []files.SnapshotInfo{b, a}, [][]*files.JobInfo{{fullManifest(a)}}, false)
		if err != nil || got.Action != tc.want {
			t.Errorf("1h window, age %v: got %s, %v; want %s", tc.age, got, err, tc.want)
		}
	}
}

// The simulator prints a run's warnings under it, and keeps them in --json.
func TestReviewSimulationShowsWarnings(t *testing.T) {
	m1, d2, d3, d4 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2)), snap("d3_daily", reviewDay(3)), snap("d4_daily", reviewDay(4))
	sc := &Scenario{
		Volume: "tank/data", Location: time.UTC, JobInfo: reviewMonthlyDaily(), Now: reviewDay(5),
		Snapshots:   []files.SnapshotInfo{d4, d3, d2, m1},
		DestBackups: [][]*files.JobInfo{{incrManifest(d3, d2), fullManifest(m1)}},
	}
	sim := sc.Run()
	var text, js strings.Builder
	if err := sim.WriteText(&text, nil); err != nil {
		t.Fatal(err)
	}
	if err := sim.WriteJSON(&js, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "next  NOOP  broken-chain\n    warning: The chain of tank/data at the destination is broken") {
		t.Errorf("text output:\n%s", text.String())
	}
	if !strings.Contains(js.String(), `"warnings": [`) || !strings.Contains(js.String(), `"name": "d2_daily"`) {
		t.Errorf("JSON output:\n%s", js.String())
	}
	if v := sim.Check(nil); len(v) == 0 {
		t.Errorf("no violation for the broken chain at the destination")
	}
}

// A future-dated backup at the destination, in a scenario loaded from files: LoadScenario
// pins Now to the newest time in the files, so the result does not depend on the day.
func TestReviewLoadScenarioPinsNow(t *testing.T) {
	sc, err := LoadScenario("testdata/scenarios/pruned-source-no-newer-monthly")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2027, 3, 12, 1, 0, 0, 0, sc.location()); !sc.Now.Equal(want) {
		t.Errorf("Now = %v, want the newest snapshot, %v", sc.Now, want)
	}
	if st := sc.Run().Steps[0]; st.Err != nil || len(st.Plan.Warnings) != 0 {
		t.Errorf("got %s, %v, warnings %q", st.Plan, st.Err, st.Plan.Warnings)
	}
}
