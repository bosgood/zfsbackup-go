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

package backup

import (
	"crypto/md5" // nolint:gosec // MD5 not used for cryptographic purposes here
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

func cacheDirOf(uri string) string {
	// nolint:gosec // MD5 not used for cryptographic purposes here
	return filepath.Join(config.WorkingDir, "cache", fmt.Sprintf("%x", md5.Sum([]byte(uri))))
}

func writeCacheFile(t *testing.T, uri, name, content string) {
	t.Helper()
	if err := os.MkdirAll(cacheDirOf(uri), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDirOf(uri), name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Before URIs were canonicalized the cache was keyed by the URI as typed. Manifests cached
// under such a spelling (the resume state of an interrupted send, among others) must follow
// the destination to its canonical cache directory.
func TestGetCacheDirAdoptsLegacySpellings(t *testing.T) {
	testCases := []struct {
		name, canonical, typed, legacy string
	}{
		{"bucket root typed with a slash", "s3://b", "s3://b/", "s3://b/"},
		{"bucket root, slash typed only before the upgrade", "s3://b", "s3://b", "s3://b/"},
		{"prefix typed without a slash", "s3://b/p/", "s3://b/p", "s3://b/p"},
		{"doubled slash", "s3://b/p/", "s3://b//p", "s3://b//p"},
	}
	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			oldWorkingDir := config.WorkingDir
			config.WorkingDir = t.TempDir()
			t.Cleanup(func() { config.WorkingDir = oldWorkingDir })

			writeCacheFile(t, c.legacy, "only-legacy", "legacy")
			writeCacheFile(t, c.legacy, "both", "legacy")
			writeCacheFile(t, c.canonical, "both", "canonical")

			j := &files.JobInfo{DestinationsAsTyped: map[string]string{c.canonical: c.typed}}
			dir, err := getCacheDir(j, c.canonical)
			if err != nil {
				t.Fatal(err)
			}
			if dir != cacheDirOf(c.canonical) {
				t.Errorf("cache dir = %s, want %s", dir, cacheDirOf(c.canonical))
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "only-legacy")); string(got) != "legacy" {
				t.Errorf("legacy-only manifest was not adopted (content %q)", got)
			}
			// The canonical copy was synced from the destination or written since the upgrade.
			if got, _ := os.ReadFile(filepath.Join(dir, "both")); string(got) != "canonical" {
				t.Errorf("canonical manifest was replaced by the legacy copy (content %q)", got)
			}
			if _, serr := os.Stat(cacheDirOf(c.legacy)); !os.IsNotExist(serr) {
				t.Errorf("legacy cache dir still exists (stat: %v)", serr)
			}
			// Idempotent.
			if _, err = getCacheDir(j, c.canonical); err != nil {
				t.Errorf("second getCacheDir: %v", err)
			}
		})
	}
}
