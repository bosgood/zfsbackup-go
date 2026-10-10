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
	"context"
	"os"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

// cleanTestSet stores a one-volume backup set of dataset@snapshot under manifestPrefix at the
// file:// destination dir and returns it and the path of its volume.
func cleanTestSet(t *testing.T, dir, dataset, snapshot, manifestPrefix string) (*files.JobInfo, string) {
	t.Helper()
	set := &files.JobInfo{
		VolumeName:     dataset,
		BaseSnapshot:   files.SnapshotInfo{Name: snapshot},
		ManifestPrefix: manifestPrefix,
		Separator:      "|",
		Compressor:     files.InternalCompressor,
	}
	vol := set.BackupVolumeObjectName(1)
	set.Volumes = []*files.VolumeInfo{{ObjectName: vol, VolumeNumber: 1}}
	writeTestManifest(t, dir, set)
	return set, writeTestObject(t, dir, vol)
}

// cachedFiles returns the names of the files in the manifest cache of jobInfo's destination.
func cachedFiles(t *testing.T, jobInfo *files.JobInfo) []string {
	t.Helper()
	cache, err := getCacheDir(jobInfo, jobInfo.Destinations[0])
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestCleanWrongPrefixRefuses: with a wrong --manifestPrefix (or after the destination's
// manifests were deleted) the destination lists no manifests, and every cached manifest is
// local-only. clean must refuse, naming the prefix, and must not point at --cleanLocal: that
// would delete every volume.
func TestCleanWrongPrefixRefuses(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	_, livePath := cleanTestSet(t, targetDir, "tank/data", "a", "bk")

	jobInfo.ManifestPrefix = "bk"
	if err := Clean(context.Background(), jobInfo, false, false); err != nil {
		t.Fatal(err) // caches the manifest
	}
	cached := cachedFiles(t, jobInfo)
	if len(cached) != 1 {
		t.Fatalf("want 1 cached manifest after a clean with the right prefix, got %q", cached)
	}

	jobInfo.ManifestPrefix = "manifests" // the default, --manifestPrefix forgotten
	for _, cleanLocal := range []bool{false, true} {
		for _, dryRun := range []bool{true, false} {
			buf := captureLogs(t)
			err := Clean(context.Background(), jobInfo, cleanLocal, dryRun)
			if err == nil || !strings.Contains(err.Error(), "refusing to clean") || !strings.Contains(err.Error(), `"manifests|"`) {
				t.Errorf("cleanLocal=%v dryRun=%v: got %v, want a refusal naming the prefix", cleanLocal, dryRun, err)
			}
			if strings.Contains(buf.String(), "use --cleanLocal") {
				t.Errorf("cleanLocal=%v dryRun=%v: suggests --cleanLocal for a destination without manifests:\n%s",
					cleanLocal, dryRun, buf.String())
			}
		}
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("clean with a forgotten --manifestPrefix deleted the live volume: %v", err)
	}
	if got := cachedFiles(t, jobInfo); len(got) != 1 || got[0] != cached[0] {
		t.Errorf("clean with a forgotten --manifestPrefix changed the cache: %q, want %q", got, cached)
	}
}

// TestCleanLocalSparesOtherPrefixManifests: two jobs share a destination, one with the default
// prefix and one with --manifestPrefix bk. A clean --cleanLocal of the first sees the cached
// manifests of the other as local-only; they were cached under another prefix, so they are not
// this job's state to delete, and neither are their volumes.
func TestCleanLocalSparesOtherPrefixManifests(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	cleanTestSet(t, targetDir, "tank/a", "a", "manifests")
	_, otherPath := cleanTestSet(t, targetDir, "tank/data", "a", "bk")

	jobInfo.ManifestPrefix = "bk"
	if err := Clean(context.Background(), jobInfo, false, false); err != nil {
		t.Fatal(err) // caches the bk manifest
	}
	jobInfo.ManifestPrefix = "manifests"
	if err := Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("clean --cleanLocal deleted a volume of the other prefix's set: %v", err)
	}
	if got := cachedFiles(t, jobInfo); len(got) != 2 {
		t.Errorf("clean --cleanLocal removed a cached manifest of the other prefix: cache holds %q", got)
	}

	// The default job's own interrupted send is still reclaimed.
	partial := &files.JobInfo{VolumeName: "tank/a", BaseSnapshot: files.SnapshotInfo{Name: "b"},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor}
	partial.Volumes = []*files.VolumeInfo{{ObjectName: partial.BackupVolumeObjectName(1), VolumeNumber: 1}}
	partialVol := writeTestObject(t, targetDir, partial.BackupVolumeObjectName(1))
	state := cachePartialManifest(t, jobInfo, partial)
	if err := Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("clean --cleanLocal left this prefix's local-only manifest, stat error - %v", err)
	}
	if _, err := os.Stat(partialVol); !os.IsNotExist(err) {
		t.Errorf("clean --cleanLocal left this prefix's local-only volume, stat error - %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("clean --cleanLocal deleted a volume of the other prefix's set: %v", err)
	}
}

// TestCleanForceSparesLocalOnlyManifests: --force deletes broken sets at the destination. A
// local-only manifest is the state of an interrupted send (--resume continues from it) and lists
// only some of its volumes; without --cleanLocal, --force must leave it and its volumes alone.
func TestCleanForceSparesLocalOnlyManifests(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	cleanTestSet(t, targetDir, "tank/data", "a", "manifests")

	partial := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "b"},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor}
	partial.Volumes = []*files.VolumeInfo{
		{ObjectName: partial.BackupVolumeObjectName(1), VolumeNumber: 1},
		{ObjectName: partial.BackupVolumeObjectName(2), VolumeNumber: 2}, // never uploaded
	}
	uploaded := writeTestObject(t, targetDir, partial.BackupVolumeObjectName(1))
	state := cachePartialManifest(t, jobInfo, partial)

	jobInfo.Force = true
	for _, dryRun := range []bool{true, false} {
		buf := captureLogs(t)
		if err := Clean(context.Background(), jobInfo, false, dryRun); err != nil {
			t.Fatalf("dryRun=%v: %v", dryRun, err)
		}
		if strings.Contains(buf.String(), "Would delete") && !strings.Contains(buf.String(), "would delete 0 objects") {
			t.Errorf("dryRun=%v: --force plans to delete the local-only set:\n%s", dryRun, buf.String())
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("clean --force without --cleanLocal deleted a local-only manifest: %v", err)
	}
	if _, err := os.Stat(uploaded); err != nil {
		t.Errorf("clean --force without --cleanLocal deleted a volume of a local-only manifest: %v", err)
	}

	// With --cleanLocal it is reclaimed.
	if err := Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("clean --force --cleanLocal left the local-only manifest, stat error - %v", err)
	}
	if _, err := os.Stat(uploaded); !os.IsNotExist(err) {
		t.Errorf("clean --force --cleanLocal left the local-only volume, stat error - %v", err)
	}
	if n := len(cachedFiles(t, jobInfo)); n != 1 {
		t.Errorf("want only the live set's manifest cached, got %d files", n)
	}
}
