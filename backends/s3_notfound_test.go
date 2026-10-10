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
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
)

type noSuchKeyClient struct{ mockS3Client }

func (noSuchKeyClient) GetObjectWithContext(aws.Context, *s3.GetObjectInput, ...request.Option) (*s3.GetObjectOutput, error) {
	return nil, awserr.New(s3.ErrCodeNoSuchKey, "The specified key does not exist.", nil)
}

// receive retries a download until --maxRetryTime unless the error is fs.ErrNotExist.
func TestS3DownloadMissingKeyIsNotExist(t *testing.T) {
	b := initWithClient(t, &noSuchKeyClient{})
	_, err := b.Download(context.Background(), "gone")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Download of a missing key: %v, want an error that is fs.ErrNotExist", err)
	}
	if err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("Download of a missing key: %v, want the SDK message", err)
	}
}
