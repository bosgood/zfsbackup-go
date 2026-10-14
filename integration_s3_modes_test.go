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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
)

// plainSend are the send flags of these tests: several 1 MiB volumes of
// uncompressed stream, so a 3 MiB dataset gives 3+ objects.
var plainSend = []string{"--volsize", "1", "--compressor", ""}

// newSource makes dataset src (mounted at /<src>) with a random file of size
// bytes, and destroys it when the test ends.
func newSource(t *testing.T, src string, size int64) {
	t.Helper()
	destroyAfter(t, src)
	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", size)
}

// mustSend runs send with args and fails the test on an error. It returns the logs.
func mustSend(t *testing.T, work string, args ...string) string {
	t.Helper()
	logs, err := itSend(t, work, args...)
	if err != nil {
		t.Fatalf("send %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
	return logs
}

// restoreAll receives the whole chain of src at target into restored, from an
// empty working directory, and checks the snapshots, their guids and the files.
func restoreAll(t *testing.T, src, target, restored string, snaps ...string) {
	t.Helper()
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "--auto", "-F", src, target, restored)
	wantSnapshots(t, restored, snaps...)
	wantGUIDs(t, src, restored, snaps...)
	compareDirs(t, "/"+src, "/"+restored)
}

// TestIntegrationS3SendModes runs the send flags that no other real-zfs test
// runs: -n, --increment, -I, -s, --compressor zfs, the external compressors
// and -D. Each subtest has its own dataset and bucket.
func TestIntegrationS3SendModes(t *testing.T) {
	pool := testPool(t)
	t.Run("DryRun", func(t *testing.T) { testSendDryRun(t, pool) })
	t.Run("Increment", func(t *testing.T) { testSendIncrement(t, pool) })
	t.Run("Intermediary", func(t *testing.T) { testSendIntermediary(t, pool) })
	t.Run("SkipMissing", func(t *testing.T) { testSendSkipMissing(t, pool) })
	t.Run("ZfsCompressor", func(t *testing.T) { testSendZfsCompressor(t, pool) })
	t.Run("External", func(t *testing.T) { testSendExternal(t, pool) })
	t.Run("Dedup", func(t *testing.T) { testSendDedup(t, pool) })
}

// testSendDryRun: send -n connects to S3, names what it would send, and
// uploads nothing; before and after a real full.
func testSendDryRun(t *testing.T, pool string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-dry")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/modes-dry"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	s1 := snapshot(t, src, "daily")

	logs := mustSend(t, work, append(slices.Clone(plainSend), "-n", "--full", src, target)...)
	wantInLog(t, logs,
		"Dry-run: would perform a full backup of "+src+"@"+s1,
		"Dry-run: would upload to 1 destination(s)",
		"Dry-run: estimated ZFS stream size",
	)
	if volumes, manifests, list := countObjects(t, client, bucket, prefix); volumes+manifests != 0 {
		t.Fatalf("a dry-run full uploaded %d volumes and %d manifests:\n%s", volumes, manifests, list)
	}

	mustSend(t, work, append(slices.Clone(plainSend), "--full", src, target)...)
	volumes, manifests, _ := countObjects(t, client, bucket, prefix)
	writeFile(t, "/"+src, "b.bin", 1<<20)
	s2 := snapshot(t, src, "daily")

	logs = mustSend(t, work, append(slices.Clone(plainSend), "-n", "--increment", src, target)...)
	wantInLog(t, logs,
		"Dry-run: would perform a incremental backup of "+src+"@"+s2,
		"Dry-run: incremental from "+s1,
		"zfs send -i "+s1,
	)
	if v, m, list := countObjects(t, client, bucket, prefix); v != volumes || m != manifests {
		t.Fatalf("a dry-run incremental changed the bucket: %d volumes and %d manifests, had %d and %d:\n%s", v, m, volumes, manifests, list)
	}
	// The dry-run did not take the send lock: a real send follows at once.
	mustSend(t, work, append(slices.Clone(plainSend), "--increment", src, target)...)
	if got := newestBackup(t, src, target); got.BaseSnapshot.Name != s2 || got.IncrementalSnapshot.Name != s1 {
		t.Fatalf("after the dry-run, send sent %s from %q, want %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, s2, s1)
	}
}

// testSendIncrement: --increment with no suffixes sends the newest snapshot
// from the newest backup at the target, then nothing.
func testSendIncrement(t *testing.T, pool string) {
	_, bucket, prefix := itBucket(t, "zfsbackup-it-incr")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/modes-incr"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	s1 := snapshot(t, src, "daily")
	mustSend(t, work, append(slices.Clone(plainSend), "--full", src, target)...)
	writeFile(t, "/"+src, "b.bin", 1<<20)
	snapshot(t, src, "daily")
	writeFile(t, "/"+src, "c.bin", 1<<20)
	s3 := snapshot(t, src, "daily")

	mustSend(t, work, append(slices.Clone(plainSend), "--increment", src, target)...)
	if got := newestBackup(t, src, target); got.BaseSnapshot.Name != s3 || got.IncrementalSnapshot.Name != s1 {
		t.Fatalf("--increment sent %s from %q, want %s from %s", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, s3, s1)
	}
	if logs, err := itSend(t, work, append(slices.Clone(plainSend), "--increment", src, target)...); !errors.Is(err, backup.ErrNoOp) {
		t.Fatalf("second --increment: got %v, want %v\n%s", err, backup.ErrNoOp, logs)
	}
	// The middle snapshot is not in the chain: the restore has s1 and s3.
	restoreAll(t, src, target, pool+"/modes-incr-restored", s1, s3)
}

// testSendIntermediary: -I carries every snapshot between the source and the
// base; the restore lists them all with the sender's guids.
func testSendIntermediary(t *testing.T, pool string) {
	_, bucket, prefix := itBucket(t, "zfsbackup-it-inter")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/modes-inter"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	s1 := snapshot(t, src, "daily")
	mustSend(t, work, append(slices.Clone(plainSend), src+"@"+s1, target)...)
	writeFile(t, "/"+src, "b.bin", 1<<20)
	s2 := snapshot(t, src, "daily")
	writeFile(t, "/"+src, "c.bin", 1<<20)
	s3 := snapshot(t, src, "daily")

	logs := mustSend(t, work, append(slices.Clone(plainSend), "-I", src+"@"+s1, src+"@"+s3, target)...)
	wantInLog(t, logs, "zfs send -I "+s1+" "+src+"@"+s3)
	m := manifestFor(t, work, src, target, s3)
	if !m.IntermediaryIncremental || m.IncrementalSnapshot.Name != s1 {
		t.Fatalf("manifest of %s: intermediary %v from %q, want true from %s", s3, m.IntermediaryIncremental, m.IncrementalSnapshot.Name, s1)
	}
	restoreAll(t, src, target, pool+"/modes-inter-restored", s1, s2, s3)

	// Explicit: the full, then the -I backup named by its source.
	restored := pool + "/modes-inter-explicit"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "-F", src+"@"+s1, target, restored)
	itReceive(t, t.TempDir(), "-F", "-i", src+"@"+s1, src+"@"+s3, target, restored)
	wantSnapshots(t, restored, s1, s2, s3)
	wantGUIDs(t, src, restored, s1, s2, s3)
}

// testSendSkipMissing: a -R send of a tree where one child lacks the snapshot
// fails without -s and skips that child with it; the restore has the other.
func testSendSkipMissing(t *testing.T, pool string) {
	_, bucket, prefix := itBucket(t, "zfsbackup-it-skip")
	target := "s3://" + bucket + "/" + prefix
	parent := pool + "/modes-skip"
	c1, c2 := parent+"/c1", parent+"/c2"
	work := t.TempDir()
	// No local mountpoint: a -R stream carries local properties, and a received
	// local mountpoint would mount the restored tree over the source
	// (TestIntegrationS3ReceiveSafety). The pool mounts at /<pool>.
	destroyAfter(t, parent)
	zfsRun(t, "create", parent)
	writeFile(t, "/"+parent, "a.bin", 1<<20)
	for _, c := range []string{c1, c2} {
		zfsRun(t, "create", c)
		writeFile(t, "/"+c, "a.bin", 1<<20)
	}
	s1 := snapshot(t, parent, "daily", "-r")
	writeFile(t, "/"+parent, "b.bin", 1<<20)
	writeFile(t, "/"+c1, "b.bin", 1<<20)
	// parent@s2 and c1@s2 in one call; c2 has no s2.
	s2 := snapshotAcross(t, "daily", parent, c1)

	logs, err := itSend(t, work, append(slices.Clone(plainSend), "-R", parent+"@"+s2, target)...)
	if err == nil {
		t.Fatalf("send -R with a child that lacks %s succeeded without -s\n%s", s2, logs)
	}
	t.Logf("send -R without -s: %v: %s", err, zfsLine(logs, c2))
	if got := itList(t, work, target)[parent]; len(got) != 0 {
		t.Fatalf("the failed send left %d backups at %s", len(got), target)
	}

	logs = mustSend(t, work, append(slices.Clone(plainSend), "-R", "-s", parent+"@"+s2, target)...)
	wantInLog(t, logs, "zfs send -R -s "+parent+"@"+s2)

	restored := pool + "/modes-skip-restored"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "-F", parent+"@"+s2, target, restored)
	// A full -R stream carries every snapshot up to s2 of each dataset it has.
	wantSnapshots(t, restored, s1, s2)
	wantSnapshots(t, restored+"/c1", s1, s2)
	wantGUIDs(t, parent, restored, s1, s2)
	wantGUIDs(t, c1, restored+"/c1", s1, s2)
	wantNoDataset(t, restored+"/c2")
	// The parent's own files: its guids match above; the source tree also has
	// c2 mounted, which the restore does not, so compare the child alone.
	compareDirs(t, "/"+c1, "/"+restored+"/c1")
}

// testSendZfsCompressor: --compressor zfs sends a compressed stream (zfs send
// -c); the objects are smaller than the data, the restore is the same files.
func testSendZfsCompressor(t *testing.T, pool string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-zfsc")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/modes-zfsc"
	work := t.TempDir()
	destroyAfter(t, src)
	zfsRun(t, "create", "-o", "mountpoint=/"+src, "-o", "compression=lz4", src)
	writeFile(t, "/"+src, "a.bin", 1<<20)
	// 8 MiB that lz4 shrinks to almost nothing: a plain stream would be 9+ MiB.
	if err := os.WriteFile(filepath.Join("/"+src, "text.txt"), bytes.Repeat([]byte("zfsbackup "), 8<<20/10), 0o644); err != nil {
		t.Fatal(err)
	}
	s1 := snapshot(t, src, "daily")
	logs := mustSend(t, work, "--compressor", "zfs", "--volsize", "1", src+"@"+s1, target)
	wantInLog(t, logs, "zfs send -c "+src+"@"+s1)
	if m := manifestFor(t, work, src, target, s1); m.Compressor != "zfs" {
		t.Fatalf("manifest compressor is %q, want zfs", m.Compressor)
	}
	var total int64
	for name, size := range objectSizes(t, client, bucket, prefix) {
		if !strings.Contains(name, ".manifest") {
			total += size
		}
	}
	if total > 4<<20 {
		t.Fatalf("the volumes hold %d bytes, want under 4 MiB: zfs send -c did not compress", total)
	}

	writeFile(t, "/"+src, "b.bin", 1<<20)
	s2 := snapshot(t, src, "daily")
	mustSend(t, work, "--compressor", "zfs", "--volsize", "1", "-i", src+"@"+s1, src+"@"+s2, target)
	restoreAll(t, src, target, pool+"/modes-zfsc-restored", s1, s2)
}

// externalMagic is the first bytes of each compressor's output.
var externalMagic = map[string][]byte{
	"gzip": {0x1f, 0x8b},
	"pigz": {0x1f, 0x8b},
	"xz":   {0xfd, '7', 'z', 'X', 'Z', 0x00},
	"zstd": {0x28, 0xb5, 0x2f, 0xfd},
}

// testSendExternal: each external compressor at --compressionLevel 1 writes
// its own format; receive decompresses it with the same binary.
func testSendExternal(t *testing.T, pool string) {
	src := pool + "/modes-ext"
	newSource(t, src, 2<<20)
	if err := os.WriteFile(filepath.Join("/"+src, "text.txt"), bytes.Repeat([]byte("zfsbackup "), 2<<20/10), 0o644); err != nil {
		t.Fatal(err)
	}
	s1 := snapshot(t, src, "daily")
	for _, name := range []string{"gzip", "pigz", "xz", "zstd"} {
		t.Run(name, func(t *testing.T) {
			if _, err := exec.LookPath(name); err != nil {
				t.Skipf("no %s in PATH (the integration image has it)", name)
			}
			client, bucket, prefix := itBucket(t, "zfsbackup-it-"+name)
			target := "s3://" + bucket + "/" + prefix
			work := t.TempDir()
			mustSend(t, work, "--compressor", name, "--compressionLevel", "1", "--volsize", "1", src+"@"+s1, target)
			m := manifestFor(t, work, src, target, s1)
			if m.Compressor != name || m.CompressionLevel != 1 {
				t.Fatalf("manifest: compressor %q level %d, want %s level 1", m.Compressor, m.CompressionLevel, name)
			}
			data := objectBytes(t, client, bucket, prefix+m.Volumes[0].ObjectName)
			if !bytes.HasPrefix(data, externalMagic[name]) {
				t.Fatalf("volume 1 starts with % x, want the %s magic % x", data[:8], name, externalMagic[name])
			}
			restored := pool + "/modes-ext-" + name
			destroyAfter(t, restored)
			itReceive(t, t.TempDir(), "-F", src+"@"+s1, target, restored)
			wantGUIDs(t, src, restored, s1)
			compareDirs(t, "/"+src, "/"+restored)
		})
	}
}

// testSendDedup: -D (deduplicated streams are deprecated in zfs 2.x; zfs 2.4
// sends a plain stream and says nothing) still sends and restores.
func testSendDedup(t *testing.T, pool string) {
	_, bucket, prefix := itBucket(t, "zfsbackup-it-dedup")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/modes-dedup"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	s1 := snapshot(t, src, "daily")
	logs := mustSend(t, work, append(slices.Clone(plainSend), "-D", src+"@"+s1, target)...)
	wantInLog(t, logs, "zfs send -D "+src+"@"+s1)
	restoreAll(t, src, target, pool+"/modes-dedup-restored", s1)
}
