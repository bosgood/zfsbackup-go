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
package files

import (
	"context"
	"crypto/md5" // nolint:gosec // test data only
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/someone1/zfsbackup-go/config"
)

// realisticJob is a JobInfo of a full send of tank/data with n volumes of realistic field values:
// the fields a send records (64-hex SHA256Sum and StreamSHA256, 32-hex MD5Sum, a CRC, ~200 MiB
// sizes, nanosecond times in a zone with an offset, the object names BackupVolumeObjectName gives).
func realisticJob(n int) *JobInfo {
	zone := time.FixedZone("PDT", -7*3600)
	base := time.Date(2026, 9, 1, 0, 0, 3, 0, zone)
	j := &JobInfo{
		StartTime:        base.Add(time.Minute),
		VolumeName:       "tank/data",
		BaseSnapshot:     SnapshotInfo{Name: "autosnap_2026-09-01_00:00:03_monthly", CreationTime: base, GUID: "1234567890123456789"},
		ManifestPrefix:   "manifests",
		Separator:        "|",
		Compressor:       InternalCompressor,
		CompressionLevel: 6,
		VolumeSize:       200,
		ZFSCommandLine:   "zfs send -p tank/data@autosnap_2026-09-01_00:00:03_monthly",
		Version:          1.0,
	}
	j.Volumes = make([]*VolumeInfo, 0, n)
	for i := 1; i <= n; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("vol%d", i)))
		stream := sha256.Sum256(h[:])
		m := md5.Sum(h[:]) // nolint:gosec // test data only
		start := base.Add(time.Duration(i)*7*time.Second + time.Duration(i%1000)*time.Microsecond + 987654321*time.Nanosecond)
		size := uint64(200<<20) + uint64(i%4096)
		j.Volumes = append(j.Volumes, &VolumeInfo{
			ObjectName:     j.BackupVolumeObjectName(int64(i)),
			VolumeNumber:   int64(i),
			SHA256Sum:      hex.EncodeToString(h[:]),
			MD5Sum:         hex.EncodeToString(m[:]),
			CRC32CSum32:    binary.BigEndian.Uint32(h[:4]),
			Size:           size,
			ZFSStreamBytes: size,
			StreamSHA256:   hex.EncodeToString(stream[:]),
			CreateTime:     start,
			CloseTime:      start.Add(6*time.Second + 123456789*time.Nanosecond),
		})
	}
	j.ZFSStreamBytes = uint64(n) * (200 << 20)
	j.EndTime = base.Add(time.Duration(n)*7*time.Second + time.Minute)
	return j
}

// writeManifestOf writes j the way backup.saveManifest does (CreateManifestVolume + json.Encoder,
// Close, CopyTo) and returns the path of the object.
func writeManifestOf(t *testing.T, j *JobInfo) string {
	t.Helper()
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	t.Cleanup(func() { config.BackupTempdir = oldTemp })
	manifest, err := CreateManifestVolume(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manifest.DeleteVolume() }()
	if err = json.NewEncoder(manifest).Encode(j); err != nil {
		t.Fatal(err)
	}
	if err = manifest.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest")
	if err = manifest.CopyTo(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// setManifestLimit lowers MaxManifestBytes for the test and restores it after. The limit is a package
// variable: a test that calls this must not call t.Parallel.
func setManifestLimit(t *testing.T, limit int) {
	t.Helper()
	old := MaxManifestBytes
	MaxManifestBytes = limit
	t.Cleanup(func() { MaxManifestBytes = old })
}

// A send of 200,000 volumes (38 TiB at the default --volsize of 200 MiB; 100 MB of JSON) is a
// backup, not a forgery: its manifest must read back. The 64 MiB limit rejected it at 128,558
// volumes (docs/specs/2026-10-10--high-risk-review/findings.md, Item 2).
func TestReadManifestLargeSend(t *testing.T) {
	const n = 200000
	path := writeManifestOf(t, realisticJob(n))
	got, err := ReadManifest(context.Background(), &JobInfo{}, path)
	if err != nil {
		t.Fatalf("the manifest of a %d-volume send does not read back: %v", n, err)
	}
	if len(got.Volumes) != n {
		t.Fatalf("read back %d volumes, want %d", len(got.Volumes), n)
	}
}

// A manifest over the limit is ErrManifestTooLong, so readers can tell it from a forgery.
func TestReadManifestTooLong(t *testing.T) {
	setManifestLimit(t, 4096)
	path := writeRawManifest(t, `{"VolumeName":"`+strings.Repeat("x", 5000)+`"}`)
	_, err := ReadManifest(context.Background(), &JobInfo{}, path)
	if !errors.Is(err, ErrManifestTooLong) {
		t.Fatalf("got %v, want ErrManifestTooLong", err)
	}
	if !strings.Contains(err.Error(), "4096 bytes") {
		t.Errorf("the error does not say the limit: %v", err)
	}
	path = writeRawManifest(t, `{"VolumeName":"`+strings.Repeat("x", 4000)+`"}`)
	if _, err = ReadManifest(context.Background(), &JobInfo{}, path); err != nil {
		t.Fatalf("a manifest under the limit: %v", err)
	}
}
