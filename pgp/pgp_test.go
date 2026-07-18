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

package pgp

import (
	"os"
	"path/filepath"
	"testing"

	pgpcrypto "github.com/ProtonMail/gopenpgp/v3/crypto"
)

func writeArmoredKeyring(t *testing.T, dir, name string, armored string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(armored), 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadKeyringsAndLookup(t *testing.T) {
	pgp := pgpcrypto.PGP()

	pub, err := pgp.KeyGeneration().AddUserId("Alice", "alice@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("generate alice: %v", err)
	}
	priv, err := pgp.KeyGeneration().AddUserId("Bob", "bob@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("generate bob: %v", err)
	}

	pubArmor, err := pub.GetArmoredPublicKey()
	if err != nil {
		t.Fatalf("armor pub: %v", err)
	}
	privArmor, err := priv.Armor()
	if err != nil {
		t.Fatalf("armor priv: %v", err)
	}

	dir := t.TempDir()
	pubPath := writeArmoredKeyring(t, dir, "public.pgp", pubArmor)
	privPath := writeArmoredKeyring(t, dir, "private.pgp", privArmor)

	if err := LoadPublicRing(pubPath); err != nil {
		t.Fatalf("LoadPublicRing: %v", err)
	}
	if err := LoadPrivateRing(privPath); err != nil {
		t.Fatalf("LoadPrivateRing: %v", err)
	}

	if got := GetPublicKeyByEmail("alice@example.com"); got == nil {
		t.Fatalf("expected to find public key for alice@example.com")
	}
	if got := GetPublicKeyByEmail("nobody@example.com"); got != nil {
		t.Fatalf("did not expect to find public key for nobody@example.com")
	}
	if got := GetPrivateKeyByEmail("bob@example.com"); got == nil {
		t.Fatalf("expected to find private key for bob@example.com")
	}

	PrintPGPDebugInformation()
}

func TestReplacePrivateKey(t *testing.T) {
	pgp := pgpcrypto.PGP()

	priv, err := pgp.KeyGeneration().AddUserId("Carol", "carol@example.com").New().GenerateKey()
	if err != nil {
		t.Fatalf("generate carol: %v", err)
	}
	locked, err := pgp.LockKey(priv, []byte("hunter2"))
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	privKeys = []*pgpcrypto.Key{locked}

	got := GetPrivateKeyByEmail("carol@example.com")
	if got != locked {
		t.Fatalf("expected to find locked key, got %v", got)
	}

	unlocked, err := got.Unlock([]byte("hunter2"))
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	ReplacePrivateKey(got, unlocked)

	got2 := GetPrivateKeyByEmail("carol@example.com")
	if got2 != unlocked {
		t.Fatalf("expected ReplacePrivateKey to swap in the unlocked key")
	}
}
