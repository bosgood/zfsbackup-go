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
	"crypto/md5" // nolint:gosec // names the old lock file, not for security
	"encoding/json"
	"fmt"
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
	defer func() { _ = manifest.DeleteVolume() }()
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
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
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
	cacheDir, err := os.MkdirTemp("", "zfsbackup-clean-cache")
	if err != nil {
		t.Fatalf("could not create temp cache dir - %v", err)
	}
	defer os.RemoveAll(cacheDir)

	oldWorkingDir := config.WorkingDir
	config.WorkingDir = cacheDir
	defer func() { config.WorkingDir = oldWorkingDir }()

	targetDir, err := os.MkdirTemp("", "zfsbackup-clean-target")
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
	if werr := os.WriteFile(strayManifest, []byte("stale"), 0600); werr != nil {
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
	if err = os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
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

// TestCleanHonoursSendLockAcrossTMPDIR: a send and a clean that see different TMPDIRs (systemd
// PrivateTmp, cron vs a shell) must still see each other's lock.
func TestCleanHonoursSendLockAcrossTMPDIR(t *testing.T) {
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

	// The send, with its TMPDIR, holds the lock (our parent is a live pid).
	_, lockPath, err := volumeLock("tank/data")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)

	t.Setenv("TMPDIR", t.TempDir()) // clean runs with another
	// Where builds before dc37d16 kept the lock; any local user can plant it, naming a live pid
	// (our parent here: pid 1 is invisible to a non-root signal 0, so it would look dead).
	planted := filepath.Join(os.TempDir(), fmt.Sprintf("zfsbackup.%x.lck", md5.Sum([]byte("tank/data"))))
	if err = os.WriteFile(planted, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); serr != nil {
		t.Fatalf("clean with another TMPDIR deleted a volume of a running send - %v", serr)
	}

	// The send is done; the file in /tmp means nothing, so the orphan goes.
	if err = os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(inFlight); !os.IsNotExist(serr) {
		t.Fatalf("a planted lock file in TMPDIR kept the orphan, stat error - %v", serr)
	}
}

// cachePartialManifest writes j's manifest to the local cache only, as a send
// does after each volume, and returns its path.
func cachePartialManifest(t *testing.T, jobInfo, j *files.JobInfo) string {
	t.Helper()
	cache, err := getCacheDir(jobInfo, jobInfo.Destinations[0])
	if err != nil {
		t.Fatal(err)
	}
	mv, err := files.CreateManifestVolume(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(mv).Encode(j); err != nil {
		t.Fatal(err)
	}
	if err = mv.Close(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cache, cachedManifestName(mv.ObjectName))
	if err = mv.CopyTo(state); err != nil {
		t.Fatal(err)
	}
	if err = mv.DeleteVolume(); err != nil {
		t.Fatal(err)
	}
	return state
}

// A send of tank/data@b is running (lock held) and has cached its in-progress
// manifest, the state --resume needs. clean --cleanLocal must leave it, and the
// volumes it lists, until the lock is free.
func TestCleanLocalSparesRunningSendState(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	live := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "a"},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor}
	live.Volumes = []*files.VolumeInfo{{ObjectName: live.BackupVolumeObjectName(1), VolumeNumber: 1}}
	writeTestManifest(t, targetDir, live)
	writeTestObject(t, targetDir, live.BackupVolumeObjectName(1))

	running := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "b"},
		ManifestPrefix: "manifests", Separator: "|", Compressor: files.InternalCompressor}
	running.Volumes = []*files.VolumeInfo{{ObjectName: running.BackupVolumeObjectName(1), VolumeNumber: 1}}
	inFlight := writeTestObject(t, targetDir, running.BackupVolumeObjectName(1))
	state := cachePartialManifest(t, jobInfo, running)

	// Another live process (our parent) holds the send lock.
	_, lockPath, err := volumeLock("tank/data")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)

	if err = Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatalf("clean --cleanLocal: %v", err)
	}
	if _, serr := os.Stat(state); serr != nil {
		t.Fatalf("clean --cleanLocal deleted the cached manifest of a running send (lock held): %v", serr)
	}
	if _, serr := os.Stat(inFlight); serr != nil {
		t.Fatalf("clean --cleanLocal deleted a volume of a running send (lock held): %v", serr)
	}

	// Once the send is done, the partial is local-only state to reclaim.
	if err = os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err = Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatalf("clean --cleanLocal after the send: %v", err)
	}
	if _, serr := os.Stat(state); !os.IsNotExist(serr) {
		t.Errorf("clean --cleanLocal left the abandoned partial manifest, stat error - %v", serr)
	}
	if _, serr := os.Stat(inFlight); !os.IsNotExist(serr) {
		t.Errorf("clean --cleanLocal left the abandoned partial's volume, stat error - %v", serr)
	}
}
