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
	"bytes"
	"compress/gzip"
	"context"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
)

// TestReadManifestBounded: someone with only the public key forges a manifest signed by their
// own key and encrypted to the real one, whose gzip body inflates to 128 MiB of JSON string.
// ReadManifest must reject it without decoding all of it (that allocated 516 MiB).
func TestReadManifestBounded(t *testing.T) {
	key, forger := newTestKey(t, "backup@example.com"), newTestKey(t, "evil@example.com")
	loadRings(t, []*openpgp.Entity{key}, []*openpgp.Entity{key})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	chunk := []byte(strings.Repeat("A", 1<<20))
	if _, err := zw.Write([]byte(`{"VolumeName":"`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := zw.Write([]byte(`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var msg bytes.Buffer
	w, err := openpgp.Encrypt(&msg, []*openpgp.Entity{key}, forger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(gz.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest")
	if err = ioutil.WriteFile(path, msg.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = ReadManifest(context.Background(), &JobInfo{SignKey: key, EncryptKey: key}, path)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "longer than 64 MiB") {
		t.Fatalf("want the 64 MiB limit to reject a 128 MiB manifest, got %v", err)
	}
	if mib := (after.TotalAlloc - before.TotalAlloc) >> 20; mib > 256 {
		t.Errorf("reading it allocated %d MiB, want at most 256 (%v)", mib, err)
	}
}
