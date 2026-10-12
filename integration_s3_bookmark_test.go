//go:build integration

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
)

// TestIntegrationS3Bookmark sends incrementals from a bookmark, once found by
// the smart options and once named with -i <dataset>#<name>, and restores the
// chain: the receiver gets snapshots the sender only has bookmarks of.
func TestIntegrationS3Bookmark(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-bm")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcbm"
	restoredBm := pool + "/restoredBm"
	destroyAfter(t, src, restoredBm)
	work := t.TempDir()
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "--volsize", "1", "--compressor", "",
	}
	send := func(args ...string) {
		t.Helper()
		if logs, err := itSend(t, work, append(args, target)...); err != nil {
			t.Fatalf("send %s: %v\n%s", strings.Join(args, " "), err, logs)
		}
	}
	wantLink := func(base, source string, bookmark bool) {
		t.Helper()
		got := manifestFor(t, work, src, target, base)
		if got.IncrementalSnapshot.Name != source || got.IncrementalSnapshot.Bookmark != bookmark {
			t.Fatalf("%s sent from %q (bookmark %v), want from %q (bookmark %v)",
				base, got.IncrementalSnapshot.Name, got.IncrementalSnapshot.Bookmark, source, bookmark)
		}
	}
	// toBookmark replaces src@name with the bookmark src#name.
	toBookmark := func(name string) {
		t.Helper()
		zfsRun(t, "bookmark", src+"@"+name, src+"#"+name)
		zfsRun(t, "destroy", src+"@"+name)
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	monthly := snapshot(t, src, "monthly")
	snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	send(append(smart, src)...)
	wantLink(monthly, "", false)

	writeFile(t, "/"+src, "b.bin", 1<<20)
	daily2 := snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	send(append(smart, src)...)
	wantLink(daily2, monthly, false)

	// The last backup's snapshot is gone, its bookmark is left: the smart
	// options send from the bookmark, not a new full.
	toBookmark(daily2)
	writeFile(t, "/"+src, "c.bin", 1<<20)
	daily3 := snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	send(append(smart, src)...)
	wantLink(daily3, daily2, true)

	// The same, named explicitly.
	toBookmark(daily3)
	writeFile(t, "/"+src, "d.bin", 1<<20)
	daily4 := snapshot(t, src, "daily")
	send("--volsize", "1", "--compressor", "", "-i", src+"#"+daily3, src+"@"+daily4)
	wantLink(daily4, daily3, true)

	// daily4 is the newest _daily and it is backed up.
	if logs, err := itSend(t, work, append(smart, src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	itReceive(t, work, "--auto", "-F", src, target, restoredBm)
	wantSnapshots(t, restoredBm, monthly, daily2, daily3, daily4)
	compareDirs(t, "/"+src, "/"+restoredBm)

	got := chainNames(itList(t, work, target)[src])
	if want := []string{monthly + "<-", daily2 + "<-" + monthly, daily3 + "<-" + daily2, daily4 + "<-" + daily3}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list holds %v, want %v", got, want)
	}
}
