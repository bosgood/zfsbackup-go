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

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/backup"
)

// oldDestination is a destination written by master (b733e96): a full of tank/data@a and an
// incremental a..b, each one volume of 65536 zero bytes of stream. See its README.md.
const oldDestination = "testdata/manifests/master-b733e96"

// clean runs `zfsbackup clean` in-process and returns what it logged.
func (env *e2eEnv) clean(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return cleanS3In(t, env.work, args...)
}

// copyDir copies the regular files under src to dst, keeping their relative paths.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() == "README.md" {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if rerr = os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0700); rerr != nil {
			return rerr
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A destination written by the version before this branch reads back: list prints both sets,
// receive --auto restores both, clean finds nothing to delete, and a smart send has nothing new.
// A failure here names an incompatibility of the reader (manifestNamed, say); fix the reader,
// never the fixture.
func TestE2EOldDestinationReads(t *testing.T) {
	env := newE2EEnv(t)
	copyDir(t, oldDestination, env.dest)
	pool := "tank/data@b\t1790899200\tsnapshot\ntank/data@a\t1788220800\tsnapshot\n"
	if err := os.WriteFile(env.snapshots, []byte(pool), 0600); err != nil {
		t.Fatal(err)
	}
	dest := "file://" + env.dest

	t.Run("list", func(t *testing.T) {
		out, logs, err := env.list(dest)
		if err != nil {
			t.Fatalf("list: %v\n%s", err, logs)
		}
		for _, want := range []string{"Snapshot: a (", "Snapshot: b (", "Incremental From Snapshot: a ("} {
			if !strings.Contains(out, want) {
				t.Errorf("list does not print %q:\n%s\n%s", want, out, logs)
			}
		}
	})

	t.Run("receive", func(t *testing.T) {
		receiveLog := filepath.Join(t.TempDir(), "receive.log")
		t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
		logs, err := guarded(t, func() (string, error) { return env.receive("--auto", "tank/data@b", dest, "restored/data") })
		if err != nil {
			t.Fatalf("receive --auto: %v\n%s", err, logs)
		}
		data, err := os.ReadFile(receiveLog)
		if err != nil {
			t.Fatalf("zfs receive was not run: %v\n%s", err, logs)
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("zfs receive ran %d times (%q), want 2\n%s", len(lines), data, logs)
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, fmt.Sprint(65536)+" ") {
				t.Errorf("zfs receive got %q, want the 65536-byte stream", line)
			}
		}
	})

	t.Run("clean", func(t *testing.T) {
		logs, err := env.clean(t, "--dry-run=true", "--cleanLocal=false", dest)
		if err != nil {
			t.Fatalf("clean: %v\n%s", err, logs)
		}
		if !strings.Contains(logs, "would delete 0 objects") {
			t.Errorf("want nothing to delete\n%s", logs)
		}
	})

	t.Run("smart send", func(t *testing.T) {
		logs, err := guarded(t, func() (string, error) { return env.send("--fullIfOlderThan", "8760h", "tank/data", dest) })
		if !errors.Is(err, backup.ErrNoOp) || !strings.Contains(logs, "Nothing new to back up.") {
			t.Errorf("smart send: %v, want nothing new to back up\n%s", err, logs)
		}
	})
}
