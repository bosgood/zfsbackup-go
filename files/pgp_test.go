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
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/pgp"
)

// newTestKey returns a new RSA key for email. Keys from NewEntity need SHA256 as their preferred
// hash before the vendored openpgp encrypts to them.
func newTestKey(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("backup", "", email, &packet.Config{RSABits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range e.Identities {
		id.SelfSignature.PreferredHash = []uint8{8}      // SHA256
		id.SelfSignature.PreferredSymmetric = []uint8{9} // AES256
	}
	// Sign the identities, so that Serialize writes self-signatures.
	if err = e.SerializePrivate(ioutil.Discard, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

// loadRings loads the pgp package's rings with the public halves of pub and the private halves
// of sec, as --publicKeyRingPath and --secretKeyRingPath would.
func loadRings(t *testing.T, pub, sec []*openpgp.Entity) {
	t.Helper()
	dir := t.TempDir()
	for _, ring := range []struct {
		kind    string
		keys    []*openpgp.Entity
		private bool
		load    func(string) error
	}{
		{openpgp.PublicKeyType, pub, false, pgp.LoadPublicRing},
		{openpgp.PrivateKeyType, sec, true, pgp.LoadPrivateRing},
	} {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, ring.kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ring.keys {
			if ring.private {
				err = e.SerializePrivate(w, nil)
			} else {
				err = e.Serialize(w)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		path := filepath.Join(dir, ring.kind)
		if err = ioutil.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err = ring.load(path); err != nil {
			t.Fatal(err)
		}
	}
}

// manifestJob is a manifest's JobInfo, encrypted to encrypt and signed by sign (either may be nil).
func manifestJob(encrypt, sign *openpgp.Entity) *JobInfo {
	return &JobInfo{
		VolumeName: "tank/data", BaseSnapshot: SnapshotInfo{Name: "a"},
		Compressor: InternalCompressor, CompressionLevel: 6, Separator: "|", ManifestPrefix: "manifests",
		EncryptKey: encrypt, SignKey: sign,
	}
}

// writeManifest writes j as a manifest volume and returns the file holding it.
func writeManifest(t *testing.T, j *JobInfo) string {
	t.Helper()
	oldTemp := config.BackupTempdir
	config.BackupTempdir = t.TempDir()
	t.Cleanup(func() { config.BackupTempdir = oldTemp })
	v, err := CreateManifestVolume(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(v).Encode(j); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	return v.filename
}

// readManifest extracts the manifest at path with the keys of j, decodes it, and verifies its end.
// It returns the step that failed and its error.
func readManifest(t *testing.T, path string, j *JobInfo) (string, error) {
	t.Helper()
	v, err := ExtractLocal(context.Background(), j, path, true)
	if err != nil {
		return "Extract", err
	}
	defer v.Close()
	if err = json.NewDecoder(v).Decode(new(JobInfo)); err != nil {
		return "Decode", err
	}
	if err = v.VerifyEnd(); err != nil {
		return "VerifyEnd", err
	}
	return "", nil
}

func fingerprint(e *openpgp.Entity) string {
	return fmt.Sprintf("%X", e.PrimaryKey.Fingerprint)
}

// With --signFrom, a manifest that is merely encrypted to the key is rejected before its JSON is
// used: anyone with the public key can write one.
func TestExtractRequiresSignature(t *testing.T) {
	key := newTestKey(t, "backup@example.com")
	loadRings(t, []*openpgp.Entity{key}, []*openpgp.Entity{key})

	unsigned := writeManifest(t, manifestJob(key, nil))
	step, err := readManifest(t, unsigned, manifestJob(key, key))
	if err == nil {
		t.Fatal("an unsigned manifest was accepted with SignKey set")
	}
	if step != "Extract" || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("want Extract to fail with 'not signed'; %s failed with %v", step, err)
	}

	signed := writeManifest(t, manifestJob(key, key))
	if step, err = readManifest(t, signed, manifestJob(key, key)); err != nil {
		t.Errorf("a signed and encrypted manifest was rejected at %s: %v", step, err)
	}
}

// With --encryptTo, a manifest that is only signed is rejected.
func TestExtractRequiresEncryption(t *testing.T) {
	key := newTestKey(t, "backup@example.com")
	loadRings(t, []*openpgp.Entity{key}, []*openpgp.Entity{key})

	signedOnly := writeManifest(t, manifestJob(nil, key))
	step, err := readManifest(t, signedOnly, manifestJob(key, key))
	if err == nil {
		t.Fatal("an unencrypted manifest was accepted with EncryptKey set")
	}
	if step != "Extract" || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("want Extract to fail with 'not encrypted'; %s failed with %v", step, err)
	}

	if step, err = readManifest(t, signedOnly, manifestJob(nil, key)); err != nil {
		t.Errorf("a signed-only manifest was rejected without EncryptKey at %s: %v", step, err)
	}
}

// A signature by a key other than --signFrom's is rejected by VerifyEnd. The JSON decodes before
// that: json.Decoder stops at the end of the value, before the EOF where the signature is checked.
func TestVerifyEndRejectsOtherSigner(t *testing.T) {
	a, b := newTestKey(t, "a@example.com"), newTestKey(t, "b@example.com")
	signedByA := writeManifest(t, manifestJob(nil, a))

	t.Run("signer in the ring", func(t *testing.T) {
		loadRings(t, []*openpgp.Entity{a, b}, []*openpgp.Entity{b})
		step, err := readManifest(t, signedByA, manifestJob(nil, b))
		if err == nil {
			t.Fatal("a manifest signed by another key was accepted")
		}
		want := fmt.Sprintf("signed by %s, want %s", fingerprint(a), fingerprint(b))
		if step != "VerifyEnd" || !strings.Contains(err.Error(), want) {
			t.Errorf("want VerifyEnd to fail with %q; %s failed with %v", want, step, err)
		}
	})

	t.Run("signer not in the ring", func(t *testing.T) {
		loadRings(t, []*openpgp.Entity{b}, []*openpgp.Entity{b})
		step, err := readManifest(t, signedByA, manifestJob(nil, b))
		if err == nil {
			t.Fatal("a manifest signed by an unknown key was accepted")
		}
		if step != "VerifyEnd" || !strings.Contains(err.Error(), "not in --signFrom's ring") {
			t.Errorf("want VerifyEnd to fail with 'not in --signFrom's ring'; %s failed with %v", step, err)
		}
	})
}

// A message no already-decrypted key opens (one sealed with a passphrase, or encrypted to a
// secret key that is still locked) makes openpgp ask for a passphrase. That must be a KeyError,
// not a panic: one such object under the manifest prefix would crash list, send, clean and
// receive, and a send would die holding its lock.
func TestExtractSymmetricMessageIsKeyError(t *testing.T) {
	key := newTestKey(t, "backup@example.com")
	old := newTestKey(t, "old@example.com")
	loadRings(t, []*openpgp.Entity{key, old}, []*openpgp.Entity{key, old})

	extract := func(t *testing.T, path string, j *JobInfo) (perr interface{}, err error) {
		t.Helper()
		defer func() { perr = recover() }()
		v, err := ExtractLocal(context.Background(), j, path, true)
		if v != nil {
			v.Close()
		}
		return nil, err
	}
	check := func(t *testing.T, path string, j *JobInfo) {
		t.Helper()
		p, err := extract(t, path, j)
		if p != nil {
			t.Fatalf("Extract panicked: %v", p)
		}
		if !errors.As(err, new(*KeyError)) {
			t.Errorf("want a KeyError, got %T %v", err, err)
		}
	}

	t.Run("symmetric message planted at the destination", func(t *testing.T) {
		var buf bytes.Buffer
		w, err := openpgp.SymmetricallyEncrypt(&buf, []byte("x"), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, "not a manifest")
		w.Close()
		path := filepath.Join(t.TempDir(), "m")
		if err = ioutil.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		check(t, path, manifestJob(key, key))
	})

	t.Run("manifest encrypted to a rotated still-locked key", func(t *testing.T) {
		path := writeManifest(t, manifestJob(old, nil))
		// The old key is in the secret ring but passphrase-protected: only --encryptTo's key is decrypted.
		locked := pgp.GetPrivateKeyByEmail("old@example.com")
		locked.PrivateKey.Encrypted = true
		for _, s := range locked.Subkeys {
			s.PrivateKey.Encrypted = true
		}
		check(t, path, manifestJob(key, nil))
	})

	t.Run("manifest encrypted to a key in neither ring", func(t *testing.T) {
		stranger := newTestKey(t, "stranger@example.com")
		check(t, writeManifest(t, manifestJob(stranger, nil)), manifestJob(key, nil))
	})
}

// With an external decompressor, the signature is verified when the decompressor's stdin copy
// reaches the end of the message. A decompressor that exits before that leaves it unchecked, so
// the volume must fail instead of ending cleanly.
func TestExtractExternalDecompressorReadsWholeMessage(t *testing.T) {
	key := newTestKey(t, "backup@example.com")
	loadRings(t, []*openpgp.Entity{key}, []*openpgp.Entity{key})

	// A signed message whose last payload byte is flipped after signing: it does not verify.
	var buf bytes.Buffer
	w, err := openpgp.Sign(&buf, key, nil, &packet.Config{DefaultHash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("zfs stream "), 100000) // more than a pipe buffer
	if _, err = w.Write(payload); err != nil {
		t.Fatal(err)
	}
	w.Close()
	msg := buf.Bytes()
	i := bytes.LastIndex(msg, []byte("stream zfs stream "))
	if i < 0 {
		t.Fatal("payload not found in the message")
	}
	msg[i+len("stream zfs stream ")-1] ^= 0xff
	path := filepath.Join(t.TempDir(), "vol")
	if err = ioutil.WriteFile(path, msg, 0600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"whole": "#!/bin/sh\nexec cat\n",
		"early": "#!/bin/sh\nexec head -c 1000\n",
	} {
		t.Run(name, func(t *testing.T) {
			script := filepath.Join(dir, name)
			if err := ioutil.WriteFile(script, []byte(body), 0755); err != nil {
				t.Fatal(err)
			}
			j := manifestJob(nil, key)
			j.Compressor, j.TrustedCompressor = script, script
			v, err := ExtractLocal(context.Background(), j, path, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, v)
			if cerr := v.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				t.Error("a volume whose signature does not verify was read without an error")
			}
		})
	}
}
