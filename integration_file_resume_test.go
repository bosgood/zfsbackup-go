//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/files"
)

// localDest is a file:// or ssh:// destination whose files the test can see:
// a directory in the test pool and its URI.
type localDest struct {
	dir, target string
}

// destMaker makes a destination on a new dataset <pool>/<name>.
type destMaker func(t *testing.T, pool, name string, props ...string) localDest

func newFileDest(t *testing.T, pool, name string, props ...string) localDest {
	t.Helper()
	_, dir := itDestDataset(t, pool, name, props...)
	return localDest{dir: dir, target: itFileTarget(dir)}
}

// TestIntegrationFileResume resumes sends to file:// that were cut short: by
// a zfs send that died, then with a volume truncated by hand, and by a real
// SIGTERM in the middle of an upload.
func TestIntegrationFileResume(t *testing.T) {
	pool := testPool(t)
	testTruncatedResume(t, pool, "fres", newFileDest)
	t.Run("SIGTERM", func(t *testing.T) { testSIGTERMResume(t, pool, "fterm", newFileDest) })
}

// resumed is what testTruncatedResume leaves for more steps.
type resumed struct {
	src, snap, work string
	dest            localDest
	args            []string // the send's arguments, without --resume
	manifest        *files.JobInfo
}

// testTruncatedResume cuts a send of 24 MiB to a new destination after two
// volumes, truncates volume 2 there, and resumes: --resume finds volume 2
// short (the Sizer), sends it and the rest again, and the set restores.
func testTruncatedResume(t *testing.T, pool, name string, mk destMaker) resumed {
	t.Helper()
	src := pool + "/" + name + "src"
	newSource(t, src, 24<<20)
	snap := snapshot(t, src, "daily")
	dest := mk(t, pool, name+"dest")
	work := t.TempDir()
	args := append(slices.Clone(plainSend), append(slices.Clone(sshRetry), src+"@"+snap, dest.target)...)

	cutSendTo(t, work, snap, dest.dir, 2, args...)
	var vol2 *files.VolumeInfo
	for _, v := range cachedPartial(t, work, snap).Volumes {
		if v.VolumeNumber == 2 {
			vol2 = v
		}
	}
	if vol2 == nil {
		t.Fatalf("the cached partial manifest of %s lists no volume 2", snap)
	}
	half := vol2.Size / 2
	truncateFile(t, filepath.Join(dest.dir, vol2.ObjectName), int64(half))

	_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, "zfs", append([]string{"--resume"}, args...)...)
	if err != nil {
		t.Fatalf("send --resume: %v\n%s", err, logs)
	}
	wantInLog(t, logs,
		"Volume "+vol2.ObjectName+" at ",
		fmt.Sprintf(" is %d bytes, want %d; it and later volumes will be re-sent.", half, vol2.Size),
		"Resuming from volume 2: 1 of 2",
	)
	m := manifestFor(t, work, src, dest.target, snap)
	wantVolumeSizes(t, dest.dir, m)
	wantFilesAt(t, dest.dir, append(volumeNames(m), manifestFiles(fileNames(t, dest.dir))...))

	restored := pool + "/" + name + "Restored"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "-F", src+"@"+snap, dest.target, restored)
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)
	return resumed{src: src, snap: snap, work: work, dest: dest, args: args, manifest: m}
}

// testSIGTERMResume stops a send with SIGTERM while it uploads a volume
// (--maxUploadSpeed makes each take seconds) and resumes it.
func testSIGTERMResume(t *testing.T, pool, name string, mk destMaker) {
	t.Helper()
	src := pool + "/" + name + "src"
	newSource(t, src, 6<<20)
	snap := snapshot(t, src, "daily")
	dest := mk(t, pool, name+"dest")
	work := t.TempDir()
	args := append(slices.Clone(plainSend), append(slices.Clone(sshRetry), src+"@"+snap, dest.target)...)

	// One upload at a time at 512 kB/s: about 2 s a volume. The signal goes
	// once volume 1 is cached, so it lands in the upload of a later one.
	logs, err := sendThenSIGTERM(t, work, func() bool { return cachedVolumes(work, snap) >= 1 },
		append([]string{"--maxParallelUploads", "1", "--maxUploadSpeed", "512"}, args...)...)
	t.Logf("send: %v", err)
	partial := cachedPartial(t, work, snap)
	kept := len(partial.Volumes)
	if short, missing := shortVolumes(t, dest.dir, partial); len(short)+len(missing) > 0 {
		t.Fatalf("after the SIGTERM the cached volumes %v are short and %v missing at %s:\n%s", short, missing, dest.dir, logs)
	}
	// What the cache does not list: the volume in flight, whole, cut or gone.
	for _, n := range fileNames(t, dest.dir) {
		if !slices.Contains(volumeNames(partial), n) {
			fi, serr := os.Stat(filepath.Join(dest.dir, n))
			if serr != nil {
				t.Fatal(serr)
			}
			t.Logf("after the SIGTERM %s holds %s (%d bytes), which the cache does not list", dest.dir, n, fi.Size())
		}
	}

	_, logs, err = itRunOut(cmd.ResetSendJobInfo, "send", work, "zfs", append([]string{"--resume"}, args...)...)
	if err != nil {
		t.Fatalf("send --resume: %v\n%s", err, logs)
	}
	wantInLog(t, logs, fmt.Sprintf("Resuming from volume %d: %d of %d", kept+1, kept, kept))
	m := manifestFor(t, work, src, dest.target, snap)
	wantVolumeSizes(t, dest.dir, m)
	wantFilesAt(t, dest.dir, append(volumeNames(m), manifestFiles(fileNames(t, dest.dir))...))

	restored := pool + "/" + name + "Restored"
	destroyAfter(t, restored)
	itReceive(t, t.TempDir(), "-F", src+"@"+snap, dest.target, restored)
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)
}
