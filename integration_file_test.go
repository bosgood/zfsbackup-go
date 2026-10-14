//go:build integration

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
)

// TestIntegrationFile runs real zfs against the file:// backend, into a
// destination that is a zfs dataset (a second pool, a USB disk): the steps of
// TestIntegrationS3, then list and clean.
func TestIntegrationFile(t *testing.T) {
	pool := testPool(t)
	_, dest := itDestDataset(t, pool, "filedest")
	target := itFileTarget(dest)
	src := pool + "/filesrc"
	restored := pool + "/fileRestored"
	restored2 := pool + "/fileRestored2"
	monthlyClone := pool + "/fileMonthly"
	// The clone first: src cannot go while a clone of its snapshot exists.
	destroyAfter(t, monthlyClone, src, restored, restored2)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	monthly := snapshot(t, src, "monthly")
	snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "--volsize", "1", "--compressor", "",
	}

	// Full: the volumes and the manifest are plain files in the dataset, each of
	// the size the manifest records.
	if logs, err := itSend(t, work, append(smart, src, target)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	full := manifestFor(t, work, src, target, monthly)
	if full.IncrementalSnapshot.Name != "" || len(full.Volumes) < 3 {
		t.Fatalf("full: sent from %q in %d volumes, want a full in 3+", full.IncrementalSnapshot.Name, len(full.Volumes))
	}
	manifests := manifestFiles(fileNames(t, dest))
	if len(manifests) != 1 {
		t.Fatalf("full: %s holds manifests %v, want 1", dest, manifests)
	}
	wantFilesAt(t, dest, append(volumeNames(full), manifests...))
	wantVolumeSizes(t, dest, full)

	// Incremental: the newest _daily, not the _hourly after it.
	writeFile(t, "/"+src, "b.bin", 1<<20)
	daily2 := snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	if logs, err := itSend(t, work, append(smart, src, target)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	got := newestBackup(t, src, target)
	if got.BaseSnapshot.Name != daily2 || got.IncrementalSnapshot.Name != monthly {
		t.Fatalf("incremental: sent %s from %q, want %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, daily2, monthly)
	}
	wantVolumeSizes(t, dest, manifestFor(t, work, src, target, daily2))

	// Noop: nothing new with these suffixes.
	if logs, err := itSend(t, work, append(smart, src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	// The three receives of TestIntegrationS3.
	itReceive(t, work, "-F", src+"@"+monthly, target, restored)
	wantSnapshots(t, restored, monthly)
	wantGUIDs(t, src, restored, monthly)
	zfsRun(t, "clone", "-o", "mountpoint=/"+monthlyClone, src+"@"+monthly, monthlyClone)
	compareDirs(t, "/"+monthlyClone, "/"+restored)
	itReceive(t, work, "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, monthly, daily2)
	wantGUIDs(t, src, restored, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored)
	// From an empty working directory: the manifests come from the dataset.
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restored2)
	wantSnapshots(t, restored2, monthly, daily2)
	wantGUIDs(t, src, restored2, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored2)

	if backups := itList(t, t.TempDir(), target)[src]; len(backups) != 2 {
		t.Fatalf("list: %d backups of %s, want 2: %v", len(backups), src, chainNames(backups))
	}

	// clean: an orphan volume goes, a file that is no volume stays.
	vol1 := full.Volumes[0].ObjectName
	if !strings.HasSuffix(vol1, "vol1") {
		t.Fatalf("volume 1 is %s, want a name ending in vol1", vol1)
	}
	orphan := strings.TrimSuffix(vol1, "vol1") + "vol99"
	if err := os.WriteFile(filepath.Join(dest, orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "notes.txt"), []byte("not a volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	withOrphan := fileNames(t, dest)
	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Dry-run: would delete 1 objects", "Would delete "+target+"/"+orphan+".")
	wantFilesAt(t, dest, withOrphan)
	if logs, err = itClean(t, work, target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Skipping unrecognized object notes.txt")
	wantFilesAt(t, dest, slices.DeleteFunc(withOrphan, func(n string) bool { return n == orphan }))

	// Pipe mode: volumes stream to the files and back, never to the temp dir.
	t.Run("Pipe", func(t *testing.T) {
		pipe := newFileDest(t, pool, "filepipe")
		restored3 := pool + "/fileRestored3"
		destroyAfter(t, restored3)
		work := t.TempDir()
		if logs, err := itSend(t, work, "--maxFileBuffer", "0", "--volsize", "1", "--full", src, pipe.target); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		itReceive(t, work, "--maxFileBuffer", "0", "--auto", "-F", src, pipe.target, restored3)
		sent := newestBackup(t, src, pipe.target).BaseSnapshot.Name
		wantSnapshots(t, restored3, sent)
		wantGUIDs(t, src, restored3, sent)
		compareDirs(t, "/"+src, "/"+restored3)
	})

	// Another spelling of the destination (a trailing slash) and no cache at
	// all: the smart send reads the manifests from the directory and has
	// nothing to do.
	t.Run("Spelling", func(t *testing.T) {
		before := fileNames(t, dest)
		if logs, err := itSend(t, t.TempDir(), append(smart, src, target+"/")...); !errors.Is(err, backup.ErrNoOp) {
			t.Fatalf("send to %s/: got %v, want %v\n%s", target, err, backup.ErrNoOp, logs)
		}
		wantFilesAt(t, dest, before)
	})
}
