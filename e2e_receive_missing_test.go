package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// An explicit receive names a backup set with its flags: -i <source> for an incremental, none
// for a full. When the destination has no manifest of that set, the error says which backups
// of the snapshot it does hold and the flags that name each, or that it holds none.
func TestE2EReceiveMissingManifestSaysWhatIsThere(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "c", CreationTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "tank/data@a", dest)
	env.sendOK(t, "--compressor", "", "-i", "a", "tank/data@b", dest)
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)

	for _, c := range []struct {
		name string
		args []string
		want []string
	}{
		{"incremental without -i", []string{"tank/data@b"}, []string{"No full backup of tank/data@b at ", "It has an incremental backup from a (run again with -i a).", "receive --auto"}},
		{"full with -i", []string{"-i", "a", "tank/data@a"}, []string{"No incremental backup from a of tank/data@a at ", "It has a full backup (run again without -i)."}},
		{"never backed up", []string{"tank/data@c"}, []string{"No full backup of tank/data@c at ", "and no other backup of it: zfsbackup list "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string(nil), c.args...), dest, "restored/data")
			logs, err := guarded(t, func() (string, error) { return env.receive(args...) })
			if err == nil {
				t.Fatalf("receive of a set that is not there succeeded\n%s", logs)
			}
			for _, want := range c.want {
				if !strings.Contains(logs, want) {
					t.Errorf("want the log to say %q\n%s", want, logs)
				}
			}
			noReceive(t, receiveLog, logs)
		})
	}
}
