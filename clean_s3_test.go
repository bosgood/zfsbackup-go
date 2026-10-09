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
	"compress/gzip"
	"context"
	"crypto/md5" // nolint:gosec // MD5 not used for cryptographic purposes here
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// fakeS3 is just enough of a path-style S3 endpoint for `clean`, `send` and `receive`:
// ListObjectsV2, HEAD/GET/PUT of an object, DELETE (recorded, so tests can assert nothing
// was deleted), and archive storage classes: an object in storageClass GLACIER or
// DEEP_ARCHIVE answers GET with InvalidObjectState until a restore (POST ?restore) was
// seen for it, after which HEAD reports the restore done. An INTELLIGENT_TIERING object with
// an archiveStatus is cold the same way, and refuses a restore request that names Days.
type fakeS3 struct {
	bucket string

	mu       sync.Mutex
	objects  map[string][]byte
	listed   []string
	deleted  []string
	requests []string

	storageClass map[string]string // key -> x-amz-storage-class, when not STANDARD
	// archiveStatus is key -> x-amz-archive-status (ARCHIVE_ACCESS, DEEP_ARCHIVE_ACCESS) of an
	// INTELLIGENT_TIERING object in an archive tier.
	archiveStatus map[string]string
	restores      []string // keys a restore was requested for, in order
	restored      map[string]bool
	// headsWithoutRestoreHeader is how many HEADs of a restored object omit
	// x-amz-restore before it appears (S3 is eventually consistent here).
	headsWithoutRestoreHeader int
	headsSeen                 map[string]int

	// onList, when set, runs (with mu held, so it may change objects) before each listing
	// that returns keys: not Init's max-keys=0 probe.
	onList func(prefix string)
	// onDelete, when set, runs (with mu held) before each DELETE; false fails it with a 500.
	onDelete func(key string) bool
}

// cold returns whether key is in an archive storage class that has not been restored.
func (f *fakeS3) cold(key string) bool {
	class := f.storageClass[key]
	return (class == "GLACIER" || class == "DEEP_ARCHIVE" || f.archiveStatus[key] != "") && !f.restored[key]
}

func newFakeS3(t *testing.T, objects map[string][]byte) *fakeS3 {
	t.Helper()
	f := &fakeS3{bucket: "bucket", objects: objects}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_S3_CUSTOM_ENDPOINT", srv.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	return f
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.String())
	if r.URL.Path == "/"+f.bucket || r.URL.Path == "/"+f.bucket+"/" {
		f.list(w, r)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+f.bucket+"/")
	data, ok := f.objects[key]
	switch {
	case r.Method == http.MethodPut:
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		if f.onDelete != nil && !f.onDelete(key) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.deleted = append(f.deleted, key)
		w.WriteHeader(http.StatusNoContent)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && r.URL.Query().Has("restore"):
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if f.archiveStatus[key] != "" && strings.Contains(string(body), "<Days>") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidArgument</Code>` +
				`<Message>Days is not allowed for an object in an Intelligent-Tiering archive tier</Message></Error>`))
			return
		}
		f.restores = append(f.restores, key)
		if f.restored == nil {
			f.restored = make(map[string]bool)
		}
		f.restored[key] = true
		w.WriteHeader(http.StatusAccepted)
	default:
		if class := f.storageClass[key]; class != "" {
			w.Header().Set("x-amz-storage-class", class)
		}
		if status := f.archiveStatus[key]; status != "" && !f.restored[key] {
			w.Header().Set("x-amz-archive-status", status)
		}
		if f.restored[key] {
			if f.headsSeen == nil {
				f.headsSeen = make(map[string]int)
			}
			f.headsSeen[key]++
			if f.headsSeen[key] > f.headsWithoutRestoreHeader {
				w.Header().Set("x-amz-restore", `ongoing-request="false", expiry-date="Fri, 21 Dec 2029 00:00:00 GMT"`)
			}
		}
		if r.Method == http.MethodGet && f.cold(key) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidObjectState</Code>` +
				`<Message>The operation is not valid for the object's storage class</Message></Error>`))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	f.listed = append(f.listed, prefix)
	if f.onList != nil && r.URL.Query().Get("max-keys") != "0" {
		f.onList(prefix)
	}
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if r.URL.Query().Get("max-keys") == "0" {
		keys = nil
	}
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.objects[k]))
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>%s</Name><Prefix>%s</Prefix>`+
		`<KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>`,
		f.bucket, prefix, len(keys), b.String())
}

// backupSet returns the objects of a one-volume full backup of tank/data under prefix, keyed by name:
// a real gzip'd manifest and its volume.
func backupSet(t *testing.T, prefix, volume string) map[string][]byte {
	t.Helper()
	return backupSetAt(t, prefix, volume, "autosnap_2026-09-01_00:00:00_monthly")
}

// backupSetAt is backupSet of a full backup of snapshot.
func backupSetAt(t *testing.T, prefix, volume, snapshot string) map[string][]byte {
	t.Helper()
	// The manifest is built in config.BackupTempdir, which an earlier in-process run may have
	// pointed at a directory it has since removed.
	oldTempdir := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	defer func() { config.BackupTempdir = oldTempdir }()
	j := &files.JobInfo{
		VolumeName:     volume,
		BaseSnapshot:   files.SnapshotInfo{Name: snapshot},
		ManifestPrefix: "manifests",
		Separator:      "|",
		Compressor:     files.InternalCompressor,
	}
	vol := j.BackupVolumeObjectName(1)
	j.Volumes = []*files.VolumeInfo{{ObjectName: vol, VolumeNumber: 1}}
	manifest, err := files.CreateManifestVolume(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.DeleteVolume()
	if err = json.NewEncoder(manifest).Encode(j); err != nil {
		t.Fatal(err)
	}
	if err = manifest.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest")
	if err = manifest.CopyTo(path); err != nil {
		t.Fatal(err)
	}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{prefix + manifest.ObjectName: data, prefix + vol: []byte("volume")}
}

func merge(sets ...map[string][]byte) map[string][]byte {
	all := make(map[string][]byte)
	for _, s := range sets {
		for k, v := range s {
			all[k] = v
		}
	}
	return all
}

// cleanS3 runs `clean` in-process against uri and returns its log and error.
func cleanS3(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return cleanS3In(t, t.TempDir(), args...)
}

// cleanS3In is cleanS3 with the working directory (and so the manifest cache) work.
func cleanS3In(t *testing.T, work string, args ...string) (string, error) {
	t.Helper()
	return cleanS3Ctx(context.Background(), t, work, args...)
}

// cleanS3Ctx is cleanS3In under ctx.
func cleanS3Ctx(ctx context.Context, t *testing.T, work string, args ...string) (string, error) {
	t.Helper()
	cmd.ResetReceiveJobInfo()
	var logs bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	cmd.RootCmd.SetArgs(append([]string{"clean", "--workingDirectory", work}, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetReceiveJobInfo()
	}()
	// cobra hands a subcommand the root's context only if it has none, so a context an earlier
	// run left on clean would win over ctx.
	cleanCmd, _, err := cmd.RootCmd.Find([]string{"clean"})
	if err != nil {
		t.Fatal(err)
	}
	cleanCmd.SetContext(ctx)
	err = cmd.RootCmd.ExecuteContext(ctx)
	return logs.String(), err
}

// TestCleanS3Prefixes reproduces the 2026-09-29 review's `clean` findings against a fake S3:
// object-store prefixes are directories, and clean deletes only volumes it can attribute to
// a manifest at exactly this destination.
func TestCleanS3Prefixes(t *testing.T) {
	testCases := []struct {
		name    string
		objects map[string][]byte
		args    []string
		errText string // "" = success
		logText string
	}{
		{
			// Written to s3://bucket/p/, cleaned as s3://bucket/p: used to list "/manifests|..."
			// and delete the manifest and every volume.
			name:    "forgotten trailing slash",
			objects: backupSet(t, "p/", "tank/data"),
			args:    []string{"s3://bucket/p"},
			logText: "would delete 0 objects",
		},
		{
			// s3://bucket/media used to match media-photos/.
			name:    "sibling prefix",
			objects: backupSet(t, "media-photos/", "tank/photos"),
			args:    []string{"s3://bucket/media"},
			logText: "would delete 0 objects",
		},
		{
			name:    "unrecognized object",
			objects: merge(backupSet(t, "p/", "tank/data"), map[string][]byte{"p/random.bin": []byte("x")}),
			args:    []string{"s3://bucket/p/"},
			logText: "Skipping unrecognized object random.bin",
		},
		{
			// Another destination's set nested under the bucket root, seen from the root.
			name:    "bucket root with a nested destination",
			objects: merge(backupSet(t, "", "tank/data"), backupSet(t, "p/", "tank/data")),
			args:    []string{"s3://bucket"},
			logText: "is another destination",
		},
		{
			name:    "bucket root holding only a nested destination",
			objects: backupSet(t, "p/", "tank/data"),
			args:    []string{"s3://bucket"},
			errText: "holds 2 objects but no manifests; refusing to clean",
		},
		{
			name:    "legacy layout",
			objects: backupSet(t, "p", "tank/data"),
			args:    []string{"s3://bucket/p/"},
			errText: "pmanifests|tank/data|",
		},
		{
			// s3://bucket//p used to write "/pmanifests|...", not "pmanifests|...".
			name:    "legacy layout behind a doubled slash",
			objects: backupSet(t, "/p", "tank/data"),
			args:    []string{"s3://bucket//p"},
			errText: "/pmanifests|tank/data|",
		},
		{
			// An old full to s3://bucket/p, interrupted before its manifest: "p" + volume name.
			name: "legacy volume without a manifest",
			objects: merge(backupSet(t, "p/", "tank/data"), map[string][]byte{
				"ptank/data|autosnap_2026-08-01_00:00:00_monthly.zstream.gz.vol1": []byte("volume"),
			}),
			args:    []string{"s3://bucket/p/"},
			logText: "looks like a backup volume written by an older version",
		},
		{
			// s3://bucket/tank/ backs up pool "data": its volume keys are "tank/data|...",
			// which from the bucket root parse as volumes of tank/data.
			name: "nested destination named like a dataset",
			objects: merge(
				backupSet(t, "", "tank/data"),
				backupSetAt(t, "tank/", "data", "autosnap_2026-10-01_00:00:00_monthly"),
			),
			args:    []string{"s3://bucket"},
			logText: "is another destination",
		},
		{
			// The same, with --force: the root's own volume "tank/data|..." is under tank/, so
			// its set used to look incomplete and --force deleted its manifest.
			name: "nested destination named like a dataset with --force",
			objects: merge(
				backupSet(t, "", "tank/data"),
				backupSetAt(t, "tank/", "data", "autosnap_2026-10-01_00:00:00_monthly"),
			),
			args:    []string{"--force=true", "s3://bucket"},
			logText: "is another destination",
		},
		{
			// s3://bucket/manifests-old/ is another destination; List("manifests") from the
			// bucket root returns its objects too, which used to be decoded as manifests.
			name: "sibling destination named manifests-something",
			objects: merge(
				backupSet(t, "", "tank/data"),
				backupSetAt(t, "manifests-old/", "tank/data", "autosnap_2026-10-01_00:00:00_monthly"),
			),
			args:    []string{"s3://bucket"},
			logText: "is another destination",
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			for _, dryRun := range []bool{true, false} {
				fake := newFakeS3(t, c.objects)
				// Explicit either way: flag values are package globals that outlive a run.
				args := append([]string{fmt.Sprintf("--dry-run=%v", dryRun)}, c.args...)
				logs, err := cleanS3(t, args...)
				switch {
				case c.errText == "" && err != nil:
					t.Errorf("clean %v: %v\n%s", args, err, logs)
				case c.errText != "" && (err == nil || !strings.Contains(logs, c.errText)):
					t.Errorf("clean %v: got %v, want an error logging %q\n%s", args, err, c.errText, logs)
				case c.logText != "" && dryRun && !strings.Contains(logs, c.logText):
					t.Errorf("clean %v: log lacks %q\n%s", args, c.logText, logs)
				}
				if strings.Contains(logs, "Would delete") || len(fake.deleted) != 0 {
					t.Errorf("clean %v deleted %q\n%s", args, fake.deleted, logs)
				}
				if strings.Contains(logs, "missing volume") {
					t.Errorf("clean %v reports an intact set as broken\n%s", args, logs)
				}
				for _, prefix := range fake.listed {
					// Besides Init's bucket probe ("") and legacy-layout check ("mediamanifests"),
					// every listing must stay inside media/.
					if c.name == "sibling prefix" && prefix != "" && prefix != "mediamanifests" && !strings.HasPrefix(prefix, "media/") {
						t.Errorf("clean of s3://bucket/media listed prefix %q", prefix)
					}
				}
			}
		})
	}
}

// TestCleanS3KeepsCacheAcrossCanonicalization: an interrupted send to s3://bucket/ left its
// volume at the destination and its partial manifest only in the cache, which the old version
// keyed by the URI as typed. clean must still find that manifest and keep the volume.
func TestCleanS3KeepsCacheAcrossCanonicalization(t *testing.T) {
	objects := backupSet(t, "", "tank/data")
	work := t.TempDir()
	// nolint:gosec // MD5 not used for cryptographic purposes here
	cacheDir := filepath.Join(work, "cache", fmt.Sprintf("%x", md5.Sum([]byte("s3://bucket/"))))
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range backupSetAt(t, "", "tank/data", "autosnap_2026-10-01_00:00:00_monthly") {
		if !strings.HasPrefix(name, "manifests|") {
			objects[name] = data
			continue
		}
		// nolint:gosec // MD5 not used for cryptographic purposes here
		cached := filepath.Join(cacheDir, fmt.Sprintf("%x", md5.Sum([]byte(name))))
		if err := ioutil.WriteFile(cached, data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, uri := range []string{"s3://bucket/", "s3://bucket"} {
		fake := newFakeS3(t, objects)
		logs, err := cleanS3In(t, work, "--dry-run=true", uri)
		if err != nil {
			t.Fatalf("clean %s: %v\n%s", uri, err, logs)
		}
		if strings.Contains(logs, "Would delete") || len(fake.deleted) != 0 {
			t.Errorf("clean %s would delete the interrupted send's volume\n%s", uri, logs)
		}
	}
}

// cachePathIn is where clean, run with working directory work, caches manifest objectName of
// destination uri.
func cachePathIn(t *testing.T, work, uri, objectName string) string {
	t.Helper()
	// nolint:gosec // MD5 not used for cryptographic purposes here
	dir := filepath.Join(work, "cache", fmt.Sprintf("%x", md5.Sum([]byte(uri))))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return filepath.Join(dir, fmt.Sprintf("%x", md5.Sum([]byte(objectName))))
}

// holdSendLock writes the send lock of dataset under work as held by the test runner (a pid this
// user can signal, so lockfile sees a live holder) and returns its path.
func holdSendLock(t *testing.T, work, dataset string) string {
	t.Helper()
	dir := filepath.Join(work, "locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// nolint:gosec // MD5 not used for cryptographic purposes here
	p := filepath.Join(dir, fmt.Sprintf("%x.lck", md5.Sum([]byte(dataset))))
	if err := ioutil.WriteFile(p, []byte(fmt.Sprintf("%d\n", os.Getppid())), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// splitSet returns the manifest and the volume of a backupSet.
func splitSet(set map[string][]byte) (manifest, volume string) {
	for k := range set {
		if strings.Contains(k, "manifests|") {
			manifest = k
		} else {
			volume = k
		}
	}
	return manifest, volume
}

// TestCleanS3SparesSendFinishingDuringClean: a send of tank/data holds its lock while clean
// starts, and finishes (last volume and manifest uploaded, manifest cached, lock released)
// while clean lists the bucket. clean then gets the lock; it must judge the volumes by what is
// at the destination after that, not by the manifests it read before.
func TestCleanS3SparesSendFinishingDuringClean(t *testing.T) {
	old := backupSet(t, "", "tank/data")
	newSet := backupSetAt(t, "", "tank/data", "autosnap_2026-10-01_00:00:00_monthly")
	newManifest, newVol := splitSet(newSet)
	for _, cleanLocal := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanLocal=%v", cleanLocal), func(t *testing.T) {
			work := t.TempDir()
			objects := merge(old)
			cached := cachePathIn(t, work, "s3://bucket", newManifest)
			if cleanLocal {
				// The send has finished volume 1: its partial manifest is cached, the volume is up.
				objects[newVol] = newSet[newVol]
				if err := ioutil.WriteFile(cached, newSet[newManifest], 0600); err != nil {
					t.Fatal(err)
				}
			}
			lock := holdSendLock(t, work, "tank/data")
			fake := newFakeS3(t, objects)
			finished := false
			fake.onList = func(prefix string) {
				if prefix != "" || finished {
					return
				}
				finished = true
				fake.objects[newVol] = newSet[newVol]
				fake.objects[newManifest] = newSet[newManifest]
				if err := ioutil.WriteFile(cached, newSet[newManifest], 0600); err != nil {
					t.Error(err)
				}
				if err := os.Remove(lock); err != nil {
					t.Error(err)
				}
			}
			args := []string{"--dry-run=false", fmt.Sprintf("--cleanLocal=%v", cleanLocal), "s3://bucket"}
			logs, err := cleanS3In(t, work, args...)
			if err != nil {
				t.Fatalf("clean %v: %v\n%s", args, err, logs)
			}
			if !finished {
				t.Fatalf("clean %v never listed the bucket\n%s", args, logs)
			}
			if len(fake.deleted) != 0 {
				t.Errorf("clean %v deleted %q, of a set whose manifest %s is at the destination\n%s", args, fake.deleted, newManifest, logs)
			}
			if _, serr := os.Stat(cached); serr != nil {
				t.Errorf("clean %v removed the finished set's cached manifest: %v\n%s", args, serr, logs)
			}
		})
	}
}

// TestCleanLocalKeepsManifestWhenDeleteFails: --cleanLocal removes a local-only manifest only
// after its volumes are deleted at the destination. Removed first, a failed delete would leave
// those volumes listed by nothing, out of every later clean's reach.
func TestCleanLocalKeepsManifestWhenDeleteFails(t *testing.T) {
	work := t.TempDir()
	objects := backupSet(t, "", "tank/data")
	partial := backupSetAt(t, "", "tank/data", "autosnap_2026-10-01_00:00:00_monthly")
	partialManifest, partialVol := splitSet(partial)
	objects[partialVol] = partial[partialVol]
	cached := cachePathIn(t, work, "s3://bucket", partialManifest)
	if err := ioutil.WriteFile(cached, partial[partialManifest], 0600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeS3(t, objects)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onDelete = func(string) bool {
		cancel() // or clean retries a failed delete for minutes
		return false
	}
	logs, err := cleanS3Ctx(ctx, t, work, "--dry-run=false", "--cleanLocal=true", "s3://bucket")
	if err == nil {
		t.Fatalf("clean succeeded although every delete failed\n%s", logs)
	}
	if _, serr := os.Stat(cached); serr != nil {
		t.Errorf("clean removed local manifest %s before deleting its volume: %v\n%s", cached, serr, logs)
	}
}

// TestCleanS3GivesUpOnChangingManifests: clean locks the datasets its read names and reads
// again; a destination that names a new dataset on every read never settles.
func TestCleanS3GivesUpOnChangingManifests(t *testing.T) {
	var sets []map[string][]byte
	for i := range 4 {
		sets = append(sets, backupSet(t, "", fmt.Sprintf("tank/d%d", i)))
	}
	fake := newFakeS3(t, merge(sets[0]))
	next := 1
	fake.onList = func(prefix string) {
		if prefix == "manifests" && next < len(sets) {
			for k, v := range sets[next] {
				fake.objects[k] = v
			}
			next++
		}
	}
	logs, err := cleanS3(t, "--dry-run=false", "s3://bucket")
	if err == nil || !strings.Contains(logs, "keep changing") {
		t.Errorf("clean: got %v, want it to give up on a destination that keeps changing\n%s", err, logs)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("clean deleted %q", fake.deleted)
	}
}

// TestCleanS3ForceDeletesBrokenSet: --force deletes a set missing a volume, manifest and cached
// copy included, and leaves the intact set alone.
func TestCleanS3ForceDeletesBrokenSet(t *testing.T) {
	work := t.TempDir()
	broken := backupSetAt(t, "", "tank/data", "autosnap_2026-10-01_00:00:00_monthly")
	brokenManifest, _ := splitSet(broken)
	fake := newFakeS3(t, merge(backupSet(t, "", "tank/data"), map[string][]byte{brokenManifest: broken[brokenManifest]}))
	logs, err := cleanS3In(t, work, "--dry-run=false", "--force=true", "s3://bucket")
	if err != nil {
		t.Fatalf("clean --force: %v\n%s", err, logs)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != brokenManifest {
		t.Errorf("clean --force deleted %q, want only %q\n%s", fake.deleted, brokenManifest, logs)
	}
	if _, serr := os.Stat(cachePathIn(t, work, "s3://bucket", brokenManifest)); !os.IsNotExist(serr) {
		t.Errorf("clean --force kept the broken set's cached manifest (%v)\n%s", serr, logs)
	}
}

// TestE2ECleanForceDeletesTheManifestItRead: --force deletes the manifest object it read, not
// a name computed from that manifest's (possibly forged) content.
func TestE2ECleanForceDeletesTheManifestItRead(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	env.sendOK(t, "--compressor", "", "tank/data@a", "file://"+env.dest)
	victim := filepath.Join(filepath.Dir(env.dest), "victim|x.manifest.gz")
	if err := ioutil.WriteFile(victim, []byte("another destination's manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	// A set missing its volume, whose name computes to ../victim|x.manifest.gz.
	forged := &files.JobInfo{
		VolumeName:     "a/../../victim",
		BaseSnapshot:   files.SnapshotInfo{Name: "x", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		Separator:      "|",
		ManifestPrefix: "manifests",
		Volumes:        []*files.VolumeInfo{{ObjectName: "nothing-here"}},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(forged); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(env.dest, "manifests|zz.manifest.gz"), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := guarded(t, func() (string, error) {
		return cleanS3In(t, env.work, "--dry-run=false", "--force=true", "file://"+env.dest)
	})
	if _, serr := os.Stat(victim); serr != nil {
		t.Errorf("clean --force deleted %s, outside the destination (clean: %v)\n%s", victim, err, logs)
	}
}
