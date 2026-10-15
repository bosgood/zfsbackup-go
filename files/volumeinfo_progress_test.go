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
	"io"
	"testing"

	"github.com/bosgood/zfsbackup-go/config"
)

// The read hook sees every byte a reader takes from the volume, and nil clears it.
func TestVolumeReadHook(t *testing.T) {
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	defer func() { config.BackupTempdir = oldTemp }()

	vol, err := CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vol.DeleteVolume() }()
	if _, err = vol.Write(bytes.Repeat([]byte{'x'}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}

	var sum uint64
	vol.SetReadHook(func(n int) { sum += uint64(n) })
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, vol); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	if sum != vol.Size || sum == 0 {
		t.Fatalf("the hook counted %d bytes, want the volume's %d", sum, vol.Size)
	}

	vol.SetReadHook(nil)
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, vol); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	if sum != vol.Size {
		t.Errorf("after SetReadHook(nil) the hook counted %d bytes, want it still at %d", sum, vol.Size)
	}
}
