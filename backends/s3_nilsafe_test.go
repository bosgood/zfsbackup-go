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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
)

// sparseListClient answers every listing that is not Init's probe (MaxKeys 0) with page, and
// counts those calls. S3-compatible servers may leave out fields AWS always sends.
type sparseListClient struct {
	mockS3Client

	page  *s3.ListObjectsV2Output
	calls int
}

func (c *sparseListClient) ListObjectsV2WithContext(
	_ aws.Context, in *s3.ListObjectsV2Input, _ ...request.Option,
) (*s3.ListObjectsV2Output, error) {
	if aws.Int64Value(in.MaxKeys) <= 1 {
		return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}, nil
	}
	c.calls++
	return c.page, nil
}

func initWithClient(t *testing.T, c s3iface.S3API) *AWSS3Backend {
	t.Helper()
	b := &AWSS3Backend{}
	conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket"}
	if err := b.Init(context.Background(), conf, WithS3Client(c), WithS3Uploader(&mockS3Uploader{})); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestS3ListToleratesMissingIsTruncated(t *testing.T) {
	c := &sparseListClient{page: &s3.ListObjectsV2Output{
		Contents: []*s3.Object{{Key: aws.String("a")}, {Key: aws.String("b")}, {}}, // the last has no Key
	}}
	b := initWithClient(t, c)
	l, err := b.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[0] != "a" || l[1] != "b" {
		t.Errorf("List = %q, want [a b]", l)
	}
}

func TestS3ListTruncatedWithoutToken(t *testing.T) {
	c := &sparseListClient{page: &s3.ListObjectsV2Output{
		IsTruncated: aws.Bool(true),
		Contents:    []*s3.Object{{Key: aws.String("a")}},
	}}
	b := initWithClient(t, c)
	if _, err := b.List(context.Background(), ""); err == nil {
		t.Error("List of a truncated page with no continuation token: no error")
	}
	if c.calls != 1 {
		t.Errorf("%d listing calls, want 1", c.calls)
	}
}

// archiveNoClassClient answers HEAD with an Intelligent-Tiering archive status but no storage
// class until RestoreObject is called; after that the object reads directly.
type archiveNoClassClient struct {
	mockS3Client

	restores int
}

func (c *archiveNoClassClient) HeadObjectWithContext(
	_ aws.Context, _ *s3.HeadObjectInput, _ ...request.Option,
) (*s3.HeadObjectOutput, error) {
	out := &s3.HeadObjectOutput{ContentLength: aws.Int64(50)}
	if c.restores == 0 {
		out.ArchiveStatus = aws.String(s3.ArchiveStatusArchiveAccess)
	}
	return out, nil
}

func (c *archiveNoClassClient) RestoreObjectWithContext(
	_ aws.Context, _ *s3.RestoreObjectInput, _ ...request.Option,
) (*s3.RestoreObjectOutput, error) {
	c.restores++
	return &s3.RestoreObjectOutput{}, nil
}

func TestS3PreDownloadArchiveStatusWithoutStorageClass(t *testing.T) {
	t.Setenv("AWS_S3_RESTORE_POLL_INTERVAL", "10ms")
	c := &archiveNoClassClient{}
	b := initWithClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.PreDownload(ctx, []string{"vol1"}); err != nil {
		t.Fatalf("PreDownload: %v", err)
	}
	if c.restores != 1 {
		t.Errorf("%d RestoreObject calls, want 1", c.restores)
	}
}
