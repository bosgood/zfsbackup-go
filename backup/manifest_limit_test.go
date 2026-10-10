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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// setManifestLimit lowers files.MaxManifestBytes for the test and restores it after. The limit is a package
// variable: a test that calls this must not call t.Parallel.
func setManifestLimit(t *testing.T, limit int) {
	t.Helper()
	old := files.MaxManifestBytes
	files.MaxManifestBytes = limit
	t.Cleanup(func() { files.MaxManifestBytes = old })
}

// manifestObjectOf stores a backup set of dataset@snapshot with n volumes at a temp dir and
// returns the set and the bytes of its manifest object.
func manifestObjectOf(t *testing.T, dataset, snapshot string, n int) (*files.JobInfo, []byte) {
	t.Helper()
	// Compressed for real (the default level 0 does not), so the object is well under the
	// JSON it holds, as a send's is.
	set := &files.JobInfo{VolumeName: dataset, BaseSnapshot: files.SnapshotInfo{Name: snapshot},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor, CompressionLevel: 6}
	for i := 1; i <= n; i++ {
		set.Volumes = append(set.Volumes, &files.VolumeInfo{ObjectName: set.BackupVolumeObjectName(int64(i)), VolumeNumber: int64(i)})
	}
	dir := t.TempDir()
	writeTestManifest(t, dir, set)
	data, err := os.ReadFile(filepath.Join(dir, set.ManifestObjectName()))
	if err != nil {
		t.Fatal(err)
	}
	return set, data
}

// A manifest longer than the limit is a backup this version cannot read, not a forgery: the
// error must not tell the user to delete it, the cached copy stays (the next run would only
// download the same object again), and it is downloaded once.
func TestReadCachedManifestKeepsTooLongManifest(t *testing.T) {
	_, jobInfo := setupCleanTest(t)
	set, data := manifestObjectOf(t, "tank/data", "a", 20)
	name := set.ManifestObjectName()
	setManifestLimit(t, 2048)
	if len(data) > files.MaxManifestBytes {
		t.Fatalf("the manifest object is %d bytes compressed, over the %d-byte limit; the test needs it under", len(data), files.MaxManifestBytes)
	}
	b := &cacheBackend{objects: map[string][]byte{name: data}}
	cache := t.TempDir()

	for round := 1; round <= 2; round++ {
		_, err := readCachedManifest(context.Background(), jobInfo, cache, b, name)
		if !errors.Is(err, files.ErrManifestTooLong) {
			t.Fatalf("round %d: got %v, want ErrManifestTooLong", round, err)
		}
		if strings.Contains(err.Error(), "delete that object") || !strings.Contains(err.Error(), "do not delete") {
			t.Errorf("round %d: the error tells the user to delete a backup: %v", round, err)
		}
		if !strings.Contains(err.Error(), "--volsize") {
			t.Errorf("round %d: the error does not say how to avoid the limit: %v", round, err)
		}
		// The cached copy wins over the object until it is deleted, so the advice must name it
		// (the error ReadManifest wraps names it too, so look for the advice).
		if !strings.Contains(err.Error(), "delete its copy in the cache, "+filepath.Join(cache, cachedManifestName(name))) {
			t.Errorf("round %d: the error does not name the cached copy: %v", round, err)
		}
		if _, serr := os.Stat(filepath.Join(cache, cachedManifestName(name))); serr != nil {
			t.Errorf("round %d: the cached copy is gone: %v", round, serr)
		}
		if b.downloads != 1 {
			t.Errorf("round %d: %d downloads, want 1", round, b.downloads)
		}
	}
}

// A send must not write a manifest its readers reject: saveManifest refuses one over the limit,
// says what to do, and caches nothing.
func TestSaveManifestRefusesOverLimit(t *testing.T) {
	_, jobInfo := setupCleanTest(t)
	set, _ := manifestObjectOf(t, "tank/data", "a", 20)
	set.Destinations = jobInfo.Destinations
	if _, err := getCacheDir(jobInfo, jobInfo.Destinations[0]); err != nil {
		t.Fatal(err)
	}
	setManifestLimit(t, 2048)

	_, err := saveManifest(context.Background(), set, false)
	if err == nil || !strings.Contains(err.Error(), "--volsize") {
		t.Fatalf("got %v, want a refusal that says to use a larger --volsize", err)
	}
	if _, serr := os.Stat(partialManifestCachePath(set, jobInfo.Destinations[0])); !os.IsNotExist(serr) {
		t.Errorf("saveManifest cached the refused manifest, stat error - %v", serr)
	}
	files.MaxManifestBytes = 1 << 20
	manifest, err := saveManifest(context.Background(), set, false)
	if err != nil {
		t.Fatalf("under the limit: %v", err)
	}
	_ = manifest.DeleteVolume()
}

// The estimate checkManifestFits refuses a send by is counted high, but not by much: it is at
// least the JSON a send writes for the same volumes, and within a few percent of it.
func TestManifestCostCountsHigh(t *testing.T) {
	set := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "a", CreationTime: time.Now()},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor, VolumeSize: 200, StartTime: time.Now()}
	const volumes, volumeBytes = 2000, 200<<20 - 50<<10
	base, perVolume, err := manifestCost(set, volumes, volumeBytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= volumes; i++ {
		sum := sha256.Sum256([]byte{byte(i)})
		set.Volumes = append(set.Volumes, &files.VolumeInfo{
			ObjectName: set.BackupVolumeObjectName(i), VolumeNumber: i,
			SHA256Sum: hex.EncodeToString(sum[:]), MD5Sum: hex.EncodeToString(sum[:16]), CRC32CSum32: uint32(i) << 20,
			Size: volumeBytes, ZFSStreamBytes: volumeBytes, StreamSHA256: hex.EncodeToString(sum[:]),
			CreateTime: time.Now(), CloseTime: time.Now(),
		})
	}
	var buf bytes.Buffer
	if err = json.NewEncoder(&buf).Encode(set); err != nil {
		t.Fatal(err)
	}
	estimate, actual := base+perVolume*volumes, uint64(buf.Len())
	if estimate < actual || estimate > actual+actual/20 {
		t.Errorf("estimated %d bytes of manifest for %d volumes, a send writes %d", estimate, volumes, actual)
	}
}

// A manifest over the limit that is no longer at the destination is cached, local-only, and
// still may be a real backup's: clean stops, deletes nothing, and says not to delete it, as the
// error said while the object was at the destination. --cleanLocal changes none of that.
func TestCleanKeepsLocalOnlyTooLongManifest(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	set, data := manifestObjectOf(t, "tank/data", "a", 20)
	manifestPath := filepath.Join(targetDir, set.ManifestObjectName())
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	vol := writeTestObject(t, targetDir, set.Volumes[0].ObjectName)
	setManifestLimit(t, 2048)
	if err := Clean(context.Background(), jobInfo, false, false); !errors.Is(err, files.ErrManifestTooLong) {
		t.Fatalf("clean with the manifest at the destination: got %v, want ErrManifestTooLong", err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(config.WorkingDir, "cache")
	for _, cleanLocal := range []bool{false, true} {
		err := Clean(context.Background(), jobInfo, cleanLocal, false)
		if !errors.Is(err, files.ErrManifestTooLong) {
			t.Fatalf("cleanLocal=%v: got %v, want ErrManifestTooLong", cleanLocal, err)
		}
		if !strings.Contains(err.Error(), "do not delete") || !strings.Contains(err.Error(), "not at the destination") {
			t.Errorf("cleanLocal=%v: the error does not say to keep a backup's manifest: %v", cleanLocal, err)
		}
		if !exists(t, vol) {
			t.Fatalf("cleanLocal=%v deleted the volume %s", cleanLocal, vol)
		}
		if objects := filesUnder(t, cached); len(objects) != 1 {
			t.Errorf("cleanLocal=%v: the cache holds %d files, want the one manifest", cleanLocal, len(objects))
		}
	}
}

// filesUnder returns the paths of the files below dir.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			paths = append(paths, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
