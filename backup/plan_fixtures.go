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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/zfs"
)

const defaultScenarioVolume = "tank/data"

// Scenario is everything the planner needs to project smart backups: the send
// flags, the pool's snapshots, what the destinations already hold and,
// optionally, how the pool changes between runs.
type Scenario struct {
	Volume      string
	JobInfo     files.JobInfo        // only the smart options are read
	Snapshots   []files.SnapshotInfo // newest-first, as zfs list -S creation returns them
	DestBackups [][]*files.JobInfo   // per destination, newest-first
	// CaptureDataset is the dataset a --snapshots listing names, when its
	// rows carry one; NameDated names the snapshots in it that were dated from
	// their sanoid names for want of a creation column (see zfs.ParseListing).
	CaptureDataset string
	NameDated      []string
	// Completable says a set missing at some destinations may be completed
	// there (see PartialSetCompletable); plan sets it from the destinations.
	Completable bool
	// Location is the zone of sanoid name timestamps and of rendered times.
	// nil means the host's zone (time.Local): sanoid names snapshots in local
	// time.
	Location *time.Location
	// Schedule is the sanoid policy applied between runs; nil leaves the pool
	// unchanged.
	Schedule *Schedule
	// From, Until and Every set the runs to simulate. A zero Until means a
	// single run against the snapshots as given ("next").
	From, Until time.Time
	Every       time.Duration
	Skips       []TimeRange // no run happens inside these ranges (the host was down)
	Checks      []string    // opt-in checks, e.g. coverage:_monthly
}

// TimeRange is a half-open interval [From, Until) of run times.
type TimeRange struct{ From, Until time.Time }

// LoadScenario reads a scenario directory:
//
//	flags           smart send flags, as passed to `zfsbackup send`
//	snapshots.txt   the pool's snapshots (see zfs.ParseSnapshotList)
//	snapshots.json  or the same as JSON, such as a link to a capture in testdata/zfs
//	manifests.txt   optional: backups already at the destinations (see ReadManifests)
//	schedule        optional: settings for ParseScheduleSpec
//
// Every file but snapshots.json ignores blank lines and '#' comments.
func LoadScenario(dir string) (*Scenario, error) {
	s := &Scenario{Volume: defaultScenarioVolume}

	if spec, err := os.ReadFile(filepath.Join(dir, "schedule")); err == nil {
		if err = s.ParseScheduleSpec(string(spec)); err != nil {
			return nil, fmt.Errorf("%s: %v", filepath.Join(dir, "schedule"), err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	err := readScenarioFile(filepath.Join(dir, "flags"), true, func(r io.Reader) error {
		j, err := ParseSmartFlags(r)
		s.JobInfo = j
		return err
	})
	var snapshots string
	if err == nil {
		snapshots, err = scenarioSnapshotsFile(dir)
	}
	if err == nil {
		err = readScenarioFile(snapshots, true, s.ReadSnapshots)
	}
	if err == nil {
		s.DestBackups = [][]*files.JobInfo{{}}
		err = readScenarioFile(filepath.Join(dir, "manifests.txt"), false, s.ReadManifests)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// scenarioSnapshotsFile returns the path of a scenario's snapshot listing:
// snapshots.json if the scenario has one, else snapshots.txt.
func scenarioSnapshotsFile(dir string) (string, error) {
	text, js := filepath.Join(dir, "snapshots.txt"), filepath.Join(dir, "snapshots.json")
	if _, err := os.Lstat(js); os.IsNotExist(err) {
		return text, nil
	}
	if _, err := os.Lstat(text); err == nil {
		return "", fmt.Errorf("%s: want snapshots.txt or snapshots.json, not both", dir)
	}
	return js, nil
}

func readScenarioFile(path string, required bool, read func(io.Reader) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) && !required {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	if err := read(f); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// AddSmartFlags registers the "smart" send options on fs, bound to j, so that
// `send`, `plan` and scenario fixtures parse them identically.
func AddSmartFlags(fs *pflag.FlagSet, j *files.JobInfo) {
	fs.BoolVar(
		&j.Full,
		"full",
		false,
		"set this flag to take a full backup of the specified volume using the most recent snapshot.",
	)
	fs.BoolVar(
		&j.Incremental,
		"increment",
		false,
		"set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target.",
	)
	fs.StringVar(
		&j.SnapshotPrefix,
		"snapshotPrefix",
		"",
		"Only consider snapshots starting with the given snapshot prefix",
	)
	fs.DurationVar(
		&j.FullIfOlderThan,
		"fullIfOlderThan",
		-1*time.Minute,
		"set this flag to do an incremental backup of the most recent snapshot from the most recent snapshot found in the target unless the "+
			"it's been greater than the time specified in this flag, then do a full backup.",
	)
	fs.StringVar(
		&j.FullSnapshotSuffix,
		"fullSnapshotSuffix",
		"",
		"When set, full backups (including those triggered by fullIfOlderThan) are taken from the newest snapshot whose name ends with this "+
			"suffix (e.g. \"_monthly\"). Use this to anchor fulls on long-lived snapshots that outlive the fullIfOlderThan window.",
	)
	fs.StringVar(
		&j.IncrementalSnapshotSuffix,
		"incrementalSnapshotSuffix",
		"",
		"When set, incremental backups target the newest snapshot whose name ends with this suffix (e.g. \"_daily\"), ignoring more frequent "+
			"snapshots (e.g. hourly) that would otherwise be picked and pruned before the next run.",
	)
}

// ValidateSmartOptions checks that exactly one of the mutually exclusive smart
// options (--full, --increment, --fullIfOlderThan) is set.
func ValidateSmartOptions(j *files.JobInfo) error {
	set := 0
	if j.Full {
		set++
	}
	if j.Incremental {
		set++
	}
	if j.FullIfOlderThan != -1*time.Minute {
		set++
	}
	switch {
	case set == 0:
		return errors.New("no \"smart\" option set: use one of --full, --increment or --fullIfOlderThan")
	case set > 1:
		return errors.New("please specify only one \"smart\" option at a time")
	}
	return nil
}

// ParseSmartFlags parses smart send flags (see AddSmartFlags), any number per
// line, as they would be passed to `zfsbackup send`.
func ParseSmartFlags(r io.Reader) (files.JobInfo, error) {
	var args []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		args = append(args, strings.Fields(zfs.StripComment(scanner.Text()))...)
	}
	if err := scanner.Err(); err != nil {
		return files.JobInfo{}, err
	}

	var j files.JobInfo
	fs := pflag.NewFlagSet("flags", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	AddSmartFlags(fs, &j)
	if err := fs.Parse(args); err != nil {
		return j, err
	}
	if fs.NArg() > 0 {
		return j, fmt.Errorf("unexpected argument %q: only smart send flags are allowed", fs.Arg(0))
	}
	return j, ValidateSmartOptions(&j)
}

// ReadSnapshots sets the scenario's snapshots from a listing (see
// zfs.ParseSnapshotList), sorted newest-first. The sort is stable, so a
// listing captured with `zfs list -S creation` keeps its exact order,
// including the order of snapshots that share a creation second.
func (s *Scenario) ReadSnapshots(r io.Reader) error {
	l, err := zfs.ParseListing(r, s.location())
	if err != nil {
		return err
	}
	s.Snapshots = l.Snapshots
	s.CaptureDataset = l.Dataset
	s.NameDated = l.NameDated
	s.sortSnapshotsNewestFirst()
	return nil
}

func (s *Scenario) sortSnapshotsNewestFirst() {
	sort.SliceStable(s.Snapshots, func(i, j int) bool {
		return s.Snapshots[i].CreationTime.After(s.Snapshots[j].CreationTime)
	})
}

// AdoptCreationTimes dates the name-dated snapshots (NameDated) from the
// backups at the destinations: a snapshot that some manifest names, as its
// base or its source, takes the creation time recorded there. send compares
// snapshots by name and creation time, so without this a snapshot dated from
// its name looks pruned next to its own backup, which was taken seconds later.
// It returns how many snapshots changed. Call it after DestBackups are set.
func (s *Scenario) AdoptCreationTimes() int {
	if len(s.NameDated) == 0 {
		return 0
	}
	recorded := make(map[string]time.Time)
	for _, dest := range s.DestBackups {
		for _, m := range dest {
			for _, snap := range []files.SnapshotInfo{m.BaseSnapshot, m.IncrementalSnapshot} {
				if snap.Name != "" {
					recorded[snap.Name] = snap.CreationTime
				}
			}
		}
	}
	nameDated := make(map[string]bool, len(s.NameDated))
	for _, name := range s.NameDated {
		nameDated[name] = true
	}
	adopted := 0
	for i := range s.Snapshots {
		snap := &s.Snapshots[i]
		if t, ok := recorded[snap.Name]; ok && nameDated[snap.Name] && !t.Equal(snap.CreationTime) {
			snap.CreationTime = t.In(s.location())
			adopted++
		}
	}
	if adopted > 0 {
		s.sortSnapshotsNewestFirst()
	}
	return adopted
}

// ReadManifests sets what the destinations already hold. Each line is one
// backup, in any order: a full backup is `<base>` and an incremental one is
// `<source> to <base>`. A line of `---` starts the next destination. A name
// may be followed by its creation epoch; otherwise its creation time comes from
// the scenario's snapshots or, failing that, from the sanoid timestamp in the
// name, so pruned snapshots need no entry in the snapshot list. Call
// ReadSnapshots first.
func (s *Scenario) ReadManifests(r io.Reader) error {
	dests := [][]*files.JobInfo{{}}
	scanner := bufio.NewScanner(r)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := zfs.StripComment(scanner.Text())
		switch line {
		case "":
			continue
		case "---":
			dests = append(dests, []*files.JobInfo{})
			continue
		}
		manifest, err := s.parseManifest(line)
		if err != nil {
			return fmt.Errorf("line %d: %v", lineNo, err)
		}
		dests[len(dests)-1] = append(dests[len(dests)-1], manifest)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, dest := range dests {
		sortBackupsNewestFirst(dest)
	}
	s.DestBackups = dests
	return nil
}

func (s *Scenario) parseManifest(line string) (*files.JobInfo, error) {
	fields := strings.Fields(line)
	manifest := &files.JobInfo{VolumeName: s.Volume}
	baseFields := fields
	for i, field := range fields {
		if field != "to" {
			continue
		}
		source, err := s.resolveSnapshot(fields[:i])
		if err != nil {
			return nil, err
		}
		manifest.IncrementalSnapshot = source
		baseFields = fields[i+1:]
		break
	}
	base, err := s.resolveSnapshot(baseFields)
	if err != nil {
		return nil, err
	}
	manifest.BaseSnapshot = base
	return manifest, nil
}

// resolveSnapshot turns `name [epoch]` into a SnapshotInfo.
func (s *Scenario) resolveSnapshot(fields []string) (files.SnapshotInfo, error) {
	if len(fields) == 0 || len(fields) > 2 {
		return files.SnapshotInfo{}, fmt.Errorf("want `<name> [epoch]`, got %q", strings.Join(fields, " "))
	}
	name := fields[0]
	snap := files.SnapshotInfo{Bookmark: strings.Contains(name, "#")}
	if snap.Bookmark {
		snap.Name = name[strings.Index(name, "#")+1:]
	} else {
		snap.Name = name[strings.Index(name, "@")+1:]
	}

	if len(fields) == 2 {
		epoch, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return files.SnapshotInfo{}, fmt.Errorf("invalid creation epoch %q for %s", fields[1], name)
		}
		snap.CreationTime = time.Unix(epoch, 0).In(s.location())
		return snap, nil
	}
	for i := range s.Snapshots {
		if s.Snapshots[i].Name == snap.Name {
			snap.CreationTime = s.Snapshots[i].CreationTime
			return snap, nil
		}
	}
	if t, ok := zfs.SnapshotNameTime(snap.Name, s.location()); ok {
		snap.CreationTime = t
		return snap, nil
	}
	return files.SnapshotInfo{}, fmt.Errorf("no creation time for %s: it is not in the snapshot list, so add its epoch", name)
}

// Zone is the zone sanoid names are read in and times are rendered in.
func (s *Scenario) Zone() *time.Location {
	return s.location()
}

func (s *Scenario) location() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// ParseScheduleSpec applies scenario settings given as key=value items,
// separated by commas or newlines ('#' comments allowed):
//
//	policy=hourly=36,daily=30,monthly=3  sanoid retention (see ParseSchedule); hourly=36 alone works too
//	snapshot-delay=3m                    how long after a boundary sanoid takes its snapshots; default 0
//	from=2026-09-24T01:00:00Z            first run; default: the newest snapshot + 1h
//	until=2027-10-01T01:00:00Z           last run
//	every=24h                            time between runs; default 24h
//	skip=2026-12-24..2027-01-03          no runs from the 24th through the 3rd (an outage); repeatable.
//	                                     A date means the whole day, a time means that instant.
//	checks=coverage:_monthly             opt-in checks, in addition to the defaults
//	volume=tank/data                     the volume being backed up
//	location=America/New_York            zone of sanoid names and rendered times; default the host's
//
// Setting any of policy, from, until or every simulates runs over time, which
// requires until. Times are RFC 3339, or 2006-01-02[T15:04:05] in location.
// Keys may come in any order: times are parsed once the whole spec is read,
// so location= applies to every one of them.
func (s *Scenario) ParseScheduleSpec(spec string) error {
	var policy []string
	var from, until string
	var skips []string
	var delay time.Duration
	listKey := ""
	for _, line := range strings.Split(spec, "\n") {
		for _, item := range strings.Split(zfs.StripComment(line), ",") {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			key, value, ok := strings.Cut(item, "=")
			if !ok {
				if listKey != "checks" {
					return fmt.Errorf("want key=value, got %q", item)
				}
				s.Checks = append(s.Checks, item)
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			listKey = key
			switch key {
			case "policy":
				policy = append(policy, value)
			case "snapshot-delay":
				d, err := time.ParseDuration(value)
				if err != nil || d < 0 {
					return fmt.Errorf("invalid snapshot-delay=%s: want a duration such as 3m", value)
				}
				delay = d
			case "checks":
				s.Checks = append(s.Checks, value)
			case "from":
				from = value
			case "until":
				until = value
			case "every":
				every, err := time.ParseDuration(value)
				if err != nil || every <= 0 {
					return fmt.Errorf("invalid every=%s: want a positive duration such as 24h", value)
				}
				s.Every = every
			case "skip":
				skips = append(skips, value)
			case "volume":
				s.Volume = value
			case "location":
				loc, err := time.LoadLocation(value)
				if err != nil {
					return fmt.Errorf("invalid location=%s: %v", value, err)
				}
				s.Location = loc
			default:
				if periodLength(key) == 0 {
					return fmt.Errorf("unknown setting %q", key)
				}
				policy = append(policy, item)
			}
		}
	}

	for _, check := range s.Checks {
		if !knownCheck(check) {
			return fmt.Errorf("unknown check %q", check)
		}
	}
	if len(policy) > 0 {
		schedule, err := ParseSchedule(strings.Join(policy, ","))
		if err != nil {
			return err
		}
		schedule.Delay = delay
		s.Schedule = schedule
	} else if delay != 0 {
		return errors.New("snapshot-delay needs a policy")
	}
	var err error
	if s.From, err = s.parseTime(from); err != nil {
		return fmt.Errorf("invalid from=%s: %v", from, err)
	}
	if s.Until, err = s.parseTime(until); err != nil {
		return fmt.Errorf("invalid until=%s: %v", until, err)
	}
	for _, value := range skips {
		r, err := s.parseSkip(value)
		if err != nil {
			return fmt.Errorf("invalid skip=%s: %v", value, err)
		}
		s.Skips = append(s.Skips, r)
	}
	if (s.Schedule != nil || !s.From.IsZero() || s.Every != 0 || len(s.Skips) > 0) && s.Until.IsZero() {
		return errors.New("simulating runs over time needs until=")
	}
	if !s.From.IsZero() && s.Until.Before(s.From) {
		return errors.New("until is before from")
	}
	if s.Schedule != nil {
		s.Schedule.Location = s.location()
	}
	return nil
}

// parseSkip reads `<from>..<until>` into a TimeRange. A date-only end covers
// its whole day; an end with a time is inclusive of that instant.
func (s *Scenario) parseSkip(value string) (TimeRange, error) {
	first, last, ok := strings.Cut(value, "..")
	if !ok {
		return TimeRange{}, errors.New("want <from>..<until>")
	}
	var r TimeRange
	from, _, err := s.parseTimeDetail(first)
	if err != nil || from.IsZero() {
		return TimeRange{}, err
	}
	until, dateOnly, err := s.parseTimeDetail(last)
	if err != nil || until.IsZero() {
		return TimeRange{}, err
	}
	if until.Before(from) {
		return TimeRange{}, errors.New("the range ends before it starts")
	}
	r.From = from
	if dateOnly {
		r.Until = until.AddDate(0, 0, 1)
	} else {
		r.Until = until.Add(time.Nanosecond)
	}
	return r, nil
}

func (s *Scenario) parseTime(value string) (time.Time, error) {
	t, _, err := s.parseTimeDetail(value)
	return t, err
}

// parseTimeDetail parses a time and reports whether it was a bare date.
func (s *Scenario) parseTimeDetail(value string) (t time.Time, dateOnly bool, err error) {
	if value == "" {
		return time.Time{}, false, nil
	}
	if t, err = time.Parse(time.RFC3339, value); err == nil {
		return t.In(s.location()), false, nil
	}
	if t, err = time.ParseInLocation("2006-01-02T15:04:05", value, s.location()); err == nil {
		return t, false, nil
	}
	if t, err = time.ParseInLocation("2006-01-02", value, s.location()); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, errors.New("want RFC 3339, such as 2026-09-24T01:00:00Z")
}
