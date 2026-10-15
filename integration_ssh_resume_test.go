//go:build integration

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// TestIntegrationSSHResume is TestIntegrationFileResume over sftp, where
// --resume stats the remote volumes (Sizer.Size over sftp), then a remote
// volume lost, then a remote manifest lost.
func TestIntegrationSSHResume(t *testing.T) {
	pool := testPool(t)
	itSSHClearEnv(t)
	r := testTruncatedResume(t, pool, "sres", newSSHDest)
	manifest := manifestFiles(fileNames(t, r.dest.dir))
	if len(manifest) != 1 {
		t.Fatalf("%s holds manifests %v, want 1", r.dest.dir, manifest)
	}
	complete := fileNames(t, r.dest.dir)

	// Volume 1 and the manifest are lost at the remote: all is sent again.
	vol1 := r.manifest.Volumes[0].ObjectName
	for _, n := range []string{vol1, manifest[0]} {
		if err := os.Remove(filepath.Join(r.dest.dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", r.work, "zfs", append([]string{"--resume"}, r.args...)...)
	if err != nil {
		t.Fatalf("send --resume: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Volume "+vol1+" missing at ", "Resuming from volume 1: 0 of")
	wantFilesAt(t, r.dest.dir, complete)
	wantVolumeSizes(t, r.dest.dir, manifestFor(t, r.work, r.src, r.dest.target, r.snap))
	restoreAll(t, r.src, r.dest.target, pool+"/sresRestored2", r.snap)

	// Only the manifest is lost: a smart send sees no backup there and sends a full.
	if err := os.Remove(filepath.Join(r.dest.dir, manifest[0])); err != nil {
		t.Fatal(err)
	}
	smart := append([]string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_daily", "--incrementalSnapshotSuffix", "_daily",
	}, slices.Clone(plainSend)...)
	mustSend(t, r.work, append(smart, append(slices.Clone(sshRetry), r.src, r.dest.target)...)...)
	if got := newestBackup(t, r.src, r.dest.target); got.BaseSnapshot.Name != r.snap || got.IncrementalSnapshot.Name != "" {
		t.Fatalf("smart send after the manifest was lost: sent %s from %q, want %s full", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, r.snap)
	}
	wantFilesAt(t, r.dest.dir, complete)
	wantVolumeSizes(t, r.dest.dir, manifestFor(t, r.work, r.src, r.dest.target, r.snap))
	restoreAll(t, r.src, r.dest.target, pool+"/sresRestored3", r.snap)

	t.Run("SIGTERM", func(t *testing.T) { testSIGTERMResume(t, pool, "sterm", newSSHDest) })
}
