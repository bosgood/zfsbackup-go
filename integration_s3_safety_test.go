//go:build integration

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3ReceiveSafety receives without -F onto a changed dataset,
// receives a stream sent with --properties, with and without --unmounted,
// and receives an incremental as a clone with --origin.
func TestIntegrationS3ReceiveSafety(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-safe")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcsafe"
	restoredS := pool + "/restoredS"
	restoredP := pool + "/restoredP"
	restoredP2 := pool + "/restoredP2"
	restoredO := pool + "/restoredO"
	m2Clone := pool + "/srcsafem2"
	// Clones before their origin; restoredP2 may be mounted over src.
	destroyAfter(t, restoredO, m2Clone, restoredP2, restoredP, restoredS, src)
	// Runs before destroyAfter (cleanups are LIFO): the received mountpoint is src's.
	t.Cleanup(func() {
		for _, d := range []string{restoredP2, restoredP} {
			_ = exec.Command("zfs", "set", "mountpoint=none", d).Run()
		}
	})
	work := t.TempDir()
	send := func(target string, args ...string) {
		t.Helper()
		if logs, err := itSend(t, work, append(append([]string{"--volsize", "1", "--compressor", ""}, args...), target)...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "daily")
	send(target, src+"@"+m1)
	send(target, "-i", src+"@"+m1, src+"@"+m2)

	// S1: without -F zfs refuses a dataset changed since its last snapshot.
	itReceive(t, t.TempDir(), "--force=false", src+"@"+m1, target, restoredS)
	local := "/" + restoredS + "/local.txt"
	if err := os.WriteFile(local, []byte("local change"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "--force=false", "-i", src+"@"+m1, src+"@"+m2, target, restoredS)
	if err == nil {
		t.Fatalf("receive without -F onto a changed dataset succeeded, want an error:\n%s", logs)
	}
	t.Logf("S1: receive without -F: %v: %s", err, zfsLine(logs, "cannot receive"))
	wantSnapshots(t, restoredS, m1)
	if !exists(local) {
		t.Fatalf("a failed receive without -F removed %s", local)
	}
	// -F rolls the change back: that is what it costs.
	itReceive(t, t.TempDir(), "-F", "-i", src+"@"+m1, src+"@"+m2, target, restoredS)
	wantSnapshots(t, restoredS, m1, m2)
	wantGUIDs(t, src, restoredS, m1, m2)
	if exists(local) {
		t.Fatalf("%s is still there after receive -F, want it rolled back", local)
	}

	// S2: --properties carries src's local mountpoint.
	_, bucket2, prefix2 := itBucket(t, "zfsbackup-it-safe2")
	target2 := "s3://" + bucket2 + "/" + prefix2
	send(target2, "--properties", src+"@"+m1)
	after := "/" + src + "/after.bin"
	writeFile(t, "/"+src, "after.bin", 64<<10)

	itReceive(t, t.TempDir(), "--unmounted", "-F", src+"@"+m1, target2, restoredP)
	if got := zfsGet(t, restoredP, "mounted"); got != "no" {
		t.Errorf("%s with --unmounted: mounted is %s, want no", restoredP, got)
	}
	if !exists(after) {
		t.Errorf("%s is hidden after receive --unmounted", after)
	}
	t.Logf("S2: %s mountpoint %s (source %s)", restoredP,
		zfsGet(t, restoredP, "mountpoint"), strings.TrimSpace(zfsRun(t, "get", "-H", "-o", "source", "mountpoint", restoredP)))

	t.Run("Mounted", func(t *testing.T) {
		logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "--unmounted=false", "-F", src+"@"+m1, target2, restoredP2)
		t.Logf("S2 Mounted: receive without --unmounted: %v", err)
		if exec.Command("zfs", "list", restoredP2).Run() == nil {
			t.Logf("S2 Mounted: %s mountpoint %s, mounted %s", restoredP2, zfsGet(t, restoredP2, "mountpoint"), zfsGet(t, restoredP2, "mounted"))
		} else {
			t.Logf("S2 Mounted: no dataset %s:\n%s", restoredP2, logs)
		}
		if !exists(after) {
			t.Errorf("%s is hidden: the received dataset mounted over the live one", after)
		}
	})

	// S3: --origin receives the incremental m1 -> m2 as a clone of src@m1.
	t.Run("Origin", func(t *testing.T) {
		itReceive(t, t.TempDir(), "--origin", src+"@"+m1, "-F", "-i", src+"@"+m1, src+"@"+m2, target, restoredO)
		if got := zfsGet(t, restoredO, "origin"); got != src+"@"+m1 {
			t.Errorf("%s has origin %s, want %s@%s", restoredO, got, src, m1)
		}
		wantSnapshots(t, restoredO, m2)
		wantGUIDs(t, src, restoredO, m2)
		compareDirs(t, cloneOf(t, pool, src, m2, "srcsafem2"), "/"+restoredO)
	})
}
