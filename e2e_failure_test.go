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
	"bytes"
	"context"
	"crypto/md5" // nolint:gosec // matches the lock file name, not for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/internal/fakezfs"
	"github.com/someone1/zfsbackup-go/log"
)

// The failure-path tests: a send whose uploads or zfs stream fail must exit, and a
// --resume must not trust volumes it cannot see. See
// docs/specs/2026-09-29--destructive-ops-fixes.

const hangTimeout = 30 * time.Second

// backupGoroutines dumps the goroutines that are inside the backup package.
func backupGoroutines() string {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	var keep []string
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "zfsbackup-go/backup.") {
			lines := strings.Split(g, "\n")
			if len(lines) > 12 {
				lines = lines[:12]
			}
			keep = append(keep, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(keep, "\n\n")
}

// guarded runs fn and fails the test, with a stack dump, if it does not return within hangTimeout.
func guarded(t *testing.T, fn func() (string, error)) (string, error) {
	t.Helper()
	type result struct {
		logs string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		logs, err := fn()
		done <- result{logs, err}
	}()
	select {
	case r := <-done:
		return r.logs, r.err
	case <-time.After(hangTimeout):
		t.Fatalf("HANG: still running after %v. Goroutines:\n%s", hangTimeout, backupGoroutines())
		return "", nil
	}
}

// receive runs `zfsbackup receive` in-process and returns what it logged.
func (env *e2eEnv) receive(args ...string) (string, error) {
	cmd.ResetReceiveJobInfo()
	var logs bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	base := []string{"receive", "--zfsPath", env.self, "--workingDirectory", env.work}
	cmd.RootCmd.SetArgs(append(base, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetReceiveJobInfo()
	}()
	err := cmd.RootCmd.ExecuteContext(context.Background())
	return logs.String(), err
}

// lockFile is where send locks volume.
func (env *e2eEnv) lockFile(volume string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes
	return filepath.Join(env.work, "locks", fmt.Sprintf("%x.lck", md5.Sum([]byte(volume))))
}

// destObjects returns every object (path relative to dir) under a file:// destination.
func destObjects(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	objects := make(map[string][]byte)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rerr := ioutil.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		objects[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

func manifestNames(objects map[string][]byte) []string {
	var names []string
	for name := range objects {
		if strings.HasPrefix(name, "manifests|") {
			names = append(names, name)
		}
	}
	return names
}

// cachedManifests decodes every manifest in the working directory's cache.
func (env *e2eEnv) cachedManifests(t *testing.T) []*files.JobInfo {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(env.work, "cache", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var manifests []*files.JobInfo
	for _, path := range paths {
		vol, err := files.ExtractLocal(context.Background(), &files.JobInfo{}, path, true)
		if err != nil {
			t.Fatalf("reading cached manifest %s: %v", path, err)
		}
		m := new(files.JobInfo)
		err = json.NewDecoder(vol).Decode(m)
		_ = vol.Close()
		if err != nil {
			t.Fatalf("decoding cached manifest %s: %v", path, err)
		}
		manifests = append(manifests, m)
	}
	return manifests
}

// copyDir copies the regular files under src to dst.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0700)
		}
		data, rerr := ioutil.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return ioutil.WriteFile(filepath.Join(dst, rel), data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkRestores receives the newest backup of tank/data@snap from dest and checks the fake
// zfs got exactly the stream it sent.
func (env *e2eEnv) checkRestores(t *testing.T, dest, snap string, streamBytes int64) {
	t.Helper()
	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	logs, err := guarded(t, func() (string, error) { return env.receive("tank/data@"+snap, dest, "restored/data") })
	if err != nil {
		t.Fatalf("receive: %v\n%s", err, logs)
	}
	got, err := ioutil.ReadFile(receiveLog)
	if err != nil {
		t.Fatalf("zfs receive was not run: %v\n%s", err, logs)
	}
	h := sha256.New()
	if _, err = io.Copy(h, fakezfs.Stream("", "tank/data@"+snap, streamBytes)); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d %s\n", streamBytes, hex.EncodeToString(h.Sum(nil))); string(got) != want {
		t.Errorf("zfs receive got %q, want the sent stream %q", got, want)
	}
}

func TestE2EUploadFailureExits(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	// A file where the data volumes' parent directory must go: every volume upload fails,
	// while manifests (manifests|...) could still be written.
	if err := ioutil.WriteFile(filepath.Join(env.dest, "tank"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := guarded(t, func() (string, error) {
		return env.send("--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Fatalf("send succeeded although no volume could be uploaded:\n%s", logs)
	}
	if _, serr := os.Stat(env.lockFile("tank/data")); !os.IsNotExist(serr) {
		t.Errorf("lock file %s left behind (stat: %v)", env.lockFile("tank/data"), serr)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("a failed send uploaded manifests %q", names)
	}
}

func TestE2EZFSSendFailureExits(t *testing.T) {
	const (
		streamBytes = 4 << 20
		failAfter   = 5 << 19 // 2.5 MiB: two complete 1 MiB volumes, then the stream dies
	)
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
	t.Setenv("FAKEZFS_FAIL_AFTER_BYTES", fmt.Sprint(failAfter))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	dest := "file://" + env.dest
	args := []string{"--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", dest}

	logs, err := guarded(t, func() (string, error) { return env.send(args...) })
	if err == nil {
		t.Fatalf("send succeeded although zfs send failed:\n%s", logs)
	}
	if !strings.Contains(logs, "cannot send: I/O error") {
		t.Errorf("log lacks zfs send's stderr:\n%s", logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("a failed send uploaded manifests %q", names)
	}
	if _, serr := os.Stat(env.lockFile("tank/data")); !os.IsNotExist(serr) {
		t.Errorf("lock file %s left behind (stat: %v)", env.lockFile("tank/data"), serr)
	}
	// The cached partial manifest may only list volumes cut before the stream died: all of
	// them full-size (uncompressed, a volume is cut at 1 MiB - 50 KiB), none the truncated tail.
	for _, m := range env.cachedManifests(t) {
		if streamed, _ := m.TotalBytesStreamedAndVols(); streamed > failAfter {
			t.Errorf("cached manifest lists %d stream bytes, but zfs send only wrote %d", streamed, failAfter)
		}
		for _, v := range m.Volumes {
			if v.ZFSStreamBytes < 1<<20-50<<10 {
				t.Errorf("cached manifest lists %s with only %d stream bytes: the truncated tail", v.ObjectName, v.ZFSStreamBytes)
			}
		}
	}

	// Resume with a healthy zfs: the result restores to exactly the sent stream.
	os.Unsetenv("FAKEZFS_FAIL_AFTER_BYTES")
	if logs, err = guarded(t, func() (string, error) { return env.send(append([]string{"--resume"}, args...)...) }); err != nil {
		t.Fatalf("resume: %v\n%s", err, logs)
	}
	env.checkRestores(t, dest, "a", streamBytes)
}

// With --maxFileBuffer 0 each volume is a pipe between the zfs stream splitter and the uploader:
// whichever side fails, the other must not wait on the pipe forever.
func TestE2EStreamingUploadFailureExits(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	if err := ioutil.WriteFile(filepath.Join(env.dest, "tank"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := guarded(t, func() (string, error) {
		return env.send("--maxFileBuffer", "0", "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Fatalf("send succeeded although no volume could be uploaded:\n%s", logs)
	}
	if _, serr := os.Stat(env.lockFile("tank/data")); !os.IsNotExist(serr) {
		t.Errorf("lock file %s left behind (stat: %v)", env.lockFile("tank/data"), serr)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("a failed send uploaded manifests %q", names)
	}
}

func TestE2EStreamingZFSSendFailureExits(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(4<<20))
	t.Setenv("FAKEZFS_FAIL_AFTER_BYTES", fmt.Sprint(5<<19))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	logs, err := guarded(t, func() (string, error) {
		return env.send(
			"--maxFileBuffer", "0", "--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s",
			"tank/data@a", "file://"+env.dest,
		)
	})
	if err == nil {
		t.Fatalf("send succeeded although zfs send failed:\n%s", logs)
	}
	if _, serr := os.Stat(env.lockFile("tank/data")); !os.IsNotExist(serr) {
		t.Errorf("lock file %s left behind (stat: %v)", env.lockFile("tank/data"), serr)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("a failed send uploaded manifests %q", names)
	}
}

// A manifest upload that fails at one of two destinations leaves a complete set at the first and
// volumes without a manifest at the second. A plain retry must refuse (it would overwrite the
// first), and --resume must complete the second with the first's manifest.
func TestE2EResumeCompletesManifestAtOneDestination(t *testing.T) {
	env := newE2EEnv(t)
	dest2 := filepath.Join(t.TempDir(), "dest2")
	if err := os.Mkdir(dest2, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	// A file where dest2's manifest directory must go: only the manifest upload fails there.
	blocker := filepath.Join(dest2, "manifests|tank")
	if err := ioutil.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--volsize", "1", "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://" + env.dest + ",file://" + dest2,
	}
	if logs, err := guarded(t, func() (string, error) { return env.send(args...) }); err == nil {
		t.Fatalf("send succeeded although the manifest upload to dest2 failed:\n%s", logs)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	before1, before2 := destObjects(t, env.dest), destObjects(t, dest2)
	names := manifestNames(before1)
	if len(names) != 1 || len(manifestNames(before2)) != 0 {
		t.Fatalf("got manifests %q at dest1 and %q at dest2, want one at dest1 only", names, manifestNames(before2))
	}

	logs, err := guarded(t, func() (string, error) { return env.send(args...) })
	if err == nil || !strings.Contains(logs, "--resume") {
		t.Errorf("plain retry: err=%v, want a refusal pointing at --resume:\n%s", err, logs)
	}
	// Smart mode, as a cron job runs it, re-plans the set and completes it.
	smart := append([]string{"--resume", "--fullIfOlderThan", "720h"}, args[:len(args)-2]...)
	smart = append(smart, "tank/data", args[len(args)-1])
	if logs, err = guarded(t, func() (string, error) { return env.send(smart...) }); err != nil {
		t.Fatalf("smart resume: %v\n%s", err, logs)
	}

	after1, after2 := destObjects(t, env.dest), destObjects(t, dest2)
	if len(after1) != len(before1) {
		t.Errorf("dest1 went from %d to %d objects", len(before1), len(after1))
	}
	for name, data := range before1 {
		if !bytes.Equal(after1[name], data) {
			t.Errorf("dest1's %s changed", name)
		}
	}
	for name, data := range before2 {
		if !bytes.Equal(after2[name], data) {
			t.Errorf("dest2's volume %s changed", name)
		}
	}
	if !bytes.Equal(after2[names[0]], before1[names[0]]) {
		t.Errorf("dest2's manifest %s is not dest1's", names[0])
	}
	env.checkRestores(t, "file://"+dest2, "a", 3<<20)

	// Complete everywhere now: another --resume refuses rather than re-sending.
	if logs, err = guarded(t, func() (string, error) { return env.send(append([]string{"--resume"}, args...)...) }); err == nil {
		t.Errorf("resume of a set complete at every destination succeeded:\n%s", logs)
	}
}

// interruptedSend leaves the state an interrupted send of tank/data@a leaves behind: volumes at
// dest and a cached manifest listing them, but no manifest at dest. (It sends to completion,
// then deletes the final manifest at dest; the cached copy lists every volume.)
func (env *e2eEnv) interruptedSend(t *testing.T, streamBytes int64, dest string, args ...string) {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	if logs, err := env.send(append(args, "tank/data@a", "file://"+dest)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	names := manifestNames(destObjects(t, dest))
	if len(names) != 1 {
		t.Fatalf("send left manifests %q at the destination, want one", names)
	}
	if err := os.Remove(filepath.Join(dest, names[0])); err != nil {
		t.Fatal(err)
	}
}

func TestE2EResumeReSendsMissingVolumes(t *testing.T) {
	const streamBytes = 4 << 20
	env := newE2EEnv(t)
	args := []string{"--volsize", "1", "--compressor", ""}
	env.interruptedSend(t, streamBytes, env.dest, args...)
	cached := env.cachedManifests(t)
	if len(cached) != 1 || len(cached[0].Volumes) < 3 {
		t.Fatalf("want one cached manifest with at least 3 volumes, got %d manifests", len(cached))
	}
	// Another host's clean took vol2 as an orphan (no manifest at the destination yet).
	vol2 := cached[0].Volumes[1].ObjectName
	if err := os.Remove(filepath.Join(env.dest, vol2)); err != nil {
		t.Fatal(err)
	}

	dest := "file://" + env.dest
	logs, err := guarded(t, func() (string, error) { return env.send(append(args, "--resume", "tank/data@a", dest)...) })
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, logs)
	}
	if want := "Volume " + vol2 + " missing at " + dest; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
	if want := "Resuming from volume 2: 1 of"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
	objects := destObjects(t, env.dest)
	for _, v := range newestBackup(t, "tank/data", dest).Volumes {
		if data, ok := objects[v.ObjectName]; !ok || uint64(len(data)) != v.Size {
			t.Errorf("manifest lists %s (%d bytes), the destination has %d bytes (present: %v)", v.ObjectName, v.Size, len(data), ok)
		}
	}
	env.checkRestores(t, dest, "a", streamBytes)
}

// A send killed mid-upload to a file:// or ssh:// destination leaves a truncated volume under
// its final name: the resume must re-send it, not vouch for it in the final manifest.
func TestE2EResumeReSendsTruncatedVolume(t *testing.T) {
	const streamBytes = 4 << 20
	env := newE2EEnv(t)
	args := []string{"--volsize", "1", "--compressor", ""}
	env.interruptedSend(t, streamBytes, env.dest, args...)
	cached := env.cachedManifests(t)
	if len(cached) != 1 || len(cached[0].Volumes) < 3 {
		t.Fatalf("want one cached manifest with at least 3 volumes, got %d manifests", len(cached))
	}
	vol2 := cached[0].Volumes[1]
	if err := os.Truncate(filepath.Join(env.dest, vol2.ObjectName), int64(vol2.Size/2)); err != nil {
		t.Fatal(err)
	}

	dest := "file://" + env.dest
	logs, err := guarded(t, func() (string, error) { return env.send(append(args, "--resume", "tank/data@a", dest)...) })
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, logs)
	}
	for _, want := range []string{
		fmt.Sprintf("Volume %s at %s is %d bytes, want %d", vol2.ObjectName, dest, vol2.Size/2, vol2.Size),
		"Resuming from volume 2: 1 of",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	env.checkRestores(t, dest, "a", streamBytes)
}

// A destination added since the interrupted attempt has none of its volumes, so the resume
// starts over, and the new destination gets the whole set.
func TestE2EResumeWithNewDestinationStartsOver(t *testing.T) {
	const streamBytes = 3 << 20
	env := newE2EEnv(t)
	env.interruptedSend(t, streamBytes, env.dest, "--volsize", "1")
	other := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}

	logs, err := guarded(t, func() (string, error) {
		return env.send("--volsize", "1", "--resume", "tank/data@a", "file://"+env.dest+",file://"+other)
	})
	if err != nil {
		t.Fatalf("resume with an added destination: %v\n%s", err, logs)
	}
	for _, want := range []string{"missing at file://" + other, "Nothing verifiable to resume; starting over."} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	env.checkRestores(t, "file://"+other, "a", streamBytes)
	env.checkRestores(t, "file://"+env.dest, "a", streamBytes)
}

func TestE2EResumeShortStreamFails(t *testing.T) {
	env := newE2EEnv(t)
	env.interruptedSend(t, 3<<20, env.dest, "--volsize", "1")
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(1<<20)) // shorter than what the cache says was sent

	logs, err := guarded(t, func() (string, error) {
		return env.send("--volsize", "1", "--resume", "tank/data@a", "file://"+env.dest)
	})
	if err == nil || !strings.Contains(logs, "zfs stream ended before") {
		t.Errorf("resume against a short stream: got %v, want the short-stream error\n%s", err, logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("the failed resume uploaded manifests %q", names)
	}
}

// TestE2EFailedSendLeavesNoWaiter: a failed send must not leave a goroutine parked waiting for
// volumes that will never finish. Harmless for one CLI run, but in-process runs add up.
func TestE2EFailedSendLeavesNoWaiter(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	if err := ioutil.WriteFile(filepath.Join(env.dest, "tank"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := guarded(t, func() (string, error) {
		return env.send("--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Fatalf("send succeeded although no volume could be uploaded:\n%s", logs)
	}
	parked := func() string {
		for _, g := range strings.Split(backupGoroutines(), "\n\n") {
			if strings.Contains(g, "backup.Backup.") && strings.Contains(g, "sync.(*WaitGroup).Wait") {
				return g
			}
		}
		return ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for parked() != "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if g := parked(); g != "" {
		t.Errorf("a failed send left a goroutine waiting on the volume counter:\n%s", g)
	}
}

// A stream that fails must never be followed by a final manifest, however long sendStream takes
// to return its error after the splitter gave up (the hook widens that window).
func TestE2EFailedStreamPublishesNoManifest(t *testing.T) {
	backup.TestHookStreamFailed = func() { time.Sleep(200 * time.Millisecond) }
	defer func() { backup.TestHookStreamFailed = nil }()
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(4<<20))
	t.Setenv("FAKEZFS_FAIL_AFTER_BYTES", fmt.Sprint(5<<19))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	logs, err := guarded(t, func() (string, error) {
		return env.send("--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Errorf("send succeeded although zfs send failed:\n%s", logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("a failed send published manifests %q", names)
	}
	for _, m := range env.cachedManifests(t) {
		if !m.EndTime.IsZero() {
			t.Errorf("a failed send finalized its cached manifest (%d volumes)", len(m.Volumes))
		}
	}
}

// The final manifest records the stream's length, however late sendStream records it.
func TestSendRecordsStreamBytes(t *testing.T) {
	const streamBytes = 3 << 20
	backup.TestHookBeforeStreamBytes = func() { time.Sleep(200 * time.Millisecond) }
	defer func() { backup.TestHookBeforeStreamBytes = nil }()
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	dest := "file://" + env.dest
	if logs, err := guarded(t, func() (string, error) { return env.send("--volsize", "1", "tank/data@a", dest) }); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	if got := newestBackup(t, "tank/data", dest).ZFSStreamBytes; got != streamBytes {
		t.Errorf("manifest records ZFSStreamBytes=%d, want %d", got, streamBytes)
	}
}
