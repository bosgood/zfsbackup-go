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
	// Location is the zone of sanoid name timestamps and of rendered times.
	// nil means UTC.
	Location *time.Location
	// Schedule is the sanoid policy applied between runs; nil leaves the pool
	// unchanged.
	Schedule *Schedule
	// From, Until and Every set the runs to simulate. A zero Until means a
	// single run against the snapshots as given ("next").
	From, Until time.Time
	Every       time.Duration
	Checks      []string // opt-in checks, e.g. coverage:_monthly
}

// LoadScenario reads a scenario directory:
//
//	flags          smart send flags, as passed to `zfsbackup send`
//	snapshots.txt  the pool's snapshots (see zfs.ParseSnapshotList)
//	manifests.txt  optional: backups already at the destinations (see ReadManifests)
//	schedule       optional: settings for ParseScheduleSpec
//
// Every file ignores blank lines and '#' comments.
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
	if err == nil {
		err = readScenarioFile(filepath.Join(dir, "snapshots.txt"), true, s.ReadSnapshots)
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
	snapshots, err := zfs.ParseSnapshotList(r, s.location())
	if err != nil {
		return err
	}
	sort.SliceStable(snapshots, func(i, j int) bool {
		return snapshots[i].CreationTime.After(snapshots[j].CreationTime)
	})
	s.Snapshots = snapshots
	return nil
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
		sortManifestsNewestFirst(dest)
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

// sortManifestsNewestFirst orders manifests the way getBackupsForTarget does.
func sortManifestsNewestFirst(manifests []*files.JobInfo) {
	sort.SliceStable(manifests, func(i, j int) bool {
		return manifests[i].BaseSnapshot.CreationTime.After(manifests[j].BaseSnapshot.CreationTime)
	})
}

func (s *Scenario) location() *time.Location {
	if s.Location == nil {
		return time.UTC
	}
	return s.Location
}

// ParseScheduleSpec applies scenario settings given as key=value items,
// separated by commas or newlines ('#' comments allowed):
//
//	policy=hourly=36,daily=30,monthly=3  sanoid retention (see ParseSchedule); hourly=36 alone works too
//	from=2026-09-24T01:00:00Z            first run; default: the newest snapshot + 1h
//	until=2027-10-01T01:00:00Z           last run
//	every=24h                            time between runs; default 24h
//	checks=coverage:_monthly             opt-in checks, in addition to the defaults
//	volume=tank/data                     the volume being backed up
//	location=America/New_York            zone of sanoid names and rendered times; default UTC
//
// Setting any of policy, from, until or every simulates runs over time, which
// requires until. Times are RFC 3339, or 2006-01-02[T15:04:05] in location.
func (s *Scenario) ParseScheduleSpec(spec string) error {
	var policy []string
	var from, until string
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

	if len(policy) > 0 {
		schedule, err := ParseSchedule(strings.Join(policy, ","))
		if err != nil {
			return err
		}
		s.Schedule = schedule
	}
	var err error
	if s.From, err = s.parseTime(from); err != nil {
		return fmt.Errorf("invalid from=%s: %v", from, err)
	}
	if s.Until, err = s.parseTime(until); err != nil {
		return fmt.Errorf("invalid until=%s: %v", until, err)
	}
	if (s.Schedule != nil || !s.From.IsZero() || s.Every != 0) && s.Until.IsZero() {
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

func (s *Scenario) parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.In(s.location()), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, s.location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("want RFC 3339, such as 2026-09-24T01:00:00Z")
}
