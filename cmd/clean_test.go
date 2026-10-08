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

package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/log"
)

// clean takes one destination: a comma-separated list used to parse as several
// and clean only the first, silently.
func TestCleanRejectsSeveralDestinations(t *testing.T) {
	ResetSendJobInfo()
	var logs bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	RootCmd.SetArgs([]string{"clean", "--dry-run", "--workingDirectory", t.TempDir(), "file:///tmp/a,file:///tmp/b"})
	defer func() {
		RootCmd.SetArgs(nil)
		ResetSendJobInfo()
	}()
	err := RootCmd.ExecuteContext(context.Background())
	if err != errInvalidInput {
		t.Fatalf("clean a,b returned %v, want an input error", err)
	}
	if !strings.Contains(logs.String(), "clean takes one destination") {
		t.Errorf("got logs %q, want the one-destination error", logs.String())
	}
}
