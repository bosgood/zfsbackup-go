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

package zfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// ZFSPath is the path to the zfs binary
var (
	ZFSPath = "zfs"
)

// SanoidTimeLayout is the time layout of the timestamp sanoid embeds in its
// snapshot names, as in autosnap_2026-09-01_00:00:00_monthly.
const SanoidTimeLayout = "2006-01-02_15:04:05"

var sanoidTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}_\d{2}:\d{2}:\d{2}`)

// GetCreationDate will use the zfs command to get and parse the creation datetime
// of the specified volume/snapshot
func GetCreationDate(ctx context.Context, target string) (time.Time, error) {
	rawTime, err := GetZFSProperty(ctx, "creation", target)
	if err != nil {
		return time.Time{}, err
	}
	epochTime, serr := strconv.ParseInt(rawTime, 10, 64)
	if serr != nil {
		return time.Time{}, serr
	}
	return time.Unix(epochTime, 0), nil
}

// GetSnapshotsAndBookmarks will retrieve all snapshots and bookmarks for the given target
func GetSnapshotsAndBookmarks(ctx context.Context, target string) ([]files.SnapshotInfo, error) {
	errB := new(bytes.Buffer)
	cmd := exec.CommandContext(
		ctx, ZFSPath, "list", "-H", "-d", "1", "-p", "-t", "snapshot,bookmark", "-r", "-o", "name,creation,type", "-S", "creation", target,
	)
	log.AppLogger.Debugf("Getting ZFS Snapshots with command \"%s\"", strings.Join(cmd.Args, " "))
	cmd.Stderr = errB
	rpipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("%s (%v)", strings.TrimSpace(errB.String()), err)
	}
	snapshots, perr := parseListOutput(rpipe)
	if perr != nil {
		// Drain the rest so zfs does not block on a full pipe before Wait.
		_, _ = io.Copy(io.Discard, rpipe)
	}
	err = cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("%s (%v)", strings.TrimSpace(errB.String()), err)
	}
	if perr != nil {
		return nil, perr
	}

	return snapshots, nil
}

// parseListOutput reads what `zfs list -H -p -o name,creation,type` prints:
// exactly three tab-separated fields per line. Unlike ParseSnapshotList it
// takes names verbatim, since ZFS names may contain spaces.
func parseListOutput(r io.Reader) ([]files.SnapshotInfo, error) {
	var snapshots []files.SnapshotInfo
	scanner := bufio.NewScanner(r)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("zfs list line %d: want name, creation and type, got %q", lineNo, scanner.Text())
		}
		creation, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("zfs list line %d: invalid creation %q", lineNo, fields[1])
		}
		snapInfo := files.SnapshotInfo{CreationTime: time.Unix(creation, 0)}
		switch fields[2] {
		case "bookmark":
			snapInfo.Name, snapInfo.Bookmark = fields[0][strings.Index(fields[0], "#")+1:], true
		case "snapshot":
			snapInfo.Name = fields[0][strings.Index(fields[0], "@")+1:]
		default:
			return nil, fmt.Errorf("zfs list line %d: unknown type %q", lineNo, fields[2])
		}
		snapshots = append(snapshots, snapInfo)
	}
	return snapshots, scanner.Err()
}

// ParseSnapshotList reads a snapshot listing as printed by
//
//	zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation <dataset>
//
// and returns the rows in input order. Each row is name[, creation[, type]],
// separated by tabs (or, when a row has no tabs, by spaces). The dataset prefix
// of the name (tank/data@, tank/data#) is optional, but the rows that have one
// must all name the same dataset. A row without a creation column takes its
// creation time from the sanoid timestamp in the name, read in loc (see
// SnapshotNameTime); a row without a type column is a bookmark when its name
// contains '#'. Blank lines and '#' comments are skipped.
//
// A listing that starts with '[' is JSON: an array of rows such as
// {"name": "tank/data@snap", "creation": 1788220800, "type": "snapshot"},
// which is `zfs list` output converted to JSON. Only name is required;
// creation is an epoch, as a number or a string, and other keys (used, refer,
// ...) are ignored.
func ParseSnapshotList(r io.Reader, loc *time.Location) ([]files.SnapshotInfo, error) {
	l, err := ParseListing(r, loc)
	if err != nil {
		return nil, err
	}
	return l.Snapshots, nil
}

// Listing is a parsed snapshot listing (see ParseSnapshotList).
type Listing struct {
	// Dataset is the dataset the rows name (tank/data for tank/data@snap),
	// or "" when every row is a bare name.
	Dataset string
	// Snapshots are the rows, in input order.
	Snapshots []files.SnapshotInfo
	// NameDated names the snapshots whose creation time came from their
	// sanoid name because the row had no creation column. The real creation
	// time is later than the name says, usually by seconds.
	NameDated []string
}

// ParseListing is ParseSnapshotList that also reports which dataset the rows
// name and which snapshots were dated from their names.
func ParseListing(r io.Reader, loc *time.Location) (*Listing, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var l *listing
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		l, err = parseJSONSnapshotList(trimmed, loc)
	} else {
		l, err = parseTextSnapshotList(data, loc)
	}
	if err != nil {
		return nil, err
	}
	return &Listing{Dataset: l.dataset, Snapshots: l.snapshots, NameDated: l.nameDated}, nil
}

func parseTextSnapshotList(data []byte, loc *time.Location) (*listing, error) {
	var l listing
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := StripComment(scanner.Text())
		if line == "" {
			continue
		}
		if err := l.addRow(line, loc); err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNo, err)
		}
	}
	return &l, scanner.Err()
}

// jsonRow is a row of a JSON snapshot listing. Keys match case-insensitively.
type jsonRow struct {
	Name     string          `json:"name"`
	Creation json.RawMessage `json:"creation"`
	Type     string          `json:"type"`
}

func parseJSONSnapshotList(data []byte, loc *time.Location) (*listing, error) {
	var rows []jsonRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf(`JSON listing: %v; want an array of rows such as {"name": "tank/data@snap"}`, err)
	}
	var l listing
	for i, row := range rows {
		if row.Name == "" {
			return nil, fmt.Errorf("row %d: no name", i+1)
		}
		// The epoch may be a number or a string; null or no key means none.
		creation := string(row.Creation)
		var s string
		if json.Unmarshal(row.Creation, &s) == nil {
			creation = s
		}
		if err := l.add(row.Name, creation, row.Type, loc); err != nil {
			return nil, fmt.Errorf("row %d: %v", i+1, err)
		}
	}
	return &l, nil
}

// listing collects the rows of a snapshot listing, which must all be of one
// dataset.
type listing struct {
	dataset   string // of the first row that names one
	snapshots []files.SnapshotInfo
	nameDated []string // snapshots dated from their names, for want of a creation column
}

// addRow adds a text row: name[, creation[, type]].
func (l *listing) addRow(line string, loc *time.Location) error {
	var fields []string
	if strings.Contains(line, "\t") {
		for _, field := range strings.Split(line, "\t") {
			if field = strings.TrimSpace(field); field != "" {
				fields = append(fields, field)
			}
		}
	} else {
		fields = strings.Fields(line)
	}
	if len(fields) > 3 {
		return fmt.Errorf("want name[, creation[, type]], got %q", line)
	}
	fields = append(fields, "", "") // creation and type are optional
	return l.add(fields[0], fields[1], fields[2], loc)
}

// add adds a row. An empty creation comes from the sanoid timestamp in the
// name; an empty kind makes a name with '#' a bookmark.
func (l *listing) add(name, creation, kind string, loc *time.Location) error {
	if i := strings.IndexAny(name, "@#"); i >= 0 {
		if l.dataset == "" {
			l.dataset = name[:i]
		} else if name[:i] != l.dataset {
			return fmt.Errorf("%s is not in %s like the rows before it: list one dataset", name, l.dataset)
		}
	}

	snapInfo := files.SnapshotInfo{Bookmark: strings.Contains(name, "#")}
	switch kind {
	case "":
	case "snapshot":
		snapInfo.Bookmark = false
	case "bookmark":
		snapInfo.Bookmark = true
	default:
		return fmt.Errorf("unknown type %q for %s", kind, name)
	}
	if snapInfo.Bookmark {
		snapInfo.Name = name[strings.Index(name, "#")+1:]
	} else {
		snapInfo.Name = name[strings.Index(name, "@")+1:]
	}

	if creation == "" {
		t, ok := SnapshotNameTime(snapInfo.Name, loc)
		if !ok {
			return fmt.Errorf("no creation time for %s: add a creation epoch or use a sanoid name", name)
		}
		snapInfo.CreationTime = t
		l.nameDated = append(l.nameDated, snapInfo.Name)
	} else {
		epoch, err := strconv.ParseInt(creation, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid creation epoch %q for %s", creation, name)
		}
		snapInfo.CreationTime = time.Unix(epoch, 0).In(loc)
	}
	l.snapshots = append(l.snapshots, snapInfo)
	return nil
}

// SnapshotNameTime returns the time embedded in a sanoid-style snapshot name
// (autosnap_2006-01-02_15:04:05_<period>), read in loc. ok is false when the
// name carries no such timestamp.
func SnapshotNameTime(name string, loc *time.Location) (time.Time, bool) {
	ts := sanoidTimestamp.FindString(name)
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(SanoidTimeLayout, ts, loc)
	return t, err == nil
}

// StripComment removes a '#' comment from a fixture line, along with
// surrounding whitespace. A '#' starts a comment at the beginning of the line
// or after whitespace; inside a name, as in tank/data#bookmark, it does not.
func StripComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			line = line[:i]
			break
		}
	}
	return strings.TrimSpace(line)
}

// GetZFSProperty will return the raw value returned by the "zfs get" command for
// the given property on the given target.
func GetZFSProperty(ctx context.Context, prop, target string) (string, error) {
	b := new(bytes.Buffer)
	errB := new(bytes.Buffer)
	cmd := exec.CommandContext(ctx, ZFSPath, "get", "-H", "-p", "-o", "value", prop, target)
	log.AppLogger.Debugf("Getting ZFS Property with command \"%s\"", strings.Join(cmd.Args, " "))
	cmd.Stdout = b
	cmd.Stderr = errB
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%s (%v)", strings.TrimSpace(errB.String()), err)
	}
	return strings.TrimSpace(b.String()), nil
}

// GetSnapshotGUID returns the guid of volume's snapshot (or bookmark) snap.
func GetSnapshotGUID(ctx context.Context, volume string, snap *files.SnapshotInfo) (string, error) {
	sep := "@"
	if snap.Bookmark {
		sep = "#"
	}
	return GetZFSProperty(ctx, "guid", volume+sep+snap.Name)
}

// GetZFSSendCommand will return the send command to use for the given JobInfo
func GetZFSSendCommand(ctx context.Context, j *files.JobInfo) *exec.Cmd {
	// Prepare the zfs send command
	zfsArgs := []string{"send"}

	if j.Replication {
		log.AppLogger.Infof("Enabling the replication (-R) flag on the send.")
		zfsArgs = append(zfsArgs, "-R")
	}

	if j.SkipMissing {
		log.AppLogger.Infof("Enabling the skip-missing (-s) flag on the send.")
		zfsArgs = append(zfsArgs, "-s")
	}

	if j.Deduplication {
		log.AppLogger.Infof("Enabling the deduplication (-D) flag on the send.")
		zfsArgs = append(zfsArgs, "-D")
	}

	if j.Properties {
		log.AppLogger.Infof("Enabling the properties (-p) flag on the send.")
		zfsArgs = append(zfsArgs, "-p")
	}

	if j.Compressor == files.ZfsCompressor {
		log.AppLogger.Infof("Enabling the compression (-c) flag on the send.")
		zfsArgs = append(zfsArgs, "-c")
	}

	if j.Raw {
		log.AppLogger.Infof("Enabling the raw (-w) flag on the send.")
		zfsArgs = append(zfsArgs, "-w")
	}

	if j.IncrementalSnapshot.Name != "" {
		incrementalName := j.IncrementalSnapshot.Name
		if j.IncrementalSnapshot.Bookmark {
			incrementalName = fmt.Sprintf("%s#%s", j.VolumeName, incrementalName)
		}

		if j.IntermediaryIncremental {
			log.AppLogger.Infof("Enabling an incremental stream with all intermediary snapshots (-I) on the send to snapshot %s", incrementalName)
			zfsArgs = append(zfsArgs, "-I", incrementalName)
		} else {
			log.AppLogger.Infof("Enabling an incremental stream (-i) on the send to snapshot %s", incrementalName)
			zfsArgs = append(zfsArgs, "-i", incrementalName)
		}
	}

	zfsArgs = append(zfsArgs, fmt.Sprintf("%s@%s", j.VolumeName, j.BaseSnapshot.Name))
	cmd := exec.CommandContext(ctx, ZFSPath, zfsArgs...)

	return cmd
}

// sendDryRunArgs builds the argv for a dry-run send by reusing the normal send
// command and injecting -n -P right after the "send" verb. exec.Command sets
// Args[0] to the binary and Args[1] to "send", so Args[2:] is everything after
// the verb.
func sendDryRunArgs(base *exec.Cmd) []string {
	return append([]string{"send", "-n", "-P"}, base.Args[2:]...)
}

// parseSendSizeEstimate pulls the byte count out of `zfs send -n -P` output,
// which contains a line of the form "size\t<bytes>".
func parseSendSizeEstimate(out string) (uint64, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "size" {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("could not parse size estimate from zfs send dry-run output")
}

// GetZFSSendDryRun estimates the number of bytes the configured send would
// stream, using zfs send's dry-run (-n) parseable (-P) output. It sends no data.
func GetZFSSendDryRun(ctx context.Context, j *files.JobInfo) (uint64, error) {
	cmd := exec.CommandContext(ctx, ZFSPath, sendDryRunArgs(GetZFSSendCommand(ctx, j))...)
	out := new(bytes.Buffer)
	errB := new(bytes.Buffer)
	cmd.Stdout = out
	cmd.Stderr = errB
	log.AppLogger.Debugf("Estimating ZFS send size with command \"%s\"", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("%s (%v)", strings.TrimSpace(errB.String()), err)
	}
	return parseSendSizeEstimate(out.String())
}

// GetZFSReceiveCommand will return the recv command to use for the given JobInfo
func GetZFSReceiveCommand(ctx context.Context, j *files.JobInfo) *exec.Cmd {
	// Prepare the zfs send command
	zfsArgs := []string{"receive"}

	if j.FullPath {
		log.AppLogger.Infof("Enabling the full path (-d) flag on the receive.")
		zfsArgs = append(zfsArgs, "-d")
	}

	if j.LastPath {
		log.AppLogger.Infof("Enabling the last path (-e) flag on the receive.")
		zfsArgs = append(zfsArgs, "-e")
	}

	if j.NotMounted {
		log.AppLogger.Infof("Enabling the not mounted (-u) flag on the receive.")
		zfsArgs = append(zfsArgs, "-u")
	}

	if j.Force {
		log.AppLogger.Infof("Enabling the forced rollback (-F) flag on the receive.")
		zfsArgs = append(zfsArgs, "-F")
	}

	if j.Origin != "" {
		log.AppLogger.Infof("Enabling the origin flag (-o) on the receive to %s", j.Origin)
		zfsArgs = append(zfsArgs, "-o", "origin="+j.Origin)
	}

	zfsArgs = append(zfsArgs, j.LocalVolume)
	cmd := exec.CommandContext(ctx, ZFSPath, zfsArgs...)

	return cmd
}
