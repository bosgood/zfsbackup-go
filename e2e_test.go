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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/internal/fakezfs"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

// The end-to-end tests run the real send pipeline against a file://
// destination, with this test binary standing in for zfs (see
// internal/fakezfs): every case passes --zfsPath <test binary> and sets
// FAKEZFS=1, so each zfs invocation re-executes the binary as the fake.
func TestMain(m *testing.M) {
	fakezfs.RunIfRequested()
	os.Exit(m.Run())
}

const monthlyOnlyScenario = "backup/testdata/scenarios/monthly-only-year"

// e2eEnv prepares the fake zfs and the scratch directories of one test.
type e2eEnv struct {
	self      string // this test binary, the fake zfs
	snapshots string // FAKEZFS_SNAPSHOTS
	zfsLog    string // FAKEZFS_LOG
	work      string // --workingDirectory
	dest      string // file:// destination directory
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := &e2eEnv{
		self:      self,
		snapshots: filepath.Join(dir, "snapshots.txt"),
		zfsLog:    filepath.Join(dir, "zfs.log"),
		work:      filepath.Join(dir, "work"),
		dest:      filepath.Join(dir, "dest"),
	}
	for _, d := range []string{env.work, env.dest} {
		if err = os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKEZFS", "1")
	t.Setenv("FAKEZFS_SNAPSHOTS", env.snapshots)
	t.Setenv("FAKEZFS_LOG", env.zfsLog)
	return env
}

// writeSnapshots writes the pool the fake zfs lists: raw rows with epochs, in
// the given (newest-first) order.
func (env *e2eEnv) writeSnapshots(t *testing.T, volume string, snaps []files.SnapshotInfo) {
	t.Helper()
	var b strings.Builder
	for _, s := range snaps {
		kind, sep := "snapshot", "@"
		if s.Bookmark {
			kind, sep = "bookmark", "#"
		}
		fmt.Fprintf(&b, "%s%s%s\t%d\t%s\n", volume, sep, s.Name, s.CreationTime.Unix(), kind)
	}
	if err := ioutil.WriteFile(env.snapshots, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

// send runs `zfsbackup send` in-process and returns what it logged.
func (env *e2eEnv) send(args ...string) (string, error) {
	cmd.ResetSendJobInfo()
	var logs bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	oldStdout := config.Stdout
	config.Stdout = ioutil.Discard
	defer func() { config.Stdout = oldStdout }()

	base := []string{"send", "--zfsPath", env.self, "--workingDirectory", env.work, "--maxParallelUploads", "1"}
	cmd.RootCmd.SetArgs(append(base, args...))
	// Leave no arguments or flag values behind: RootCmd would reuse them, for
	// example in TestVersion's main(), which runs with os.Args.
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetSendJobInfo()
	}()
	err := cmd.RootCmd.ExecuteContext(context.Background())
	return logs.String(), err
}

// plan runs `zfsbackup plan` in-process and returns what it printed and logged.
func (env *e2eEnv) plan(args ...string) (out, logs string, err error) {
	cmd.ResetSendJobInfo()
	var outBuf, logBuf bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logBuf, "", 0)))
	oldStdout := config.Stdout
	config.Stdout = &outBuf
	defer func() { config.Stdout = oldStdout }()

	cmd.RootCmd.SetArgs(append([]string{"plan", "--zfsPath", env.self, "--workingDirectory", env.work}, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetSendJobInfo()
	}()
	err = cmd.RootCmd.ExecuteContext(context.Background())
	return outBuf.String(), logBuf.String(), err
}

// scenarioFlags reads a scenario's send flags.
func scenarioFlags(t *testing.T, dir string) []string {
	t.Helper()
	sc, err := ioutil.ReadFile(filepath.Join(dir, "flags"))
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	for _, line := range strings.Split(string(sc), "\n") {
		flags = append(flags, strings.Fields(zfs.StripComment(line))...)
	}
	return flags
}

// newestBackup reads the destination back as the smart options see it and
// returns its newest backup.
func newestBackup(t *testing.T, volume, target string) *files.JobInfo {
	t.Helper()
	jobInfo := &files.JobInfo{ManifestPrefix: "manifests", MaxParallelUploads: 1, MaxBackoffTime: time.Minute, MaxRetryTime: time.Minute}
	backups, err := backup.BackupsAtTarget(context.Background(), volume, target, jobInfo)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) == 0 {
		t.Fatalf("no backups at %s", target)
	}
	return backups[0]
}

// checkSent fails unless the newest backup at target is what plan sends.
func checkSent(t *testing.T, volume, target string, plan backup.Plan) {
	t.Helper()
	got := newestBackup(t, volume, target)
	want := files.SnapshotInfo{}
	if plan.Action == backup.PlanIncremental {
		want = plan.Source
	}
	if !got.BaseSnapshot.Equal(&plan.Base) || !got.IncrementalSnapshot.Equal(&want) || got.IncrementalSnapshot.Bookmark != want.Bookmark {
		t.Errorf("%s holds %s from %+v, the planner planned %s", target, got.BaseSnapshot.Name, got.IncrementalSnapshot, plan)
	}
}

func TestE2EDryRunSendsNothing(t *testing.T) {
	env := newE2EEnv(t)
	sc, err := backup.LoadScenario(monthlyOnlyScenario)
	if err != nil {
		t.Fatal(err)
	}
	env.writeSnapshots(t, "tank/data", sc.Snapshots)

	args := append([]string{"-n"}, scenarioFlags(t, monthlyOnlyScenario)...)
	logs, err := env.send(append(args, "tank/data", "file://"+env.dest)...)
	if err != nil {
		t.Fatalf("send -n: %v\n%s", err, logs)
	}
	if want := "Dry-run: would perform a full backup of tank/data@autosnap_2026-09-01_00:00:00_monthly"; !strings.Contains(logs, want) {
		t.Errorf("want %q in the log:\n%s", want, logs)
	}
	if entries, _ := ioutil.ReadDir(env.dest); len(entries) != 0 {
		t.Errorf("dry run wrote %d objects to the destination", len(entries))
	}
	zfsLog, err := ioutil.ReadFile(env.zfsLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(zfsLog)), "\n") {
		if strings.HasPrefix(line, "send ") && !strings.HasPrefix(line, "send -n -P ") {
			t.Errorf("dry run ran zfs %s", line)
		}
	}
	if !strings.Contains(string(zfsLog), "send -n -P tank/data@autosnap_2026-09-01_00:00:00_monthly") {
		t.Errorf("dry run did not estimate the send size; zfs calls:\n%s", zfsLog)
	}
}

// TestE2ESequenceMatchesPlanner replays a year of simulated runs through the
// real send: each run sees the pool of its simulated step, and must back up
// what the planner planned. Afterwards the manifests the real code wrote and
// reads back must be exactly the simulator's destination.
func TestE2ESequenceMatchesPlanner(t *testing.T) {
	env := newE2EEnv(t)
	sc, err := backup.LoadScenario(monthlyOnlyScenario)
	if err != nil {
		t.Fatal(err)
	}
	sim := sc.Run()
	flags := scenarioFlags(t, monthlyOnlyScenario)
	target := "file://" + env.dest

	for _, step := range sim.Steps {
		env.writeSnapshots(t, sc.Volume, step.Snapshots)
		logs, err := env.send(append(flags, sc.Volume, target)...)
		switch {
		case step.Err != nil:
			t.Fatalf("%v: the planner failed (%v); the scenario should not", step.At, step.Err)
		case step.Plan.Action == backup.PlanNoop && !errors.Is(err, backup.ErrNoOp):
			t.Fatalf("%v: planned a no-op, send returned %v\n%s", step.At, err, logs)
		case step.Plan.Action != backup.PlanNoop && err != nil:
			t.Fatalf("%v: planned %s, send failed: %v\n%s", step.At, step.Plan, err, logs)
		case step.Plan.Action != backup.PlanNoop:
			checkSent(t, sc.Volume, target, step.Plan)
		}
	}

	jobInfo := &files.JobInfo{ManifestPrefix: "manifests", MaxParallelUploads: 1, MaxBackoffTime: time.Minute, MaxRetryTime: time.Minute}
	got, err := backup.BackupsAtTarget(context.Background(), sc.Volume, target, jobInfo)
	if err != nil {
		t.Fatal(err)
	}
	want := sim.Manifests[0]
	if len(got) != len(want) {
		t.Fatalf("the destination holds %d backups, the planner expected %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].BaseSnapshot.Equal(&want[i].BaseSnapshot) || !got[i].IncrementalSnapshot.Equal(&want[i].IncrementalSnapshot) {
			t.Errorf("backup %d: destination has %s from %q, planner expected %s from %q", i,
				got[i].BaseSnapshot.Name, got[i].IncrementalSnapshot.Name, want[i].BaseSnapshot.Name, want[i].IncrementalSnapshot.Name)
		}
	}
}

// TestE2ENextRunScenarios replays every next-run golden scenario through the
// real send: the backups its manifests.txt lists are first created at file://
// destinations with manual sends, then one smart send runs against the
// scenario's pool and must do what the planner planned.
func TestE2ENextRunScenarios(t *testing.T) {
	dirs, err := filepath.Glob("backup/testdata/scenarios/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		sc, err := backup.LoadScenario(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if !sc.Until.IsZero() {
			continue // runs over time: see TestE2ESequenceMatchesPlanner
		}
		t.Run(filepath.Base(dir), func(t *testing.T) {
			env := newE2EEnv(t)
			step := sc.Run().Steps[0]

			var targets []string
			for d, manifests := range sc.DestBackups {
				dest := filepath.Join(env.dest, fmt.Sprint(d))
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
				targets = append(targets, "file://"+dest)
				for i := len(manifests) - 1; i >= 0; i-- { // oldest first
					m := manifests[i]
					pool, args := []files.SnapshotInfo{m.BaseSnapshot}, []string(nil)
					if source := m.IncrementalSnapshot; source.Name != "" {
						pool, args = append(pool, source), []string{"-i", sc.Volume + "@" + source.Name}
						if source.Bookmark {
							args[1] = sc.Volume + "#" + source.Name
						}
					}
					env.writeSnapshots(t, sc.Volume, pool)
					if logs, err := env.send(append(args, sc.Volume+"@"+m.BaseSnapshot.Name, targets[d])...); err != nil {
						t.Fatalf("creating %s at %s: %v\n%s", m.BaseSnapshot.Name, targets[d], err, logs)
					}
				}
			}

			env.writeSnapshots(t, sc.Volume, sc.Snapshots)
			logs, err := env.send(append(scenarioFlags(t, dir), sc.Volume, strings.Join(targets, ","))...)
			switch {
			case step.Err != nil:
				if err == nil || err.Error() != step.Err.Error() {
					t.Errorf("the planner failed with %q, send returned %v\n%s", step.Err, err, logs)
				}
			case step.Plan.Action == backup.PlanNoop:
				if !errors.Is(err, backup.ErrNoOp) {
					t.Errorf("planned a no-op, send returned %v\n%s", err, logs)
				}
			case err != nil:
				t.Errorf("planned %s, send failed: %v\n%s", step.Plan, err, logs)
			default:
				for _, target := range targets {
					checkSent(t, sc.Volume, target, step.Plan)
				}
			}
		})
	}
}

// TestE2EExitCodes runs the real binary: a smart send that finds nothing new
// exits 0, as does its first (full) run, and a plan with a failing check
// exits 2.
func TestE2EExitCodes(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, cannot build the binary")
	}
	env := newE2EEnv(t)
	bin := filepath.Join(t.TempDir(), "zfsbackup")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	sc, err := backup.LoadScenario(monthlyOnlyScenario)
	if err != nil {
		t.Fatal(err)
	}
	env.writeSnapshots(t, "tank/data", sc.Snapshots)

	run := func(args ...string) (int, string) {
		c := exec.Command(bin, args...)
		out, err := c.CombinedOutput()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("zfsbackup %v: %v", args, err)
		}
		return c.ProcessState.ExitCode(), string(out)
	}
	send := append([]string{"send", "--zfsPath", env.self, "--workingDirectory", env.work}, scenarioFlags(t, monthlyOnlyScenario)...)
	send = append(send, "tank/data", "file://"+env.dest)

	// The dry run's report is visible at the default log level.
	code, out := run(append([]string{send[0], "-n"}, send[1:]...)...)
	if code != 0 || !strings.Contains(out, "Dry-run: would perform a full backup of tank/data@autosnap_2026-09-01_00:00:00_monthly") {
		t.Errorf("send -n exited %d without the Dry-run report:\n%s", code, out)
	}
	if code, out = run(send...); code != 0 {
		t.Fatalf("first send (a full) exited %d:\n%s", code, out)
	}
	code, out = run(send...)
	if code != 0 || !strings.Contains(out, "Nothing new to back up.") {
		t.Errorf("second send (a no-op) exited %d, want 0:\n%s", code, out)
	}

	broken := filepath.Join(t.TempDir(), "manifests.txt")
	if err = ioutil.WriteFile(broken, []byte("autosnap_2026-06-01_00:00:00_monthly to autosnap_2026-07-01_00:00:00_monthly\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out = run("plan", "--workingDirectory", env.work, "--increment", "--snapshots", env.snapshots, "--manifests", broken, "tank/data")
	if code != 2 || !strings.Contains(out, "chain-links") {
		t.Errorf("plan with a broken chain exited %d, want 2:\n%s", code, out)
	}

	// Every run removed its temporary directory, the failed ones included.
	if left, err := ioutil.ReadDir(filepath.Join(env.work, "temp")); err != nil || len(left) != 0 {
		t.Errorf("runs left %d temporary directories behind (%v)", len(left), err)
	}
}
