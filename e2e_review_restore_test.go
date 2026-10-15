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

// Regression tests for the restore findings of the 2026-10-12 review (findings 2, 5-10, 19):
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/files"
)

var (
	rrTa = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rrTb = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rrTc = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

// rrChain sends the full a, a->b and b->c of tank/data to env.dest and returns the receive log.
func rrChain(t *testing.T, env *e2eEnv) string {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "c", CreationTime: rrTc}, {Name: "b", CreationTime: rrTb}, {Name: "a", CreationTime: rrTa},
	})
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "tank/data@a", dest)
	env.sendOK(t, "--compressor", "", "-i", "a", "tank/data@b", dest)
	env.sendOK(t, "--compressor", "", "-i", "b", "tank/data@c", dest)
	return rrReceiveLog(t)
}

// rrReceiveLog sets FAKEZFS_RECEIVE_LOG to a new file and returns its path.
func rrReceiveLog(t *testing.T) string {
	t.Helper()
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	return receiveLog
}

// rrReceives returns how many times zfs receive ran.
func rrReceives(receiveLog string) int {
	data, err := os.ReadFile(receiveLog)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// rrAppendPool adds rows to the fake's snapshot fixture.
func rrAppendPool(t *testing.T, env *e2eEnv, rows string) {
	t.Helper()
	pool, err := os.ReadFile(env.snapshots)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err = os.WriteFile(env.snapshots, append(pool, rows...), 0600); err != nil {
		t.Fatal(err)
	}
}

// rrZFSCalls returns the zfs invocations that start with prefix.
func rrZFSCalls(t *testing.T, env *e2eEnv, prefix string) []string {
	t.Helper()
	data, _ := os.ReadFile(env.zfsLog)
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			calls = append(calls, line)
		}
	}
	return calls
}

// Finding 2: a pipe-mode receive (--maxFileBuffer 0) of a volume that fits in the volume's write
// buffer (256 KiB) deadlocked: Close held the lock while it flushed into the pipe, and Extract
// waited for the lock before it read.
func TestE2EReviewPipeReceiveSmallVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain", []string{"--compressor", ""}},
		{"gzip", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: rrTa}})
			env.sendOK(t, append(tc.args, "tank/data@a", "file://"+env.dest)...)
			receiveLog := rrReceiveLog(t)
			logs, err := guarded(t, func() (string, error) {
				return env.receive("--maxFileBuffer", "0", "tank/data@a", "file://"+env.dest, "restored/data")
			})
			if err != nil {
				t.Fatalf("receive: %v\n%s", err, logs)
			}
			if n := rrReceives(receiveLog); n != 1 {
				t.Errorf("zfs receive ran %d times, want 1", n)
			}
		})
	}
}

// Finding 5: receive --auto keyed a parent by the text of its creation time, zone included. A
// link written by a sender in another zone could not find its parent.
func TestE2EReviewAutoChainAcrossTimeZones(t *testing.T) {
	env := newE2EEnv(t)
	receiveLog := rrChain(t, env)
	plusOne := time.FixedZone("CET", 3600)
	reforgeManifest(t, env, "manifests|tank/data|b|to|c.manifest.gz", func(m *files.JobInfo) {
		m.BaseSnapshot.CreationTime = m.BaseSnapshot.CreationTime.In(plusOne)
		m.IncrementalSnapshot.CreationTime = m.IncrementalSnapshot.CreationTime.In(plusOne)
	})
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--auto", "tank/data@c", "file://"+env.dest, "restored/data")
	})
	if err != nil {
		t.Fatalf("receive --auto of a chain whose last link was written in another zone: %v\n%s", err, logs)
	}
	if n := rrReceives(receiveLog); n != 3 {
		t.Errorf("zfs receive ran %d times, want 3\n%s", n, logs)
	}
}

// Finding 6: receive --auto --origin passed -o origin= to every step (real zfs refuses a clone
// origin for a dataset that exists), and counted the origin as a snapshot of the target.
func TestE2EReviewAutoOrigin(t *testing.T) {
	t.Run("FirstStepOnly", func(t *testing.T) {
		env := newE2EEnv(t)
		receiveLog := rrChain(t, env)
		// other/base@a: another dataset's snapshot named a, created when tank/data@a was.
		rrAppendPool(t, env, fmt.Sprintf("other/base@a\t%d\tsnapshot\n", rrTa.Unix()))
		logs, err := guarded(t, func() (string, error) {
			return env.receive("--auto", "--origin", "other/base@a", "tank/data@c", "file://"+env.dest, "restored/data")
		})
		if err != nil {
			t.Fatalf("receive --auto --origin: %v\n%s", err, logs)
		}
		receives := rrZFSCalls(t, env, "receive ")
		if len(receives) != 2 || rrReceives(receiveLog) != 2 {
			t.Fatalf("want 2 zfs receives, got %q\n%s", receives, logs)
		}
		if !strings.Contains(receives[0], "origin=other/base@a") || strings.Contains(receives[1], "origin=") {
			t.Errorf("want -o origin= on the first zfs receive only:\n%s", strings.Join(receives, "\n"))
		}
	})
	t.Run("OriginNamedAsTarget", func(t *testing.T) {
		env := newE2EEnv(t)
		receiveLog := rrChain(t, env)
		logs, err := guarded(t, func() (string, error) {
			return env.receive("--auto", "--origin", "tank/data@c", "tank/data@c", "file://"+env.dest, "restored/data")
		})
		pool, _ := os.ReadFile(env.snapshots)
		if err == nil && !strings.Contains(string(pool), "restored/data@c") {
			t.Errorf("receive --auto --origin tank/data@c exited 0 with restored/data@c not there; zfs receive ran %d "+
				"times:\n%s", rrReceives(receiveLog), logs)
		}
		if strings.Contains(logs, "already exists") {
			t.Errorf("the origin counted as a snapshot of the target:\n%s", logs)
		}
	})
}

// Finding 7: "already exists, nothing to do" compared name and creation time only. A local
// snapshot of the same name and second but another guid is another snapshot.
func TestE2EReviewReceiveExistingSnapshotGUID(t *testing.T) {
	setup := func(t *testing.T) (*e2eEnv, string) {
		env := newE2EEnv(t)
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: rrTa}})
		env.sendOK(t, "--compressor", "", "tank/data@a", "file://"+env.dest)
		// restored/data@a: same name, same second.
		rrAppendPool(t, env, fmt.Sprintf("restored/data@a\t%d\tsnapshot\n", rrTa.Unix()))
		return env, rrReceiveLog(t)
	}
	for _, mode := range [][]string{nil, {"--auto"}} {
		name := "explicit"
		if mode != nil {
			name = "auto"
		}
		t.Run(name+"/OtherGUID", func(t *testing.T) {
			env, receiveLog := setup(t)
			t.Setenv("FAKEZFS_GUIDS", "restored/data@a=42")
			logs, err := guarded(t, func() (string, error) {
				return env.receive(append(mode, "tank/data@a", "file://"+env.dest, "restored/data")...)
			})
			if err == nil {
				t.Fatalf("receive into a dataset whose @a has another guid exited 0:\n%s", logs)
			}
			if !strings.Contains(logs, "another guid (42") || strings.Contains(logs, "already exists") {
				t.Errorf("want the guid mismatch named, not \"already exists\":\n%s", logs)
			}
			if n := rrReceives(receiveLog); n != 0 {
				t.Errorf("zfs receive ran %d times", n)
			}
		})
		t.Run(name+"/SameGUID", func(t *testing.T) {
			env, _ := setup(t)
			logs, err := guarded(t, func() (string, error) {
				return env.receive(append(mode, "tank/data@a", "file://"+env.dest, "restored/data")...)
			})
			if err != nil || !strings.Contains(logs, "already exists") {
				t.Errorf("receive of a snapshot the target has: %v\n%s", err, logs)
			}
		})
		t.Run(name+"/ManifestWithoutGUID", func(t *testing.T) {
			env, _ := setup(t)
			t.Setenv("FAKEZFS_GUIDS", "restored/data@a=42")
			reforgeManifest(t, env, "manifests|tank/data|a.manifest.gz", func(m *files.JobInfo) { m.BaseSnapshot.GUID = "" })
			logs, err := guarded(t, func() (string, error) {
				return env.receive(append(mode, "tank/data@a", "file://"+env.dest, "restored/data")...)
			})
			if err != nil || !strings.Contains(logs, "already exists") {
				t.Errorf("a manifest of an older version, with no guid, must still compare by name and time: %v\n%s", err, logs)
			}
		})
	}
}

// Finding 8: the mount guard of a --replication stream asked about the top dataset only. A child
// with a local mountpoint is in the stream too.
func TestE2EReviewReplicationMountGuardChecksChildren(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	rrAppendPool(t, env, fmt.Sprintf("tank/data@a\t%d\tsnapshot\ntank/data/home@a\t%d\tsnapshot\n", rrTa.Unix(), rrTa.Unix()))
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "--replication", "tank/data@a", dest)
	t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/data/home=local")
	receiveLog := rrReceiveLog(t)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("tank/data@a", dest, "restored/data")
	})
	if err == nil {
		t.Fatalf("a -R stream whose child tank/data/home has a local mountpoint was received without --unmounted:\n%s", logs)
	}
	if !strings.Contains(logs, "would mount restored/data/home over /tank/data/home") {
		t.Errorf("want the child named in the refusal:\n%s", logs)
	}
	if n := rrReceives(receiveLog); n != 0 {
		t.Errorf("zfs receive ran %d times", n)
	}
	logs, err = guarded(t, func() (string, error) {
		return env.receive("--unmounted", "tank/data@a", dest, "restored/data")
	})
	if err != nil {
		t.Errorf("receive --unmounted: %v\n%s", err, logs)
	}
}

// Finding 9: the mount guard looked for the source dataset only. On another host, a dataset of
// its own may be mounted at the path the stream sets.
func TestE2EReviewMountGuardRecordedMountpoint(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/srcA", []files.SnapshotInfo{{Name: "s", CreationTime: rrTa}})
	t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/srcA=local:/shared")
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "--properties", "tank/srcA@s", dest)
	manifest := "manifests|tank/srcA|s.manifest.gz"
	var recorded map[string]string
	reforgeManifest(t, env, manifest, func(m *files.JobInfo) { recorded = m.SourceMountpoints })
	if recorded["tank/srcA"] != "/shared" || len(recorded) != 1 {
		t.Errorf("the manifest records the mountpoints %q, want tank/srcA at /shared", recorded)
	}

	// The receiving host: no tank/srcA; tank/liveB is mounted at /shared.
	env.writeSnapshots(t, "tank/liveB", []files.SnapshotInfo{{Name: "x", CreationTime: rrTa}})
	t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/liveB=local:/shared")
	receiveLog := rrReceiveLog(t)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("tank/srcA@s", dest, "tank/restoredA")
	})
	if err == nil {
		t.Fatalf("a stream that mounts at /shared was received without --unmounted while tank/liveB is mounted there:\n%s", logs)
	}
	if !strings.Contains(logs, "tank/liveB is mounted there") {
		t.Errorf("want tank/liveB named in the refusal:\n%s", logs)
	}
	if n := rrReceives(receiveLog); n != 0 {
		t.Errorf("zfs receive ran %d times", n)
	}

	if logs, err = guarded(t, func() (string, error) {
		return env.receive("--unmounted", "tank/srcA@s", dest, "tank/restoredA")
	}); err != nil {
		t.Errorf("receive --unmounted: %v\n%s", err, logs)
	}

	// A manifest of an older version records no mountpoint: nothing to compare.
	reforgeManifest(t, env, manifest, func(m *files.JobInfo) { m.SourceMountpoints = nil })
	if logs, err = guarded(t, func() (string, error) {
		return env.receive("tank/srcA@s", dest, "tank/restoredB")
	}); err != nil {
		t.Errorf("receive of a manifest without SourceMountpoints: %v\n%s", err, logs)
	}
}

// Finding 10: --force had no guard beyond the same-name collision. It refuses now when zfs would
// destroy data, unless --allowDestroy is given too.
func TestE2EReviewForceRefusesDestroy(t *testing.T) {
	refused := func(t *testing.T, env *e2eEnv, receiveLog, want string, args ...string) {
		t.Helper()
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err == nil {
			t.Fatalf("receive %q exited 0:\n%s", args, logs)
		}
		if !strings.Contains(logs, want) || !strings.Contains(logs, "--allowDestroy") {
			t.Errorf("want %q and --allowDestroy in the log:\n%s", want, logs)
		}
		if n := rrReceives(receiveLog); n != 0 {
			t.Errorf("zfs receive ran %d times before the refusal", n)
		}
	}
	accepted := func(t *testing.T, env *e2eEnv, args ...string) {
		t.Helper()
		if logs, err := guarded(t, func() (string, error) { return env.receive(args...) }); err != nil {
			t.Errorf("receive %q: %v\n%s", args, err, logs)
		}
	}
	// twoSnaps sends the full a and the incremental a->b, with extra send flags, and receives a
	// into restored/data.
	twoSnaps := func(t *testing.T, extra ...string) (*e2eEnv, string) {
		env := newE2EEnv(t)
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "b", CreationTime: rrTb}, {Name: "a", CreationTime: rrTa}})
		dest := "file://" + env.dest
		env.sendOK(t, append(append([]string{"--compressor", ""}, extra...), "tank/data@a", dest)...)
		env.sendOK(t, append(append([]string{"--compressor", ""}, extra...), "-i", "a", "tank/data@b", dest)...)
		accepted(t, env, "tank/data@a", dest, "restored/data")
		return env, rrReceiveLog(t)
	}

	t.Run("FullOverDatasetWithoutSnapshots", func(t *testing.T) {
		env := newE2EEnv(t)
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: rrTa}})
		dest := "file://" + env.dest
		env.sendOK(t, "--compressor", "", "tank/data@a", dest)
		t.Setenv("FAKEZFS_DATASETS", "restored/data")
		receiveLog := rrReceiveLog(t)
		refused(t, env, receiveLog, "restored/data exists and has no snapshots", "-F", "tank/data@a", dest, "restored/data")
		accepted(t, env, "-F", "--allowDestroy", "tank/data@a", dest, "restored/data")
	})
	t.Run("NewerLocalSnapshot", func(t *testing.T) {
		env, receiveLog := twoSnaps(t)
		rrAppendPool(t, env, fmt.Sprintf("restored/data@mine\t%d\tsnapshot\n", rrTa.Add(time.Hour).Unix()))
		dest := "file://" + env.dest
		refused(t, env, receiveLog, "newer than a: restored/data@mine", "-F", "-i", "a", "tank/data@b", dest, "restored/data")
		accepted(t, env, "-F", "--allowDestroy", "-i", "a", "tank/data@b", dest, "restored/data")
	})
	t.Run("NothingToDestroy", func(t *testing.T) {
		env, _ := twoSnaps(t)
		accepted(t, env, "-F", "-i", "a", "tank/data@b", "file://"+env.dest, "restored/data")
	})
	t.Run("ReplicationIncremental", func(t *testing.T) {
		env, receiveLog := twoSnaps(t, "--replication")
		// A dataset below the target that the sender may not have had: -F may destroy it.
		rrAppendPool(t, env, fmt.Sprintf("restored/data/mine@x\t%d\tsnapshot\n", rrTa.Unix()))
		dest := "file://" + env.dest
		refused(t, env, receiveLog, "sent with --replication", "-F", "-i", "a", "tank/data@b", dest, "restored/data")
		accepted(t, env, "-F", "--allowDestroy", "-i", "a", "tank/data@b", dest, "restored/data")
	})
}

// Finding 19: a receive whose temporary file cannot be written (a full disk) downloaded the
// volume again and again for --maxRetryTime. Run with `make test-enospc`.
func TestE2EReviewReceiveENOSPCIsPermanent(t *testing.T) {
	work := enospcWork(t)
	for _, tc := range []struct {
		name  string
		bytes int
	}{
		{"close", 200 << 10}, // fits in the write buffer: only Close's flush writes the file
		{"copy", 1 << 20},    // the copy writes the file
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(tc.bytes))
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: rrTa}})
			dest := "file://" + env.dest
			env.sendOK(t, "--compressor", "", "--volsize", "10", "tank/data@a", dest)
			// The receive works in a directory whose temp/ is the small tmpfs.
			env.work = filepath.Join(work, "receive-"+tc.name)
			if err := os.MkdirAll(env.work, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(work, "temp"), filepath.Join(env.work, "temp")); err != nil {
				t.Fatal(err)
			}
			logs, err := guarded(t, func() (string, error) {
				return env.receive("--maxRetryTime", "5s", "--maxBackoffTime", "200ms", "tank/data@a", dest, "restored/data")
			})
			if err == nil {
				t.Fatalf("receive succeeded although the temp dir is too small:\n%s", logs)
			}
			if n := strings.Count(logs, "error trying to download file"); n != 1 {
				t.Errorf("the volume was downloaded %d times, want 1 (a full disk is not fixed by a retry):\n%s", n, logs)
			}
		})
	}
}
