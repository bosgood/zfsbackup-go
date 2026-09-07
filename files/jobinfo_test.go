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
	"strings"
	"testing"
)

// validSendJob returns a JobInfo that passes ValidateSendFlags, so each test
// case below isolates the one field it changes.
func validSendJob() *JobInfo {
	return &JobInfo{
		MaxFileBuffer:      5,
		MaxParallelUploads: 4,
		MaxRetryTime:       12 * 60,
		MaxBackoffTime:     30,
		CompressionLevel:   6,
		Separator:          "|",
		UploadChunkSize:    10,
	}
}

func TestValidateSendFlagsSeparator(t *testing.T) {
	testCases := []struct {
		name    string
		sep     string
		wantErr bool
	}{
		// A separator is safe only when at least one of its characters cannot
		// occur in a ZFS dataset path. No snapshot name can then absorb it.
		{name: "default pipe", sep: "|"},
		{name: "pipe pair", sep: "||"},
		{name: "at sign", sep: "@"},
		{name: "pipe then legal char", sep: "|-"},
		{name: "legal char then pipe", sep: "a|"},
		{name: "pipe surrounded by legal chars", sep: "|_x"},

		// Every character below is legal in a ZFS dataset path, so joining a
		// volume name and snapshot names with it gives an ambiguous object name.
		{name: "empty", sep: "", wantErr: true},
		{name: "underscore", sep: "_", wantErr: true},
		{name: "hyphen", sep: "-", wantErr: true},
		{name: "colon", sep: ":", wantErr: true},
		{name: "period", sep: ".", wantErr: true},
		{name: "letter", sep: "x", wantErr: true},
		{name: "digit", sep: "1", wantErr: true},
		{name: "slash", sep: "/", wantErr: true},
		{name: "several legal chars", sep: "-to-", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			j := validSendJob()
			j.Separator = tc.sep
			err := j.ValidateSendFlags()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateSendFlags accepted separator %q, want it rejected", tc.sep)
				}
				if !strings.Contains(err.Error(), "separator") {
					t.Errorf("got err %q, want it to name the separator", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateSendFlags rejected separator %q - %v", tc.sep, err)
			}
		})
	}
}

// TestObjectNameCollision states the invariant that makes the separator rule
// matter: an incremental backup and a full backup must never compute the same
// object name. An empty separator breaks it, because an incremental from "a" to
// "b" and a full of a snapshot named "atob" both join to "tank/dataatob". The
// second backup would then silently overwrite the first at the destination.
func TestObjectNameCollision(t *testing.T) {
	incremental := &JobInfo{
		VolumeName:          "tank/data",
		ManifestPrefix:      "manifests",
		BaseSnapshot:        SnapshotInfo{Name: "b"},
		IncrementalSnapshot: SnapshotInfo{Name: "a"},
	}
	full := &JobInfo{
		VolumeName:     "tank/data",
		ManifestPrefix: "manifests",
		BaseSnapshot:   SnapshotInfo{Name: "atob"},
	}

	// With the empty separator the two names collide. ValidateSendFlags must
	// therefore refuse it before a backup can run.
	incremental.Separator, full.Separator = "", ""
	if incremental.BackupVolumeObjectName(1) != full.BackupVolumeObjectName(1) {
		t.Fatalf("expected the empty separator to collide, but it did not; the test no longer proves anything")
	}
	j := validSendJob()
	j.Separator = ""
	if err := j.ValidateSendFlags(); err == nil {
		t.Error("ValidateSendFlags accepted the empty separator, which produces colliding object names")
	}

	// With the default separator the same pair must stay distinct, for the
	// volume names as well as the manifest names.
	incremental.Separator, full.Separator = "|", "|"
	if got, other := incremental.BackupVolumeObjectName(1), full.BackupVolumeObjectName(1); got == other {
		t.Errorf("volume object names collide with the default separator: %q", got)
	}
	if got, other := incremental.ManifestObjectName(), full.ManifestObjectName(); got == other {
		t.Errorf("manifest object names collide with the default separator: %q", got)
	}
}
