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
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// cacheBackend serves objects for download; the first failures downloads of each break off
// halfway, like a dropped connection.
type cacheBackend struct {
	mockBackend
	objects   map[string][]byte
	failures  int
	downloads int
}

type brokenReader struct {
	r io.Reader
}

func (b *brokenReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset by peer")
	}
	return n, err
}

func (c *cacheBackend) List(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	for name := range c.objects {
		names = append(names, name)
	}
	return names, nil
}

func (c *cacheBackend) Download(ctx context.Context, name string) (io.ReadCloser, error) {
	c.downloads++
	data, ok := c.objects[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	if c.failures > 0 {
		c.failures--
		return ioutil.NopCloser(&brokenReader{bytes.NewReader(data[:len(data)/2])}), nil
	}
	return ioutil.NopCloser(bytes.NewReader(data)), nil
}

var _ backends.Backend = (*cacheBackend)(nil)

func TestDownloadToLeavesNoPartialFileOnError(t *testing.T) {
	dir := t.TempDir()
	b := &cacheBackend{objects: map[string][]byte{"manifests|x": bytes.Repeat([]byte{'m'}, 4096)}, failures: 1}
	path := filepath.Join(dir, "cached")
	if err := downloadTo(context.Background(), b, "manifests|x", path); err == nil {
		t.Fatal("a broken download succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a broken download left %v behind", entries)
	}
}

// A download that broke off must be downloaded again by the next sync, not trusted by its name.
func TestSyncCacheRetriesTruncatedDownload(t *testing.T) {
	oldWorkingDir := config.WorkingDir
	config.WorkingDir = t.TempDir()
	defer func() { config.WorkingDir = oldWorkingDir }()
	cache := t.TempDir()
	manifest := bytes.Repeat([]byte{'m'}, 4096)
	b := &cacheBackend{objects: map[string][]byte{"manifests|x": manifest}, failures: 1}
	j := &files.JobInfo{ManifestPrefix: "manifests"}

	if _, _, err := syncCache(context.Background(), j, cache, b); err == nil {
		t.Fatal("a sync with a broken download succeeded")
	}
	if _, _, err := syncCache(context.Background(), j, cache, b); err != nil {
		t.Fatal(err)
	}
	if b.downloads != 2 {
		t.Errorf("the second sync made %d downloads in all, want it to download again", b.downloads)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one cached manifest, got %v (%v)", entries, err)
	}
	if data, _ := ioutil.ReadFile(filepath.Join(cache, entries[0].Name())); !bytes.Equal(data, manifest) {
		t.Errorf("cached %d bytes, want the %d-byte manifest", len(data), len(manifest))
	}
}
