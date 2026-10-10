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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/files"
)

// A --resume to dest1,dest2 must not trust dest2's volumes on dest1's word. Attempt A sends the
// set to both; it is then made to look interrupted. Attempt B, a plain send to dest1 alone,
// rewrites dest1's volumes (the encryption makes them differ from A's) and fails. Attempt C
// resumes to both: dest2 still holds A's volumes under the same names and sizes, so only its own
// cache can tell them apart from B's, and the manifest C publishes there must restore.
func TestE2EResumeDoesNotTrustOtherDestinationsCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail is the object at dest1 whose upload attempt B fails at, given A's manifest name.
		fail func(t *testing.T, dest, manifest string) string
		// drop is the first volume removed after attempt A; 0 keeps them all.
		drop int64
	}{
		{
			name: "partial manifest",
			fail: func(t *testing.T, dest, _ string) string {
				return strings.TrimSuffix(volumeNamed(t, dest, 2), "2") + "3"
			},
			drop: 3,
		},
		{
			name: "final manifest",
			fail: func(t *testing.T, dest, manifest string) string { return filepath.Join(dest, manifest) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const streamBytes = 3 << 20
			env := newE2EEnv(t)
			dest2 := newDest(t)
			rings := writeRings(t, newKey(t))
			t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(streamBytes))
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
			dests := "file://" + env.dest + ",file://" + dest2
			args := append([]string{"--volsize", "1", "--compressor", "", "--maxRetryTime", "2s", "--maxBackoffTime", "1s"}, rings...)

			env.sendOK(t, append(args, "tank/data@a", dests)...)
			manifests := manifestNames(destObjects(t, env.dest))
			if len(manifests) != 1 {
				t.Fatalf("want one manifest at dest1, got %q", manifests)
			}
			for _, d := range []string{env.dest, dest2} {
				if err := os.Remove(filepath.Join(d, manifests[0])); err != nil {
					t.Fatal(err)
				}
				if tc.drop > 0 {
					dropVolumesFrom(t, d, tc.drop)
				}
			}

			// A directory in place of the object: its upload fails.
			blocked := tc.fail(t, env.dest, manifests[0])
			if err := os.Mkdir(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			logs, err := guarded(t, func() (string, error) { return env.send(append(args, "tank/data@a", "file://"+env.dest)...) })
			if err == nil {
				t.Fatalf("attempt B succeeded:\n%s", logs)
			}
			if err = os.Remove(blocked); err != nil {
				t.Fatal(err)
			}

			logs, err = guarded(t, func() (string, error) { return env.send(append(args, "--resume", "tank/data@a", dests)...) })
			if err != nil {
				t.Fatalf("attempt C: %v\n%s", err, logs)
			}

			for _, d := range []string{env.dest, dest2} {
				t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
				rargs := append(append([]string{}, rings...), "--maxRetryTime", "2s", "tank/data@a", "file://"+d, "restored/data")
				if logs, err = guarded(t, func() (string, error) { return env.receive(rargs...) }); err != nil {
					t.Errorf("the resume exited 0 but a restore from %s fails: %v\n%s", d, err, logs)
				}
			}
		})
	}
}
