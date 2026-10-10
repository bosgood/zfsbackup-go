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
	"context"
	"errors"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
)

// failingWriter accepts n bytes, then fails every write.
type failingWriter struct {
	n int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		written := w.n
		w.n = 0
		return written, errors.New("no space left on device")
	}
	w.n -= len(p)
	return len(p), nil
}

// A write that fails at Close's final flush (ENOSPC on the last buffer) must fail Close: the
// volume on disk is short, and its size and checksums describe bytes that were never written.
func TestVolumeCloseReportsFlushError(t *testing.T) {
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	defer func() { config.BackupTempdir = oldTemp }()

	v, err := CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.DeleteVolume() }()
	SetVolumeSink(v, &failingWriter{n: 10 << 10})
	if _, err = v.Write(bytes.Repeat([]byte{'x'}, 100<<10)); err != nil {
		t.Fatal(err) // still in the buffer
	}
	if err = v.Close(); err == nil {
		t.Fatalf("Close succeeded although its flush failed; Size=%d", v.Size)
	}
	if v.SHA256Sum != "" || v.Size != 0 {
		t.Errorf("a failed Close recorded Size=%d SHA256Sum=%q", v.Size, v.SHA256Sum)
	}
	if again := v.Close(); again == nil {
		t.Error("a second Close of a failed volume returned nil")
	}
}

// A file shorter than what was written to it (a late ENOSPC/EDQUOT the writes did not report)
// fails Close.
func TestVolumeCloseChecksFileSize(t *testing.T) {
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	defer func() { config.BackupTempdir = oldTemp }()

	v, err := CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.DeleteVolume() }()
	SetVolumeSink(v, &bytes.Buffer{}) // accepts everything, writes nothing to the file
	if _, err = v.Write(bytes.Repeat([]byte{'x'}, 100<<10)); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err == nil {
		t.Fatal("Close succeeded although the file holds none of the bytes written")
	}
}
