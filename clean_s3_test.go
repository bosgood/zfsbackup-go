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

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// fakeS3 is just enough of a path-style S3 endpoint for `clean`: ListObjectsV2,
// HEAD/GET of an object, and DELETE (recorded, so tests can assert nothing was deleted).
type fakeS3 struct {
	bucket string

	mu       sync.Mutex
	objects  map[string][]byte
	listed   []string
	deleted  []string
	requests []string
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
	case r.Method == http.MethodDelete:
		f.deleted = append(f.deleted, key)
		w.WriteHeader(http.StatusNoContent)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
	default:
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	f.listed = append(f.listed, prefix)
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
	cmd.ResetReceiveJobInfo()
	var logs bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	cmd.RootCmd.SetArgs(append([]string{"clean", "--workingDirectory", work}, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetReceiveJobInfo()
	}()
	err := cmd.RootCmd.ExecuteContext(context.Background())
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
			logText: "no manifest at this destination is for dataset p/tank/data",
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
