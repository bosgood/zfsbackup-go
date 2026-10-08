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
	"io/ioutil"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// describeManifests renders manifests as "base" or "source to base" with
// creation times, for comparisons.
func describeManifests(manifests []*files.JobInfo) []string {
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		d := fmt.Sprintf("%s@%d", m.BaseSnapshot.Name, m.BaseSnapshot.CreationTime.Unix())
		if m.IncrementalSnapshot.Name != "" {
			d = fmt.Sprintf("%s@%d to %s", m.IncrementalSnapshot.Name, m.IncrementalSnapshot.CreationTime.Unix(), d)
		}
		out = append(out, d)
	}
	return out
}

func TestLoadScenario(t *testing.T) {
	s, err := LoadScenario("testdata/scenarios/loader-smoke")
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}

	if s.Volume != "tank/data" || !s.Until.IsZero() || s.Schedule != nil {
		t.Errorf("got volume %q, until %v, schedule %v; want tank/data and a single next run", s.Volume, s.Until, s.Schedule)
	}
	j := s.JobInfo
	if j.FullIfOlderThan != 4320*time.Hour || j.FullSnapshotSuffix != "_monthly" || j.IncrementalSnapshotSuffix != "_monthly" ||
		j.Full || j.Incremental || j.SnapshotPrefix != "" {
		t.Errorf("got smart options %+v", j)
	}

	wantSnaps := []files.SnapshotInfo{
		{Name: "autosnap_2026-09-24_00:00:00_daily", CreationTime: time.Unix(1790208000, 0)},
		{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: time.Unix(1788220800, 0)},
		{Name: "autosnap_2026-08-01_00:00:00_monthly", CreationTime: time.Unix(1785542400, 0), Bookmark: true},
		{Name: "autosnap_2026-07-01_00:00:00_monthly", CreationTime: time.Unix(1782864000, 0)},
	}
	if len(s.Snapshots) != len(wantSnaps) {
		t.Fatalf("got snapshots %+v, want %+v", s.Snapshots, wantSnaps)
	}
	for i := range wantSnaps {
		if !snapshotsIdentical(s.Snapshots[i], wantSnaps[i]) {
			t.Errorf("snapshot %d = %+v, want %+v", i, s.Snapshots[i], wantSnaps[i])
		}
	}

	if len(s.DestBackups) != 1 {
		t.Fatalf("got %d destinations, want 1", len(s.DestBackups))
	}
	want := []string{
		"autosnap_2026-07-01_00:00:00_monthly@1782864000 to autosnap_2026-08-01_00:00:00_monthly@1785542400",
		"autosnap_2026-07-01_00:00:00_monthly@1782864000",
	}
	if got := describeManifests(s.DestBackups[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("got manifests %v, want newest-first %v", got, want)
	}
	for _, m := range s.DestBackups[0] {
		if m.VolumeName != "tank/data" {
			t.Errorf("manifest %v has volume %q, want tank/data", m.BaseSnapshot.Name, m.VolumeName)
		}
	}
}

func TestLoadScenarioSnapshotsJSON(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := ioutil.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("flags", "--full\n")
	write("snapshots.json", `[{"name": "tank/data@autosnap_2026-09-01_00:00:00_monthly", "used": "0B"}]`)
	s, err := LoadScenario(dir)
	if err != nil {
		t.Fatalf("LoadScenario: %v", err)
	}
	if len(s.Snapshots) != 1 || s.Snapshots[0].Name != "autosnap_2026-09-01_00:00:00_monthly" {
		t.Errorf("got snapshots %+v, want the one in snapshots.json", s.Snapshots)
	}

	write("snapshots.txt", "autosnap_2026-08-01_00:00:00_monthly\n")
	if _, err = LoadScenario(dir); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("got error %v, want one for having both snapshots.txt and snapshots.json", err)
	}
}

func TestReadManifests(t *testing.T) {
	s := &Scenario{Volume: "pool/app"}
	if err := s.ReadSnapshots(strings.NewReader("tank/data@snap-a\t1788220800\tsnapshot\n")); err != nil {
		t.Fatalf("ReadSnapshots: %v", err)
	}
	input := `
# destination 1
snap-a                                    # creation from the snapshot list
autosnap_2026-07-01_00:00:00_monthly      # pruned: creation from the name
old-manual 1751328000 to snap-a           # explicit epoch
---
# destination 2 has nothing
---
tank/data@snap-a
`
	if err := s.ReadManifests(strings.NewReader(input)); err != nil {
		t.Fatalf("ReadManifests: %v", err)
	}
	var got [][]string
	for _, dest := range s.DestBackups {
		got = append(got, describeManifests(dest))
	}
	want := [][]string{
		{"snap-a@1788220800", "old-manual@1751328000 to snap-a@1788220800", "autosnap_2026-07-01_00:00:00_monthly@1782864000"},
		{},
		{"snap-a@1788220800"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if v := s.DestBackups[0][0].VolumeName; v != "pool/app" {
		t.Errorf("manifest volume = %q, want the scenario's pool/app", v)
	}

	for _, bad := range []string{"unknown-snap\n", "snap-a to\n", "snap-a notanepoch\n", "a b c\n"} {
		if err := s.ReadManifests(strings.NewReader(bad)); err == nil {
			t.Errorf("ReadManifests(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseSmartFlags(t *testing.T) {
	j, err := ParseSmartFlags(strings.NewReader("--increment\n--snapshotPrefix autosnap_ # comment\n"))
	if err != nil || !j.Incremental || j.SnapshotPrefix != "autosnap_" || j.FullIfOlderThan != -1*time.Minute {
		t.Errorf("got %+v, %v", j, err)
	}
	for _, bad := range []string{
		"",                   // no smart option
		"--full --increment", // two smart options
		"--fullIfOlderThan 720h --compressor zfs", // not a smart flag
		"--full tank/data",                        // positional argument
	} {
		if _, err := ParseSmartFlags(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseSmartFlags(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseScheduleSpec(t *testing.T) {
	s := &Scenario{}
	spec := `
policy=hourly=36,daily=30   # sanoid retention
monthly=3
from=2026-09-24T01:00:00Z, until=2027-10-01
every=12h
snapshot-delay=3m
skip=2026-12-24..2027-01-03, skip=2027-02-01T00:00:00Z..2027-02-02
volume=pool/app
location=UTC
`
	if err := s.ParseScheduleSpec(spec); err != nil {
		t.Fatalf("ParseScheduleSpec: %v", err)
	}
	if s.Schedule == nil || s.Schedule.String() != "monthly=3,daily=30,hourly=36" {
		t.Errorf("got schedule %v", s.Schedule)
	}
	if !s.From.Equal(time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)) || !s.Until.Equal(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got from %v until %v", s.From, s.Until)
	}
	if s.Every != 12*time.Hour || s.Volume != "pool/app" {
		t.Errorf("got every %v, volume %q", s.Every, s.Volume)
	}
	if s.Schedule.Delay != 3*time.Minute {
		t.Errorf("got snapshot delay %v, want 3m", s.Schedule.Delay)
	}
	wantSkips := []TimeRange{
		{From: time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), Until: time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)},
		{From: time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2027, 2, 2, 0, 0, 0, 0, time.UTC)},
	}
	if len(s.Skips) != len(wantSkips) {
		t.Fatalf("got skips %v, want %v", s.Skips, wantSkips)
	}
	for i := range wantSkips {
		if !s.Skips[i].From.Equal(wantSkips[i].From) || !s.Skips[i].Until.Equal(wantSkips[i].Until) {
			t.Errorf("skip %d: got %v, want %v", i, s.Skips[i], wantSkips[i])
		}
	}

	for _, bad := range []string{
		"policy=hourly=36",                                     // simulating needs until
		"until=2027-01-01,from=2027-02-01",                     // until before from
		"until=2027-01-01,every=0s",                            // every must be positive
		"until=2027-01-01,frequency=2",                         // unknown setting
		"until=2027-01-01,policy=fortnightly=2",                // unknown period
		"until=next-week",                                      // bad time
		"until=2027-01-01,location=Mars/Olympus",               // unknown zone
		"until=2027-01-01,oops",                                // not key=value
		"checks=chain-link",                                    // unknown check
		"checks=coverage:",                                     // coverage needs a suffix
		"until=2027-01-01,policy=hourly=36,snapshot-delay=-1m", // delay cannot be negative
		"until=2027-01-01,snapshot-delay=3m",                   // delay without a policy
		"until=2027-01-01,skip=2026-12-24",                     // skip needs from..until
		"until=2027-01-01,skip=2027-01-03..2026-12-24",         // reversed range
		"skip=2026-12-24..2026-12-26",                          // simulating needs until
	} {
		if err := (&Scenario{}).ParseScheduleSpec(bad); err == nil {
			t.Errorf("ParseScheduleSpec(%q) succeeded, want an error", bad)
		}
	}
}
