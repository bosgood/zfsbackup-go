//go:build integration

package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3EncryptedResume is TestIntegrationS3Resume with rings: the
// volumes of the cut send are ciphertext, a resume that re-sends a volume
// encrypts it with a new session key, a tampered volume fails the restore,
// and a bookmark is the source of an encrypted incremental.
func TestIntegrationS3EncryptedResume(t *testing.T) {
	// 3.5 MiB: two or three complete volumes, then the cut. An encrypted volume ends one
	// copy step (512 KiB) late: go-crypto's packet writer holds data back, so the counter
	// sees under 974 KiB after 1 MiB of stream, and a volume holds 1.5 MiB of it
	// (files.TestEncryptedVolumeStreamBytes).
	const cut = 7 << 19
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-encres")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcencres"
	restoredER := pool + "/restoredER"
	restoredER2 := pool + "/restoredER2"
	restoredER3 := pool + "/restoredER3"
	restoredER4 := pool + "/restoredER4"
	destroyAfter(t, src, restoredER, restoredER2, restoredER3, restoredER4)
	work := t.TempDir()
	rings := writeRings(t, newKey(t))
	flaky, arm := flakyZFS(t, cut, 5)
	send := func(zfsPath string, args ...string) (string, error) {
		t.Helper()
		_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, zfsPath, append(slices.Clone(rings), args...)...)
		return logs, err
	}
	// A complete backup first: clean and resume need a destination with a manifest.
	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "first.bin", 64<<10)
	first := snapshot(t, src, "first")
	if logs, err := send("zfs", "--volsize", "1", "--compressor", "", src+"@"+first, target); err != nil {
		t.Fatalf("send %s: %v\n%s", first, err, logs)
	}
	before := objectNames(t, client, bucket, prefix)

	writeFile(t, "/"+src, "a.bin", 4<<20)
	snap := snapshot(t, src, "daily")
	args := []string{"--volsize", "1", "--compressor", "", src + "@" + snap, target}
	restore := func(dataset string) {
		t.Helper()
		itReceive(t, t.TempDir(), append(slices.Clone(rings), "-F", src+"@"+snap, target, dataset)...)
		wantSnapshots(t, dataset, snap)
		wantGUIDs(t, src, dataset, snap)
		compareDirs(t, "/"+src, "/"+dataset)
	}
	resume := func(wantLog ...string) {
		t.Helper()
		logs, err := send("zfs", append([]string{"--resume"}, args...)...)
		if err != nil {
			t.Fatalf("send --resume: %v\n%s", err, logs)
		}
		for _, w := range wantLog {
			if !strings.Contains(logs, w) {
				t.Errorf("want %q in the log:\n%s", w, logs)
			}
		}
	}
	manifestOf := func(snap string) string {
		t.Helper()
		for _, n := range objectNames(t, client, bucket, prefix) {
			if strings.Contains(n, ".manifest") && strings.Contains(n, snap) {
				return n
			}
		}
		t.Fatalf("no manifest of %s at %s", snap, target)
		return ""
	}

	// The cut. The cached partial manifest is encrypted too, so the test reads
	// its volume count from the resume's log, not from the cache.
	arm()
	logs, err := send(flaky, args...)
	if err == nil || !strings.Contains(logs, "flaky zfs") {
		t.Fatalf("send through the flaky zfs: got %v, want its failure in the log:\n%s", err, logs)
	}
	added := slices.DeleteFunc(objectNames(t, client, bucket, prefix), func(n string) bool { return slices.Contains(before, n) })
	kept := len(added)
	if kept < 2 || kept > 3 {
		t.Fatalf("after the kill the destination got %v, want 2 or 3 volumes:\n%s", added, logs)
	}
	wantPGPVolume(t, added[0], objectBytes(t, client, bucket, prefix+added[0]))

	resume(fmt.Sprintf("Resuming from volume %d: %d of %d cached volumes", kept+1, kept, kept))
	m := manifestFor(t, work, src, target, snap, rings...)
	if len(m.Volumes) <= kept {
		t.Fatalf("%s has %d volumes, want %d+", snap, len(m.Volumes), kept+1)
	}
	var head []string
	for _, v := range m.Volumes[:kept] {
		head = append(head, v.ObjectName)
	}
	if !slices.Equal(added, head) {
		t.Errorf("the cut send uploaded %v, the manifest starts with %v", added, head)
	}
	wantPGPVolume(t, m.Volumes[kept].ObjectName, objectBytes(t, client, bucket, prefix+m.Volumes[kept].ObjectName))
	restore(restoredER)

	// Volume 1 and the manifest are gone: all is sent again, with a new
	// session key, so volume 1 has new bytes and a new hash.
	h1 := m.Volumes[0].SHA256Sum
	vol1 := m.Volumes[0].ObjectName
	deleteObject(t, client, bucket, prefix+vol1)
	deleteObject(t, client, bucket, prefix+manifestOf(snap))
	resume("Volume "+vol1+" missing at", "Resuming from volume 1: 0 of")
	// A fresh work: the cache of work may hold the old manifest under the same name.
	m2 := manifestFor(t, t.TempDir(), src, target, snap, rings...)
	if m2.Volumes[0].SHA256Sum == h1 {
		t.Errorf("re-sent volume 1 has the old hash %s, want new ciphertext", h1)
	}
	restore(restoredER2)

	// A tampered volume: the restore fails and leaves no dataset.
	vol2 := m2.Volumes[1].ObjectName
	good := objectBytes(t, client, bucket, prefix+vol2)
	bad := slices.Clone(good)
	bad[len(bad)/2] ^= 0xff
	putObject(t, client, bucket, prefix+vol2, bad)
	logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), append(slices.Clone(rings), "-F", src+"@"+snap, target, restoredER3)...)
	if err == nil {
		t.Fatalf("receive with a tampered %s succeeded, want an error:\n%s", vol2, logs)
	}
	wantNoDataset(t, restoredER3)
	if !strings.Contains(logs, vol2) {
		t.Errorf("want the tampered volume %s named in the log:\n%s", vol2, logs)
	}
	putObject(t, client, bucket, prefix+vol2, good)
	restore(restoredER3)

	// A bookmark as the source of an encrypted incremental.
	zfsRun(t, "bookmark", src+"@"+snap, src+"#"+snap)
	zfsRun(t, "destroy", src+"@"+snap)
	writeFile(t, "/"+src, "e.bin", 1<<20)
	snap2 := snapshot(t, src, "daily")
	if logs, err := send("zfs", "--volsize", "1", "--compressor", "", "-i", src+"#"+snap, src+"@"+snap2, target); err != nil {
		t.Fatalf("send from the bookmark: %v\n%s", err, logs)
	}
	if b := manifestFor(t, t.TempDir(), src, target, snap2, rings...); b.IncrementalSnapshot.Name != snap || !b.IncrementalSnapshot.Bookmark {
		t.Fatalf("%s sent from %q (bookmark %v), want from the bookmark %s", snap2, b.IncrementalSnapshot.Name, b.IncrementalSnapshot.Bookmark, snap)
	}
	itReceive(t, t.TempDir(), append(slices.Clone(rings), "--auto", "-F", src, target, restoredER4)...)
	wantSnapshots(t, restoredER4, snap, snap2)
	wantGUIDs(t, src, restoredER4, snap, snap2)
	compareDirs(t, "/"+src, "/"+restoredER4)
}
