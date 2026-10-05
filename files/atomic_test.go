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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, errors.New("connection reset")
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.n -= len(p)
	return len(p), nil
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFileAtomic(dir, "f", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(dir, "f", &failingReader{n: 10}); err == nil {
		t.Fatal("a failing reader did not fail the write")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "f")); err != nil || string(data) != "old" {
		t.Errorf("a failed write changed f to %q (%v)", data, err)
	}
	if err := WriteFileAtomic(dir, "g", io.LimitReader(&failingReader{n: 10}, 5)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("want only f and g, got %v", entries)
	}
	for _, e := range entries {
		if IsAtomicTemp(e.Name()) {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}
