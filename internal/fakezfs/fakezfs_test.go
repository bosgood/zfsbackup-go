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
