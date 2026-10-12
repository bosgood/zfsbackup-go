//go:build integration

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3ManifestPrefix runs two jobs with their own --manifestPrefix
// in one bucket. Volume names hold no prefix, so a set both jobs send is one
// set of objects. list reads one prefix; clean deletes only orphans of its own
// prefix's sets and spares any set the other prefix names.
func TestIntegrationS3ManifestPrefix(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-pfx")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcpfx"
	restoredA := pool + "/restoredA"
	restoredB := pool + "/restoredB"
	destroyAfter(t, src, restoredA, restoredB)
	work := t.TempDir()
	jobA := []string{"--manifestPrefix", "jobA"}
	jobB := []string{"--manifestPrefix", "jobB"}
	send := func(job []string, args ...string) {
		t.Helper()
		args = append(append(slices.Clone(job), "--volsize", "1", "--compressor", ""), append(args, target)...)
		if logs, err := itSend(t, work, args...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}
	clean := func(job []string, args ...string) string {
		t.Helper()
		logs, err := itClean(t, work, append(append(slices.Clone(job), args...), target)...)
		if err != nil {
			t.Fatalf("clean %v %v: %v\n%s", job, args, err, logs)
		}
		return logs
	}
	has := func(name string) bool { return slices.Contains(objectNames(t, client, bucket, prefix), name) }

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "daily")
	send(jobA, src+"@"+m1)
	send(jobB, src+"@"+m1)
	send(jobA, "-i", src+"@"+m1, src+"@"+m2)
	// A set only jobB names: a full of m2.
	send(jobB, src+"@"+m2)

	var manifestsA, manifestsB int
	for _, n := range objectNames(t, client, bucket, prefix) {
		switch {
		case strings.HasPrefix(n, "jobA") && strings.Contains(n, ".manifest"):
			manifestsA++
		case strings.HasPrefix(n, "jobB") && strings.Contains(n, ".manifest"):
			manifestsB++
		case strings.HasPrefix(n, "job"):
			t.Errorf("object %s starts with a manifest prefix but is no manifest", n)
		}
	}
	if manifestsA != 2 || manifestsB != 2 {
		t.Fatalf("%s holds %d jobA and %d jobB manifests, want 2 and 2", target, manifestsA, manifestsB)
	}
	// Volume names hold no prefix: both jobs' m1 sets are the same objects.
	setA := manifestFor(t, work, src, target, m1, jobA...)
	setB := manifestFor(t, work, src, target, m1, jobB...)
	if setA.Volumes[0].ObjectName != setB.Volumes[0].ObjectName {
		t.Errorf("volume 1 of m1 is %s for jobA, %s for jobB; want one name", setA.Volumes[0].ObjectName, setB.Volumes[0].ObjectName)
	}

	got := chainNames(itList(t, work, append(slices.Clone(jobA), target)...)[src])
	if want := []string{m1 + "<-", m2 + "<-" + m1}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list jobA holds %v, want %v", got, want)
	}
	got = chainNames(itList(t, work, append(slices.Clone(jobB), target)...)[src])
	if want := []string{m1 + "<-", m2 + "<-"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list jobB holds %v, want %v", got, want)
	}
	out, logs, err := itRunOut(cmd.ResetListJobInfo, "list", work, "zfs", "--jsonOutput", target)
	t.Logf("list with the default prefix: error %v, output %q", err, strings.TrimSpace(out))
	if err == nil && strings.Contains(out, src) {
		t.Errorf("list with the default prefix lists %s:\n%s\n%s", src, out, logs)
	}

	for _, job := range [][]string{jobA, jobB} {
		if logs := clean(job, "--dry-run"); !strings.Contains(logs, "Dry-run: would delete 0 objects") {
			t.Errorf("clean %v --dry-run: want 0 deletes:\n%s", job, logs)
		}
	}

	// Objects but no manifests under the default prefix: clean refuses and deletes nothing.
	before := objectNames(t, client, bucket, prefix)
	if logs, err := itClean(t, work, target); err == nil {
		t.Errorf("clean with the default prefix succeeded, want a refusal:\n%s", logs)
	}
	if after := objectNames(t, client, bucket, prefix); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Fatalf("a refused clean changed %s: %v, want %v", target, after, before)
	}

	// Orphans: one of jobA's m1->m2 set, one of jobB's full of m2, and one of
	// the m1 set both name.
	orphan := func(set string) string {
		t.Helper()
		if !strings.HasSuffix(set, "vol1") {
			t.Fatalf("volume 1 is %s, want a name ending in vol1", set)
		}
		name := strings.TrimSuffix(set, "vol1") + "vol77"
		putObject(t, client, bucket, prefix+name, []byte("orphan"))
		return name
	}
	orphanA := orphan(manifestFor(t, work, src, target, m2, jobA...).Volumes[0].ObjectName)
	orphanB := orphan(manifestFor(t, work, src, target, m2, jobB...).Volumes[0].ObjectName)
	orphanShared := orphan(setA.Volumes[0].ObjectName)

	clean(jobA)
	if has(orphanA) {
		t.Errorf("clean jobA kept its orphan %s", orphanA)
	}
	if !has(orphanB) {
		t.Errorf("clean jobA deleted %s, an object of a set jobB names", orphanB)
	}
	clean(jobB)
	if has(orphanB) {
		t.Errorf("clean jobB kept its orphan %s", orphanB)
	}
	// The other prefix's manifests are matched by set name only, never read:
	// an orphan of a set both prefixes name is spared by both.
	if !has(orphanShared) {
		t.Errorf("clean deleted %s, an object of a set both prefixes name", orphanShared)
	}
	for _, job := range [][]string{jobA, jobB} {
		for _, snap := range []string{m1, m2} {
			requireVolumes(t, manifestFor(t, work, src, target, snap, job...), objectNames(t, client, bucket, prefix))
		}
	}

	itReceive(t, t.TempDir(), append(slices.Clone(jobB), "--auto", "-F", src+"@"+m1, target, restoredB)...)
	wantSnapshots(t, restoredB, m1)
	wantGUIDs(t, src, restoredB, m1)
	itReceive(t, t.TempDir(), append(slices.Clone(jobA), "--auto", "-F", src, target, restoredA)...)
	wantSnapshots(t, restoredA, m1, m2)
	wantGUIDs(t, src, restoredA, m1, m2)
	compareDirs(t, "/"+src, "/"+restoredA)
}
