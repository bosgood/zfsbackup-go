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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// Completing a partial set downloads its manifest from a destination that has it. A lifecycle
// rule may have moved that manifest to GLACIER with its volumes, and the cache (saveManifest
// writes it for every destination) means nothing else thaws it: copyManifest must.
func TestE2ECompletePartialThawsColdManifest(t *testing.T) {
	env := newE2EEnv(t)
	vols := env.sentSet(t)
	objs := destObjects(t, env.dest)
	manifests := manifestNames(objs)
	if len(manifests) != 1 {
		t.Fatalf("want one manifest, got %q", manifests)
	}
	manifest := manifests[0]
	f := newFakeS3(t, objs)
	f.storageClass = map[string]string{manifest: "GLACIER"}
	for _, v := range vols {
		f.storageClass[v] = "GLACIER"
	}
	t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
	if err := os.WriteFile(cachePathIn(t, env.work, "s3://bucket", manifest), objs[manifest], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(env.dest, manifest)); err != nil {
		t.Fatal(err)
	}
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	logs, err := guarded(t, func() (string, error) {
		return env.send("--fullIfOlderThan", "720h", "--compressor", "", "--maxRetryTime", "5s", "tank/data", "s3://bucket,file://"+env.dest)
	})
	if err != nil {
		t.Fatalf("completing the partial set from a cold manifest failed: %v\nrestores: %v\n%s", err, restoresOf(f), logs)
	}
	thawed := false
	for _, key := range restoresOf(f) {
		thawed = thawed || strings.Contains(key, manifest)
	}
	if !thawed {
		t.Errorf("the manifest was not restored from GLACIER; restores: %v", restoresOf(f))
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 1 {
		t.Errorf("the manifest was not copied to the file destination: %q", names)
	}
}
