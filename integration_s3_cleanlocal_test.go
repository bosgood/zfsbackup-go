//go:build integration

package main

import (
	"context"
	"crypto/md5" //nolint:gosec // the lock name, as volumeLock makes it
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/backup"
)

// TestIntegrationS3CleanLocal runs clean against the cached partial manifest
// of a cut send: without --cleanLocal and with --force it stays and protects
// its volumes, a busy dataset keeps it under --cleanLocal too, and --cleanLocal
// deletes it and its volumes. A partial whose volume is gone gets the Notice
// that --force leaves it alone. Subtests: Repair (a broken set at one of two
// destinations, repaired with clean --force and a send) and Denied (a user who
// may not delete).
func TestIntegrationS3CleanLocal(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-cl")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srccl"
	src2 := pool + "/srccl2"
	restoredCl := pool + "/restoredCl"
	destroyAfter(t, src, src2, restoredCl)
	work := t.TempDir()
	send := func(args ...string) {
		t.Helper()
		args = append([]string{"--volsize", "1", "--compressor", ""}, append(args, target)...)
		if logs, err := itSend(t, work, args...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}
	clean := func(args ...string) string {
		t.Helper()
		logs, err := itClean(t, work, append(args, target)...)
		if err != nil {
			t.Fatalf("clean %v: %v\n%s", args, err, logs)
		}
		return logs
	}
	wantObjects := func(want []string) {
		t.Helper()
		wantObjectsAt(t, client, bucket, prefix, want)
	}
	wantCache := func(want []string) {
		t.Helper()
		if got := cachedFiles(t, work); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("cached manifests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "first.bin", 64<<10)
	first := snapshot(t, src, "first")
	send(src + "@" + first)
	zfsRun(t, "create", "-o", "mountpoint=/"+src2, src2)
	writeFile(t, "/"+src2, "first.bin", 64<<10)
	first2 := snapshot(t, src2, "first")
	send(src2 + "@" + first2)
	healthy := objectNames(t, client, bucket, prefix)

	writeFile(t, "/"+src, "a.bin", 4<<20)
	daily := snapshot(t, src, "daily")
	fulls := cachedFiles(t, work)
	partial := cutSend(t, client, bucket, prefix, work, src, daily, 2)
	partialPath := newCachedFile(t, fulls, cachedFiles(t, work))
	cut := objectNames(t, client, bucket, prefix)
	withPartial := cachedFiles(t, work)

	// 1. No flags: the partial is live, and clean says how to delete it.
	wantInLog(t, clean(), "There are 1 local manifests not found in the destination, use --cleanLocal")
	wantObjects(cut)
	wantCache(withPartial)

	// 2. --force is about broken sets at the destination: the partial stays.
	logs := clean("--force")
	wantInLog(t, logs, "There are 1 local manifests not found in the destination, use --cleanLocal")
	wantNotInLog(t, logs, "Removing entire backupset")
	wantObjects(cut)
	wantCache(withPartial)

	// 3. A send of src runs (its lock holds a live pid): --cleanLocal leaves the
	// partial and its volumes alone, and still deletes the orphan of src2.
	lock := lockPath(t, work, src)
	holder := exec.Command("sleep", "600") // a live pid the test owns, whatever the container's process tree
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	if err := os.WriteFile(lock, []byte(fmt.Sprintf("%d\n", holder.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })
	orphan2 := orphanOf(t, client, bucket, prefix, manifestFor(t, work, src2, target, first2).Volumes[0].ObjectName, 77)
	wantInLog(t, clean("--cleanLocal"), "A send of "+src+" appears to be running")
	if slices.Contains(objectNames(t, client, bucket, prefix), orphan2) {
		t.Errorf("clean --cleanLocal kept %s, an orphan of %s, whose dataset is not busy", orphan2, src2)
	}
	wantObjects(cut)
	wantCache(withPartial)
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	// 4. --cleanLocal --dry-run names what it would delete and deletes nothing.
	logs = clean("--cleanLocal", "--dry-run")
	wantInLog(t, logs, "Would delete local manifest "+partialPath+".", "Dry-run: would delete 2 objects")
	for _, v := range partial {
		wantInLog(t, logs, "Would delete "+strings.TrimSuffix(target, "/")+"/"+v+".")
	}
	wantObjects(cut)
	wantCache(withPartial)

	// 5. --cleanLocal: the partial's volumes, then its cached manifest.
	clean("--cleanLocal")
	wantObjects(healthy)
	wantCache(fulls)
	requireVolumes(t, manifestFor(t, work, src, target, first), healthy)
	requireVolumes(t, manifestFor(t, work, src2, target, first2), healthy)

	// 6. Nothing left to delete, and the incremental the cut send was goes again.
	wantInLog(t, clean("--dry-run"), "Dry-run: would delete 0 objects")
	send("-i", src+"@"+first, src+"@"+daily)
	if got, want := chainNames(itList(t, work, target)[src]), []string{first + "<-", daily + "<-" + first}; !slices.Equal(got, want) {
		t.Fatalf("list after the send holds %v, want %v", got, want)
	}
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restoredCl)
	wantSnapshots(t, restoredCl, first, daily)
	wantGUIDs(t, src, restoredCl, first, daily)
	compareDirs(t, "/"+src, "/"+restoredCl)
	complete := objectNames(t, client, bucket, prefix)

	// 7. A partial that lists a volume the destination lost: --force leaves it
	// alone and says so; --cleanLocal deletes it and the volume that is left.
	writeFile(t, "/"+src, "b.bin", 4<<20)
	weekly := snapshot(t, src, "weekly")
	before := cachedFiles(t, work)
	partial2 := cutSend(t, client, bucket, prefix, work, src, weekly, 2)
	partial2Path := newCachedFile(t, before, cachedFiles(t, work))
	deleteObject(t, client, bucket, prefix+partial2[1])
	wantInLog(t, clean("--force"),
		"The local manifest "+partial2Path+" is missing volume "+partial2[1],
		"--force leaves it alone, --cleanLocal deletes it")
	if !slices.Contains(objectNames(t, client, bucket, prefix), partial2[0]) {
		t.Fatalf("clean --force deleted %s, a volume of the partial it must leave alone", partial2[0])
	}
	if !slices.Contains(cachedFiles(t, work), partial2Path) {
		t.Fatalf("clean --force deleted the cached partial %s", partial2Path)
	}
	clean("--cleanLocal")
	wantObjects(complete)
	wantCache(before)
	requireVolumes(t, manifestFor(t, work, src, target, first), complete)
	requireVolumes(t, manifestFor(t, work, src, target, daily), complete)

	t.Run("Repair", func(t *testing.T) { cleanLocalRepair(t, pool) })
	t.Run("Denied", func(t *testing.T) { cleanLocalDenied(t, pool) })
}

// cleanLocalRepair: a volume of the newest set is lost at d2 of two
// destinations. clean --force at d2 removes the broken set there and nothing
// at d1; a send of that set to d2 alone makes the two equal again, the next
// smart send is a noop, and d2 restores on its own.
func cleanLocalRepair(t *testing.T, pool string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clrep")
	p1, p2 := prefix+"d1/", prefix+"d2/"
	d1, d2 := "s3://"+bucket+"/"+p1, "s3://"+bucket+"/"+p2
	src := pool + "/srcclrep"
	restored := pool + "/restoredClRep"
	destroyAfter(t, src, restored)
	work := t.TempDir()
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_monthly", "--volsize", "1", "--compressor", "",
	}
	send := func(args ...string) {
		t.Helper()
		if logs, err := itSend(t, work, args...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}
	wantSame := func() []string {
		t.Helper()
		n1, n2 := objectNames(t, client, bucket, p1), objectNames(t, client, bucket, p2)
		if strings.Join(n1, " ") != strings.Join(n2, " ") {
			t.Fatalf("the destinations differ:\nd1: %v\nd2: %v", n1, n2)
		}
		return n1
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	send(append(smart, src, d1+","+d2)...)
	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "monthly")
	send(append(smart, src, d1+","+d2)...)
	atD1 := wantSame()

	// A broken set: its manifest stays, a volume is gone.
	m2Set := manifestFor(t, work, src, d1, m2)
	deleteObject(t, client, bucket, p2+m2Set.Volumes[0].ObjectName)

	logs, err := itClean(t, work, "--force", d2)
	if err != nil {
		t.Fatalf("clean --force %s: %v\n%s", d2, err, logs)
	}
	wantInLog(t, logs, "Removing entire backupset")
	for _, n := range objectNames(t, client, bucket, p2) {
		if strings.Contains(n, m2) {
			t.Errorf("after clean --force d2 holds %s, an object of the broken set", n)
		}
	}
	wantObjectsAt(t, client, bucket, p1, atD1)
	// list reads a cached name from the cache: an m2 here means its copy stayed.
	if got := chainNames(itList(t, work, d2)[src]); !slices.Equal(got, []string{m1 + "<-"}) {
		t.Fatalf("list %s after clean --force holds %v, want %s<- only", d2, got, m1)
	}

	send("--volsize", "1", "--compressor", "", "-i", src+"@"+m1, src+"@"+m2, d2)
	wantSame()
	if logs, err := itSend(t, work, append(smart, src, d1+","+d2)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("smart send after the repair: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	itReceive(t, t.TempDir(), "--auto", "-F", src, d2, restored)
	wantSnapshots(t, restored, m1, m2)
	wantGUIDs(t, src, restored, m1, m2)
	compareDirs(t, "/"+src, "/"+restored)
}

// cleanLocalDenied: a user who may list and read but not delete runs clean
// --cleanLocal on a cut send. It fails with the server's AccessDenied and
// changes nothing, in the bucket or in the cache; a user who may delete then
// finishes the job. It needs the MinIO user of integration/compose.yaml.
func cleanLocalDenied(t *testing.T, pool string) {
	key, secret := os.Getenv("ZFSBACKUP_IT_NODELETE_KEY"), os.Getenv("ZFSBACKUP_IT_NODELETE_SECRET")
	if key == "" || secret == "" {
		t.Skip("ZFSBACKUP_IT_NODELETE_KEY and ZFSBACKUP_IT_NODELETE_SECRET are unset: no user without s3:DeleteObject")
	}
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clden")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcclden"
	restored := pool + "/restoredClDen"
	destroyAfter(t, src, restored)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "first.bin", 64<<10)
	first := snapshot(t, src, "first")
	if logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", src+"@"+first, target); err != nil {
		t.Fatalf("send %s: %v\n%s", first, err, logs)
	}
	writeFile(t, "/"+src, "a.bin", 4<<20)
	daily := snapshot(t, src, "daily")
	partial := cutSend(t, client, bucket, prefix, work, src, daily, 2)
	before := objectNames(t, client, bucket, prefix)
	files := cachedFiles(t, work)

	// In a subtest, so that t.Setenv gives the root credentials back on return.
	t.Run("as nodelete", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", key)
		t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
		// clean retries a denied delete for 10 minutes; the deadline ends that.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		logs, err := itCleanCtx(ctx, t, work, "--cleanLocal", target)
		if err == nil {
			t.Fatalf("clean --cleanLocal as a user who may not delete succeeded:\n%s", logs)
		}
		t.Logf("clean as nodelete: %v", err)
		wantInLog(t, logs, "Could not delete object", "AccessDenied")
	})
	wantObjectsAt(t, client, bucket, prefix, before)
	if got := cachedFiles(t, work); !slices.Equal(got, files) {
		t.Fatalf("a failed clean changed the cache: %v, want %v", got, files)
	}

	if logs, err := itClean(t, work, "--cleanLocal", target); err != nil {
		t.Fatalf("clean --cleanLocal: %v\n%s", err, logs)
	}
	after := objectNames(t, client, bucket, prefix)
	for _, v := range partial {
		if slices.Contains(after, v) {
			t.Errorf("clean --cleanLocal kept %s, a volume of the partial", v)
		}
	}
	if got := cachedFiles(t, work); len(got) != len(files)-1 {
		t.Errorf("clean --cleanLocal left the cache %v, want the partial gone from %v", got, files)
	}
	requireVolumes(t, manifestFor(t, work, src, target, first), after)
	itReceive(t, t.TempDir(), "-F", src+"@"+first, target, restored)
	wantGUIDs(t, src, restored, first)
}

// newCachedFile returns the one path in after that is not in before, and fails
// unless there is exactly one.
func newCachedFile(t *testing.T, before, after []string) string {
	t.Helper()
	added := slices.DeleteFunc(slices.Clone(after), func(p string) bool { return slices.Contains(before, p) })
	if len(added) != 1 {
		t.Fatalf("the cut send added %v to the cache, want one partial manifest", added)
	}
	return added[0]
}

// lockPath returns the send lock of dataset in work, as volumeLock names it,
// and makes its directory.
func lockPath(t *testing.T, work, dataset string) string {
	t.Helper()
	dir := filepath.Join(work, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, fmt.Sprintf("%x.lck", md5.Sum([]byte(dataset)))) //nolint:gosec // see the import
}
