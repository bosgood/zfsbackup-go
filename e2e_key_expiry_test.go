package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/bosgood/zfsbackup-go/files"
)

// expiringRings writes the rings of a new Ed25519 key that expires in ten years and returns
// the flags that load them.
func expiringRings(t *testing.T, email string) []string {
	t.Helper()
	return expiringRingsFor(t, email, 10*365*24*time.Hour)
}

// expiringRingsFor is expiringRings with the key's lifetime.
func expiringRingsFor(t *testing.T, email string, lifetime time.Duration) []string {
	t.Helper()
	e, err := openpgp.NewEntity("expiring", "", email, &packet.Config{
		Algorithm:       packet.PubKeyAlgoEdDSA,
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, blockType string, serialize func(io.Writer) error) string {
		var b bytes.Buffer
		w, aerr := armor.Encode(&b, blockType, nil)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if aerr = serialize(w); aerr != nil {
			t.Fatal(aerr)
		}
		w.Close()
		path := filepath.Join(dir, name)
		if aerr = os.WriteFile(path, b.Bytes(), 0600); aerr != nil {
			t.Fatal(aerr)
		}
		return path
	}
	pub := write("pub.asc", openpgp.PublicKeyType, e.Serialize)
	sec := write("sec.asc", openpgp.PrivateKeyType, func(w io.Writer) error { return e.SerializePrivate(w, nil) })
	return []string{"--publicKeyRingPath", pub, "--secretKeyRingPath", sec}
}

// A key with an expiry date is refused by send and receive unless --allowKeyExpiry: go-crypto
// checks a signer's expiry at restore time, so the backup would not restore after it.
func TestE2EKeyExpiryNeedsAllowKeyExpiry(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	const email = "expiring@example.com"
	rings := expiringRings(t, email)
	dest := "file://" + env.dest

	for _, keys := range [][]string{
		{"--encryptTo", email},
		{"--signFrom", email},
		{"--encryptTo", email, "--signFrom", email},
	} {
		args := append(append(append([]string{}, rings...), keys...), "tank/data@a", dest)
		logs, err := env.send(args...)
		if err == nil {
			t.Fatalf("send %v with an expiring key succeeded\n%s", keys, logs)
		}
		if !strings.Contains(logs, "Refusing PGP keys with an expiry date") || !strings.Contains(logs, "--allowKeyExpiry") {
			t.Errorf("send %v: the refusal does not name --allowKeyExpiry\n%s", keys, logs)
		}
	}
	if entries, err := os.ReadDir(env.dest); err == nil && len(entries) != 0 {
		t.Fatalf("a refused send left %d objects at the destination", len(entries))
	}

	keys := append(append([]string{}, rings...), "--encryptTo", email, "--signFrom", email)
	logs, err := env.send(append(append([]string{"--allowKeyExpiry"}, keys...), "tank/data@a", dest)...)
	if err != nil {
		t.Fatalf("send --allowKeyExpiry: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "its restore fails") {
		t.Errorf("send --allowKeyExpiry does not warn about the expiry\n%s", logs)
	}

	logs, err = env.receive(append(append([]string{}, keys...), "tank/data@a", dest, "restored/data")...)
	if err == nil || !strings.Contains(logs, "Refusing PGP keys with an expiry date") {
		t.Fatalf("receive with an expiring key: err = %v\n%s", err, logs)
	}
	logs, err = env.receive(append(append([]string{"--allowKeyExpiry"}, keys...), "tank/data@a", dest, "restored/data")...)
	if err != nil {
		t.Fatalf("receive --allowKeyExpiry: %v\n%s", err, logs)
	}

	// The fixtures never expire: no flag needed.
	logs, err = env.send("--publicKeyRingPath", "testdata/pgp/ed25519-public.asc",
		"--secretKeyRingPath", "testdata/pgp/ed25519-secret.asc",
		"--encryptTo", "ed25519@example.com", "--signFrom", "ed25519@example.com",
		"tank/data@a", "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("send with a key that never expires: %v\n%s", err, logs)
	}
	if strings.Contains(logs, "expires") {
		t.Errorf("a key that never expires got an expiry message\n%s", logs)
	}
}
