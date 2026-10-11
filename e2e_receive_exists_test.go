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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// receiveTargets are the ways to name restored/data as the target of a receive of tank/data:
// directly, with -d (the first component of the backed-up name replaced) and with -e (the last
// component appended).
var receiveTargets = []struct {
	name  string
	flags []string
	local string
}{
	{"plain", nil, "restored/data"},
	{"fullPath", []string{"--fullPath"}, "restored"},
	{"lastPath", []string{"--lastPath"}, "restored"},
}

// sentWithLocal sends tank/data@a (created 2026-09-01) to env.dest, then gives the pool
// restored/data@a created at local, and returns the receive log zfs receive would write.
func (env *e2eEnv) sentWithLocal(t *testing.T, local time.Time) string {
	t.Helper()
	sent := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	env.writeSnapshots(t, "tank/data", nil)
	pool := fmt.Sprintf("tank/data@a\t%d\tsnapshot\n", sent.Unix())
	if err := os.WriteFile(env.snapshots, []byte(pool), 0600); err != nil {
		t.Fatal(err)
	}
	env.sendOK(t, "--volsize", "1", "--compressor", "", "tank/data@a", "file://"+env.dest)
	pool += fmt.Sprintf("restored/data@a\t%d\tsnapshot\n", local.Unix())
	if err := os.WriteFile(env.snapshots, []byte(pool), 0600); err != nil {
		t.Fatal(err)
	}
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	return receiveLog
}

// noReceive fails the test if zfs receive ran.
func noReceive(t *testing.T, receiveLog, logs string) {
	t.Helper()
	if data, err := os.ReadFile(receiveLog); err == nil && len(data) > 0 {
		t.Errorf("zfs receive ran: %q\n%s", data, logs)
	}
}

// An explicit receive of a snapshot the target already holds (same name, same creation time)
// does nothing: zfs would refuse it, or with --force overwrite the dataset.
func TestE2EReceiveOfLocalSnapshotIsNoOp(t *testing.T) {
	for _, target := range receiveTargets {
		t.Run(target.name, func(t *testing.T) {
			env := newE2EEnv(t)
			receiveLog := env.sentWithLocal(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
			args := append(append([]string{"--force=false"}, target.flags...), "tank/data@a", "file://"+env.dest, target.local)
			logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
			if err != nil {
				t.Fatalf("receive: %v\n%s", err, logs)
			}
			if !strings.Contains(logs, "already exists") {
				t.Errorf("want the no-op notice\n%s", logs)
			}
			noReceive(t, receiveLog, logs)
		})
	}
}

// A local snapshot with the backed-up name but another creation time is another snapshot: an
// explicit receive refuses it, with or without --force, names both times, and says how to get
// the name free without losing the local snapshot.
func TestE2EReceiveRefusesSameNameOtherSnapshot(t *testing.T) {
	for _, target := range receiveTargets {
		for _, force := range []string{"--force=false", "--force=true"} {
			t.Run(target.name+"/"+strings.TrimPrefix(force, "--force="), func(t *testing.T) {
				env := newE2EEnv(t)
				receiveLog := env.sentWithLocal(t, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
				args := append(append([]string{force}, target.flags...), "tank/data@a", "file://"+env.dest, target.local)
				logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
				if err == nil {
					t.Errorf("receive over another snapshot named a succeeded\n%s", logs)
				}
				for _, want := range []string{"2026-09-01 00:00:00 UTC", "2026-09-02 00:00:00 UTC", "zfs rename restored/data@a"} {
					if !strings.Contains(logs, want) {
						t.Errorf("want the log to name %s\n%s", want, logs)
					}
				}
				noReceive(t, receiveLog, logs)
			})
		}
	}
}

// receive --auto checks the source of each incremental step in the listing, as an explicit
// receive does: the earlier step's zfs receive put it there. A second run finds the chain's
// last snapshot there and does nothing. If a step's receive did not leave its snapshot where
// the next step looks (here: the fake cannot tell what a salted stream carries), the next step
// stops before zfs receive.
func TestE2EReceiveAutoChecksEachSource(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost=", lost), func(t *testing.T) {
			env := newE2EEnv(t)
			t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
				{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
				{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			})
			dest := "file://" + env.dest
			env.sendOK(t, "--compressor", "", "tank/data@a", dest)
			env.sendOK(t, "--compressor", "", "-i", "a", "tank/data@b", dest)
			if lost {
				t.Setenv("FAKEZFS_STREAM_SALT", "unknown")
			}
			receiveLog := filepath.Join(t.TempDir(), "receive.log")
			t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
			receive := func() (string, error) {
				return guarded(t, func() (string, error) { return env.receive("--auto", "tank/data@b", dest, "restored/data") })
			}
			logs, err := receive()
			got, _ := os.ReadFile(receiveLog)
			if lost {
				if err == nil || !strings.Contains(logs, "Selected incremental snapshot does not exist") {
					t.Errorf("receive --auto with step 1's snapshot missing: %v, want the missing source refused\n%s", err, logs)
				}
				if n := strings.Count(string(got), "\n"); n != 1 {
					t.Errorf("want 1 zfs receive (the full of a), got %d\n%s", n, logs)
				}
				return
			}
			if err != nil {
				t.Fatalf("receive --auto: %v\n%s", err, logs)
			}
			if n := strings.Count(string(got), "\n"); n != 2 {
				t.Errorf("want 2 zfs receives, got %d\n%s", n, logs)
			}
			if logs, err = receive(); err != nil || !strings.Contains(logs, "already exists") {
				t.Errorf("a second receive --auto: %v, want the no-op notice\n%s", err, logs)
			}
			if again, _ := os.ReadFile(receiveLog); len(again) != len(got) {
				t.Errorf("the second receive --auto ran zfs receive\n%s", logs)
			}
		})
	}
}
