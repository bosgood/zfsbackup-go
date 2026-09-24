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

// Package fakezfs is a pure-Go stand-in for the zfs command, so tests can run
// every zfs call zfsbackup makes without ZFS. A test binary turns into the
// fake when it runs with FAKEZFS=1 (see RunIfRequested): point zfs.ZFSPath
// (or --zfsPath) at os.Args[0] and every zfs invocation lands in Main.
//
// Configuration is by environment:
//
//	FAKEZFS_SNAPSHOTS      snapshot fixture in the format zfs.ParseSnapshotList reads; rows
//	                       with a dataset prefix belong to that dataset, rows without one to
//	                       every dataset; bare sanoid names are read in UTC
//	FAKEZFS_STREAM_BYTES   bytes `zfs send` writes (default 65536)
//	FAKEZFS_DRYRUN_OUTPUT  replaces what `zfs send -n -P` prints
//	FAKEZFS_LOG            file every invocation's arguments are appended to, one line each
//
// Only the invocations zfs/zfs.go issues are understood; anything else exits
// 1 with its arguments on stderr, so an unexpected call fails loudly.
package fakezfs

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/zfs"
)

const defaultStreamBytes = 65536

// RunIfRequested turns the current process into the fake when FAKEZFS=1: it
// runs Main with the process's arguments and exits. Call it first in TestMain.
func RunIfRequested() {
	if os.Getenv("FAKEZFS") != "1" {
		return
	}
	os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// Main runs the fake with args (the zfs arguments, without the program name)
// and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := logInvocation(args); err != nil {
		fmt.Fprintf(stderr, "fakezfs: %v\n", err)
		return 1
	}
	var err error
	switch {
	case len(args) == 0:
		err = errUnexpected
	case args[0] == "list":
		err = list(args[1:], stdout)
	case args[0] == "get":
		err = get(args[1:], stdout)
	case args[0] == "send":
		err = send(args[1:], stdout)
	default:
		err = errUnexpected
	}
	if errors.Is(err, errUnexpected) {
		fmt.Fprintf(stderr, "fakezfs: unexpected invocation: zfs %s\n", strings.Join(args, " "))
		return 1
	} else if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

var errUnexpected = errors.New("unexpected invocation")

// list: list -H -d 1 -p -t snapshot,bookmark -r -o name,creation,type -S creation <dataset>
func list(args []string, stdout io.Writer) error {
	dataset, ok := trailingArg(args, "-H", "-d", "1", "-p", "-t", "snapshot,bookmark", "-r", "-o", "name,creation,type", "-S", "creation")
	if !ok {
		return errUnexpected
	}
	snaps, err := datasetSnapshots(dataset)
	if err != nil {
		return err
	}
	for _, s := range snaps {
		kind, sep := "snapshot", "@"
		if s.Bookmark {
			kind, sep = "bookmark", "#"
		}
		if _, err := fmt.Fprintf(stdout, "%s%s%s\t%d\t%s\n", dataset, sep, s.Name, s.CreationTime.Unix(), kind); err != nil {
			return err
		}
	}
	return nil
}

// get: get -H -p -o value creation <dataset>[@snapshot|#bookmark]
func get(args []string, stdout io.Writer) error {
	target, ok := trailingArg(args, "-H", "-p", "-o", "value", "creation")
	if !ok {
		return errUnexpected
	}
	dataset := target
	if i := strings.IndexAny(target, "@#"); i >= 0 {
		dataset = target[:i]
	}
	snaps, err := datasetSnapshots(dataset)
	if err != nil {
		return err
	}
	if dataset == target {
		_, err = fmt.Fprintln(stdout, 1)
		return err
	}
	s, err := lookup(dataset, target, snaps)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, s.CreationTime.Unix())
	return err
}

// send: send [-n] [-P] [-RsDpcw...] [-i|-I <source>] <dataset>@<snapshot>
func send(args []string, stdout io.Writer) error {
	var dryRun, parsable bool
	var source string
	for len(args) > 1 {
		flag := args[0]
		if len(flag) < 2 || flag[0] != '-' {
			return errUnexpected
		}
		args = args[1:]
		for _, c := range flag[1:] {
			switch c {
			case 'n':
				dryRun = true
			case 'P':
				parsable = true
			case 'R', 's', 'D', 'p', 'c', 'w', 'L', 'e', 'b':
			case 'i', 'I':
				if source != "" || len(args) < 2 {
					return errUnexpected
				}
				source, args = args[0], args[1:]
			default:
				return errUnexpected
			}
		}
	}
	if len(args) != 1 || !strings.Contains(args[0], "@") {
		return errUnexpected
	}
	target := args[0]
	dataset := target[:strings.Index(target, "@")]
	snaps, err := datasetSnapshots(dataset)
	if err != nil {
		return err
	}
	if _, err = lookup(dataset, target, snaps); err != nil {
		return err
	}
	if source != "" {
		ref := source
		if !strings.ContainsAny(ref, "@#") {
			ref = "@" + ref
		}
		if ref[0] == '@' || ref[0] == '#' {
			ref = dataset + ref
		}
		if _, err = lookup(dataset, ref, snaps); err != nil {
			return err
		}
	}

	size, err := streamBytes()
	if err != nil {
		return err
	}
	if dryRun {
		if !parsable {
			return nil
		}
		if out, ok := os.LookupEnv("FAKEZFS_DRYRUN_OUTPUT"); ok {
			_, err = io.WriteString(stdout, out)
			return err
		}
		if source == "" {
			_, err = fmt.Fprintf(stdout, "full\t%s\t%d\nsize\t%d\n", target, size, size)
		} else {
			_, err = fmt.Fprintf(stdout, "incremental\t%s\t%s\t%d\nsize\t%d\n", source, target, size, size)
		}
		return err
	}

	// A stream that differs for every (source, target) pair, reproducibly.
	h := fnv.New64a()
	_, _ = io.WriteString(h, source+"\x00"+target)
	_, err = io.CopyN(stdout, rand.New(rand.NewSource(int64(h.Sum64()))), size) // nolint:gosec // not for security
	return err
}

// trailingArg matches args against the fixed arguments want followed by one
// more argument, which it returns.
func trailingArg(args []string, want ...string) (string, bool) {
	if len(args) != len(want)+1 {
		return "", false
	}
	for i := range want {
		if args[i] != want[i] {
			return "", false
		}
	}
	return args[len(want)], true
}

// datasetSnapshots returns the fixture's snapshots and bookmarks of dataset,
// newest-first, like `zfs list -S creation`.
func datasetSnapshots(dataset string) ([]files.SnapshotInfo, error) {
	path := os.Getenv("FAKEZFS_SNAPSHOTS")
	if path == "" {
		return nil, errors.New("fakezfs: FAKEZFS_SNAPSHOTS is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fakezfs: %v", err)
	}
	var rows strings.Builder
	exists := false
	for _, line := range strings.Split(string(data), "\n") {
		line = zfs.StripComment(line)
		if line == "" {
			continue
		}
		name := line
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			name = line[:i]
		} else if i := strings.IndexByte(line, ' '); i >= 0 {
			name = line[:i]
		}
		if i := strings.IndexAny(name, "@#"); i >= 0 && name[:i] != dataset {
			continue
		}
		exists = true
		rows.WriteString(line + "\n")
	}
	if !exists {
		return nil, fmt.Errorf("cannot open '%s': dataset does not exist", dataset)
	}
	snaps, err := zfs.ParseSnapshotList(strings.NewReader(rows.String()), time.UTC)
	if err != nil {
		return nil, fmt.Errorf("fakezfs: %s: %v", path, err)
	}
	sort.SliceStable(snaps, func(i, j int) bool {
		return snaps[i].CreationTime.After(snaps[j].CreationTime)
	})
	return snaps, nil
}

// lookup finds dataset@snapshot or dataset#bookmark.
func lookup(dataset, ref string, snaps []files.SnapshotInfo) (files.SnapshotInfo, error) {
	i := strings.IndexAny(ref, "@#")
	if i < 0 || ref[:i] != dataset {
		return files.SnapshotInfo{}, fmt.Errorf("cannot open '%s': dataset does not exist", ref)
	}
	bookmark, name := ref[i] == '#', ref[i+1:]
	for _, s := range snaps {
		if s.Name == name && s.Bookmark == bookmark {
			return s, nil
		}
	}
	return files.SnapshotInfo{}, fmt.Errorf("cannot open '%s': dataset does not exist", ref)
}

func streamBytes() (int64, error) {
	value := os.Getenv("FAKEZFS_STREAM_BYTES")
	if value == "" {
		return defaultStreamBytes, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("fakezfs: invalid FAKEZFS_STREAM_BYTES %q", value)
	}
	return n, nil
}

func logInvocation(args []string) error {
	path := os.Getenv("FAKEZFS_LOG")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(f, strings.Join(args, " ")); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
