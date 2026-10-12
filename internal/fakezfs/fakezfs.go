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
//	FAKEZFS_SNAPSHOTS      snapshot fixture in the text format zfs.ParseSnapshotList reads
//	                       (not JSON); rows with a dataset prefix belong to that dataset,
//	                       rows without one to every dataset; bare sanoid names are read in UTC
//	FAKEZFS_STREAM_BYTES   bytes `zfs send` writes (default 65536)
//	FAKEZFS_FAIL_AFTER_BYTES  when set, `zfs send` writes at most this many bytes, then prints
//	                       "cannot send: I/O error" and exits 1, like a send that dies mid-stream
//	FAKEZFS_STREAM_SALT    when set, changes the bytes `zfs send` writes (not the snapshots'
//	                       guids), like a snapshot whose contents changed under the same name
//	FAKEZFS_RECEIVE_LOG    file `zfs receive` appends "<bytes> <sha256 hex>" of its stdin to
//
// `zfs receive` also adds the snapshot it received to FAKEZFS_SNAPSHOTS, as real zfs lists it
// after a receive. It finds which snapshot the stream carries by comparing it with what `zfs send`
// writes for each snapshot in the fixture (with this FAKEZFS_STREAM_SALT). A stream that matches
// none, such as one written by a shim, adds nothing, unless FAKEZFS_RECEIVE_UNKNOWN names it:
//
//	FAKEZFS_RECEIVE_UNKNOWN  file of sent snapshots (dataset@name), one per line; a receive of
//	                       a stream that matches no fixture snapshot takes the first line
//	FAKEZFS_DRYRUN_OUTPUT  replaces what `zfs send -n -P` prints
//	FAKEZFS_LOG            file every invocation's arguments are appended to, one line each
//
// Only the invocations zfs/zfs.go issues are understood; anything else exits
// 1 with its arguments on stderr, so an unexpected call fails loudly.
package fakezfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	case args[0] == "receive":
		err = receive(args[1:], stdin)
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
	// A snapshot or bookmark as the target lists only itself, as real zfs does.
	ref := ""
	if i := strings.IndexAny(dataset, "@#"); i >= 0 {
		dataset, ref = dataset[:i], dataset
	}
	snaps, err := datasetSnapshots(dataset)
	if err != nil {
		return err
	}
	if ref != "" {
		s, lerr := lookup(dataset, ref, snaps)
		if lerr != nil {
			return lerr
		}
		snaps = []files.SnapshotInfo{s}
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

// get: get -H -p -o value creation|guid <dataset>[@snapshot|#bookmark]. A snapshot's guid is
// derived from its name and creation time, so a snapshot recreated under the same name gets a
// new one; a bookmark has its snapshot's guid.
func get(args []string, stdout io.Writer) error {
	if len(args) != 6 || (args[4] != "creation" && args[4] != "guid") {
		return errUnexpected
	}
	prop := args[4]
	target, ok := trailingArg(args, "-H", "-p", "-o", "value", prop)
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
	if prop == "guid" {
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%s@%s\x00%d", dataset, s.Name, s.CreationTime.Unix())
		_, err = fmt.Fprintln(stdout, h.Sum64())
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
	dataset, _, _ := strings.Cut(target, "@")
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

	salt := os.Getenv("FAKEZFS_STREAM_SALT")
	if value, ok := os.LookupEnv("FAKEZFS_FAIL_AFTER_BYTES"); ok {
		failAfter, perr := strconv.ParseInt(value, 10, 64)
		if perr != nil || failAfter < 0 {
			return fmt.Errorf("fakezfs: invalid FAKEZFS_FAIL_AFTER_BYTES %q", value)
		}
		if failAfter < size {
			if _, err = io.CopyN(stdout, StreamSalted(source, target, salt, size), failAfter); err != nil {
				return err
			}
			return errors.New("cannot send: I/O error")
		}
	}
	_, err = io.CopyN(stdout, StreamSalted(source, target, salt, size), size)
	return err
}

// Stream returns the size bytes `zfs send` writes for an incremental from source (or a full
// when source is "") to target: a stream that differs for every pair, reproducibly.
func Stream(source, target string, size int64) io.Reader {
	return StreamSalted(source, target, "", size)
}

// StreamSalted is Stream as `zfs send` writes it with FAKEZFS_STREAM_SALT=salt.
func StreamSalted(source, target, salt string, size int64) io.Reader {
	h := fnv.New64a()
	_, _ = io.WriteString(h, source+"\x00"+target)
	if salt != "" {
		_, _ = io.WriteString(h, "\x00"+salt)
	}
	return io.LimitReader(rand.New(rand.NewSource(int64(h.Sum64()))), size) // nolint:gosec // not for security
}

// receive: receive [-d] [-e] [-u] [-F] [-o origin=<snapshot>] <dataset> - consumes the stream,
// records its length and hash in FAKEZFS_RECEIVE_LOG, and adds the snapshot it carries to
// FAKEZFS_SNAPSHOTS. A snapshot that is there already is an error, as with real zfs.
func receive(args []string, stdin io.Reader) error {
	var fullPath, lastPath bool
	for len(args) > 1 {
		switch args[0] {
		case "-d":
			fullPath = true
		case "-e":
			lastPath = true
		case "-u", "-F":
		case "-o":
			if len(args) < 3 {
				return errUnexpected
			}
			args = args[1:]
		default:
			return errUnexpected
		}
		args = args[1:]
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errUnexpected
	}
	h := sha256.New()
	var head bytes.Buffer
	n, err := io.Copy(io.MultiWriter(h, &prefixWriter{buf: &head, max: streamHeadBytes}), stdin)
	if err != nil {
		return err
	}
	sum := h.Sum(nil)
	if err = recordReceived(args[0], fullPath, lastPath, head.Bytes(), n, sum); err != nil {
		return err
	}
	path := os.Getenv("FAKEZFS_RECEIVE_LOG")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(f, "%d %s\n", n, hex.EncodeToString(sum)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// streamHeadBytes is how much of a received stream receive compares with each candidate.
const streamHeadBytes = 64

// prefixWriter keeps the first max bytes written to it.
type prefixWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// recordReceived finds the snapshot of the fixture whose stream (full, or incremental from
// another snapshot or bookmark of its dataset, as `zfs send` names the source) is the n bytes
// that start with head and hash to sum, and adds it to FAKEZFS_SNAPSHOTS under the dataset
// zfs receive writes to (receivedDataset).
func recordReceived(target string, fullPath, lastPath bool, head []byte, n int64, sum []byte) error {
	path := os.Getenv("FAKEZFS_SNAPSHOTS")
	if path == "" || n == 0 { // every candidate starts with nothing
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("fakezfs: %v", err)
	}
	salt := os.Getenv("FAKEZFS_STREAM_SALT")
	for _, dataset := range fixtureDatasets(string(data)) {
		snaps, err := datasetSnapshots(dataset)
		if err != nil {
			return err
		}
		for _, snap := range snaps {
			if snap.Bookmark {
				continue
			}
			sent := dataset + "@" + snap.Name
			sources := []string{""}
			for _, s := range snaps {
				switch {
				case s.Bookmark:
					sources = append(sources, dataset+"#"+s.Name)
				case s.Name != snap.Name:
					sources = append(sources, s.Name)
				}
			}
			for _, source := range sources {
				if sameStream(StreamSalted(source, sent, salt, n), head, sum) {
					return addSnapshot(path, data, receivedDataset(target, dataset, fullPath, lastPath), snap)
				}
			}
		}
	}
	return receiveUnknown(path, data, target, fullPath, lastPath)
}

// receiveUnknown adds the snapshot that the first line of FAKEZFS_RECEIVE_UNKNOWN names, and
// removes that line. Without the variable it does nothing.
func receiveUnknown(path string, data []byte, target string, fullPath, lastPath bool) error {
	queue := os.Getenv("FAKEZFS_RECEIVE_UNKNOWN")
	if queue == "" {
		return nil
	}
	lines, err := os.ReadFile(queue)
	if err != nil {
		return fmt.Errorf("fakezfs: %v", err)
	}
	first, rest, _ := strings.Cut(string(lines), "\n")
	dataset, name, ok := strings.Cut(strings.TrimSpace(first), "@")
	if !ok {
		return fmt.Errorf("fakezfs: FAKEZFS_RECEIVE_UNKNOWN: want dataset@name, got %q", first)
	}
	snaps, err := datasetSnapshots(dataset)
	if err != nil {
		return err
	}
	snap, err := lookup(dataset, dataset+"@"+name, snaps)
	if err != nil {
		return err
	}
	if err = os.WriteFile(queue, []byte(rest), 0600); err != nil {
		return fmt.Errorf("fakezfs: %v", err)
	}
	return addSnapshot(path, data, receivedDataset(target, dataset, fullPath, lastPath), snap)
}

// receivedDataset is the dataset `zfs receive [-d|-e] target` writes a stream of sent to: target,
// or with -d target plus sent without its pool, or with -e target plus sent's last component.
func receivedDataset(target, sent string, fullPath, lastPath bool) string {
	switch {
	case fullPath:
		if _, rest, ok := strings.Cut(sent, "/"); ok {
			return target + "/" + rest
		}
	case lastPath:
		return target + "/" + sent[strings.LastIndex(sent, "/")+1:]
	}
	return target
}

// sameStream returns whether r starts with head and hashes to sum.
func sameStream(r io.Reader, head, sum []byte) bool {
	got := make([]byte, len(head))
	if _, err := io.ReadFull(r, got); err != nil || !bytes.Equal(got, head) {
		return false
	}
	h := sha256.New()
	_, _ = h.Write(got)
	if _, err := io.Copy(h, r); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), sum)
}

// fixtureDatasets returns the datasets the fixture rows name, in order.
func fixtureDatasets(data string) []string {
	var datasets []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(data, "\n") {
		line = zfs.StripComment(line)
		i := strings.IndexAny(line, "@#")
		if i <= 0 || seen[line[:i]] {
			continue
		}
		seen[line[:i]] = true
		datasets = append(datasets, line[:i])
	}
	return datasets
}

// addSnapshot appends dataset@snap, with snap's creation time (a received snapshot keeps it), to
// the fixture at path, whose content is data.
func addSnapshot(path string, data []byte, dataset string, snap files.SnapshotInfo) error {
	if snaps, err := datasetSnapshots(dataset); err == nil {
		for _, s := range snaps {
			if s.Name == snap.Name && !s.Bookmark {
				return fmt.Errorf("cannot receive new snapshot: destination '%s@%s' exists", dataset, snap.Name)
			}
		}
	}
	row := fmt.Sprintf("%s@%s\t%d\tsnapshot\n", dataset, snap.Name, snap.CreationTime.Unix())
	if len(data) > 0 && data[len(data)-1] != '\n' {
		row = "\n" + row
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("fakezfs: %v", err)
	}
	if _, err = f.WriteString(row); err != nil {
		_ = f.Close()
		return fmt.Errorf("fakezfs: %v", err)
	}
	return f.Close()
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
