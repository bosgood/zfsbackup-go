//go:build integration

package main

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/cmd"
)

// TestIntegrationS3NativeEncryption backs up a dataset under a ZFS native
// encryption root, as production's backup3/enc is: a raw chain sent with and
// without the key loaded, a raw restore that opens only with the sender's key,
// a raw incremental from a bookmark, a forgotten --raw refused, a plaintext
// send of the encrypted dataset, and raw + GPG stacked.
func TestIntegrationS3NativeEncryption(t *testing.T) {
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-nat")
	target := "s3://" + bucket + "/" + prefix
	client2, bucket2, prefix2 := itBucket(t, "zfsbackup-it-nat2")
	target2 := "s3://" + bucket2 + "/" + prefix2
	client3, bucket3, prefix3 := itBucket(t, "zfsbackup-it-nat3")
	target3 := "s3://" + bucket3 + "/" + prefix3
	_, bucket4, prefix4 := itBucket(t, "zfsbackup-it-nat4")
	target4 := "s3://" + bucket4 + "/" + prefix4
	root := pool + "/enc"
	ds := root + "/user/app/navidrome"
	restoredRaw := pool + "/restoredRaw"
	restoredPlain := pool + "/restoredPlain"
	restoredIn := root + "/restoredIn"
	restoredBoth := pool + "/restoredBoth"
	// Clones before root, whose descendants they are clones of.
	destroyAfter(t, pool+"/encm1", pool+"/encm3", restoredRaw, restoredPlain, restoredBoth, root)
	work := t.TempDir()

	keyFile := filepath.Join(t.TempDir(), "key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	zfsRun(t, "create", "-o", "encryption=aes-256-gcm", "-o", "keyformat=raw", "-o", "keylocation=file://"+keyFile, root)
	zfsRun(t, "create", "-p", ds)

	prop := func(dataset, name string) string {
		t.Helper()
		return strings.TrimSpace(zfsRun(t, "get", "-H", "-o", "value", name, dataset))
	}
	loadAndMount := func(dataset string) {
		t.Helper()
		zfsRun(t, "load-key", "-L", "file://"+keyFile, dataset)
		zfsRun(t, "mount", dataset)
	}
	smart := []string{
		"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly",
		"--incrementalSnapshotSuffix", "_monthly", "--volsize", "1", "--compressor", "",
	}
	raw := append(slices.Clone(smart), "--raw")
	send := func(args ...string) {
		t.Helper()
		if logs, err := itSend(t, work, args...); err != nil {
			t.Fatalf("send %v: %v\n%s", args, err, logs)
		}
	}
	wantLink := func(snap, source string, bookmark bool) {
		t.Helper()
		b := manifestFor(t, work, ds, target, snap)
		if b.IncrementalSnapshot.Name != source || b.IncrementalSnapshot.Bookmark != bookmark || !b.Raw {
			t.Fatalf("%s sent from %q (bookmark %v, raw %v), want from %q (bookmark %v), raw",
				snap, b.IncrementalSnapshot.Name, b.IncrementalSnapshot.Bookmark, b.Raw, source, bookmark)
		}
	}
	mountFails := func(dataset string) {
		t.Helper()
		if out, err := exec.Command("zfs", "mount", dataset).CombinedOutput(); err == nil {
			t.Fatalf("zfs mount %s without its key succeeded, want an error", dataset)
		} else {
			t.Logf("zfs mount %s without the key: %v: %s", dataset, err, strings.TrimSpace(string(out)))
		}
	}

	// N1: a raw full.
	writeFile(t, "/"+ds, "a.bin", 3<<20)
	m1 := snapshotTie(t, ds, "monthly", "daily", "hourly")[0]
	send(append(slices.Clone(raw), ds, target)...)
	wantLink(m1, "", false)

	// N2: the key is not loaded on the backup host (a reboot before the cron run).
	writeFile(t, "/"+ds, "b.bin", 1<<20)
	m2 := snapshotTie(t, ds, "monthly", "daily", "hourly")[0]
	zfsRun(t, "unmount", ds)
	zfsRun(t, "unmount", root)
	zfsRun(t, "unload-key", "-r", root)
	if got := prop(root, "keystatus"); got != "unavailable" {
		t.Fatalf("%s keystatus %s after unload-key, want unavailable", root, got)
	}
	send(append(slices.Clone(raw), ds, target)...)
	wantLink(m2, m1, false)
	if logs, err := itSend(t, work, "--volsize", "1", "--compressor", "", "-i", ds+"@"+m1, ds+"@"+m2, target2); err == nil {
		t.Fatalf("a plaintext send with the key unloaded succeeded, want an error:\n%s", logs)
	}
	if _, manifests, list := countObjects(t, client2, bucket2, prefix2); manifests != 0 {
		t.Fatalf("%s holds %d manifests after the failed send, want 0:\n%s", target2, manifests, list)
	}
	zfsRun(t, "load-key", "-r", root)
	zfsRun(t, "mount", root)
	zfsRun(t, "mount", ds)

	// N3: a raw restore lands without the key and opens nothing without it.
	itReceive(t, t.TempDir(), "--auto", "-F", "--unmounted", ds, target, restoredRaw)
	wantSnapshots(t, restoredRaw, m1, m2)
	wantGUIDs(t, ds, restoredRaw, m1, m2)
	if got := prop(restoredRaw, "keystatus"); got != "unavailable" {
		t.Errorf("%s keystatus %s, want unavailable", restoredRaw, got)
	}
	if got := prop(restoredRaw, "encryptionroot"); got != restoredRaw {
		t.Errorf("%s encryptionroot %s, want itself", restoredRaw, got)
	}
	mountFails(restoredRaw)

	// N4: with the sender's key.
	loadAndMount(restoredRaw)
	compareDirs(t, "/"+ds, "/"+restoredRaw)

	// N5: a raw incremental from a bookmark.
	zfsRun(t, "bookmark", ds+"@"+m2, ds+"#"+m2)
	zfsRun(t, "destroy", ds+"@"+m2)
	writeFile(t, "/"+ds, "c.bin", 1<<20)
	m3 := snapshotTie(t, ds, "monthly", "daily", "hourly")[0]
	send(append(slices.Clone(raw), ds, target)...)
	wantLink(m3, m2, true)
	itReceive(t, t.TempDir(), "--auto", "-F", "--unmounted", ds, target, restoredRaw)
	wantSnapshots(t, restoredRaw, m1, m2, m3)
	wantGUIDs(t, ds, restoredRaw, m1, m2, m3)
	compareDirs(t, "/"+ds, "/"+restoredRaw)

	// N6: a forgotten --raw is refused, smart and explicit; nothing is uploaded.
	writeFile(t, "/"+ds, "d.bin", 1<<20)
	m4 := snapshotTie(t, ds, "monthly", "daily", "hourly")[0]
	before := objectNames(t, client, bucket, prefix)
	for _, args := range [][]string{
		append(slices.Clone(smart), ds, target),
		{"--volsize", "1", "--compressor", "", "-i", ds + "@" + m3, ds + "@" + m4, target},
	} {
		logs, err := itSend(t, work, args...)
		if err == nil || !strings.Contains(err.Error(), "mix raw and plaintext") {
			t.Fatalf("send %v without --raw onto the raw chain: got %v, want a mixed-chain error\n%s", args, err, logs)
		}
		if got := objectNames(t, client, bucket, prefix); strings.Join(got, " ") != strings.Join(before, " ") {
			t.Fatalf("the refused send %v changed %s: %v, want %v", args, target, got, before)
		}
	}
	listed := itList(t, work, target)[ds]
	if len(listed) != 3 {
		t.Fatalf("list holds %v, want 3 backups", chainNames(listed))
	}
	for _, b := range listed {
		if !b.Raw {
			t.Errorf("the backup of %s is not raw", b.BaseSnapshot.Name)
		}
	}
	send(append(slices.Clone(raw), ds, target)...)
	wantLink(m4, m3, false)
	itReceive(t, t.TempDir(), "--auto", "-F", "--unmounted", ds, target, restoredRaw)
	wantSnapshots(t, restoredRaw, m1, m2, m3, m4)
	wantGUIDs(t, ds, restoredRaw, m4)
	compareDirs(t, "/"+ds, "/"+restoredRaw)
	// A new chain may change the kind.
	send("--full", "--fullSnapshotSuffix", "_monthly", "--volsize", "1", "--compressor", "", ds, target4)
	if b := manifestFor(t, work, ds, target4, m4); b.Raw || b.IncrementalSnapshot.Name != "" {
		t.Errorf("the full of %s at %s: raw %v, source %q; want a plaintext full", m4, target4, b.Raw, b.IncrementalSnapshot.Name)
	}

	// N7: a plaintext send of the encrypted dataset (key loaded).
	send("--volsize", "1", "--compressor", "", ds+"@"+m1, target2)
	if manifestFor(t, work, ds, target2, m1).Raw {
		t.Errorf("the plaintext send of %s is recorded as raw", m1)
	}
	m1Clone := cloneOf(t, pool, ds, m1, "encm1")
	itReceive(t, t.TempDir(), "-F", ds+"@"+m1, target2, restoredPlain)
	if got := prop(restoredPlain, "encryption"); got != "off" {
		t.Errorf("%s encryption %s, want off", restoredPlain, got)
	}
	compareDirs(t, m1Clone, "/"+restoredPlain)
	itReceive(t, t.TempDir(), "-F", ds+"@"+m1, target2, restoredIn)
	if got := prop(restoredIn, "encryption"); got != "aes-256-gcm" {
		t.Errorf("%s encryption %s, want aes-256-gcm (inherited)", restoredIn, got)
	}
	if got := prop(restoredIn, "encryptionroot"); got != root {
		t.Errorf("%s encryptionroot %s, want %s", restoredIn, got, root)
	}
	compareDirs(t, m1Clone, "/"+restoredIn)

	// N8: raw + GPG.
	rings := writeRings(t, newKey(t))
	send(append(append([]string{"--raw"}, rings...), "--volsize", "1", "--compressor", "", ds+"@"+m3, target3)...)
	vol1 := manifestFor(t, work, ds, target3, m3, rings...).Volumes[0].ObjectName
	wantPGPVolume(t, vol1, objectBytes(t, client3, bucket3, prefix3+vol1))
	for _, n := range objectNames(t, client3, bucket3, prefix3) {
		if strings.Contains(n, ".manifest") {
			wantPGPVolume(t, n, objectBytes(t, client3, bucket3, prefix3+n))
		}
	}
	if logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), "-F", "--unmounted", ds+"@"+m3, target3, restoredBoth); err == nil {
		t.Fatalf("receive of raw + GPG without rings succeeded, want an error:\n%s", logs)
	}
	wantNoDataset(t, restoredBoth)
	itReceive(t, t.TempDir(), append(slices.Clone(rings), "-F", "--unmounted", ds+"@"+m3, target3, restoredBoth)...)
	wantSnapshots(t, restoredBoth, m3)
	if got := prop(restoredBoth, "keystatus"); got != "unavailable" {
		t.Errorf("%s keystatus %s, want unavailable", restoredBoth, got)
	}
	loadAndMount(restoredBoth)
	compareDirs(t, cloneOf(t, pool, ds, m3, "encm3"), "/"+restoredBoth)

	// N9: the raw chain's destination is consistent.
	logs, err := itClean(t, t.TempDir(), "--dry-run", target)
	if err != nil || !strings.Contains(logs, "Dry-run: would delete 0 objects") {
		t.Fatalf("clean --dry-run: %v, want 0 deletes:\n%s", err, logs)
	}
}
