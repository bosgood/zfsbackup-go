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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// With --encryptTo and --signFrom, a manifest encrypted to the public key but not signed must be
// rejected. Anyone who can write to the destination has the public key; a forged manifest
// claiming a newer full would otherwise make every smart send back up nothing, exit 0, forever.
func TestE2ESignFromRejectsForgedManifest(t *testing.T) {
	env := newE2EEnv(t)
	key := newKey(t)
	rings := writeRings(t, key)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	dest := "file://" + env.dest
	smart := append(append([]string{}, rings...), "--fullIfOlderThan", "720h", "tank/data", dest)
	env.sendOK(t, smart...)

	// The forger has only the public key: the manifest is encrypted, not signed.
	forged := &files.JobInfo{
		VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "z", CreationTime: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)},
		Compressor: files.InternalCompressor, CompressionLevel: 6, Separator: "|", ManifestPrefix: "manifests",
		EncryptKey: key, StartTime: time.Now(), EndTime: time.Now(),
	}
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	t.Cleanup(func() { config.BackupTempdir = oldTemp })
	vol, err := files.CreateManifestVolume(context.Background(), forged)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(vol).Encode(forged); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(env.dest, vol.ObjectName)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err = vol.CopyTo(target); err != nil {
		t.Fatal(err)
	}

	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "b", CreationTime: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	logs, err := guarded(t, func() (string, error) { return env.send(smart...) })
	if err == nil {
		t.Fatalf("send accepted the forged manifest and exited 0:\n%s", logs)
	}
	if !strings.Contains(err.Error()+logs, "not signed") {
		t.Errorf("send failed, but not because the manifest is unsigned: %v\n%s", err, logs)
	}
	if strings.Contains(logs, "Nothing new to back up") {
		t.Errorf("send printed 'Nothing new to back up' with a forged manifest at the destination:\n%s", logs)
	}
}
