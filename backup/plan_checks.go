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
	"sort"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// Violation is a broken invariant found by Simulation.Check.
type Violation struct {
	Check string
	// At is the run that broke the invariant; zero when it is not tied to a
	// run of a simulation over time.
	At     time.Time
	Detail string
}

// defaultChecks run on every simulation, in this order.
var defaultChecks = []struct {
	name string
	run  func(*Simulation) []Violation
}{
	{"no-errors", (*Simulation).checkNoErrors},
	{"chain-links", (*Simulation).checkChainLinks},
	{"source-present", (*Simulation).checkSourcePresent},
	{"no-duplicate-send", (*Simulation).checkNoDuplicateSend},
	{"no-orphan-full", (*Simulation).checkNoOrphanFull},
	{"full-cadence", (*Simulation).checkFullCadence},
	{"restore-depth", (*Simulation).checkRestoreDepth},
}

// The opt-in checks coverage:<suffix> and only:<suffix>.
const (
	coverageCheck = "coverage:"
	onlyCheck     = "only:"
)

func knownCheck(name string) bool {
	for _, prefix := range []string{coverageCheck, onlyCheck} {
		if strings.HasPrefix(name, prefix) {
			return len(name) > len(prefix)
		}
	}
	for _, c := range defaultChecks {
		if c.name == name {
			return true
		}
	}
	return false
}

// Check runs the default checks and then the opt-in checks named in checks
// (coverage:<suffix>, only:<suffix>). Naming a default check changes nothing.
func (sim *Simulation) Check(checks []string) []Violation {
	var found []Violation
	for _, c := range defaultChecks {
		found = append(found, c.run(sim)...)
	}
	for _, name := range checks {
		if suffix := strings.TrimPrefix(name, coverageCheck); suffix != name {
			found = append(found, sim.checkCoverage(name, suffix)...)
		}
		if suffix := strings.TrimPrefix(name, onlyCheck); suffix != name {
			found = append(found, sim.checkOnly(name, suffix)...)
		}
	}
	return found
}

// checkNoErrors: every run makes a decision; a planner error is what `send`
// would fail with. Consecutive runs failing the same way are one violation.
func (sim *Simulation) checkNoErrors() []Violation {
	var found []Violation
	for i := 0; i < len(sim.Steps); {
		st := sim.Steps[i]
		j := i + 1
		if st.Err == nil {
			i = j
			continue
		}
		for j < len(sim.Steps) && sameOutcome(sim.Steps[j], st) {
			j++
		}
		detail := st.Err.Error()
		if j-i > 1 {
			detail += fmt.Sprintf(" (%d runs, through %s)", j-i, sim.formatTime(sim.Steps[j-1].At))
		}
		found = append(found, Violation{Check: "no-errors", At: st.At, Detail: detail})
		i = j
	}
	return found
}

// checkChainLinks: an incremental can only be restored onto a backup of its
// source at the same destination.
func (sim *Simulation) checkChainLinks() []Violation {
	var found []Violation
	for d, manifests := range sim.Manifests {
		for i := len(manifests) - 1; i >= 0; i-- { // oldest first
			m := manifests[i]
			if m.IncrementalSnapshot.Name == "" || parentOf(manifests, m.IncrementalSnapshot) != nil {
				continue
			}
			found = append(found, Violation{
				Check: "chain-links",
				At:    sim.sentAt(m),
				Detail: fmt.Sprintf("%s%s cannot be restored: no backup of %s",
					sim.destination(d), describeBackup(m.BaseSnapshot, m.IncrementalSnapshot), m.IncrementalSnapshot.Name),
			})
		}
	}
	return found
}

// checkSourcePresent: an incremental can only be sent from a snapshot (or
// bookmark) that is still on the pool.
func (sim *Simulation) checkSourcePresent() []Violation {
	var found []Violation
	for _, st := range sim.Steps {
		if st.Err != nil || st.Plan.Action != PlanIncremental {
			continue
		}
		source := st.Plan.Source
		if !validateSnapShotExistsFromSnaps(&source, st.Snapshots, true) {
			found = append(found, Violation{
				Check:  "source-present",
				At:     st.At,
				Detail: fmt.Sprintf("%s: %s is not on the pool", describeBackup(st.Plan.Base, st.Plan.Source), source.Name),
			})
		}
	}
	return found
}

// checkNoDuplicateSend: a snapshot already backed up at a destination (as a
// full or as an incremental's target) is never sent there again: the data is
// there, and the same backup would even write the same objects. Completing a
// partial set sends nothing, so it is not judged; an explicit --full of a
// backed-up snapshot starts a new chain on purpose, so it is not one either.
func (sim *Simulation) checkNoDuplicateSend() []Violation {
	backedUp := make([]map[string]string, len(sim.Initial)) // per destination: base => how and when it was sent
	for d, dest := range sim.Initial {
		backedUp[d] = make(map[string]string)
		for _, m := range dest {
			backedUp[d][snapshotID(m.BaseSnapshot)] = describeBackup(m.BaseSnapshot, m.IncrementalSnapshot) + " before the first run"
		}
	}
	var found []Violation
	for _, st := range sim.Steps {
		if st.Err != nil || st.Plan.Action == PlanNoop || st.Plan.Reason == reasonCompletePartial {
			continue
		}
		m := manifestFor("", st.Plan)
		id := snapshotID(m.BaseSnapshot)
		for d := range backedUp {
			if earlier, dup := backedUp[d][id]; dup && st.Plan.Reason != reasonExplicitFull {
				found = append(found, Violation{
					Check: "no-duplicate-send",
					At:    st.At,
					Detail: fmt.Sprintf("%s%s re-sends %s, already backed up by %s",
						sim.destination(d), describeBackup(m.BaseSnapshot, m.IncrementalSnapshot), m.BaseSnapshot.Name, earlier),
				})
				continue
			}
			backedUp[d][id] = describeBackup(m.BaseSnapshot, m.IncrementalSnapshot) + " sent " + sim.formatTime(st.At)
		}
	}
	return found
}

// checkNoOrphanFull: the backup after a full chains from that full; otherwise
// the full starts a chain nothing continues.
func (sim *Simulation) checkNoOrphanFull() []Violation {
	var found []Violation
	var full *Step
	for i := range sim.Steps {
		st := &sim.Steps[i]
		if st.Err != nil || st.Plan.Action == PlanNoop {
			continue
		}
		if st.Plan.Action == PlanFull {
			full = st
			continue
		}
		if full != nil && !st.Plan.Source.Equal(&full.Plan.Base) {
			found = append(found, Violation{
				Check: "no-orphan-full",
				At:    st.At,
				Detail: fmt.Sprintf("%s skips FULL %s sent %s",
					describeBackup(st.Plan.Base, st.Plan.Source), full.Plan.Base.Name, sim.formatTime(full.At)),
			})
		}
		full = nil
	}
	return found
}

// checkFullCadence: with --fullIfOlderThan, a full never comes less than one
// window (minus the slack, see fullSlack) after the previous one, and never
// more than one window plus the slack after the previous full a run sent: a
// full is never overdue. The first full after the initial state may come any
// time later: it may be catching up, as may the first after a completed
// partial set. Without a known slack (no snapshot period to go by) there is
// nothing to judge.
func (sim *Simulation) checkFullCadence() []Violation {
	window := sim.Scenario.JobInfo.FullIfOlderThan
	slack := sim.fullSlack()
	if window < 0 || slack == 0 {
		return nil
	}

	var last *files.SnapshotInfo // base of the latest full
	lastSent := false            // whether a run sent it
	if len(sim.Initial) > 0 {
		for _, m := range sim.Initial[0] {
			if m.IncrementalSnapshot.Name == "" {
				base := m.BaseSnapshot
				last = &base
				break
			}
		}
	}
	var found []Violation
	overdue := false
	for _, st := range sim.Steps {
		if st.Err == nil && st.Plan.Action == PlanFull {
			// Completing a partial set sends a full that some destination already holds:
			// it is the latest full from here on, but not a new one to judge.
			if st.Plan.Reason == reasonCompletePartial {
				base := st.Plan.Base
				last, lastSent, overdue = &base, false, false
				continue
			}
			if last != nil {
				gap := st.Plan.Base.CreationTime.Sub(last.CreationTime)
				if gap < window-slack || (lastSent && gap > window+slack) {
					found = append(found, Violation{
						Check: "full-cadence",
						At:    st.At,
						Detail: fmt.Sprintf("FULL %s is %s after FULL %s, want %s ± %s",
							st.Plan.Base.Name, formatDays(gap), last.Name, formatDays(window), formatDays(slack)),
					})
				}
			}
			base := st.Plan.Base
			last, lastSent, overdue = &base, true, false
			continue
		}
		if last == nil || overdue || st.At.IsZero() || len(st.Snapshots) == 0 {
			continue
		}
		if age := st.Snapshots[0].CreationTime.Sub(last.CreationTime); age > window+slack {
			overdue = true
			found = append(found, Violation{
				Check:  "full-cadence",
				At:     st.At,
				Detail: fmt.Sprintf("no full since %s, %s ago, want one every %s ± %s", last.Name, formatDays(age), formatDays(window), formatDays(slack)),
			})
		}
	}
	return found
}

// checkRestoreDepth: restoring the newest backup never takes more incrementals
// than fit into one window (plus slack), so chains do not grow forever.
func (sim *Simulation) checkRestoreDepth() []Violation {
	window := sim.Scenario.JobInfo.FullIfOlderThan
	spacing := sim.incrementalSpacing()
	if window < 0 || spacing <= 0 || len(sim.Initial) == 0 {
		return nil
	}
	limit := int((window+sim.fullSlack()+spacing-1)/spacing) + 1

	var found []Violation
	dest := cloneDestinations(sim.Initial)[0]
	over := false
	for _, st := range sim.Steps {
		if st.Err != nil || st.Plan.Action == PlanNoop {
			continue
		}
		dest = addManifest(dest, manifestFor(sim.Scenario.Volume, st.Plan))
		depth := chainDepth(dest)
		if depth > limit && !over {
			found = append(found, Violation{
				Check:  "restore-depth",
				At:     st.At,
				Detail: fmt.Sprintf("restoring %s takes %d incrementals, want at most %d", dest[0].BaseSnapshot.Name, depth, limit),
			})
		}
		over = depth > limit
	}
	return found
}

// checkCoverage: every snapshot ending in suffix is backed up exactly once;
// in a simulation over time that includes every one the schedule took from
// the first scheduled run on, whether or not a run saw it (one taken and
// pruned during an outage was never sent either).
func (sim *Simulation) checkCoverage(name, suffix string) []Violation {
	seen := make(map[string]bool)
	var candidates []files.SnapshotInfo
	consider := func(s files.SnapshotInfo) {
		id := snapshotID(s)
		if !s.Bookmark && strings.HasSuffix(s.Name, suffix) && !seen[id] {
			seen[id] = true
			candidates = append(candidates, s)
		}
	}
	for _, s := range sim.Created {
		consider(s)
	}
	for _, st := range sim.Steps { // a Simulation built by hand may have no Created
		for _, s := range st.Snapshots {
			consider(s)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].CreationTime.Before(candidates[j].CreationTime)
	})
	// The horizon is the first SCHEDULED run: one inside a skip= range makes no
	// step, but the snapshots taken from then on were still due to be sent.
	var horizon time.Time
	switch {
	case sim.Scenario != nil && !sim.Scenario.Until.IsZero():
		horizon = sim.Scenario.firstRun()
	case len(sim.Steps) > 0 && !sim.Steps[0].At.IsZero():
		horizon = sim.Steps[0].At
	}

	var found []Violation
	for d, manifests := range sim.Manifests {
		for i := range candidates {
			backups := 0
			for _, m := range manifests {
				if m.BaseSnapshot.Equal(&candidates[i]) {
					backups++
				}
			}
			switch {
			case backups > 1:
				found = append(found, Violation{
					Check:  name,
					Detail: fmt.Sprintf("%s%s is the base of %d backups", sim.destination(d), candidates[i].Name, backups),
				})
			case backups == 0 && !horizon.IsZero() && !candidates[i].CreationTime.Before(horizon):
				found = append(found, Violation{
					Check:  name,
					Detail: fmt.Sprintf("%s%s is never backed up", sim.destination(d), candidates[i].Name),
				})
			}
		}
	}
	return found
}

// checkOnly: every backup a run sends is of a snapshot ending in suffix, so a
// job meant to send one period only (monthly-only) sends nothing else, which
// a forgotten --incrementalSnapshotSuffix would. Consecutive offending runs
// are one violation.
func (sim *Simulation) checkOnly(name, suffix string) []Violation {
	offends := func(st Step) bool {
		return st.Err == nil && st.Plan.Action != PlanNoop && !strings.HasSuffix(st.Plan.Base.Name, suffix)
	}
	var found []Violation
	for i := 0; i < len(sim.Steps); {
		st := sim.Steps[i]
		j := i + 1
		if !offends(st) {
			i = j
			continue
		}
		for j < len(sim.Steps) && offends(sim.Steps[j]) {
			j++
		}
		detail := describeBackup(st.Plan.Base, st.Plan.Source) + " is not of a snapshot ending in " + suffix
		if j-i > 1 {
			detail = fmt.Sprintf("%d backups of snapshots not ending in %s, %s through %s",
				j-i, suffix, describeBackup(st.Plan.Base, st.Plan.Source), sim.formatTime(sim.Steps[j-1].At))
		}
		found = append(found, Violation{Check: name, At: st.At, Detail: detail})
		i = j
	}
	return found
}

// fullSlack is how far a full may drift from the window: once the window has
// elapsed it waits for the next full candidate (one period of the full
// suffix, or else of the longest scheduled period) and then for the next run.
func (sim *Simulation) fullSlack() time.Duration {
	sc := sim.Scenario
	slack := periodLength(suffixPeriod(sc.JobInfo.FullSnapshotSuffix))
	if slack == 0 && sc.Schedule != nil && len(sc.Schedule.Periods) > 0 {
		slack = periodLength(sc.Schedule.Periods[0].Name)
	}
	if !sc.Until.IsZero() {
		slack += sc.every()
	}
	return slack
}

// incrementalSpacing is the least time between two incrementals: no closer
// than the snapshots the incremental suffix selects (or the most frequent
// scheduled ones), and no closer than the runs. 0 when unknown.
func (sim *Simulation) incrementalSpacing() time.Duration {
	sc := sim.Scenario
	spacing := minSpacing(suffixPeriod(sc.JobInfo.IncrementalSnapshotSuffix))
	if spacing == 0 && sc.Schedule != nil && len(sc.Schedule.Periods) > 0 {
		spacing = minSpacing(sc.Schedule.Periods[len(sc.Schedule.Periods)-1].Name)
	}
	if !sc.Until.IsZero() && sc.every() > spacing {
		spacing = sc.every()
	}
	return spacing
}

// suffixPeriod returns the sanoid period a suffix such as "_monthly" selects,
// or "".
func suffixPeriod(suffix string) string {
	if name := strings.TrimPrefix(suffix, "_"); periodLength(name) > 0 {
		return name
	}
	return ""
}

// minSpacing is the least time between two sanoid snapshots of a period.
func minSpacing(period string) time.Duration {
	switch period {
	case "monthly":
		return 28 * 24 * time.Hour
	case "yearly":
		return 365 * 24 * time.Hour
	}
	return periodLength(period)
}

// chainDepth counts the incrementals restoring the newest backup takes.
func chainDepth(manifests []*files.JobInfo) int {
	if len(manifests) == 0 {
		return 0
	}
	// Parents by base snapshot, chosen as parentOf does.
	parents := make(map[string]*files.JobInfo, len(manifests))
	for _, m := range manifests {
		id := snapshotID(m.BaseSnapshot)
		if p, ok := parents[id]; !ok || (m.IncrementalSnapshot.Name == "" && p.IncrementalSnapshot.Name != "") {
			parents[id] = m
		}
	}
	depth := 0
	for m := manifests[0]; m != nil && m.IncrementalSnapshot.Name != "" && depth < len(manifests); {
		depth++
		m = parents[snapshotID(m.IncrementalSnapshot)]
	}
	return depth
}

// snapshotID identifies a snapshot by name and creation time, as
// SnapshotInfo.Equal compares them.
func snapshotID(s files.SnapshotInfo) string {
	return fmt.Sprintf("%s@%d", s.Name, s.CreationTime.UnixNano())
}

// parentOf finds the backup an incremental from source restores onto,
// preferring a full backup as linkManifests does.
func parentOf(manifests []*files.JobInfo, source files.SnapshotInfo) *files.JobInfo {
	var parent *files.JobInfo
	for _, m := range manifests {
		if !m.BaseSnapshot.Equal(&source) {
			continue
		}
		if m.IncrementalSnapshot.Name == "" {
			return m
		}
		if parent == nil {
			parent = m
		}
	}
	return parent
}

// sentAt returns when the backup was last sent, or zero if it was already at
// the destination before the first run.
func (sim *Simulation) sentAt(m *files.JobInfo) time.Time {
	var at time.Time
	for _, st := range sim.Steps {
		if st.Err == nil && st.Plan.Action != PlanNoop {
			sent := manifestFor("", st.Plan)
			if sent.BaseSnapshot.Name == m.BaseSnapshot.Name && sent.IncrementalSnapshot.Name == m.IncrementalSnapshot.Name {
				at = st.At
			}
		}
	}
	return at
}

// destination labels violations when there is more than one destination.
func (sim *Simulation) destination(i int) string {
	if len(sim.Manifests) > 1 {
		return fmt.Sprintf("destination %d: ", i+1)
	}
	return ""
}

// describeBackup renders a backup as "FULL <base>" or "INCR <base> from <source>".
func describeBackup(base, source files.SnapshotInfo) string {
	if source.Name == "" {
		return "FULL " + base.Name
	}
	return fmt.Sprintf("INCR %s from %s", base.Name, snapshotRef(source))
}

// formatDays renders a duration in days and hours, e.g. 181d or 180d1h.
func formatDays(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	d = d.Round(time.Hour) // first, so 29d23h30m is 30d, not 29d24h
	days, hours := d/(24*time.Hour), d%(24*time.Hour)/time.Hour
	if hours == 0 {
		return fmt.Sprintf("%s%dd", sign, days)
	}
	return fmt.Sprintf("%s%dd%dh", sign, days, hours)
}
