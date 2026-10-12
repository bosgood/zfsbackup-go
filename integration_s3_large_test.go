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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/dustin/go-humanize"
)

const (
	// largeBytes is what TestIntegrationS3Large sends: random bytes, so the compressor cannot
	// shrink them, and the default --volsize cuts them into largeVolumes volumes.
	largeBytes = 1 << 30
	// largeVolumes is the volumes largeBytes becomes at the default --volsize 200: five of about
	// 200 MiB and one of what is left.
	largeVolumes = 6
	// largePartBytes is the default --uploadChunkSize: each volume's multipart upload has one part
	// per 10 MiB.
	largePartBytes = 10 << 20
	// largeVolumeMin is the least a full volume holds: sendStream cuts when the compressed counter
	// reaches --volsize less 50 KiB. pgzip then flushes a few MiB of blocks (largeVolumeSlack).
	largeVolumeMin   = 200<<20 - 50<<10
	largeVolumeSlack = 16 << 20
)

// TestIntegrationS3Large sends about 1 GiB with the flags production uses (--volsize 200,
// --maxFileBuffer 5, --maxParallelUploads 4, the internal compressor). Each volume is then a
// multipart upload of 10 MiB parts, which no other test reaches: their --volsize 1 objects stay
// under the 5 MiB at which the AWS uploader switches to multipart, and go up in one PUT. It sends
// file-buffered and in pipe mode, restores each with an empty working directory (one
// file-buffered, one in pipe mode), and in the Replication subtest sends a -R stream of a parent
// with two children and receives it as a tree.
func TestIntegrationS3Large(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-large")
	target := "s3://" + bucket + "/" + prefix
	_, bucketPipe, prefixPipe := itBucket(t, "zfsbackup-it-large-pipe")
	targetPipe := "s3://" + bucketPipe + "/" + prefixPipe
	src := pool + "/srclarge"
	restored := pool + "/restoredL"
	restoredPipe := pool + "/restoredLP"
	destroyAfter(t, restoredPipe, restored, src)
	work := t.TempDir()

	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "big.bin", largeBytes)
	snap := snapshot(t, src, "monthly")

	// File-buffered, every flag at its default.
	timed(t, "send file-buffered", func() {
		if logs, err := itSend(t, work, src+"@"+snap, target); err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
	})
	wantLargeVolumes(t, client, bucket, prefix)

	// Pipe mode: the uploader reads a volume of unknown size and still goes multipart.
	timed(t, "send pipe", func() {
		if logs, err := itSend(t, work, "--maxFileBuffer", "0", src+"@"+snap, targetPipe); err != nil {
			t.Fatalf("send --maxFileBuffer 0: %v\n%s", err, logs)
		}
	})
	wantLargeVolumes(t, client, bucketPipe, prefixPipe)

	// Each restore with an empty working directory: the manifest comes from S3.
	timed(t, "receive file-buffered", func() {
		itReceive(t, t.TempDir(), "-F", src+"@"+snap, target, restored)
	})
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)

	timed(t, "receive pipe", func() {
		itReceive(t, t.TempDir(), "--maxFileBuffer", "0", "-F", src+"@"+snap, targetPipe, restoredPipe)
	})
	wantSnapshots(t, restoredPipe, snap)
	wantGUIDs(t, src, restoredPipe, snap)
	compareDirs(t, "/"+src, "/"+restoredPipe)

	// A replication stream (-R) of a parent with two children, received as a tree.
	t.Run("Replication", func(t *testing.T) {
		client, bucket, prefix := itBucket(t, "zfsbackup-it-large-repl")
		target := "s3://" + bucket + "/" + prefix
		parent := pool + "/srcrepl"
		restoredR := pool + "/restoredR"
		destroyAfter(t, restoredR, parent)
		work := t.TempDir()

		// No local mountpoint: a -R stream carries local properties, and a received local
		// mountpoint would mount the restored tree over the source (TestIntegrationS3ReceiveSafety).
		// These inherit the pool's, so the restored tree mounts under its own name.
		children := []string{parent + "/c1", parent + "/c2"}
		zfsRun(t, "create", parent)
		writeFile(t, "/"+parent, "p.bin", 8<<20)
		for _, c := range children {
			zfsRun(t, "create", c)
			writeFile(t, "/"+c, "c.bin", 8<<20)
		}
		snap := snapshot(t, parent, "monthly", "-r")

		if logs, err := itSend(t, work, "-R", parent+"@"+snap, target); err != nil {
			t.Fatalf("send -R: %v\n%s", err, logs)
		}
		// 24 MiB of random data: one volume, three parts.
		sizes := objectSizes(t, client, bucket, prefix)
		for key, size := range sizes {
			if strings.Contains(key, ".manifest") {
				continue
			}
			if size < 24<<20 {
				t.Fatalf("the -R stream's volume %s is %s, want the 24 MiB of three datasets", key, humanize.IBytes(uint64(size)))
			}
			wantMultipart(t, client, bucket, key)
		}

		itReceive(t, t.TempDir(), "-F", parent+"@"+snap, target, restoredR)
		for _, ds := range append([]string{parent}, children...) {
			got := restoredR + strings.TrimPrefix(ds, parent)
			wantSnapshots(t, got, snap)
			wantGUIDs(t, ds, got, snap)
		}
		compareDirs(t, "/"+parent, "/"+restoredR)
	})
}

// wantLargeVolumes fails unless the backup set under prefix holds largeVolumes volumes of the
// sizes largeBytes gives at the default --volsize, each a multipart upload of largePartBytes
// parts, and one manifest.
func wantLargeVolumes(t *testing.T, client *s3.S3, bucket, prefix string) {
	t.Helper()
	sizes := objectSizes(t, client, bucket, prefix)
	var volumes []string
	var manifests int
	for key := range sizes {
		if strings.Contains(key, ".manifest") {
			manifests++
			continue
		}
		volumes = append(volumes, key)
	}
	sort.Strings(volumes)
	var listing []string
	for _, key := range volumes {
		listing = append(listing, fmt.Sprintf("%s %s", humanize.IBytes(uint64(sizes[key])), key))
	}
	t.Logf("s3://%s/%s holds %d volumes and %d manifests:\n%s", bucket, prefix, len(volumes), manifests, strings.Join(listing, "\n"))
	if len(volumes) != largeVolumes || manifests != 1 {
		t.Fatalf("s3://%s/%s holds %d volumes and %d manifests, want %d and 1", bucket, prefix, len(volumes), manifests, largeVolumes)
	}
	var total int64
	for i, key := range volumes {
		size := sizes[key]
		total += size
		// The object names sort by volume number; only the last one is short.
		if i < len(volumes)-1 && (size < largeVolumeMin || size > largeVolumeMin+largeVolumeSlack) {
			t.Errorf("volume %s is %d bytes, want between %d and %d (--volsize 200)", key, size, largeVolumeMin, largeVolumeMin+largeVolumeSlack)
		}
		wantMultipart(t, client, bucket, key)
	}
	if total < largeBytes {
		t.Errorf("the volumes hold %d bytes together, want at least the %d sent (random data does not compress)", total, largeBytes)
	}
}

// wantMultipart fails unless the object at key went up as a multipart upload of largePartBytes
// parts: S3 and MinIO end such an object's ETag with "-<parts>", and an object of one PUT has
// its content's MD5 as the ETag.
func wantMultipart(t *testing.T, client *s3.S3, bucket, key string) {
	t.Helper()
	head, err := client.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("could not head s3://%s/%s: %v", bucket, key, err)
	}
	etag := strings.Trim(aws.StringValue(head.ETag), `"`)
	size := aws.Int64Value(head.ContentLength)
	wantParts := (size + largePartBytes - 1) / largePartBytes
	i := strings.LastIndexByte(etag, '-')
	if i < 0 {
		t.Fatalf("s3://%s/%s (%s) has ETag %s: one PUT, want a multipart upload of %d parts", bucket, key, humanize.IBytes(uint64(size)), etag, wantParts)
	}
	parts, perr := strconv.ParseInt(etag[i+1:], 10, 64)
	if perr != nil || parts != wantParts {
		t.Fatalf("s3://%s/%s (%s) has ETag %s, want a multipart upload of %d parts of %s", bucket, key, humanize.IBytes(uint64(size)), etag, wantParts, humanize.IBytes(largePartBytes))
	}
}

// objectSizes returns the size of each object under prefix in bucket, by key without the prefix.
func objectSizes(t *testing.T, client *s3.S3, bucket, prefix string) map[string]int64 {
	t.Helper()
	sizes := make(map[string]int64)
	err := client.ListObjectsV2Pages(&s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)},
		func(page *s3.ListObjectsV2Output, _ bool) bool {
			for _, o := range page.Contents {
				sizes[strings.TrimPrefix(aws.StringValue(o.Key), prefix)] = aws.Int64Value(o.Size)
			}
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	return sizes
}

// timed runs f and logs how long it took, for the test log of a run that moves gigabytes.
func timed(t *testing.T, what string, f func()) {
	t.Helper()
	start := time.Now()
	f()
	t.Logf("%s took %v", what, time.Since(start).Round(time.Millisecond))
}
