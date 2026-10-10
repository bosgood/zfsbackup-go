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
	"syscall"
)

// AtomicTempPrefix starts the names of WriteFileAtomic's temporary files. Whoever lists a
// directory it writes to must skip them.
const AtomicTempPrefix = ".tmp-"

// IsAtomicTemp reports whether name is one of WriteFileAtomic's temporary files.
func IsAtomicTemp(name string) bool {
	return strings.HasPrefix(filepath.Base(name), AtomicTempPrefix)
}

// syncDir is SyncDir; tests replace it to see when WriteFileAtomic syncs.
var syncDir = SyncDir

// SyncDir flushes dir itself to disk, so that a file created, renamed or removed in it stays
// that way after a crash. A file system that cannot sync a directory (EINVAL, ENOTSUP) is not
// an error: there is nothing more to do there.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// WriteFileAtomic writes r to dir/name such that dir/name is never a partial file: it writes a
// temporary file in dir, syncs and closes it, renames it into place and syncs dir. On an error
// before the rename, dir/name is left as it was and the temporary file is removed; an error
// syncing dir means dir/name may not survive a crash.
func WriteFileAtomic(dir, name string, r io.Reader) (err error) {
	tmp, err := os.CreateTemp(dir, AtomicTempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, r); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}
