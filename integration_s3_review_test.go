//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// The TestIntegrationS3Review* tests check on real zfs and MinIO the fixes of findings 7, 8, 9,
// 10, 12 and 14 of docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md. Each
// asserts the safe behavior, and where a flag overrides it, the behavior with that flag.

// reviewFlags are the send flags of the review tests: 1 MiB volumes, no compressor.
var reviewFlags = []string{"--volsize", "1", "--compressor", ""}

// reviewSend runs send with reviewFlags, then args, then target, and returns its logs. It fails
// the test on an error.
func reviewSend(t *testing.T, work, target string, args ...string) string {
	t.Helper()
	logs, err := itSend(t, work, append(append(slices.Clone(reviewFlags), args...), target)...)
	if err != nil {
		t.Fatalf("send %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
	return logs
}

// reviewReceive runs receive with an empty working directory (the manifests come from the
// destination) and returns its logs. It fails the test on an error.
func reviewReceive(t *testing.T, args ...string) string {
	t.Helper()
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), args...)
	if err != nil {
		t.Fatalf("receive %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
	return logs
}

// reviewRefused runs receive with an empty working directory and returns its logs. It fails
// the test unless the receive returns an error.
func reviewRefused(t *testing.T, args ...string) string {
	t.Helper()
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), args...)
	if err == nil {
		t.Fatalf("receive %s succeeded, want a refusal:\n%s", strings.Join(args, " "), logs)
	}
	t.Logf("receive %s: refused: %v", strings.Join(args, " "), err)
	return logs
}

// pathExists returns whether path exists.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapshotSet returns the names of the snapshots of dataset, sorted by name: for snapshots
// of one second, whose createtxg order is not known.
func snapshotSet(t *testing.T, dataset string) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(zfsRun(t, "list", "-H", "-o", "name", "-t", "snapshot", dataset)), "\n") {
		if line == "" {
			continue
		}
		names = append(names, strings.TrimPrefix(line, dataset+"@"))
	}
	slices.Sort(names)
	return names
}

// snapshotAcross takes a sanoid-style snapshot of kind of each dataset in ONE zfs snapshot call
// and returns its name. zfs takes them atomically, in one txg: the snapshots share the name and
// the creation second, and each has its own guid, as two hosts whose sanoid runs in the same
// second. As snapshot, it waits for a second after the last one.
func snapshotAcross(t *testing.T, kind string, datasets ...string) string {
	t.Helper()
	now := time.Now().UTC()
	for now.Unix() <= lastSnapshot.Unix() {
		time.Sleep(50 * time.Millisecond)
		now = time.Now().UTC()
	}
	lastSnapshot = now
	name := "autosnap_" + now.Format("2006-01-02_15:04:05") + "_" + kind
	args := []string{"snapshot"}
	for _, d := range datasets {
		args = append(args, d+"@"+name)
	}
	zfsRun(t, args...)
	created := zfsGet(t, datasets[0]+"@"+name, "creation")
	for _, d := range datasets[1:] {
		if got := zfsGet(t, d+"@"+name, "creation"); got != created {
			t.Fatalf("one zfs snapshot call gave %s@%s creation %s and %s@%s creation %s, want one second",
				datasets[0], name, created, d, name, got)
		}
	}
	return name
}

// TestIntegrationS3ReviewGUID (finding 7): two datasets hold a snapshot of one name taken in one
// second, with other data and so another guid. A backup of one, received into the other, is
// refused (explicit and --auto, with and without -F): the receive must not say "already exists"
// over another snapshot. A real received copy still gets "already exists, nothing to do".
func TestIntegrationS3ReviewGUID(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-rvguid")
	target := "s3://" + bucket + "/" + prefix
	hostA := pool + "/rvguidA"
	hostB := pool + "/rvguidB"
	copyC := pool + "/rvguidC"
	destroyAfter(t, copyC, hostB, hostA)
	work := t.TempDir()

	for _, ds := range []string{hostA, hostB} {
		zfsRun(t, "create", "-o", "mountpoint=/"+ds, ds)
	}
	writeFile(t, "/"+hostA, "a.bin", 2<<20)
	writeFile(t, "/"+hostB, "b.bin", 2<<20)
	snap := snapshotAcross(t, "monthly", hostA, hostB)
	guidA, guidB := zfsGet(t, hostA+"@"+snap, "guid"), zfsGet(t, hostB+"@"+snap, "guid")
	if guidA == guidB {
		t.Fatalf("%s@%s and %s@%s have one guid %s, want two", hostA, snap, hostB, snap, guidA)
	}
	t.Logf("%s@%s guid %s, %s@%s guid %s, creation %s", hostA, snap, guidA, hostB, snap, guidB, zfsGet(t, hostA+"@"+snap, "creation"))

	reviewSend(t, work, target, hostA+"@"+snap)
	if got := manifestFor(t, work, hostA, target, snap).BaseSnapshot.GUID; got != guidA {
		t.Fatalf("the manifest of %s@%s records guid %q, want %s", hostA, snap, got, guidA)
	}

	for _, args := range [][]string{
		{"--auto", hostA, target, hostB},
		{"--auto", "-F", hostA, target, hostB},
		{hostA + "@" + snap, target, hostB},
		{"-F", hostA + "@" + snap, target, hostB},
	} {
		logs := reviewRefused(t, args...)
		wantInLog(t, logs, "another guid")
		wantNotInLog(t, logs, "already exists, nothing to do")
	}
	wantSnapshots(t, hostB, snap)
	if got := zfsGet(t, hostB+"@"+snap, "guid"); got != guidB {
		t.Fatalf("%s@%s has guid %s after the refusals, want %s", hostB, snap, got, guidB)
	}
	if !pathExists("/"+hostB+"/b.bin") || pathExists("/"+hostB+"/a.bin") {
		t.Fatalf("%s lost its own data or got %s's", hostB, hostA)
	}

	// Control: a copy received from the backup has the backup's guid.
	reviewReceive(t, hostA+"@"+snap, target, copyC)
	wantSnapshots(t, copyC, snap)
	wantGUIDs(t, hostA, copyC, snap)
	compareDirs(t, "/"+hostA, "/"+copyC)
	for _, args := range [][]string{
		{hostA + "@" + snap, target, copyC},
		{"--auto", hostA, target, copyC},
	} {
		logs := reviewReceive(t, args...)
		wantInLog(t, logs, "already exists, nothing to do")
	}
}

// TestIntegrationS3ReviewMountGuard checks the mount guards of a receive on real zfs.
// Replication (finding 8): a -R tree whose child has a local mountpoint, mounted, received on the
// same host. Shared (finding 9): a --properties backup whose source is gone, received where
// another dataset is mounted at its recorded path. SharedReplication: the same for a -R child.
// Each is refused without --unmounted, and the live files stay visible; with --unmounted each is
// received and not mounted.
func TestIntegrationS3ReviewMountGuard(t *testing.T) {
	pool := testPool(t)

	t.Run("Replication", func(t *testing.T) {
		mnt := t.TempDir() // first: its removal runs after the destroys (cleanups are LIFO)
		_, bucket, prefix := itBucket(t, "zfsbackup-it-rvmnt8")
		target := "s3://" + bucket + "/" + prefix
		parent := pool + "/rvmnt8"
		child := parent + "/home"
		restored := pool + "/rvmnt8R"
		destroyAfter(t, restored, parent)
		// Runs before destroyAfter: a restored child mounted over the live one is moved first.
		t.Cleanup(func() { _ = exec.Command("zfs", "set", "mountpoint=none", restored+"/home").Run() })
		work := t.TempDir()
		home := filepath.Join(mnt, "home")

		zfsRun(t, "create", parent)
		zfsRun(t, "create", "-o", "mountpoint="+home, child)
		writeFile(t, "/"+parent, "p.bin", 1<<20)
		writeFile(t, home, "c.bin", 1<<20)
		snap := snapshot(t, parent, "monthly", "-r")
		live := filepath.Join(home, "live.txt")
		if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}

		reviewSend(t, work, target, "-R", parent+"@"+snap)
		if got := manifestFor(t, work, parent, target, snap).SourceMountpoints; got[child] != home {
			t.Errorf("the manifest records the mountpoints %v, want %s at %s", got, child, home)
		}

		logs := reviewRefused(t, parent+"@"+snap, target, restored)
		wantInLog(t, logs, "--unmounted", child)
		wantNoDataset(t, restored)
		if !pathExists(live) {
			t.Fatalf("%s is hidden after a refused receive", live)
		}

		reviewReceive(t, "--unmounted", parent+"@"+snap, target, restored)
		for _, ds := range []string{restored, restored + "/home"} {
			if got := zfsGet(t, ds, "mounted"); got != "no" {
				t.Errorf("%s after receive --unmounted: mounted is %s, want no", ds, got)
			}
		}
		wantGUIDs(t, parent, restored, snap)
		wantGUIDs(t, child, restored+"/home", snap)
		if !pathExists(live) {
			t.Fatalf("%s is hidden after receive --unmounted", live)
		}
		t.Logf("%s/home mountpoint %s", restored, zfsGet(t, restored+"/home", "mountpoint"))
	})

	t.Run("Shared", func(t *testing.T) {
		mnt := t.TempDir()
		_, bucket, prefix := itBucket(t, "zfsbackup-it-rvmnt9")
		target := "s3://" + bucket + "/" + prefix
		srcA := pool + "/rvshareA"
		liveB := pool + "/rvshareB"
		restored := pool + "/rvshareR"
		destroyAfter(t, restored, liveB, srcA)
		t.Cleanup(func() { _ = exec.Command("zfs", "set", "mountpoint=none", restored).Run() })
		work := t.TempDir()
		shared := filepath.Join(mnt, "shared")

		// Host A: srcA at the shared path, sent with its properties.
		zfsRun(t, "create", "-o", "mountpoint="+shared, srcA)
		writeFile(t, shared, "a.bin", 1<<20)
		snap := snapshot(t, srcA, "monthly")
		reviewSend(t, work, target, "--properties", srcA+"@"+snap)
		manifest := manifestFor(t, work, srcA, target, snap)
		if got := manifest.SourceMountpoints; len(got) != 1 || got[srcA] != shared {
			t.Fatalf("the manifest records the mountpoints %v, want only %s at %s", got, srcA, shared)
		}

		// Host B: no srcA; its own dataset at the same path.
		zfsRun(t, "destroy", "-r", srcA)
		zfsRun(t, "create", "-o", "mountpoint="+shared, liveB)
		live := filepath.Join(shared, "live.txt")
		if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}

		// -F is a rollback: it does not lift the guard.
		for _, args := range [][]string{
			{srcA + "@" + snap, target, restored},
			{"-F", srcA + "@" + snap, target, restored},
		} {
			logs := reviewRefused(t, args...)
			wantInLog(t, logs, "--unmounted", liveB)
			wantNoDataset(t, restored)
			if !pathExists(live) {
				t.Fatalf("%s is hidden after a refused receive", live)
			}
		}

		logs := reviewReceive(t, "--unmounted", srcA+"@"+snap, target, restored)
		wantInLog(t, logs, "its mountpoint from the stream is "+shared)
		wantSnapshots(t, restored, snap)
		if got := zfsGet(t, restored+"@"+snap, "guid"); got != manifest.BaseSnapshot.GUID {
			t.Errorf("%s@%s has guid %s, the backup's is %s", restored, snap, got, manifest.BaseSnapshot.GUID)
		}
		if got := zfsGet(t, restored, "mounted"); got != "no" {
			t.Errorf("%s after receive --unmounted: mounted is %s, want no", restored, got)
		}
		if got := zfsGet(t, restored, "mountpoint"); got != shared {
			t.Errorf("%s has mountpoint %s, want the stream's %s", restored, got, shared)
		}
		if !pathExists(live) {
			t.Fatalf("%s is hidden after receive --unmounted", live)
		}
	})

	t.Run("SharedReplication", func(t *testing.T) {
		mnt := t.TempDir()
		_, bucket, prefix := itBucket(t, "zfsbackup-it-rvmnt9r")
		target := "s3://" + bucket + "/" + prefix
		parent := pool + "/rvshareRA"
		child := parent + "/data"
		liveC := pool + "/rvshareRC"
		restored := pool + "/rvshareRR"
		destroyAfter(t, restored, liveC, parent)
		t.Cleanup(func() { _ = exec.Command("zfs", "set", "mountpoint=none", restored+"/data").Run() })
		work := t.TempDir()
		shared := filepath.Join(mnt, "data")

		zfsRun(t, "create", parent)
		zfsRun(t, "create", "-o", "mountpoint="+shared, child)
		writeFile(t, shared, "c.bin", 1<<20)
		snap := snapshot(t, parent, "monthly", "-r")
		reviewSend(t, work, target, "-R", parent+"@"+snap)
		if got := manifestFor(t, work, parent, target, snap).SourceMountpoints; len(got) != 1 || got[child] != shared {
			t.Fatalf("the manifest records the mountpoints %v, want only %s at %s", got, child, shared)
		}

		zfsRun(t, "destroy", "-r", parent)
		zfsRun(t, "create", "-o", "mountpoint="+shared, liveC)
		live := filepath.Join(shared, "live.txt")
		if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}

		logs := reviewRefused(t, parent+"@"+snap, target, restored)
		wantInLog(t, logs, "--unmounted", liveC)
		wantNoDataset(t, restored)
		if !pathExists(live) {
			t.Fatalf("%s is hidden after a refused receive", live)
		}

		reviewReceive(t, "--unmounted", parent+"@"+snap, target, restored)
		wantSnapshots(t, restored+"/data", snap)
		if got := zfsGet(t, restored+"/data", "mounted"); got != "no" {
			t.Errorf("%s/data after receive --unmounted: mounted is %s, want no", restored, got)
		}
		if !pathExists(live) {
			t.Fatalf("%s is hidden after receive --unmounted", live)
		}
	})
}

// TestIntegrationS3ReviewForce (finding 10): receive -F refuses what makes zfs destroy data,
// unless --allowDestroy is given too: a full into a dataset without snapshots (FullOverDataset),
// local snapshots newer than the incremental's source (NewerSnapshot), one taken in the source's
// second (SameSecondSnapshot), a -R incremental into a tree with another child
// (ReplicationExtraChild). With --allowDestroy each is received, and only then is the data lost.
// AutoReplicationChain (W2): receive --auto -F of a 3-link -R chain into a new dataset needs no
// --allowDestroy.
func TestIntegrationS3ReviewForce(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-rvforce")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/rvforce"
	destroyAfter(t, src)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 1<<20)
	m1 := snapshot(t, src, "monthly")
	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "daily")
	reviewSend(t, work, target, src+"@"+m1)
	reviewSend(t, work, target, "-i", src+"@"+m1, src+"@"+m2)

	t.Run("FullOverDataset", func(t *testing.T) {
		tgt := pool + "/rvforceA"
		destroyAfter(t, tgt)
		zfsRun(t, "create", "-o", "mountpoint=/"+tgt, tgt)
		live := "/" + tgt + "/live.txt"
		if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}

		logs := reviewRefused(t, "-F", src+"@"+m1, target, tgt)
		wantInLog(t, logs, "exists and has no snapshots", "--allowDestroy")
		wantSnapshots(t, tgt)
		if !pathExists(live) {
			t.Fatalf("%s is gone after a refused receive", live)
		}

		reviewReceive(t, "-F", "--allowDestroy", src+"@"+m1, target, tgt)
		wantSnapshots(t, tgt, m1)
		wantGUIDs(t, src, tgt, m1)
		if got := zfsGet(t, tgt, "mounted"); got != "yes" {
			t.Fatalf("%s after receive -F --allowDestroy: mounted is %s, want yes (the files cannot be checked)", tgt, got)
		}
		if pathExists(live) {
			t.Errorf("%s is still there after receive -F --allowDestroy: zfs did not replace the dataset", live)
		}
		if !pathExists("/" + tgt + "/a.bin") {
			t.Errorf("/%s/a.bin is missing after receive -F --allowDestroy", tgt)
		}
	})

	t.Run("NewerSnapshot", func(t *testing.T) {
		tgt := pool + "/rvforceB"
		destroyAfter(t, tgt)
		reviewReceive(t, src+"@"+m1, target, tgt)
		zfsRun(t, "snapshot", tgt+"@mine")

		for _, args := range [][]string{
			{"--auto", "-F", src, target, tgt},
			{"-F", "-i", src + "@" + m1, src + "@" + m2, target, tgt},
		} {
			logs := reviewRefused(t, args...)
			wantInLog(t, logs, tgt+"@mine", "--allowDestroy")
			wantSnapshots(t, tgt, m1, "mine")
		}

		reviewReceive(t, "--auto", "-F", "--allowDestroy", src, target, tgt)
		wantSnapshots(t, tgt, m1, m2)
		wantGUIDs(t, src, tgt, m1, m2)
		compareDirs(t, "/"+src, "/"+tgt)
	})

	t.Run("SameSecondSnapshot", func(t *testing.T) {
		_, bucket, prefix := itBucket(t, "zfsbackup-it-rvforcet")
		target := "s3://" + bucket + "/" + prefix
		srcT := pool + "/rvforceT"
		destroyAfter(t, srcT)

		zfsRun(t, "create", "-o", "mountpoint=/"+srcT, srcT)
		writeFile(t, "/"+srcT, "a.bin", 1<<20)
		tie := snapshotTie(t, srcT, "monthly", "hourly")
		m, h := tie[0], tie[1]
		writeFile(t, "/"+srcT, "b.bin", 1<<20)
		d := snapshot(t, srcT, "daily")
		reviewSend(t, work, target, srcT+"@"+m)
		reviewSend(t, work, target, "-i", srcT+"@"+m, srcT+"@"+d)
		guidD := zfsGet(t, srcT+"@"+d, "guid")
		t.Logf("%s@%s createtxg %s, %s@%s createtxg %s", srcT, m, zfsGet(t, srcT+"@"+m, "createtxg"), srcT, h, zfsGet(t, srcT+"@"+h, "createtxg"))

		// srcT is the target: it has m and h of one second, and not d.
		zfsRun(t, "destroy", srcT+"@"+d)
		before := snapshotSet(t, srcT)
		logs := reviewRefused(t, "-F", "-i", srcT+"@"+m, srcT+"@"+d, target, srcT)
		wantInLog(t, logs, srcT+"@"+h, "taken in the same second", "--allowDestroy")
		if got := snapshotSet(t, srcT); !slices.Equal(got, before) {
			t.Fatalf("%s has snapshots %v after a refused receive, want %v", srcT, got, before)
		}

		reviewReceive(t, "-F", "--allowDestroy", "-i", srcT+"@"+m, srcT+"@"+d, target, srcT)
		after := snapshotSet(t, srcT)
		if !slices.Contains(after, m) || !slices.Contains(after, d) {
			t.Fatalf("%s has snapshots %v after receive -F --allowDestroy, want %s and %s", srcT, after, m, d)
		}
		if got := zfsGet(t, srcT+"@"+d, "guid"); got != guidD {
			t.Errorf("%s@%s has guid %s, the backup's is %s", srcT, d, got, guidD)
		}
		t.Logf("after receive -F --allowDestroy %s has %v: %s@%s kept %v (zfs destroys it only when its txg is after %s's)",
			srcT, after, srcT, h, slices.Contains(after, h), m)
	})

	_, bucketR, prefixR := itBucket(t, "zfsbackup-it-rvforcer")
	targetR := "s3://" + bucketR + "/" + prefixR
	// No local mountpoints: the mount guard is not what these check.
	srcR := pool + "/rvforceR"
	destroyAfter(t, srcR)
	zfsRun(t, "create", srcR)
	zfsRun(t, "create", srcR+"/c")
	writeFile(t, "/"+srcR, "p.bin", 1<<20)
	writeFile(t, "/"+srcR+"/c", "c.bin", 1<<20)
	r1 := snapshot(t, srcR, "monthly", "-r")
	writeFile(t, "/"+srcR+"/c", "c2.bin", 1<<20)
	r2 := snapshot(t, srcR, "daily", "-r")
	writeFile(t, "/"+srcR, "p3.bin", 1<<20)
	r3 := snapshot(t, srcR, "daily", "-r")
	reviewSend(t, work, targetR, "-R", srcR+"@"+r1)
	reviewSend(t, work, targetR, "-R", "-i", srcR+"@"+r1, srcR+"@"+r2)
	reviewSend(t, work, targetR, "-R", "-i", srcR+"@"+r2, srcR+"@"+r3)

	t.Run("AutoReplicationChain", func(t *testing.T) {
		tgt := pool + "/rvforceW"
		destroyAfter(t, tgt)
		reviewReceive(t, "--auto", "-F", srcR, targetR, tgt)
		wantSnapshots(t, tgt, r1, r2, r3)
		wantSnapshots(t, tgt+"/c", r1, r2, r3)
		wantGUIDs(t, srcR, tgt, r1, r2, r3)
		wantGUIDs(t, srcR+"/c", tgt+"/c", r1, r2, r3)
		compareDirs(t, "/"+srcR, "/"+tgt)
	})

	t.Run("ReplicationExtraChild", func(t *testing.T) {
		tgt := pool + "/rvforceRT"
		scratch := tgt + "/scratch"
		destroyAfter(t, tgt)
		reviewReceive(t, srcR+"@"+r1, targetR, tgt)
		wantSnapshots(t, tgt+"/c", r1)
		zfsRun(t, "create", scratch)

		logs := reviewRefused(t, "-F", "-i", srcR+"@"+r1, srcR+"@"+r2, targetR, tgt)
		wantInLog(t, logs, scratch, "--replication", "--allowDestroy")
		if exec.Command("zfs", "list", scratch).Run() != nil {
			t.Fatalf("%s is gone after a refused receive", scratch)
		}
		wantSnapshots(t, tgt, r1)

		reviewReceive(t, "-F", "--allowDestroy", "-i", srcR+"@"+r1, srcR+"@"+r2, targetR, tgt)
		wantSnapshots(t, tgt, r1, r2)
		wantSnapshots(t, tgt+"/c", r1, r2)
		wantGUIDs(t, srcR+"/c", tgt+"/c", r1, r2)
		if exec.Command("zfs", "list", scratch).Run() == nil {
			t.Errorf("%s is still there after receive -F --allowDestroy of a -R incremental: zfs did not destroy it", scratch)
		}
	})
}

// TestIntegrationS3ReviewMultipartAbort (finding 12): a SIGTERM while a volume of more than 5 MiB
// goes up as a multipart upload. Afterwards the bucket holds no multipart upload of that volume:
// the canceled upload was aborted, and its parts are not left behind, billed and invisible to
// clean. --maxUploadSpeed makes the upload last seconds, so the signal lands in it.
func TestIntegrationS3ReviewMultipartAbort(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-rvmpu")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/rvmpu"
	destroyAfter(t, src)
	work := t.TempDir()
	// processFlags sets the rate limit, and nothing resets it: the next test would upload slowly.
	oldBucket := config.BackupUploadBucket
	t.Cleanup(func() { config.BackupUploadBucket = oldBucket })

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "big.bin", 64<<20)
	snap := snapshot(t, src, "monthly")
	// The default --volsize (200) and compressor: one volume of the whole 64 MiB stream.
	vol := files.JobInfo{
		VolumeName: src, BaseSnapshot: files.SnapshotInfo{Name: snap},
		Compressor: files.InternalCompressor, Separator: files.DefaultSeparator,
	}
	key := prefix + vol.BackupVolumeObjectName(1)

	ctx, stop := cmd.SignalContext(context.Background())
	defer stop()
	type result struct {
		logs string
		err  error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, logs, err := itRunCtx(ctx, cmd.ResetSendJobInfo, "send", work, "zfs",
			"--maxUploadSpeed", "10000", src+"@"+snap, target)
		done <- result{logs, err}
	}()
	t.Cleanup(func() {
		stop()
		<-finished
	})

	// Wait for a part of the volume's multipart upload.
	deadline := time.Now().Add(2 * time.Minute)
	var uploadID string
	var parts int
	for uploadID == "" {
		select {
		case r := <-done:
			t.Fatalf("send returned before a part of %s was seen (%v); the test cannot signal it:\n%s", key, r.err, r.logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no part of a multipart upload of s3://%s/%s in 2 minutes", bucket, key)
		}
		for _, id := range multipartUploads(t, client, bucket, key) {
			if n := len(uploadParts(t, client, bucket, key, id)); n > 0 {
				uploadID, parts = id, n
				break
			}
		}
		if uploadID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Logf("multipart upload %s of %s holds %d parts: SIGTERM", uploadID, key, parts)
	// One signal only: a second one exits the test binary.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err == nil {
		t.Fatalf("send stopped by SIGTERM returned no error:\n%s", r.logs)
	}
	wantInLog(t, r.logs, "Received SIGTERM; stopping.")
	t.Logf("send: %v", r.err)
	if slices.Contains(objectNames(t, client, bucket, prefix), vol.BackupVolumeObjectName(1)) {
		t.Fatalf("%s was uploaded whole before the SIGTERM took effect; slow the upload more:\n%s", key, r.logs)
	}

	// Checked at once: a process exits when send returns, so an abort sent later never happens.
	if ids := multipartUploads(t, client, bucket, key); len(ids) > 0 {
		late := "it is still there 35 s later: never aborted"
		for end := time.Now().Add(35 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
			if len(multipartUploads(t, client, bucket, key)) == 0 {
				late = "it went away later: aborted after send returned, which a real process does not wait for"
				break
			}
		}
		t.Fatalf("after the SIGTERM s3://%s/%s has multipart uploads %v (%s):\n%s", bucket, key, ids, late, r.logs)
	}
	if _, err := client.ListParts(&s3.ListPartsInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	}); err == nil {
		t.Errorf("ListParts of the canceled upload %s works after the SIGTERM: its parts are still stored", uploadID)
	} else {
		t.Logf("ListParts of the canceled upload: %v", err)
	}
	stop()
}

// multipartUploads returns the IDs of the multipart uploads of key in progress in bucket. It lists
// with the whole key as the prefix: MinIO lists the uploads of one object name only.
func multipartUploads(t *testing.T, client *s3.S3, bucket, key string) []string {
	t.Helper()
	out, err := client.ListMultipartUploads(&s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	if err != nil {
		t.Fatalf("could not list the multipart uploads of s3://%s/%s: %v", bucket, key, err)
	}
	var ids []string
	for _, u := range out.Uploads {
		if aws.StringValue(u.Key) == key {
			ids = append(ids, aws.StringValue(u.UploadId))
		}
	}
	return ids
}

// uploadParts returns the parts that the multipart upload id of key holds now.
func uploadParts(t *testing.T, client *s3.S3, bucket, key, id string) []*s3.Part {
	t.Helper()
	out, err := client.ListParts(&s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id)})
	if err != nil {
		t.Fatalf("could not list the parts of upload %s of s3://%s/%s: %v", id, bucket, key, err)
	}
	return out.Parts
}

// TestIntegrationS3ReviewBookmarkReplication (finding 14). First the zfs fact the rule rests on:
// zfs send refuses a bookmark source with -R (and -p, -I). Then smart -R sends: with @x and #x
// both on the pool (one creation time), the incremental is sent from the snapshot; an explicit
// -R -i with a bare name the same. And when the last backup's snapshot is left only as a bookmark,
// smart -R sends a full of a newer _monthly (source-pruned) with a Warning, not a failure.
func TestIntegrationS3ReviewBookmarkReplication(t *testing.T) {
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-rvbmr")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/rvbmr"
	destroyAfter(t, src)
	work := t.TempDir()
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "-R", src,
	}
	wantLink := func(base, source string) {
		t.Helper()
		got := manifestFor(t, work, src, target, base)
		if got.IncrementalSnapshot.Name != source || got.IncrementalSnapshot.Bookmark {
			t.Fatalf("%s sent from %q (bookmark %v), want from the snapshot %q",
				base, got.IncrementalSnapshot.Name, got.IncrementalSnapshot.Bookmark, source)
		}
		if !got.Replication {
			t.Fatalf("the backup of %s was not sent with -R", base)
		}
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 1<<20)
	m1 := snapshot(t, src, "monthly")
	zfsRun(t, "bookmark", src+"@"+m1, src+"#"+m1)
	if s, b := zfsGet(t, src+"@"+m1, "creation"), zfsGet(t, src+"#"+m1, "creation"); s != b {
		t.Fatalf("%s@%s has creation %s, its bookmark %s, want one", src, m1, s, b)
	}
	reviewSend(t, work, target, smart...)
	wantLink(m1, "")

	writeFile(t, "/"+src, "b.bin", 1<<20)
	d1 := snapshot(t, src, "daily")

	// (a) The zfs fact behind needsSnapshotSource: zfs send refuses a bookmark source with -R and
	// -I, and accepts one with -p and -D (zfs 2.4.4). If this changes, change the rule, do not
	// adapt the test. Every flag is tried before the test stops, so one run tells all.
	var wrong []string
	for _, c := range []struct {
		flags  []string
		refuse bool
	}{{[]string{"-R", "-i"}, true}, {[]string{"-I"}, true}, {[]string{"-p", "-i"}, false}, {[]string{"-D", "-i"}, false}} {
		args := append(append([]string{"send", "-n", "-P"}, c.flags...), src+"#"+m1, src+"@"+d1)
		out, err := exec.Command("zfs", args...).CombinedOutput()
		t.Logf("zfs %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		if (err != nil) != c.refuse {
			wrong = append(wrong, c.flags[0])
		}
	}
	// -n can pass where the real stream fails: send -p for real and drop the bytes.
	full := exec.Command("bash", "-c", "zfs send -p -i '"+src+"#"+m1+"' '"+src+"@"+d1+"' > /dev/null")
	if out, err := full.CombinedOutput(); err != nil {
		t.Logf("zfs send -p -i from the bookmark: %v: %s", err, strings.TrimSpace(string(out)))
		wrong = append(wrong, "-p (real send)")
	}
	if len(wrong) > 0 {
		t.Fatalf("zfs does not do what needsSnapshotSource expects with %v: it expects a bookmark "+
			"source refused with -R and -I, accepted with -p and -D", wrong)
	}

	// (b) @m1 and #m1 tie on creation: the smart -R incremental is sent from the snapshot. From
	// the bookmark, zfs would refuse it and the send would fail.
	reviewSend(t, work, target, smart...)
	wantLink(d1, m1)

	// The same with an explicit -R -i and a bare name, @d1 and #d1 both there.
	zfsRun(t, "bookmark", src+"@"+d1, src+"#"+d1)
	writeFile(t, "/"+src, "c.bin", 1<<20)
	d2 := snapshot(t, src, "daily")
	reviewSend(t, work, target, "-R", "-i", d1, src+"@"+d2)
	wantLink(d2, d1)

	// (c) The last backup's snapshot is left only as a bookmark: no -R incremental can come from
	// it. A full of the newer _monthly, with a Warning.
	zfsRun(t, "bookmark", src+"@"+d2, src+"#"+d2)
	zfsRun(t, "destroy", src+"@"+d2)
	writeFile(t, "/"+src, "d.bin", 1<<20)
	m2 := snapshot(t, src, "monthly")
	d3 := snapshot(t, src, "daily")
	logs := reviewSend(t, work, target, smart...)
	wantInLog(t, logs, "Only a bookmark of "+d2)
	wantLink(m2, "")
	got := chainNames(itList(t, work, target)[src])
	if want := []string{m1 + "<-", d1 + "<-" + m1, d2 + "<-" + d1, m2 + "<-"}; !slices.Equal(got, want) {
		t.Fatalf("list holds %v, want %v (no backup of %s)", got, want, d3)
	}
}
