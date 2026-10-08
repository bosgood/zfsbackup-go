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
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// autosnap returns the snapshot sanoid takes for period at t (UTC names).
func autosnap(period string, t time.Time) files.SnapshotInfo {
	return files.SnapshotInfo{Name: fmt.Sprintf("autosnap_%s_%s", t.UTC().Format("2006-01-02_15:04:05"), period), CreationTime: t}
}

// hourlies returns n hourly snapshots ending at end, newest-first.
func hourlies(end time.Time, n int) []files.SnapshotInfo {
	out := make([]files.SnapshotInfo, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, autosnap("hourly", end.Add(-time.Duration(i)*time.Hour)))
	}
	return out
}

func snapshotNames(snaps []files.SnapshotInfo) []string {
	names := make([]string, 0, len(snaps))
	for _, s := range snaps {
		names = append(names, s.Name)
	}
	return names
}

func mustSchedule(t *testing.T, policy string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(policy)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", policy, err)
	}
	return s
}

func TestParseSchedule(t *testing.T) {
	s, err := ParseSchedule("hourly=36, monthly=3,daily=30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := s.String(), "monthly=3,daily=30,hourly=36"; got != want {
		t.Errorf("String() = %q, want %q (longest period first)", got, want)
	}
	for _, bad := range []string{"", "hourly", "fortnightly=2", "daily=-1", "daily=x", "daily=1,daily=2"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q) succeeded, want an error", bad)
		}
	}
}

// nolint:funlen // one scenario per sanoid rule
func TestScheduleAdvance(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

	t.Run("a day of hourlies is added and pruned by age", func(t *testing.T) {
		s := mustSchedule(t, "hourly=36")
		got := s.Advance(hourlies(t0, 36), t0, t0.Add(24*time.Hour))
		// Sanoid keeps a snapshot until it is older than 36 hours, so the one
		// taken exactly 36 hours ago survives: Keep+1 snapshots, as on a real
		// pool (hourly=48 shows 49 hourlies).
		if want := hourlies(t0.Add(24*time.Hour), 37); !reflect.DeepEqual(snapshotNames(got), snapshotNames(want)) {
			t.Errorf("got %v\nwant %v", snapshotNames(got), snapshotNames(want))
		}
	})

	t.Run("the 1st of a month brings coincident monthly, daily and hourly", func(t *testing.T) {
		s := mustSchedule(t, "hourly=2,daily=2,monthly=2")
		oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got := s.Advance(nil, oct1.Add(-30*time.Minute), oct1.Add(30*time.Minute))
		want := []string{
			"autosnap_2026-10-01_00:00:00_monthly",
			"autosnap_2026-10-01_00:00:00_daily",
			"autosnap_2026-10-01_00:00:00_hourly",
		}
		if !reflect.DeepEqual(snapshotNames(got), want) {
			t.Fatalf("got %v, want %v", snapshotNames(got), want)
		}
		for _, snap := range got {
			if !snap.CreationTime.Equal(oct1) {
				t.Errorf("%s created %v, want %v", snap.Name, snap.CreationTime, oct1)
			}
		}
	})

	t.Run("nothing is added without a boundary", func(t *testing.T) {
		s := mustSchedule(t, "hourly=36,daily=30,monthly=3")
		in := hourlies(t0, 3)
		got := s.Advance(in, t0.Add(10*time.Minute), t0.Add(50*time.Minute))
		if !reflect.DeepEqual(snapshotNames(got), snapshotNames(in)) {
			t.Errorf("got %v, want %v", snapshotNames(got), snapshotNames(in))
		}
	})

	t.Run("only autosnap snapshots are pruned", func(t *testing.T) {
		s := mustSchedule(t, "hourly=1")
		old := t0.Add(-240 * time.Hour)
		in := []files.SnapshotInfo{
			autosnap("hourly", t0),
			{Name: "manual-before-upgrade", CreationTime: old},
			{Name: autosnap("hourly", old).Name, CreationTime: old, Bookmark: true},
			autosnap("hourly", old),
		}
		got := s.Advance(in, t0, t0.Add(30*time.Minute))
		want := []string{autosnap("hourly", t0).Name, "manual-before-upgrade", autosnap("hourly", old).Name}
		if !reflect.DeepEqual(snapshotNames(got), want) || !got[2].Bookmark {
			t.Errorf("got %+v, want %v with the last one a bookmark", got, want)
		}
	})

	t.Run("never prunes below the retention count", func(t *testing.T) {
		s := mustSchedule(t, "monthly=3")
		in := []files.SnapshotInfo{
			autosnap("monthly", time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)),
			autosnap("monthly", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
		}
		got := s.Advance(in, t0, t0.Add(time.Hour))
		if !reflect.DeepEqual(snapshotNames(got), snapshotNames(in)) {
			t.Errorf("got %v, want %v", snapshotNames(got), snapshotNames(in))
		}
	})

	t.Run("a count of 0 takes none and prunes the rest", func(t *testing.T) {
		s := mustSchedule(t, "hourly=0,daily=1")
		got := s.Advance(hourlies(t0, 3), t0, t0.Add(2*time.Hour))
		if len(got) != 0 {
			t.Errorf("got %v, want no snapshots", snapshotNames(got))
		}
	})

	t.Run("weekly snapshots fall on Monday", func(t *testing.T) {
		s := mustSchedule(t, "weekly=4")
		got := s.Advance(nil, t0, t0.Add(7*24*time.Hour)) // Thursday to Thursday
		if want := []string{"autosnap_2026-09-28_00:00:00_weekly"}; !reflect.DeepEqual(snapshotNames(got), want) {
			t.Errorf("got %v, want %v", snapshotNames(got), want)
		}
	})

	t.Run("boundaries and names follow the location", func(t *testing.T) {
		ny, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Skipf("no tzdata: %v", err)
		}
		s := mustSchedule(t, "daily=1")
		s.Location = ny
		midnight := time.Date(2026, 9, 24, 0, 0, 0, 0, ny)
		got := s.Advance(nil, midnight.Add(-time.Hour), midnight.Add(time.Hour))
		if len(got) != 1 || got[0].Name != "autosnap_2026-09-24_00:00:00_daily" || !got[0].CreationTime.Equal(t0.Add(4*time.Hour)) {
			t.Errorf("got %+v, want autosnap_2026-09-24_00:00:00_daily at 04:00 UTC", got)
		}
	})

	t.Run("tie order and gap are adjustable", func(t *testing.T) {
		s := mustSchedule(t, "hourly=2,daily=2,monthly=2")
		s.tieOrder = []string{"daily", "hourly", "monthly"}
		s.tieGap = time.Second
		oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got := s.Advance(nil, oct1.Add(-30*time.Minute), oct1.Add(30*time.Minute))
		want := []string{
			"autosnap_2026-10-01_00:00:02_daily",
			"autosnap_2026-10-01_00:00:01_hourly",
			"autosnap_2026-10-01_00:00:00_monthly",
		}
		if !reflect.DeepEqual(snapshotNames(got), want) {
			t.Errorf("got %v, want %v", snapshotNames(got), want)
		}
	})

	t.Run("a delay moves every snapshot and its name past the boundary", func(t *testing.T) {
		s := mustSchedule(t, "hourly=36,monthly=3")
		s.Delay = 3 * time.Minute
		oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		got := s.Advance(nil, oct1.Add(-time.Hour), oct1)
		if len(got) != 2 {
			t.Fatalf("got %v, want the monthly and the hourly", snapshotNames(got))
		}
		for _, snap := range got {
			if !snap.CreationTime.Equal(oct1.Add(3*time.Minute)) || !strings.HasPrefix(snap.Name, "autosnap_2026-10-01_00:03:00_") {
				t.Errorf("got %s at %v, want a 00:03:00 creation time and name", snap.Name, snap.CreationTime)
			}
		}
	})

	t.Run("the input is not modified", func(t *testing.T) {
		s := mustSchedule(t, "hourly=1")
		in := hourlies(t0, 3)
		before := strings.Join(snapshotNames(in), ",")
		s.Advance(in, t0, t0.Add(5*time.Hour))
		if after := strings.Join(snapshotNames(in), ","); after != before {
			t.Errorf("input changed from %s to %s", before, after)
		}
	})
}
