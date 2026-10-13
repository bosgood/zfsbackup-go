package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/files"
)

// End-to-end regression tests for the findings 3, 4 and 14 of the 2026-10-12 adversarial
// review (docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md).

// pinPlanClock makes the smart planner of send and plan read now as the time (backup.Now)
// until the test ends.
func pinPlanClock(t *testing.T, now time.Time) {
	t.Helper()
	old := backup.Now
	backup.Now = func() time.Time { return now }
	t.Cleanup(func() { backup.Now = old })
}

func reviewSnap(name string, day int) files.SnapshotInfo {
	return files.SnapshotInfo{Name: name, CreationTime: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC)}
}

// Finding 3: a set deleted from the middle of the chain at the destination. The smart send
// does not continue the chain: it sends nothing, with a Warning that names the missing link,
// until a newer monthly exists, and then a full.
func TestE2EReviewBrokenChainIsNotContinued(t *testing.T) {
	env := newE2EEnv(t)
	pinPlanClock(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	flags := []string{"--compressor", "", "--fullIfOlderThan", "720h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_daily"}
	target := "file://" + env.dest
	m1, d2, d3, d4 := reviewSnap("d1_monthly", 1), reviewSnap("d2_daily", 2), reviewSnap("d3_daily", 3), reviewSnap("d4_daily", 4)
	pool := []files.SnapshotInfo{m1}
	for _, s := range []files.SnapshotInfo{d2, d3} {
		env.writeSnapshots(t, "tank/data", pool)
		env.sendOK(t, append(flags, "tank/data", target)...)
		pool = append([]files.SnapshotInfo{s}, pool...)
	}
	env.writeSnapshots(t, "tank/data", pool)
	env.sendOK(t, append(flags, "tank/data", target)...)

	removed := 0
	for name := range destObjects(t, env.dest) {
		if strings.HasPrefix(name, "manifests|") && strings.Contains(name, "d2_daily") && strings.Contains(name, "d1_monthly") {
			if err := os.Remove(filepath.Join(env.dest, name)); err != nil {
				t.Fatal(err)
			}
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("removed %d manifests of the set d2_daily from d1_monthly, want 1", removed)
	}
	if err := os.RemoveAll(filepath.Join(env.work, "cache")); err != nil {
		t.Fatal(err)
	}

	pool = append([]files.SnapshotInfo{d4}, pool...)
	env.writeSnapshots(t, "tank/data", pool)
	logs, err := env.send(append(flags, "tank/data", target)...)
	if !errors.Is(err, backup.ErrNoOp) {
		t.Errorf("smart send onto a broken chain: %v, want a no-op\n%s", err, logs)
	}
	if !strings.Contains(logs, "is broken: it holds no backup of d2_daily") {
		t.Errorf("no warning names the missing link d2_daily:\n%s", logs)
	}
	for name := range destObjects(t, env.dest) {
		if strings.Contains(name, "d4_daily") {
			t.Errorf("the send wrote %s onto the broken chain", name)
		}
	}
	out, _, err := env.plan(append(flags[2:], "tank/data", target)...) // plan has no --compressor
	if err == nil || !strings.Contains(out, "next  NOOP  broken-chain\n    warning: The chain of tank/data") {
		t.Errorf("plan returned %v and printed:\n%s", err, out)
	}

	m5 := reviewSnap("d5_monthly", 5)
	env.writeSnapshots(t, "tank/data", append([]files.SnapshotInfo{m5}, pool...))
	logs, err = env.send(append(flags, "tank/data", target)...)
	if err != nil {
		t.Fatalf("send with a newer monthly: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "is broken: it holds no backup of d2_daily") {
		t.Errorf("no warning names the missing link d2_daily:\n%s", logs)
	}
	found := false
	for name := range destObjects(t, env.dest) {
		found = found || strings.HasPrefix(name, "manifests|") && strings.Contains(name, "d5_monthly") && !strings.Contains(name, "|to|")
	}
	if !found {
		t.Errorf("no full of d5_monthly at the destination")
	}
}

// Finding 4: a snapshot taken while the clock read 2099, and a backup of it. send ignores both
// with a Warning and sends a full of the real monthly; the next run continues from that full.
func TestE2EReviewFutureDatedBackupIsIgnored(t *testing.T) {
	env := newE2EEnv(t)
	pinPlanClock(t, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	flags := []string{"--compressor", "", "--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly"}
	target := "file://" + env.dest
	bad := files.SnapshotInfo{Name: "autosnap_2099-01-01_00:00:00_monthly", CreationTime: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}
	sep := reviewSnap("autosnap_2026-09-01_00:00:00_monthly", 1)

	// An explicit send is not planned: an older version's smart send did the same.
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{bad})
	env.sendOK(t, "--compressor", "", "tank/data@"+bad.Name, target)

	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{bad, sep})
	logs, err := env.send(append(flags, "tank/data", target)...)
	if err != nil {
		t.Fatalf("smart send: %v\n%s", err, logs)
	}
	for _, want := range []string{
		"Ignoring tank/data@" + bad.Name + ": it is dated 2099-01-01T00:00:00Z",
		"Ignoring the backup FULL " + bad.Name + " at the destination",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("want %q in the log:\n%s", want, logs)
		}
	}
	found := false
	for name := range destObjects(t, env.dest) {
		found = found || strings.HasPrefix(name, "manifests|") && strings.Contains(name, sep.Name) && !strings.Contains(name, "|to|")
	}
	if !found {
		t.Fatalf("no full of %s at the destination", sep.Name)
	}

	pinPlanClock(t, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	oct := reviewSnap("autosnap_2026-10-01_00:00:00_monthly", 1)
	oct.CreationTime = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{bad, oct, sep})
	out, _, err := env.plan(append(flags[2:], "tank/data", target)...) // plan has no --compressor
	want := fmt.Sprintf("next  INCR  %s  from %s  newer-candidate\n    warning: Ignoring tank/data@%s", oct.Name, sep.Name, bad.Name)
	if err != nil || !strings.Contains(out, want) {
		t.Errorf("plan returned %v and printed:\n%s\nwant:\n%s", err, out, want)
	}
	env.sendOK(t, append(flags, "tank/data", target)...)
	found = false
	for name := range destObjects(t, env.dest) {
		found = found || strings.HasPrefix(name, "manifests|") && strings.Contains(name, sep.Name+"|to|"+oct.Name)
	}
	if !found {
		t.Errorf("no incremental %s from %s at the destination", oct.Name, sep.Name)
	}
}

// Finding 14: with -R, a last backup left only as a bookmark is a pruned source: no `zfs send -R
// -i #bookmark` (real zfs refuses it), a Warning, and a full once a newer monthly exists. With a
// snapshot and a bookmark of the same name on the pool, the snapshot is the source.
func TestE2EReviewBookmarkSourceWithReplication(t *testing.T) {
	flags := []string{"--compressor", "", "--fullIfOlderThan", "720h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_daily", "-R"}
	m1, d2, d3 := reviewSnap("m1_monthly", 1), reviewSnap("d2_daily", 2), reviewSnap("d3_daily", 3)
	bm := d2
	bm.Bookmark = true
	setup := func(t *testing.T) (*e2eEnv, string) {
		env := newE2EEnv(t)
		pinPlanClock(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
		t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
		target := "file://" + env.dest
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{m1})
		env.sendOK(t, append(flags, "tank/data", target)...)
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{d2, m1})
		env.sendOK(t, append(flags, "tank/data", target)...)
		return env, target
	}
	zfsSends := func(t *testing.T, env *e2eEnv) string {
		t.Helper()
		b, err := os.ReadFile(env.zfsLog)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("bookmark only", func(t *testing.T) {
		env, target := setup(t)
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{d3, bm, m1})
		for _, dry := range []bool{true, false} {
			args := append([]string{}, flags...)
			if dry {
				args = append([]string{"-n"}, args...)
			}
			logs, err := env.send(append(args, "tank/data", target)...)
			if !errors.Is(err, backup.ErrNoOp) {
				t.Errorf("dry run %v: %v, want a no-op\n%s", dry, err, logs)
			}
			if !strings.Contains(logs, "Only a bookmark of d2_daily") {
				t.Errorf("dry run %v: no warning that only a bookmark is left:\n%s", dry, logs)
			}
		}
		if log := zfsSends(t, env); strings.Contains(log, "tank/data#d2_daily") {
			t.Errorf("zfs was asked to send from the bookmark:\n%s", log)
		}

		m3 := reviewSnap("d3_monthly", 3)
		m3.CreationTime = m3.CreationTime.Add(time.Second)
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{m3, d3, bm, m1})
		env.sendOK(t, append(flags, "tank/data", target)...)
		if log := zfsSends(t, env); !strings.Contains(log, "send -R tank/data@d3_monthly") {
			t.Errorf("no full -R send of d3_monthly:\n%s", log)
		}
	})

	t.Run("snapshot and bookmark", func(t *testing.T) {
		env, target := setup(t)
		env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{d3, bm, d2, m1}) // the bookmark first in the tie
		env.sendOK(t, append(flags, "tank/data", target)...)
		log := zfsSends(t, env)
		if strings.Contains(log, "tank/data#d2_daily") || !strings.Contains(log, "send -R -i d2_daily tank/data@d3_daily") {
			t.Errorf("want the -R incremental from the snapshot d2_daily, not the bookmark:\n%s", log)
		}
	})
}
