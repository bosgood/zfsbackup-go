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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/internal/fakezfs"
)

// A volume object replaced by one far larger than its signed manifest records is rejected once
// it is past that size, not downloaded whole first.
func TestE2EReceiveStopsOversizeVolumeEarly(t *testing.T) {
	env := newE2EEnv(t)
	rings := writeRings(t, newKey(t))
	vols := env.sentSet(t, rings...)
	if err := ioutil.WriteFile(filepath.Join(env.dest, vols[0]), make([]byte, 64<<20), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
	logs, err := guarded(t, func() (string, error) {
		return env.receive(append(append([]string{}, rings...), "tank/data@a", "file://"+env.dest, "restored/data")...)
	})
	if err == nil {
		t.Fatalf("receive of a set with an oversize volume succeeded\n%s", logs)
	}
	if strings.Contains(logs, fmt.Sprintf("got %d bytes", 64<<20)) || !strings.Contains(logs, "larger than the") {
		t.Errorf("want the volume rejected as larger than its manifest records, before all of it is read\n%s", logs)
	}
}

// writeOtherDatasetJunk writes size bytes of junk under a manifest name of another dataset, tank/other.
func writeOtherDatasetJunk(t *testing.T, dest string, size int) {
	t.Helper()
	junk := make([]byte, size)
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(dest, "manifests|tank/other|zz.manifest.gz.pgp"), junk, 0600); err != nil {
		t.Fatal(err)
	}
}

// receive --auto reads only the manifests of the dataset it restores: junk under another
// dataset's manifest name, whatever its size, does not stop it.
func TestE2EReceiveAutoIgnoresOtherDatasetsJunk(t *testing.T) {
	for _, size := range []int{4 << 10, 48 << 20, 65 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			env := newE2EEnv(t)
			rings := writeRings(t, newKey(t))
			env.sentSet(t, rings...)
			writeOtherDatasetJunk(t, env.dest, size)
			t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
			logs, err := guarded(t, func() (string, error) {
				return env.receive(append(append([]string{"--auto"}, rings...), "tank/data@a", "file://"+env.dest, "restored/data")...)
			})
			if err != nil {
				t.Fatalf("receive --auto: %v\n%s", err, logs)
			}
			var cached int64
			_ = filepath.Walk(filepath.Join(env.work, "cache"), func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					cached += info.Size()
				}
				return nil
			})
			if cached > files.MaxManifestBytes {
				t.Errorf("the cache holds %d bytes, more than one manifest may be", cached)
			}
		})
	}
}

// A smart send reads only the manifests of the dataset it sends.
func TestE2ESmartSendIgnoresOtherDatasetsJunk(t *testing.T) {
	env := newE2EEnv(t)
	rings := writeRings(t, newKey(t))
	env.sentSet(t, rings...)
	writeOtherDatasetJunk(t, env.dest, 4<<10)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "b", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	env.sendOK(t, append(append([]string{"--compressor", "", "--fullIfOlderThan", "720h"}, rings...), "tank/data", "file://"+env.dest)...)
	if _, ok := destObjects(t, env.dest)["manifests|tank/data|a|to|b.manifest.gz.pgp"]; !ok {
		t.Errorf("the smart send did not send the incremental a->b")
	}
}

// reforgeManifest decodes the unencrypted manifest name at dest, applies edit, and stores it again
// under the name it then has.
func reforgeManifest(t *testing.T, env *e2eEnv, name string, edit func(*files.JobInfo)) {
	t.Helper()
	path := filepath.Join(env.dest, name)
	vol, err := files.ExtractLocal(context.Background(), &files.JobInfo{}, path, true)
	if err != nil {
		t.Fatal(err)
	}
	m := new(files.JobInfo)
	err = json.NewDecoder(vol).Decode(m)
	vol.Close()
	if err != nil {
		t.Fatal(err)
	}
	edit(m)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err = json.NewEncoder(gz).Encode(m); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = ioutil.WriteFile(filepath.Join(env.dest, m.StoredManifestObjectName("manifests")), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(env.work, "cache")); err != nil {
		t.Fatal(err)
	}
}

// A manifest whose Volumes holds null is an error, not a panic.
func TestE2EReceiveRejectsNullVolume(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	env.sendOK(t, "--compressor", "", "tank/data@a", "file://"+env.dest)
	reforgeManifest(t, env, "manifests|tank/data|a.manifest.gz", func(m *files.JobInfo) { m.Volumes = append(m.Volumes, nil) })
	t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
	var panicked interface{}
	var err error
	func() {
		defer func() { panicked = recover() }()
		// Not guarded: its goroutine would put the panic out of recover's reach.
		_, err = env.receive("tank/data@a", "file://"+env.dest, "restored/data")
	}()
	if panicked != nil || err == nil {
		t.Errorf("receive of a manifest with a null volume: panic %v, error %v; want an error", panicked, err)
	}
}

// A manifest that is its own parent (an incremental from the snapshot it is of) makes receive
// --auto fail, not loop forever.
func TestE2EReceiveAutoFailsOnManifestLoop(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	env.sendOK(t, "--compressor", "", "tank/data@a", "file://"+env.dest)
	reforgeManifest(t, env, "manifests|tank/data|a.manifest.gz", func(m *files.JobInfo) { m.IncrementalSnapshot = m.BaseSnapshot })
	t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--auto", "tank/data@a", "file://"+env.dest, "restored/data")
	})
	if err == nil || !strings.Contains(logs, "loop") {
		t.Errorf("want receive --auto to fail on the loop, got %v\n%s", err, logs)
	}
}

// After a chain restart retired the full of a, the restored copy has @a and the destination has
// a->b and the full of b. --auto must apply a->b: zfs refuses a full into a dataset with snapshots.
func TestE2EAutoIncrementalOntoExistingParentAfterFullRetired(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	a := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	b := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()
	fixture := fmt.Sprintf("tank/data@b\t%d\tsnapshot\ntank/data@a\t%d\tsnapshot\n", b, a)
	if err := ioutil.WriteFile(env.snapshots, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"tank/data@a"}, {"-i", "a", "tank/data@b"}, {"tank/data@b"}} {
		env.sendOK(t, append(append([]string{"--compressor", ""}, args...), "file://"+env.dest)...)
	}
	for name := range destObjects(t, env.dest) {
		if name == "manifests|tank/data|a.manifest.gz" || strings.HasPrefix(name, "tank/data|a.zstream") {
			if err := os.Remove(filepath.Join(env.dest, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(env.work, "cache")); err != nil {
		t.Fatal(err)
	}
	fixture += fmt.Sprintf("restored/data@a\t%d\tsnapshot\n", a)
	if err := ioutil.WriteFile(env.snapshots, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--auto", "tank/data@b", "file://"+env.dest, "restored/data")
	})
	if err != nil {
		t.Fatalf("receive --auto: %v\n%s", err, logs)
	}
	got, _ := ioutil.ReadFile(receiveLog)
	h := sha256.New()
	if _, err = io.Copy(h, fakezfs.Stream("a", "tank/data@b", 4096)); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("4096 %x\n", h.Sum(nil)); string(got) != want {
		t.Errorf("zfs receive got %q, want only the incremental a->b (%q)\n%s", got, want, logs)
	}
}

// A negative --maxFileBuffer is an input error, not a panic.
func TestE2EReceiveRejectsNegativeMaxFileBuffer(t *testing.T) {
	env := newE2EEnv(t)
	rings := writeRings(t, newKey(t))
	env.sentSet(t, rings...)
	t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
	var panicked interface{}
	var err error
	func() {
		defer func() { panicked = recover() }()
		_, err = env.receive(append(append([]string{"--maxFileBuffer", "-1"}, rings...), "tank/data@a", "file://"+env.dest, "restored/data")...)
	}()
	if panicked != nil || err == nil {
		t.Errorf("receive --maxFileBuffer -1: panic %v, error %v; want an error", panicked, err)
	}
}
