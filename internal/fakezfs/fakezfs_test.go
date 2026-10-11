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

package fakezfs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `# two datasets and rows that belong to any dataset
tank/data@b	200	snapshot
tank/data#a	100	bookmark
tank/other@z	900	snapshot
tank/data@c	300	snapshot
autosnap_2026-09-01_00:00:00_monthly
`

// setup writes the fixture and points the fake at it and at a fresh log.
func setup(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	snapshots := filepath.Join(dir, "snapshots.txt")
	if err := os.WriteFile(snapshots, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(dir, "zfs.log")
	t.Setenv("FAKEZFS_SNAPSHOTS", snapshots)
	t.Setenv("FAKEZFS_LOG", logPath)
	return logPath
}

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Main(args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestList(t *testing.T) {
	setup(t)
	code, out, errOut := run("list", "-H", "-d", "1", "-p", "-t", "snapshot,bookmark", "-r", "-o", "name,creation,type", "-S", "creation", "tank/data")
	want := "tank/data@autosnap_2026-09-01_00:00:00_monthly\t1788220800\tsnapshot\n" +
		"tank/data@c\t300\tsnapshot\n" +
		"tank/data@b\t200\tsnapshot\n" +
		"tank/data#a\t100\tbookmark\n"
	if code != 0 || out != want {
		t.Errorf("got %d %q (stderr %q), want 0 %q", code, out, errOut, want)
	}

	if code, _, errOut = run("list", "-H", "-d", "1", "-p", "-t", "snapshot,bookmark", "-r", "-o", "name,creation,type", "-S", "creation",
		"tank/missing"); code != 0 {
		// Unprefixed rows belong to every dataset, so tank/missing exists here.
		t.Errorf("got exit %d (%s) for a dataset with unprefixed rows", code, errOut)
	}
	if code, _, _ = run("list", "-H", "tank/data"); code != 1 {
		t.Errorf("an unexpected list invocation exited %d, want 1", code)
	}
}

func TestGetCreation(t *testing.T) {
	setup(t)
	for target, want := range map[string]string{"tank/data@b": "200\n", "tank/data#a": "100\n", "tank/data": "1\n"} {
		if code, out, errOut := run("get", "-H", "-p", "-o", "value", "creation", target); code != 0 || out != want {
			t.Errorf("get creation %s: got %d %q (stderr %q), want %q", target, code, out, errOut, want)
		}
	}
	code, _, errOut := run("get", "-H", "-p", "-o", "value", "creation", "tank/data@nope")
	if code != 1 || !strings.Contains(errOut, "cannot open 'tank/data@nope': dataset does not exist") {
		t.Errorf("unknown snapshot: got %d %q", code, errOut)
	}
	if code, _, _ = run("get", "-H", "-p", "-o", "value", "used", "tank/data@b"); code != 1 {
		t.Errorf("unsupported property exited %d, want 1", code)
	}
}

func TestSendDryRun(t *testing.T) {
	setup(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "1234")
	code, out, errOut := run("send", "-n", "-P", "-i", "b", "tank/data@c")
	if code != 0 || strings.Count(out, "size\t1234\n") != 1 || !strings.HasPrefix(out, "incremental\tb\ttank/data@c\t1234\n") {
		t.Errorf("got %d %q (stderr %q)", code, out, errOut)
	}
	t.Setenv("FAKEZFS_DRYRUN_OUTPUT", "garbage\n")
	if code, out, _ = run("send", "-n", "-P", "tank/data@c"); code != 0 || out != "garbage\n" {
		t.Errorf("FAKEZFS_DRYRUN_OUTPUT: got %d %q", code, out)
	}
}

func TestSend(t *testing.T) {
	setup(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	_, full, _ := run("send", "tank/data@c")
	code, incr, errOut := run("send", "-c", "-i", "b", "tank/data@c")
	if code != 0 || len(full) != 4096 || len(incr) != 4096 {
		t.Fatalf("got exit %d (%s), %d and %d bytes, want 4096 each", code, errOut, len(full), len(incr))
	}
	if full == incr {
		t.Errorf("full and incremental streams are identical")
	}
	if _, again, _ := run("send", "-c", "-i", "b", "tank/data@c"); again != incr {
		t.Errorf("the same send produced different bytes")
	}
	if code, _, _ = run("send", "-i", "tank/data#a", "tank/data@c"); code != 0 {
		t.Errorf("send from a bookmark exited %d", code)
	}
	for _, args := range [][]string{
		{"send", "tank/data@nope"},            // missing target
		{"send", "-i", "nope", "tank/data@c"}, // missing source
		{"send", "-i", "a", "tank/data@c"},    // a is a bookmark, not a snapshot
		{"send", "-x", "tank/data@c"},         // unknown flag
	} {
		if code, _, _ = run(args...); code != 1 {
			t.Errorf("%v exited %d, want 1", args, code)
		}
	}
}

func TestUnexpectedAndLog(t *testing.T) {
	logPath := setup(t)
	code, _, errOut := run("destroy", "-r", "tank/data")
	if code != 1 || !strings.Contains(errOut, "unexpected invocation: zfs destroy -r tank/data") {
		t.Errorf("got %d %q", code, errOut)
	}
	run("get", "-H", "-p", "-o", "value", "creation", "tank/data@b")
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "destroy -r tank/data\nget -H -p -o value creation tank/data@b\n"; string(log) != want {
		t.Errorf("log = %q, want %q", log, want)
	}
}

func TestGetGUID(t *testing.T) {
	setup(t)
	guid := func(target string) string {
		t.Helper()
		code, out, errOut := run("get", "-H", "-p", "-o", "value", "guid", target)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("guid of %s: got %d %q (stderr %q)", target, code, out, errOut)
		}
		return out
	}
	b, c := guid("tank/data@b"), guid("tank/data@c")
	if b == c || b != guid("tank/data@b") {
		t.Errorf("guids of b and c: %q %q, want distinct and stable", b, c)
	}

	// Recreated under the same name, b is another snapshot.
	path := os.Getenv("FAKEZFS_SNAPSHOTS")
	if err := os.WriteFile(path, []byte(strings.Replace(fixture, "tank/data@b\t200", "tank/data@b\t250", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if again := guid("tank/data@b"); again == b {
		t.Errorf("recreated b kept its guid %q", b)
	}
}

func TestStreamSalt(t *testing.T) {
	read := func(r interface{ Read([]byte) (int, error) }) []byte {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	plain := read(Stream("", "tank/data@a", 1024))
	if !bytes.Equal(plain, read(StreamSalted("", "tank/data@a", "", 1024))) {
		t.Error("an empty salt changed the stream")
	}
	if bytes.Equal(plain, read(StreamSalted("", "tank/data@a", "x", 1024))) {
		t.Error("a salt left the stream unchanged")
	}
}

// receive adds the snapshot the stream carries, with its creation time, to the dataset zfs
// receive writes to, and refuses it the second time. A stream from no fixture snapshot adds nothing.
func TestReceiveAddsSnapshot(t *testing.T) {
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	listOf := func(dataset string) string {
		t.Helper()
		_, out, _ := run("list", "-H", "-d", "1", "-p", "-t", "snapshot,bookmark", "-r", "-o", "name,creation,type", "-S", "creation", dataset)
		return out
	}
	for _, tc := range []struct {
		name    string
		args    []string
		dataset string
	}{
		{"plain", []string{"receive", "restored/data"}, "restored/data"},
		{"fullPath", []string{"receive", "-d", "-u", "restored"}, "restored/data"},
		{"lastPath", []string{"receive", "-e", "-F", "-o", "origin=x@y", "restored"}, "restored/data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			_, incr, _ := run("send", "-i", "b", "tank/data@c")
			var errOut bytes.Buffer
			if code := Main(tc.args, strings.NewReader(incr), io.Discard, &errOut); code != 0 {
				t.Fatalf("receive exited %d: %s", code, errOut.String())
			}
			if want := tc.dataset + "@c\t300\tsnapshot\n"; !strings.Contains(listOf(tc.dataset), want) {
				t.Errorf("the listing of %s does not hold %q:\n%s", tc.dataset, want, listOf(tc.dataset))
			}
			errOut.Reset()
			if code := Main(tc.args, strings.NewReader(incr), io.Discard, &errOut); code != 1 ||
				!strings.Contains(errOut.String(), "destination '"+tc.dataset+"@c' exists") {
				t.Errorf("a second receive of c: exit %d %q, want 1 and destination exists", code, errOut.String())
			}
		})
	}

	setup(t)
	before, err := os.ReadFile(os.Getenv("FAKEZFS_SNAPSHOTS"))
	if err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"receive", "restored/data"}, bytes.NewReader(make([]byte, 4096)), io.Discard, io.Discard); code != 0 {
		t.Fatalf("receive of an unknown stream exited %d", code)
	}
	if after, _ := os.ReadFile(os.Getenv("FAKEZFS_SNAPSHOTS")); !bytes.Equal(after, before) {
		t.Errorf("an unknown stream changed the fixture:\n%s", after)
	}

	// FAKEZFS_RECEIVE_UNKNOWN names what unknown streams carry, one line per receive.
	queue := filepath.Join(t.TempDir(), "unknown")
	if err = os.WriteFile(queue, []byte("tank/data@b\ntank/data@c\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEZFS_RECEIVE_UNKNOWN", queue)
	for _, want := range []string{"restored/data@b\t200\tsnapshot\n", "restored/data@c\t300\tsnapshot\n"} {
		if code := Main([]string{"receive", "restored/data"}, bytes.NewReader(make([]byte, 4096)), io.Discard, io.Discard); code != 0 {
			t.Fatalf("receive of a queued stream exited %d", code)
		}
		if out := listOf("restored/data"); !strings.Contains(out, want) {
			t.Errorf("the listing does not hold %q:\n%s", want, out)
		}
	}
	if rest, _ := os.ReadFile(queue); len(rest) != 0 {
		t.Errorf("the queue still holds %q", rest)
	}
}
