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

package backends

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/log"
)

func TestRestoreDays(t *testing.T) {
	const (
		mib = int64(1) << 20
		tib = int64(1) << 40
		pib = int64(1) << 50
	)
	// At 50 MiB/s a day downloads 50 MiB * 86400 = 4,529,848,320,000 bytes (about 4.12 TiB); at
	// 500 MiB/s ten times that. days = ceil(total / per day) + 2, clamped to 3..30.
	for _, tc := range []struct {
		total, rate, want int64
	}{
		{0, 50 * mib, 3},         // 0 + 2 = 2, raised to 3
		{4 * tib, 50 * mib, 3},   // 4 TiB / 4.12 TiB = 0.97: 1 + 2 = 3
		{20 * tib, 50 * mib, 7},  // 20 TiB / 4.12 TiB = 4.85: 5 + 2 = 7
		{pib, 50 * mib, 30},      // 1 PiB / 4.12 TiB = 248.6: 249 + 2, cut to 30
		{20 * tib, 500 * mib, 3}, // 20 TiB / 41.2 TiB = 0.49: 1 + 2 = 3
	} {
		if got := restoreDays(tc.total, tc.rate); got != tc.want {
			t.Errorf("restoreDays(%d, %d) = %d, want %d", tc.total, tc.rate, got, tc.want)
		}
	}
}

// The restore window follows the bytes to download: two DEEP_ARCHIVE keys of 10 TiB each at the
// default 50 MiB/s need 7 days. AWS_S3_RESTORE_DAYS overrides the result;
// AWS_S3_RESTORE_DOWNLOAD_RATE changes the rate.
func TestS3PreDownloadRestoreDaysFollowSize(t *testing.T) {
	const tib = int64(1) << 40
	for _, tc := range []struct {
		name, days, rate string
		want             int64
	}{
		{"default", "", "", restoreDays(20*tib, 50<<20)},
		{"days", "9", "", 9},
		{"rate", "", "500MiB", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_S3_RESTORE_DAYS", tc.days)
			t.Setenv("AWS_S3_RESTORE_DOWNLOAD_RATE", tc.rate)
			c := &recheckS3Client{keys: map[string]*recheckKey{
				"vol1": {class: s3.ObjectStorageClassDeepArchive, size: 10 * tib},
				"vol2": {class: s3.ObjectStorageClassDeepArchive, size: 10 * tib},
			}}
			b := newRecheckBackend(t, c)
			if err := preDownload(t, b, "vol1", "vol2"); err != nil {
				t.Fatalf("PreDownload: %v", err)
			}
			for _, key := range []string{"vol1", "vol2"} {
				if got := c.keys[key].restoreDays; got == nil || *got != tc.want {
					t.Errorf("RestoreObject of %s: Days %v, want %d", key, aws.Int64Value(got), tc.want)
				}
			}
		})
	}
}

// A bad AWS_S3_RESTORE_DAYS or AWS_S3_RESTORE_DOWNLOAD_RATE is an error, not a silent default:
// Init fails, before any command does work, and so does a PreDownload that sees it later.
func TestS3PreDownloadRejectsBadRestoreEnv(t *testing.T) {
	for _, tc := range []struct{ days, rate string }{
		{"0", ""}, {"31", ""}, {"x", ""}, {"", "fast"}, {"", "0"},
	} {
		c := &recheckS3Client{keys: map[string]*recheckKey{"vol1": deepArchive()}}
		t.Setenv("AWS_S3_RESTORE_DAYS", tc.days)
		t.Setenv("AWS_S3_RESTORE_DOWNLOAD_RATE", tc.rate)
		conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket"}
		if err := (&AWSS3Backend{}).Init(context.Background(), conf, WithS3Client(c), WithS3Uploader(&mockS3Uploader{})); err == nil {
			t.Errorf("Init with AWS_S3_RESTORE_DAYS=%q AWS_S3_RESTORE_DOWNLOAD_RATE=%q succeeded", tc.days, tc.rate)
		}

		t.Setenv("AWS_S3_RESTORE_DAYS", "")
		t.Setenv("AWS_S3_RESTORE_DOWNLOAD_RATE", "")
		b := newRecheckBackend(t, c)
		t.Setenv("AWS_S3_RESTORE_DAYS", tc.days)
		t.Setenv("AWS_S3_RESTORE_DOWNLOAD_RATE", tc.rate)
		if err := preDownload(t, b, "vol1"); err == nil {
			t.Errorf("PreDownload with AWS_S3_RESTORE_DAYS=%q AWS_S3_RESTORE_DOWNLOAD_RATE=%q succeeded", tc.days, tc.rate)
		}
		if len(c.restores) != 0 {
			t.Errorf("PreDownload with a bad setting restored %q", c.restores)
		}
	}
}

// The restore notice says where the days come from, and gives none when only Intelligent-Tiering
// objects (which take no lifetime) are restored.
func TestS3PreDownloadRestoreNotice(t *testing.T) {
	it := func() *recheckKey {
		return &recheckKey{class: s3.ObjectStorageClassIntelligentTiering, archive: s3.ArchiveStatusArchiveAccess}
	}
	for _, tc := range []struct {
		name, days string
		keys       map[string]*recheckKey
		want, not  []string
	}{
		{"computed", "", map[string]*recheckKey{"vol1": deepArchive()},
			[]string{"for 3 days, the time to download", "(AWS_S3_RESTORE_DAYS overrides)"}, []string{"Intelligent-Tiering"}},
		{"override", "9", map[string]*recheckKey{"vol1": deepArchive()},
			[]string{"for 9 days, as AWS_S3_RESTORE_DAYS sets"}, []string{"the time to download"}},
		{"mixed", "", map[string]*recheckKey{"vol1": deepArchive(), "vol2": it()},
			[]string{"for 3 days", "; the Intelligent-Tiering objects stay restored"}, nil},
		{"intelligent-tiering", "", map[string]*recheckKey{"vol1": it()},
			[]string{"restoring 1 objects"}, []string{"days", "Intelligent-Tiering objects stay"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_S3_RESTORE_DAYS", tc.days)
			buf := new(bytes.Buffer)
			log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(buf, "", 0)))
			t.Cleanup(func() {
				log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(os.Stderr, "", 0)))
			})
			c := &recheckS3Client{keys: tc.keys}
			keys := make([]string, 0, len(tc.keys))
			for k := range tc.keys {
				keys = append(keys, k)
			}
			if err := preDownload(t, newRecheckBackend(t, c), keys...); err != nil {
				t.Fatalf("PreDownload: %v", err)
			}
			var notice string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, "s3 backend: restoring ") {
					notice = line
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(notice, want) {
					t.Errorf("the notice %q lacks %q", notice, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(notice, not) {
					t.Errorf("the notice %q has %q", notice, not)
				}
			}
		})
	}
}
