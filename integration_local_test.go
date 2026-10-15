//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bosgood/zfsbackup-go/cmd"
	"github.com/bosgood/zfsbackup-go/config"
	"github.com/bosgood/zfsbackup-go/files"
)

// Helpers of the file:// and ssh:// integration tests. integration/sshd.sh
// starts sshd in the test container and exports ZFSBACKUP_IT_SSH_*.

// sshdLog is where integration/sshd.sh has sshd log (LogLevel VERBOSE).
const sshdLog = "/var/tmp/sshd.log"

// sshRetry ends a failing ssh send in seconds: the last --maxRetryTime wins
// over the 2m of itRunCtx.
var sshRetry = []string{"--maxRetryTime", "10s"}

// itSSHEnv returns the sshd that integration/sshd.sh started: its host and
// port, the directory of the extra keys and pwonly's password. It skips the
// test outside the container.
func itSSHEnv(t *testing.T) (host, port, keys, password string) {
	t.Helper()
	port = os.Getenv("ZFSBACKUP_IT_SSH_PORT")
	if port == "" {
		t.Skip("no sshd (ZFSBACKUP_IT_SSH_PORT is unset); run through make integration-docker")
	}
	return os.Getenv("ZFSBACKUP_IT_SSH_HOST"), port, os.Getenv("ZFSBACKUP_IT_SSH_KEYS"), os.Getenv("ZFSBACKUP_IT_SSH_PASSWORD")
}

// itSSHClearEnv sets every SSH_* variable the ssh backend reads to empty: the
// backend then uses /root/.ssh/id_ed25519 and /root/.ssh/known_hosts. A test
// sets the ones it wants after this.
func itSSHClearEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"SSH_AUTH_SOCK", "SSH_KEY_FILE", "SSH_KNOWN_HOSTS", "SSH_USERNAME", "SSH_PASSWORD"} {
		t.Setenv(v, "")
	}
}

// itDestDataset makes the dataset <pool>/<name>, mounted at /<pool>/<name>,
// with props ("refquota=8M") set, and destroys it when the test ends. It
// returns the dataset and its directory.
func itDestDataset(t *testing.T, pool, name string, props ...string) (dataset, dir string) {
	t.Helper()
	return itDestDatasetAt(t, pool, name, "/"+pool+"/"+name, props...)
}

// itDestDatasetAt is itDestDataset with the dataset mounted at dir.
func itDestDatasetAt(t *testing.T, pool, name, dir string, props ...string) (dataset, mountpoint string) {
	t.Helper()
	dataset = pool + "/" + name
	destroyAfter(t, dataset)
	args := []string{"create", "-o", "mountpoint=" + dir}
	for _, p := range props {
		args = append(args, "-o", p)
	}
	zfsRun(t, append(args, dataset)...)
	return dataset, dir
}

// itFileTarget is the file:// destination of dir.
func itFileTarget(dir string) string {
	return "file://" + dir
}

// itSSHTarget gives dir (and what is in it) to username and returns the
// ssh:// destination of dir as that user at the test sshd.
func itSSHTarget(t *testing.T, username, dir string) string {
	t.Helper()
	host, port, _, _ := itSSHEnv(t)
	chownTree(t, username, dir)
	return "ssh://" + username + "@" + host + ":" + port + dir
}

// itSSHTargetPassword is itSSHTarget with the password in the URI.
func itSSHTargetPassword(t *testing.T, username, password, dir string) string {
	t.Helper()
	host, port, _, _ := itSSHEnv(t)
	chownTree(t, username, dir)
	return "ssh://" + username + ":" + password + "@" + host + ":" + port + dir
}

// chownTree gives dir and everything under it to username.
func chownTree(t *testing.T, username, dir string) {
	t.Helper()
	u, err := user.Lookup(username)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	err = filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fileNames returns the regular files under dir, relative to it, sorted: the
// objects of a file:// or ssh:// destination, as objectNames gives them for S3.
func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			names = append(names, strings.TrimPrefix(path, dir+"/"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

// wantFilesAt fails the test unless the regular files under dir are want.
func wantFilesAt(t *testing.T, dir string, want []string) {
	t.Helper()
	want = slices.Sorted(slices.Values(want))
	if got := fileNames(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files under %s:\n%s\nwant:\n%s", dir, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// manifestFiles returns the names in names that are manifests.
func manifestFiles(names []string) []string {
	var m []string
	for _, n := range names {
		if strings.Contains(n, ".manifest") {
			m = append(m, n)
		}
	}
	return m
}

// volumeNames returns the object names of the volumes m lists.
func volumeNames(m *files.JobInfo) []string {
	names := make([]string, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		names = append(names, v.ObjectName)
	}
	return names
}

// wantVolumeSizes fails the test unless every volume m lists is a file under
// dir with the size m records.
func wantVolumeSizes(t *testing.T, dir string, m *files.JobInfo) {
	t.Helper()
	for _, v := range m.Volumes {
		fi, err := os.Stat(filepath.Join(dir, v.ObjectName))
		if err != nil {
			t.Fatal(err)
		}
		if uint64(fi.Size()) != v.Size {
			t.Errorf("%s under %s is %d bytes, the manifest says %d", v.ObjectName, dir, fi.Size(), v.Size)
		}
	}
}

// truncateFile cuts the file at path to size bytes.
func truncateFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// copyFile copies the file at from to to.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sshdLogSize returns the size of sshd's log now: sshdLogHas reads from there.
func sshdLogSize(t *testing.T) int64 {
	t.Helper()
	fi, err := os.Stat(sshdLog)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// sshdLogHas reports whether sshd logged want after offset (from sshdLogSize).
// Each subtest reads from its own offset: other logins log the same lines.
func sshdLogHas(t *testing.T, offset int64, want string) bool {
	t.Helper()
	data, err := os.ReadFile(sshdLog)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(data[min(offset, int64(len(data))):], []byte(want))
}

// sshdLogSince returns what sshd logged after offset, for a failure message.
func sshdLogSince(t *testing.T, offset int64) string {
	t.Helper()
	data, err := os.ReadFile(sshdLog)
	if err != nil {
		t.Fatal(err)
	}
	return string(data[min(offset, int64(len(data))):])
}

// keyFingerprint returns the SHA256 fingerprint of the public half of the
// private key at path (its .pub file), as sshd logs it.
func keyFingerprint(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data) //nolint:dogsled // only the key
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(pub)
}

// cachedVolumes returns the volumes of snap that the cached manifests in work
// list, or 0 while a cache file is being written. It never fails the test: it
// polls while a send runs.
func cachedVolumes(work, snap string) int {
	paths, _ := filepath.Glob(filepath.Join(work, "cache", "*", "*"))
	n := 0
	for _, path := range paths {
		vol, err := files.ExtractLocal(context.Background(), &files.JobInfo{}, path, true)
		if err != nil {
			continue
		}
		m := new(files.JobInfo)
		err = json.NewDecoder(vol).Decode(m)
		_ = vol.Close()
		if err == nil && m.BaseSnapshot.Name == snap {
			n = max(n, len(m.Volumes))
		}
	}
	return n
}

// cachedPartial returns the cached manifest of snap in work: after a failed
// send, the partial one. It fails the test if there is none.
func cachedPartial(t *testing.T, work, snap string) *files.JobInfo {
	t.Helper()
	for _, m := range (&e2eEnv{work: work}).cachedManifests(t) {
		if m.BaseSnapshot.Name == snap {
			return m
		}
	}
	t.Fatalf("no cached manifest of %s in %s", snap, work)
	return nil
}

// cutSendTo sends with args (ending in src@snap and target) through a zfs that
// cuts the stream after 2.5 MiB, as cutSend does for S3, and returns the
// volumes the cached partial manifest lists. dir is the directory of target.
// It fails unless the send failed by the cut, left wantVolumes volumes and dir
// got exactly those.
func cutSendTo(t *testing.T, work, snap, dir string, wantVolumes int, args ...string) []string {
	t.Helper()
	flaky, arm := flakyZFS(t, 5<<19, 5)
	before := fileNames(t, dir)
	arm()
	_, logs, err := itRunOut(cmd.ResetSendJobInfo, "send", work, flaky, args...)
	if err == nil || !strings.Contains(logs, "flaky zfs") {
		t.Fatalf("send through the flaky zfs: got %v, want its failure in the log:\n%s", err, logs)
	}
	partial := cutPartial(t, work, snap, before, fileNames(t, dir), logs)
	if len(partial) != wantVolumes {
		t.Fatalf("after the cut the cached partial manifest lists %v, want %d volumes:\n%s", partial, wantVolumes, logs)
	}
	return partial
}

// sendThenSIGTERM runs send with args in work, waits until ready reports true
// (polled every 50 ms, for 2 minutes at most), sends this process one SIGTERM
// and returns the send's logs and error. It fails the test if the send ends
// before ready. A --maxUploadSpeed in args sets a global that nothing resets:
// it is restored when the send returns.
func sendThenSIGTERM(t *testing.T, work string, ready func() bool, args ...string) (string, error) {
	t.Helper()
	oldBucket := config.BackupUploadBucket
	t.Cleanup(func() { config.BackupUploadBucket = oldBucket })

	ctx, stop := cmd.SignalContext(context.Background())
	defer stop()
	type result struct {
		logs string
		err  error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, logs, err := itRunCtx(ctx, cmd.ResetSendJobInfo, "send", work, "zfs", args...)
		done <- result{logs, err}
	}()
	t.Cleanup(func() {
		stop()
		<-finished
	})

	for deadline := time.Now().Add(2 * time.Minute); !ready(); time.Sleep(50 * time.Millisecond) {
		select {
		case r := <-done:
			t.Fatalf("send returned before the test could signal it (%v); slow the upload more:\n%s", r.err, r.logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the send did not get ready to be signaled in 2 minutes")
		}
	}
	// One signal only: a second one exits the test binary.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r := <-done
	// At once, not only in t.Cleanup: the caller's next send must not upload slowly.
	config.BackupUploadBucket = oldBucket
	if r.err == nil {
		t.Fatalf("send stopped by SIGTERM returned no error:\n%s", r.logs)
	}
	wantInLog(t, r.logs, "Received SIGTERM; stopping.")
	return r.logs, r.err
}

// shortVolumes returns the volumes of m whose file under dir is shorter than m
// says, and those m lists that are not there.
func shortVolumes(t *testing.T, dir string, m *files.JobInfo) (short, missing []string) {
	t.Helper()
	for _, v := range m.Volumes {
		fi, err := os.Stat(filepath.Join(dir, v.ObjectName))
		switch {
		case os.IsNotExist(err):
			missing = append(missing, v.ObjectName)
		case err != nil:
			t.Fatal(err)
		case uint64(fi.Size()) < v.Size:
			short = append(short, v.ObjectName)
		}
	}
	return short, missing
}
