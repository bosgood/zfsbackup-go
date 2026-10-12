//go:build integration

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

// TestIntegrationS3TwoDestinations sends to two S3 prefixes at once: the sets
// are equal at both, a manifest lost at one is completed without a re-upload,
// a lost volume puts them out of sync, and each restores on its own.
func TestIntegrationS3TwoDestinations(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-two")
	p1, p2 := prefix+"d1/", prefix+"d2/"
	d1, d2 := "s3://"+bucket+"/"+p1, "s3://"+bucket+"/"+p2
	both := d1 + "," + d2
	src := pool + "/srctwo"
	restoredT1 := pool + "/restoredT1"
	restoredT2 := pool + "/restoredT2"
	destroyAfter(t, src, restoredT1, restoredT2)
	work := t.TempDir()
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_monthly", "--volsize", "1", "--compressor", "",
	}
	send := func() string {
		t.Helper()
		logs, err := itSend(t, work, append(smart, src, both)...)
		if err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		return logs
	}
	// wantSame fails unless both destinations hold the same names, and returns them.
	wantSame := func() []string {
		t.Helper()
		n1, n2 := objectNames(t, client, bucket, p1), objectNames(t, client, bucket, p2)
		if strings.Join(n1, " ") != strings.Join(n2, " ") {
			t.Fatalf("the destinations differ:\nd1: %v\nd2: %v", n1, n2)
		}
		return n1
	}
	manifestOf := func(names []string, snap string) string {
		t.Helper()
		for _, n := range names {
			if strings.Contains(n, ".manifest") && strings.Contains(n, snap) {
				return n
			}
		}
		t.Fatalf("no manifest of %s in %v", snap, names)
		return ""
	}
	lastModified := func(key string) time.Time {
		t.Helper()
		out, err := client.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("could not head s3://%s/%s: %v", bucket, key, err)
		}
		return aws.TimeValue(out.LastModified)
	}

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	m1 := snapshot(t, src, "monthly")
	send()
	volumes, manifests, list := countObjects(t, client, bucket, p1)
	if volumes < 3 || manifests != 1 {
		t.Fatalf("d1 holds %d volumes and %d manifests, want 3+ and 1:\n%s", volumes, manifests, list)
	}
	wantSame()

	writeFile(t, "/"+src, "b.bin", 1<<20)
	m2 := snapshot(t, src, "monthly")
	send()
	if got := manifestFor(t, work, src, d1, m2).IncrementalSnapshot.Name; got != m1 {
		t.Fatalf("%s sent from %q, want from %s", m2, got, m1)
	}
	names := wantSame()

	// The manifest of m2 is lost at d2, its volumes are there: complete-partial
	// uploads the manifest only.
	m2Set := manifestFor(t, work, src, d1, m2)
	before := map[string]time.Time{}
	for _, v := range m2Set.Volumes {
		before[v.ObjectName] = lastModified(p2 + v.ObjectName)
	}
	deleteObject(t, client, bucket, p2+manifestOf(names, m2))
	time.Sleep(time.Second) // LastModified has second precision: a re-upload must show
	if logs := send(); !strings.Contains(logs, "complete-partial") {
		t.Errorf("want complete-partial in the log:\n%s", logs)
	}
	wantSame()
	for key, at := range before {
		if got := lastModified(p2 + key); !got.Equal(at) {
			t.Errorf("%s at d2 was uploaded again: last modified %v, before %v", key, got, at)
		}
	}

	// The manifest and a volume of m2 are lost at d2: the destinations are out
	// of sync, and a new monthly makes the planner say so.
	deleteObject(t, client, bucket, p2+manifestOf(names, m2))
	deleteObject(t, client, bucket, p2+m2Set.Volumes[0].ObjectName)
	at1, at2 := objectNames(t, client, bucket, p1), objectNames(t, client, bucket, p2)
	writeFile(t, "/"+src, "c.bin", 1<<20)
	snapshot(t, src, "monthly")
	logs, err := itSend(t, work, append(smart, src, both)...)
	const outOfSync = "last incremental backup at destinations do not match"
	if err == nil || !strings.Contains(fmt.Sprint(err)+logs, outOfSync) {
		t.Fatalf("send to out-of-sync destinations: got %v, want %q:\n%s", err, outOfSync, logs)
	}
	if got := objectNames(t, client, bucket, p1); strings.Join(got, " ") != strings.Join(at1, " ") {
		t.Errorf("the failed send changed d1: %v, want %v", got, at1)
	}
	if got := objectNames(t, client, bucket, p2); strings.Join(got, " ") != strings.Join(at2, " ") {
		t.Errorf("the failed send changed d2: %v, want %v", got, at2)
	}

	// Each destination restores what it holds. Repairing d2 is the operator's
	// job (clean --force at d2, then send); --resume cannot, a volume is gone.
	itReceive(t, t.TempDir(), "--auto", "-F", src, d1, restoredT1)
	wantSnapshots(t, restoredT1, m1, m2)
	wantGUIDs(t, src, restoredT1, m1, m2)
	itReceive(t, t.TempDir(), "--auto", "-F", src, d2, restoredT2)
	wantSnapshots(t, restoredT2, m1)
	wantGUIDs(t, src, restoredT2, m1)
}
