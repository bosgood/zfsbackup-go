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
	"compress/gzip"
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// dropVolumesFrom deletes the volumes numbered from and up at dir, as if the interrupted attempt
// had stopped before uploading them.
func dropVolumesFrom(t *testing.T, dir string, from int64) {
	t.Helper()
	for name := range destObjects(t, dir) {
		if _, _, _, n, ok := files.ParseBackupVolumeObjectName(name, "|"); ok && n >= from {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A --resume skips the bytes the interrupted attempt already uploaded. If the stream is not the
// same one (the snapshot was destroyed and recreated under its name, or its contents differ), the
// set would be the old stream's start spliced onto the new one's end: the resume must refuse.
func TestE2EResumeRefusesChangedStream(t *testing.T) {
	const streamBytes = 4 << 20
	for _, tc := range []struct {
		name     string
		creation time.Time // of the snapshot at resume time
		want     string
	}{
		{"recreated", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), "option mismatch: guid of a differs"},
		// Only the stream's bytes differ: same creation, same guid. Only its hash tells.
		{"salt only", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "zfs stream differs from the interrupted attempt's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			args := []string{"--volsize", "1", "--compressor", ""}
			env.interruptedSend(t, streamBytes, env.dest, args...)
			dropVolumesFrom(t, env.dest, 3)
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: tc.creation}})
			t.Setenv("FAKEZFS_STREAM_SALT", "recreated")

			logs, err := guarded(t, func() (string, error) {
				return env.send(append(args, "--resume", "tank/data@a", "file://"+env.dest)...)
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resume against another stream: got %v, want %q:\n%s", err, tc.want, logs)
			}
			if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
				t.Errorf("the refused resume published manifests %q", names)
			}
		})
	}
}

// A key rotated under the same address between the attempts: the resumed set would be encrypted
// to two keys, half each.
func TestE2EResumeRefusesRotatedKey(t *testing.T) {
	env := newE2EEnv(t)
	k1, k2 := newKey(t), newKey(t)
	args := []string{"--volsize", "1", "--compressor", ""}
	env.interruptedSend(t, 4<<20, env.dest, append(args, writeRings(t, k1)...)...)
	dropVolumesFrom(t, env.dest, 3)

	// k2 first: it is the key backup@example.com now resolves to; k1 still reads the cache.
	resume := append(append(args, writeRings(t, k2, k1)...), "--resume", "tank/data@a", "file://"+env.dest)
	logs, err := guarded(t, func() (string, error) { return env.send(resume...) })
	if err == nil || !strings.Contains(err.Error()+logs, "option mismatch") {
		t.Errorf("resume with a rotated key: got %v, want an option mismatch:\n%s", err, logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("the refused resume published manifests %q", names)
	}
}

// A partial manifest written before resumes recorded stream hashes cannot prove anything about
// the stream: resume refuses it rather than trusting it.
func TestE2EResumeRefusesUnhashedPartial(t *testing.T) {
	env := newE2EEnv(t)
	args := []string{"--volsize", "1", "--compressor", ""}
	env.interruptedSend(t, 3<<20, env.dest, args...)
	dropVolumesFrom(t, env.dest, 3)
	paths, err := filepath.Glob(filepath.Join(env.work, "cache", "*", "*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("want one cached manifest, got %q (%v)", paths, err)
	}
	rewriteManifest(t, paths[0], func(m map[string]interface{}) {
		for _, v := range m["Volumes"].([]interface{}) {
			delete(v.(map[string]interface{}), "StreamSHA256")
		}
	})

	logs, err := guarded(t, func() (string, error) {
		return env.send(append(args, "--resume", "tank/data@a", "file://"+env.dest)...)
	})
	if err == nil || !strings.Contains(err.Error(), "start over without --resume") {
		t.Errorf("resume of a partial without stream hashes: got %v, want a refusal:\n%s", err, logs)
	}
}

// rewriteManifest edits an unencrypted, gzipped manifest file in place.
func rewriteManifest(t *testing.T, path string, edit func(map[string]interface{})) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]interface{})
	err = json.NewDecoder(zr).Decode(&m)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	edit(m)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err = json.NewEncoder(zw).Encode(m); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = ioutil.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
