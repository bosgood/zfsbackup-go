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
)

// TestIntegrationS3Names backs up a dataset whose name holds a space and a
// colon, both legal in zfs: the object keys, the mountpoints, the lock and
// the list key carry the name through send, list, receive and clean.
func TestIntegrationS3Names(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-names")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/na me:1"
	restored := pool + "/re stored:1"
	work := t.TempDir()
	newSource(t, src, 2<<20)
	s1 := snapshot(t, src, "daily")
	writeFile(t, "/"+src, "b c.bin", 1<<20)
	s2 := snapshot(t, src, "daily")

	mustSend(t, work, append(slices.Clone(plainSend), src+"@"+s1, target)...)
	mustSend(t, work, append(slices.Clone(plainSend), "-i", src+"@"+s1, src+"@"+s2, target)...)
	var manifests int
	for _, name := range objectNames(t, client, bucket, prefix) {
		if strings.Contains(name, ".manifest") {
			manifests++
		}
		if !strings.Contains(name, "na me:1") {
			t.Errorf("object %q does not carry the dataset name", name)
		}
	}
	if manifests != 2 {
		t.Fatalf("%d manifests at %s, want 2", manifests, target)
	}

	got := chainNames(itList(t, t.TempDir(), target)[src])
	if want := []string{s1 + "<-", s2 + "<-" + s1}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("list holds %v under %q, want %v", got, src, want)
	}

	restoreAll(t, src, target, restored, s1, s2)

	logs, err := itClean(t, work, "--dry-run", target)
	if err != nil {
		t.Fatalf("clean --dry-run: %v\n%s", err, logs)
	}
	wantNotInLog(t, logs, "unrecognized", "Would delete")
}
