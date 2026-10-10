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

import "testing"

// TestRedactURINoScheme covers strings with no "://": an scp-style typo for ssh://, a
// scheme with one slash, and the tail a comma split leaves of a password that holds a ','.
// The CLI logs each of them when it rejects the destination.
func TestRedactURINoScheme(t *testing.T) {
	testCases := map[string]string{
		"backup:hunter2secret@nas.example:/backups":     "nas.example:/backups",
		"ssh:/backup:hunter2secret@nas.example/backups": "nas.example/backups",
		"hunter2secret@nas.example/backups":             "nas.example/backups",
		"u:p@w@nas.example/backups":                     "nas.example/backups",
		"notauri":                                       "notauri",
		"/tmp/dest":                                     "/tmp/dest",
	}
	for in, want := range testCases {
		if got := RedactURI(in); got != want {
			t.Errorf("RedactURI(%q) = %q, want %q", in, got, want)
		}
	}
}
