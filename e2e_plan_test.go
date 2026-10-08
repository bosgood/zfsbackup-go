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
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// A capture without creation times (the JSON form the runbook promotes) dates
// snapshots from their sanoid names; the real creation is seconds later and
// that is what the manifests hold. plan must adopt the manifests' times for
// those snapshots so that it agrees with send against the live pool.
func TestE2EPlanNamesOnlyCaptureMatchesLivePool(t *testing.T) {
	env := newE2EEnv(t)
	lag := 7 * time.Second
	at := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }
	snap := func(t0 time.Time, period string) files.SnapshotInfo {
		return files.SnapshotInfo{Name: "autosnap_" + t0.Format("2006-01-02_15:04:05") + "_" + period, CreationTime: t0.Add(lag)}
	}
	flags := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly"}
	target := "file://" + env.dest

	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{snap(at(2026, 9), "monthly")})
	env.sendOK(t, append(flags, "tank/data", target)...)

	pool := []files.SnapshotInfo{snap(at(2026, 10), "monthly"), snap(at(2026, 9), "monthly")}
	env.writeSnapshots(t, "tank/data", pool)

	// The capture: names only, as in testdata/zfs/*.json.
	capture := filepath.Join(t.TempDir(), "capture.txt")
	var b strings.Builder
	for _, s := range pool {
		b.WriteString("tank/data@" + s.Name + "\n")
	}
	if err := ioutil.WriteFile(capture, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}

	live, _, lerr := env.plan(append(flags, "tank/data", target)...)
	if lerr != nil {
		t.Fatalf("plan against the live pool: %v\n%s", lerr, live)
	}
	if !strings.Contains(live, "INCR") {
		t.Fatalf("plan against the live pool is not an INCR:\n%s", live)
	}
	fromCapture, logs, cerr := env.plan(append(flags, "--snapshots", capture, "tank/data", target)...)
	if cerr != nil {
		t.Errorf("plan from the capture: %v\n%s", cerr, fromCapture)
	}
	if live != fromCapture {
		t.Errorf("plan from a names-only capture of the same pool differs from plan against the live pool:\n--- live\n%s--- capture\n%s", live, fromCapture)
	}
	if !strings.Contains(logs, "The capture has no creation times: 1 taken from the manifests") {
		t.Errorf("plan did not warn that the capture has no creation times; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "zfs list -H -p -o name,creation -t snapshot,bookmark -S creation tank/data") {
		t.Errorf("the warning does not name the capture command to use; logs:\n%s", logs)
	}
}

// --snapshots of one dataset is refused for another volume argument.
func TestE2EPlanRejectsOtherDataset(t *testing.T) {
	env := newE2EEnv(t)
	capture := filepath.Join(t.TempDir(), "capture.txt")
	if err := ioutil.WriteFile(capture, []byte("tank/OTHER@autosnap_2026-09-01_00:00:00_monthly\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, logs, err := env.plan("--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly",
		"--snapshots", capture, "tank/data")
	if err == nil {
		t.Fatalf("plan of tank/data accepted a listing of tank/OTHER:\n%s", out)
	}
	if !strings.Contains(logs, "--snapshots lists tank/OTHER, not tank/data") {
		t.Errorf("got logs %q, want the dataset mismatch named", logs)
	}
}
