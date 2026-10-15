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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/cmd"
	"github.com/bosgood/zfsbackup-go/files"
)

const progressStreamBytes = 4 << 20

// progressLines are the log lines of the tracker named label.
func progressLines(logs, label string) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.HasPrefix(l, label+": ") {
			out = append(out, l)
		}
	}
	return out
}

var progressPercentRE = regexp.MustCompile(`: (\d+)% \(`)

// progressPercents are the percents of the tracker named label, in order.
func progressPercents(t *testing.T, logs, label string) []int {
	t.Helper()
	var out []int
	for _, l := range progressLines(logs, label) {
		m := progressPercentRE.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("not a progress line: %q", l)
		}
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// wantProgressTo100 fails unless the percents are strictly increasing and end at one 100.
func wantProgressTo100(t *testing.T, pcts []int, logs string) {
	t.Helper()
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Fatalf("percents %v, want them to end at 100:\n%s", pcts, logs)
	}
	for i := 1; i < len(pcts); i++ {
		if pcts[i] <= pcts[i-1] {
			t.Fatalf("percents not strictly increasing: %v", pcts)
		}
	}
}

func progressSnapshot(t *testing.T, env *e2eEnv) {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(progressStreamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
}

// volumeBoundaries are the percents a send prints when a volume's upload returns, and 100.
func volumeBoundaries(t *testing.T, dest string) map[int]bool {
	t.Helper()
	boundaries := map[int]bool{100: true}
	var sum uint64
	for _, v := range newestBackup(t, "tank/data", dest).Volumes {
		sum += v.ZFSStreamBytes
		boundaries[min(int(sum*100/progressStreamBytes), 99)] = true
	}
	return boundaries
}

var progressArgs = []string{"--volsize", "1", "--compressor", ""}

var pace100RE = regexp.MustCompile(`, [0-9.]+ [KMG]?i?B/s, took [0-9hms]+$`)

func TestE2EProgressSend(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	dest := "file://" + env.dest
	logs, err := guarded(t, func() (string, error) { return env.send(append(progressArgs, "tank/data@a", dest)...) })
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	label := "Uploading tank/data@a to " + dest
	pcts := progressPercents(t, logs, label)
	wantProgressTo100(t, pcts, logs)
	// From a temp file the count moves inside a volume, not only when its upload returns.
	boundaries := volumeBoundaries(t, dest)
	inside := false
	for _, p := range pcts {
		inside = inside || !boundaries[p]
	}
	if !inside {
		t.Errorf("every percent is at a volume boundary %v: %v", boundaries, pcts)
	}
	lines := progressLines(logs, label)
	for _, l := range lines {
		if !strings.Contains(l, "of an estimated 4.0 MiB)") {
			t.Errorf("line lacks the estimate: %q", l)
		}
	}
	// A send this small may finish in under 2 s: then its lines carry no pace at all.
	last := lines[len(lines)-1]
	if !pace100RE.MatchString(last) && (strings.Contains(last, "left") || strings.Contains(last, "took")) {
		t.Errorf("the 100%% line is %q, want a rate and the time taken, or no pace", last)
	}

	quiet := newE2EEnv(t)
	progressSnapshot(t, quiet)
	dest = "file://" + quiet.dest
	logs, err = guarded(t, func() (string, error) {
		return quiet.send(append(progressArgs, "--logLevel", "warning", "tank/data@a", dest)...)
	})
	if err != nil {
		t.Fatalf("send at --logLevel warning: %v\n%s", err, logs)
	}
	if lines := progressLines(logs, "Uploading tank/data@a to "+dest); len(lines) != 0 {
		t.Errorf("--logLevel warning printed progress: %q", lines)
	}
}

func TestE2EProgressResume(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	t.Setenv("FAKEZFS_FAIL_AFTER_BYTES", fmt.Sprint(5<<19)) // two whole volumes, then the stream dies
	dest := "file://" + env.dest
	args := append(append([]string{}, progressArgs...), "--maxRetryTime", "2s", "--maxBackoffTime", "1s", "tank/data@a", dest)
	if logs, err := guarded(t, func() (string, error) { return env.send(args...) }); err == nil {
		t.Fatalf("send succeeded although zfs send failed:\n%s", logs)
	}
	cached := env.cachedManifests(t)
	if len(cached) != 1 {
		t.Fatalf("want one cached manifest, got %d", len(cached))
	}
	kept, _ := cached[0].TotalBytesStreamedAndVols()
	if kept == 0 {
		t.Fatal("the interrupted send kept no volume: nothing to resume")
	}

	os.Unsetenv("FAKEZFS_FAIL_AFTER_BYTES")
	logs, err := guarded(t, func() (string, error) { return env.send(append([]string{"--resume"}, args...)...) })
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, logs)
	}
	pcts := progressPercents(t, logs, "Uploading tank/data@a to "+dest)
	wantProgressTo100(t, pcts, logs)
	if keptPct := int(kept * 100 / progressStreamBytes); pcts[0] < keptPct {
		t.Errorf("the resume's first percent is %d, below the %d%% it kept: %v", pcts[0], keptPct, pcts)
	}
}

func TestE2EProgressReceive(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	dest := "file://" + env.dest
	if logs, err := guarded(t, func() (string, error) { return env.send(append(progressArgs, "tank/data@a", dest)...) }); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	logs, err := guarded(t, func() (string, error) {
		_, rlogs, rerr := env.run(cmd.ResetReceiveJobInfo, "receive", "--zfsPath", env.self, "--workingDirectory", t.TempDir(),
			"tank/data@a", dest, "restored/data")
		return rlogs, rerr
	})
	if err != nil {
		t.Fatalf("receive: %v\n%s", err, logs)
	}
	label := "Downloading tank/data@a from " + dest
	wantProgressTo100(t, progressPercents(t, logs, label), logs)
	for _, l := range progressLines(logs, label) {
		if !strings.Contains(l, " of 4.0 MiB)") || strings.Contains(l, "estimated") {
			t.Errorf("line %q, want the exact total of 4.0 MiB", l)
		}
	}
}

func TestE2EProgressTwoDestinations(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	dest2 := filepath.Join(t.TempDir(), "dest2")
	if err := os.Mkdir(dest2, 0700); err != nil {
		t.Fatal(err)
	}
	dests := []string{"file://" + env.dest, "file://" + dest2}
	logs, err := guarded(t, func() (string, error) {
		return env.send(append(progressArgs, "tank/data@a", strings.Join(dests, ","))...)
	})
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	for _, d := range dests {
		wantProgressTo100(t, progressPercents(t, logs, "Uploading tank/data@a to "+d), logs)
	}
	if lines := progressLines(logs, "Uploading tank/data@a to delete://"); len(lines) != 0 {
		t.Errorf("the delete:// link printed progress: %q", lines)
	}
}

func TestE2EProgressPipe(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	dest := "file://" + env.dest
	logs, err := guarded(t, func() (string, error) {
		return env.send(append(progressArgs, "--maxFileBuffer", "0", "tank/data@a", dest)...)
	})
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	pcts := progressPercents(t, logs, "Uploading tank/data@a to "+dest)
	wantProgressTo100(t, pcts, logs)
	// A pipe volume counts when its upload returns: every percent is at a volume boundary.
	boundaries := volumeBoundaries(t, dest)
	for _, p := range pcts {
		if !boundaries[p] {
			t.Errorf("percent %d is not at a volume boundary %v: %v", p, boundaries, pcts)
		}
	}

	logs, err = guarded(t, func() (string, error) {
		_, rlogs, rerr := env.run(cmd.ResetReceiveJobInfo, "receive", "--zfsPath", env.self, "--workingDirectory", t.TempDir(),
			"--maxFileBuffer", "0", "tank/data@a", dest, "restored/data")
		return rlogs, rerr
	})
	if err != nil {
		t.Fatalf("receive: %v\n%s", err, logs)
	}
	wantProgressTo100(t, progressPercents(t, logs, "Downloading tank/data@a from "+dest), logs)
}

func TestE2EProgressJSONOutput(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	dest := "file://" + env.dest
	var out string
	logs, err := guarded(t, func() (string, error) {
		o, logs, err := env.run(cmd.ResetSendJobInfo, append([]string{"send", "--zfsPath", env.self, "--workingDirectory", env.work,
			"--jsonOutput"}, append(progressArgs, "tank/data@a", dest)...)...)
		out = o
		return logs, err
	})
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	if len(progressLines(logs, "Uploading tank/data@a to "+dest)) == 0 {
		t.Fatalf("no progress lines in the log:\n%s", logs)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	var v map[string]any
	if err = dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	if dec.More() || strings.TrimSpace(out[dec.InputOffset():]) != "" {
		t.Errorf("stdout holds more than one JSON object:\n%s", out)
	}
}

func TestE2EProgressNoEstimate(t *testing.T) {
	env := newE2EEnv(t)
	progressSnapshot(t, env)
	t.Setenv("FAKEZFS_DRYRUN_OUTPUT", "garbage")
	dest := "file://" + env.dest
	logs, err := guarded(t, func() (string, error) { return env.send(append(progressArgs, "tank/data@a", dest)...) })
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "Could not estimate the size of the zfs stream") {
		t.Errorf("log lacks the estimate warning:\n%s", logs)
	}
	if lines := progressLines(logs, "Uploading tank/data@a to "+dest); len(lines) != 0 {
		t.Errorf("progress with no estimate: %q", lines)
	}
}
