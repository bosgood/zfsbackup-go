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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// enospcWork is a working directory whose temp/ is a tiny tmpfs; `make test-enospc` provides it.
func enospcWork(t *testing.T) string {
	t.Helper()
	work := os.Getenv("ZFSBACKUP_ENOSPC_WORK")
	if work == "" {
		t.Skip("needs ZFSBACKUP_ENOSPC_WORK with a small tmpfs at temp/; run `make test-enospc`")
	}
	return work
}

// fill writes to dir until it is full, and removes what it wrote when the test ends.
func fill(t *testing.T, dir string) {
	t.Helper()
	f, err := os.CreateTemp(dir, "filler")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	defer f.Close()
	chunk := make([]byte, 4<<10)
	for {
		if _, err = f.Write(chunk); err != nil {
			break
		}
	}
	for { // the last partial page
		if _, err = f.Write(chunk[:1]); err != nil {
			return
		}
	}
}

// A volume or manifest that hits ENOSPC at its final flush must fail the send, not publish a
// short volume or a 0-byte manifest.
func TestE2EENOSPCAtFinalFlushFails(t *testing.T) {
	work := enospcWork(t)
	for _, tc := range []struct {
		name string
		args []string
		full bool
	}{
		// 200 KiB fits in the volume's write buffer: only Close's flush writes the file.
		{"volume", []string{"--compressor", ""}, false},
		// Volumes go through pipes; the manifest is the only file, and the tmpfs is already full.
		{"manifest", []string{"--maxFileBuffer", "0"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			env.work = filepath.Join(work, tc.name)
			if err := os.MkdirAll(filepath.Join(env.work, "temp"), 0700); err != nil {
				t.Fatal(err)
			}
			// The tmpfs is mounted at <work>/temp; each case gets its own working directory beside it.
			if err := os.Remove(filepath.Join(env.work, "temp")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(work, "temp"), filepath.Join(env.work, "temp")); err != nil {
				t.Fatal(err)
			}
			if tc.full {
				fill(t, filepath.Join(work, "temp"))
			}
			t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(200<<10))
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
			args := append(tc.args, "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
			logs, err := guarded(t, func() (string, error) { return env.send(args...) })
			if err == nil {
				t.Errorf("send succeeded although the temp dir is full:\n%s", logs)
			}
			if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
				t.Errorf("a send that hit ENOSPC published manifests %q", names)
			}
		})
	}
}
