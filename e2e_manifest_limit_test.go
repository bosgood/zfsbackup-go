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
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// A send whose manifest would be longer than files.MaxManifestBytes fails, says to use a larger
// --volsize, and publishes no manifest: a manifest no reader accepts would lock every reader out
// of the destination (docs/specs/2026-10-10--high-risk-review/findings.md, Item 2).
func TestE2ESendRefusesManifestOverLimit(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(8<<20))
	old := files.MaxManifestBytes
	files.MaxManifestBytes = 4096
	t.Cleanup(func() { files.MaxManifestBytes = old })

	logs, err := guarded(t, func() (string, error) {
		return env.send("--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", "file://"+env.dest)
	})
	if err == nil {
		t.Fatalf("a send of 8 volumes with a 4 KiB manifest limit succeeded:\n%s", logs)
	}
	if !strings.Contains(logs, "--volsize") {
		t.Errorf("the log does not say to use a larger --volsize: %v\n%s", err, logs)
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("the failed send published a manifest: %q", names)
	}
}
