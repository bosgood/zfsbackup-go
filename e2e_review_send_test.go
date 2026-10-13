package main

// Regression tests for the send findings 1, 11, 14 (the Warning), 15, 16 and 17 of
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md. Each one failed on
// the code before the fix.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// stderrZFS writes a bash wrapper for --zfsPath: a real `zfs send` (not -n) first prints a
// warning on stderr, as -R -s does for a child without the snapshot, then runs the fake, which
// exits 0. Every other call goes to the fake.
func stderrZFS(t *testing.T, env *e2eEnv) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
if [ "$1" = send ] && [[ " $* " != *" -n "* ]]; then
    echo "WARNING: could not send tank/data/child@a: does not exist" >&2
fi
exec %q "$@"
`, env.self)
	path := filepath.Join(t.TempDir(), "zfs")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func reviewOneSnapshot(t *testing.T, env *e2eEnv) {
	t.Helper()
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
}

// recreatedSource backs up a full of tank/data@a, then destroys a and takes it again a day later
// (new creation time, so a new guid in the fake), and takes b after it.
func recreatedSource(t *testing.T, env *e2eEnv) (dest string, full *files.JobInfo) {
	t.Helper()
	t1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: t1}})
	dest = "file://" + env.dest
	env.sendOK(t, "--compressor", "", "tank/data@a", dest)
	full = newestBackup(t, "tank/data", dest)
	t2 := t1.Add(24 * time.Hour)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "b", CreationTime: t2.Add(time.Hour)},
		{Name: "a", CreationTime: t2},
	})
	return dest, full
}

// Finding 1: an explicit -i from a snapshot that was destroyed and taken again under the same
// name. The destination holds a full of the OLD snapshot only. The send must refuse; before the
// fix it published an incremental that no restore can apply, and exited 0.
func TestReviewSendRefusesIncrementalFromRecreatedSource(t *testing.T) {
	env := newE2EEnv(t)
	dest, _ := recreatedSource(t, env)
	logs, err := env.send("--compressor", "", "-i", "tank/data@a", "tank/data@b", dest)
	if err == nil {
		t.Fatalf("send -i from the re-created a succeeded:\n%s", logs)
	}
	if !strings.Contains(logs, "destroyed and created again") {
		t.Errorf("the refusal does not say why:\n%s", logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 1 {
		t.Errorf("destination holds manifests %q, want only the full of a", names)
	}
}

// Finding 1: when the destination holds no backup of the source at all, say so at Notice (it was
// an Info line, hidden at the default level), and send.
func TestReviewSendNoticesNoBackupOfSource(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "b", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	dest := "file://" + env.dest
	logs, err := env.send("--compressor", "", "-i", "a", "tank/data@b", dest)
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "No backup of tank/data@a at ") {
		t.Errorf("no Notice that the destination has no backup of the source:\n%s", logs)
	}
}

// Finding 11: a bare -i name that starts with the dataset name ("tanknight" of tank) must be the
// snapshot tank@tanknight, not tank@night.
func TestReviewSendIncrementalNameStartsWithVolume(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank", []files.SnapshotInfo{
		{Name: "c", CreationTime: time.Unix(400, 0)},
		{Name: "tanknight", CreationTime: time.Unix(300, 0)},
		{Name: "night", CreationTime: time.Unix(200, 0)},
	})
	logs, err := env.send("--compressor", "", "-i", "tanknight", "tank@c", "file://"+env.dest)
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	got := newestBackup(t, "tank", "file://"+env.dest)
	if got.IncrementalSnapshot.Name != "tanknight" {
		t.Errorf("manifest records an incremental from %q, want tanknight", got.IncrementalSnapshot.Name)
	}
	zlog, _ := os.ReadFile(env.zfsLog)
	if !strings.Contains(string(zlog), "-i tanknight tank@c") {
		t.Errorf("zfs send was not asked for -i tanknight; zfs log:\n%s", zlog)
	}
}

// Finding 11, receive side: `receive -i tanknight tank@c` must look for the incremental from
// tanknight. The send names the source in full, which was trimmed right before the fix too.
func TestReviewReceiveIncrementalNameStartsWithVolume(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank", []files.SnapshotInfo{
		{Name: "c", CreationTime: time.Unix(400, 0)},
		{Name: "tanknight", CreationTime: time.Unix(300, 0)},
		{Name: "night", CreationTime: time.Unix(200, 0)},
	})
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "-i", "tank@tanknight", "tank@c", dest)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--origin", "tank@tanknight", "-i", "tanknight", "tank@c", dest, "restored/data")
	})
	if err != nil {
		t.Fatalf("receive -i tanknight: %v\n%s", err, logs)
	}
	if strings.Contains(logs, "from night") {
		t.Errorf("receive looked for an incremental from night:\n%s", logs)
	}
}

// Finding 14: a dry run whose size estimate fails (zfs refuses the send) must say so at the
// default log level; it was a Debug line.
func TestReviewDryRunEstimateFailureWarns(t *testing.T) {
	env := newE2EEnv(t)
	reviewOneSnapshot(t, env)
	t.Setenv("FAKEZFS_DRYRUN_OUTPUT", "garbage")
	logs, err := env.send("-n", "tank/data@a", "file://"+env.dest)
	if err != nil {
		t.Fatalf("send -n: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "Dry-run: could not estimate ZFS send size") {
		t.Errorf("the failed estimate is not in the log at the default level:\n%s", logs)
	}
}

// Finding 15: a dry run of a set that is already at the destination must refuse it, as the
// real send does.
func TestReviewDryRunOfExistingSetIsRefused(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(1<<20))
	reviewOneSnapshot(t, env)
	args := []string{"tank/data@a", "file://" + env.dest}
	env.sendOK(t, args...)
	before := destObjects(t, env.dest)
	logs, err := env.send(append([]string{"-n"}, args...)...)
	if err == nil || !strings.Contains(logs, "already exists") {
		t.Errorf("send -n of an existing set: got %v, want the refusal the real send gives:\n%s", err, logs)
	}
	if after := destObjects(t, env.dest); len(after) != len(before) {
		t.Errorf("the dry run changed the destination: %d objects, then %d", len(before), len(after))
	}
}

// Finding 15 with finding 1: the dry run refuses the incremental from a re-created source too.
func TestReviewDryRunOfIncrementalFromRecreatedSourceIsRefused(t *testing.T) {
	env := newE2EEnv(t)
	dest, _ := recreatedSource(t, env)
	logs, err := env.send("-n", "--compressor", "", "-i", "tank/data@a", "tank/data@b", dest)
	if err == nil || !strings.Contains(logs, "destroyed and created again") {
		t.Errorf("send -n -i from the re-created a: got %v, want the refusal:\n%s", err, logs)
	}
}

// Finding 15: a dry run of a manifest over the limit refuses it, as the real send does.
func TestReviewDryRunOfOversizedManifestIsRefused(t *testing.T) {
	env := newE2EEnv(t)
	reviewOneSnapshot(t, env)
	// 10 TiB at --volsize 1 is some 10 million volumes.
	t.Setenv("FAKEZFS_DRYRUN_OUTPUT", fmt.Sprintf("full\ttank/data@a\t%d\nsize\t%d\n", int64(10)<<40, int64(10)<<40))
	logs, err := env.send("-n", "--volsize", "1", "tank/data@a", "file://"+env.dest)
	if err == nil || !strings.Contains(logs, "send with a --volsize of at least") {
		t.Errorf("send -n of an oversized manifest: got %v, want the refusal:\n%s", err, logs)
	}
}

// Finding 15: the dry run of a set that is at one of two destinations, with --resume, only says
// that it would copy the manifest. It must not copy it.
func TestReviewDryRunOfPartialSetWritesNothing(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	reviewOneSnapshot(t, env)
	dest2 := filepath.Join(t.TempDir(), "dest2")
	if err := os.Mkdir(dest2, 0o700); err != nil {
		t.Fatal(err)
	}
	d1, d2 := "file://"+env.dest, "file://"+dest2
	env.sendOK(t, "--compressor", "", "tank/data@a", d1)
	logs, err := env.send("-n", "--resume", "--compressor", "", "tank/data@a", d1+","+d2)
	if err != nil {
		t.Fatalf("send -n --resume: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "would check its volumes there and copy its manifest") {
		t.Errorf("the dry run does not say it would copy the manifest:\n%s", logs)
	}
	if objects := destObjects(t, dest2); len(objects) != 0 {
		t.Errorf("the dry run wrote %d objects to the second destination", len(objects))
	}
}

// Finding 16: --volsize 0 gave one volume of unlimited size; on S3 that fails after 10,000 parts.
func TestReviewVolsizeZeroIsRefused(t *testing.T) {
	env := newE2EEnv(t)
	reviewOneSnapshot(t, env)
	logs, err := env.send("--volsize", "0", "--compressor", "", "tank/data@a", "file://"+env.dest)
	if err == nil {
		t.Errorf("send --volsize 0 succeeded:\n%s", logs)
	}
	if objects := destObjects(t, env.dest); len(objects) != 0 {
		t.Errorf("send --volsize 0 wrote %d objects", len(objects))
	}
}

// Finding 16: a --volsize that S3 cannot take in 10,000 parts of --uploadChunkSize is refused
// before anything connects to S3.
func TestReviewVolsizeOverS3PartsIsRefused(t *testing.T) {
	env := newE2EEnv(t)
	reviewOneSnapshot(t, env)
	volsize := fmt.Sprint(files.S3MaxVolumeParts*10 + 1)
	logs, err := env.send("--volsize", volsize, "--uploadChunkSize", "10", "tank/data@a", "s3://bucket/prefix")
	if err == nil || !strings.Contains(logs, "too large for the s3:// destination") {
		t.Errorf("send --volsize %s to s3: got %v, want the refusal:\n%s", volsize, err, logs)
	}
}

// Finding 17: zfs send prints a warning on stderr (as -R -s does for a skipped child) and exits
// 0. The operator must see it at the default log level.
func TestReviewZFSStderrWarningIsLogged(t *testing.T) {
	env := newE2EEnv(t)
	reviewOneSnapshot(t, env)
	logs, err := guarded(t, func() (string, error) {
		return env.send("--zfsPath", stderrZFS(t, env), "-R", "--skip-missing", "tank/data@a", "file://"+env.dest)
	})
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "could not send tank/data/child@a") {
		t.Errorf("zfs send's stderr warning is not in the log at the default level:\n%s", logs)
	}
}
