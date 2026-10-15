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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/files"
)

const mixedChain = "mix raw and plaintext"

// rawChainEnv is a destination that holds a full of tank/data@a sent with --raw; the pool also
// has b, a month newer.
func rawChainEnv(t *testing.T) *e2eEnv {
	t.Helper()
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "b_monthly", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "a_monthly", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	env.sendOK(t, "--compressor", "", "--raw", "tank/data@a_monthly", "file://"+env.dest)
	return env
}

// An incremental must carry the --raw of the backup it continues from, in a smart send and in
// an explicit -i send; a full starts a new chain and may change it.
func TestE2EChainKeepsStreamKind(t *testing.T) {
	env := rawChainEnv(t)
	dest := "file://" + env.dest
	smart := []string{
		"--compressor", "", "--fullIfOlderThan", "4320h",
		"--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly",
	}
	refused := func(what string, args ...string) {
		t.Helper()
		logs, err := guarded(t, func() (string, error) { return env.send(args...) })
		if err == nil || !strings.Contains(err.Error(), mixedChain) {
			t.Fatalf("%s: got %v, want an error that holds %q\n%s", what, err, mixedChain, logs)
		}
		if got := manifestNames(destObjects(t, env.dest)); len(got) != 1 {
			t.Fatalf("%s: the destination holds manifests %v, want only the raw full", what, got)
		}
	}

	refused("smart send without --raw", append(smart, "tank/data", dest)...)
	refused("explicit -i without --raw", "--compressor", "", "-i", "tank/data@a_monthly", "tank/data@b_monthly", dest)

	env.sendOK(t, "--compressor", "", "--raw", "-i", "tank/data@a_monthly", "tank/data@b_monthly", dest)
	got := newestBackup(t, "tank/data", dest)
	if got.BaseSnapshot.Name != "b_monthly" || got.IncrementalSnapshot.Name != "a_monthly" || !got.Raw {
		t.Fatalf("sent %s from %q (raw %v), want b_monthly from a_monthly, raw", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, got.Raw)
	}

	// A new chain may change the kind.
	env.sendOK(t, "--compressor", "", "--full", "tank/data", dest)
	if got := manifestNames(destObjects(t, env.dest)); len(got) != 3 {
		t.Fatalf("after a plaintext full the destination holds manifests %v, want 3", got)
	}
	got = newestBackup(t, "tank/data", dest)
	if got.BaseSnapshot.Name != "b_monthly" || got.IncrementalSnapshot.Name != "" || got.Raw {
		t.Fatalf("sent %s from %q (raw %v), want a plaintext full of b_monthly", got.BaseSnapshot.Name, got.IncrementalSnapshot.Name, got.Raw)
	}
}

// receive --auto refuses a chain whose links differ in --raw before it receives anything.
func TestE2EReceiveAutoRefusesMixedChain(t *testing.T) {
	env := rawChainEnv(t)
	dest := "file://" + env.dest
	env.sendOK(t, "--compressor", "", "--raw", "-i", "tank/data@a_monthly", "tank/data@b_monthly", dest)
	var incremental string
	for _, n := range manifestNames(destObjects(t, env.dest)) {
		if strings.Contains(n, "|to|") {
			incremental = n
		}
	}
	if incremental == "" {
		t.Fatalf("no incremental manifest at %s", env.dest)
	}
	// What an older version left when a run forgot --raw.
	reforgeManifest(t, env, incremental, func(m *files.JobInfo) { m.Raw = false })

	receiveLog := filepath.Join(t.TempDir(), "receive.log")
	t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
	logs, err := guarded(t, func() (string, error) {
		return env.receive("--auto", "tank/data", dest, "restored/data")
	})
	if err == nil || !strings.Contains(err.Error(), mixedChain) {
		t.Fatalf("receive --auto of a mixed chain: got %v, want an error that holds %q\n%s", err, mixedChain, logs)
	}
	if _, serr := os.Stat(receiveLog); serr == nil {
		got, _ := os.ReadFile(receiveLog)
		t.Errorf("receive --auto of a mixed chain ran zfs receive: %q", got)
	}
}
