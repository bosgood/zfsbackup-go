//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestIntegrationSSHTwoDestinations sends one set to a file:// and an ssh://
// destination at once. A truncated volume at the ssh one stops --resume from
// completing the set there (the size check of the complete-partial path);
// once the volume is back, it completes, and each destination restores alone.
func TestIntegrationSSHTwoDestinations(t *testing.T) {
	pool := testPool(t)
	itSSHClearEnv(t)
	a := newFileDest(t, pool, "dest-a")
	b := newSSHDest(t, pool, "dest-b")
	src := pool + "/sshtwosrc"
	newSource(t, src, 3<<20)
	snap := snapshot(t, src, "daily")
	work := t.TempDir()
	args := append(slices.Clone(plainSend), append(slices.Clone(sshRetry), src+"@"+snap, a.target+","+b.target)...)
	mtimes := func(dir string) map[string]time.Time {
		t.Helper()
		m := map[string]time.Time{}
		for _, n := range fileNames(t, dir) {
			fi, err := os.Stat(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			m[n] = fi.ModTime()
		}
		return m
	}

	mustSend(t, work, args...)
	names := fileNames(t, a.dir)
	wantFilesAt(t, b.dir, names)
	m := manifestFor(t, work, src, a.target, snap)
	if len(m.Volumes) < 3 {
		t.Fatalf("%d volumes, want 3+", len(m.Volumes))
	}
	wantVolumeSizes(t, a.dir, m)
	wantVolumeSizes(t, b.dir, m)

	// Volume 1 cut short and the manifest lost at b: --resume must not copy
	// the manifest over a volume it does not describe.
	vol1 := m.Volumes[0]
	half := vol1.Size / 2
	truncateFile(t, filepath.Join(b.dir, vol1.ObjectName), int64(half))
	manifest := manifestFiles(names)
	if err := os.Remove(filepath.Join(b.dir, manifest[0])); err != nil {
		t.Fatal(err)
	}
	beforeA := mtimes(a.dir)
	logs, err := itSend(t, work, append([]string{"--resume"}, args...)...)
	want := fmt.Sprintf("its volume %s there is not the one the manifest at %s describes (%d bytes, SHA256 not read; want %d bytes",
		vol1.ObjectName, a.target, half, vol1.Size)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("send --resume with a short volume at %s: got %v, want %q\n%s", b.target, err, want, logs)
	}
	if got := mtimes(a.dir); fmt.Sprint(got) != fmt.Sprint(beforeA) {
		t.Errorf("the failed send changed %s:\n%v\nwant:\n%v", a.dir, got, beforeA)
	}

	// The volume is back: --resume copies the manifest and nothing else.
	copyFile(t, filepath.Join(a.dir, vol1.ObjectName), filepath.Join(b.dir, vol1.ObjectName))
	logs, err = itSend(t, work, append([]string{"--resume"}, args...)...)
	if err != nil {
		t.Fatalf("send --resume with the volume back: %v\n%s", err, logs)
	}
	wantInLog(t, logs, "Completed backup set "+manifest[0]+" at "+b.target)
	wantFilesAt(t, b.dir, names)
	if got := mtimes(a.dir); fmt.Sprint(got) != fmt.Sprint(beforeA) {
		t.Errorf("completing %s changed %s:\n%v\nwant:\n%v", b.target, a.dir, got, beforeA)
	}

	restoreAll(t, src, a.target, pool+"/sshtwoRestoredA", snap)
	restoreAll(t, src, b.target, pool+"/sshtwoRestoredB", snap)
}
