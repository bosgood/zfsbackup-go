//go:build integration

// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3 runs real zfs against the real s3:// backend: sanoid-style
// snapshots, a smart full, a smart incremental, a noop, and receives of each
// back into new datasets. Run it with make integration-docker (a Linux host
// with the zfs module) or make integration-vm (a Mac); see integration/README.md.
func TestIntegrationS3(t *testing.T) {
	pool := testPool(t)
	client, bucket := itBucket(t, "zfsbackup-it")
	target := "s3://" + bucket + "/"
	src := pool + "/src"
	restored := pool + "/restored"
	restored2 := pool + "/restored2"
	monthlyClone := pool + "/monthly"
	// The clone first: src cannot go while a clone of its snapshot exists.
	destroyAfter(t, monthlyClone, src, restored, restored2)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	monthly := snapshot(t, src, "monthly")
	snapshot(t, src, "daily")
	snapshot(t, src, "hourly")

	// No compressor: the volume counter sees compressed bytes, and pgzip holds
	// several MiB in its blocks, so with it a 3 MiB stream fits in one volume.
	// The Pipe subtest keeps the default compressor.
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_daily", "--volsize", "1", "--compressor", "",
	}

	// Full: no backup yet, so the newest _monthly.
	logs, err := itSend(t, work, append(smart, src, target)...)
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	got := newestBackup(t, src, target)
	if got.BaseSnapshot.Name != monthly || got.IncrementalSnapshot.Name != "" {
		t.Fatalf("full: sent %s from %q, want %s full", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, monthly)
	}
	volumes, manifests, list := countObjects(t, client, bucket)
	if volumes < 3 || manifests != 1 {
		t.Fatalf("full: bucket holds %d volumes and %d manifests, want 3+ and 1:\n%s\n%s", volumes, manifests, list, logs)
	}

	// Incremental: the newest _daily, not the _hourly after it.
	writeFile(t, "/"+src, "b.bin", 1<<20)
	daily2 := snapshot(t, src, "daily")
	snapshot(t, src, "hourly")
	if logs, err := itSend(t, work, append(smart, src, target)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	got = newestBackup(t, src, target)
	if got.BaseSnapshot.Name != daily2 || got.IncrementalSnapshot.Name != monthly {
		t.Fatalf("incremental: sent %s from %q, want %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, daily2, monthly)
	}

	// Noop: nothing new with these suffixes.
	if logs, err := itSend(t, work, append(smart, src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("noop: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}

	// Receive the full explicitly. src has b.bin now: compare with a clone of
	// the snapshot. Not .zfs/snapshot: the kernel mounts that with a helper in
	// the host's mount namespace, which a container does not see.
	itReceive(t, work, "-F", src+"@"+monthly, target, restored)
	wantSnapshots(t, restored, monthly)
	zfsRun(t, "clone", "-o", "mountpoint=/"+monthlyClone, src+"@"+monthly, monthlyClone)
	compareDirs(t, "/"+monthlyClone, "/"+restored)

	// receive --auto: monthly is there, so one incremental step.
	itReceive(t, work, "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored)

	// receive --auto into nothing: the full, then the incremental.
	itReceive(t, work, "--auto", "-F", src, target, restored2)
	wantSnapshots(t, restored2, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored2)

	// Pipe mode: volumes stream to S3 multipart uploads and back, never to disk.
	t.Run("Pipe", func(t *testing.T) {
		_, bucket := itBucket(t, "zfsbackup-it-pipe")
		target := "s3://" + bucket + "/"
		restored3 := pool + "/restored3"
		destroyAfter(t, restored3)
		work := t.TempDir()
		if logs, err := itSend(t, work, "--maxFileBuffer", "0", "--volsize", "1", "--full", src, target); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		itReceive(t, work, "--maxFileBuffer", "0", "--auto", "-F", src, target, restored3)
		compareDirs(t, "/"+src, "/"+restored3)
	})
}

// testPool returns ZFSBACKUP_TEST_POOL (default tank) and skips the test when
// zfs has no such pool.
func testPool(t *testing.T) string {
	t.Helper()
	pool := os.Getenv("ZFSBACKUP_TEST_POOL")
	if pool == "" {
		pool = "tank"
	}
	if err := exec.Command("zpool", "list", pool).Run(); err != nil {
		t.Skipf("no pool %s; run through make integration-docker", pool)
	}
	return pool
}

// itBucket creates a bucket named <name>-<unix seconds> and deletes it after the test.
func itBucket(t *testing.T, name string) (client *s3.S3, bucket string) {
	t.Helper()
	bucket = fmt.Sprintf("%s-%d", name, time.Now().Unix())
	client, cleanup := setupS3Bucket(t, bucket)
	t.Cleanup(cleanup)
	return client, bucket
}

// countObjects counts the volumes and the manifests in bucket, and lists them
// with their sizes.
func countObjects(t *testing.T, client *s3.S3, bucket string) (volumes, manifests int, list string) {
	t.Helper()
	var b strings.Builder
	err := client.ListObjectsV2Pages(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)},
		func(page *s3.ListObjectsV2Output, _ bool) bool {
			for _, o := range page.Contents {
				fmt.Fprintf(&b, "%10d %s\n", aws.Int64Value(o.Size), aws.StringValue(o.Key))
				if strings.Contains(aws.StringValue(o.Key), ".manifest") {
					manifests++
				} else {
					volumes++
				}
			}
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	return volumes, manifests, b.String()
}

// zfsRun runs zfs and returns its stdout; it fails the test with zfs's stderr.
func zfsRun(t *testing.T, args ...string) string {
	t.Helper()
	var stderr strings.Builder
	c := exec.Command("zfs", args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		t.Fatalf("zfs %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// destroyAfter destroys the datasets that exist when the test ends.
func destroyAfter(t *testing.T, datasets ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, d := range datasets {
			if exec.Command("zfs", "list", d).Run() == nil {
				zfsRun(t, "destroy", "-r", "-f", d)
			}
		}
	})
}

// writeFile writes size random bytes: the compressor cannot shrink them, so
// --volsize 1 cuts the stream into several volumes.
func writeFile(t *testing.T, dir, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.CopyN(f, rand.Reader, size); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

var lastSnapshot time.Time

// snapshot takes a sanoid-style snapshot of kind (monthly, daily, hourly) and
// returns its name. It first waits for a new second: two snapshots in one
// second tie on creation time, and selection must not depend on tie order.
func snapshot(t *testing.T, dataset, kind string) string {
	t.Helper()
	now := time.Now().UTC()
	for now.Unix() == lastSnapshot.Unix() {
		time.Sleep(50 * time.Millisecond)
		now = time.Now().UTC()
	}
	lastSnapshot = now
	name := "autosnap_" + now.Format("2006-01-02_15:04:05") + "_" + kind
	zfsRun(t, "snapshot", dataset+"@"+name)
	return name
}

// wantSnapshots fails unless dataset has these snapshots, oldest first, and no others.
func wantSnapshots(t *testing.T, dataset string, want ...string) {
	t.Helper()
	out := zfsRun(t, "list", "-H", "-o", "name", "-s", "createtxg", "-t", "snapshot", dataset)
	var got []string
	for _, line := range strings.Fields(out) {
		got = append(got, strings.TrimPrefix(line, dataset+"@"))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%s has snapshots %v, want %v", dataset, got, want)
	}
}

// itSend runs send in-process with the real zfs and returns its logs.
func itSend(t *testing.T, work string, args ...string) (string, error) {
	t.Helper()
	return itRun(cmd.ResetSendJobInfo, "send", work, args...)
}

// itReceive runs receive in-process with the real zfs; it fails the test on an error.
func itReceive(t *testing.T, work string, args ...string) {
	t.Helper()
	if logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", work, args...); err != nil {
		t.Fatalf("receive %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
}

// itRun runs `zfsbackup <command>` as e2eEnv.run does. --maxRetryTime 2m: a
// broken S3 fails the test in minutes, not the 12-hour default.
func itRun(reset func(), command, work string, args ...string) (string, error) {
	base := []string{command, "--zfsPath", "zfs", "--workingDirectory", work, "--logLevel", "debug", "--maxRetryTime", "2m"}
	_, logs, err := (&e2eEnv{}).run(reset, append(base, args...)...)
	return logs, err
}
