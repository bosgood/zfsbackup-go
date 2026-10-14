//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationFileFull sends to a file:// dataset that fills up (refquota)
// in the middle of the send, and to one that is read-only.
func TestIntegrationFileFull(t *testing.T) {
	pool := testPool(t)
	testFullResume(t, pool, "ffull", newFileDest)
	t.Run("ReadOnly", func(t *testing.T) { testReadOnlyFile(t, pool) })
}

// testFullResume sends 20 MiB in 4 MiB volumes, one upload at a time, to a
// destination with a 6 MiB refquota: volume 1 fits, volume 2 fails in the
// middle of its write (not at its open, as a volume after it would). The send
// fails, and what it leaves there is only volume 1, whole: the cut volume 2
// is removed. With the quota lifted, --resume completes from there and the
// set restores. One upload at a time: parallel uploads that fail leave a gap
// in the cache, and --resume re-sends from the gap.
func testFullResume(t *testing.T, pool, name string, mk destMaker) {
	t.Helper()
	src := pool + "/" + name + "src"
	newSource(t, src, 20<<20)
	snap := snapshot(t, src, "daily")
	destDataset := pool + "/" + name + "dest"
	dest := mk(t, pool, name+"dest", "refquota=6M")
	work := t.TempDir()
	args := append([]string{"--volsize", "4", "--compressor", "", "--maxParallelUploads", "1"},
		append(slices.Clone(sshRetry), "--full", src, dest.target)...)

	_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, "zfs", args...)
	if err == nil {
		t.Fatalf("send of 20 MiB into a 6 MiB refquota: no error\n%s", logs)
	}
	t.Logf("send into the full destination: %v", err)
	t.Logf("first upload error of the log: %s", zfsLine(logs, "Error while uploading"))
	partial := cachedPartial(t, work, snap)
	if len(partial.Volumes) != 1 {
		t.Fatalf("the cached partial manifest of %s lists %v, want volume 1 only:\n%s", snap, volumeNames(partial), logs)
	}
	// Each file there is a whole volume the cache lists; no manifest.
	sizes := map[string]uint64{}
	for _, v := range partial.Volumes {
		sizes[v.ObjectName] = v.Size
	}
	for _, n := range fileNames(t, dest.dir) {
		fi, serr := os.Stat(filepath.Join(dest.dir, n))
		if serr != nil {
			t.Fatal(serr)
		}
		want, listed := sizes[n]
		switch {
		case !listed:
			t.Errorf("%s holds %s (%d bytes), which the cached partial manifest does not list: a cut volume stayed", dest.dir, n, fi.Size())
		case uint64(fi.Size()) != want:
			t.Errorf("%s holds %s at %d bytes, the cached partial manifest says %d", dest.dir, n, fi.Size(), want)
		}
	}
	wantNotInLog(t, logs, "Error deleting failed upload file")
	if t.Failed() {
		t.Fatalf("send log:\n%s", logs)
	}

	zfsRun(t, "set", "refquota=none", destDataset)
	_, logs, err = itRunOut(cmd.ResetSendJobInfo, "send", work, "zfs", append([]string{"--resume"}, args...)...)
	if err != nil {
		t.Fatalf("send --resume after the quota is lifted: %v\n%s", err, logs)
	}
	wantNotInLog(t, logs, "it and later volumes will be re-sent", "Error deleting failed upload file")
	m := manifestFor(t, work, src, dest.target, snap)
	wantVolumeSizes(t, dest.dir, m)

	restored := pool + "/" + name + "Restored"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "-F", src+"@"+snap, dest.target, restored)
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)
}

// testReadOnlyFile: a file:// dataset set readonly=on after one backup. A new
// send fails and writes nothing; list, receive and clean --dry-run work; a
// real clean cannot delete the orphan, which stays.
func testReadOnlyFile(t *testing.T, pool string) {
	destDataset, dir := itDestDataset(t, pool, "frodest")
	target := itFileTarget(dir)
	// Registered after itDestDataset, so it runs before the destroy (t.Cleanup is LIFO).
	t.Cleanup(func() { zfsRun(t, "set", "readonly=off", destDataset) })
	src := pool + "/frosrc"
	newSource(t, src, 1<<20)
	s1 := snapshot(t, src, "daily")
	work := t.TempDir()
	mustSend(t, work, append(slices.Clone(plainSend), src+"@"+s1, target)...)
	m := manifestFor(t, work, src, target, s1)
	orphan := strings.TrimSuffix(m.Volumes[0].ObjectName, "vol1") + "vol99"
	if err := os.WriteFile(filepath.Join(dir, orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileNames(t, dir)
	zfsRun(t, "set", "readonly=on", destDataset)

	writeFile(t, "/"+src, "b.bin", 1<<20)
	s2 := snapshot(t, src, "daily")
	start := time.Now()
	logs, err := itSend(t, work, append(slices.Clone(plainSend), append(slices.Clone(sshRetry), "-i", src+"@"+s1, src+"@"+s2, target)...)...)
	if err == nil {
		t.Fatalf("send to a read-only dataset: no error\n%s", logs)
	}
	t.Logf("send to the read-only dataset after %v: %v", time.Since(start).Round(time.Second), err)
	t.Logf("first read-only line of the log: %s", zfsLine(logs, "read-only"))
	wantFilesAt(t, dir, before)

	if backups := itList(t, t.TempDir(), target)[src]; len(backups) != 1 {
		t.Fatalf("list of the read-only dataset: %v, want the backup of %s", chainNames(backups), s1)
	}
	restored := pool + "/froRestored"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, s1)
	wantGUIDs(t, src, restored, s1)

	logs, err = itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run of the read-only dataset: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Would delete "+target+"/"+orphan+".")
	// clean retries a delete for 10 minutes, a read-only file system too: stop it sooner.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logs, err = itCleanCtx(ctx, t, work, target)
	if err == nil {
		t.Fatalf("clean of the read-only dataset: no error\n%s", logs)
	}
	t.Logf("clean of the read-only dataset: %v", err)
	wantFilesAt(t, dir, before)
}
