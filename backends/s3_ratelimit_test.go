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
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/s3/s3manager/s3manageriface"
	"github.com/juju/ratelimit"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
)

// bodyRecorder keeps the Body that Upload hands to s3manager.
type bodyRecorder struct {
	s3manageriface.UploaderAPI
	body io.Reader
}

func (m *bodyRecorder) UploadWithContext(
	_ aws.Context, in *s3manager.UploadInput, _ ...func(*s3manager.Uploader),
) (*s3manager.UploadOutput, error) {
	m.body = in.Body
	return &s3manager.UploadOutput{}, nil
}

// s3manager reads a body that implements io.ReaderAt through io.SectionReader, which calls
// VolumeInfo.ReadAt and skips the --maxUploadSpeed limiter that only wraps VolumeInfo.Read. The
// body Upload passes must therefore offer Read only.
func TestS3UploadBodyIsNotSeekable(t *testing.T) {
	_, vol, _, err := prepareTestVols()
	if err != nil {
		t.Fatalf("error preparing volume for testing - %v", err)
	}
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	defer vol.Close()

	rec := &bodyRecorder{}
	b := &AWSS3Backend{}
	conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket", MaxParallelUploadBuffer: make(chan bool, 1)}
	if err = b.Init(context.Background(), conf, WithS3Client(&mockS3Client{}), WithS3Uploader(rec)); err != nil {
		t.Fatal(err)
	}
	if err = b.Upload(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.body.(io.ReaderAt); ok {
		t.Errorf("the upload body (%T) implements io.ReaderAt; s3manager would bypass the rate limiter", rec.body)
	}
	if _, ok := rec.body.(io.Seeker); ok {
		t.Errorf("the upload body (%T) implements io.Seeker; s3manager would bypass the rate limiter", rec.body)
	}
}

// putServer is a minimal S3 endpoint: an empty listing for any bucket GET, and a PUT that
// drains the body.
func putServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Name>bucket</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`))
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"x"`)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_S3_CUSTOM_ENDPOINT", srv.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
}

// An S3 upload of a file-buffered volume through the real s3manager obeys --maxUploadSpeed.
func TestS3UploadRateLimited(t *testing.T) {
	if testing.Short() {
		t.Skip("takes seconds by design")
	}
	putServer(t)

	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	vol, err := files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(vol, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err = vol.Close(); err != nil {
		t.Fatal(err)
	}
	defer vol.DeleteVolume()
	vol.ObjectName = "ratelimited"

	old := config.BackupUploadBucket
	t.Cleanup(func() { config.BackupUploadBucket = old })
	config.BackupUploadBucket = ratelimit.NewBucketWithRate(1<<20, 1<<20) // 1 MiB/s
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	defer vol.Close()

	b := &AWSS3Backend{}
	conf := &BackendConfig{
		TargetURI:               AWSS3BackendPrefix + "://bucket",
		MaxParallelUploadBuffer: make(chan bool, 1),
		MaxParallelUploads:      1,
		UploadChunkSize:         int(s3manager.MinUploadPartSize),
	}
	if err = b.Init(context.Background(), conf); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = b.Upload(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	// 1 MiB is in the bucket at the start; the other 3 MiB take 3 seconds. Keep the bound loose.
	if took := time.Since(start); took < 2*time.Second {
		t.Errorf("a 4 MiB upload at 1 MiB/s took %v, want at least 2s", took)
	}
}
