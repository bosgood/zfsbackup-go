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

package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteFileAtomic syncs dir after the rename: until then, a crash can lose the new name.
func TestWriteFileAtomicSyncsDir(t *testing.T) {
	dir := t.TempDir()
	var synced []string
	old := syncDir
	syncDir = func(d string) error {
		if _, err := os.Stat(filepath.Join(dir, "f")); err != nil {
			t.Errorf("dir synced before the rename: %v", err)
		}
		synced = append(synced, d)
		return old(d)
	}
	t.Cleanup(func() { syncDir = old })

	if err := WriteFileAtomic(dir, "f", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Errorf("synced %q, want [%q]", synced, dir)
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir: %v", err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Errorf("SyncDir of a missing dir: got %v, want not-exist", err)
	}
}
