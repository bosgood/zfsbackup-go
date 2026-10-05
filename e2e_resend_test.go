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

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/files"
)

// Re-sending a backup set that already exists must never overwrite it: volume boundaries
// are not reproducible, so a re-send that dies partway leaves the old manifest pointing at
// changed objects.

// sameObjects fails unless the destination holds exactly the objects in want, byte for byte.
func sameObjects(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	got := destObjects(t, dir)
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s changed or disappeared", name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s was added", name)
		}
	}
}

func TestE2EExplicitFullAlreadyBackedUpIsNoop(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "autosnap_2026-09-15_00:00:00_daily", CreationTime: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},
		{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	args := []string{"--full", "--fullSnapshotSuffix", "_monthly", "tank/data", "file://" + env.dest}
	if logs, err := env.send(args...); err != nil {
		t.Fatalf("first send: %v\n%s", err, logs)
	}
	before := destObjects(t, env.dest)

	logs, err := env.send(args...)
	if !errors.Is(err, backup.ErrNoOp) || !strings.Contains(logs, "Nothing new to back up.") {
		t.Fatalf("second --full of the same monthly: got %v, want the no-op\n%s", err, logs)
	}
	sameObjects(t, env.dest, before)
}

// TestE2ERefusesToOverwrite: a manual send of a set that is already at the destination
// fails before streaming anything, and leaves the destination and the cache untouched.
func TestE2ERefusesToOverwrite(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	args := []string{"--volsize", "1", "tank/data@a", "file://" + env.dest}
	if logs, err := env.send(args...); err != nil {
		t.Fatalf("first send: %v\n%s", err, logs)
	}
	dest, cache := destObjects(t, env.dest), destObjects(t, filepath.Join(env.work, "cache"))
	if len(manifestNames(dest)) != 1 || len(cache) != 1 {
		t.Fatalf("first send left %d manifests at the destination and %d in the cache, want 1 and 1", len(manifestNames(dest)), len(cache))
	}
	zfsSends := func() int {
		data, err := os.ReadFile(env.zfsLog)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count("\n"+string(data), "\nsend ")
	}
	sends := zfsSends()
	if sends != 1 {
		t.Fatalf("the first send ran zfs send %d times, want 1", sends)
	}

	for _, extra := range [][]string{nil, {"--resume"}} {
		logs, err := guarded(t, func() (string, error) { return env.send(append(extra, args...)...) })
		if err == nil || !strings.Contains(logs, "already exists") {
			t.Errorf("send %v of an existing set: got %v, want the refusal\n%s", extra, err, logs)
		}
	}
	sameObjects(t, env.dest, dest)
	sameObjects(t, filepath.Join(env.work, "cache"), cache)
	if after := zfsSends(); after != sends {
		t.Errorf("the refused sends ran zfs send %d times", after-sends)
	}
}

// TestE2EChecksExistingSetUnderLock: a send must not judge whether the set exists before it
// holds the lock. Checked earlier, a send that finished in between would be overwritten.
func TestE2EChecksExistingSetUnderLock(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	args := []string{"tank/data@a", "file://" + env.dest}
	if logs, err := env.send(args...); err != nil {
		t.Fatalf("first send: %v\n%s", err, logs)
	}

	// Another live process (our parent) holds the lock.
	lock := lockFile("tank/data")
	if err := os.WriteFile(lock, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lock)

	logs, err := guarded(t, func() (string, error) { return env.send(args...) })
	if err == nil || !strings.Contains(logs, "Cannot lock") || strings.Contains(logs, "already exists") {
		t.Errorf("send while another holds the lock: got %v, want only the lock error\n%s", err, logs)
	}
}

// TestE2EVolumeBoundariesReproducible documents the root cause behind refusing to overwrite:
// the same stream does not split into the same volumes twice.
func TestE2EVolumeBoundariesReproducible(t *testing.T) {
	t.Skip("volume boundaries are not reproducible; see docs/specs/2026-09-29--destructive-ops-fixes")
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "12582912") // 12 MiB
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	var first []string
	for i := 0; i < 3; i++ {
		dest := filepath.Join(env.dest, fmt.Sprintf("d%d", i))
		if err := os.Mkdir(dest, 0700); err != nil {
			t.Fatal(err)
		}
		if logs, err := env.send("--volsize", "2", "tank/data@a", "file://"+dest); err != nil {
			t.Fatalf("send %d: %v\n%s", i, err, logs)
		}
		var bounds []string
		for _, v := range newestBackup(t, "tank/data", "file://"+dest).Volumes {
			bounds = append(bounds, fmt.Sprintf("%d:%.8s", v.ZFSStreamBytes, v.SHA256Sum))
		}
		if i == 0 {
			first = bounds
		} else if strings.Join(bounds, " ") != strings.Join(first, " ") {
			t.Errorf("run %d split the stream as %v, run 0 as %v", i, bounds, first)
		}
	}
}
