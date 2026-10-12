package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// An explicit receive of an incremental with --origin clones the origin snapshot of another
// dataset and applies the stream to the clone: the origin is the source then, and the target
// need not exist. Without --origin, a target that lacks the source is refused before zfs
// receive runs. An origin that is not there is an error.
func TestE2EReceiveIncrementalWithOrigin(t *testing.T) {
	setup := func(t *testing.T) (*e2eEnv, string) {
		t.Helper()
		env := newE2EEnv(t)
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
			{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
			{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		})
		dest := "file://" + env.dest
		env.sendOK(t, "--compressor", "", "tank/data@a", dest)
		env.sendOK(t, "--compressor", "", "-i", "a", "tank/data@b", dest)
		receiveLog := filepath.Join(t.TempDir(), "receive.log")
		t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
		return env, receiveLog
	}

	t.Run("origin", func(t *testing.T) {
		env, receiveLog := setup(t)
		args := []string{"--origin", "tank/data@a", "-i", "a", "tank/data@b", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err != nil {
			t.Fatalf("receive --origin: %v\n%s", err, logs)
		}
		if data, rerr := os.ReadFile(receiveLog); rerr != nil || len(data) == 0 {
			t.Errorf("zfs receive did not run (%v)\n%s", rerr, logs)
		}
		pool, rerr := os.ReadFile(env.snapshots)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !strings.Contains(string(pool), "restored/data@b\t") {
			t.Errorf("restored/data@b is not in the pool after the receive:\n%s", pool)
		}
	})

	t.Run("no origin", func(t *testing.T) {
		env, receiveLog := setup(t)
		args := []string{"-i", "a", "tank/data@b", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err == nil {
			t.Fatalf("receive of an incremental into a dataset without its source succeeded\n%s", logs)
		}
		noReceive(t, receiveLog, logs)
	})

	t.Run("missing origin", func(t *testing.T) {
		env, receiveLog := setup(t)
		args := []string{"--origin", "tank/data@nope", "-i", "a", "tank/data@b", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err == nil {
			t.Fatalf("receive --origin of a snapshot that is not there succeeded\n%s", logs)
		}
		if !strings.Contains(logs, "tank/data@nope") {
			t.Errorf("want the log to name the origin\n%s", logs)
		}
		noReceive(t, receiveLog, logs)
	})
}
