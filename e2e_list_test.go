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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/op/go-logging"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
)

// list runs `zfsbackup list` in-process and returns what it printed and logged.
func (env *e2eEnv) list(args ...string) (out, logs string, err error) {
	cmd.ResetListJobInfo()
	var outBuf, logBuf bytes.Buffer
	log.AppLogger.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logBuf, "", 0)))
	oldStdout := config.Stdout
	config.Stdout = &outBuf
	defer func() { config.Stdout = oldStdout }()

	cmd.RootCmd.SetArgs(append([]string{"list", "--zfsPath", env.self, "--workingDirectory", env.work}, args...))
	defer func() {
		cmd.RootCmd.SetArgs(nil)
		cmd.ResetListJobInfo()
	}()
	err = cmd.RootCmd.ExecuteContext(context.Background())
	return outBuf.String(), logBuf.String(), err
}

// list with a secret ring that lacks the key the manifests are encrypted to fails, names the key
// problem and how to pass the right ring, and never advises deleting the manifest.
func TestE2EListWithWrongKeyAdvises(t *testing.T) {
	env := newE2EEnv(t)
	t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	dest := "file://" + env.dest
	env.sendOK(t, append(writeRings(t, newKey(t)), "tank/data@a", dest)...)

	_, logs, err := env.list(append(writeRings(t, newKey(t)), dest)...)
	if err == nil {
		t.Fatalf("list with the wrong secret ring succeeded\n%s", logs)
	}
	if strings.Contains(logs, "delete that object") || !strings.Contains(logs, "--secretKeyRingPath") {
		t.Errorf("want the key advice, not the delete advice\n%s", logs)
	}
}
