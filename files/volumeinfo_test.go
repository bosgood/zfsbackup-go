package files

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"path/filepath"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/pgp"
)

const pgpFixtures = "../testdata/pgp/"

// loadKeys points the pgp package's global rings at one fixture pair and
// returns the entity for email from each.
func loadKeys(t *testing.T, prefix, email string) (pub, sec *openpgp.Entity) {
	t.Helper()
	if err := pgp.LoadPublicRing(pgpFixtures + prefix + "-public.asc"); err != nil {
		t.Fatal(err)
	}
	if err := pgp.LoadPrivateRing(pgpFixtures + prefix + "-secret.asc"); err != nil {
		t.Fatal(err)
	}
	pub, sec = pgp.GetPublicKeyByEmail(email), pgp.GetPrivateKeyByEmail(email)
	if pub == nil || sec == nil {
		t.Fatalf("fixture %s has no key for %s", prefix, email)
	}
	return pub, sec
}

func testJob(encrypt, sign *openpgp.Entity) *JobInfo {
	return &JobInfo{
		VolumeName:    "tank/data",
		BaseSnapshot:  SnapshotInfo{Name: "a", CreationTime: time.Unix(1700000000, 0)},
		Compressor:    "",
		MaxFileBuffer: 1, // temp file, not a pipe
		EncryptKey:    encrypt,
		SignKey:       sign,
	}
}

// writeVolume writes payload through the send-side pipeline and returns the
// temp file it produced.
func writeVolume(t *testing.T, j *JobInfo, payload []byte) string {
	t.Helper()
	v, err := CreateBackupVolume(context.Background(), j, 1)
	if err != nil {
		t.Fatalf("CreateBackupVolume: %v", err)
	}
	if _, err = v.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err = v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return v.filename
}

func readVolume(j *JobInfo, path string) ([]byte, error) {
	v, err := ExtractLocal(context.Background(), j, path, false)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	return ioutil.ReadAll(v)
}

func TestVolumePGPRoundTrip(t *testing.T) {
	config.BackupTempdir = t.TempDir()
	payload := bytes.Repeat([]byte("zfs send stream "), 4096) // 64 KiB, crosses the bufio size

	for _, fixture := range []struct{ prefix, email string }{
		{"ed25519", "ed25519@example.com"},
		{"rsa", "rsa@example.com"},
	} {
		pub, sec := loadKeys(t, fixture.prefix, fixture.email)
		modes := []struct {
			name       string
			send, recv *JobInfo
		}{
			{"encrypt", testJob(pub, nil), testJob(sec, nil)},
			{"sign", testJob(nil, sec), testJob(nil, pub)},
			{"encrypt+sign", testJob(pub, sec), testJob(sec, pub)},
		}
		for _, m := range modes {
			t.Run(fixture.prefix+"/"+m.name, func(t *testing.T) {
				path := writeVolume(t, m.send, payload)
				raw, _ := ioutil.ReadFile(path)
				if bytes.Contains(raw, payload[:64]) && m.name != "sign" {
					t.Fatal("volume contains the plaintext")
				}
				got, err := readVolume(m.recv, path)
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
				}
			})
		}
	}
}

func TestVolumePGPTamperDetected(t *testing.T) {
	config.BackupTempdir = t.TempDir()
	pub, sec := loadKeys(t, "ed25519", "ed25519@example.com")
	payload := bytes.Repeat([]byte("x"), 8192)
	path := writeVolume(t, testJob(pub, sec), payload)

	raw, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err = ioutil.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readVolume(testJob(sec, pub), path); err == nil {
		t.Fatal("tampered volume read back without error")
	}
}

func TestVolumePGPWrongKeyFails(t *testing.T) {
	config.BackupTempdir = t.TempDir()
	pub, _ := loadKeys(t, "ed25519", "ed25519@example.com")
	path := writeVolume(t, testJob(pub, nil), []byte("secret"))
	// Rings now hold only the RSA keys: nothing can decrypt the volume.
	_, rsaSec := loadKeys(t, "rsa", "rsa@example.com")
	if _, err := readVolume(testJob(rsaSec, nil), path); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

// GnuPG does not emit RFC 9580 v6 keys, so this one is generated here:
// Ed25519 (algo 27) primary with an X25519 (algo 25) subkey.
func TestVolumePGPRoundTripV6Key(t *testing.T) {
	config.BackupTempdir = t.TempDir()
	const email = "v6@example.com"
	e, err := openpgp.NewEntity("zfsbackup v6 test", "", email, &packet.Config{
		V6Keys:    true,
		Algorithm: packet.PubKeyAlgoEd25519,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeArmored := func(name, blockType string, serialize func(w io.Writer) error) {
		var b bytes.Buffer
		w, aerr := armor.Encode(&b, blockType, nil)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if aerr = serialize(w); aerr != nil {
			t.Fatal(aerr)
		}
		w.Close()
		if aerr = ioutil.WriteFile(filepath.Join(dir, name), b.Bytes(), 0600); aerr != nil {
			t.Fatal(aerr)
		}
	}
	writeArmored("v6-public.asc", openpgp.PublicKeyType, e.Serialize)
	writeArmored("v6-secret.asc", openpgp.PrivateKeyType, func(w io.Writer) error { return e.SerializePrivate(w, nil) })
	if err = pgp.LoadPublicRing(filepath.Join(dir, "v6-public.asc")); err != nil {
		t.Fatal(err)
	}
	if err = pgp.LoadPrivateRing(filepath.Join(dir, "v6-secret.asc")); err != nil {
		t.Fatal(err)
	}
	pub, sec := pgp.GetPublicKeyByEmail(email), pgp.GetPrivateKeyByEmail(email)
	if pub == nil || sec == nil {
		t.Fatal("v6 key not found after loading")
	}
	if pub.PrimaryKey.Version != 6 || pub.PrimaryKey.PubKeyAlgo != packet.PubKeyAlgoEd25519 {
		t.Fatalf("primary key is v%d algo %d, want v6 Ed25519", pub.PrimaryKey.Version, pub.PrimaryKey.PubKeyAlgo)
	}

	payload := bytes.Repeat([]byte("v6 "), 4096)
	path := writeVolume(t, testJob(pub, sec), payload)
	got, err := readVolume(testJob(sec, pub), path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
