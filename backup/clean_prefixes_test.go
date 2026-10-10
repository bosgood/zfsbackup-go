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

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"

	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/pgp"
)

// Two jobs that share a destination under different --manifestPrefix values
// (docs/specs/2026-10-10--high-risk-review/findings.md, Item 1 and Other 1).

// prefixedSet is cleanTestSet with an optional incremental source and encryption key. EncryptTo
// is set as a send would, so the decoded manifest's StoredManifestObjectName matches its name.
func prefixedSet(
	t *testing.T, dir, dataset, snapshot, incremental, prefix string, key *openpgp.Entity, email string,
) (*files.JobInfo, string) {
	t.Helper()
	set := &files.JobInfo{
		VolumeName:          dataset,
		BaseSnapshot:        files.SnapshotInfo{Name: snapshot},
		IncrementalSnapshot: files.SnapshotInfo{Name: incremental},
		ManifestPrefix:      prefix,
		Separator:           "|",
		Compressor:          files.InternalCompressor,
		EncryptKey:          key,
		EncryptTo:           email,
	}
	vol := set.BackupVolumeObjectName(1)
	set.Volumes = []*files.VolumeInfo{{ObjectName: vol, VolumeNumber: 1}}
	writeTestManifest(t, dir, set)
	return set, writeTestObject(t, dir, vol)
}

// makeLocalOnly caches set's manifest (a clean under its prefix downloads it), then removes the
// manifest object from the destination so the cached copy is local-only. jobInfo's prefix and key
// are restored. It returns the cached file's path.
func makeLocalOnly(t *testing.T, targetDir string, jobInfo, set *files.JobInfo) string {
	t.Helper()
	savedPrefix, savedKey := jobInfo.ManifestPrefix, jobInfo.EncryptKey
	jobInfo.ManifestPrefix, jobInfo.EncryptKey = set.ManifestPrefix, set.EncryptKey
	if err := Clean(context.Background(), jobInfo, false, false); err != nil {
		t.Fatalf("clean under prefix %s (to cache its manifest): %v", set.ManifestPrefix, err)
	}
	jobInfo.ManifestPrefix, jobInfo.EncryptKey = savedPrefix, savedKey
	object := set.ManifestObjectName()
	if err := os.Remove(filepath.Join(targetDir, object)); err != nil {
		t.Fatal(err)
	}
	cache, err := getCacheDir(jobInfo, jobInfo.Destinations[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, cachedManifestName(object))
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("expected the %s manifest in the cache: %v", set.ManifestPrefix, err)
	}
	return path
}

// truncateHalf cuts the file at path to half its size.
func truncateHalf(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(path, info.Size()/2); err != nil {
		t.Fatal(err)
	}
	if _, err = readManifest(context.Background(), path, &files.JobInfo{}); err == nil {
		t.Fatalf("the truncated manifest %s still decodes; the test needs an undecodable one", path)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}

// cleanLocalRefuses runs clean --cleanLocal as a dry run and for real. An undecodable local-only
// manifest cannot be told from another job's, so both must refuse naming it, say what to do, and
// delete nothing: not the cached manifest, not the volume. The error must not point at
// --cleanLocal, which is what the user just passed. It returns the real run's error.
func cleanLocalRefuses(t *testing.T, jobInfo *files.JobInfo, cached, volume string) error {
	t.Helper()
	var last error
	for _, dryRun := range []bool{true, false} {
		logs := captureLogs(t)
		err := Clean(context.Background(), jobInfo, true, dryRun)
		if err == nil || !strings.Contains(err.Error(), cached) {
			t.Fatalf("dryRun=%v: got %v, want a refusal naming the cached manifest %s", dryRun, err, cached)
		}
		if strings.Contains(err.Error(), "--cleanLocal") || strings.Contains(logs.String(), "--cleanLocal") {
			t.Errorf("dryRun=%v: the refusal points at --cleanLocal, which was passed:\n%v\n%s", dryRun, err, logs.String())
		}
		if strings.Contains(logs.String(), "Would delete") {
			t.Errorf("dryRun=%v: announced a delete:\n%s", dryRun, logs.String())
		}
		if !exists(t, cached) {
			t.Fatalf("dryRun=%v: clean --cleanLocal deleted the cached manifest %s", dryRun, cached)
		}
		if !exists(t, volume) {
			t.Fatalf("dryRun=%v: clean --cleanLocal deleted the volume %s", dryRun, volume)
		}
		last = err
	}
	return last
}

// TestCleanLocalKeepsTruncatedOtherPrefixManifest: dataset tank/a is live under this prefix; the
// manifest of tank/b under prefix p2 is cached, gone from the destination, and truncated.
func TestCleanLocalKeepsTruncatedOtherPrefixManifest(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	prefixedSet(t, targetDir, "tank/a", "a", "", "manifests", nil, "")
	setB, volB := prefixedSet(t, targetDir, "tank/b", "a", "", "p2", nil, "")
	cached := makeLocalOnly(t, targetDir, jobInfo, setB)
	truncateHalf(t, cached)

	err := cleanLocalRefuses(t, jobInfo, cached, volB)
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("the refusal does not say how to get rid of the file: %v", err)
	}
}

// TestCleanLocalKeepsTruncatedOtherPrefixManifestSameDataset: as above, but both sets are of
// tank/data (snapshot a under this prefix, b under p2), so b's volume is an orphan of a known
// dataset unless the unreadable manifest stops the run.
func TestCleanLocalKeepsTruncatedOtherPrefixManifestSameDataset(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	prefixedSet(t, targetDir, "tank/data", "a", "", "manifests", nil, "")
	setB, volB := prefixedSet(t, targetDir, "tank/data", "b", "", "p2", nil, "")
	cached := makeLocalOnly(t, targetDir, jobInfo, setB)
	truncateHalf(t, cached)

	_ = cleanLocalRefuses(t, jobInfo, cached, volB)
}

// --- key helpers (copied from files/pgp_test.go, which backup/ cannot import) ---

func prefixedKey(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("backup", "", email, &packet.Config{RSABits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range e.Identities {
		id.SelfSignature.PreferredHash = []uint8{8}
		id.SelfSignature.PreferredSymmetric = []uint8{9}
	}
	if err = e.SerializePrivate(io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func loadPrefixedRings(t *testing.T, keys ...*openpgp.Entity) {
	t.Helper()
	dir := t.TempDir()
	for _, ring := range []struct {
		kind    string
		private bool
		load    func(string) error
	}{
		{openpgp.PublicKeyType, false, pgp.LoadPublicRing},
		{openpgp.PrivateKeyType, true, pgp.LoadPrivateRing},
	} {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, ring.kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range keys {
			if ring.private {
				err = e.SerializePrivate(w, nil)
			} else {
				err = e.Serialize(w)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		path := filepath.Join(dir, ring.kind)
		if err = os.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err = ring.load(path); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCleanLocalKeepsOtherKeysManifest: the p2 set (tank/data@b) is encrypted to K2, this
// prefix's set (tank/data@a) to K1, and the clean run holds K1 only. The cached p2 manifest is
// another job's: the refusal must say it is a key problem, not a damaged file.
func TestCleanLocalKeepsOtherKeysManifest(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	k1, k2 := prefixedKey(t, "k1@example.com"), prefixedKey(t, "k2@example.com")
	loadPrefixedRings(t, k1, k2)
	t.Cleanup(func() { loadPrefixedRings(t) }) // leave the package rings empty

	jobInfo.EncryptKey, jobInfo.EncryptTo = k1, "k1@example.com"
	prefixedSet(t, targetDir, "tank/data", "a", "", "manifests", k1, "k1@example.com")
	setB, volB := prefixedSet(t, targetDir, "tank/data", "b", "", "p2", k2, "k2@example.com")
	cached := makeLocalOnly(t, targetDir, jobInfo, setB)

	loadPrefixedRings(t, k1)
	if _, rerr := readManifest(context.Background(), cached, jobInfo); !errors.As(rerr, new(*files.KeyError)) {
		t.Fatalf("reading the K2 manifest with K1 only: got %v, want a KeyError", rerr)
	}

	err := cleanLocalRefuses(t, jobInfo, cached, volB)
	if !strings.Contains(err.Error(), "key") || !strings.Contains(err.Error(), "another job") {
		t.Errorf("the refusal does not name the key problem: %v", err)
	}
}

// TestCleanSparesOtherPrefixSetsOfSameDataset: two jobs back up tank/data to one destination,
// one under the default prefix (a full of @a) and one under p2 (a full of @b and an incremental
// a..b). clean under either prefix never reads the other's manifests, but their names are at the
// destination: a volume of a set that a manifest under another prefix names is not an orphan.
// A volume of a set no manifest names is.
func TestCleanSparesOtherPrefixSetsOfSameDataset(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	_, volA := prefixedSet(t, targetDir, "tank/data", "a", "", "manifests", nil, "")
	_, volB := prefixedSet(t, targetDir, "tank/data", "b", "", "p2", nil, "")
	_, volAB := prefixedSet(t, targetDir, "tank/data", "b", "a", "p2", nil, "")
	orphanSet := &files.JobInfo{VolumeName: "tank/data", BaseSnapshot: files.SnapshotInfo{Name: "c"},
		Separator: "|", Compressor: files.InternalCompressor}
	orphan := writeTestObject(t, targetDir, orphanSet.BackupVolumeObjectName(1))

	// "manifests" first: once a clean under p2 has cached p2's manifests, a clean under
	// "manifests" spares their volumes as foreign cached manifests, and the names are not tested.
	for _, prefix := range []string{"manifests", "p2"} {
		jobInfo.ManifestPrefix = prefix
		for _, dryRun := range []bool{true, false} {
			logs := captureLogs(t)
			if err := Clean(context.Background(), jobInfo, false, dryRun); err != nil {
				t.Fatalf("clean --manifestPrefix %s dryRun=%v: %v", prefix, dryRun, err)
			}
			for _, kept := range []string{volA, volB, volAB} {
				if strings.Contains(logs.String(), "Would delete "+joinURI(jobInfo.Destinations[0], strings.TrimPrefix(kept, targetDir+"/"))) {
					t.Errorf("clean --manifestPrefix %s dryRun=%v announced deleting %s:\n%s", prefix, dryRun, kept, logs.String())
				}
				if !exists(t, kept) {
					t.Fatalf("clean --manifestPrefix %s dryRun=%v deleted %s", prefix, dryRun, kept)
				}
			}
			if dryRun != exists(t, orphan) {
				t.Errorf("clean --manifestPrefix %s dryRun=%v: orphan %s exists=%v", prefix, dryRun, orphan, exists(t, orphan))
			}
		}
		orphan = writeTestObject(t, targetDir, orphanSet.BackupVolumeObjectName(1))
	}
}

// TestCleanSparesOtherPrefixSetsWithManifestInName: a prefix or snapshot name may hold
// ".manifest". The set a manifest under another prefix is for is read from the name's last
// ".manifest", so the volumes of that set are spared all the same.
func TestCleanSparesOtherPrefixSetsWithManifestInName(t *testing.T) {
	targetDir, jobInfo := setupCleanTest(t)
	_, volA := prefixedSet(t, targetDir, "tank/data", "a", "", "manifests", nil, "")
	_, volB := prefixedSet(t, targetDir, "tank/data", "b", "", "old.manifests", nil, "")
	_, volC := prefixedSet(t, targetDir, "tank/data", "daily.manifest-1", "", "p2", nil, "")
	_, volCD := prefixedSet(t, targetDir, "tank/data", "d", "daily.manifest-1", "p2", nil, "")

	for _, dryRun := range []bool{true, false} {
		logs := captureLogs(t)
		if err := Clean(context.Background(), jobInfo, false, dryRun); err != nil {
			t.Fatalf("clean dryRun=%v: %v", dryRun, err)
		}
		for _, kept := range []string{volA, volB, volC, volCD} {
			if strings.Contains(logs.String(), "Would delete "+joinURI(jobInfo.Destinations[0], strings.TrimPrefix(kept, targetDir+"/"))) {
				t.Errorf("clean dryRun=%v announced deleting %s:\n%s", dryRun, kept, logs.String())
			}
			if !exists(t, kept) {
				t.Fatalf("clean dryRun=%v deleted %s", dryRun, kept)
			}
		}
	}
}

func TestOtherPrefixSets(t *testing.T) {
	for obj, want := range map[string]string{
		"p2|tank|a.manifest.gz":                             "|tank|a",
		"old.manifests|tank|b.manifest.gz.pgp":              "|tank|b",
		"p2|tank|daily.manifest-1.manifest.gz":              "|tank|daily.manifest-1",
		"p2|tank|daily.manifest-1|to|d.manifest.gz":         "|tank|daily.manifest-1|to|d",
		"p.manifest|tank|x.manifest|to|y.manifest.manifest": "|tank|x.manifest|to|y.manifest",
	} {
		if got := otherPrefixSets([]string{obj}, "manifests|", []string{"|"}); got[want] != obj {
			t.Errorf("otherPrefixSets(%q) = %v, want key %q", obj, got, want)
		}
	}
}
