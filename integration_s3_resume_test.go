//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/bosgood/zfsbackup-go/cmd"
)

// flakyZFS writes a zfs wrapper for --zfsPath and returns its path and a func
// that arms it. Armed, it cuts the next `zfs send` stream (not the `zfs send
// -n` estimate) after cutBytes, stalls for stall seconds and fails. Every other
// call goes to the real zfs. The real zfs send gets SIGPIPE when head exits:
// the intended death. The stall lets the volumes cut before it finish their
// uploads: a failed stream cancels the uploads still in flight, and a send
// that dies within milliseconds leaves nothing to resume. The stall is a timing
// guess: a stalled VM fails the "same volumes" check; rerun before you debug.
func flakyZFS(t *testing.T, cutBytes int64, stall int) (path string, arm func()) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "armed")
	path = filepath.Join(dir, "zfs")
	script := fmt.Sprintf(`#!/bin/bash
marker=%q
if [ "$1" = send ] && [ -e "$marker" ] && [[ " $* " != *" -n "* ]]; then
	rm -f "$marker"
	zfs "$@" | head -c %d
	sleep %d
	echo "flaky zfs: stream cut after %d bytes" >&2
	exit 1
fi
exec zfs "$@"
`, marker, cutBytes, stall, cutBytes)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable script
		t.Fatal(err)
	}
	return path, func() {
		t.Helper()
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIntegrationS3Resume kills a zfs send mid-stream, checks that clean
// spares the volumes it uploaded while the cache holds the partial manifest,
// and resumes it: after the kill, after the destination lost a volume and the
// manifest, and after it lost only the manifest.
func TestIntegrationS3Resume(t *testing.T) {
	const cut = 5 << 19 // 2.5 MiB: two complete 1 MiB volumes, then the cut
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-res")
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcres"
	restoredRes := pool + "/restoredRes"
	restoredRes2 := pool + "/restoredRes2"
	destroyAfter(t, src, restoredRes, restoredRes2)
	work := t.TempDir()
	flaky, arm := flakyZFS(t, cut, 5)
	send := func(zfsPath string, args ...string) (string, error) {
		t.Helper()
		_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, zfsPath, args...)
		return logs, err
	}
	manifests := func(names []string) (m []string) {
		for _, n := range names {
			if strings.Contains(n, ".manifest") {
				m = append(m, n)
			}
		}
		return m
	}

	// A complete backup first: clean refuses a destination with no manifest at
	// all, so without it clean could not show what the partial protects.
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

	arm()
	logs, err := send(flaky, args...)
	if err == nil || !strings.Contains(logs, "flaky zfs") {
		t.Fatalf("send through the flaky zfs: got %v, want its failure in the log:\n%s", err, logs)
	}
	for _, m := range (&e2eEnv{work: work}).cachedManifests(t) {
		for _, v := range m.Volumes {
			if m.BaseSnapshot.Name == snap && v.ZFSStreamBytes < 1<<20-50<<10 {
				t.Errorf("cached manifest lists %s with only %d stream bytes: the truncated tail", v.ObjectName, v.ZFSStreamBytes)
			}
		}
	}
	partial := cutPartial(t, work, snap, before, objectNames(t, client, bucket, prefix), logs)
	afterKill := objectNames(t, client, bucket, prefix)
	if len(partial) != 2 {
		t.Fatalf("after the kill the cached partial manifest lists %v, want 2 volumes:\n%s", partial, logs)
	}

	// The cache protects the partial's volumes; without it they are orphans.
	logs, err = itClean(t, work, "--dry-run", target)
	if err != nil || !strings.Contains(logs, "Dry-run: would delete 0 objects") {
		t.Fatalf("clean --dry-run with the cache: %v, want 0 deletes:\n%s", err, logs)
	}
	logs, err = itClean(t, t.TempDir(), "--dry-run", target)
	if err != nil || !strings.Contains(logs, "Dry-run: would delete 2 objects") {
		t.Fatalf("clean --dry-run without the cache: %v, want the 2 partial volumes deleted:\n%s", err, logs)
	}
	for _, v := range partial {
		if want := "Would delete " + strings.TrimSuffix(target, "/") + "/" + v + "."; !strings.Contains(logs, want) {
			t.Errorf("want %q in the log:\n%s", want, logs)
		}
	}
	if got := objectNames(t, client, bucket, prefix); strings.Join(got, " ") != strings.Join(afterKill, " ") {
		t.Fatalf("a dry-run changed the destination: %v, want %v", got, afterKill)
	}

	resume := func(wantLog ...string) string {
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
		return logs
	}
	restore := func(work, dataset string) {
		t.Helper()
		itReceive(t, work, "-F", src+"@"+snap, target, dataset)
		wantSnapshots(t, dataset, snap)
		wantGUIDs(t, src, dataset, snap)
		compareDirs(t, "/"+src, "/"+dataset)
	}

	resume("Resuming from volume 3: 2 of")
	if got := manifests(objectNames(t, client, bucket, prefix)); len(got) != 2 {
		t.Fatalf("after the resume the destination holds manifests %v, want 2", got)
	}
	restore(work, restoredRes)

	// The destination lost volume 1 and the manifest: all is sent again.
	m := manifestFor(t, work, src, target, snap)
	var manifest string
	for _, n := range manifests(objectNames(t, client, bucket, prefix)) {
		if strings.Contains(n, snap) {
			manifest = n
		}
	}
	if manifest == "" {
		t.Fatalf("no manifest of %s at %s", snap, target)
	}
	vol1 := m.Volumes[0].ObjectName
	complete := objectNames(t, client, bucket, prefix)
	deleteObject(t, client, bucket, prefix+vol1)
	deleteObject(t, client, bucket, prefix+manifest)
	resume("Volume "+vol1+" missing at", "Resuming from volume 1: 0 of")
	if got := objectNames(t, client, bucket, prefix); strings.Join(got, " ") != strings.Join(complete, " ") {
		t.Fatalf("after the resume the destination holds %v, want %v", got, complete)
	}
	// From an empty cache: the restore reads the re-sent manifest at the destination.
	restore(t.TempDir(), restoredRes2)

	// The destination lost only the manifest: the volumes stay, the manifest comes back.
	deleteObject(t, client, bucket, prefix+manifest)
	if logs := resume(); strings.Contains(logs, "missing at") {
		t.Errorf("resume with every volume present re-sent one:\n%s", logs)
	}
	if got := objectNames(t, client, bucket, prefix); strings.Join(got, " ") != strings.Join(complete, " ") {
		t.Fatalf("after the resume the destination holds %v, want %v", got, complete)
	}
	// The default compressor: pgzip holds several MiB in its blocks, so the cut
	// leaves some number of complete volumes, not a known one.
	t.Run("Gzip", func(t *testing.T) {
		client, bucket, prefix := itBucket(t, "zfsbackup-it-resgz")
		target := "s3://" + bucket + "/" + prefix
		src := pool + "/srcresgz"
		restored := pool + "/restoredResGz"
		destroyAfter(t, src, restored)
		work := t.TempDir()
		flaky, arm := flakyZFS(t, 12<<20, 5)
		zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
		writeFile(t, "/"+src, "a.bin", 16<<20)
		snap := snapshot(t, src, "daily")
		args := []string{"--volsize", "1", src + "@" + snap, target}

		arm()
		_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, flaky, args...)
		if err == nil || !strings.Contains(logs, "flaky zfs") {
			t.Fatalf("send through the flaky zfs: got %v, want its failure in the log:\n%s", err, logs)
		}
		partial := cutPartial(t, work, snap, nil, objectNames(t, client, bucket, prefix), logs)
		if len(partial) == 0 {
			t.Fatalf("after the kill the cached partial manifest lists no volume:\n%s", logs)
		}

		_, logs, err = itRunOut(cmd.ResetSendJobInfo, "send", work, "zfs", append([]string{"--resume"}, args...)...)
		if err != nil {
			t.Fatalf("send --resume: %v\n%s", err, logs)
		}
		if want := fmt.Sprintf("Resuming from volume %d: %d of", len(partial)+1, len(partial)); !strings.Contains(logs, want) {
			t.Errorf("want %q in the log:\n%s", want, logs)
		}
		itReceive(t, t.TempDir(), "-F", src+"@"+snap, target, restored)
		wantSnapshots(t, restored, snap)
		wantGUIDs(t, src, restored, snap)
		compareDirs(t, "/"+src, "/"+restored)
	})
}

// cutSend sends src@snap to s3://<bucket>/<prefix> through a zfs that cuts the
// stream after 2.5 MiB, and returns the volumes the cached partial manifest
// lists: the complete 1 MiB volumes before the cut, also at the destination.
// wantVolumes is how many the caller's data gives (4 MiB of random bytes: 2).
// extra goes before the snapshot ("-i", a source). It fails unless the send
// failed by the cut and left exactly wantVolumes volumes.
func cutSend(t *testing.T, client *s3.S3, bucket, prefix, work, src, snap string, wantVolumes int, extra ...string) []string {
	t.Helper()
	target := "s3://" + bucket + "/" + prefix
	flaky, arm := flakyZFS(t, 5<<19, 5)
	before := objectNames(t, client, bucket, prefix)
	args := append(append([]string{"--volsize", "1", "--compressor", ""}, extra...), src+"@"+snap, target)
	arm()
	_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, flaky, args...)
	if err == nil || !strings.Contains(logs, "flaky zfs") {
		t.Fatalf("send through the flaky zfs: got %v, want its failure in the log:\n%s", err, logs)
	}
	partial := cutPartial(t, work, snap, before, objectNames(t, client, bucket, prefix), logs)
	if len(partial) != wantVolumes {
		t.Fatalf("after the cut the cached partial manifest lists %v, want %d volumes (the snapshot's data decides):\n%s", partial, wantVolumes, logs)
	}
	return partial
}

// cutPartial returns the volumes the cached partial manifest of snap in work
// lists, sorted, and fails unless they are exactly the names in after that are
// not in before: what the destination got from the cut send.
func cutPartial(t *testing.T, work, snap string, before, after []string, logs string) []string {
	t.Helper()
	var partial []string
	for _, m := range (&e2eEnv{work: work}).cachedManifests(t) {
		if m.BaseSnapshot.Name != snap {
			continue
		}
		for _, v := range m.Volumes {
			partial = append(partial, v.ObjectName)
		}
	}
	added := slices.DeleteFunc(slices.Clone(after), func(n string) bool { return slices.Contains(before, n) })
	sort.Strings(partial)
	if strings.Join(added, " ") != strings.Join(partial, " ") {
		t.Fatalf("after the kill the cached partial manifest lists %v and the destination got %v; want the same volumes:\n%s",
			partial, added, logs)
	}
	return partial
}
