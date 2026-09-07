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
	"reflect"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

const testVolume = "ypool/enc/user/data"

// manifest models a backup manifest as stored at a destination. An empty incr
// means a full backup.
func manifest(base, incr string, baseTime, incrTime time.Time) *files.JobInfo {
	j := &files.JobInfo{
		VolumeName:   testVolume,
		Separator:    "|",
		BaseSnapshot: files.SnapshotInfo{Name: base, CreationTime: baseTime},
	}
	if incr != "" {
		j.IncrementalSnapshot = files.SnapshotInfo{Name: incr, CreationTime: incrTime}
	}
	return j
}

// restoreChain replicates the chain walk AutoRestore performs once manifests
// are linked: start at the target manifest and follow ParentSnap until a full
// backup is reached. It returns the snapshots that would be received, in the
// order they would be applied (oldest first).
func restoreChain(t *testing.T, target *files.JobInfo) []string {
	t.Helper()

	var chain []*files.JobInfo
	for cur := target; ; cur = cur.ParentSnap {
		chain = append(chain, cur)
		if cur.IncrementalSnapshot.Name == "" {
			break // full backup, chain is complete
		}
		if cur.ParentSnap == nil {
			t.Fatalf("no parent manifest for %s (incremental from %s)",
				cur.BaseSnapshot.Name, cur.IncrementalSnapshot.Name)
		}
		if len(chain) > len(chain)+100 {
			t.Fatal("cycle in manifest chain")
		}
	}

	ordered := make([]string, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		ordered = append(ordered, chain[i].BaseSnapshot.Name)
	}
	return ordered
}

// A branched chain arises when an incremental is taken from an older snapshot
// than the most recent backup - e.g. falling back to the last full's base when
// the previous daily has been pruned locally. linkManifests tracks parents
// rather than a linear history, so both branches remain restorable.
func TestLinkManifestsBranchedChain(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }

	full := manifest("m1", "", day(1), time.Time{})
	d38 := manifest("d38", "m1", day(38), day(1))
	d39 := manifest("d39", "d38", day(39), day(38))
	// d39 was pruned locally, so this increments from the full's base instead.
	d40 := manifest("d40", "m1", day(40), day(1))

	linkManifests([]*files.JobInfo{full, d38, d39, d40})

	if d40.ParentSnap != full {
		t.Errorf("d40 parent = %v, want the full backup m1", d40.ParentSnap)
	}
	if d38.ParentSnap != full {
		t.Errorf("d38 parent = %v, want the full backup m1", d38.ParentSnap)
	}
	if d39.ParentSnap != d38 {
		t.Errorf("d39 parent = %v, want d38", d39.ParentSnap)
	}

	// The branch restores in two streams, skipping the d38/d39 leg entirely.
	if got, want := restoreChain(t, d40), []string{"m1", "d40"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restore chain for d40 = %v, want %v", got, want)
	}
	// The original leg is unaffected and still restorable.
	if got, want := restoreChain(t, d39), []string{"m1", "d38", "d39"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restore chain for d39 = %v, want %v", got, want)
	}
}

// Falling back to a full can re-upload a full of a snapshot the destination
// already holds a full for. Both manifests share a base snapshot, and therefore
// an object name, so the second overwrites the first rather than accumulating.
func TestLinkManifestsDuplicateFull(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }

	first := manifest("m1", "", day(1), time.Time{})
	second := manifest("m1", "", day(1), time.Time{})
	child := manifest("d40", "m1", day(40), day(1))

	if first.ManifestObjectName() != second.ManifestObjectName() {
		t.Errorf("duplicate fulls have different object names (%s vs %s); they would accumulate at the destination",
			first.ManifestObjectName(), second.ManifestObjectName())
	}

	linkManifests([]*files.JobInfo{first, second, child})

	if child.ParentSnap == nil {
		t.Fatal("child has no parent despite two candidate full backups")
	}
	if got, want := restoreChain(t, child), []string{"m1", "d40"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restore chain = %v, want %v", got, want)
	}
}
