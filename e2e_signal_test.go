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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bosgood/zfsbackup-go/cmd"
	"github.com/bosgood/zfsbackup-go/files"
)

// TestE2ESendSIGTERMStopsCleanly: a SIGTERM mid-send stops the pipeline, releases the lock,
// removes the temp files and publishes no manifest; a --resume then completes the set.
// The test process receives a real SIGTERM: it survives only because SignalContext registers
// the handler first. Never send a second signal here; that would exit the test binary.
func TestE2ESendSIGTERMStopsCleanly(t *testing.T) {
	const streamBytes = 4 << 20
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	args := []string{"--volsize", "1", "--compressor", "", "tank/data@a", "file://" + env.dest}

	ctx, stop := cmd.SignalContext(context.Background())
	defer stop()
	type result struct {
		logs string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		// 256 KiB/s: a 1 MiB volume every 4 s, so the SIGTERM lands mid-send. One file buffer:
		// zfs send is still running then, so the cancel kills it.
		logs, err := guarded(t, func() (string, error) {
			return env.sendCtx(ctx, append([]string{"--maxUploadSpeed", "256", "--maxFileBuffer", "1"}, args...)...)
		})
		done <- result{logs, err}
	}()

	// Wait for the first volume to finish the pipeline: then a partial manifest is in the cache.
	// (A .vol1 at the destination is no sign: the file backend writes it under its final name.)
	deadline := time.Now().Add(10 * time.Second)
	for !hasPartialManifest(t, env.work) {
		if time.Now().After(deadline) {
			t.Fatalf("no partial manifest in the cache after 10 s; the destination holds %d objects", len(destObjects(t, env.dest)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r := <-done

	if r.err == nil {
		t.Errorf("the interrupted send returned no error:\n%s", r.logs)
	}
	if want := "Received SIGTERM; stopping. Nothing more is uploaded. A send continues with --resume."; !strings.Contains(r.logs, want) {
		t.Errorf("log lacks %q:\n%s", want, r.logs)
	}
	// The signal's Notice is the one line about the stop: the stages do not each report the cancel.
	for _, noise := range []string{"signal: killed", "Could not kill"} {
		if strings.Contains(r.logs, noise) {
			t.Errorf("log reports the cancel's echo %q:\n%s", noise, r.logs)
		}
	}
	if names := manifestNames(destObjects(t, env.dest)); len(names) != 0 {
		t.Errorf("the interrupted send published manifests %q", names)
	}
	if _, err := os.Stat(env.lockFile("tank/data")); !os.IsNotExist(err) {
		t.Errorf("the lock file is still there (stat: %v)", err)
	}
	if entries, err := os.ReadDir(filepath.Join(env.work, "temp")); err != nil || len(entries) != 0 {
		t.Errorf("temp dir holds %v (%v), want nothing", entries, err)
	}

	logs, err := guarded(t, func() (string, error) { return env.send(append([]string{"--resume"}, args...)...) })
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, logs)
	}
	if want := "Resuming from volume"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
	env.checkRestores(t, "file://"+env.dest, "a", streamBytes)
}

// hasPartialManifest reports whether the working directory's cache holds a manifest.
func hasPartialManifest(t *testing.T, work string) bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(work, "cache", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths) > 0
}
