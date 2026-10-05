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
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// writeTestManifest stores a manifest for j at the file:// destination dir, as send would.
func writeTestManifest(t *testing.T, dir string, j *files.JobInfo) {
	t.Helper()
	manifest, err := files.CreateManifestVolume(context.Background(), j)
	if err != nil {
		t.Fatalf("could not create manifest - %v", err)
	}
	defer manifest.DeleteVolume()
	if err = json.NewEncoder(manifest).Encode(j); err != nil {
		t.Fatalf("could not encode manifest - %v", err)
	}
	if err = manifest.Close(); err != nil {
		t.Fatalf("could not close manifest - %v", err)
	}
	dest := filepath.Join(dir, manifest.ObjectName)
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	if err = manifest.CopyTo(dest); err != nil {
		t.Fatalf("could not copy manifest - %v", err)
	}
}

// writeTestObject creates a destination object with the given name.
func writeTestObject(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatalf("could not write object %s - %v", name, err)
	}
	return path
}

// setupCleanTest points config.WorkingDir at a temp cache and returns a file:// destination
// directory and a jobInfo for cleaning it.
func setupCleanTest(t *testing.T) (string, *files.JobInfo) {
	t.Helper()
	oldWorkingDir := config.WorkingDir
	config.WorkingDir = t.TempDir()
	t.Cleanup(func() { config.WorkingDir = oldWorkingDir })
	if err := os.MkdirAll(filepath.Join(config.WorkingDir, "temp"), 0700); err != nil {
		t.Fatal(err)
	}
	oldTempdir := config.BackupTempdir
	config.BackupTempdir = filepath.Join(config.WorkingDir, "temp")
	t.Cleanup(func() { config.BackupTempdir = oldTempdir })

	targetDir := t.TempDir()
	return targetDir, &files.JobInfo{
		ManifestPrefix:     "manifests",
		Separator:          "|",
		Destinations:       []string{"file://" + targetDir},
		MaxParallelUploads: 1,
		MaxBackoffTime:     5 * time.Second,
		MaxRetryTime:       1 * time.Minute,
	}
}

// TestCleanDryRun verifies that running Clean with dryRun=true does not delete
// any objects from the destination, while a subsequent real run deletes the
// orphaned volume but neither the live one nor objects it does not recognize.
// Clean resolves its backend from the destination URI, so this exercises the
// real FileBackend against a local directory rather than a mock.
func TestCleanDryRun(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)

	set := &files.JobInfo{
		VolumeName:     "tank/data",
		BaseSnapshot:   files.SnapshotInfo{Name: "a"},
		ManifestPrefix: "manifests",
		Separator:      "|",
		Compressor:     files.InternalCompressor,
	}
	live := set.BackupVolumeObjectName(1)
	set.Volumes = []*files.VolumeInfo{{ObjectName: live, VolumeNumber: 1}}
	writeTestManifest(t, targetDir, set)
	livePath := writeTestObject(t, targetDir, live)
	orphanPath := writeTestObject(t, targetDir, set.BackupVolumeObjectName(2))
	foreignPath := writeTestObject(t, targetDir, "data-block-stray")

	// Dry-run: nothing may be deleted.
	if cerr := Clean(context.Background(), jobInfo, false, true); cerr != nil {
		t.Fatalf("dry-run Clean returned error - %v", cerr)
	}
	for _, p := range []string{livePath, orphanPath, foreignPath} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("dry-run should not have deleted %s, stat error - %v", p, serr)
		}
	}

	// Real run: only the orphaned volume goes.
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(orphanPath); !os.IsNotExist(serr) {
		t.Fatalf("expected orphaned volume %s to be deleted, got stat error - %v", orphanPath, serr)
	}
	for _, p := range []string{livePath, foreignPath} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("Clean should not have deleted %s, stat error - %v", p, serr)
		}
	}
}

// TestCleanRefusesWithoutManifests: a destination with objects but no manifest at all is
// almost certainly the wrong URI, so clean must refuse rather than delete everything.
func TestCleanRefusesWithoutManifests(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	set := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "a"}, Separator: "|"}
	orphanPath := writeTestObject(t, targetDir, set.BackupVolumeObjectName(1))

	for _, dryRun := range []bool{true, false} {
		err := Clean(context.Background(), jobInfo, false, dryRun)
		if err == nil || !strings.Contains(err.Error(), "no manifests; refusing to clean") {
			t.Errorf("dryRun=%v: got %v, want the no-manifests refusal", dryRun, err)
		}
	}
	if _, serr := os.Stat(orphanPath); serr != nil {
		t.Fatalf("Clean deleted %s from a destination without manifests - %v", orphanPath, serr)
	}

	// An empty destination is fine.
	emptyJob := *jobInfo
	emptyJob.Destinations = []string{"file://" + t.TempDir()}
	if err := Clean(context.Background(), &emptyJob, false, false); err != nil {
		t.Errorf("Clean of an empty destination - %v", err)
	}
}

// TestCleanDryRunLocalManifests covers the --cleanLocal branch: a dry run must
// leave local-only cached manifests alone, and a real run must delete them.
func TestCleanDryRunLocalManifests(t *testing.T) {
	cacheDir, err := ioutil.TempDir("", "zfsbackup-clean-cache")
	if err != nil {
		t.Fatalf("could not create temp cache dir - %v", err)
	}
	defer os.RemoveAll(cacheDir)

	oldWorkingDir := config.WorkingDir
	config.WorkingDir = cacheDir
	defer func() { config.WorkingDir = oldWorkingDir }()

	targetDir, err := ioutil.TempDir("", "zfsbackup-clean-target")
	if err != nil {
		t.Fatalf("could not create temp target dir - %v", err)
	}
	defer os.RemoveAll(targetDir)

	target := "file://" + targetDir
	jobInfo := &files.JobInfo{
		ManifestPrefix:     "manifests",
		Destinations:       []string{target},
		MaxParallelUploads: 1,
		MaxBackoffTime:     5 * time.Second,
		MaxRetryTime:       1 * time.Minute,
	}

	// A cached manifest with no counterpart in the destination is exactly what
	// --cleanLocal removes.
	localCachePath, cerr := getCacheDir(jobInfo, target)
	if cerr != nil {
		t.Fatalf("could not resolve cache dir - %v", cerr)
	}
	strayManifest := filepath.Join(localCachePath, "0123456789abcdef")
	if werr := ioutil.WriteFile(strayManifest, []byte("stale"), 0600); werr != nil {
		t.Fatalf("could not write stray cached manifest - %v", werr)
	}

	// Dry-run: the cached manifest must survive.
	if err := Clean(context.Background(), jobInfo, true, true); err != nil {
		t.Fatalf("dry-run Clean returned error - %v", err)
	}
	if _, serr := os.Stat(strayManifest); serr != nil {
		t.Fatalf("dry-run should not have deleted %s, stat error - %v", strayManifest, serr)
	}

	// Real run: it must be gone.
	if err := Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatalf("Clean returned error - %v", err)
	}
	if _, serr := os.Stat(strayManifest); !os.IsNotExist(serr) {
		t.Fatalf("expected %s to be deleted, got stat error - %v", strayManifest, serr)
	}
}

// TestCleanSkipsDatasetOfRunningSend: a running send has uploaded volumes that no manifest
// lists yet. While its lock is held, clean must not treat them as orphans.
func TestCleanSkipsDatasetOfRunningSend(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)

	set := &files.JobInfo{
		VolumeName:     "tank/data",
		BaseSnapshot:   files.SnapshotInfo{Name: "a"},
		ManifestPrefix: "manifests",
		Separator:      "|",
		Compressor:     files.InternalCompressor,
	}
	live := set.BackupVolumeObjectName(1)
	set.Volumes = []*files.VolumeInfo{{ObjectName: live, VolumeNumber: 1}}
	writeTestManifest(t, targetDir, set)
	writeTestObject(t, targetDir, live)
	inFlight := writeTestObject(t, targetDir, set.BackupVolumeObjectName(2))

	// Another live process (our parent) holds the send lock.
	_, lockPath, err := volumeLock("tank/data")
	if err != nil {
		t.Fatal(err)
	}
	if err = ioutil.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)

	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); serr != nil {
		t.Fatalf("clean deleted a volume of a dataset whose send lock is held - %v", serr)
	}

	// Once the send is done, the same volume is an orphan again.
	if err = os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); !os.IsNotExist(serr) {
		t.Fatalf("expected the orphan to be deleted once the lock is free, got stat error - %v", serr)
	}
	if _, serr := os.Stat(lockPath); !os.IsNotExist(serr) {
		t.Errorf("clean left its lock file behind (stat: %v)", serr)
	}
}
