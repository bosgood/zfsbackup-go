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
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/s3/s3manager/s3manageriface"
)

// blockingUploader signals started when an upload begins and returns when release is closed.
type blockingUploader struct {
	s3manageriface.UploaderAPI
	started, release chan struct{}
}

func (u *blockingUploader) UploadWithContext(
	_ aws.Context, _ *s3manager.UploadInput, _ ...func(*s3manager.Uploader),
) (*s3manager.UploadOutput, error) {
	close(u.started)
	<-u.release
	return &s3manager.UploadOutput{}, nil
}

// Close waits for a running Upload instead of clearing the client under it.
func TestS3CloseWaitsForUpload(t *testing.T) {
	_, vol, _, err := prepareTestVols()
	if err != nil {
		t.Fatalf("error preparing volume for testing - %v", err)
	}
	if err = vol.OpenVolume(); err != nil {
		t.Fatal(err)
	}
	defer vol.Close()

	u := &blockingUploader{started: make(chan struct{}), release: make(chan struct{})}
	b := &AWSS3Backend{}
	conf := &BackendConfig{TargetURI: AWSS3BackendPrefix + "://goodbucket", MaxParallelUploadBuffer: make(chan bool, 1)}
	if err = b.Init(context.Background(), conf, WithS3Client(&mockS3Client{}), WithS3Uploader(u)); err != nil {
		t.Fatal(err)
	}
	uploaded := make(chan error, 1)
	go func() { uploaded <- b.Upload(context.Background(), vol) }()
	<-u.started

	closed := make(chan struct{})
	go func() {
		_ = b.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Error("Close returned while an Upload was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(u.release)
	if err := <-uploaded; err != nil {
		t.Fatal(err)
	}
	<-closed
}
