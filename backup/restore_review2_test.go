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

// Regression tests for the second review of the fixes of the 2026-10-12 review (W1, W2, W3, N1,
// N5): docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md.

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/files"
	"github.com/bosgood/zfsbackup-go/zfs"
)

// pipeSequence returns a sequence of one 4 MiB volume, larger than the volume's write buffer:
// its writer waits for a reader.
func pipeSequence() (*cacheBackend, downloadSequence, chan *files.VolumeInfo) {
	data := bytes.Repeat([]byte("zfs stream "), 400<<10)
	c := make(chan *files.VolumeInfo, 1)
	return &cacheBackend{objects: map[string][]byte{"v1": data}},
		downloadSequence{volume: &files.VolumeInfo{ObjectName: "v1", Size: uint64(len(data))}, c: c}, c
}

// returnsWithin fails the test unless fn returns within 10 s, and returns its error.
func returnsWithin(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: it waits on a pipe that nobody reads", what)
		return nil
	}
}

// W1: in pipe mode, zfs receive exits early (here at once). The reader stops and returns an
// error; the writer of the volume must fail too, not wait in a write to the pipe forever (Receive's
// wg.Wait() waited for it). The writer's ctx is not canceled here: the reader must abort it.
func TestReview2PipeReaderFailureAbortsWriter(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		job  files.JobInfo
	}{
		{"zfs receive exits", "false", files.JobInfo{}},
		{"extract fails", "cat", files.JobInfo{Compressor: "no-such-decompressor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, seq, c := pipeSequence()
			buffer := make(chan interface{}, 1)
			buffer <- nil
			writer := make(chan error, 1)
			go func() { writer <- processSequence(context.Background(), seq, b, true) }()
			job := tc.job
			if err := returnsWithin(t, "receiveStream", func() error {
				return receiveStream(context.Background(), exec.Command(tc.cmd), &job, c, buffer)
			}); err == nil {
				t.Fatal("receiveStream succeeded")
			}
			if err := returnsWithin(t, "the volume's writer", func() error { return <-writer }); err == nil {
				t.Error("the writer of a volume that was not read to its end succeeded")
			}
		})
	}
}

// W1: another part of the restore failed, and nobody takes the volume from its channel. Its
// writer must stop when the restore's ctx is canceled.
func TestReview2PipeCanceledRestoreAbortsWriter(t *testing.T) {
	b, seq, c := pipeSequence()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := make(chan error, 1)
	go func() { writer <- processSequence(ctx, seq, b, true) }()
	<-c // the volume is ready; nothing reads it
	cancel()
	if err := returnsWithin(t, "the volume's writer", func() error { return <-writer }); err == nil {
		t.Error("the writer of a volume that nobody read succeeded")
	}
}

// N1, W2: what refuseDestroy counts as data that -F destroys. No case here runs zfs (exists is
// false, or no --replication incremental): the e2e tests cover the datasets below the target.
func TestReview2RefuseDestroy(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sn := func(name string, t time.Time) files.SnapshotInfo {
		return files.SnapshotInfo{Name: name, CreationTime: t}
	}
	full := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: sn("a", at)}
	incr := func(base, source string, replication bool) *files.JobInfo {
		return &files.JobInfo{
			VolumeName: "tank/data", BaseSnapshot: sn(base, at.Add(time.Hour)), IncrementalSnapshot: sn(source, at), Replication: replication,
		}
	}
	bookmark := files.SnapshotInfo{Name: "a", CreationTime: at, Bookmark: true}
	for _, tc := range []struct {
		name   string
		chain  []*files.JobInfo
		local  []files.SnapshotInfo
		exists bool
		want   string // "" = allowed
	}{
		{"same-second snapshot of another name", []*files.JobInfo{incr("b", "a", false)},
			[]files.SnapshotInfo{sn("a", at), sn("a_hourly", at)}, true, "restored@a_hourly"},
		{"older snapshot only", []*files.JobInfo{incr("b", "a", false)},
			[]files.SnapshotInfo{sn("a", at), sn("old", at.Add(-time.Hour))}, true, ""},
		{"bookmarks only, full stream", []*files.JobInfo{full}, []files.SnapshotInfo{bookmark}, true, "has no snapshots"},
		{"full into a new target", []*files.JobInfo{full}, nil, false, ""},
		{"replication chain into a new target", []*files.JobInfo{full, incr("b", "a", true)}, nil, false, ""},
		// exists is false only so that no zfs runs: the older snapshot is what counts here.
		{"replication with an older snapshot", []*files.JobInfo{incr("b", "a", true)},
			[]files.SnapshotInfo{sn("a", at), sn("old", at.Add(-time.Hour))}, false, "restored@old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &files.JobInfo{Force: true}
			err := refuseDestroy(context.Background(), j, tc.chain, "restored", tc.local, tc.exists)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Errorf("allowed; want a refusal that names %q", tc.want)
			case tc.want != "":
				logs := captureLogs(t)
				_ = refuseDestroy(context.Background(), j, tc.chain, "restored", tc.local, tc.exists)
				if !strings.Contains(logs.String(), tc.want) {
					t.Errorf("the refusal does not name %q:\n%s", tc.want, logs.String())
				}
				j.AllowDestroy = true
				if err = refuseDestroy(context.Background(), j, tc.chain, "restored", tc.local, tc.exists); err != nil {
					t.Errorf("refused with --allowDestroy: %v", err)
				}
			}
		})
	}
}

// W3: which mountpoints a stream sets. With --replication, a child's own mountpoint and one it
// inherits from a dataset in the stream; never one inherited from outside the stream, or a default.
func TestReview2MountpointInStream(t *testing.T) {
	j := &files.JobInfo{VolumeName: "tank/data", Replication: true}
	props := &files.JobInfo{VolumeName: "tank/data", Properties: true}
	for _, tc := range []struct {
		job  *files.JobInfo
		row  zfs.Mountpoint
		want bool
	}{
		{j, zfs.Mountpoint{Dataset: "tank/data", Source: "local"}, true},
		{j, zfs.Mountpoint{Dataset: "tank/data/home", Source: "received"}, true},
		{j, zfs.Mountpoint{Dataset: "tank/data/home", Source: "inherited from tank/data"}, true},
		{j, zfs.Mountpoint{Dataset: "tank/data/home/x", Source: "inherited from tank/data/home"}, true},
		{j, zfs.Mountpoint{Dataset: "tank/data", Source: "inherited from tank"}, false},
		{j, zfs.Mountpoint{Dataset: "tank/data/home", Source: "inherited from tank"}, false},
		{j, zfs.Mountpoint{Dataset: "tank/data/home", Source: "inherited from tank/data2"}, false},
		{j, zfs.Mountpoint{Dataset: "tank/data/home", Source: "default"}, false},
		{props, zfs.Mountpoint{Dataset: "tank/data/home", Source: "inherited from tank/data"}, false},
	} {
		if got := mountpointInStream(tc.job, tc.row); got != tc.want {
			t.Errorf("replication=%v %s (%s): got %v, want %v", tc.job.Replication, tc.row.Dataset, tc.row.Source, got, tc.want)
		}
	}
}

// N5: with --increment, "nothing newer" returned before the broken-chain check, without a word.
// The next incremental is refused, so the Warning must come now.
func TestReview2IncrementNothingNewerWarnsOfBrokenChain(t *testing.T) {
	m1, d2, d3 := snap("d1_monthly", reviewDay(1)), snap("d2_daily", reviewDay(2)), snap("d3_daily", reviewDay(3))
	broken := []*files.JobInfo{incrManifest(d3, d2), fullManifest(m1)} // incrManifest(d2, m1) is gone
	ji := files.JobInfo{VolumeName: "tank/data", Incremental: true, FullIfOlderThan: -1 * time.Minute}
	got, err := planSmartAt(reviewDay(4), &ji, []files.SnapshotInfo{d3, d2, m1}, [][]*files.JobInfo{broken}, false)
	if err != nil || got.Action != PlanNoop || got.Reason != reasonNothingNewer {
		t.Fatalf("got %s, %v; want NOOP %s", got, err, reasonNothingNewer)
	}
	if !hasWarning(got, "broken", "no backup of d2_daily", "--full") {
		t.Errorf("warnings %q do not name the missing link d2_daily", got.Warnings)
	}
}
