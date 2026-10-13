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

package backup

// Regression tests for the third review of the fixes of the 2026-10-12 review (R1-R3):
// docs/specs/2026-10-12--adversarial-review-plan-send-restore/findings.md.

import (
	"context"
	"crypto/md5" // nolint:gosec // the cache's file names, not cryptography
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

// R1: with -F, a target whose snapshots could not be listed for a reason other than "does not
// exist" is refused: refuseDestroy would take it for a new target and let a full replace it.
func TestReview3RefuseUnlistedTarget(t *testing.T) {
	notExist := errors.New("cannot open 'restored': dataset does not exist (exit status 1)")
	denied := errors.New("cannot open 'restored': permission denied (exit status 1)")
	for _, tc := range []struct {
		name         string
		force, allow bool
		listErr      error
		refused      bool
	}{
		{"listed", true, false, nil, false},
		{"does not exist", true, false, notExist, false},
		{"permission denied", true, false, denied, true},
		{"permission denied, --allowDestroy", true, true, denied, false},
		{"permission denied, no --force", false, false, denied, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			err := refuseUnlistedTarget(&files.JobInfo{Force: tc.force, AllowDestroy: tc.allow}, "restored", tc.listErr)
			if (err != nil) != tc.refused {
				t.Fatalf("refused = %v (%v), want %v", err != nil, err, tc.refused)
			}
			if tc.refused && !strings.Contains(logs.String(), "permission denied") {
				t.Errorf("the refusal does not name the cause:\n%s", logs)
			}
		})
	}
}

// archiveBackend is a cacheBackend whose objects in cold a GET cannot read without a PreDownload,
// which a dry run must never call.
type archiveBackend struct {
	cacheBackend
	t    *testing.T
	cold map[string]bool
}

func (a *archiveBackend) NotReadable(ctx context.Context, objects []string) ([]string, error) {
	var cold []string
	for _, name := range objects {
		if a.cold[name] {
			cold = append(cold, name)
		}
	}
	return cold, nil
}

func (a *archiveBackend) PreDownload(ctx context.Context, objects []string) error {
	a.t.Errorf("PreDownload(%v) in a run that must not restore anything", objects)
	return nil
}

// R2: a dry run (noRestore) fails when a manifest of the dataset is archived and not cached:
// without it the planner saw a hole in the chain and planned a full the real send would not. An
// archived manifest of another dataset, or one already in the cache, does not stop it.
func TestReview3DryRunArchivedManifest(t *testing.T) {
	ours, other := "manifests|tank/data|full|a", "manifests|tank/other|full|a"
	j := &files.JobInfo{ManifestPrefix: "manifests", Separator: "|"}
	run := func(t *testing.T, cold []string, cached ...string) ([]string, error) {
		t.Helper()
		cache := t.TempDir()
		for _, name := range cached {
			if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("%x", md5.Sum([]byte(name)))), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		b := &archiveBackend{
			cacheBackend: cacheBackend{objects: map[string][]byte{ours: nil, other: nil}}, t: t, cold: map[string]bool{},
		}
		for _, name := range cold {
			b.cold[name] = true
		}
		names, _, err := syncCacheMode(context.Background(), j, cache, b, true, "tank/data")
		if b.downloads != 0 {
			t.Errorf("%d downloads of an archived manifest", b.downloads)
		}
		return names, err
	}

	t.Run("ours archived", func(t *testing.T) {
		_, err := run(t, []string{ours, other})
		var archived *archivedManifestsError
		if !errors.As(err, &archived) || !strings.Contains(err.Error(), ours) || strings.Contains(err.Error(), other) {
			t.Errorf("got %v; want an archivedManifestsError that names %s only", err, ours)
		}
	})
	t.Run("other archived", func(t *testing.T) {
		names, err := run(t, []string{other}, ours)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != ours {
			t.Errorf("names %v, want %s only", names, ours)
		}
	})
	t.Run("ours archived and cached", func(t *testing.T) {
		if _, err := run(t, []string{ours, other}, ours); err != nil {
			t.Errorf("a cached archived manifest stopped the run: %v", err)
		}
	})
}

// R3: where the host has no boot_id (FreeBSD, macOS), the boot time names the boot: a lock from
// another boot is stale there too.
func TestReview3BootLockUsesBootTime(t *testing.T) {
	live := os.Getppid() // a pid this user can signal: lockfile sees it as running
	lock := lockHeldBy(t, live, live, "boottime-1.000000", "")
	platformBootID = func() string { return "boottime-2.000000" }
	if err := lock.TryLock(); err != nil {
		t.Fatalf("TryLock of a lock from another boot time: %v", err)
	}
	data, err := os.ReadFile(lock.bootFile())
	if err != nil || string(data) != fmt.Sprintf("%d boottime-2.000000\n", os.Getpid()) {
		t.Errorf("boot file holds %q (%v), want this process and this boot time", data, err)
	}
	if err = lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// bootTimeID is stable within one boot: two reads name the same boot.
func TestReview3BootTimeIDStable(t *testing.T) {
	if a, b := bootTimeID(), bootTimeID(); a != b {
		t.Errorf("bootTimeID changed within one boot: %q, %q", a, b)
	}
}
