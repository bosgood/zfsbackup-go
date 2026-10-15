package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/files"
)

// A stream sent with --properties carries the source's mountpoint. A receive of it into
// another dataset on the host that holds the source, without --unmounted, would mount the
// restored dataset over the live one: it is refused, and the error says to use --unmounted.
// With --unmounted it goes through, and the Notice says where the dataset mounts next time.
// A source whose mountpoint is not its own (inherited, so not in the stream) and a stream
// sent without properties are received as before.
func TestE2EReceivePropertiesMountGuard(t *testing.T) {
	setup := func(t *testing.T, sendArgs ...string) (*e2eEnv, string) {
		t.Helper()
		env := newE2EEnv(t)
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
			{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		})
		env.sendOK(t, append([]string{"--compressor", ""}, append(sendArgs, "tank/data@a", "file://"+env.dest)...)...)
		receiveLog := filepath.Join(t.TempDir(), "receive.log")
		t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
		return env, receiveLog
	}
	received := func(t *testing.T, receiveLog, logs string) {
		t.Helper()
		if data, err := os.ReadFile(receiveLog); err != nil || len(data) == 0 {
			t.Errorf("zfs receive did not run (%v)\n%s", err, logs)
		}
	}

	t.Run("refused", func(t *testing.T) {
		env, receiveLog := setup(t, "--properties")
		t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/data=local")
		args := []string{"--unmounted=false", "tank/data@a", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err == nil {
			t.Fatalf("receive of a properties stream over the live source succeeded\n%s", logs)
		}
		for _, want := range []string{"--unmounted", "/tank/data", "zfs set mountpoint="} {
			if !strings.Contains(logs, want) {
				t.Errorf("want the log to say %q\n%s", want, logs)
			}
		}
		noReceive(t, receiveLog, logs)
	})

	t.Run("unmounted", func(t *testing.T) {
		env, receiveLog := setup(t, "--properties")
		t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/data=local,restored/data=received")
		args := []string{"--unmounted", "tank/data@a", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err != nil {
			t.Fatalf("receive --unmounted: %v\n%s", err, logs)
		}
		received(t, receiveLog, logs)
		if !strings.Contains(logs, "zfs set mountpoint=<path> restored/data") {
			t.Errorf("want the notice that says where restored/data mounts next time\n%s", logs)
		}
	})

	t.Run("inherited mountpoint", func(t *testing.T) {
		env, receiveLog := setup(t, "--properties")
		args := []string{"--unmounted=false", "tank/data@a", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err != nil {
			t.Fatalf("receive: %v\n%s", err, logs)
		}
		received(t, receiveLog, logs)
	})

	t.Run("no properties", func(t *testing.T) {
		env, receiveLog := setup(t)
		t.Setenv("FAKEZFS_MOUNTPOINTS", "tank/data=local")
		args := []string{"--unmounted=false", "tank/data@a", "file://" + env.dest, "restored/data"}
		logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
		if err != nil {
			t.Fatalf("receive: %v\n%s", err, logs)
		}
		received(t, receiveLog, logs)
	})
}
