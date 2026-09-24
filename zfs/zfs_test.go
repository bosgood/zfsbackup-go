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

// External test package: the fake zfs (internal/fakezfs) imports zfs for its
// fixture parser, so these tests cannot live inside package zfs.
package zfs_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/internal/fakezfs"
	"github.com/someone1/zfsbackup-go/zfs"
)

// TestMain lets the test binary stand in for zfs: with FAKEZFS=1 it runs the
// fake instead of the tests (see useFakeZFS).
func TestMain(m *testing.M) {
	fakezfs.RunIfRequested()
	os.Exit(m.Run())
}

// useFakeZFS points zfs.ZFSPath at this test binary, which runs as the fake
// zfs with the given snapshot fixture.
func useFakeZFS(t *testing.T, fixture string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshots.txt")
	if err = os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEZFS", "1")
	t.Setenv("FAKEZFS_SNAPSHOTS", path)
	old := zfs.ZFSPath
	zfs.ZFSPath = self
	t.Cleanup(func() { zfs.ZFSPath = old })
}

const fakeFixture = `tank/data@c	300	snapshot
tank/data#b	200	bookmark
tank/data@b	200	snapshot
tank/data@a	100	snapshot
tank/data@with space	50	snapshot
tank/other@z	900	snapshot
`

func TestGetSnapshotsAndBookmarks(t *testing.T) {
	useFakeZFS(t, fakeFixture)
	got, err := zfs.GetSnapshotsAndBookmarks(context.Background(), "tank/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []files.SnapshotInfo{
		{Name: "c", CreationTime: time.Unix(300, 0)},
		{Name: "b", CreationTime: time.Unix(200, 0), Bookmark: true},
		{Name: "b", CreationTime: time.Unix(200, 0)},
		{Name: "a", CreationTime: time.Unix(100, 0)},
		{Name: "with space", CreationTime: time.Unix(50, 0)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Bookmark != want[i].Bookmark || !got[i].CreationTime.Equal(want[i].CreationTime) {
			t.Errorf("snapshot %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if _, err = zfs.GetSnapshotsAndBookmarks(context.Background(), "tank/missing"); err == nil ||
		!strings.Contains(err.Error(), "dataset does not exist") {
		t.Errorf("unknown dataset: got error %v, want zfs's stderr in it", err)
	}
}

func TestGetCreationDate(t *testing.T) {
	useFakeZFS(t, fakeFixture)
	for target, want := range map[string]int64{"tank/data@a": 100, "tank/data#b": 200} {
		got, err := zfs.GetCreationDate(context.Background(), target)
		if err != nil || !got.Equal(time.Unix(want, 0)) {
			t.Errorf("GetCreationDate(%s) = %v, %v; want %v", target, got, err, time.Unix(want, 0))
		}
	}
	_, err := zfs.GetCreationDate(context.Background(), "tank/data@nope")
	if err == nil || !strings.Contains(err.Error(), "cannot open 'tank/data@nope': dataset does not exist (exit status 1)") {
		t.Errorf("unknown target: got error %v, want zfs's stderr and exit status", err)
	}
}

func TestGetZFSSendDryRun(t *testing.T) {
	useFakeZFS(t, fakeFixture)
	t.Setenv("FAKEZFS_STREAM_BYTES", "123456")
	j := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "c"}, IncrementalSnapshot: files.SnapshotInfo{Name: "a"}}
	if size, err := zfs.GetZFSSendDryRun(context.Background(), j); err != nil || size != 123456 {
		t.Errorf("got %d, %v; want 123456", size, err)
	}

	t.Setenv("FAKEZFS_DRYRUN_OUTPUT", "incremental\ta\ttank/data@c\n")
	if _, err := zfs.GetZFSSendDryRun(context.Background(), j); err == nil || !strings.Contains(err.Error(), "could not parse") {
		t.Errorf("output without a size line: got error %v, want could not parse", err)
	}
}

func TestGetZFSSendCommand(t *testing.T) {
	base := files.SnapshotInfo{Name: "c"}
	testCases := []struct {
		name    string
		jobInfo files.JobInfo
		want    []string
	}{
		{"full", files.JobInfo{}, []string{"send", "tank/data@c"}},
		{"incremental", files.JobInfo{IncrementalSnapshot: files.SnapshotInfo{Name: "a"}}, []string{"send", "-i", "a", "tank/data@c"}},
		{
			"intermediary",
			files.JobInfo{IncrementalSnapshot: files.SnapshotInfo{Name: "a"}, IntermediaryIncremental: true},
			[]string{"send", "-I", "a", "tank/data@c"},
		},
		{
			"from a bookmark",
			files.JobInfo{IncrementalSnapshot: files.SnapshotInfo{Name: "b", Bookmark: true}},
			[]string{"send", "-i", "tank/data#b", "tank/data@c"},
		},
		{
			"flags",
			files.JobInfo{Replication: true, Properties: true, Compressor: files.ZfsCompressor, Raw: true},
			[]string{"send", "-R", "-p", "-c", "-w", "tank/data@c"},
		},
		{"internal compressor sends uncompressed", files.JobInfo{Compressor: files.InternalCompressor}, []string{"send", "tank/data@c"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.jobInfo
			j.VolumeName, j.BaseSnapshot = "tank/data", base
			cmd := zfs.GetZFSSendCommand(context.Background(), &j)
			if got := cmd.Args[1:]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got zfs %v, want zfs %v", got, tc.want)
			}
		})
	}
}

// nolint:funlen // table-driven test
func TestParseSnapshotList(t *testing.T) {
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	aug1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sep24 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	edt := time.FixedZone("EDT", -4*60*60)

	testCases := []struct {
		name    string
		input   string
		loc     *time.Location
		want    []files.SnapshotInfo
		wantErr string
	}{
		{
			name:  "raw snapshot row",
			input: "tank/data@autosnap_2026-09-01_00:00:00_monthly\t1788220800\tsnapshot\n",
			want:  []files.SnapshotInfo{{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: sep1}},
		},
		{
			name:  "raw bookmark row",
			input: "tank/data#autosnap_2026-08-01_00:00:00_monthly\t1785542400\tbookmark\n",
			want:  []files.SnapshotInfo{{Name: "autosnap_2026-08-01_00:00:00_monthly", CreationTime: aug1, Bookmark: true}},
		},
		{
			name:  "raw row separated by spaces",
			input: "tank/data@manual 1788220800 snapshot\n",
			want:  []files.SnapshotInfo{{Name: "manual", CreationTime: sep1}},
		},
		{
			name:  "name and epoch without a type",
			input: "before-upgrade\t1788220800\n",
			want:  []files.SnapshotInfo{{Name: "before-upgrade", CreationTime: sep1}},
		},
		{
			name:  "bare sanoid name",
			input: "autosnap_2026-09-24_00:00:00_daily\n",
			want:  []files.SnapshotInfo{{Name: "autosnap_2026-09-24_00:00:00_daily", CreationTime: sep24}},
		},
		{
			name:  "bare sanoid name is read in the given location",
			input: "autosnap_2026-09-24_00:00:00_daily\n",
			loc:   edt,
			want:  []files.SnapshotInfo{{Name: "autosnap_2026-09-24_00:00:00_daily", CreationTime: sep24.Add(4 * time.Hour)}},
		},
		{
			name:  "bare names with dataset prefixes",
			input: "tank/data@autosnap_2026-09-01_00:00:00_monthly\ntank/data#autosnap_2026-08-01_00:00:00_monthly\n",
			want: []files.SnapshotInfo{
				{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: sep1},
				{Name: "autosnap_2026-08-01_00:00:00_monthly", CreationTime: aug1, Bookmark: true},
			},
		},
		{
			name: "comments and blank lines are skipped, input order is kept",
			input: "# captured on the pool host\n\n" +
				"autosnap_2026-08-01_00:00:00_monthly   # older first on purpose\n" +
				"   # indented comment\n" +
				"tank/data#autosnap_2026-09-01_00:00:00_monthly\t1788220800\tbookmark\n",
			want: []files.SnapshotInfo{
				{Name: "autosnap_2026-08-01_00:00:00_monthly", CreationTime: aug1},
				{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: sep1, Bookmark: true},
			},
		},
		{
			name:    "bare non-sanoid name",
			input:   "autosnap_2026-09-01_00:00:00_monthly\nmanual-snapshot\n",
			wantErr: "line 2: no creation time",
		},
		{
			name:    "invalid epoch",
			input:   "tank/data@a\tyesterday\tsnapshot\n",
			wantErr: "invalid creation epoch",
		},
		{
			name:    "unknown type",
			input:   "tank/data@a\t1788220800\tfilesystem\n",
			wantErr: "unknown type",
		},
		{
			name:    "too many fields",
			input:   "tank/data@a\t1788220800\tsnapshot\textra\n",
			wantErr: "want name[, creation[, type]]",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			loc := tc.loc
			if loc == nil {
				loc = time.UTC
			}
			got, err := zfs.ParseSnapshotList(strings.NewReader(tc.input), loc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d snapshots %+v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i].Name != tc.want[i].Name || got[i].Bookmark != tc.want[i].Bookmark ||
					!got[i].CreationTime.Equal(tc.want[i].CreationTime) {
					t.Errorf("snapshot %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
