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
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/zfs"
)

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
