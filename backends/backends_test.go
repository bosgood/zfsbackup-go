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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/someone1/zfsbackup-go/files"
)

var (
	errTest = errors.New("used for testing")
)

type errTestFunc func(error) bool

func nilErrTest(e error) bool           { return e == nil }
func errTestErrTest(e error) bool       { return e == errTest }
func errInvalidURIErrTest(e error) bool { return e == ErrInvalidURI }
func invalidByteErrTest(e error) bool {
	_, ok := e.(hex.InvalidByteError)
	return ok
}

func prepareTestVols() (payload []byte, goodVol, badVol *files.VolumeInfo, err error) {
	payload = make([]byte, 10*1024*1024)
	if _, err = rand.Read(payload); err != nil {
		return
	}
	reader := bytes.NewReader(payload)
	goodVol, err = files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		return
	}
	_, err = io.Copy(goodVol, reader)
	if err != nil {
		return
	}
	err = goodVol.Close()
	if err != nil {
		return
	}
	goodVol.ObjectName = strings.Join([]string{"this", "is", "just", "a", "test"}, "-") + ".ext"

	badVol, err = files.CreateSimpleVolume(context.Background(), false)
	if err != nil {
		return
	}
	err = badVol.Close()
	if err != nil {
		return
	}
	badVol.ObjectName = strings.Join([]string{"this", "is", "just", "a", "badtest"}, "-") + ".ext"

	err = badVol.DeleteVolume()

	return payload, goodVol, badVol, err
}

func TestGetBackendForURI(t *testing.T) {
	_, err := GetBackendForURI("thiswon'texist://")
	if err != ErrInvalidPrefix {
		t.Errorf("Expecting err %v, got %v for non-existent prefix", ErrInvalidPrefix, err)
	}

	_, err = GetBackendForURI("thisisinvalid")
	if err != ErrInvalidURI {
		t.Errorf("Expecting err %v, got %v for invalid URI", ErrInvalidURI, err)
	}
}

func TestCanonicalURI(t *testing.T) {
	testCases := map[string]string{
		"s3://b":           "s3://b",
		"s3://b/":          "s3://b",
		"s3://b/p":         "s3://b/p/",
		"s3://b/p/":        "s3://b/p/",
		"s3://b//p//":      "s3://b/p/",
		"s3://b/p/q":       "s3://b/p/q/",
		"gs://b/p":         "gs://b/p/",
		"azure://c/p":      "azure://c/p/",
		"b2://b/p":         "b2://b/p/",
		"file:///tmp/dest": "file:///tmp/dest",
		"file:///tmp/d/":   "file:///tmp/d/",
		"ssh://h/path":     "ssh://h/path",
		"delete://":        "delete://",
		"notauri":          "notauri",
	}
	for in, want := range testCases {
		if got := CanonicalURI(in); got != want {
			t.Errorf("CanonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// The same normalization applies to every object-store backend; GCS, Azure and B2 cannot
// Init without a live service, so check the parsing they share.
func TestParseObjectURI(t *testing.T) {
	testCases := []struct {
		path, rawPrefix, prefix string
	}{
		{"b", "", ""},
		{"b/", "", ""},
		{"b/p", "p", "p/"},
		{"b/p/", "p", "p/"},
		{"b//p//", "p", "p/"},
		{"b/p/q", "p/q", "p/q/"},
	}
	for _, scheme := range []string{AWSS3BackendPrefix, GoogleCloudStorageBackendPrefix, AzureBackendPrefix, B2BackendPrefix} {
		for _, c := range testCases {
			bucket, rawPrefix, prefix, err := parseObjectURI(scheme+"://"+c.path, scheme)
			if err != nil || bucket != "b" || rawPrefix != c.rawPrefix || prefix != c.prefix {
				t.Errorf("parseObjectURI(%s://%s) = %q, %q, %q, %v; want b, %q, %q",
					scheme, c.path, bucket, rawPrefix, prefix, err, c.rawPrefix, c.prefix)
			}
		}
		if _, _, _, err := parseObjectURI("other://b/p", scheme); err != ErrInvalidURI {
			t.Errorf("%s: wrong scheme gave %v, want ErrInvalidURI", scheme, err)
		}
	}
}

func TestStripPrefix(t *testing.T) {
	if name, ok := stripPrefix("p/", "p/a"); !ok || name != "a" {
		t.Errorf("stripPrefix(p/, p/a) = %q, %v", name, ok)
	}
	for _, key := range []string{"photos/a", "pa", "/p/a"} {
		if _, ok := stripPrefix("p/", key); ok {
			t.Errorf("stripPrefix(p/, %s) matched", key)
		}
	}
	if name, ok := stripPrefix("", "a"); !ok || name != "a" {
		t.Errorf("stripPrefix('', a) = %q, %v", name, ok)
	}
}

func BackendTest(ctx context.Context, prefix, uri string, skipPrefix bool, b Backend, opts ...Option) func(*testing.T) {
	return func(t *testing.T) {
		testPayLoad, goodVol, badVol, perr := prepareTestVols()
		if perr != nil {
			t.Fatalf("Error while creating test volumes: %v", perr)
		}
		defer func() {
			if err := goodVol.DeleteVolume(); err != nil {
				t.Errorf("could not delete good vol - %v", err)
			}
		}()

		t.Run("Init", func(t *testing.T) {
			// Bad TargetURI
			conf := &BackendConfig{
				TargetURI:               "notvalid://" + uri,
				UploadChunkSize:         8 * 1024 * 1024,
				MaxParallelUploads:      5,
				MaxParallelUploadBuffer: make(chan bool, 5),
			}
			err := b.Init(ctx, conf)
			if err != ErrInvalidURI {
				t.Fatalf("Expected Invalid URI error, got %v", err)
			}

			// Good TargetURI
			conf = &BackendConfig{
				TargetURI:               prefix + "://" + uri,
				UploadChunkSize:         8 * 1024 * 1024,
				MaxParallelUploads:      5,
				MaxParallelUploadBuffer: make(chan bool, 5),
			}
			err = b.Init(ctx, conf, opts...)
			if err != nil {
				t.Fatalf("Issue initilazing backend: %v", err)
			}

			if !skipPrefix {
				// Good TargetURI with URI prefix
				uri += "/prefix/"
				conf = &BackendConfig{
					TargetURI:               prefix + "://" + uri,
					UploadChunkSize:         8 * 1024 * 1024,
					MaxParallelUploads:      5,
					MaxParallelUploadBuffer: make(chan bool, 5),
				}
				err = b.Init(ctx, conf, opts...)
				if err != nil {
					t.Fatalf("Issue initilazing backend: %v", err)
				}
			}
		})

		t.Run("Upload", func(t *testing.T) {
			err := goodVol.OpenVolume()
			if err != nil {
				t.Errorf("could not open good volume due to error %v", err)
			}
			defer goodVol.Close()
			err = b.Upload(ctx, goodVol)
			if err != nil {
				t.Fatalf("Issue uploading goodvol: %v", err)
			}

			err = b.Upload(ctx, badVol)
			if err == nil {
				t.Errorf("Expecting non-nil error uploading badvol, got nil instead.")
			}
		})

		t.Run("List", func(t *testing.T) {
			names, err := b.List(ctx, "")
			if err != nil {
				t.Fatalf("Issue listing backend: %v", err)
			}

			if len(names) != 1 {
				t.Fatalf("Expecting exactly one name from list, got %d instead.", len(names))
			}

			if names[0] != goodVol.ObjectName {
				t.Fatalf("Expecting name '%s', got '%s' instead", goodVol.ObjectName, names[0])
			}

			names, err = b.List(ctx, "badprefix")
			if err != nil {
				t.Fatalf("Issue listing container: %v", err)
			}

			if len(names) != 0 {
				t.Fatalf("Expecting exactly zero names from list, got %d instead.", len(names))
			}
		})

		t.Run("PreDownload", func(t *testing.T) {
			err := b.PreDownload(ctx, []string{goodVol.ObjectName})
			if err != nil {
				t.Fatalf("Issue calling PreDownload: %v", err)
			}
		})

		t.Run("Download", func(t *testing.T) {
			r, err := b.Download(ctx, goodVol.ObjectName)
			if err != nil {
				t.Fatalf("Issue calling Download: %v", err)
			}
			defer r.Close()

			buf := bytes.NewBuffer(nil)
			_, err = io.Copy(buf, r)
			if err != nil {
				t.Fatalf("error reading: %v", err)
			}

			if !reflect.DeepEqual(testPayLoad, buf.Bytes()) {
				t.Fatalf("downloaded object does not equal expected payload")
			}

			_, err = b.Download(ctx, badVol.ObjectName)
			if err == nil {
				t.Fatalf("expecting non-nil response, got nil instead")
			}
		})

		t.Run("Delete", func(t *testing.T) {
			err := b.Delete(ctx, goodVol.ObjectName)
			if err != nil {
				t.Errorf("Issue calling Delete: %v", err)
			}

			names, err := b.List(ctx, "")
			if err != nil {
				t.Errorf("Issue listing backend: %v", err)
			}

			if len(names) != 0 {
				t.Errorf("Expecting exactly zero names from list, got %d instead.", len(names))
			}
		})

		t.Run("Close", func(t *testing.T) {
			err := b.Close()
			if err != nil {
				t.Fatalf("Issue closing backend: %v", err)
			}
		})
	}
}

func TestRedactURI(t *testing.T) {
	testCases := map[string]string{
		"ssh://user:secret@host:22/path": "ssh://user:xxxxx@host:22/path",
		"ssh://u:p@h/x":                  "ssh://u:xxxxx@h/x",
		"ssh://bob:p%40ss@host/x":        "ssh://bob:xxxxx@host/x",
		"ssh://user@host/path":           "ssh://user@host/path",
		"ssh://host/path":                "ssh://host/path",
		"s3://bucket/p@q/":               "s3://bucket/p@q/",
		"s3://bucket/p/":                 "s3://bucket/p/",
		"s3://bucket":                    "s3://bucket",
		"file:///x":                      "file:///x",
		"file:///tmp/dest":               "file:///tmp/dest",
		"delete://":                      "delete://",
		"notauri":                        "notauri",
	}
	for in, want := range testCases {
		if got := RedactURI(in); got != want {
			t.Errorf("RedactURI(%q) = %q, want %q", in, got, want)
		}
	}

	// A password with a character url.Parse misreads ('/', '?', '#', '@') has no
	// reliable structure: whatever comes out, the password must not.
	for _, in := range []string{
		"ssh://u:pa/ss@h/x",
		"ssh://u:p?w@h/x",
		"ssh://u:p#w@h/x",
		"ssh://u:p@w@h/x",
		"ssh://u:p%zz@h/x",
		// url.Parse reads "u:" or "u:1234" as host:port and the rest as path, query or
		// fragment, so the URI seems to have no user at all.
		"ssh://u:/secret@h/x",
		"ssh://u:?secret@h/x",
		"ssh://u:#secret@h/x",
		"ssh://u:1234/secret@h/x",
		"ssh://u:99?pw@h/x",
		"ssh://u:2024/Secret@h/x",
		"ssh://u:12345#frag@h/x",
	} {
		got := RedactURI(in)
		password := in[len("ssh://u:"):strings.LastIndex(in, "@")]
		if strings.Contains(got, password) || strings.Contains(got, password[:2]) {
			t.Errorf("RedactURI(%q) = %q leaks the password", in, got)
		}
		if !strings.HasPrefix(got, "ssh://") || !strings.HasSuffix(got, "h/x") {
			t.Errorf("RedactURI(%q) = %q, want the scheme and the host and path kept", in, got)
		}
	}
}

// The error ssh's Init returns for an unparsable URI must not carry the password.
func TestSSHInitErrorRedacts(t *testing.T) {
	b := &SSHBackend{}
	err := b.Init(context.Background(), &BackendConfig{TargetURI: "ssh://user:pa/ss@host/path"})
	if err == nil {
		t.Fatal("Init accepted a URI with an unescaped '/' in the password")
	}
	if strings.Contains(err.Error(), "pa/ss") {
		t.Errorf("Init error leaks the password: %v", err)
	}
}

func TestLegacySpellings(t *testing.T) {
	testCases := []struct {
		canonical, typed string
		want             []string
	}{
		{"s3://b", "s3://b/", []string{"s3://b/"}},
		{"s3://b", "s3://b", []string{"s3://b/"}},
		{"s3://b/p/", "s3://b/p", []string{"s3://b/p"}},
		{"s3://b/p/", "", []string{"s3://b/p"}},
		{"s3://b/p/", "s3://b//p", []string{"s3://b/p", "s3://b//p"}},
		{"gs://b/p/", "gs://b/p//", []string{"gs://b/p", "gs://b/p//"}},
		{"file:///tmp/d", "file:///tmp/d", nil},
		{"ssh://h/path", "", nil},
	}
	for _, c := range testCases {
		if got := LegacySpellings(c.canonical, c.typed); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LegacySpellings(%q, %q) = %q, want %q", c.canonical, c.typed, got, c.want)
		}
	}
}

func TestLegacyPrefixes(t *testing.T) {
	testCases := []struct {
		uri, typed string
		want       []string
	}{
		{"s3://b/p/", "", []string{"p"}},
		{"s3://b/p/", "s3://b/p/", []string{"p"}},
		{"s3://b/p/", "s3://b/p", []string{"p"}},
		{"s3://b/p/", "s3://b//p", []string{"p", "/p"}},
		{"s3://b/p/", "s3://b/p//", []string{"p", "p//"}},
		{"s3://b/p/q/", "s3://b/p/q", []string{"p/q"}},
		{"s3://b", "s3://b/", nil},
		{"s3://b", "s3://b//", []string{"/"}},
	}
	for _, c := range testCases {
		if got := legacyPrefixes(c.uri, c.typed, AWSS3BackendPrefix); !reflect.DeepEqual(got, c.want) {
			t.Errorf("legacyPrefixes(%q, %q) = %q, want %q", c.uri, c.typed, got, c.want)
		}
	}
}

func TestFindLegacyVolume(t *testing.T) {
	keys := []string{"p/tank/data|a.zstream.gz.vol1", "ptank/data|old.zstream.gz.vol1", "ptank/other|x.txt"}
	firstKey := func(prefix string) (string, error) {
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				return k, nil
			}
		}
		return "", nil
	}
	got, err := findLegacyVolume([]string{"p"}, []string{"tank/data"}, []string{"|"}, firstKey)
	if err != nil || got != "ptank/data|old.zstream.gz.vol1" {
		t.Errorf("findLegacyVolume = %q, %v", got, err)
	}
	// Not a volume name, and a dataset nobody asked about: nothing to report.
	for _, dataset := range []string{"tank/other", "tank/missing"} {
		if got, err = findLegacyVolume([]string{"p"}, []string{dataset}, []string{"|", ""}, firstKey); err != nil || got != "" {
			t.Errorf("findLegacyVolume(%s) = %q, %v; want nothing", dataset, got, err)
		}
	}
}
