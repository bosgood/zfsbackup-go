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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

const scenarios = "../backup/testdata/scenarios"

// runPlan runs `zfsbackup plan args...` in-process and returns its output.
func runPlanCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ResetSendJobInfo()

	var out, logs bytes.Buffer
	oldStdout := config.Stdout
	config.Stdout = &out
	defer func() { config.Stdout = oldStdout }()
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))

	RootCmd.SetArgs(append([]string{"plan", "--workingDirectory", t.TempDir()}, args...))
	err := RootCmd.ExecuteContext(context.Background())
	if err != nil {
		t.Logf("plan logged:\n%s", logs.String())
	}
	return out.String(), err
}

// scenarioFlags returns the send flags a scenario's flags file holds.
func scenarioFlags(t *testing.T, name string) []string {
	t.Helper()
	b, err := ioutil.ReadFile(filepath.Join(scenarios, name, "flags"))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, line := range strings.Split(string(b), "\n") {
		args = append(args, strings.Fields(zfs.StripComment(line))...)
	}
	return args
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := ioutil.ReadFile(filepath.Join(scenarios, name, "expected.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlanNextRun(t *testing.T) {
	const name = "monthly-only-next-incr"
	args := append(scenarioFlags(t, name),
		"--snapshots", filepath.Join(scenarios, name, "snapshots.txt"),
		"--manifests", filepath.Join(scenarios, name, "manifests.txt"),
		"tank/data")
	out, err := runPlanCommand(t, args...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if want := readGolden(t, name); out != want {
		t.Errorf("got:\n%s\nwant %s/expected.txt:\n%s", out, name, want)
	}
}

func TestPlanSchedule(t *testing.T) {
	const name = "monthly-only-5-months"
	args := append(scenarioFlags(t, name),
		"--snapshots", filepath.Join(scenarios, name, "snapshots.txt"),
		"--schedule", "policy=hourly=36,daily=30,monthly=3,from=2026-09-24T01:00:00Z,until=2027-02-20T01:00:00Z,every=24h,checks=coverage:_monthly",
		"tank/data")
	out, err := runPlanCommand(t, args...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if want := readGolden(t, name); out != want {
		t.Errorf("got:\n%s\nwant %s/expected.txt:\n%s", out, name, want)
	}
}

func TestPlanJSON(t *testing.T) {
	const name = "monthly-only-next-incr"
	args := append(scenarioFlags(t, name),
		"--jsonOutput",
		"--snapshots", filepath.Join(scenarios, name, "snapshots.txt"),
		"--manifests", filepath.Join(scenarios, name, "manifests.txt"),
		"tank/data")
	out, err := runPlanCommand(t, args...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	type snapshot struct{ Name string }
	var got struct {
		Volume string
		Runs   []struct {
			At     *string
			Action string
			Base   *snapshot
			Source *snapshot
			Reason string
		}
		Destinations [][]struct{ Base, Source *snapshot }
		Violations   []interface{}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Volume != "tank/data" || len(got.Runs) != 1 || len(got.Destinations) != 1 || len(got.Violations) != 0 {
		t.Fatalf("unexpected plan: %s", out)
	}
	run := got.Runs[0]
	if run.At != nil || run.Action != "incremental" || run.Reason != "newer-candidate" || run.Base == nil || run.Source == nil ||
		run.Base.Name != "autosnap_2026-10-01_00:00:00_monthly" || run.Source.Name != "autosnap_2026-09-01_00:00:00_monthly" {
		t.Errorf("unexpected run: %s", out)
	}
	if dest := got.Destinations[0]; len(dest) != 2 || dest[0].Base.Name != "autosnap_2026-10-01_00:00:00_monthly" || dest[0].Source == nil {
		t.Errorf("want the new incremental first at the destination, got %s", out)
	}
}

func TestPlanChecksFailed(t *testing.T) {
	dir := t.TempDir()
	snapshots := filepath.Join(dir, "snapshots.txt")
	manifests := filepath.Join(dir, "manifests.txt")
	if err := ioutil.WriteFile(snapshots, []byte("autosnap_2026-10-01_00:00:00_monthly\nautosnap_2026-09-01_00:00:00_monthly\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// An incremental whose source was never backed up cannot be restored.
	manifests1 := "autosnap_2026-08-01_00:00:00_monthly to autosnap_2026-09-01_00:00:00_monthly\n"
	if err := ioutil.WriteFile(manifests, []byte(manifests1), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runPlanCommand(t, "--increment", "--snapshots", snapshots, "--manifests", manifests, "tank/data")
	if err != errChecksFailed {
		t.Fatalf("got error %v, want errChecksFailed (exit status 2)", err)
	}
	if !strings.Contains(out, "checks: 1 violation(s)\n  chain-links  ") {
		t.Errorf("want a chain-links violation, got:\n%s", out)
	}
}

func TestPlanFlagErrors(t *testing.T) {
	snapshots := filepath.Join(scenarios, "monthly-only-noop", "snapshots.txt")
	manifests := filepath.Join(scenarios, "monthly-only-noop", "manifests.txt")
	for _, args := range [][]string{
		{"--snapshots", snapshots, "tank/data"},                                                    // no smart option
		{"--full", "--increment", "--snapshots", snapshots, "tank/data"},                           // two smart options
		{"--full", "--snapshots", snapshots, "tank/data@snap"},                                     // a snapshot, not a volume
		{"--full", "--manifests", manifests, "--snapshots", snapshots, "tank/data", "file:///tmp"}, // both destination sources
		{"--full", "--snapshots", snapshots, "--schedule", "until=soon", "tank/data"},              // bad schedule
		{"--full", "--snapshots", filepath.Join(scenarios, "missing.txt"), "tank/data"},            // missing file
	} {
		if _, err := runPlanCommand(t, args...); err == nil || err == errChecksFailed {
			t.Errorf("plan %v: got error %v, want an input error", args, err)
		}
	}
}
