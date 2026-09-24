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
	"fmt"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// PlanAction is what a smart backup run does.
type PlanAction string

// The actions a smart backup run can take.
const (
	PlanFull        PlanAction = "full"
	PlanIncremental PlanAction = "incremental"
	PlanNoop        PlanAction = "noop"
)

// Why a plan was chosen. These are stable identifiers that goldens and --json
// output depend on, not log messages.
const (
	reasonNoPreviousFull      = "no-previous-full"
	reasonWindowElapsed       = "window-elapsed"
	reasonSourcePruned        = "source-pruned"
	reasonNewerCandidate      = "newer-candidate"
	reasonNothingNewer        = "nothing-newer"
	reasonExplicitFull        = "explicit-full"
	reasonExplicitIncremental = "explicit-incremental"
)

// Plan is the decision of one smart backup run: a full backup of Base, an
// incremental backup from Source to Base, or nothing.
type Plan struct {
	Action PlanAction
	Base   files.SnapshotInfo // zero for noop
	// Source is the incremental source. It is zero for full and noop plans,
	// except with reason source-pruned, where it names the pruned snapshot.
	Source files.SnapshotInfo
	Reason string
}

func (p Plan) String() string {
	switch p.Action {
	case PlanFull:
		return fmt.Sprintf("full backup of %s (%s)", p.Base.Name, p.Reason)
	case PlanIncremental:
		return fmt.Sprintf("incremental backup of %s from %s (%s)", p.Base.Name, p.Source.Name, p.Reason)
	default:
		return fmt.Sprintf("nothing to back up (%s)", p.Reason)
	}
}

// planSmartSnapshots decides what a "smart" backup sends. It is pure (performs
// no I/O) so it can be unit-tested and simulated: all ZFS and backend state is
// passed in. snapshots must be sorted newest-first, and destBackups[i] holds
// the manifests found at destination i, also newest-first. It reads only the
// smart options from jobInfo.
//
// When FullSnapshotSuffix/IncrementalSnapshotSuffix are set, full backups are
// anchored on the newest snapshot matching the full suffix (e.g. "_monthly")
// and incrementals target the newest snapshot matching the incremental suffix
// (e.g. "_daily"). With both suffixes empty this preserves the historical
// behavior of using the single newest matching snapshot for everything.
// nolint:funlen,gocyclo // Difficult to break this up
func planSmartSnapshots(jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo) (Plan, error) {
	if len(snapshots) == 0 {
		return Plan{}, fmt.Errorf("no snapshots found")
	}

	fullBase := newestMatchingSnapshot(snapshots, jobInfo.SnapshotPrefix, jobInfo.FullSnapshotSuffix)
	incrBase := newestMatchingSnapshot(snapshots, jobInfo.SnapshotPrefix, jobInfo.IncrementalSnapshotSuffix)

	// An explicit full backup always anchors on the full-candidate snapshot.
	if jobInfo.Full {
		if fullBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the full backup criteria")
		}
		return Plan{Action: PlanFull, Base: *fullBase, Reason: reasonExplicitFull}, nil
	}

	// Gather the most recent backup and most recent full backup per destination.
	lastComparableSnapshots := make([]*files.SnapshotInfo, len(destBackups))
	lastBackup := make([]*files.SnapshotInfo, len(destBackups))
	for idx := range destBackups {
		if len(destBackups[idx]) == 0 {
			continue
		}
		lastBackup[idx] = &destBackups[idx][0].BaseSnapshot
		if jobInfo.Incremental {
			lastComparableSnapshots[idx] = &destBackups[idx][0].BaseSnapshot
		}
		if jobInfo.FullIfOlderThan != -1*time.Minute {
			for _, bkp := range destBackups[idx] {
				if bkp.IncrementalSnapshot.Name == "" {
					lastComparableSnapshots[idx] = &bkp.BaseSnapshot
					break
				}
			}
		}
	}

	var lastNotEqual bool
	// Verify that all "comparable" snapshots are the same across destinations
	for i := 1; i < len(lastComparableSnapshots); i++ {
		if !lastComparableSnapshots[i-1].Equal(lastComparableSnapshots[i]) {
			return Plan{}, fmt.Errorf("destinations are out of sync, cannot continue with smart option")
		}

		if !lastNotEqual && !lastBackup[i-1].Equal(lastBackup[i]) {
			lastNotEqual = true
		}
	}

	// Now select the proper job options and continue
	if jobInfo.Incremental {
		if incrBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the incremental backup criteria")
		}
		if lastComparableSnapshots[0] == nil {
			return Plan{}, fmt.Errorf("no snapshot to increment from - try doing a full backup instead")
		}
		if !incrBase.CreationTime.After(lastComparableSnapshots[0].CreationTime) {
			return Plan{Action: PlanNoop, Reason: reasonNothingNewer}, nil
		}
		return Plan{Action: PlanIncremental, Base: *incrBase, Source: *lastComparableSnapshots[0], Reason: reasonExplicitIncremental}, nil
	}

	if jobInfo.FullIfOlderThan == -1*time.Minute {
		return Plan{}, fmt.Errorf("no smart backup option set")
	}

	lastFull := lastComparableSnapshots[0]

	// No previous full backup found, so do one.
	if lastFull == nil {
		if fullBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the full backup criteria")
		}
		return Plan{Action: PlanFull, Base: *fullBase, Reason: reasonNoPreviousFull}, nil
	}

	// Roll onto a newer full-candidate snapshot once the last full is older
	// than the configured window. Age is measured against the most recent
	// snapshot; the full is anchored on the newest full-candidate (e.g. the
	// newest "_monthly"), which must be newer than the existing full.
	ageExceeded := snapshots[0].CreationTime.Sub(lastFull.CreationTime) > jobInfo.FullIfOlderThan
	hasNewerFullBase := fullBase != nil && fullBase.CreationTime.After(lastFull.CreationTime)
	if ageExceeded && hasNewerFullBase {
		return Plan{Action: PlanFull, Base: *fullBase, Reason: reasonWindowElapsed}, nil
	}

	// Otherwise perform an incremental up to the incremental-candidate snapshot.
	if incrBase == nil {
		return Plan{}, fmt.Errorf("no snapshots found matching the incremental backup criteria")
	}
	if lastNotEqual {
		return Plan{}, fmt.Errorf("want to do an incremental backup but last incremental backup at destinations do not match")
	}
	if !incrBase.CreationTime.After(lastBackup[0].CreationTime) {
		return Plan{Action: PlanNoop, Reason: reasonNothingNewer}, nil
	}

	// The incremental source (the most recent backup) must still exist locally
	// to send from it. If it has been pruned, fall back to a full backup. The
	// copy keeps the destination state untouched: the existence check flags
	// the source as a bookmark if only a bookmark of it is left.
	source := *lastBackup[0]
	if !validateSnapShotExistsFromSnaps(&source, snapshots, true) {
		if fullBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the full backup criteria")
		}
		return Plan{Action: PlanFull, Base: *fullBase, Source: source, Reason: reasonSourcePruned}, nil
	}
	return Plan{Action: PlanIncremental, Base: *incrBase, Source: source, Reason: reasonNewerCandidate}, nil
}
