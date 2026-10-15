package pgp_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/bosgood/zfsbackup-go/pgp"
)

func TestExpiriesNoneForFixtures(t *testing.T) {
	for _, f := range []struct{ file, email string }{
		{"ed25519-public.asc", "ed25519@example.com"},
		{"ed25519-pass-public.asc", "ed25519-pass@example.com"},
		{"rsa-public.asc", "rsa@example.com"},
	} {
		if err := pgp.LoadPublicRing(fixtures + f.file); err != nil {
			t.Fatal(err)
		}
		if got := pgp.Expiries(pgp.GetPublicKeyByEmail(f.email)); len(got) != 0 {
			t.Errorf("%s: Expiries = %v, want none (the fixtures never expire)", f.file, got)
		}
	}
}

func TestExpiriesPrimaryKey(t *testing.T) {
	created := time.Unix(1700000000, 0)
	const lifetime = 3 * 365 * 24 * 3600
	e, err := openpgp.NewEntity("exp", "", "exp@example.com", &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		KeyLifetimeSecs: lifetime,
		Time:            func() time.Time { return created },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := created.Add(lifetime * time.Second)
	var primary bool
	for _, x := range pgp.Expiries(e) {
		if !x.Subkey && x.Fingerprint == fmt.Sprintf("%X", e.PrimaryKey.Fingerprint) {
			primary = true
			if !x.At.Equal(want) {
				t.Errorf("primary key expires %v, want %v", x.At, want)
			}
		}
	}
	if !primary {
		t.Errorf("Expiries = %v, want the primary key", pgp.Expiries(e))
	}
}

// A key whose primary never expires but whose new signing subkey does.
func TestExpiriesSubkeyOnly(t *testing.T) {
	e, err := openpgp.NewEntity("sub", "", "sub@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	if got := pgp.Expiries(e); len(got) != 0 {
		t.Fatalf("Expiries of a key without expiry = %v", got)
	}
	if err = e.AddSigningSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, KeyLifetimeSecs: 3600}); err != nil {
		t.Fatal(err)
	}
	got := pgp.Expiries(e)
	if len(got) == 0 {
		t.Fatal("Expiries missed the signing subkey's expiry")
	}
	sub := e.Subkeys[len(e.Subkeys)-1].PublicKey
	for _, x := range got {
		if !x.Subkey || x.Fingerprint != fmt.Sprintf("%X", sub.Fingerprint) {
			t.Errorf("unexpected expiry %v", x)
		}
	}
}

func TestExpiriesSignatureLifetime(t *testing.T) {
	created := time.Unix(1700000000, 0)
	e, err := openpgp.NewEntity("sig", "", "sig@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
		Time:      func() time.Time { return created },
	})
	if err != nil {
		t.Fatal(err)
	}
	selfSig, _ := e.PrimarySelfSignature()
	lifetime := uint32(600)
	selfSig.SigLifetimeSecs = &lifetime
	got := pgp.Expiries(e)
	if len(got) != 1 || got[0].Subkey || !got[0].At.Equal(created.Add(600*time.Second)) {
		t.Errorf("Expiries = %v, want the primary self-signature at %v", got, created.Add(600*time.Second))
	}
}
