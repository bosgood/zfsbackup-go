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

package files

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
)

// writeRawManifest writes json, gzipped and unencrypted, as a manifest file.
func writeRawManifest(t *testing.T, json string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(json)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A manifest well under MaxManifestBytes of `{},` decodes to a VolumeInfo (some 480 bytes) per 3
// bytes of JSON: at the cap, some 10 GB of heap. More volumes than a manifest of real ones can
// hold is an error, found before they are decoded.
func TestReadManifestRejectsTooManyVolumes(t *testing.T) {
	setManifestLimit(t, 1<<20)
	n := MaxManifestVolumes() + 1
	path := writeRawManifest(t, `{"Volumes":[`+strings.Repeat("{},", n-1)+`{}]}`)
	_, err := ReadManifest(context.Background(), &JobInfo{}, path)
	if err == nil || !strings.Contains(err.Error(), "volumes") {
		t.Fatalf("a manifest of %d empty volumes: got %v, want an error about its volumes", n, err)
	}
}

// A null volume decodes to a nil *VolumeInfo, which every reader of Volumes dereferences.
func TestReadManifestRejectsNullVolume(t *testing.T) {
	path := writeRawManifest(t, `{"VolumeName":"tank/data","Volumes":[{"ObjectName":"x"},null]}`)
	_, err := ReadManifest(context.Background(), &JobInfo{}, path)
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("got %v, want an error about the null volume", err)
	}
	path = writeRawManifest(t, `{"VolumeName":"tank/data","Volumes":[{"ObjectName":"x"}]}`)
	if m, rerr := ReadManifest(context.Background(), &JobInfo{}, path); rerr != nil || len(m.Volumes) != 1 {
		t.Fatalf("a manifest of one volume: got %v, %v", m, rerr)
	}
}

// The error for a manifest signed by an unknown key must not tell the user to trust that key: the
// key may be one they rotated to, or anyone's who can write the destination.
func TestUnknownSignerErrorDoesNotAdviseTrust(t *testing.T) {
	a, b := newTestKey(t, "a@example.com"), newTestKey(t, "b@example.com")
	signedByA := writeManifest(t, manifestJob(nil, a))

	for _, tc := range []struct {
		name string
		pub  []*openpgp.Entity
	}{
		{"signer not in the ring", []*openpgp.Entity{b}},
		{"signer in the ring", []*openpgp.Entity{a, b}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loadRings(t, tc.pub, []*openpgp.Entity{b})
			_, err := readManifest(t, signedByA, manifestJob(nil, b))
			if err == nil {
				t.Fatal("a manifest signed by another key was accepted")
			}
			msg := err.Error()
			if strings.Contains(msg, "to accept it") || strings.Contains(msg, "rotated away from, pass") {
				t.Errorf("the error advises trusting the key: %v", err)
			}
			if !strings.Contains(msg, "fingerprint") || !strings.Contains(msg, "not yours") {
				t.Errorf("the error does not say the key may not be yours, or to check its fingerprint: %v", err)
			}
		})
	}
}
