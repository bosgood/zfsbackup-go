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
	"testing"

	"golang.org/x/crypto/openpgp"
)

func TestParseBackupVolumeObjectName(t *testing.T) {
	key := &openpgp.Entity{}
	for _, compressor := range []string{InternalCompressor, "xz", ZfsCompressor, ""} {
		for _, encrypt := range []bool{false, true} {
			for _, incr := range []string{"", "autosnap_2026-08-01_00:00:00_monthly"} {
				j := &JobInfo{
					VolumeName:          "tank/data",
					BaseSnapshot:        SnapshotInfo{Name: "autosnap_2026-09-01_00:00:00_monthly"},
					IncrementalSnapshot: SnapshotInfo{Name: incr},
					Compressor:          compressor,
					Separator:           "|",
				}
				if encrypt {
					j.EncryptKey = key
				}
				name := j.BackupVolumeObjectName(12)
				volume, base, gotIncr, volNum, ok := ParseBackupVolumeObjectName(name, "|")
				if !ok || volume != j.VolumeName || base != j.BaseSnapshot.Name || gotIncr != incr || volNum != 12 {
					t.Errorf("%s: got %q %q %q %d %v", name, volume, base, gotIncr, volNum, ok)
				}
			}
		}
	}

	for _, name := range []string{
		"manifests|x.manifest.gz",
		"manifests|tank/data|a.manifest.gz",
		"random.bin",
		"/tank|s.zstream.vol1",
		"tank|s.zstream.vol0",
		"tank|s.zstream.vol01",
		"tank|s.zstream.volx",
		"tank|s.zstream.gz",
		"tank|s.zstream.gz.pgp.extra.vol1",
		"tank|s.zstream.pgp.gz.vol1",
		"tank.zstream.vol1",
		"tank|a|b|s.zstream.vol1",
		"|s.zstream.vol1",
		"tank|.zstream.vol1",
	} {
		if volume, base, incr, volNum, ok := ParseBackupVolumeObjectName(name, "|"); ok {
			t.Errorf("%s: parsed as %q %q %q %d", name, volume, base, incr, volNum)
		}
	}
}
