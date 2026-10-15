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
	"slices"
	"strings"
	"testing"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// TestIntegrationS3Corrupt stores a volume that does not match its manifest
// (a flipped byte, a byte short, a byte long) and a manifest that is not a
// manifest. A file-mode receive refuses the volume before zfs receive reads
// it; a pipe-mode receive fails too. Either way no dataset is left behind.
func TestIntegrationS3Corrupt(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-corrupt")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/corrupt"
	work := t.TempDir()
	newSource(t, src, 3<<20)
	s1 := snapshot(t, src, "daily")
	mustSend(t, work, append(slices.Clone(plainSend), src+"@"+s1, target)...)
	m := manifestFor(t, work, src, target, s1)
	if len(m.Volumes) < 3 {
		t.Fatalf("the backup has %d volumes, want 3+", len(m.Volumes))
	}
	vol := m.Volumes[1].ObjectName
	key := prefix + vol
	good := objectBytes(t, client, bucket, key)

	flipped := slices.Clone(good)
	flipped[len(flipped)/2] ^= 0xff
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"Flipped", flipped, vol + " does not match its manifest: got "},
		{"Truncated", good[:len(good)-1], vol + " does not match its manifest: got "},
		{"Longer", append(slices.Clone(good), 0), vol + " does not match its manifest: it is larger than the "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			putObject(t, client, bucket, key, c.data)
			t.Cleanup(func() { putObject(t, client, bucket, key, good) })
			restored := pool + "/corrupt-" + strings.ToLower(c.name)
			destroyAfter(t, restored)

			// File mode: the check refuses the volume before zfs receive sees it.
			// zfs receive is already running on the good volume before it, and is
			// killed ("incomplete stream"); zfs discards the partial dataset.
			logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "-F", src+"@"+s1, target, restored)
			if err == nil {
				t.Fatalf("receive of a %s volume succeeded\n%s", c.name, logs)
			}
			wantInLog(t, logs, c.want)
			wantNoDataset(t, restored)

			// Pipe mode: zfs receive reads the bytes first. A flipped byte fails the
			// stream's own checksum in zfs; a short or long object fails the manifest
			// check. Either way the receive fails and zfs discards the partial dataset.
			logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "--maxFileBuffer", "0", "-F", src+"@"+s1, target, restored)
			if err == nil {
				t.Fatalf("pipe-mode receive of a %s volume succeeded\n%s", c.name, logs)
			}
			t.Logf("pipe mode, %s: %v: %s", c.name, err, zfsLine(logs, "does not match"))
			wantNoDataset(t, restored)
		})
	}

	t.Run("Manifest", func(t *testing.T) {
		var manifestKey string
		for _, name := range objectNames(t, client, bucket, prefix) {
			if strings.Contains(name, ".manifest") {
				manifestKey = prefix + name
			}
		}
		if manifestKey == "" {
			t.Fatal("no manifest object")
		}
		goodManifest := objectBytes(t, client, bucket, manifestKey)
		putObject(t, client, bucket, manifestKey, []byte("this is not a gzip manifest"))
		t.Cleanup(func() { putObject(t, client, bucket, manifestKey, goodManifest) })
		restored := pool + "/corrupt-manifest"
		destroyAfter(t, restored)

		// An empty working directory: the manifest must come from S3.
		_, logs, err := itRunOut(cmd.ResetListJobInfo, "list", t.TempDir(), "zfs", "--jsonOutput", target)
		if err == nil {
			t.Fatalf("list read a manifest that is not gzip\n%s", logs)
		}
		wantInLog(t, logs, strings.TrimPrefix(manifestKey, prefix))
		logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "-F", src+"@"+s1, target, restored)
		if err == nil {
			t.Fatalf("receive with a manifest that is not gzip succeeded\n%s", logs)
		}
		wantNoDataset(t, restored)
	})

	// Everything back in place: the backup restores.
	restoreAll(t, src, target, pool+"/corrupt-restored", s1)
}
