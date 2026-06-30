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

package config

import (
	"fmt"
	"runtime/debug"
)

const (
	// VersionNumber represents the current version of zfsbackup
	VersionNumber = .3
	// ProgramName is the name for zfsbackup
	ProgramName = "zfsbackup"
)

// GitCommit is the git SHA the binary was built from. It may be set at build
// time via -ldflags "-X github.com/someone1/zfsbackup-go/config.GitCommit=...".
// When unset, GitCommitSHA falls back to the VCS info embedded by `go build`.
var GitCommit string

// Version will return the current version of zfsbackup
func Version() string {
	return fmt.Sprintf("%.2g", VersionNumber)
}

// GitCommitSHA returns the git commit the binary was built from, or "unknown"
// if it could not be determined.
func GitCommitSHA() string {
	if GitCommit != "" {
		return GitCommit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}
