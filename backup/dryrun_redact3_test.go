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
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// TestBackupDryRunRedactsDestinations: the dry-run preview names every destination, and
// must not print the password of one.
func TestBackupDryRunRedactsDestinations(t *testing.T) {
	fakeZFS(t, "tank/data@snap1\t1700000000\tsnapshot\n")
	logs := captureLogs(t)

	jobInfo := &files.JobInfo{
		VolumeName:   "tank/data",
		Destinations: []string{"ssh://backup:hunter2secret@nas.example/backups"},
		BaseSnapshot: files.SnapshotInfo{Name: "snap1", CreationTime: time.Unix(1700000000, 0)},
	}
	if err := Backup(context.Background(), jobInfo, true); err != nil {
		t.Fatalf("dry-run Backup returned error - %v", err)
	}
	if !strings.Contains(logs.String(), "nas.example/backups") {
		t.Fatalf("dry-run did not name the destination:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "hunter2secret") {
		t.Errorf("dry-run log holds the password:\n%s", logs.String())
	}
}
