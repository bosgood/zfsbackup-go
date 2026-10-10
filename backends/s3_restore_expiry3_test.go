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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
)

// restoredS3Client answers HEAD with a GLACIER object whose restored copy expires at expiry,
// and records RestoreObject calls.
type restoredS3Client struct {
	mockS3Client

	expiry   time.Time
	restores []string
}

func (c *restoredS3Client) HeadObjectWithContext(
	_ aws.Context, _ *s3.HeadObjectInput, _ ...request.Option,
) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{
		StorageClass:  aws.String(s3.ObjectStorageClassGlacier),
		ContentLength: aws.Int64(50),
		Restore:       aws.String(`ongoing-request="false", expiry-date="` + c.expiry.UTC().Format(http.TimeFormat) + `"`),
	}, nil
}

func (c *restoredS3Client) RestoreObjectWithContext(
	_ aws.Context, in *s3.RestoreObjectInput, _ ...request.Option,
) (*s3.RestoreObjectOutput, error) {
	c.restores = append(c.restores, aws.StringValue(in.Key))
	return &s3.RestoreObjectOutput{}, nil
}

// A restored copy that expires before the downloads after PreDownload are likely done is
// restored again, which extends it; one that lasts is left alone.
func TestS3PreDownloadExtendsExpiringRestore(t *testing.T) {
	t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
	for _, tc := range []struct {
		name   string
		expiry time.Duration // from now
		extend bool
	}{
		{name: "expires in 2h", expiry: 2 * time.Hour, extend: true},
		{name: "expires in 23h", expiry: 23 * time.Hour, extend: true},
		{name: "expires in 3 days", expiry: 72 * time.Hour, extend: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &restoredS3Client{expiry: time.Now().Add(tc.expiry)}
			b := &AWSS3Backend{}
			conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket"}
			if err := b.Init(context.Background(), conf, WithS3Client(c), WithS3Uploader(&mockS3Uploader{})); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := b.PreDownload(ctx, []string{"vol1"}); err != nil {
				t.Fatalf("PreDownload: %v", err)
			}
			if got := len(c.restores) > 0; got != tc.extend {
				t.Errorf("restore requested: %v, want %v (requests %q)", got, tc.extend, c.restores)
			}
		})
	}
}
