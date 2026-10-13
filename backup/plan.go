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
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
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
	reasonAlreadyBackedUp     = "already-backed-up"
	reasonCompletePartial     = "complete-partial"
	reasonBrokenChain         = "broken-chain"
)

// Now is the clock the smart planner reads to find snapshots and backups dated in the future
// (see dropFutureDated). Tests set it, as they set HostZone; nothing else does.
var Now = time.Now

// futureSlack is how far after now a snapshot or a backup may be dated. One dated later is from a
// wrong clock (a host that booted with its clock in 2099, say): the planner ignores it. A day
// covers time zones and a clock that is a few hours fast.
const futureSlack = 24 * time.Hour

// fullWindowSlack is the slack of --fullIfOlderThan: a full is due when the last full is older
// than the window minus this. sanoid takes its snapshots some seconds after the hour, a few
// seconds earlier or later each time; without the slack, a 720h window after a 30-day month
// would depend on those seconds. A window of an hour or less has no slack.
const fullWindowSlack = 10 * time.Minute

// fullWindowElapsed reports whether a last full of age age is old enough for a new full.
func fullWindowElapsed(age, window time.Duration) bool {
	if window > time.Hour {
		window -= fullWindowSlack
	}
	return age > window
}

// Plan is the decision of one smart backup run: a full backup of Base, an
// incremental backup from Source to Base, or nothing.
type Plan struct {
	Action PlanAction
	Base   files.SnapshotInfo // zero for noop
	// Source is the incremental source. It is zero for full and noop plans,
	// except with reason source-pruned, where it names the pruned snapshot.
	Source files.SnapshotInfo
	Reason string
	// FullDue is set when the last full is older than --fullIfOlderThan but
	// no full candidate newer than the last backup exists yet.
	FullDue bool
	// Warnings tell what the planner ignored or found wrong, also when it fails. send logs them
	// at Warning; plan prints them under the run.
	Warnings []string
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

// planSmartSnapshots decides what a "smart" backup sends now (the clock Now), as planSmartAt
// does, and logs the plan's warnings at Warning.
func planSmartSnapshots(
	jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo, completable bool,
) (Plan, error) {
	p, err := planSmartAt(Now(), jobInfo, snapshots, destBackups, completable)
	for _, w := range p.Warnings {
		log.AppLogger.Warningf("%s", w)
	}
	return p, err
}

// planSmartAt decides what a "smart" backup run at now sends. It is pure (performs
// no I/O) so it can be unit-tested and simulated: all ZFS and backend state is
// passed in. snapshots must be sorted newest-first, and destBackups[i] holds
// the manifests found at destination i, also newest-first. It reads only the
// smart options, Raw and the zfs send flags that cannot send from a bookmark
// from jobInfo, and VolumeName for messages.
//
// Snapshots and backups dated more than futureSlack after now are ignored, with
// a warning (see dropFutureDated). The returned plan holds its warnings also
// when the error is not nil.
func planSmartAt(
	now time.Time, jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo, completable bool,
) (Plan, error) {
	snapshots, destBackups, warnings := dropFutureDated(jobInfo.VolumeName, now, snapshots, destBackups)
	p, err := planSmart(jobInfo, snapshots, destBackups, completable)
	p.Warnings = append(warnings, p.Warnings...)
	return p, err
}

// dropFutureDated removes the snapshots and the backups dated more than futureSlack after now,
// and returns a warning for each. A snapshot taken while the clock was wrong (in 2099, say) would
// otherwise be backed up as the newest, and every later snapshot would look older than that
// backup: each run would send nothing, with exit 0, and an explicit --full would not change it.
// A backup is dated by its base snapshot. The inputs are not changed.
func dropFutureDated(
	volume string, now time.Time, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo,
) ([]files.SnapshotInfo, [][]*files.JobInfo, []string) {
	limit := now.Add(futureSlack)
	var warnings []string
	keptSnapshots := make([]files.SnapshotInfo, 0, len(snapshots))
	for _, s := range snapshots {
		if !s.CreationTime.After(limit) {
			keptSnapshots = append(keptSnapshots, s)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"Ignoring %s%s: it is dated %s, more than %v after now (%s). Was the clock wrong when it was taken? "+
				"A smart send does not back it up; destroy it, or send it with an explicit snapshot name.",
			volume, snapshotSeparator(s), s.CreationTime.UTC().Format(time.RFC3339), futureSlack, now.UTC().Format(time.RFC3339),
		))
	}
	keptBackups := make([][]*files.JobInfo, len(destBackups))
	for idx, backups := range destBackups {
		keptBackups[idx] = make([]*files.JobInfo, 0, len(backups))
		for _, m := range backups {
			if !m.BaseSnapshot.CreationTime.After(limit) {
				keptBackups[idx] = append(keptBackups[idx], m)
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"Ignoring the backup %s at %s: its snapshot %s is dated %s, more than %v after now (%s). Was the clock "+
					"wrong when it was taken? The next backups do not continue from it.",
				describeBackup(m.BaseSnapshot, m.IncrementalSnapshot), destinationLabel(idx, len(destBackups)),
				m.BaseSnapshot.Name, m.BaseSnapshot.CreationTime.UTC().Format(time.RFC3339), futureSlack, now.UTC().Format(time.RFC3339),
			))
		}
	}
	return keptSnapshots, keptBackups, warnings
}

// snapshotSeparator is "@name" for a snapshot and "#name" for a bookmark.
func snapshotSeparator(s files.SnapshotInfo) string {
	if s.Bookmark {
		return "#" + s.Name
	}
	return "@" + s.Name
}

// destinationLabel names destination idx of n in a message.
func destinationLabel(idx, n int) string {
	if n > 1 {
		return fmt.Sprintf("destination #%d", idx+1)
	}
	return "the destination"
}

// brokenLink returns the snapshot whose backup is missing from the chain of the newest backup in
// backups (newest-first): restoring the newest backup needs a backup of it, and the destination
// has none. It is nil when that chain reaches a full, and for no backups. Links are found as
// parentOf finds them.
func brokenLink(backups []*files.JobInfo) *files.SnapshotInfo {
	if len(backups) == 0 {
		return nil
	}
	m := backups[0]
	for steps := 0; m.IncrementalSnapshot.Name != ""; steps++ {
		parent := parentOf(backups, m.IncrementalSnapshot)
		if parent == nil || steps >= len(backups) { // steps: a loop of links cannot be restored either
			missing := m.IncrementalSnapshot
			return &missing
		}
		m = parent
	}
	return nil
}

// needsSnapshotSource reports whether jobInfo has a zfs send flag that cannot send from a bookmark:
// zfs send refuses a bookmark source with -R and -I ("multiple snapshots cannot be sent from a
// bookmark", zfs 2.4.4). It accepts one with -p and -D (TestIntegrationS3ReviewBookmarkReplication).
func needsSnapshotSource(jobInfo *files.JobInfo) bool {
	return jobInfo.Replication || jobInfo.IntermediaryIncremental
}

// planSmart is planSmartAt without the future-dated check.
//
// When FullSnapshotSuffix/IncrementalSnapshotSuffix are set, full backups are
// anchored on the newest snapshot matching the full suffix (e.g. "_monthly")
// and incrementals target the newest snapshot matching the incremental suffix
// (e.g. "_daily"). With both suffixes empty this preserves the historical
// behavior of using the single newest matching snapshot for everything.
// nolint:funlen,gocyclo // Difficult to break this up
func planSmart(
	jobInfo *files.JobInfo, snapshots []files.SnapshotInfo, destBackups [][]*files.JobInfo, completable bool,
) (Plan, error) {
	if len(snapshots) == 0 {
		return Plan{}, fmt.Errorf("no snapshots found")
	}

	fullBase := newestMatchingSnapshot(snapshots, jobInfo.SnapshotPrefix, jobInfo.FullSnapshotSuffix)
	incrBase := newestMatchingSnapshot(snapshots, jobInfo.SnapshotPrefix, jobInfo.IncrementalSnapshotSuffix)

	// An explicit full backup always anchors on the full-candidate snapshot, unless that full
	// already exists: sending it again would overwrite a good backup set in place.
	if jobInfo.Full {
		if fullBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the full backup criteria")
		}
		var have, missing []int
		for idx := range destBackups {
			if hasFullOf(destBackups[idx], fullBase) {
				have = append(have, idx)
			} else {
				missing = append(missing, idx)
			}
		}
		switch {
		case len(have) > 0 && len(missing) == 0:
			return Plan{Action: PlanNoop, Reason: reasonAlreadyBackedUp}, nil
		case len(have) > 0 && !jobInfo.Resume:
			// Usually a manifest upload that failed at some destinations; --resume completes the set.
			return Plan{}, fmt.Errorf(
				"destinations are out of sync: destination #%d already has a full of %s, destination #%d does not; "+
					"run again with --resume to complete it there",
				have[0]+1, fullBase.Name, missing[0]+1,
			)
		}
		return Plan{Action: PlanFull, Base: *fullBase, Reason: reasonExplicitFull}, nil
	}

	// A manifest upload that failed at some destinations leaves them one set behind the rest.
	// Re-plan that set: Backup completes it there, after checking every volume, instead of
	// sending anything. completable says the lagging destinations hold its volumes (or --resume
	// asked for it): otherwise they diverged some other way, and the planning below decides.
	if partial := partialSet(destBackups); partial != nil && completable {
		if partial.IncrementalSnapshot.Name == "" {
			return Plan{Action: PlanFull, Base: partial.BaseSnapshot, Reason: reasonCompletePartial}, nil
		}
		return Plan{Action: PlanIncremental, Base: partial.BaseSnapshot, Source: partial.IncrementalSnapshot, Reason: reasonCompletePartial}, nil
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

	if err := checkLastBackupsOnPool(jobInfo.VolumeName, snapshots, lastBackup); err != nil {
		return Plan{}, err
	}

	var lastNotEqual bool
	// Verify that all "comparable" snapshots are the same across destinations
	for i := 1; i < len(lastComparableSnapshots); i++ {
		if !lastComparableSnapshots[i-1].Equal(lastComparableSnapshots[i]) {
			kind := "full backup"
			if jobInfo.Incremental {
				kind = "backup"
			}
			return Plan{}, fmt.Errorf(
				"destinations are out of sync, cannot continue with smart option: destination #%d's last %s is %s, destination #%d's is %s",
				i, kind, snapshotName(lastComparableSnapshots[i-1]), i+1, snapshotName(lastComparableSnapshots[i]),
			)
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
		idx, missing := firstBrokenChain(destBackups)
		if !incrBase.CreationTime.After(lastComparableSnapshots[0].CreationTime) {
			p := Plan{Action: PlanNoop, Reason: reasonNothingNewer}
			if missing != nil {
				// Nothing to send now, but the next run fails: say so now.
				p.Warnings = []string{fmt.Sprintf(
					"The chain of %s at %s is broken: it holds no backup of %s, so its newest backup, %s, and every "+
						"incremental after it cannot be restored. The next incremental is refused. Start a new chain with --full.",
					jobInfo.VolumeName, destinationLabel(idx, len(destBackups)), missing.Name, destBackups[idx][0].BaseSnapshot.Name,
				)}
			}
			return p, nil
		}
		if missing != nil {
			return Plan{}, fmt.Errorf(
				"cannot continue the chain of %s: %s holds no backup of %s, so its newest backup, %s, cannot be "+
					"restored, and an incremental from it could not be either. Start a new chain with --full",
				jobInfo.VolumeName, destinationLabel(idx, len(destBackups)), missing.Name, destBackups[idx][0].BaseSnapshot.Name,
			)
		}
		if err := sameStreamKind(jobInfo, destBackups); err != nil {
			return Plan{}, err
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
	// newest "_monthly"), which must be newer than the most recent backup.
	// An older candidate was already sent (so the full would re-send it), or
	// predates the last incremental (which the next incremental continues
	// from, orphaning the full). When destinations disagree on the most recent
	// backup, the one furthest behind decides, so the full brings them back
	// in step whatever order they are given in.
	ageExceeded := fullWindowElapsed(snapshots[0].CreationTime.Sub(lastFull.CreationTime), jobInfo.FullIfOlderThan)
	// Nothing matches the full backup criteria at all, so no full can ever be taken and
	// --fullIfOlderThan can never be honored: the prefix or full suffix is almost certainly
	// wrong. Fail loudly rather than extending the incremental chain forever.
	if ageExceeded && fullBase == nil {
		return Plan{}, fmt.Errorf(
			"full backup is due (last full %s is older than %v) but no snapshots found matching the full backup criteria",
			lastFull.Name, jobInfo.FullIfOlderThan,
		)
	}
	behind := lastBackup[0]
	for _, b := range lastBackup[1:] {
		if b.CreationTime.Before(behind.CreationTime) {
			behind = b
		}
	}
	hasNewerFullBase := fullBase != nil && fullBase.CreationTime.After(behind.CreationTime)

	// The newest backup at every destination must restore: its chain must reach a full there. A
	// set deleted from the middle of the chain (clean --force, or by hand) breaks it; an
	// incremental that continues from the newest backup could not be restored either. So a broken
	// chain is handled as a pruned source is: a full of a newer full candidate, or nothing until
	// one exists. Either way the warning names the missing link.
	var brokenWarning string
	brokenIdx, missing := firstBrokenChain(destBackups)
	if missing != nil {
		brokenWarning = fmt.Sprintf(
			"The chain of %s at %s is broken: it holds no backup of %s, so its newest backup, %s, and every "+
				"incremental after it cannot be restored. ",
			jobInfo.VolumeName, destinationLabel(brokenIdx, len(destBackups)), missing.Name,
			destBackups[brokenIdx][0].BaseSnapshot.Name,
		)
	}

	if ageExceeded && hasNewerFullBase {
		p := Plan{Action: PlanFull, Base: *fullBase, Reason: reasonWindowElapsed}
		if missing != nil {
			p.Warnings = []string{brokenWarning + "The full backup of " + fullBase.Name + " starts a new chain."}
		}
		return p, nil
	}
	fullDue := ageExceeded

	if missing != nil {
		if !hasNewerFullBase {
			return Plan{
				Action: PlanNoop, Source: *missing, Reason: reasonBrokenChain, FullDue: fullDue,
				Warnings: []string{brokenWarning + "No full backup candidate newer than the last backup exists yet: " +
					"nothing is sent until one does. To start a new chain now, send a full backup of a newer snapshot."},
			}, nil
		}
		return Plan{
			Action: PlanFull, Base: *fullBase, Source: *missing, Reason: reasonBrokenChain,
			Warnings: []string{brokenWarning + "The full backup of " + fullBase.Name + " starts a new chain."},
		}, nil
	}

	// Otherwise perform an incremental up to the incremental-candidate snapshot.
	if incrBase == nil {
		return Plan{}, fmt.Errorf("no snapshots found matching the incremental backup criteria")
	}
	if lastNotEqual {
		return Plan{}, fmt.Errorf("want to do an incremental backup but last incremental backup at destinations do not match")
	}
	if !incrBase.CreationTime.After(lastBackup[0].CreationTime) {
		return Plan{Action: PlanNoop, Reason: reasonNothingNewer, FullDue: fullDue}, nil
	}

	// The incremental source (the most recent backup) must still exist locally
	// to send from it. If it has been pruned, fall back to a full backup of a
	// full-candidate newer than that backup; an older one would only re-send
	// data already backed up, so wait for the next one instead. The copy keeps
	// the destination state untouched: the existence check flags the source as
	// a bookmark if only a bookmark of it is left. zfs send cannot send from a
	// bookmark with -R or -I: with those, a source left only as a
	// bookmark is a pruned source too.
	source := *lastBackup[0]
	present := validateSnapShotExistsFromSnaps(&source, snapshots, true)
	onlyBookmark := present && source.Bookmark && needsSnapshotSource(jobInfo)
	if !present || onlyBookmark {
		if fullBase == nil {
			return Plan{}, fmt.Errorf("no snapshots found matching the full backup criteria")
		}
		var warnings []string
		if onlyBookmark {
			warnings = []string{fmt.Sprintf(
				"Only a bookmark of %s, the last backup, is left on the pool, and zfs send cannot send from a "+
					"bookmark with -R or -I: no incremental can be sent from it.",
				source.Name,
			)}
		}
		if !fullBase.CreationTime.After(source.CreationTime) {
			return Plan{Action: PlanNoop, Source: source, Reason: reasonSourcePruned, FullDue: fullDue, Warnings: warnings}, nil
		}
		return Plan{Action: PlanFull, Base: *fullBase, Source: source, Reason: reasonSourcePruned, Warnings: warnings}, nil
	}
	if err := sameStreamKind(jobInfo, destBackups); err != nil {
		return Plan{}, err
	}
	return Plan{Action: PlanIncremental, Base: *incrBase, Source: source, Reason: reasonNewerCandidate, FullDue: fullDue}, nil
}

// firstBrokenChain returns the first destination whose newest backup does not restore, and the
// snapshot whose backup is missing there (see brokenLink). The snapshot is nil when every chain
// reaches a full.
func firstBrokenChain(destBackups [][]*files.JobInfo) (int, *files.SnapshotInfo) {
	for idx, backups := range destBackups {
		if missing := brokenLink(backups); missing != nil {
			return idx, missing
		}
	}
	return 0, nil
}

// sameStreamKind fails when the incremental jobInfo plans would continue a chain whose last
// backup at any destination was sent with the other --raw setting: a chain keeps one stream
// kind, and only a full may change it. The incremental's source is each destination's newest
// backup (the planner has already checked that they agree).
func sameStreamKind(jobInfo *files.JobInfo, destBackups [][]*files.JobInfo) error {
	for _, backups := range destBackups {
		if len(backups) > 0 {
			if err := MixedChainError(jobInfo.VolumeName, backups[0], jobInfo.Raw); err != nil {
				return err
			}
		}
	}
	return nil
}

// MixedChainError is the error for an incremental, sent with raw or not, that continues from
// the backup source: nil when both have the same stream kind. Every place that refuses a mixed
// chain uses it, so the wording (and its "mix raw and plaintext" substring) is the same.
func MixedChainError(volume string, source *files.JobInfo, raw bool) error {
	if source.Raw == raw {
		return nil
	}
	return fmt.Errorf(
		"cannot continue the chain of %s: the backup of %s was sent %s, this run is %s; it would mix raw and plaintext "+
			"streams. Run %s, or start a new chain with --full",
		volume, source.BaseSnapshot.Name, withRaw(source.Raw), withRaw(raw), withRaw(source.Raw),
	)
}

func withRaw(raw bool) string {
	if raw {
		return "with --raw"
	}
	return "without --raw"
}

// checkLastBackupsOnPool fails with ErrLastBackupInFuture when a destination's last backup is
// dated after every snapshot on the pool and is not on it: no snapshot would ever look newer, so
// every run would quietly send nothing. Another host writing the same prefix and dataset name, a
// forged manifest or a wrong clock leaves such a backup; so does a pool or capture older than the
// destination (a zfs rollback, a stale --snapshots capture). snapshots is newest-first.
func checkLastBackupsOnPool(volume string, snapshots []files.SnapshotInfo, lastBackup []*files.SnapshotInfo) error {
	for idx, last := range lastBackup {
		if last == nil || !last.CreationTime.After(snapshots[0].CreationTime) {
			continue
		}
		if onPool := *last; validateSnapShotExistsFromSnaps(&onPool, snapshots, true) {
			continue
		}
		dest := "the destination's"
		if len(lastBackup) > 1 {
			dest = fmt.Sprintf("destination #%d's", idx+1)
		}
		return fmt.Errorf(
			"%w: %s last backup of %s is %s, dated %s, after every snapshot on the pool; "+
				"is another host writing this prefix, is the pool or capture older than the destination "+
				"(a zfs rollback, a stale --snapshots capture), or was the clock wrong?",
			ErrLastBackupInFuture, dest, volume, last.Name, last.CreationTime.UTC().Format(time.RFC3339),
		)
	}
	return nil
}

// Step is one simulated run.
type Step struct {
	At        time.Time            // zero for a single next run
	Snapshots []files.SnapshotInfo // the pool at this run, newest-first
	Plan      Plan
	Err       error
}

// Simulation is the outcome of running a Scenario.
type Simulation struct {
	Scenario *Scenario
	Steps    []Step
	// Initial and Manifests are the destinations' backups before the first
	// run and after the last one, newest-first per destination.
	Initial   [][]*files.JobInfo
	Manifests [][]*files.JobInfo
	// Created is every snapshot the pool held or the schedule took, oldest
	// first, including those taken and pruned while no run happened.
	Created []files.SnapshotInfo
}

// record adds the snapshots not recorded yet to sim.Created.
func (sim *Simulation) record(snapshots []files.SnapshotInfo, known map[string]bool) {
	for i := len(snapshots) - 1; i >= 0; i-- { // newest-first in, oldest-first out
		if id := snapshotID(snapshots[i]); !known[id] {
			known[id] = true
			sim.Created = append(sim.Created, snapshots[i])
		}
	}
}

// Run simulates the scenario. Without Until it plans a single run against the
// snapshots as given. Otherwise it plans a run every Every from From (default:
// an hour after the newest snapshot) to Until, letting the Schedule take and
// prune snapshots in between; runs inside a Skips range do not happen, but the
// pool keeps changing. Every backup a run sends is added to each destination,
// so the next run sees it the way getBackupsForTarget would read it back.
func (s *Scenario) Run() *Simulation {
	sim := &Simulation{Scenario: s, Initial: cloneDestinations(s.DestBackups)}
	snapshots := s.Snapshots
	known := make(map[string]bool)
	sim.record(snapshots, known)
	dest := cloneDestinations(s.DestBackups)
	if s.Until.IsZero() {
		sim.Steps = append(sim.Steps, s.run(time.Time{}, snapshots, dest))
		sim.Manifests = dest
		return sim
	}

	// The schedule takes the snapshots due after the newest one given.
	taken := s.From
	if len(snapshots) > 0 {
		taken = snapshots[0].CreationTime
	}
	for at := s.firstRun(); !at.After(s.Until); at = s.nextRun(at) {
		if s.Schedule != nil {
			var took []files.SnapshotInfo
			snapshots, took = s.Schedule.Advance(snapshots, taken, at)
			sim.record(took, known) // including those already pruned again
			if at.After(taken) {
				taken = at
			}
		}
		if s.skipped(at) {
			continue // the host was down: sanoid still ran, the cron job did not
		}
		sim.Steps = append(sim.Steps, s.run(at, visibleAt(snapshots, at, dest), dest))
	}
	sim.Manifests = dest
	return sim
}

// skipped reports whether at falls inside a Skips range, [From, Until).
func (s *Scenario) skipped(at time.Time) bool {
	for _, r := range s.Skips {
		if !at.Before(r.From) && at.Before(r.Until) {
			return true
		}
	}
	return false
}

// visibleAt drops the snapshots created after at from a newest-first list: a
// run cannot see them yet (from= may be earlier than the newest snapshot). It
// keeps those a destination already holds a backup of or from: the backup
// says the pool had them by then, and without them a from= earlier than the
// last backup would read that backup as one from the future.
func visibleAt(snapshots []files.SnapshotInfo, at time.Time, dest [][]*files.JobInfo) []files.SnapshotInfo {
	i := 0
	for i < len(snapshots) && snapshots[i].CreationTime.After(at) {
		i++
	}
	if i == 0 {
		return snapshots
	}
	var visible []files.SnapshotInfo
	for _, snap := range snapshots[:i] {
		if backedUp(dest, snap) {
			visible = append(visible, snap)
		}
	}
	return append(visible, snapshots[i:]...)
}

// backedUp reports whether a destination holds a backup of or from snap.
func backedUp(dest [][]*files.JobInfo, snap files.SnapshotInfo) bool {
	for _, backups := range dest {
		for _, m := range backups {
			if m.BaseSnapshot.Equal(&snap) || m.IncrementalSnapshot.Equal(&snap) {
				return true
			}
		}
	}
	return false
}

// run plans one run and records what it sends at every destination.
func (s *Scenario) run(at time.Time, snapshots []files.SnapshotInfo, dest [][]*files.JobInfo) Step {
	jobInfo := files.JobInfo{
		VolumeName:                s.Volume,
		Full:                      s.JobInfo.Full,
		Incremental:               s.JobInfo.Incremental,
		FullIfOlderThan:           s.JobInfo.FullIfOlderThan,
		SnapshotPrefix:            s.JobInfo.SnapshotPrefix,
		FullSnapshotSuffix:        s.JobInfo.FullSnapshotSuffix,
		IncrementalSnapshotSuffix: s.JobInfo.IncrementalSnapshotSuffix,
		Resume:                    s.JobInfo.Resume,
		Raw:                       s.JobInfo.Raw,
		Replication:               s.JobInfo.Replication,
		Properties:                s.JobInfo.Properties,
		Deduplication:             s.JobInfo.Deduplication,
		IntermediaryIncremental:   s.JobInfo.IntermediaryIncremental,
	}
	plan, err := planSmartAt(s.Clock(at), &jobInfo, snapshots, dest, s.Completable)
	if err == nil && plan.Action != PlanNoop {
		for i := range dest {
			m := manifestFor(s.Volume, plan)
			m.Raw = jobInfo.Raw
			dest[i] = addManifest(dest[i], m)
		}
	}
	return Step{At: at, Snapshots: snapshots, Plan: plan, Err: err}
}

// Clock is "now" for a run at at: the plan's wall clock (Now, or s.Now when set), or at when at
// is later. A simulated run in the future sees its own time; one in the past still sees the
// backups made since as dated in the past, not in the future.
func (s *Scenario) Clock(at time.Time) time.Time {
	now := s.Now
	if now.IsZero() {
		now = Now()
	}
	if at.After(now) {
		return at
	}
	return now
}

// manifestFor is the manifest of the backup a plan sends.
func manifestFor(volume string, p Plan) *files.JobInfo {
	m := &files.JobInfo{VolumeName: volume, BaseSnapshot: p.Base}
	if p.Action == PlanIncremental {
		m.IncrementalSnapshot = p.Source
	}
	return m
}

func (s *Scenario) firstRun() time.Time {
	switch {
	case !s.From.IsZero():
		return s.From
	case len(s.Snapshots) > 0:
		return s.Snapshots[0].CreationTime.Add(time.Hour)
	default:
		return s.Until
	}
}

// nextRun steps by Every (default 24h). Whole days are added on the calendar
// to the first run's wall-clock time, so a daily run keeps that time across
// DST changes, like cron. Stepping from the previous run instead would keep a
// run moved out of the spring-forward gap at its new time for good.
func (s *Scenario) nextRun(at time.Time) time.Time {
	every := s.every()
	if every%(24*time.Hour) != 0 {
		return at.Add(every)
	}
	first := s.firstRun().In(s.location())
	// at is a whole number of days after first, give or take a DST shift.
	days := int((at.Sub(first)+12*time.Hour)/(24*time.Hour)) + int(every/(24*time.Hour))
	return wallClock(first.Year(), first.Month(), first.Day()+days,
		first.Hour(), first.Minute(), first.Second(), first.Nanosecond(), s.location())
}

// wallClock is time.Date, except that a wall-clock time the spring-forward gap
// skips (02:30 in New York on the day clocks go from 02:00 to 03:00) is the
// same time read with the offset before the gap, 03:30 the new time: after the
// jump, as cron runs a job of the skipped hour, never before it. time.Date
// gives 01:30 there. A time the fall-back hour repeats is its first instance.
func wallClock(year int, month time.Month, day, hour, minute, sec, nsec int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, hour, minute, sec, nsec, loc)
	want := time.Date(year, month, day, hour, minute, sec, nsec, time.UTC)
	got := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	if got.Before(want) {
		t = t.Add(want.Sub(got))
	}
	return t
}

func (s *Scenario) every() time.Duration {
	if s.Every <= 0 {
		return 24 * time.Hour
	}
	return s.Every
}

// addManifest records a backup at a destination. Sending the same backup again
// writes the same object names, so it replaces the earlier manifest. The list
// stays newest-first, with ties in the order they were sent.
func addManifest(manifests []*files.JobInfo, m *files.JobInfo) []*files.JobInfo {
	out := make([]*files.JobInfo, 0, len(manifests)+1)
	for _, existing := range manifests {
		if existing.BaseSnapshot.Name != m.BaseSnapshot.Name || existing.IncrementalSnapshot.Name != m.IncrementalSnapshot.Name {
			out = append(out, existing)
		}
	}
	i := 0
	for i < len(out) && !out[i].BaseSnapshot.CreationTime.Before(m.BaseSnapshot.CreationTime) {
		i++
	}
	return append(out[:i], append([]*files.JobInfo{m}, out[i:]...)...)
}

func cloneDestinations(dests [][]*files.JobInfo) [][]*files.JobInfo {
	out := make([][]*files.JobInfo, len(dests))
	for i, dest := range dests {
		out[i] = make([]*files.JobInfo, len(dest))
		for j, m := range dest {
			c := *m
			out[i][j] = &c
		}
	}
	return out
}

// WriteText renders the simulation in the golden format: one line per run,
// with consecutive identical no-ops or errors collapsed into one line, then
// the check results.
func (sim *Simulation) WriteText(w io.Writer, violations []Violation) error {
	var b strings.Builder
	for i := 0; i < len(sim.Steps); {
		step := sim.Steps[i]
		j := i + 1
		if step.Err != nil || step.Plan.Action == PlanNoop {
			for j < len(sim.Steps) && sameOutcome(sim.Steps[j], step) {
				j++
			}
		}
		at := sim.formatTime(step.At)
		if j-i > 1 {
			at += ".." + sim.formatTime(sim.Steps[j-1].At)
		}
		fmt.Fprintf(&b, "%s  %s\n", at, step.describe(j-i))
		for _, w := range step.Plan.Warnings {
			fmt.Fprintf(&b, "    warning: %s\n", w)
		}
		i = j
	}

	if len(violations) == 0 {
		b.WriteString("checks: OK\n")
	} else {
		fmt.Fprintf(&b, "checks: %d violation(s)\n", len(violations))
		for _, v := range violations {
			b.WriteString("  " + v.Check)
			if !v.At.IsZero() {
				b.WriteString("  " + sim.formatTime(v.At))
			}
			b.WriteString("  " + v.Detail + "\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes every run, the destinations after the last run and the
// check results as one JSON document.
func (sim *Simulation) WriteJSON(w io.Writer, violations []Violation) error {
	type snapshot struct {
		Name     string    `json:"name"`
		Creation time.Time `json:"creation"`
		Bookmark bool      `json:"bookmark,omitempty"`
	}
	loc := sim.Scenario.location()
	ref := func(s files.SnapshotInfo) *snapshot {
		if s.Name == "" {
			return nil
		}
		return &snapshot{Name: s.Name, Creation: s.CreationTime.In(loc), Bookmark: s.Bookmark}
	}
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		t = t.In(loc)
		return &t
	}
	type run struct {
		At     *time.Time `json:"at,omitempty"` // omitted for a single next run
		Action string     `json:"action"`       // full, incremental, noop or error
		Base   *snapshot  `json:"base,omitempty"`
		Source *snapshot  `json:"source,omitempty"`
		// The pruned incremental source (reason source-pruned), or the snapshot whose backup is
		// missing from the chain (reason broken-chain).
		MissingSource *snapshot `json:"missingSource,omitempty"`
		Reason        string    `json:"reason,omitempty"`
		FullDue       bool      `json:"fullDue,omitempty"` // a full is due but waits for a newer candidate
		Error         string    `json:"error,omitempty"`
		Warnings      []string  `json:"warnings,omitempty"`
		Snapshots     int       `json:"snapshots"` // on the pool at this run
	}
	type backup struct {
		Base   *snapshot `json:"base"`
		Source *snapshot `json:"source,omitempty"`
	}
	type violation struct {
		Check  string     `json:"check"`
		At     *time.Time `json:"at,omitempty"`
		Detail string     `json:"detail"`
	}
	out := struct {
		Volume       string      `json:"volume"`
		Runs         []run       `json:"runs"`
		Destinations [][]backup  `json:"destinations"` // after the last run, newest-first
		Violations   []violation `json:"violations"`
	}{Volume: sim.Scenario.Volume, Runs: []run{}, Destinations: [][]backup{}, Violations: []violation{}}

	for _, st := range sim.Steps {
		r := run{At: at(st.At), Action: string(st.Plan.Action), Reason: st.Plan.Reason, FullDue: st.Plan.FullDue, Snapshots: len(st.Snapshots)}
		switch {
		case st.Err != nil:
			r = run{At: r.At, Action: "error", Error: st.Err.Error(), Snapshots: r.Snapshots}
		case st.Plan.Action == PlanIncremental:
			r.Base, r.Source = ref(st.Plan.Base), ref(st.Plan.Source)
		default:
			r.Base, r.MissingSource = ref(st.Plan.Base), ref(st.Plan.Source)
		}
		r.Warnings = st.Plan.Warnings
		out.Runs = append(out.Runs, r)
	}
	for _, dest := range sim.Manifests {
		backups := []backup{}
		for _, m := range dest {
			backups = append(backups, backup{Base: ref(m.BaseSnapshot), Source: ref(m.IncrementalSnapshot)})
		}
		out.Destinations = append(out.Destinations, backups)
	}
	for _, v := range violations {
		out.Violations = append(out.Violations, violation{Check: v.Check, At: at(v.At), Detail: v.Detail})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// describe renders a step's outcome; n > 1 counts collapsed identical steps.
func (st Step) describe(n int) string {
	count := ""
	if n > 1 {
		count = fmt.Sprintf(" x%d", n)
	}
	switch {
	case st.Err != nil:
		return fmt.Sprintf("ERROR%s  %v", count, st.Err)
	case st.Plan.Action == PlanFull:
		return fmt.Sprintf("FULL  %s  %s", st.Plan.Base.Name, st.Plan.Reason)
	case st.Plan.Action == PlanIncremental:
		return fmt.Sprintf("INCR  %s  from %s  %s%s", st.Plan.Base.Name, snapshotRef(st.Plan.Source), st.Plan.Reason, st.fullDue())
	default:
		return fmt.Sprintf("NOOP%s  %s%s", count, st.Plan.Reason, st.fullDue())
	}
}

// fullDue marks a plan whose full is due but waits for a newer full candidate.
func (st Step) fullDue() string {
	if st.Plan.FullDue {
		return "  full-due"
	}
	return ""
}

func sameOutcome(a, b Step) bool {
	if strings.Join(a.Plan.Warnings, "\n") != strings.Join(b.Plan.Warnings, "\n") {
		return false
	}
	if a.Err != nil || b.Err != nil {
		return a.Err != nil && b.Err != nil && a.Err.Error() == b.Err.Error()
	}
	return a.Plan.Action == PlanNoop && b.Plan.Action == PlanNoop && a.Plan.Reason == b.Plan.Reason &&
		a.Plan.FullDue == b.Plan.FullDue
}

// snapshotRef names a snapshot, marking bookmarks with '#'.
func snapshotRef(s files.SnapshotInfo) string {
	if s.Bookmark {
		return "#" + s.Name
	}
	return s.Name
}

// formatTime renders a run time in the scenario's location; the zero time is
// the single "next" run.
func (sim *Simulation) formatTime(t time.Time) string {
	if t.IsZero() {
		return "next"
	}
	return t.In(sim.Scenario.location()).Format(time.RFC3339)
}

// partialSet returns the newest backup set at some destinations when every other destination
// has exactly the backups that precede it there, i.e. lacks just that set. Otherwise nil.
func partialSet(destBackups [][]*files.JobInfo) *files.JobInfo {
	var leader []*files.JobInfo
	for _, backups := range destBackups {
		if len(backups) > len(leader) {
			leader = backups
		}
	}
	if len(leader) == 0 {
		return nil
	}
	behind := false
	for _, backups := range destBackups {
		switch {
		case sameSets(backups, leader):
		case sameSets(backups, leader[1:]):
			behind = true
		default:
			return nil
		}
	}
	if !behind {
		return nil
	}
	return leader[0]
}

// snapshotName is s's name, or "none".
func snapshotName(s *files.SnapshotInfo) string {
	if s == nil {
		return "none"
	}
	return s.Name
}

// sameSets reports whether a and b list the same backup sets in the same order.
func sameSets(a, b []*files.JobInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		//nolint:gosec // G602: len(a) == len(b) above
		if !a[i].BaseSnapshot.Equal(&b[i].BaseSnapshot) || !a[i].IncrementalSnapshot.Equal(&b[i].IncrementalSnapshot) {
			return false
		}
	}
	return true
}

// hasFullOf reports whether backups include a full backup of snapshot.
func hasFullOf(backups []*files.JobInfo, snapshot *files.SnapshotInfo) bool {
	for _, b := range backups {
		if b.IncrementalSnapshot.Name == "" && b.BaseSnapshot.Equal(snapshot) {
			return true
		}
	}
	return false
}
