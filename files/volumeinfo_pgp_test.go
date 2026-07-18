// Copyright © 2026 zfsbackup-go contributors
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
	"io"
	"testing"

	pgpcrypto "github.com/ProtonMail/gopenpgp/v3/crypto"
)

func generateTestKey(t *testing.T, name, email string) *pgpcrypto.Key {
	t.Helper()
	pgp := pgpcrypto.PGP()
	key, err := pgp.KeyGeneration().AddUserId(name, email).New().GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	return key
}

// TestPGPEncryptSignRoundTrip exercises the volumeinfo encrypt+sign and
// decrypt+verify code paths end-to-end without ZFS.
func TestPGPEncryptSignRoundTrip(t *testing.T) {
	encKey := generateTestKey(t, "Encryptor", "enc@example.com")
	signKey := generateTestKey(t, "Signer", "sign@example.com")

	plaintext := []byte("zfsbackup-go gopenpgp migration test payload")

	cases := []struct {
		name    string
		encrypt *pgpcrypto.Key
		sign    *pgpcrypto.Key
	}{
		{"encrypt-only", encKey, nil},
		{"sign-only", nil, signKey},
		{"encrypt-and-sign", encKey, signKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sendJob := &JobInfo{
				Compressor:       "",
				CompressionLevel: 1,
				EncryptKey:       tc.encrypt,
				SignKey:          tc.sign,
			}
			v, err := prepareVolume(ctx, sendJob, false, false)
			if err != nil {
				t.Fatalf("prepareVolume: %v", err)
			}
			if _, err := v.Write(plaintext); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := v.Close(); err != nil {
				t.Fatalf("close write side: %v", err)
			}

			recvJob := &JobInfo{
				Compressor: "",
				EncryptKey: tc.encrypt,
				SignKey:    tc.sign,
			}
			rv, err := ExtractLocal(ctx, recvJob, v.filename, false)
			if err != nil {
				t.Fatalf("ExtractLocal: %v", err)
			}
			var got bytes.Buffer
			if _, err := io.Copy(&got, rv); err != nil {
				t.Fatalf("read decrypted: %v", err)
			}
			if err := rv.Close(); err != nil {
				t.Fatalf("close read side: %v", err)
			}
			if !bytes.Equal(got.Bytes(), plaintext) {
				t.Fatalf("plaintext mismatch: got %q want %q", got.Bytes(), plaintext)
			}
		})
	}
}
