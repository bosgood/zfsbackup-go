package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

// TestVerifiedVolumesCacheGap: parallel uploads that failed leave a gap in the cached partial
// manifest (volumes 1, 2 and 4 cached, 3 not; all of them at the destination). --resume keeps
// volumes 1 and 2, and its Notice names volume 3, the one not cached. It named volume 4, which
// is there, as "missing".
func TestVerifiedVolumesCacheGap(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	dir := t.TempDir()
	j := &files.JobInfo{
		VolumeName:   "tank/data",
		BaseSnapshot: files.SnapshotInfo{Name: "a"},
		Compressor:   files.InternalCompressor,
		Separator:    files.DefaultSeparator,
	}
	var cached []*files.VolumeInfo
	for _, n := range []int64{1, 2, 3, 4} {
		name := j.BackupVolumeObjectName(n)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("volume"), 0o600); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			cached = append(cached, &files.VolumeInfo{ObjectName: name, VolumeNumber: n, Size: uint64(len("volume"))})
		}
	}
	uri := "file://" + dir
	backend, err := prepareBackend(ctx, j, uri, make(chan bool, 1))
	if err != nil {
		t.Fatal(err)
	}

	kept, err := verifiedVolumes(ctx, j, cached, []destination{{uri: uri, backend: backend}})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || kept[0].VolumeNumber != 1 || kept[1].VolumeNumber != 2 {
		t.Fatalf("kept %d volumes, want volumes 1 and 2", len(kept))
	}
	want := "Volume " + j.BackupVolumeObjectName(3) + " is not in the cached state of the interrupted attempt"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
	if bad := "Volume " + j.BackupVolumeObjectName(4) + " missing"; strings.Contains(logs.String(), bad) {
		t.Errorf("log says %q, and that volume is at the destination:\n%s", bad, logs)
	}
}
