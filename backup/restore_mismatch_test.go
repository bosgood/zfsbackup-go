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
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// receive --auto matches a local snapshot by name and creation time. When only the name matches
// (the snapshot was destroyed and taken again under its name), zfs refuses the full stream with
// its own message; the user must be told which local snapshot is not the backed-up one
// (docs/specs/2026-10-10--high-risk-review/findings.md, Item 4 and Other 3).
func TestNoteLocalSnapshotMismatch(t *testing.T) {
	backedUp := files.SnapshotInfo{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	local := []files.SnapshotInfo{
		{Name: "b", CreationTime: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		{Name: "a", CreationTime: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)},
	}

	logs := captureLogs(t)
	noteLocalSnapshotMismatch("restored/data", &backedUp, local)
	want := "The local snapshot restored/data@a is not the one backed up: it was created 2026-09-03 12:00:00 UTC, " +
		"the backup's on 2026-09-01 00:00:00 UTC. The restore does not build on it"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("want a Notice %q, got:\n%s", want, logs.String())
	}

	logs = captureLogs(t)
	noteLocalSnapshotMismatch("restored/data", &backedUp, local[:1])
	if logs.Len() != 0 {
		t.Errorf("no local snapshot of that name, yet a Notice:\n%s", logs.String())
	}
	noteLocalSnapshotMismatch("restored/data", &backedUp, []files.SnapshotInfo{backedUp})
	if logs.Len() != 0 {
		t.Errorf("the local snapshot is the backed-up one, yet a Notice:\n%s", logs.String())
	}
}
