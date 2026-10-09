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
	"fmt"
	"reflect"
	"testing"

	"github.com/spf13/cobra"

	"github.com/someone1/zfsbackup-go/backup"
)

func TestExitCode(t *testing.T) {
	testCases := []struct {
		cmd  *cobra.Command
		err  error
		want int
	}{
		{sendCmd, nil, 0},
		{sendCmd, backup.ErrNoOp, 0}, // a smart send with nothing new to back up
		{sendCmd, fmt.Errorf("wrapped: %w", backup.ErrNoOp), 0},
		{sendCmd, errInvalidInput, 255},
		{sendCmd, fmt.Errorf("wrapped: %w", backup.ErrLastBackupInFuture), 2},
		{planCmd, nil, 0},
		{planCmd, errChecksFailed, 2},
		{planCmd, errInvalidInput, 1},
	}
	for _, tc := range testCases {
		if got := exitCode(tc.cmd, tc.err); got != tc.want {
			t.Errorf("exitCode(%s, %v) = %d, want %d", tc.cmd.Name(), tc.err, got, tc.want)
		}
	}
}

func TestParseDestinationsKeepsTypedSpelling(t *testing.T) {
	dests, typed := parseDestinations("s3://b//p,s3://c/,file:///tmp/d")
	want := []string{"s3://b/p/", "s3://c", "file:///tmp/d"}
	if !reflect.DeepEqual(dests, want) {
		t.Errorf("destinations = %q, want %q", dests, want)
	}
	wantTyped := map[string]string{"s3://b/p/": "s3://b//p", "s3://c": "s3://c/", "file:///tmp/d": "file:///tmp/d"}
	if !reflect.DeepEqual(typed, wantTyped) {
		t.Errorf("typed = %q, want %q", typed, wantTyped)
	}
}
