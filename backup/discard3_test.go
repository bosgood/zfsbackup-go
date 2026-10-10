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
	"os"
	"path/filepath"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// discardPartialManifests syncs the cache dir after removing a manifest: a crash before that
// can bring the earlier attempt's cache back, and a later --resume would trust it.
func TestDiscardPartialManifestsSyncsDir(t *testing.T) {
	oldWorkingDir := config.WorkingDir
	config.WorkingDir = t.TempDir()
	t.Cleanup(func() { config.WorkingDir = oldWorkingDir })

	j := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "a"}, ManifestPrefix: "manifests", Separator: "|"}
	d := destination{uri: "file:///nowhere"}
	path := partialManifestCachePath(j, d.uri)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}

	var synced []string
	old := syncDir
	syncDir = func(dir string) error {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("dir synced before the remove: %v", err)
		}
		synced = append(synced, dir)
		return old(dir)
	}
	t.Cleanup(func() { syncDir = old })

	if err := discardPartialManifests(j, []destination{d}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(path); len(synced) != 1 || synced[0] != want {
		t.Errorf("synced %q, want [%q]", synced, want)
	}
}
