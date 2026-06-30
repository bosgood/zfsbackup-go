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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
)

func TestVersion(t *testing.T) {
	old := config.Stdout
	buf := bytes.NewBuffer(nil)
	config.Stdout = buf
	defer func() { config.Stdout = old }()

	os.Args = []string{config.ProgramName, "version"}
	main()

	if !strings.Contains(buf.String(), fmt.Sprintf("Version:\tv%s", config.Version())) {
		t.Fatalf("expected version in version command output, did not receive one:\n%s", buf.String())
	}

	buf.Reset()
	os.Args = []string{config.ProgramName, "version", "--jsonOutput"}
	main()
	jout := struct {
		Version string
	}{}
	if err := json.Unmarshal(buf.Bytes(), &jout); err != nil {
		t.Fatalf("expected output to be JSON, got error while trying to decode - %v", err)
	} else if jout.Version != config.Version() {
		t.Fatalf("expected version to be '%s', got '%s' instead", config.Version(), jout.Version)
	}
}
