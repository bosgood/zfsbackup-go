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
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
)

// recheckKey describes one object of a recheckS3Client.
type recheckKey struct {
	class string
	// restoredUntil, if set, is the expiry of the object's restored copy, given the number of
	// waits that have ended so far. A zero time means the copy is gone (no x-amz-restore header).
	restoredUntil func(waits int) time.Time

	restoreDays     *int64 // Days of the last RestoreObject
	restoreHeads    int    // HEADs since the last RestoreObject; 0 when none was issued
	restoreFinished bool   // a HEAD after the last RestoreObject showed the copy
}

// recheckS3Client models restores that finish on the second HEAD after RestoreObject. Every
// such finish counts as the end of a wait. It records RestoreObject calls in order.
type recheckS3Client struct {
	mockS3Client

	keys     map[string]*recheckKey
	waits    int
	restores []string
}

func restoredHeader(expiry time.Time) *string {
	return aws.String(`ongoing-request="false", expiry-date="` + expiry.UTC().Format(http.TimeFormat) + `"`)
}

func (c *recheckS3Client) HeadObjectWithContext(
	_ aws.Context, in *s3.HeadObjectInput, _ ...request.Option,
) (*s3.HeadObjectOutput, error) {
	k := c.keys[aws.StringValue(in.Key)]
	out := &s3.HeadObjectOutput{StorageClass: aws.String(k.class), ContentLength: aws.Int64(50)}
	if k.restoreHeads > 0 {
		k.restoreHeads++
		if k.restoreHeads == 2 {
			out.Restore = aws.String(`ongoing-request="true"`)
			return out, nil
		}
		if !k.restoreFinished {
			k.restoreFinished = true
			c.waits++
		}
		out.Restore = restoredHeader(time.Now().Add(72 * time.Hour))
		return out, nil
	}
	if k.restoredUntil != nil {
		if expiry := k.restoredUntil(c.waits); !expiry.IsZero() {
			out.Restore = restoredHeader(expiry)
		}
	}
	return out, nil
}

func (c *recheckS3Client) RestoreObjectWithContext(
	_ aws.Context, in *s3.RestoreObjectInput, _ ...request.Option,
) (*s3.RestoreObjectOutput, error) {
	key := aws.StringValue(in.Key)
	c.restores = append(c.restores, key)
	k := c.keys[key]
	k.restoreDays = in.RestoreRequest.Days
	if k.restoredUntil == nil || k.restoredUntil(c.waits).IsZero() {
		// A new restore; one that extends a present copy changes nothing a HEAD shows here.
		k.restoreHeads = 1
		k.restoreFinished = false
	}
	return &s3.RestoreObjectOutput{}, nil
}

func newRecheckBackend(t *testing.T, c *recheckS3Client) *AWSS3Backend {
	t.Helper()
	t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
	b := &AWSS3Backend{}
	conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket"}
	if err := b.Init(context.Background(), conf, WithS3Client(c), WithS3Uploader(&mockS3Uploader{})); err != nil {
		t.Fatal(err)
	}
	return b
}

func preDownload(t *testing.T, b *AWSS3Backend, keys ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.PreDownload(ctx, keys)
}

// deepArchive is an object with no restored copy.
func deepArchive() *recheckKey { return &recheckKey{class: s3.ObjectStorageClassDeepArchive} }

// A copy that lasted past the margin when PreDownload began, but expires soon after another
// key's long restore, is extended once that wait ends.
func TestS3PreDownloadExtendsCopyThatExpiresDuringWait(t *testing.T) {
	c := &recheckS3Client{keys: map[string]*recheckKey{
		"vol1": {class: s3.ObjectStorageClassGlacier, restoredUntil: func(waits int) time.Time {
			if waits == 0 {
				return time.Now().Add(30 * time.Hour)
			}
			return time.Now().Add(time.Hour)
		}},
		"vol2": deepArchive(),
	}}
	b := newRecheckBackend(t, c)
	if err := preDownload(t, b, "vol1", "vol2"); err != nil {
		t.Fatalf("PreDownload: %v", err)
	}
	if want := []string{"vol2", "vol1"}; strings.Join(c.restores, ",") != strings.Join(want, ",") {
		t.Errorf("RestoreObject calls %q, want %q", c.restores, want)
	}
}

// A copy that expired during another key's wait is restored and waited for.
func TestS3PreDownloadRestoresCopyThatExpiredDuringWait(t *testing.T) {
	c := &recheckS3Client{keys: map[string]*recheckKey{
		"vol1": {class: s3.ObjectStorageClassGlacier, restoredUntil: func(waits int) time.Time {
			if waits == 0 {
				return time.Now().Add(30 * time.Hour)
			}
			return time.Time{}
		}},
		"vol2": deepArchive(),
	}}
	b := newRecheckBackend(t, c)
	if err := preDownload(t, b, "vol1", "vol2"); err != nil {
		t.Fatalf("PreDownload: %v", err)
	}
	if want := []string{"vol2", "vol1"}; strings.Join(c.restores, ",") != strings.Join(want, ",") {
		t.Fatalf("RestoreObject calls %q, want %q", c.restores, want)
	}
	vol1 := c.keys["vol1"]
	if vol1.restoreDays == nil {
		t.Error("the restore of vol1 has no Days")
	}
	if !vol1.restoreFinished {
		t.Error("PreDownload returned before the restore of vol1 finished")
	}
}

// When copies keep expiring during the waits, PreDownload gives up after a bounded number of
// rounds instead of restoring forever.
func TestS3PreDownloadGivesUpAfterThreeRounds(t *testing.T) {
	goneAfter := func(n int) func(int) time.Time {
		return func(waits int) time.Time {
			if waits >= n {
				return time.Time{}
			}
			return time.Now().Add(30 * time.Hour)
		}
	}
	c := &recheckS3Client{keys: map[string]*recheckKey{
		"vol0": deepArchive(),
		"vol1": {class: s3.ObjectStorageClassGlacier, restoredUntil: goneAfter(1)},
		"vol2": {class: s3.ObjectStorageClassGlacier, restoredUntil: goneAfter(2)},
		"vol3": {class: s3.ObjectStorageClassGlacier, restoredUntil: goneAfter(3)},
	}}
	b := newRecheckBackend(t, c)
	err := preDownload(t, b, "vol0", "vol1", "vol2", "vol3")
	if err == nil || !strings.Contains(err.Error(), "rounds") {
		t.Fatalf("PreDownload: %v, want an error that mentions rounds", err)
	}
	if len(c.restores) > 4 {
		t.Errorf("%d RestoreObject calls (%q), want at most 4", len(c.restores), c.restores)
	}
}
