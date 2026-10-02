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
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/files"
)

// Re-sending a backup set that already exists must never overwrite it: volume boundaries
// are not reproducible, so a re-send that dies partway leaves the old manifest pointing at
// changed objects.

// sameObjects fails unless the destination holds exactly the objects in want, byte for byte.
func sameObjects(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	got := destObjects(t, dir)
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s changed or disappeared", name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s was added", name)
		}
	}
}

func TestE2EExplicitFullAlreadyBackedUpIsNoop(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "autosnap_2026-09-15_00:00:00_daily", CreationTime: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},
		{Name: "autosnap_2026-09-01_00:00:00_monthly", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	args := []string{"--full", "--fullSnapshotSuffix", "_monthly", "tank/data", "file://" + env.dest}
	if logs, err := env.send(args...); err != nil {
		t.Fatalf("first send: %v\n%s", err, logs)
	}
	before := destObjects(t, env.dest)

	logs, err := env.send(args...)
	if !errors.Is(err, backup.ErrNoOp) || !strings.Contains(logs, "Nothing new to back up.") {
		t.Fatalf("second --full of the same monthly: got %v, want the no-op\n%s", err, logs)
	}
	sameObjects(t, env.dest, before)
}
