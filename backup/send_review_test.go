package backup

// Regression tests for finding 18 of
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md: a lock left by a
// send before a restart, whose pid now belongs to another live process.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nightlyone/lockfile"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// lockHeldBy writes the lock of tank/data as held by pid, with a boot file that names bootPid and
// boot, and sets the host's boot id to current. It returns the lock.
func lockHeldBy(t *testing.T, pid, bootPid int, boot, current string) bootLock {
	t.Helper()
	oldWorkingDir, oldBootIDPath, oldPlatformBootID := config.WorkingDir, bootIDPath, platformBootID
	t.Cleanup(func() {
		config.WorkingDir, bootIDPath, platformBootID = oldWorkingDir, oldBootIDPath, oldPlatformBootID
	})
	config.WorkingDir = t.TempDir()
	platformBootID = func() string { return "" } // current is the only boot id
	bootIDPath = filepath.Join(t.TempDir(), "boot_id")
	if current != "" {
		if err := os.WriteFile(bootIDPath, []byte(current+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lock, path, err := volumeLock("tank/data")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if boot != "" {
		if err = os.WriteFile(path+".boot", []byte(fmt.Sprintf("%d %s\n", bootPid, boot)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return lock
}

// A lock from another boot is stale even though its pid is a live process now: the lock is taken,
// with a Warning, and the boot file then names this process and this boot.
func TestBootLockFromAnotherBootIsStale(t *testing.T) {
	logs := captureLogs(t)
	live := os.Getppid() // a pid this user can signal: lockfile sees it as running
	lock := lockHeldBy(t, live, live, "old-boot", "this-boot")
	if err := lock.TryLock(); err != nil {
		t.Fatalf("TryLock of a lock from another boot: %v", err)
	}
	if !strings.Contains(logs.String(), "from before the last restart") {
		t.Errorf("no Warning about the stale lock:\n%s", logs)
	}
	data, err := os.ReadFile(lock.bootFile())
	if err != nil || string(data) != fmt.Sprintf("%d this-boot\n", os.Getpid()) {
		t.Errorf("boot file holds %q (%v), want this process and this boot", data, err)
	}
	if err = lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(lock.bootFile()); !os.IsNotExist(err) {
		t.Errorf("Unlock left the boot file (stat: %v)", err)
	}
}

// A lock is live, as before, when its boot file names this boot, names another pid (it was left
// by an earlier holder), is missing (an older version took the lock), or the host has no boot id.
func TestBootLockLiveCases(t *testing.T) {
	live := os.Getppid()
	for _, c := range []struct {
		name          string
		bootPid       int
		boot, current string
	}{
		{"same boot", live, "this-boot", "this-boot"},
		{"boot file of another pid", live + 1, "old-boot", "this-boot"},
		{"no boot file", 0, "", "this-boot"},
		{"no boot id on this host", live, "old-boot", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			lock := lockHeldBy(t, live, c.bootPid, c.boot, c.current)
			if err := lock.TryLock(); !errors.Is(err, lockfile.ErrBusy) {
				t.Errorf("TryLock = %v, want ErrBusy", err)
			}
		})
	}
}

// The busy message must say what to do when the pid is not a send: remove the lock. It said only
// "Run again when it is done", which never comes for a stale lock.
func TestBackupBusyLockMessageSaysHowToRemove(t *testing.T) {
	logs := captureLogs(t)
	live := os.Getppid()
	lock := lockHeldBy(t, live, live, "this-boot", "this-boot")
	jobInfo := &files.JobInfo{
		VolumeName:         "tank/data",
		Destinations:       []string{"file://" + t.TempDir()},
		BaseSnapshot:       files.SnapshotInfo{Name: "a"},
		MaxParallelUploads: 1,
		MaxFileBuffer:      1,
		MaxBackoffTime:     time.Minute,
		MaxRetryTime:       time.Minute,
	}
	err := Backup(t.Context(), jobInfo, false)
	if !errors.Is(err, lockfile.ErrBusy) {
		t.Fatalf("Backup = %v, want ErrBusy", err)
	}
	want := fmt.Sprintf("If pid %d is not %s (after a restart, another process can get its pid), remove %s.", live, config.ProgramName, lock)
	if !strings.Contains(logs.String(), want) {
		t.Errorf("want %q in the log:\n%s", want, logs)
	}
}
