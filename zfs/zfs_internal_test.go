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

package zfs

import (
	"context"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

func TestParseSendSizeEstimate(t *testing.T) {
	testCases := []struct {
		name    string
		out     string
		want    uint64
		wantErr bool
	}{
		{
			name: "typical -P output",
			out:  "full\ttank/data@snap1\t123456\nsize\t123456\n",
			want: 123456,
		},
		{
			name: "incremental -P output",
			out:  "incremental\tsnap1\ttank/data@snap2\t42\nsize\t42\n",
			want: 42,
		},
		{
			name:    "no size line",
			out:     "full\ttank/data@snap1\t123456\n",
			wantErr: true,
		},
		{
			name:    "non-numeric size",
			out:     "size\tnotanumber\n",
			wantErr: true,
		},
		{
			name:    "empty output",
			out:     "",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSendSizeEstimate(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got nil error, want one")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSendDryRunArgs pins the invariant sendDryRunArgs relies on: that
// GetZFSSendCommand emits [ZFSPath, "send", ...], so Args[2:] is everything
// after the verb.
func TestSendDryRunArgs(t *testing.T) {
	ctx := context.Background()

	j := &files.JobInfo{
		VolumeName:   "tank/data",
		BaseSnapshot: files.SnapshotInfo{Name: "snap2"},
	}
	base := GetZFSSendCommand(ctx, j)
	if len(base.Args) < 2 || base.Args[1] != "send" {
		t.Fatalf("GetZFSSendCommand args = %v, want Args[1] == \"send\"", base.Args)
	}

	got := sendDryRunArgs(base)
	if got[0] != "send" || got[1] != "-n" || got[2] != "-P" {
		t.Fatalf("sendDryRunArgs = %v, want it to start with send -n -P", got)
	}
	if want := "tank/data@snap2"; got[len(got)-1] != want {
		t.Errorf("last arg = %q, want %q", got[len(got)-1], want)
	}
	if strings.Join(got[3:], " ") != strings.Join(base.Args[2:], " ") {
		t.Errorf("args after -n -P = %v, want %v", got[3:], base.Args[2:])
	}

	// The incremental form must keep -i and its value intact.
	j.IncrementalSnapshot = files.SnapshotInfo{Name: "snap1"}
	got = sendDryRunArgs(GetZFSSendCommand(ctx, j))
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-i snap1") {
		t.Errorf("incremental args = %q, want them to contain \"-i snap1\"", joined)
	}
	if !strings.HasPrefix(joined, "send -n -P ") {
		t.Errorf("incremental args = %q, want the -n -P prefix", joined)
	}
}

// TestSendDryRunArgsDoesNotMutateBase guards against the append aliasing hazard.
func TestSendDryRunArgsDoesNotMutateBase(t *testing.T) {
	base := GetZFSSendCommand(context.Background(), &files.JobInfo{
		VolumeName:   "tank/data",
		BaseSnapshot: files.SnapshotInfo{Name: "snap1"},
	})
	before := strings.Join(base.Args, " ")
	_ = sendDryRunArgs(base)
	if after := strings.Join(base.Args, " "); after != before {
		t.Errorf("sendDryRunArgs mutated the base command: %q -> %q", before, after)
	}
}
