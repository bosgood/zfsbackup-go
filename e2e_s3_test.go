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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// coldS3Set sends a 3-volume set to env.dest and mirrors it into a fake S3
// bucket whose volumes are in class, returning the fake and the volume names.
func coldS3Set(t *testing.T, env *e2eEnv, class string) (*fakeS3, []string) {
	t.Helper()
	vols := env.sentSet(t)
	objs := map[string][]byte{}
	for name, data := range destObjects(t, env.dest) {
		objs[name] = data
	}
	f := newFakeS3(t, objs)
	f.storageClass = make(map[string]string)
	for _, v := range vols {
		f.storageClass[v] = class
	}
	t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
	return f, vols
}

// receivedOnce asserts the fake zfs ran exactly one receive, logged at path.
func receivedOnce(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("zfs receive was not run: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Errorf("want 1 zfs receive, got %d:\n%s", len(lines), data)
	}
}

func restoresOf(f *fakeS3) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := append([]string(nil), f.restores...)
	sort.Strings(got)
	return got
}

// Volumes a lifecycle rule moved to DEEP_ARCHIVE: receive must ask S3 to
// restore them, once each, instead of retrying GETs that can never succeed.
func TestE2ES3ReceiveRestoresDeepArchive(t *testing.T) {
	env := newE2EEnv(t)
	f, vols := coldS3Set(t, env, "DEEP_ARCHIVE")
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--maxRetryTime", "20s", "tank/data@a", "s3://bucket", "restored/data")
	})
	if err != nil {
		t.Fatalf("receive from DEEP_ARCHIVE volumes failed: %v\n%s", err, logs)
	}
	want := append([]string(nil), vols...)
	sort.Strings(want)
	if got := restoresOf(f); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("restore requests %v, want one per volume %v", got, want)
	}
	receivedOnce(t, receiveLog)
}

// Intelligent-Tiering moves an object it has not read in months to an archive tier: its
// storage class stays INTELLIGENT_TIERING, and only x-amz-archive-status says a GET will
// fail until it is restored.
func TestE2ES3ReceiveRestoresIntelligentTieringArchive(t *testing.T) {
	for _, status := range []string{"ARCHIVE_ACCESS", "DEEP_ARCHIVE_ACCESS"} {
		t.Run(status, func(t *testing.T) {
			env := newE2EEnv(t)
			f, vols := coldS3Set(t, env, "INTELLIGENT_TIERING")
			f.archiveStatus = make(map[string]string)
			for _, v := range vols {
				f.archiveStatus[v] = status
			}
			receiveLog := filepath.Join(t.TempDir(), "receive.log")
			t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
			logs, err := guarded(t, func() (string, error) {
				return env.receive("--maxRetryTime", "5s", "tank/data@a", "s3://bucket", "restored/data")
			})
			if err != nil {
				t.Fatalf("receive from %s volumes failed: %v\n%s", status, err, logs)
			}
			want := append([]string(nil), vols...)
			sort.Strings(want)
			if got := restoresOf(f); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("restore requests %v, want one per volume %v", got, want)
			}
			receivedOnce(t, receiveLog)
		})
	}
}

// A restored object's HEAD may not carry x-amz-restore yet: the wait loop must
// treat that as "not yet", not dereference nil.
func TestS3PreDownloadNilRestoreHeader(t *testing.T) {
	env := newE2EEnv(t)
	f, _ := coldS3Set(t, env, "GLACIER")
	f.headsWithoutRestoreHeader = 1
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--maxRetryTime", "20s", "tank/data@a", "s3://bucket", "restored/data")
	})
	if err != nil {
		t.Fatalf("receive from GLACIER volumes failed: %v\n%s", err, logs)
	}
	receivedOnce(t, receiveLog)
}

// A set whose manifest is missing at an S3 destination with GLACIER volumes:
// the smart run that completes it must restore the volumes before hashing them.
func TestE2ES3CompletePartialThawsFirst(t *testing.T) {
	env := newE2EEnv(t)
	f, vols := coldS3Set(t, env, "GLACIER")
	var manifest string
	f.mu.Lock()
	for name := range f.objects {
		if strings.HasPrefix(name, "manifests|") {
			manifest = name
			delete(f.objects, name)
		}
	}
	f.mu.Unlock()
	if manifest == "" {
		t.Fatal("no manifest in the mirrored set")
	}
	dests := "file://" + env.dest + ",s3://bucket"
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	logs, err := guarded(t, func() (string, error) {
		return env.send("--fullIfOlderThan", "720h", "--compressor", "", "--maxRetryTime", "20s", "tank/data", dests)
	})
	if err != nil {
		t.Fatalf("completing the partial set failed: %v\n%s", err, logs)
	}
	want := append([]string(nil), vols...)
	sort.Strings(want)
	if got := restoresOf(f); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("restore requests %v, want one per volume %v", got, want)
	}
	f.mu.Lock()
	_, ok := f.objects[manifest]
	f.mu.Unlock()
	if !ok {
		t.Errorf("the manifest %s was not uploaded to the S3 destination", manifest)
	}
}
