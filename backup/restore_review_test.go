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
	"path/filepath"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/config"
	"github.com/bosgood/zfsbackup-go/files"
)

// Finding 19: a local temp-file failure (full disk, no temp dir) is permanent. It was retried
// with a new download of the same object for --maxRetryTime (12 h by default).
func TestReviewTempFileErrorIsPermanent(t *testing.T) {
	old := config.BackupTempdir
	config.BackupTempdir = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { config.BackupTempdir = old })
	b := &cacheBackend{objects: map[string][]byte{"v1": []byte("data")}}
	j := &files.JobInfo{MaxRetryTime: 1500 * time.Millisecond, MaxBackoffTime: 200 * time.Millisecond}
	c := make(chan *files.VolumeInfo, 1)
	buffer := make(chan interface{}, 1)
	seq := downloadSequence{volume: &files.VolumeInfo{ObjectName: "v1", Size: 4}, c: c}
	if err := downloadSequenceVolume(context.Background(), j, seq, b, buffer, false); err == nil {
		t.Fatal("download with an unwritable temp dir succeeded")
	}
	if b.downloads != 1 {
		t.Errorf("the temp-file failure was retried: %d downloads of the same object (want 1)", b.downloads)
	}
}

// A failed download (the read, not the write) is still retried. Before the fix of finding 19,
// the error was lost: processSequence put vol.Close's nil in err and returned it, so a download
// that broke off counted as done, and its volume never got to zfs receive.
func TestReviewDownloadErrorIsRetried(t *testing.T) {
	old := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	t.Cleanup(func() { config.BackupTempdir = old })
	b := &cacheBackend{objects: map[string][]byte{"v1": []byte("data")}, failures: 1}
	j := &files.JobInfo{MaxRetryTime: 10 * time.Second, MaxBackoffTime: 100 * time.Millisecond}
	c := make(chan *files.VolumeInfo, 1)
	buffer := make(chan interface{}, 1)
	seq := downloadSequence{volume: &files.VolumeInfo{ObjectName: "v1", Size: 4}, c: c}
	// The hash does not match (the VolumeInfo has none): only the count of downloads matters.
	_ = downloadSequenceVolume(context.Background(), j, seq, b, buffer, false)
	if b.downloads != 2 {
		t.Errorf("a broken download was tried %d times, want 2", b.downloads)
	}
}
