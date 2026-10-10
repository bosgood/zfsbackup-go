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
	"os"
	"strings"
	"testing"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/log"
)

// The AWS_S3_ENABLE_DEBUG logger prints the SDK's arguments, with no "%!(EXTRA ...)" suffix.
func TestS3DebugLoggerFormats(t *testing.T) {
	buf := new(bytes.Buffer)
	log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(buf, "", 0)))
	t.Cleanup(func() {
		log.AppLogger.SetBackend(logging.MultiLogger(logging.NewLogBackend(os.Stderr, "", 0)))
	})

	logger{}.Log("a", 1)
	out := buf.String()
	if !strings.Contains(out, "s3 backend: a 1") || strings.Contains(out, "EXTRA") {
		t.Errorf("logged %q, want a line with %q and no EXTRA", out, "s3 backend: a 1")
	}
}
