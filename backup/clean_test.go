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
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// TestCleanDryRun verifies that running Clean with dryRun=true does not delete
// any objects from the destination, while a subsequent real run does. Clean
// resolves its backend from the destination URI, so this exercises the real
// FileBackend against a local directory rather than a mock.
func TestCleanDryRun(t *testing.T) {
	// Cache/scratch directory used by getCacheDir via config.WorkingDir.
	cacheDir, err := ioutil.TempDir("", "zfsbackup-clean-cache")
	if err != nil {
		t.Fatalf("could not create temp cache dir - %v", err)
	}
	defer os.RemoveAll(cacheDir)

	oldWorkingDir := config.WorkingDir
	config.WorkingDir = cacheDir
	defer func() { config.WorkingDir = oldWorkingDir }()

	// The file:// destination, seeded with a stray object that is not referenced
	// by any manifest - exactly what Clean is meant to delete.
	targetDir, err := ioutil.TempDir("", "zfsbackup-clean-target")
	if err != nil {
		t.Fatalf("could not create temp target dir - %v", err)
	}
	defer os.RemoveAll(targetDir)

	strayPath := filepath.Join(targetDir, "data-block-stray")
	if werr := ioutil.WriteFile(strayPath, []byte("payload"), 0600); werr != nil {
		t.Fatalf("could not write stray object - %v", werr)
	}

	jobInfo := &files.JobInfo{
		ManifestPrefix:     "manifests",
		Destinations:       []string{"file://" + targetDir},
		MaxParallelUploads: 1,
		MaxBackoffTime:     5 * time.Second,
		MaxRetryTime:       1 * time.Minute,
	}

	// Dry-run: the stray object must be left untouched.
	if cerr := Clean(context.Background(), jobInfo, false, true); cerr != nil {
		t.Fatalf("dry-run Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(strayPath); serr != nil {
		t.Fatalf("dry-run should not have deleted stray object %s, stat error - %v", strayPath, serr)
	}

	// Real run: the stray object must now be deleted.
	if cerr := Clean(context.Background(), jobInfo, false, false); cerr != nil {
		t.Fatalf("Clean returned error - %v", cerr)
	}
	if _, serr := os.Stat(strayPath); !os.IsNotExist(serr) {
		t.Fatalf("expected stray object %s to be deleted, got stat error - %v", strayPath, serr)
	}
}

// TestCleanDryRunLocalManifests covers the --cleanLocal branch: a dry run must
// leave local-only cached manifests alone, and a real run must delete them.
func TestCleanDryRunLocalManifests(t *testing.T) {
	cacheDir, err := ioutil.TempDir("", "zfsbackup-clean-cache")
	if err != nil {
		t.Fatalf("could not create temp cache dir - %v", err)
	}
	defer os.RemoveAll(cacheDir)

	oldWorkingDir := config.WorkingDir
	config.WorkingDir = cacheDir
	defer func() { config.WorkingDir = oldWorkingDir }()

	targetDir, err := ioutil.TempDir("", "zfsbackup-clean-target")
	if err != nil {
		t.Fatalf("could not create temp target dir - %v", err)
	}
	defer os.RemoveAll(targetDir)

	target := "file://" + targetDir
	jobInfo := &files.JobInfo{
		ManifestPrefix:     "manifests",
		Destinations:       []string{target},
		MaxParallelUploads: 1,
		MaxBackoffTime:     5 * time.Second,
		MaxRetryTime:       1 * time.Minute,
	}

	// A cached manifest with no counterpart in the destination is exactly what
	// --cleanLocal removes.
	localCachePath, cerr := getCacheDir(target)
	if cerr != nil {
		t.Fatalf("could not resolve cache dir - %v", cerr)
	}
	strayManifest := filepath.Join(localCachePath, "0123456789abcdef")
	if werr := ioutil.WriteFile(strayManifest, []byte("stale"), 0600); werr != nil {
		t.Fatalf("could not write stray cached manifest - %v", werr)
	}

	// Dry-run: the cached manifest must survive.
	if err := Clean(context.Background(), jobInfo, true, true); err != nil {
		t.Fatalf("dry-run Clean returned error - %v", err)
	}
	if _, serr := os.Stat(strayManifest); serr != nil {
		t.Fatalf("dry-run should not have deleted %s, stat error - %v", strayManifest, serr)
	}

	// Real run: it must be gone.
	if err := Clean(context.Background(), jobInfo, true, false); err != nil {
		t.Fatalf("Clean returned error - %v", err)
	}
	if _, serr := os.Stat(strayManifest); !os.IsNotExist(serr) {
		t.Fatalf("expected %s to be deleted, got stat error - %v", strayManifest, serr)
	}
}
