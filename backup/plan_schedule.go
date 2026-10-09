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
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/zfs"
)

// sanoidPeriods lists the periods sanoid snapshots, longest first, with the
// length sanoid uses when pruning them.
var sanoidPeriods = []struct {
	name   string
	length time.Duration
}{
	{"yearly", 8766 * time.Hour}, // 365.25 days
	{"monthly", 31 * 24 * time.Hour},
	{"weekly", 7 * 24 * time.Hour},
	{"daily", 24 * time.Hour},
	{"hourly", time.Hour},
}

// periodLength is the length of a sanoid period, or 0 if name is not one.
func periodLength(name string) time.Duration {
	for _, p := range sanoidPeriods {
		if p.name == name {
			return p.length
		}
	}
	return 0
}

// Period is a sanoid snapshot period and its retention count.
type Period struct {
	Name string // yearly, monthly, weekly, daily or hourly
	Keep int
}

// Schedule simulates sanoid taking and pruning autosnap_ snapshots of one
// dataset, with sanoid's default times: hourly at :00, daily at 00:00, weekly
// on Monday, monthly on the 1st and yearly on January 1st.
type Schedule struct {
	Periods  []Period       // longest first
	Location *time.Location // zone of the period boundaries and snapshot names; nil means UTC
	// Delay is how long after a boundary sanoid takes the snapshot: its cron
	// minute plus the time zfs takes, 2-4 minutes on the captured pool. The
	// creation time and the name carry it, so a run at the boundary itself
	// does not see the snapshot yet.
	Delay time.Duration

	// Snapshots taken at the same boundary (the 1st of a month at midnight
	// brings a monthly, a daily and an hourly) are listed newest-first in
	// tieOrder, tieGap apart. Sanoid guarantees neither; tests vary both to
	// show that the planner does not depend on them.
	tieOrder []string
	tieGap   time.Duration
}

// ParseSchedule parses a sanoid retention policy such as
// "hourly=36,daily=30,monthly=3". A count of 0 takes no snapshots of that
// period and prunes the existing ones, as sanoid does.
func ParseSchedule(policy string) (*Schedule, error) {
	keep := make(map[string]int)
	for _, item := range strings.Split(policy, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, count, ok := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		if !ok {
			return nil, fmt.Errorf("policy: want period=count, got %q", item)
		}
		if periodLength(name) == 0 {
			return nil, fmt.Errorf("policy: unknown period %q, want yearly, monthly, weekly, daily or hourly", name)
		}
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("policy: invalid count %q for %s", count, name)
		}
		if _, dup := keep[name]; dup {
			return nil, fmt.Errorf("policy: %s is set twice", name)
		}
		keep[name] = n
	}

	s := &Schedule{}
	for _, p := range sanoidPeriods {
		if n, ok := keep[p.name]; ok {
			s.Periods = append(s.Periods, Period{Name: p.name, Keep: n})
		}
	}
	if len(s.Periods) == 0 {
		return nil, errors.New("policy: no periods given")
	}
	return s, nil
}

// String renders the policy the way ParseSchedule reads it.
func (s *Schedule) String() string {
	items := make([]string, 0, len(s.Periods))
	for _, p := range s.Periods {
		items = append(items, fmt.Sprintf("%s=%d", p.Name, p.Keep))
	}
	return strings.Join(items, ",")
}

// Advance returns the pool as of time to: snaps plus every snapshot sanoid
// takes at a period boundary in (from, to], pruned as sanoid would prune at
// to. It also returns the snapshots it took, before pruning, so that those
// taken and destroyed between two runs are known. Both are newest-first; snaps
// is not modified.
func (s *Schedule) Advance(snaps []files.SnapshotInfo, from, to time.Time) (pool, taken []files.SnapshotInfo) {
	type boundary struct {
		at      time.Time
		periods []string // in tie order
	}
	var boundaries []*boundary
	byTime := make(map[int64]*boundary)
	for _, name := range s.tieOrderOrDefault() {
		p, ok := s.period(name)
		if !ok || p.Keep == 0 {
			continue
		}
		for _, t := range s.boundaries(p, from, to) {
			b := byTime[t.UnixNano()]
			if b == nil {
				b = &boundary{at: t}
				byTime[t.UnixNano()] = b
				boundaries = append(boundaries, b)
			}
			b.periods = append(b.periods, p.Name)
		}
	}

	for _, b := range boundaries {
		for i, name := range b.periods {
			creation := b.at.Add(s.Delay + time.Duration(len(b.periods)-1-i)*s.tieGap)
			taken = append(taken, files.SnapshotInfo{
				Name:         fmt.Sprintf("autosnap_%s_%s", creation.In(s.location()).Format(zfs.SanoidTimeLayout), name),
				CreationTime: creation,
			})
		}
	}
	newestFirst := func(snaps []files.SnapshotInfo) {
		// Stable, so snapshots with equal creation times keep the order above.
		sort.SliceStable(snaps, func(i, j int) bool {
			return snaps[i].CreationTime.After(snaps[j].CreationTime)
		})
	}
	newestFirst(taken)
	pool = append(append([]files.SnapshotInfo(nil), snaps...), taken...)
	newestFirst(pool)
	return s.prune(pool, to), taken
}

// boundaries returns the times in (from, to] at which sanoid snapshots period
// p, oldest first. A zero from starts at boundaries so old that pruning would
// remove them anyway, which keeps it cheap.
func (s *Schedule) boundaries(p Period, from, to time.Time) []time.Time {
	length := periodLength(p.Name)
	if from.IsZero() {
		from = to.Add(-time.Duration(p.Keep+2) * length)
	}

	f := from.In(s.location())
	midnight := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, s.location())
	t, next := midnight, func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	switch p.Name {
	case "hourly":
		t = time.Date(f.Year(), f.Month(), f.Day(), f.Hour(), 0, 0, 0, s.location())
		next = func(t time.Time) time.Time { return t.Add(time.Hour) }
	case "weekly":
		t = midnight.AddDate(0, 0, -((int(f.Weekday()) + 6) % 7)) // Monday
		next = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	case "monthly":
		t = time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, s.location())
		next = func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
	case "yearly":
		t = time.Date(f.Year(), time.January, 1, 0, 0, 0, 0, s.location())
		next = func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }
	}

	var out []time.Time
	for ; !t.After(to); t = next(t) {
		if t.After(from) {
			out = append(out, t)
		}
	}
	return out
}

// prune applies sanoid's retention rule at time now: a snapshot of a period is
// destroyed once it is older than Keep periods, oldest first, but never while
// Keep or fewer snapshots of that period remain. Only autosnap_ snapshots are
// pruned; bookmarks and other snapshots are left alone. snaps is newest-first.
func (s *Schedule) prune(snaps []files.SnapshotInfo, now time.Time) []files.SnapshotInfo {
	destroyed := make(map[int]bool)
	for _, p := range s.Periods {
		var mine []int // indices of this period's snapshots, newest-first
		for i := range snaps {
			if !snaps[i].Bookmark && strings.HasPrefix(snaps[i].Name, "autosnap_") && strings.HasSuffix(snaps[i].Name, "_"+p.Name) {
				mine = append(mine, i)
			}
		}
		expiry := now.Add(-time.Duration(p.Keep) * periodLength(p.Name))
		left := len(mine)
		for k := len(mine) - 1; k >= 0 && left > p.Keep; k-- {
			if !snaps[mine[k]].CreationTime.Before(expiry) {
				break
			}
			destroyed[mine[k]] = true
			left--
		}
	}

	kept := make([]files.SnapshotInfo, 0, len(snaps)-len(destroyed))
	for i := range snaps {
		if !destroyed[i] {
			kept = append(kept, snaps[i])
		}
	}
	return kept
}

func (s *Schedule) period(name string) (Period, bool) {
	for _, p := range s.Periods {
		if p.Name == name {
			return p, true
		}
	}
	return Period{}, false
}

func (s *Schedule) tieOrderOrDefault() []string {
	if s.tieOrder != nil {
		return s.tieOrder
	}
	names := make([]string, 0, len(sanoidPeriods))
	for _, p := range sanoidPeriods {
		names = append(names, p.name)
	}
	return names
}

func (s *Schedule) location() *time.Location {
	if s.Location == nil {
		return time.UTC
	}
	return s.Location
}
