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

package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// A manifest download stops past files.MaxManifestBytes and leaves nothing behind: whoever can
// write the destination must not be able to fill the cache's disk.
func TestDownloadToStopsOversizeManifest(t *testing.T) {
	setManifestLimit(t, 1<<20)
	dir := t.TempDir()
	b := &cacheBackend{objects: map[string][]byte{"manifests|x": make([]byte, files.MaxManifestBytes+1)}}
	if err := downloadTo(context.Background(), b, "manifests|x", filepath.Join(dir, "cached")); err == nil {
		t.Fatal("an object larger than any manifest was downloaded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the download left %v behind", entries)
	}
	b.objects["manifests|x"] = make([]byte, files.MaxManifestBytes)
	if err := downloadTo(context.Background(), b, "manifests|x", filepath.Join(dir, "cached")); err != nil {
		t.Errorf("an object of exactly files.MaxManifestBytes: %v", err)
	}
}

// A 300 MiB object under the manifest prefix is not downloaded whole into the cache, and clean
// fails naming it.
func TestCleanDoesNotCacheHugeManifest(t *testing.T) {
	setManifestLimit(t, 64<<20)
	targetDir, jobInfo := setupCleanTest(t)
	set := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "a"},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor}
	set.Volumes = []*files.VolumeInfo{{ObjectName: set.BackupVolumeObjectName(1), VolumeNumber: 1}}
	writeTestManifest(t, targetDir, set)
	f, err := os.Create(filepath.Join(targetDir, "manifests|evil.manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(300 << 20); err != nil { // sparse at the source
		t.Fatal(err)
	}
	f.Close()
	cerr := Clean(context.Background(), jobInfo, false, true)
	if cerr == nil || !strings.Contains(cerr.Error(), "manifests|evil.manifest") || !strings.Contains(cerr.Error(), "delete") {
		t.Errorf("want clean to fail naming the object and how to remove it, got %v", cerr)
	}
	var total int64
	_ = filepath.Walk(cacheDirFor(jobInfo.Destinations[0]), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if total > 1<<20 {
		t.Errorf("the cache holds %d bytes after one clean", total)
	}
}

// A manifest that does not decode is not kept in the cache, and the error names the object and
// says how to get rid of it.
func TestReadCachedManifestKeepsNoJunk(t *testing.T) {
	cache := t.TempDir()
	name := "manifests|tank/data|a.manifest.gz"
	b := &cacheBackend{objects: map[string][]byte{name: bytes.Repeat([]byte("junk"), 1024)}}
	j := &files.JobInfo{ManifestPrefix: "manifests", Separator: "|"}
	_, err := readCachedManifest(context.Background(), j, cache, b, name)
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "delete") {
		t.Errorf("want an error naming %s and how to remove it, got %v", name, err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("the junk is still cached: %v", entries)
	}
}

// syncCache removes temporary files that a killed write left in the cache, but not one that may
// be a write in progress.
func TestSyncCacheRemovesStaleTemps(t *testing.T) {
	cache := t.TempDir()
	stale, fresh := filepath.Join(cache, files.AtomicTempPrefix+"1"), filepath.Join(cache, files.AtomicTempPrefix+"2")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	b := &cacheBackend{objects: map[string][]byte{}}
	if _, _, err := syncCache(context.Background(), &files.JobInfo{ManifestPrefix: "manifests"}, cache, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temporary file is still there (%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh temporary file is gone: %v", err)
	}
}

func TestManifestMayBeFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"manifests|tank/data|a.manifest.gz", true},
		{"manifests|tank/data|a|to|b.manifest.gz.pgp", true},
		{"manifests#tank/data#a.manifest.gz", true}, // written with another --separator
		{"manifests|tank/other|zz.manifest.gz.pgp", false},
		{"manifests|tank/data2|a.manifest.gz", false},
		{"manifests|tank/data/child|a.manifest.gz", false},
		{"manifests|tank|data|a.manifest.gz", false},
		{"manifests|evil.manifest", false},
	} {
		if got := manifestMayBeFor(tc.name, "manifests", "tank/data"); got != tc.want {
			t.Errorf("manifestMayBeFor(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// After a chain restart retired the full of a, an incremental a->b applies to a dataset that
// has @a; the full of b does not (zfs refuses a full into a dataset with snapshots).
func TestBackupThatAppliesIncrementalFromLocalSource(t *testing.T) {
	a := files.SnapshotInfo{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	b := files.SnapshotInfo{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	incr := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: b, IncrementalSnapshot: a}
	full := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: b}
	if got := backupThatApplies([]*files.JobInfo{incr, full}, []files.SnapshotInfo{a}); got != incr {
		t.Errorf("got %v, want the incremental a->b", got)
	}
	if got := backupThatApplies([]*files.JobInfo{incr, full}, nil); got != full {
		t.Errorf("into an empty dataset: got %v, want the full of b", got)
	}
}

// A manifest that is its own ancestor does not make backupThatApplies loop forever.
func TestBackupThatAppliesStopsOnLoop(t *testing.T) {
	a := files.SnapshotInfo{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	b := files.SnapshotInfo{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	self := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: a, IncrementalSnapshot: a}
	self.ParentSnap = self
	x := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: a, IncrementalSnapshot: b}
	y := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: b, IncrementalSnapshot: a, ParentSnap: x}
	x.ParentSnap = y
	done := make(chan *files.JobInfo, 1)
	go func() {
		backupThatApplies([]*files.JobInfo{self}, nil)
		done <- backupThatApplies([]*files.JobInfo{y}, nil)
	}()
	select {
	case got := <-done:
		if got != y {
			t.Errorf("got %v, want the only backup", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("backupThatApplies loops on a manifest that is its own ancestor")
	}
}
