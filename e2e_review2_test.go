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

package main

// Regression tests for the second review of the fixes of the 2026-10-12 review (W2, W3, W4):
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bosgood/zfsbackup-go/files"
)

// r2Tree puts tank/data and tank/data/home, each with the snapshots a, b and c, on the pool, and
// sends the --replication chain a, a->b, b->c to env.dest.
func r2Tree(t *testing.T, env *e2eEnv) string {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	var rows strings.Builder
	for _, ds := range []string{"tank/data", "tank/data/home"} {
		for name, at := range map[string]int64{"a": rrTa.Unix(), "b": rrTb.Unix(), "c": rrTc.Unix()} {
			fmt.Fprintf(&rows, "%s@%s\t%d\tsnapshot\n", ds, name, at)
		}
	}
	rrAppendPool(t, env, rows.String())
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "--replication", "tank/data@a", dest)
	env.sendOK(t, "--compressor", "", "--replication", "-i", "a", "tank/data@b", dest)
	env.sendOK(t, "--compressor", "", "--replication", "-i", "b", "tank/data@c", dest)
	return dest
}

// W2: receive --auto -F of a --replication chain checked -F step by step. The full went in, and
// the next step was refused: a part of the chain restored, and an exit code that is not 0.
// --auto now checks the whole chain before the first receive. What the chain's own steps
// received (here the children of the full) does not count.
func TestE2EReview2AutoForceReplicationChain(t *testing.T) {
	t.Run("NewTarget", func(t *testing.T) {
		env := newE2EEnv(t)
		dest := r2Tree(t, env)
		receiveLog := rrReceiveLog(t)
		logs, err := guarded(t, func() (string, error) {
			return env.receive("--auto", "-F", "tank/data@c", dest, "restored/data")
		})
		if err != nil {
			t.Fatalf("receive --auto -F of a --replication chain into a new dataset: %v\n%s", err, logs)
		}
		if n := rrReceives(receiveLog); n != 3 {
			t.Errorf("zfs receive ran %d times, want 3", n)
		}
	})
	t.Run("TargetWithChildren", func(t *testing.T) {
		env := newE2EEnv(t)
		dest := r2Tree(t, env)
		if logs, err := guarded(t, func() (string, error) {
			return env.receive("tank/data@a", dest, "restored/data")
		}); err != nil {
			t.Fatalf("receive of the full: %v\n%s", err, logs)
		}
		// restored/data/home is there before the chain: -F may destroy it.
		receiveLog := rrReceiveLog(t)
		logs, err := guarded(t, func() (string, error) {
			return env.receive("--auto", "-F", "tank/data@c", dest, "restored/data")
		})
		if err == nil {
			t.Fatalf("receive --auto -F into a tree with children exited 0:\n%s", logs)
		}
		if !strings.Contains(logs, "restored/data/home") || !strings.Contains(logs, "--allowDestroy") {
			t.Errorf("want restored/data/home and --allowDestroy in the refusal:\n%s", logs)
		}
		if n := rrReceives(receiveLog); n != 0 {
			t.Errorf("zfs receive ran %d times before the refusal", n)
		}
		if logs, err = guarded(t, func() (string, error) {
			return env.receive("--auto", "-F", "--allowDestroy", "tank/data@c", dest, "restored/data")
		}); err != nil {
			t.Fatalf("receive --auto -F --allowDestroy: %v\n%s", err, logs)
		}
		if n := rrReceives(receiveLog); n != 2 {
			t.Errorf("zfs receive ran %d times, want 2", n)
		}
	})
}

// W3: the manifest recorded the mountpoint of the top dataset only. A --replication stream also
// sets the mountpoints of the datasets below: their own, and those they inherit from a dataset
// in the stream. On another host, a dataset of its own may be mounted at one of them.
func TestE2EReview2MountGuardRecordsReplicatedChildren(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	at := rrTa.Unix()
	rrAppendPool(t, env, fmt.Sprintf("tank/srcA@s\t%d\tsnapshot\ntank/srcA/home@s\t%d\tsnapshot\n"+
		"tank/srcA/tmp@s\t%d\tsnapshot\ntank/srcA/var@s\t%d\tsnapshot\n", at, at, at, at))
	t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/srcA=local:/srv,tank/srcA/home=inherited from tank/srcA:/srv/home,"+
		"tank/srcA/tmp=inherited from tank:/tank/srcA/tmp,tank/srcA/var=local:/var/lib/app")
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "--replication", "tank/srcA@s", dest)
	var recorded map[string]string
	reforgeManifest(t, env, "manifests|tank/srcA|s.manifest.gz", func(m *files.JobInfo) { recorded = m.SourceMountpoints })
	want := map[string]string{"tank/srcA": "/srv", "tank/srcA/home": "/srv/home", "tank/srcA/var": "/var/lib/app"}
	if !reflect.DeepEqual(recorded, want) {
		t.Errorf("the manifest records the mountpoints %q, want %q", recorded, want)
	}

	// The receiving host: no tank/srcA; tank/liveB is mounted at /srv/home.
	rrAppendPool(t, env, fmt.Sprintf("tank/liveB@x\t%d\tsnapshot\n", at))
	t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/liveB=local:/srv/home")
	receiveLog := rrReceiveLog(t)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("tank/srcA@s", dest, "tank/restoredA")
	})
	if err == nil {
		t.Fatalf("a stream that mounts tank/restoredA/home at /srv/home was received without --unmounted "+
			"while tank/liveB is mounted there:\n%s", logs)
	}
	if !strings.Contains(logs, "mount tank/restoredA/home over tank/liveB") {
		t.Errorf("want tank/restoredA/home and tank/liveB named in the refusal:\n%s", logs)
	}
	if n := rrReceives(receiveLog); n != 0 {
		t.Errorf("zfs receive ran %d times", n)
	}
	if logs, err = guarded(t, func() (string, error) {
		return env.receive("--unmounted", "tank/srcA@s", dest, "tank/restoredA")
	}); err != nil {
		t.Errorf("receive --unmounted: %v\n%s", err, logs)
	}
}

// W4: send --dry-run read the manifests at the destination through PreDownload, which on S3
// restores an archived (Glacier) manifest: hours of waiting, and it costs money. A dry run must
// not start one. Without that manifest its plan would be wrong (R2 of the third review), so it
// stops and says how to cache it.
func TestE2EReview2DryRunStartsNoRestore(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"explicit", []string{"-i", "a", "tank/data@b"}},
		{"smart", []string{"--fullIfOlderThan", "720h", "tank/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "b", CreationTime: rrTb}, {Name: "a", CreationTime: rrTa}})
			env.sendOK(t, "--compressor", "", "tank/data@a", "file://"+env.dest)
			objs := destObjects(t, env.dest)
			manifests := manifestNames(objs)
			if len(manifests) != 1 {
				t.Fatalf("want one manifest, got %q", manifests)
			}
			f := newFakeS3(t, objs)
			f.storageClass = map[string]string{manifests[0]: "GLACIER"}
			t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
			env.work = t.TempDir() // an empty cache: the manifest is only at the destination
			logs, err := guarded(t, func() (string, error) {
				return env.send(append([]string{"--dry-run", "--compressor", ""}, append(tc.args, "s3://bucket")...)...)
			})
			if err == nil {
				t.Errorf("send --dry-run with an archived manifest of tank/data that is not cached succeeded:\n%s", logs)
			}
			if restores := restoresOf(f); len(restores) != 0 {
				t.Errorf("send --dry-run asked S3 to restore %q", restores)
			}
			if !strings.Contains(logs, manifests[0]) || !strings.Contains(logs, "archive storage class") {
				t.Errorf("want an error that names the archived manifest %s:\n%s", manifests[0], logs)
			}
		})
	}
}
