package files

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/someone1/zfsbackup-go/config"
)

// gpgHome imports the ed25519 fixture into a throwaway GNUPGHOME and returns
// a runner for gpg commands against it.
func gpgHome(t *testing.T) func(args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	// Not t.TempDir(): gpg-agent puts its socket in the home directory and
	// that path is too long for a Unix socket on macOS.
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("gpg", append([]string{"--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		runErr := cmd.Run()
		if runErr != nil {
			runErr = &gpgError{runErr, errb.String()}
		}
		return out.Bytes(), runErr
	}
	t.Cleanup(func() {
		kill := exec.Command("gpgconf", "--kill", "gpg-agent")
		kill.Env = append(os.Environ(), "GNUPGHOME="+home)
		_ = kill.Run()
		os.RemoveAll(home)
	})
	if _, err = run("--import", pgpFixtures+"ed25519-secret.asc"); err != nil {
		t.Fatalf("gpg --import: %v", err)
	}
	return run
}

type gpgError struct {
	err    error
	stderr string
}

func (e *gpgError) Error() string { return e.err.Error() + ": " + e.stderr }

func TestGPGDecryptsAndVerifiesOurVolume(t *testing.T) {
	run := gpgHome(t)
	config.BackupTempdir = t.TempDir()
	pub, sec := loadKeys(t, "ed25519", "ed25519@example.com")
	payload := bytes.Repeat([]byte("interop "), 1024)
	path := writeVolume(t, testJob(pub, sec), payload)

	status := filepath.Join(t.TempDir(), "status")
	out, err := run("--trust-model", "always", "--status-file", status, "--decrypt", path)
	if err != nil {
		t.Fatalf("gpg --decrypt: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("gpg decrypted %d bytes, want %d", len(out), len(payload))
	}
	// gpg checks the signature while decrypting (`--verify` rejects an
	// encrypted message); the status lines say whether it was good.
	st, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[GNUPG:] DECRYPTION_OKAY", "[GNUPG:] GOODSIG ", "[GNUPG:] VALIDSIG "} {
		if !bytes.Contains(st, []byte(want)) {
			t.Errorf("gpg status has no %q:\n%s", want, st)
		}
	}
}

func TestWeDecryptGPGEncryptedVolume(t *testing.T) {
	run := gpgHome(t)
	config.BackupTempdir = t.TempDir()
	pub, sec := loadKeys(t, "ed25519", "ed25519@example.com")
	payload := bytes.Repeat([]byte("from gpg "), 1024)
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, payload, 0600); err != nil {
		t.Fatal(err)
	}
	enc := plain + ".pgp"
	if _, err := run("--trust-model", "always", "--recipient", "ed25519@example.com",
		"--local-user", "ed25519@example.com", "--compress-algo", "none",
		"--output", enc, "--sign", "--encrypt", plain); err != nil {
		t.Fatalf("gpg --encrypt: %v", err)
	}
	got, err := readVolume(testJob(sec, pub), enc)
	if err != nil {
		t.Fatalf("ExtractLocal on gpg output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch (%d bytes)", len(got))
	}
}
