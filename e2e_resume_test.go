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
	"fmt"
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

// newDest makes another empty file:// destination directory.
func newDest(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dest2")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --resume completes a set whose manifest is missing at a destination by copying it there. It
// must not vouch for volumes there that are not the ones the manifest describes.
func TestE2EResumeRefusesToCompleteOverForeignVolumes(t *testing.T) {
	const streamBytes = 4 << 20
	for _, tc := range []struct {
		name string
		// lagging leaves dest2 with volumes but no manifest, dest1 with the complete set.
		lagging func(t *testing.T, env *e2eEnv, dest2 string)
	}{
		{"truncated volume", func(t *testing.T, env *e2eEnv, dest2 string) {
			env.interruptedSend(t, streamBytes, dest2, "--volsize", "1", "--compressor", "")
			env.sendOK(t, "--volsize", "1", "--compressor", "", "tank/data@a", "file://"+env.dest)
			vol := volumeNamed(t, dest2, 2)
			if err := os.Truncate(vol, 1000); err != nil {
				t.Fatal(err)
			}
		}},
		{"same size other bytes", func(t *testing.T, env *e2eEnv, dest2 string) {
			env.interruptedSend(t, streamBytes, dest2, "--volsize", "1", "--compressor", "")
			env.sendOK(t, "--volsize", "1", "--compressor", "", "tank/data@a", "file://"+env.dest)
			vol := volumeNamed(t, dest2, 2)
			data, err := ioutil.ReadFile(vol)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)/2] ^= 0xff
			if err = ioutil.WriteFile(vol, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		// An earlier attempt with another --volsize left same-named volumes.
		{"other volsize", func(t *testing.T, env *e2eEnv, dest2 string) {
			env.interruptedSend(t, streamBytes, dest2, "--volsize", "1", "--compressor", "")
			env.sendOK(t, "--volsize", "2", "--compressor", "", "tank/data@a", "file://"+env.dest)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			dest2 := newDest(t)
			tc.lagging(t, env, dest2)
			if names := manifestNames(destObjects(t, dest2)); len(names) != 0 {
				t.Fatalf("setup left manifests %q at dest2", names)
			}
			logs, err := guarded(t, func() (string, error) {
				return env.send("--volsize", "2", "--compressor", "", "--resume", "tank/data@a", "file://"+env.dest+",file://"+dest2)
			})
			if err == nil {
				t.Errorf("resume completed the set over volumes it does not describe:\n%s", logs)
			}
			if names := manifestNames(destObjects(t, dest2)); len(names) != 0 {
				t.Errorf("dest2 got manifests %q", names)
			}
		})
	}
}

// Object names do not tell -R -p or the compressor "" from "zfs" apart: completing a set must
// compare the options as a resume does.
func TestE2EResumeCompleteRefusesOtherVariant(t *testing.T) {
	env := newE2EEnv(t)
	dest2 := newDest(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	dests := "file://" + env.dest + ",file://" + dest2
	env.sendOK(t, "--volsize", "1", "-R", "-p", "--compressor", "zfs", "tank/data@a", dests)
	for _, name := range manifestNames(destObjects(t, dest2)) {
		if err := os.Remove(filepath.Join(dest2, name)); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := guarded(t, func() (string, error) {
		return env.send("--volsize", "1", "--compressor", "", "--resume", "tank/data@a", dests)
	})
	if err == nil || !strings.Contains(err.Error(), "option mismatch") {
		t.Errorf("completing a -R -p set without -R -p: got %v, want an option mismatch:\n%s", err, logs)
	}
	if names := manifestNames(destObjects(t, dest2)); len(names) != 0 {
		t.Errorf("dest2 got manifests %q", names)
	}
}

// After a completion, the lagging destination's cache holds the manifest it now has, not what an
// earlier attempt left there under the same name.
func TestE2ECompletedSetRefreshesLaggingCache(t *testing.T) {
	env := newE2EEnv(t)
	dest2 := newDest(t)
	env.interruptedSend(t, 3<<20, dest2, "--volsize", "1", "--compressor", "")
	env.sendOK(t, "--volsize", "1", "--compressor", "", "tank/data@a", "file://"+env.dest)
	env.sendOK(t, "--volsize", "1", "--compressor", "", "--resume", "tank/data@a", "file://"+env.dest+",file://"+dest2)

	names := manifestNames(destObjects(t, dest2))
	if len(names) != 1 {
		t.Fatalf("dest2 has manifests %q, want one", names)
	}
	published := destObjects(t, dest2)[names[0]]
	paths, err := filepath.Glob(filepath.Join(env.work, "cache", "*", "*"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("want a cached manifest per destination, got %q (%v)", paths, err)
	}
	for _, path := range paths {
		data, err := ioutil.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, published) {
			t.Errorf("cached %s is not the published manifest", path)
		}
	}
	env.checkRestores(t, "file://"+dest2, "a", 3<<20)
}

// sendOK runs a send that must succeed.
func (env *e2eEnv) sendOK(t *testing.T, args ...string) {
	t.Helper()
	if logs, err := guarded(t, func() (string, error) { return env.send(args...) }); err != nil {
		t.Fatalf("send %q: %v\n%s", args, err, logs)
	}
}

// volumeNamed returns the path of volume n under the file:// destination dir.
func volumeNamed(t *testing.T, dir string, n int64) string {
	t.Helper()
	for name := range destObjects(t, dir) {
		if _, _, _, num, ok := files.ParseBackupVolumeObjectName(name, "|"); ok && num == n {
			return filepath.Join(dir, name)
		}
	}
	t.Fatalf("no volume %d under %s", n, dir)
	return ""
}

// A cached manifest truncated by a kill or a failed download (by an older version, which wrote
// the cache in place) must not wedge every later send to that destination.
func TestE2ESmartSendHealsTruncatedCache(t *testing.T) {
	for _, keep := range []string{"empty", "half"} {
		t.Run(keep, func(t *testing.T) {
			env := newE2EEnv(t)
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
				{Name: "b", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
				{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			})
			dest := "file://" + env.dest
			env.sendOK(t, "tank/data@a", dest)
			paths, err := filepath.Glob(filepath.Join(env.work, "cache", "*", "*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("want one cached manifest, got %q (%v)", paths, err)
			}
			info, err := os.Stat(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			size := int64(0)
			if keep == "half" {
				size = info.Size() / 2
			}
			if err = os.Truncate(paths[0], size); err != nil {
				t.Fatal(err)
			}
			env.sendOK(t, "--increment", "tank/data", dest)
			if got := newestBackup(t, "tank/data", dest); got.BaseSnapshot.Name != "b" || got.IncrementalSnapshot.Name != "a" {
				t.Errorf("newest backup is %s from %q, want b from a", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name)
			}
		})
	}
}

// A truncated partial manifest is no resume state: the resume starts over.
func TestE2EResumeIgnoresTruncatedPartial(t *testing.T) {
	const streamBytes = 3 << 20
	env := newE2EEnv(t)
	args := []string{"--volsize", "1", "--compressor", ""}
	env.interruptedSend(t, streamBytes, env.dest, args...)
	paths, err := filepath.Glob(filepath.Join(env.work, "cache", "*", "*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("want one cached manifest, got %q (%v)", paths, err)
	}
	if err = os.Truncate(paths[0], 100); err != nil {
		t.Fatal(err)
	}
	dest := "file://" + env.dest
	env.sendOK(t, append(args, "--resume", "tank/data@a", dest)...)
	env.checkRestores(t, dest, "a", streamBytes)
}
