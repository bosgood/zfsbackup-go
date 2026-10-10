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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// --resume with several destinations trusts no single destination's cache
// (docs/specs/2026-10-10--high-risk-review/findings.md, Item 3: refuted, tests ported).

const otherDestStreamBytes = 3 << 20

var otherDestArgs = []string{"--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s"}

// interruptedAtAll sends tank/data@a to dirs (3 volumes, no encryption) and makes it look
// interrupted after volume 2 at every destination: no manifest, no volume 3, a cache of all 3.
func interruptedAtAll(t *testing.T, env *e2eEnv, dirs []string) {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(otherDestStreamBytes))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	uris := make([]string, len(dirs))
	for i, d := range dirs {
		uris[i] = "file://" + d
	}
	env.sendOK(t, append(append([]string{}, otherDestArgs...), "tank/data@a", strings.Join(uris, ","))...)
	for _, d := range dirs {
		names := manifestNames(destObjects(t, d))
		if len(names) != 1 {
			t.Fatalf("want one manifest at %s, got %q", d, names)
		}
		if err := os.Remove(filepath.Join(d, names[0])); err != nil {
			t.Fatal(err)
		}
		dropVolumesFrom(t, d, 3)
	}
}

// blockVolume puts a directory where volume n of the set would be written at dir, so an upload
// of it fails. It returns a function that removes the directory again.
func blockVolume(t *testing.T, dir string, n int64) func() {
	t.Helper()
	blocked := strings.TrimSuffix(volumeNamed(t, dir, 2), "2") + fmt.Sprint(n)
	if n == 1 || n == 2 {
		blocked = volumeNamed(t, dir, n)
		if err := os.Remove(blocked); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(blocked); err != nil {
			t.Fatal(err)
		}
	}
}

// changedObjects counts the objects in before whose bytes differ in after.
func changedObjects(before, after map[string][]byte) int {
	changed := 0
	for name, data := range before {
		if got, ok := after[name]; ok && !bytes.Equal(got, data) {
			changed++
		}
	}
	return changed
}

func wantInLog(t *testing.T, logs string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

// Attempt 1 to (a,b) is interrupted after volume 2. A plain send to ONE of them rewrites volumes
// 1-2 there with other bytes of the same size (FAKEZFS_STREAM_SALT, no encryption) and fails at
// volume 3, so that destination's cache records the new SHA-256s. A --resume to (a,b) must see
// the caches differ and start over, whichever destination is first.
func TestE2EResumeCacheOfOtherDestinationCuts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int // index into (a,b) of the destination the plain send goes to
	}{
		{"rewritten destination is first", 0},
		{"rewritten destination is second", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			dirs := []string{env.dest, newDest(t)}
			interruptedAtAll(t, env, dirs)
			target := dirs[tc.target]

			t.Setenv("FAKEZFS_STREAM_SALT", "salt")
			unblock := blockVolume(t, target, 3)
			before := destObjects(t, target)
			logs, err := guarded(t, func() (string, error) {
				return env.send(append(append([]string{}, otherDestArgs...), "tank/data@a", "file://"+target)...)
			})
			if err == nil {
				t.Fatalf("the plain send to %s succeeded:\n%s", target, logs)
			}
			wantInLog(t, logs, "Discarding the cached state of an earlier attempt")
			if changedObjects(before, destObjects(t, target)) != 2 {
				t.Fatalf("the plain send did not rewrite volumes 1-2 at %s; the scenario needs it to", target)
			}
			unblock()
			t.Setenv("FAKEZFS_STREAM_SALT", "")

			dests := "file://" + dirs[0] + ",file://" + dirs[1]
			logs, err = guarded(t, func() (string, error) {
				return env.send(append(append([]string{}, otherDestArgs...), "--resume", "tank/data@a", dests)...)
			})
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, logs)
			}
			wantInLog(t, logs,
				fmt.Sprintf("The cached state for file://%s differs from the one for file://%s at volume ", dirs[1], dirs[0]),
				"Nothing verifiable to resume; starting over.",
			)
			for _, d := range dirs {
				env.checkRestores(t, "file://"+d, "a", otherDestStreamBytes)
			}
		})
	}
}

// The plain send to a alone fails before any volume is cached (volume 1's upload fails).
// discardPartialManifests already removed a's cache, so a later --resume starts over: with a
// first, "No previous manifest file exists" (and b's cache is discarded too); with b first, b's
// cache is the old one but a has none, and nothing is kept.
func TestE2EResumeUncachedDestinationStartsOver(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []int
		wants []string
	}{
		{"resume lists a first", []int{0, 1}, []string{
			"No previous manifest file exists, nothing to resume; starting over.",
			"Discarding the cached state of an earlier attempt",
		}},
		{"resume lists b first", []int{1, 0}, []string{
			"No cached state of the interrupted attempt for file://%s; every volume will be re-sent.",
			"Nothing verifiable to resume; starting over.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newE2EEnv(t)
			dirs := []string{env.dest, newDest(t)}
			interruptedAtAll(t, env, dirs)
			a := dirs[0]
			if n := len(env.cachedManifests(t)); n != 2 {
				t.Fatalf("want a cache for both destinations, got %d", n)
			}

			t.Setenv("FAKEZFS_STREAM_SALT", "salt")
			vol1 := volumeNamed(t, a, 1)
			orig, err := os.ReadFile(vol1)
			if err != nil {
				t.Fatal(err)
			}
			unblock := blockVolume(t, a, 1)
			logs, err := guarded(t, func() (string, error) {
				return env.send(append(append([]string{}, otherDestArgs...), "tank/data@a", "file://"+a)...)
			})
			if err == nil {
				t.Fatalf("the plain send to a succeeded:\n%s", logs)
			}
			wantInLog(t, logs, "Discarding the cached state of an earlier attempt")
			if n := len(env.cachedManifests(t)); n != 1 {
				t.Fatalf("after the failed plain send want only b's cache, got %d caches", n)
			}
			unblock()
			// Put attempt 1's volume 1 back: a now holds the old bytes but has no cache.
			if err = os.WriteFile(vol1, orig, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKEZFS_STREAM_SALT", "")

			dests := "file://" + dirs[tc.order[0]] + ",file://" + dirs[tc.order[1]]
			logs, err = guarded(t, func() (string, error) {
				return env.send(append(append([]string{}, otherDestArgs...), "--resume", "tank/data@a", dests)...)
			})
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, logs)
			}
			for _, w := range tc.wants {
				if strings.Contains(w, "%s") {
					w = fmt.Sprintf(w, a)
				}
				wantInLog(t, logs, w)
			}
			if strings.Contains(logs, "Resuming from volume 2") || strings.Contains(logs, "Resuming from volume 3") {
				t.Errorf("the resume kept volumes:\n%s", logs)
			}
			for _, d := range dirs {
				env.checkRestores(t, "file://"+d, "a", otherDestStreamBytes)
			}
		})
	}
}
