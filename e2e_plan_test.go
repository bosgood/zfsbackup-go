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
	"time"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/files"
)

// A capture without creation times (the JSON form the runbook promotes) dates
// snapshots from their sanoid names; the real creation is seconds later and
// that is what the manifests hold. plan must adopt the manifests' times for
// those snapshots so that it agrees with send against the live pool.
func TestE2EPlanNamesOnlyCaptureMatchesLivePool(t *testing.T) {
	setLocal(t, time.UTC) // the names below are in UTC
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
	if err := os.WriteFile(capture, []byte(b.String()), 0600); err != nil {
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
	if err := os.WriteFile(capture, []byte("tank/OTHER@autosnap_2026-09-01_00:00:00_monthly\n"), 0600); err != nil {
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

// planOneFullAtTwoDests sends a full of tank/data@a_monthly to two destinations and removes
// its manifest from the second: the trace of a manifest upload that failed there, which
// --resume completes.
func planOneFullAtTwoDests(t *testing.T, env *e2eEnv) (dests string) {
	t.Helper()
	dest2 := newDest(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a_monthly", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	dests = "file://" + env.dest + ",file://" + dest2
	env.sendOK(t, "--volsize", "1", "--compressor", "", "--full", "--fullSnapshotSuffix", "_monthly", "tank/data", dests)
	for _, name := range manifestNames(destObjects(t, dest2)) {
		if err := os.Remove(filepath.Join(dest2, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dests
}

// plan --full --resume agrees with send --full --resume: the set is completed at the
// destination that lacks its manifest.
func TestE2EPlanFullResumeMatchesSend(t *testing.T) {
	env := newE2EEnv(t)
	dests := planOneFullAtTwoDests(t, env)
	flags := []string{"--full", "--fullSnapshotSuffix", "_monthly", "--resume"}

	out, logs, err := env.plan(append(flags, "tank/data", dests)...)
	if err != nil {
		t.Errorf("plan --full --resume: %v\n%s%s", err, out, logs)
	}
	if !strings.Contains(out, "a_monthly") {
		t.Errorf("plan --full --resume does not send a_monthly:\n%s", out)
	}
	slogs, serr := env.send(append([]string{"-n", "--volsize", "1", "--compressor", ""}, append(flags, "tank/data", dests)...)...)
	if serr != nil {
		t.Fatalf("send -n --full --resume: %v\n%s", serr, slogs)
	}
	if !strings.Contains(slogs, "would perform a full backup of tank/data@a_monthly") {
		t.Errorf("send -n --full --resume does not send a full of a_monthly:\n%s", slogs)
	}
}

// plan --resume --manifests completes a set missing at a destination, as send --resume does.
func TestE2EPlanResumeWithManifestsFile(t *testing.T) {
	env := newE2EEnv(t)
	dests := planOneFullAtTwoDests(t, env)
	man := filepath.Join(t.TempDir(), "manifests.txt")
	if err := os.WriteFile(man, []byte(fmt.Sprintf("a_monthly %d\n---\n", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix())), 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly", "--resume"}

	out, logs, err := env.plan(append(flags, "--manifests", man, "tank/data")...)
	if err != nil {
		t.Errorf("plan --resume --manifests: %v\n%s%s", err, out, logs)
	}
	if !strings.Contains(out, "complete-partial") {
		t.Errorf("plan --resume --manifests does not complete the partial set:\n%s", out)
	}
	slogs, serr := env.send(append([]string{"-n"}, append(flags, "tank/data", dests)...)...)
	if serr != nil {
		t.Fatalf("send -n --resume: %v\n%s", serr, slogs)
	}
	if !strings.Contains(slogs, "would perform a full backup of tank/data@a_monthly") {
		t.Errorf("send -n --resume does not complete the full of a_monthly:\n%s", slogs)
	}
}

// --manifests with creation epochs (what a manifest records) next to a names-only capture:
// the snapshots the manifests name take their recorded times, as with destination URIs, so
// plan sends the incremental send sends instead of a source-pruned full.
func TestE2EPlanManifestsFileAdoptsCreationTimes(t *testing.T) {
	env := newE2EEnv(t)
	lag := 7 * time.Second
	at := func(m time.Month) time.Time { return time.Date(2026, m, 1, 0, 0, 0, 0, time.UTC) }
	snap := func(t0 time.Time) files.SnapshotInfo {
		return files.SnapshotInfo{Name: "autosnap_" + t0.Format("2006-01-02_15:04:05") + "_monthly", CreationTime: t0.Add(lag)}
	}
	flags := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly"}
	target := "file://" + env.dest

	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{snap(at(9))})
	env.sendOK(t, append(flags, "tank/data", target)...)
	pool := []files.SnapshotInfo{snap(at(10)), snap(at(9))}
	env.writeSnapshots(t, "tank/data", pool)

	dir := t.TempDir()
	capture, man := filepath.Join(dir, "capture.txt"), filepath.Join(dir, "manifests.txt")
	var b strings.Builder
	for _, s := range pool {
		b.WriteString("tank/data@" + s.Name + "\n")
	}
	if err := os.WriteFile(capture, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(man, []byte(fmt.Sprintf("%s %d\n", pool[1].Name, pool[1].CreationTime.Unix())), 0600); err != nil {
		t.Fatal(err)
	}

	out, logs, err := env.plan(append(flags, "--snapshots", capture, "--manifests", man, "tank/data")...)
	if err != nil {
		t.Errorf("plan --manifests: %v\n%s%s", err, out, logs)
	}
	if !strings.Contains(out, "INCR") || strings.Contains(out, "source-pruned") {
		t.Errorf("plan from a names-only capture and --manifests is not the incremental send makes:\n%s", out)
	}
	if !strings.Contains(logs, "The capture has no creation times: 1 taken from the manifests") {
		t.Errorf("plan did not warn that the capture has no creation times; logs:\n%s", logs)
	}
	slogs, serr := env.send(append([]string{"-n"}, append(flags, "tank/data", target)...)...)
	if serr != nil {
		t.Fatalf("send -n: %v\n%s", serr, slogs)
	}
	if !strings.Contains(slogs, "would perform a incremental backup of tank/data@"+pool[0].Name) {
		t.Errorf("send -n does not send the incremental:\n%s", slogs)
	}
}

// setLocal makes loc the host's zone (backup.HostZone) for the rest of the test. Not
// time.Local: the runtime's timers read it, so writing it is a data race under -race.
func setLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := backup.HostZone
	backup.HostZone = loc
	t.Cleanup(func() { backup.HostZone = old })
}

// Sanoid names snapshots in the host's local time. Without location=, a names-only capture
// dates them in that zone: on a New York host the adopted monthly would otherwise sort after
// hourlies taken hours later, and plan would disagree with send against the live pool.
func TestE2EPlanNamesOnlyCaptureInHostZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	setLocal(t, ny)
	env := newE2EEnv(t)
	lag := 7 * time.Second
	snap := func(local time.Time, period string) files.SnapshotInfo {
		return files.SnapshotInfo{Name: "autosnap_" + local.Format("2006-01-02_15:04:05") + "_" + period, CreationTime: local.Add(lag)}
	}
	flags := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly"}
	target := "file://" + env.dest
	monthly := snap(time.Date(2026, 9, 1, 0, 0, 0, 0, ny), "monthly")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{monthly})
	env.sendOK(t, append(flags, "tank/data", target)...)
	pool := []files.SnapshotInfo{
		snap(time.Date(2026, 9, 1, 3, 0, 0, 0, ny), "hourly"),
		snap(time.Date(2026, 9, 1, 2, 0, 0, 0, ny), "hourly"),
		monthly,
	}
	env.writeSnapshots(t, "tank/data", pool)
	capture := filepath.Join(t.TempDir(), "capture.txt")
	var b strings.Builder
	for _, s := range pool {
		b.WriteString("tank/data@" + s.Name + "\n")
	}
	if err := os.WriteFile(capture, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}

	live, _, lerr := env.plan(append(flags, "tank/data", target)...)
	if lerr != nil || !strings.Contains(live, "INCR") {
		t.Fatalf("plan against the live pool (%v) is not an INCR:\n%s", lerr, live)
	}
	fromCapture, logs, cerr := env.plan(append(flags, "--snapshots", capture, "tank/data", target)...)
	if cerr != nil || live != fromCapture {
		t.Errorf("plan from a names-only capture (%v) differs from plan against the live pool:\n--- live\n%s--- capture\n%s", cerr, live, fromCapture)
	}
	if !strings.Contains(logs, "read as local (E") { // EST or EDT
		t.Errorf("the names-only warning does not name the zone the names were read in; logs:\n%s", logs)
	}
}

// A last backup dated after everything on the pool (another host writing the same prefix and
// dataset name, a forged manifest, or a clock that was wrong) used to make every smart send
// "Nothing new to back up" with exit 0. send and plan refuse it instead.
func TestE2ESendRefusesLastBackupNewerThanPool(t *testing.T) {
	env := newE2EEnv(t)
	flags := []string{"--fullIfOlderThan", "4320h", "--fullSnapshotSuffix", "_monthly", "--incrementalSnapshotSuffix", "_monthly"}
	target := "file://" + env.dest
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "autosnap_2099-01-01_00:00:00_monthly", CreationTime: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}})
	env.sendOK(t, append(flags, "tank/data", target)...)

	var pool []files.SnapshotInfo
	for m := 12; m >= 1; m-- {
		d := time.Date(2026, time.Month(m), 1, 0, 0, 7, 0, time.UTC)
		pool = append(pool, files.SnapshotInfo{Name: "autosnap_" + d.Format("2006-01-02_15:04:05") + "_monthly", CreationTime: d})
	}
	env.writeSnapshots(t, "tank/data", pool)
	const want = "the destination's last backup of tank/data is autosnap_2099-01-01_00:00:00_monthly, dated 2099-01-01T00:00:00Z, " +
		"after every snapshot on the pool"

	logs, err := env.send(append(flags, "tank/data", target)...)
	if !errors.Is(err, backup.ErrLastBackupInFuture) {
		t.Errorf("send returned %v, want ErrLastBackupInFuture\n%s", err, logs)
	}
	if !strings.Contains(logs, want) {
		t.Errorf("send did not say why; logs:\n%s", logs)
	}
	out, plogs, err := env.plan(append(flags, "tank/data", target)...)
	if err == nil || !strings.Contains(out, want) || strings.Contains(out, "checks: OK") {
		t.Errorf("plan returned %v and printed:\n%s\nlogs:\n%s", err, out, plogs)
	}
}
