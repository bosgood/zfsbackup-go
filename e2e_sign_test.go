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
	"fmt"
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

// Rotating the signing key: a new key for the same address signs from now on, and the old public
// key stays in the ring. Every manifest at the destination is signed by the old key, so a smart
// send that trusts only --signFrom's key fails on all of them, every run, and so does a restore of
// a chain that spans the rotation. --trustSigner <old> accepts them.
func TestE2ESignKeyRotationWithTrustSigner(t *testing.T) {
	env := newE2EEnv(t)
	k1, k2 := newKey(t), newKey(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	a := files.SnapshotInfo{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	b := files.SnapshotInfo{Name: "b", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{a})
	dest := "file://" + env.dest
	env.sendOK(t, append(writeRings(t, k1), "--fullIfOlderThan", "720h", "tank/data", dest)...)

	// Rotate: k2 is now backup@example.com's key (first in both rings); k1 stays in both.
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{b, a})
	rotated := writeRings(t, k2, k1)
	smart := append(append([]string{}, rotated...), "--fullIfOlderThan", "720h", "tank/data", dest)
	logs, err := guarded(t, func() (string, error) { return env.send(smart...) })
	if err == nil {
		t.Fatalf("a smart send accepted manifests signed by the old key without --trustSigner:\n%s", logs)
	}
	if !strings.Contains(err.Error()+logs, "pass --trustSigner") {
		t.Errorf("the send failed, but its error does not point at --trustSigner: %v\n%s", err, logs)
	}

	stranger := fmt.Sprintf("%X", newKey(t).PrimaryKey.Fingerprint)
	logs, err = guarded(t, func() (string, error) {
		return env.send(append([]string{"--trustSigner", stranger}, smart...)...)
	})
	if err == nil || !strings.Contains(logs, stranger) {
		t.Errorf("--trustSigner with a key not in the public ring: want an error naming it, got %v\n%s", err, logs)
	}

	old := fmt.Sprintf("%X", k1.PrimaryKey.Fingerprint)
	env.sendOK(t, append([]string{"--trustSigner", old}, smart...)...)

	receive := append(append([]string{}, rotated...), "--auto", "tank/data@b", dest, "restored/data")
	if logs, err = guarded(t, func() (string, error) { return env.receive(receive...) }); err == nil {
		t.Errorf("receive of a chain spanning the rotation succeeded without --trustSigner:\n%s", logs)
	} else if !strings.Contains(err.Error()+logs, "pass --trustSigner") {
		t.Errorf("the receive failed, but its error does not point at --trustSigner: %v\n%s", err, logs)
	}
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	if logs, err = guarded(t, func() (string, error) {
		return env.receive(append([]string{"--trustSigner", old}, receive...)...)
	}); err != nil {
		t.Fatalf("receive --trustSigner <old> of a chain spanning the rotation: %v\n%s", err, logs)
	}
	got, _ := os.ReadFile(receiveLog)
	if n := strings.Count(string(got), "\n"); n != 2 {
		t.Errorf("want 2 zfs receives (the full of a, then a to b), got %d\n%s", n, logs)
	}
}
