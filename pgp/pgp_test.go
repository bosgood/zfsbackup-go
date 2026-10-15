package pgp_test

import (
	"testing"

	"github.com/bosgood/zfsbackup-go/pgp"
)

const (
	fixtures = "../testdata/pgp/"
	// OpenPGP public-key algorithm ids (RFC 4880 §9.1, RFC 9580 §9.1).
	pubKeyAlgoRSA   = 1
	pubKeyAlgoECDH  = 18
	pubKeyAlgoEdDSA = 22
)

func TestLoadEd25519PublicRing(t *testing.T) {
	if err := pgp.LoadPublicRing(fixtures + "ed25519-public.asc"); err != nil {
		t.Fatalf("LoadPublicRing: %v", err)
	}
	e := pgp.GetPublicKeyByEmail("ed25519@example.com")
	if e == nil {
		t.Fatal("no public key for ed25519@example.com")
	}
	if got := int(e.PrimaryKey.PubKeyAlgo); got != pubKeyAlgoEdDSA {
		t.Errorf("primary key algorithm = %d, want %d (EdDSA)", got, pubKeyAlgoEdDSA)
	}
	var encSubkeys int
	for _, sk := range e.Subkeys {
		if int(sk.PublicKey.PubKeyAlgo) == pubKeyAlgoECDH {
			encSubkeys++
		}
	}
	if encSubkeys != 1 {
		t.Errorf("found %d ECDH (cv25519) encryption subkeys, want 1", encSubkeys)
	}
	if pgp.GetPublicKeyByEmail("nobody@example.com") != nil {
		t.Error("found a key for an unknown email")
	}
}

func TestLoadEd25519PrivateRing(t *testing.T) {
	if err := pgp.LoadPrivateRing(fixtures + "ed25519-secret.asc"); err != nil {
		t.Fatalf("LoadPrivateRing: %v", err)
	}
	e := pgp.GetPrivateKeyByEmail("ed25519@example.com")
	if e == nil {
		t.Fatal("no private key for ed25519@example.com")
	}
	if e.PrivateKey == nil || e.PrivateKey.Encrypted {
		t.Errorf("expected an unprotected private key, got %+v", e.PrivateKey)
	}
	for i, sk := range e.Subkeys {
		if sk.PrivateKey == nil {
			t.Errorf("subkey %d has no private key", i)
		}
	}
}

func TestDecryptPassphraseProtectedEd25519Key(t *testing.T) {
	if err := pgp.LoadPrivateRing(fixtures + "ed25519-pass-secret.asc"); err != nil {
		t.Fatalf("LoadPrivateRing: %v", err)
	}
	e := pgp.GetPrivateKeyByEmail("ed25519-pass@example.com")
	if e == nil {
		t.Fatal("no private key for ed25519-pass@example.com")
	}
	if !e.PrivateKey.Encrypted {
		t.Fatal("fixture private key is not passphrase protected")
	}
	if err := e.PrivateKey.Decrypt([]byte("wrong")); err == nil {
		t.Error("wrong passphrase decrypted the primary key")
	}
	if err := e.PrivateKey.Decrypt([]byte("zfsbackup-test")); err != nil {
		t.Fatalf("Decrypt primary: %v", err)
	}
	for i, sk := range e.Subkeys {
		if err := sk.PrivateKey.Decrypt([]byte("zfsbackup-test")); err != nil {
			t.Errorf("Decrypt subkey %d: %v", i, err)
		}
	}
}

// The keyrings the tool accepted before must keep loading.
func TestLoadRSARingsStillWork(t *testing.T) {
	if err := pgp.LoadPublicRing(fixtures + "rsa-public.asc"); err != nil {
		t.Fatalf("LoadPublicRing: %v", err)
	}
	if err := pgp.LoadPrivateRing(fixtures + "rsa-secret.asc"); err != nil {
		t.Fatalf("LoadPrivateRing: %v", err)
	}
	e := pgp.GetPublicKeyByEmail("rsa@example.com")
	if e == nil || int(e.PrimaryKey.PubKeyAlgo) != pubKeyAlgoRSA {
		t.Fatalf("rsa public key: %+v", e)
	}
	if pgp.GetPrivateKeyByEmail("rsa@example.com") == nil {
		t.Fatal("rsa private key not found")
	}
}

func TestLoadMissingRing(t *testing.T) {
	if err := pgp.LoadPublicRing(fixtures + "does-not-exist.asc"); err == nil {
		t.Fatal("expected an error for a missing keyring")
	}
}
