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
	"strings"
	"testing"
)

// TestE2ESendRejectedDestinationRedacted: send logs a destination it cannot parse. Neither
// the tail a comma split leaves of a password that holds a ',' nor an scp-style URI with no
// scheme may put the password in that log line.
func TestE2ESendRejectedDestinationRedacted(t *testing.T) {
	for name, dest := range map[string]string{
		"comma in password": "ssh://backup:hunter2,secretTAIL@nas.example/backups",
		"scp style":         "backup:secretTAIL@nas.example:/backups",
	} {
		t.Run(name, func(t *testing.T) {
			env := newE2EEnv(t)
			logs, err := env.send("tank/data@snap1", dest)
			if err == nil {
				t.Fatalf("send to %q succeeded", dest)
			}
			if strings.Contains(logs, "secretTAIL") {
				t.Errorf("send logged part of the password:\n%s", logs)
			}
		})
	}
}
