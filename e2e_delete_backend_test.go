package main

// The delete:// backend is the last link of the send chain: it removes a volume's temp file once
// every real destination holds the volume. cmd's postRunCleanup removes the whole run temp dir
// after every run, so a look at the temp dir after the send proves nothing about that link.
// This test looks before the final manifest, while the run's temp dir is still there: every
// volume has passed the chain by then, so the dir must hold no volume file.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

func TestE2ESendDeletesVolumeTempFiles(t *testing.T) {
	const (
		streamBytes = 4 << 20
		fileBuffers = 2
	)
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	dest := "file://" + env.dest

	var left []string
	ran := false
	backup.TestHookBeforeFinalManifest = func() {
		ran = true
		entries, err := os.ReadDir(config.BackupTempdir)
		if err != nil {
			t.Errorf("read the run temp dir: %v", err)
			return
		}
		for _, e := range entries {
			left = append(left, e.Name())
		}
	}
	t.Cleanup(func() { backup.TestHookBeforeFinalManifest = nil })
	env.sendOK(t, "--volsize", "1", "--compressor", "", "--maxFileBuffer", fmt.Sprint(fileBuffers), "tank/data@a", dest)
	backup.TestHookBeforeFinalManifest = nil

	if !ran {
		t.Fatal("the hook did not run")
	}
	if len(left) != 0 {
		t.Errorf("temp files still there before the final manifest: %v, want none", left)
	}
	// More volumes than file buffers: the send could only go on because the chain freed slots,
	// and every freed slot's file must be gone.
	if n := len(newestBackup(t, "tank/data", dest).Volumes); n <= fileBuffers {
		t.Errorf("the send made %d volumes, want more than %d file buffers", n, fileBuffers)
	}
	// delete:// is no real destination: the manifest cache holds one dir, for the file destination.
	entries, err := os.ReadDir(filepath.Join(env.work, "cache"))
	if err != nil || len(entries) != 1 {
		t.Errorf("the manifest cache holds %d entries (%v), want one", len(entries), err)
	}
}
