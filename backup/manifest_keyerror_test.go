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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

// A manifest encrypted to a key this run lacks is a backup of the user's written with another
// key, not a forgery: the error names the key problem and how to pass the right ring, never
// advises deleting the object, keeps the cached copy (the object is the same bytes), and the
// object is downloaded once.
func TestReadCachedManifestWrongKeyAdvice(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	k1, k2 := prefixedKey(t, "k1@example.com"), prefixedKey(t, "k2@example.com")
	loadPrefixedRings(t, k1, k2)
	t.Cleanup(func() { loadPrefixedRings(t) }) // leave the package rings empty

	set, _ := prefixedSet(t, targetDir, "tank/data", "a", "", "manifests", k1, "k1@example.com")
	name := set.ManifestObjectName()
	data, err := os.ReadFile(filepath.Join(targetDir, name))
	if err != nil {
		t.Fatal(err)
	}
	loadPrefixedRings(t, k2)
	jobInfo.EncryptKey, jobInfo.EncryptTo = k2, "k2@example.com"
	b := &cacheBackend{objects: map[string][]byte{name: data}}
	cache := t.TempDir()

	for round := 1; round <= 2; round++ {
		_, err := readCachedManifest(context.Background(), jobInfo, cache, b, name)
		if !errors.As(err, new(*files.KeyError)) {
			t.Fatalf("round %d: got %v, want a KeyError", round, err)
		}
		if strings.Contains(err.Error(), "delete that object") {
			t.Errorf("round %d: the error tells the user to delete a backup: %v", round, err)
		}
		if !strings.Contains(err.Error(), "--secretKeyRingPath") {
			t.Errorf("round %d: the error does not say how to pass the key: %v", round, err)
		}
		if _, serr := os.Stat(filepath.Join(cache, cachedManifestName(name))); serr != nil {
			t.Errorf("round %d: the cached copy is gone: %v", round, serr)
		}
		if b.downloads != 1 {
			t.Errorf("round %d: %d downloads, want 1", round, b.downloads)
		}
	}
}
