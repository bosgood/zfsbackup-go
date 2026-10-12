//go:build integration

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3Rollover lets the --fullIfOlderThan window elapse for real:
// one send with a 1s window among sends with 4320h. The new chain restores,
// the old chain still restores, a receiver on the old chain cannot take the
// new one, and clean deletes the old chain once its manifests are gone.
func TestIntegrationS3Rollover(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-roll")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcroll"
	restoredNew := pool + "/restoredNew"
	restoredOld := pool + "/restoredOld"
	restoredNew2 := pool + "/restoredNew2"
	m2Clone := pool + "/srcrollm2"
	// The clone first: src cannot go while a clone of its snapshot exists.
	destroyAfter(t, m2Clone, src, restoredNew, restoredOld, restoredNew2)
	work := t.TempDir()
	base := []string{
		"--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly",
		"--volsize", "1", "--compressor", "",
	}
	const long, short = "4320h", "1s"
	send := func(window string) string {
		t.Helper()
		logs, err := itSend(t, work, append(base, "--fullIfOlderThan", window, src, target)...)
		if err != nil {
			t.Fatalf("send --fullIfOlderThan %s: %v\n%s", window, err, logs)
		}
		return logs
	}
	wantLink := func(snap, source string) {
		t.Helper()
		if got := manifestFor(t, work, src, target, snap).IncrementalSnapshot.Name; got != source {
			t.Fatalf("%s sent from %q, want from %q", snap, got, source)
		}
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	snapshot(t, src, "hourly")
	send(long)
	wantLink(m1, "")

	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "monthly")
	snapshot(t, src, "hourly")
	send(long)
	wantLink(m2, m1)
	m2Path := cloneOf(t, pool, src, m2, "srcrollm2")

	// The window elapses: the newest snapshot is more than 1s newer than m1.
	writeFile(t, "/"+src, "c.bin", 1<<20)
	m3 := snapshot(t, src, "monthly")
	snapshot(t, src, "hourly")
	if logs := send(short); !strings.Contains(logs, "window-elapsed") {
		t.Errorf("want window-elapsed in the log:\n%s", logs)
	}
	wantLink(m3, "")

	writeFile(t, "/"+src, "d.bin", 1<<20)
	m4 := snapshot(t, src, "monthly")
	snapshot(t, src, "hourly")
	send(long)
	wantLink(m4, m3)

	got := chainNames(itList(t, work, target)[src])
	if want := []string{m1 + "<-", m2 + "<-" + m1, m3 + "<-", m4 + "<-" + m3}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list holds %v, want %v", got, want)
	}

	// R1: into nothing, --auto takes the new chain.
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restoredNew)
	wantSnapshots(t, restoredNew, m3, m4)
	wantGUIDs(t, src, restoredNew, m3, m4)
	compareDirs(t, "/"+src, "/"+restoredNew)

	// R2: the old chain still restores.
	itReceive(t, t.TempDir(), "--auto", "-F", src+"@"+m2, target, restoredOld)
	wantSnapshots(t, restoredOld, m1, m2)
	wantGUIDs(t, src, restoredOld, m1, m2)
	compareDirs(t, m2Path, "/"+restoredOld)

	// R3: a receiver on the old chain. No incremental reaches m1 or m2, so the
	// full of m3 is next, and zfs refuses a full into a dataset with snapshots.
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "--auto", "-F", src+"@"+m4, target, restoredOld)
	if err == nil {
		t.Fatalf("receive of the new chain onto the old one succeeded, want an error:\n%s", logs)
	}
	wantSnapshots(t, restoredOld, m1, m2)
	if !strings.Contains(logs, "destination has snapshots") {
		t.Errorf("want zfs's \"destination has snapshots\" in the log:\n%s", logs)
	}
	t.Logf("R3: receive onto the old chain: %v: %s", err, zfsLine(logs, "destination has snapshots"))

	if logs, err = itSend(t, work, append(base, "--fullIfOlderThan", long, src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	// The old chain is retired: its manifests go, clean takes its volumes.
	// A fresh work: the cache of work still holds the manifests, and clean
	// treats a cached copy as live.
	oldVolumes := len(manifestFor(t, work, src, target, m1).Volumes) + len(manifestFor(t, work, src, target, m2).Volumes)
	var kept []string
	for _, n := range objectNames(t, client, bucket, prefix) {
		switch {
		case strings.Contains(n, ".manifest") && (strings.Contains(n, m1) || strings.Contains(n, m2)):
			deleteObject(t, client, bucket, prefix+n)
		case !strings.Contains(n, m1) && !strings.Contains(n, m2):
			kept = append(kept, n)
		}
	}
	logs, err = itClean(t, t.TempDir(), "--dry-run", target)
	if want := fmt.Sprintf("Dry-run: would delete %d objects", oldVolumes); err != nil || !strings.Contains(logs, want) {
		t.Fatalf("clean --dry-run: %v, want %q in the log:\n%s", err, want, logs)
	}
	if logs, err = itClean(t, t.TempDir(), target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	if got := objectNames(t, client, bucket, prefix); strings.Join(got, " ") != strings.Join(kept, " ") {
		t.Fatalf("after clean %s holds %v, want the m3 and m4 sets %v", target, got, kept)
	}
	fresh := t.TempDir()
	for _, snap := range []string{m3, m4} {
		requireVolumes(t, manifestFor(t, fresh, src, target, snap), kept)
	}
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restoredNew2)
	wantSnapshots(t, restoredNew2, m3, m4)
	wantGUIDs(t, src, restoredNew2, m3, m4)
	compareDirs(t, "/"+src, "/"+restoredNew2)
}
