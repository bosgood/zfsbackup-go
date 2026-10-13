//go:build integration

package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

// TestIntegrationS3CleanLayout runs clean on one bucket with three
// destinations: run/, run/sub/ nested in it, and other/. Each clean deletes
// only its own orphan; a nested destination and a dataset no manifest here
// names are skipped and said so; the bucket root (no manifests of its own) and
// two destinations at once are refused. Subtests: Separator (manifests and
// volumes written with --separator '#'), Legacy (a volume under the key prefix
// of an older version) and Versioned (a bucket with versioning on).
func TestIntegrationS3CleanLayout(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clay")
	root := "s3://" + bucket + "/" + prefix
	pRun, pSub, pOther := prefix+"run/", prefix+"run/sub/", prefix+"other/"
	run, sub, other := "s3://"+bucket+"/"+pRun, "s3://"+bucket+"/"+pSub, "s3://"+bucket+"/"+pOther
	src := pool + "/srclay"
	src2 := pool + "/srclay2"
	restored := pool + "/restoredLay"
	restored2 := pool + "/restoredLay2"
	destroyAfter(t, src, src2, restored, restored2)
	work := t.TempDir()
	send := func(snap, target string) {
		t.Helper()
		if logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", snap, target); err != nil {
			t.Fatalf("send %s to %s: %v\n%s", snap, target, err, logs)
		}
	}
	has := func(key string) bool { return slices.Contains(objectNames(t, client, bucket, ""), key) }

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m := snapshot(t, src, "monthly")
	zfsRun(t, "create", "-o", "mountpoint=/"+src2, src2)
	writeFile(t, "/"+src2, "a.bin", 64<<10)
	m2 := snapshot(t, src2, "monthly")
	send(src+"@"+m, run)
	send(src+"@"+m, sub)
	send(src2+"@"+m2, other)
	// Volume names hold no prefix: the one name is an orphan of the srclay set
	// at every prefix.
	vol1 := manifestFor(t, work, src, run, m).Volumes[0].ObjectName
	name := orphanOf(t, client, bucket, pRun, vol1, 77)
	orphanOf(t, client, bucket, pSub, vol1, 77)
	orphanOf(t, client, bucket, pOther, vol1, 77)
	littered := objectNames(t, client, bucket, prefix)

	// run/: its orphan only. Every object of run/sub/, its manifest too, is
	// another destination's.
	logs, err := itClean(t, work, run)
	if err != nil {
		t.Fatalf("clean %s: %v\n%s", run, err, logs)
	}
	if has(pRun + name) {
		t.Errorf("clean %s kept its orphan %s:\n%s", run, name, logs)
	}
	if !has(pSub+name) || !has(pOther+name) {
		t.Fatalf("clean %s deleted an orphan of another destination", run)
	}
	wantInLog(t, logs, "Skipping sub/", ": sub/ holds its own manifests, so it is another destination.")
	wantNotInLog(t, logs, "other/")

	if logs, err = itClean(t, work, sub); err != nil {
		t.Fatalf("clean %s: %v\n%s", sub, err, logs)
	}
	if has(pSub + name) {
		t.Errorf("clean %s kept its orphan %s", sub, name)
	}
	if !has(pOther + name) {
		t.Fatalf("clean %s deleted the orphan at %s", sub, other)
	}

	// other/ has no manifest of srclay: the name is not its to delete.
	logs, err = itClean(t, work, other)
	if err != nil {
		t.Fatalf("clean %s: %v\n%s", other, err, logs)
	}
	if !has(pOther + name) {
		t.Errorf("clean %s deleted %s, a volume of a dataset no manifest there names", other, name)
	}
	wantInLog(t, logs, "Skipping "+name+": no manifest at this destination is for dataset "+src)
	afterClean := objectNames(t, client, bucket, prefix)
	if want := slices.DeleteFunc(slices.Clone(littered), func(n string) bool { return n == "run/"+name || n == "run/sub/"+name }); !slices.Equal(afterClean, want) {
		t.Errorf("after the cleans the bucket holds %v, want %v", afterClean, want)
	}

	// The root has objects but no manifests of its own; two destinations are one too many.
	logs, err = itClean(t, work, root)
	if err == nil {
		t.Fatalf("clean %s succeeded, want a refusal:\n%s", root, logs)
	}
	wantInLog(t, logs, "objects but no manifests; refusing to clean")
	logs, err = itClean(t, work, run+","+other)
	if err == nil {
		t.Fatalf("clean of two destinations succeeded, want a refusal:\n%s", logs)
	}
	wantInLog(t, logs, "clean takes one destination, got")
	wantObjectsAt(t, client, bucket, prefix, afterClean)

	requireVolumes(t, manifestFor(t, work, src, run, m), objectNames(t, client, bucket, pRun))
	requireVolumes(t, manifestFor(t, work, src, sub, m), objectNames(t, client, bucket, pSub))
	requireVolumes(t, manifestFor(t, work, src2, other, m2), objectNames(t, client, bucket, pOther))
	itReceive(t, t.TempDir(), "-F", src+"@"+m, run, restored)
	wantGUIDs(t, src, restored, m)
	itReceive(t, t.TempDir(), "-F", src2+"@"+m2, other, restored2)
	wantGUIDs(t, src2, restored2, m2)

	t.Run("Separator", func(t *testing.T) { cleanLayoutSeparator(t, pool) })
	t.Run("Legacy", func(t *testing.T) { cleanLayoutLegacy(t, pool) })
	t.Run("Versioned", func(t *testing.T) { cleanLayoutVersioned(t, pool) })
}

// cleanLayoutSeparator: clean has no --separator flag. It reads manifests
// written with '#' as its own, deletes a '#' orphan, and deletes a '|' name of
// the same set too.
func cleanLayoutSeparator(t *testing.T, pool string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clsep")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srclsep"
	restored := pool + "/restoredClSep"
	destroyAfter(t, src, restored)
	work := t.TempDir()
	send := func(args ...string) {
		t.Helper()
		args = append([]string{"--separator", "#", "--volsize", "1", "--compressor", ""}, append(args, target)...)
		if logs, err := itSend(t, work, args...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	send(src + "@" + m1)
	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "monthly")
	send("-i", src+"@"+m1, src+"@"+m2)
	sent := objectNames(t, client, bucket, prefix)
	manifests := 0
	for _, n := range sent {
		if !strings.Contains(n, "#") || strings.Contains(n, "|") {
			t.Errorf("object %s: want '#' in it and no '|'", n)
		}
		if strings.HasPrefix(n, "manifests#") {
			manifests++
		}
	}
	if manifests != 2 {
		t.Fatalf("%s holds %d manifests#... objects, want 2: %v", target, manifests, sent)
	}

	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Dry-run: would delete 0 objects")
	wantNotInLog(t, logs, "Skipping unrecognized object")

	orphan := orphanOf(t, client, bucket, prefix, manifestFor(t, work, src, target, m2).Volumes[0].ObjectName, 77)
	pipe := strings.ReplaceAll(orphan, "#", "|")
	putObject(t, client, bucket, prefix+pipe, []byte("orphan"))
	if logs, err := itClean(t, work, target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	wantObjectsAt(t, client, bucket, prefix, sent)
	for _, snap := range []string{m1, m2} {
		requireVolumes(t, manifestFor(t, work, src, target, snap), sent)
	}

	itReceive(t, t.TempDir(), "--auto", "-F", "--separator", "#", src, target, restored)
	wantSnapshots(t, restored, m1, m2)
	wantGUIDs(t, src, restored, m1, m2)
}

// cleanLayoutLegacy: a volume at the key an older version wrote for
// s3://<bucket>/p/ (p<name>, outside p/) gets clean's Warning, and clean
// leaves it: it is outside the destination.
func cleanLayoutLegacy(t *testing.T, pool string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clold")
	p := prefix + "p/"
	target := "s3://" + bucket + "/" + p
	src := pool + "/srclold"
	restored := pool + "/restoredClOld"
	destroyAfter(t, src, restored)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 64<<10)
	m := snapshot(t, src, "monthly")
	if logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", src+"@"+m, target); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	set := manifestFor(t, work, src, target, m)
	legacy := strings.TrimSuffix(p, "/") + set.Volumes[0].ObjectName // bucket-absolute
	putObject(t, client, bucket, legacy, []byte("old"))

	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Found "+legacy+", which looks like a backup volume written by an older version outside of",
		"Dry-run: would delete 0 objects")

	if logs, err := itClean(t, work, target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	if !slices.Contains(objectNames(t, client, bucket, ""), legacy) {
		t.Errorf("clean deleted %s, outside of %s", legacy, target)
	}
	requireVolumes(t, set, objectNames(t, client, bucket, p))
	itReceive(t, t.TempDir(), "-F", src+"@"+m, target, restored)
	wantGUIDs(t, src, restored, m)
}

// cleanLayoutVersioned: in a bucket with versioning on, clean's delete of an
// orphan leaves a delete marker, the orphan is gone from the listing, and the
// backup is whole.
func cleanLayoutVersioned(t *testing.T, pool string) {
	if os.Getenv("ZFSBACKUP_IT_BUCKET") != "" {
		t.Skip("ZFSBACKUP_IT_BUCKET is set: the test does not turn versioning on in a bucket it shares")
	}
	client, bucket, prefix := itBucket(t, "zfsbackup-it-clver")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srclver"
	restored := pool + "/restoredClVer"
	destroyAfter(t, src, restored)
	work := t.TempDir()

	_, err := client.PutBucketVersioning(&s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &s3.VersioningConfiguration{Status: aws.String(s3.BucketVersioningStatusEnabled)},
	})
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "NotImplemented" {
		t.Skipf("the server cannot turn versioning on: %v", err)
	}
	if err != nil {
		t.Fatalf("could not turn versioning on for %s: %v", bucket, err)
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 64<<10)
	m := snapshot(t, src, "monthly")
	logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", src+"@"+m, target)
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	sent := objectNames(t, client, bucket, prefix)
	orphan := orphanOf(t, client, bucket, prefix, manifestFor(t, work, src, target, m).Volumes[0].ObjectName, 77)

	if logs, err = itClean(t, work, target); err != nil {
		t.Fatalf("clean: %v\n%s", err, logs)
	}
	wantObjectsAt(t, client, bucket, prefix, sent)
	versions, err := client.ListObjectVersions(&s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix + orphan)})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.DeleteMarkers) == 0 {
		t.Fatalf("no delete marker for %s: versioning was not on (versions %v)", orphan, versions.Versions)
	}
	if got := itList(t, work, target)[src]; len(got) != 1 {
		t.Fatalf("list after clean: %v, want one backup", chainNames(got))
	}
	logs, err = itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Dry-run: would delete 0 objects")
	itReceive(t, t.TempDir(), "-F", src+"@"+m, target, restored)
	wantGUIDs(t, src, restored, m)
}
