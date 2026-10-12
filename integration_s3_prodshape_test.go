//go:build integration

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
)

// TestIntegrationS3ProdShape runs the production flags on a dataset named
// like production's (5 parts): sanoid's monthly, daily and hourly tie on one
// creation second, a manual snapshot has the suffix but not the prefix, and
// receive --fullPath and --lastPath name the restored datasets.
func TestIntegrationS3ProdShape(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-prod")
	target := "s3://" + bucket + "/" + prefix
	ds := pool + "/plain/user/app/navidrome"
	restoredD := pool + "/restoredD"
	restoredE := pool + "/restoredE"
	// -r takes the children.
	destroyAfter(t, restoredD, restoredE, pool+"/plain")
	work := t.TempDir()
	flags := []string{
		"--snapshotPrefix", "autosnap_", "--fullIfOlderThan", "4320h",
		"--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly",
		"--volsize", "1", "--compressor", "",
	}
	send := func() {
		t.Helper()
		if logs, err := itSend(t, work, append(flags, ds, target)...); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
	}
	wantLink := func(snap, source string) {
		t.Helper()
		if got := manifestFor(t, work, ds, target, snap).IncrementalSnapshot.Name; got != source {
			t.Fatalf("%s sent from %q, want from %q", snap, got, source)
		}
	}
	monthlyTie := func() string {
		t.Helper()
		return snapshotTie(t, ds, "monthly", "daily", "hourly")[0]
	}

	zfsRun(t, "create", "-p", ds)
	writeFile(t, "/"+ds, "a.bin", 3<<20)
	tie := snapshotTie(t, ds, "monthly", "daily", "hourly")
	m1 := tie[0]
	// The tie is real: one creation second for all three.
	for _, s := range tie[1:] {
		if got, want := zfsGet(t, ds+"@"+s, "creation"), zfsGet(t, ds+"@"+m1, "creation"); got != want {
			t.Fatalf("%s was created at %s, %s at %s: not a tie", s, got, m1, want)
		}
	}
	snapshotTie(t, ds, "daily", "hourly")
	snapshotTie(t, ds, "daily", "hourly")
	snapshot(t, ds, "hourly")
	send()
	wantLink(m1, "")

	writeFile(t, "/"+ds, "b.bin", 1<<20)
	m2 := monthlyTie()
	snapshotTie(t, ds, "daily", "hourly")
	send()
	wantLink(m2, m1)

	// A manual snapshot with the suffix and not the prefix: not a candidate.
	zfsRun(t, "snapshot", ds+"@pre-upgrade_monthly")
	if logs, err := itSend(t, work, append(flags, ds, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("send after pre-upgrade_monthly: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	writeFile(t, "/"+ds, "c.bin", 1<<20)
	m3 := monthlyTie()
	send()
	wantLink(m3, m2)
	got := chainNames(itList(t, work, target)[ds])
	if want := []string{m1 + "<-", m2 + "<-" + m1, m3 + "<-" + m2}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list holds %v, want %v", got, want)
	}

	// --fullPath: the whole name, less the pool, under the target.
	zfsRun(t, "create", restoredD)
	dWork := t.TempDir()
	restoredDS := restoredD + "/plain/user/app/navidrome"
	itReceive(t, dWork, "--auto", "-F", "--fullPath", ds, target, restoredD)
	wantSnapshots(t, restoredDS, m1, m2, m3)
	wantGUIDs(t, ds, restoredDS, m1, m2, m3)
	compareDirs(t, "/"+ds, "/"+restoredDS)

	// --lastPath: the last part only.
	zfsRun(t, "create", restoredE)
	restoredES := restoredE + "/navidrome"
	itReceive(t, t.TempDir(), "--auto", "-F", "--lastPath", ds, target, restoredE)
	wantSnapshots(t, restoredES, m1, m2, m3)
	wantGUIDs(t, ds, restoredES, m1, m2, m3)
	compareDirs(t, "/"+ds, "/"+restoredES)

	// A second run onto the -d target: --auto lists the computed name and
	// sends one step.
	writeFile(t, "/"+ds, "d.bin", 1<<20)
	m4 := monthlyTie()
	send()
	wantLink(m4, m3)
	itReceive(t, dWork, "--auto", "-F", "--fullPath", ds, target, restoredD)
	wantSnapshots(t, restoredDS, m1, m2, m3, m4)
	wantGUIDs(t, ds, restoredDS, m4)
	compareDirs(t, "/"+ds, "/"+restoredDS)

	// The / of the dataset name is in every object key.
	for _, n := range objectNames(t, client, bucket, prefix) {
		if !strings.Contains(n, "plain/user/app/navidrome") {
			t.Errorf("object %s does not hold the dataset name", n)
		}
	}
	if logs, err := itClean(t, t.TempDir(), "--dry-run", target); err != nil || !strings.Contains(logs, "Dry-run: would delete 0 objects") {
		t.Fatalf("clean --dry-run: %v, want 0 deletes:\n%s", err, logs)
	}
}
