//go:build integration

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bosgood/zfsbackup-go/backup"
)

// newSSHDest makes a destination on a new dataset <pool>/<name>, reached as
// root over sftp at the test sshd.
func newSSHDest(t *testing.T, pool, name string, props ...string) localDest {
	t.Helper()
	_, dir := itDestDataset(t, pool, name, props...)
	return localDest{dir: dir, target: itSSHTarget(t, "root", dir)}
}

// newSSHJailDest makes a destination on a new dataset <pool>/<name>, reached
// as the chrooted user jailed: the remote path /<name> is
// <ZFSBACKUP_IT_SSH_JAIL>/<name> here. sshd runs in the test's container, so
// for every other user a remote path is also a local path of the test, and
// a backend that touches the local file system by mistake still works.
func newSSHJailDest(t *testing.T, pool, name string, props ...string) localDest {
	t.Helper()
	host, port, _, _ := itSSHEnv(t)
	jail := os.Getenv("ZFSBACKUP_IT_SSH_JAIL")
	if jail == "" {
		t.Fatal("ZFSBACKUP_IT_SSH_JAIL is unset; integration/sshd.sh sets it")
	}
	_, dir := itDestDatasetAt(t, pool, name, filepath.Join(jail, name), props...)
	chownTree(t, "jailed", dir)
	return localDest{dir: dir, target: "ssh://jailed@" + host + ":" + port + "/" + name}
}

// TestIntegrationSSH runs real zfs against the ssh:// backend and a real
// OpenSSH sshd, with no SSH_* variable set: the default key and known_hosts in
// /root/.ssh do the work. The steps of TestIntegrationS3 with 24+ volumes on
// four parallel uploads over one sftp session, then list and clean.
func TestIntegrationSSH(t *testing.T) {
	pool := testPool(t)
	itSSHClearEnv(t)
	dest := newSSHDest(t, pool, "sshdest")
	target := dest.target
	src := pool + "/sshsrc"
	restored := pool + "/sshRestored"
	restored2 := pool + "/sshRestored2"
	monthlyClone := pool + "/sshMonthly"
	destroyAfter(t, monthlyClone, src, restored, restored2)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 24<<20)
	monthly := snapshot(t, src, "monthly")
	snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	smart := append([]string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "--volsize", "1", "--compressor", "",
		"--maxParallelUploads", "4",
	}, sshRetry...)

	offset := sshdLogSize(t)
	if logs, err := itSend(t, work, append(smart, src, target)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	full := manifestFor(t, work, src, target, monthly)
	if full.IncrementalSnapshot.Name != "" || len(full.Volumes) < 24 {
		t.Fatalf("full: sent from %q in %d volumes, want a full in 24+", full.IncrementalSnapshot.Name, len(full.Volumes))
	}
	manifests := manifestFiles(fileNames(t, dest.dir))
	if len(manifests) != 1 {
		t.Fatalf("full: %s holds manifests %v, want 1", dest.dir, manifests)
	}
	wantFilesAt(t, dest.dir, append(volumeNames(full), manifests...))
	wantVolumeSizes(t, dest.dir, full)
	if want := "Accepted publickey for root from 127.0.0.1"; !sshdLogHas(t, offset, want) {
		t.Errorf("sshd logged no %q:\n%s", want, sshdLogSince(t, offset))
	}

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
	if logs, err := itSend(t, work, append(smart, src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	itReceive(t, work, "-F", src+"@"+monthly, target, restored)
	wantSnapshots(t, restored, monthly)
	wantGUIDs(t, src, restored, monthly)
	zfsRun(t, "clone", "-o", "mountpoint=/"+monthlyClone, src+"@"+monthly, monthlyClone)
	compareDirs(t, "/"+monthlyClone, "/"+restored)
	itReceive(t, work, "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, monthly, daily2)
	wantGUIDs(t, src, restored, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored)
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restored2)
	wantSnapshots(t, restored2, monthly, daily2)
	wantGUIDs(t, src, restored2, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored2)

	if backups := itList(t, t.TempDir(), target)[src]; len(backups) != 2 {
		t.Fatalf("list: %d backups of %s, want 2: %v", len(backups), src, chainNames(backups))
	}

	// clean over sftp: the orphan goes, notes.txt stays.
	orphan := strings.TrimSuffix(full.Volumes[0].ObjectName, "vol1") + "vol99"
	if err := os.WriteFile(filepath.Join(dest.dir, orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest.dir, "notes.txt"), []byte("not a volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	withOrphan := fileNames(t, dest.dir)
	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Dry-run: would delete 1 objects", "Would delete "+target+"/"+orphan+".")
	wantFilesAt(t, dest.dir, withOrphan)
	if logs, err = itClean(t, work, target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Skipping unrecognized object notes.txt")
	wantFilesAt(t, dest.dir, slices.DeleteFunc(withOrphan, func(n string) bool { return n == orphan }))

	t.Run("Pipe", func(t *testing.T) {
		dest := newSSHDest(t, pool, "sshpipe")
		restored3 := pool + "/sshRestored3"
		destroyAfter(t, restored3)
		work := t.TempDir()
		if logs, err := itSend(t, work, append([]string{"--maxFileBuffer", "0", "--volsize", "1", "--full", src, dest.target}, sshRetry...)...); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		itReceive(t, work, "--maxFileBuffer", "0", "--auto", "-F", src, dest.target, restored3)
		sent := newestBackup(t, src, dest.target).BaseSnapshot.Name
		wantSnapshots(t, restored3, sent)
		wantGUIDs(t, src, restored3, sent)
		compareDirs(t, "/"+src, "/"+restored3)
	})

	// A dataset name with a space and a colon: the remote paths carry both.
	t.Run("Names", func(t *testing.T) {
		dest := newSSHDest(t, pool, "sshnames")
		src := pool + "/na me:1"
		restored := pool + "/re stored:1"
		newSource(t, src, 2<<20)
		s1 := snapshot(t, src, "daily")
		mustSend(t, t.TempDir(), append(slices.Clone(plainSend), append(slices.Clone(sshRetry), src+"@"+s1, dest.target)...)...)
		var spaced bool
		for _, n := range fileNames(t, dest.dir) {
			spaced = spaced || strings.Contains(n, "na me:1")
		}
		if !spaced {
			t.Fatalf("no file under %s carries the name %q: %v", dest.dir, src, fileNames(t, dest.dir))
		}
		restoreAll(t, src, dest.target, restored, s1)
	})

	// The default flags (gzip, --volsize 200): 256 MiB of random data in two volumes.
	t.Run("Default", func(t *testing.T) {
		dest := newSSHDest(t, pool, "sshdefault")
		src := pool + "/sshdefsrc"
		newSource(t, src, 256<<20)
		s1 := snapshot(t, src, "daily")
		work := t.TempDir()
		mustSend(t, work, append(slices.Clone(sshRetry), src+"@"+s1, dest.target)...)
		if m := manifestFor(t, work, src, dest.target, s1); len(m.Volumes) != 2 {
			t.Errorf("256 MiB at the default --volsize went in %d volumes, want 2", len(m.Volumes))
		}
		restoreAll(t, src, dest.target, pool+"/sshdefRestored", s1)
	})
}
