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
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"golang.org/x/crypto/openpgp/packet"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/files"
)

// TestIntegrationS3 runs real zfs against the real s3:// backend: sanoid-style
// snapshots, a smart full, a smart incremental, a noop, and receives of each
// back into new datasets. Run it with make integration-docker (a Linux host
// with the zfs module) or make integration-vm (a Mac); see integration/README.md.
func TestIntegrationS3(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it")
	target := "s3://" + bucket + "/" + prefix
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
	volumes, manifests, list := countObjects(t, client, bucket, prefix)
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
	wantGUIDs(t, src, restored, monthly)
	zfsRun(t, "clone", "-o", "mountpoint=/"+monthlyClone, src+"@"+monthly, monthlyClone)
	compareDirs(t, "/"+monthlyClone, "/"+restored)

	// receive --auto: monthly is there, so one incremental step.
	itReceive(t, work, "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, monthly, daily2)
	wantGUIDs(t, src, restored, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored)

	// receive --auto into nothing: the full, then the incremental.
	itReceive(t, work, "--auto", "-F", src, target, restored2)
	wantSnapshots(t, restored2, monthly, daily2)
	wantGUIDs(t, src, restored2, monthly, daily2)
	compareDirs(t, "/"+src, "/"+restored2)

	// Pipe mode: volumes stream to S3 multipart uploads and back, never to disk.
	t.Run("Pipe", func(t *testing.T) {
		_, bucket, prefix := itBucket(t, "zfsbackup-it-pipe")
		target := "s3://" + bucket + "/" + prefix
		restored3 := pool + "/restored3"
		destroyAfter(t, restored3)
		work := t.TempDir()
		if logs, err := itSend(t, work, "--maxFileBuffer", "0", "--volsize", "1", "--full", src, target); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		itReceive(t, work, "--maxFileBuffer", "0", "--auto", "-F", src, target, restored3)
		sent := newestBackup(t, src, target).BaseSnapshot.Name
		wantSnapshots(t, restored3, sent)
		wantGUIDs(t, src, restored3, sent)
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

// itBucket returns where the test writes: a new bucket <name>-<unix seconds>,
// deleted after the test, or with ZFSBACKUP_IT_BUCKET set (real AWS S3) the
// prefix <name>-<unix seconds>/ in that bucket, whose objects are deleted
// after the test. The SDK's usual chain gives the credentials.
func itBucket(t *testing.T, name string) (client *s3.S3, bucket, prefix string) {
	t.Helper()
	unique := fmt.Sprintf("%s-%d", name, time.Now().Unix())
	bucket = os.Getenv("ZFSBACKUP_IT_BUCKET")
	if bucket == "" {
		var cleanup func()
		client, cleanup = setupS3Bucket(t, unique)
		t.Cleanup(cleanup)
		return client, unique, ""
	}
	sess, err := session.NewSession(aws.NewConfig().
		WithS3ForcePathStyle(true).
		WithEndpoint(os.Getenv("AWS_S3_CUSTOM_ENDPOINT")))
	if err != nil {
		t.Fatal(err)
	}
	client = s3.New(sess)
	prefix = unique + "/"
	t.Cleanup(func() {
		err := client.ListObjectsV2Pages(&s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)},
			func(page *s3.ListObjectsV2Output, _ bool) bool {
				for _, o := range page.Contents {
					if _, derr := client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); derr != nil {
						t.Errorf("could not delete s3://%s/%s: %v", bucket, aws.StringValue(o.Key), derr)
					}
				}
				return true
			})
		if err != nil {
			t.Errorf("could not list s3://%s/%s: %v", bucket, prefix, err)
		}
	})
	return client, bucket, prefix
}

// countObjects counts the volumes and the manifests under prefix in bucket,
// and lists them with their sizes.
func countObjects(t *testing.T, client *s3.S3, bucket, prefix string) (volumes, manifests int, list string) {
	t.Helper()
	var b strings.Builder
	err := client.ListObjectsV2Pages(&s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)},
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

// snapshotTie takes one sanoid-style snapshot of dataset per kind, all in one
// creation second, as sanoid's monthly, daily and hourly tie on the 1st. It
// returns the names in the order of kinds. zfs takes one snapshot of a dataset
// per call (and per channel program), so it takes them one by one right after
// a second starts, and starts again if the second rolled over meanwhile.
func snapshotTie(t *testing.T, dataset string, kinds ...string) []string {
	t.Helper()
	for try := 0; try < 3; try++ {
		time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
		now := time.Now().UTC()
		lastSnapshot = now
		names := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			name := "autosnap_" + now.Format("2006-01-02_15:04:05") + "_" + kind
			zfsRun(t, "snapshot", dataset+"@"+name)
			names = append(names, name)
		}
		created := zfsGet(t, dataset+"@"+names[0], "creation")
		tie := true
		for _, n := range names[1:] {
			tie = tie && zfsGet(t, dataset+"@"+n, "creation") == created
		}
		if tie {
			return names
		}
		for _, n := range names {
			zfsRun(t, "destroy", dataset+"@"+n)
		}
	}
	t.Fatalf("could not take %v of %s in one second", kinds, dataset)
	return nil
}

// zfsGet returns the parsable value of property of a dataset, snapshot or bookmark.
func zfsGet(t *testing.T, name, property string) string {
	t.Helper()
	return strings.TrimSpace(zfsRun(t, "get", "-H", "-p", "-o", "value", property, name))
}

// wantGUIDs fails unless each snapshot of restored has the guid of the
// snapshot of that name on src: a received snapshot keeps the sender's guid
// when the stream arrived intact. Where src has only a bookmark of the name
// (the snapshot was destroyed after the backup), it compares the bookmark's.
func wantGUIDs(t *testing.T, src, restored string, snaps ...string) {
	t.Helper()
	for _, s := range snaps {
		from := src + "@" + s
		if exec.Command("zfs", "list", "-t", "snapshot", from).Run() != nil {
			from = src + "#" + s
		}
		if got, want := zfsGet(t, restored+"@"+s, "guid"), zfsGet(t, from, "guid"); got != want {
			t.Fatalf("%s@%s has guid %s, %s has %s", restored, s, got, from, want)
		}
	}
}

// objectBytes returns the object at key in bucket.
func objectBytes(t *testing.T, client *s3.S3, bucket, key string) []byte {
	t.Helper()
	out, err := client.GetObject(&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("could not get s3://%s/%s: %v", bucket, key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("could not read s3://%s/%s: %v", bucket, key, err)
	}
	return data
}

// wantPGPVolume fails unless data starts with a public-key encrypted session
// key: the first packet openpgp.Encrypt writes. A signed-only object starts
// with a one-pass signature instead.
func wantPGPVolume(t *testing.T, name string, data []byte) {
	t.Helper()
	p, err := packet.NewReader(bytes.NewReader(data)).Next()
	if err != nil {
		t.Fatalf("%s: no OpenPGP packet: %v", name, err)
	}
	if _, ok := p.(*packet.EncryptedKey); !ok {
		t.Fatalf("%s starts with a %T, want a *packet.EncryptedKey", name, p)
	}
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

// itRun runs `zfsbackup <command>` with the real zfs and returns its logs.
func itRun(reset func(), command, work string, args ...string) (string, error) {
	_, logs, err := itRunOut(reset, command, work, "zfs", args...)
	return logs, err
}

// itRunOut runs `zfsbackup <command>` as e2eEnv.run does, with zfsPath as the
// zfs binary, and returns what it printed and logged. --maxRetryTime 2m (only
// send and receive have it): a broken S3 fails the test in minutes, not the
// 12-hour default.
func itRunOut(reset func(), command, work, zfsPath string, args ...string) (out, logs string, err error) {
	base := []string{command, "--zfsPath", zfsPath, "--workingDirectory", work, "--logLevel", "debug"}
	if command == "send" || command == "receive" {
		base = append(base, "--maxRetryTime", "2m")
	}
	return (&e2eEnv{}).run(reset, append(base, args...)...)
}

// itList runs `list --jsonOutput` and returns the backups it printed: by
// dataset, oldest first. It fails the test on an error.
func itList(t *testing.T, work string, args ...string) map[string][]*files.JobInfo {
	t.Helper()
	out, logs, err := itRunOut(cmd.ResetListJobInfo, "list", work, "zfs", append([]string{"--jsonOutput"}, args...)...)
	if err != nil {
		t.Fatalf("list %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
	var got map[string][]*files.JobInfo
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("list %s printed no JSON: %v\n%s\n%s", strings.Join(args, " "), err, out, logs)
	}
	return got
}

// itClean runs clean and returns its logs. The bool flags leak between
// in-process runs, so it passes each one; a value in args comes after and wins.
func itClean(t *testing.T, work string, args ...string) (string, error) {
	t.Helper()
	base := []string{"--dry-run=false", "--force=false", "--cleanLocal=false"}
	_, logs, err := itRunOut(cmd.ResetReceiveJobInfo, "clean", work, "zfs", append(base, args...)...)
	return logs, err
}

// objectNames returns the keys under prefix in bucket, sorted, without the prefix.
func objectNames(t *testing.T, client *s3.S3, bucket, prefix string) []string {
	t.Helper()
	var names []string
	err := client.ListObjectsV2Pages(&s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)},
		func(page *s3.ListObjectsV2Output, _ bool) bool {
			for _, o := range page.Contents {
				names = append(names, strings.TrimPrefix(aws.StringValue(o.Key), prefix))
			}
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

// putObject writes data to bucket at key.
func putObject(t *testing.T, client *s3.S3, bucket, key string, data []byte) {
	t.Helper()
	if _, err := client.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
		t.Fatalf("could not put s3://%s/%s: %v", bucket, key, err)
	}
}

// deleteObject deletes key in bucket.
func deleteObject(t *testing.T, client *s3.S3, bucket, key string) {
	t.Helper()
	if _, err := client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		t.Fatalf("could not delete s3://%s/%s: %v", bucket, key, err)
	}
}

// cloneOf clones src@snap to <pool>/name, mounted at /<pool>/name, and returns
// that path. Callers put the clone in destroyAfter before src.
func cloneOf(t *testing.T, pool, src, snap, name string) string {
	t.Helper()
	clone := pool + "/" + name
	zfsRun(t, "clone", "-o", "mountpoint=/"+clone, src+"@"+snap, clone)
	return "/" + clone
}

// manifestFor returns the backup of src@snap at target, as list reads it with
// work as the working directory. extra goes to list: the rings of an encrypted
// backup, a --manifestPrefix. It fails the test if there is none.
func manifestFor(t *testing.T, work, src, target, snap string, extra ...string) *files.JobInfo {
	t.Helper()
	var names []string
	for _, b := range itList(t, work, append(slices.Clone(extra), target)...)[src] {
		if b.BaseSnapshot.Name == snap {
			return b
		}
		names = append(names, b.BaseSnapshot.Name)
	}
	t.Fatalf("no backup of %s@%s at %s; there are %v", src, snap, target, names)
	return nil
}

// chainNames returns "base<-source" for each backup, oldest first: a full is "base<-".
func chainNames(backups []*files.JobInfo) []string {
	names := make([]string, 0, len(backups))
	for _, b := range backups {
		names = append(names, b.BaseSnapshot.Name+"<-"+b.IncrementalSnapshot.Name)
	}
	return names
}

// zfsLine returns the first line of logs that holds want: zfs's own message
// among the program's, for the test log.
func zfsLine(logs, want string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, want) {
			return strings.TrimSpace(line)
		}
	}
	return "(no line holds " + strconv.Quote(want) + ")"
}
