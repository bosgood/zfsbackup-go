//go:build integration

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/bosgood/zfsbackup-go/backends"
	"github.com/bosgood/zfsbackup-go/cmd"
)

// TestIntegrationSSHAuth sends one small snapshot over sftp, once per way of
// logging in and once per way of failing to: keys, the agent, passwords,
// users, ports, host keys, the redaction of a password, bad URIs. Each send
// goes to a new remote directory; a send that works must make a backup that
// list sees.
func TestIntegrationSSHAuth(t *testing.T) {
	pool := testPool(t)
	host, port, keys, password := itSSHEnv(t)
	itSSHClearEnv(t)
	_, base := itDestDataset(t, pool, "sshauth")
	src := pool + "/sshauthsrc"
	newSource(t, src, 1<<20)
	snap := snapshot(t, src, "daily")
	hostPort := host + ":" + port
	keyonly := filepath.Join(keys, "keyonly_ed25519")
	agentonly := filepath.Join(keys, "agentonly_ed25519")

	// newDir makes a remote directory for one send, owned by username.
	n := 0
	newDir := func(t *testing.T, username string) string {
		t.Helper()
		n++
		dir := filepath.Join(base, fmt.Sprintf("d%d", n))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		chownTree(t, username, dir)
		return dir
	}
	send := func(t *testing.T, work, target string) (string, error) {
		t.Helper()
		return itSend(t, work, append(slices.Clone(plainSend), append(slices.Clone(sshRetry), src+"@"+snap, target)...)...)
	}
	// ok sends to target and fails unless that works and list sees the backup.
	ok := func(t *testing.T, target string) {
		t.Helper()
		if logs, err := send(t, t.TempDir(), target); err != nil {
			t.Fatalf("send to %s: %v\n%s", backends.RedactURI(target), err, logs)
		}
		if backups := itList(t, t.TempDir(), target)[src]; len(backups) != 1 {
			t.Fatalf("list of %s: %v, want the backup of %s", backends.RedactURI(target), chainNames(backups), snap)
		}
	}
	// refused sends to target, fails unless the login is refused, and returns the error's text.
	refused := func(t *testing.T, target string) string {
		t.Helper()
		logs, err := send(t, t.TempDir(), target)
		if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
			t.Fatalf("send to %s: got %v, want the login refused\n%s", backends.RedactURI(target), err, logs)
		}
		return err.Error()
	}
	// accepted fails unless sshd logged, after offset, a login of username by
	// method ("publickey", "password") and, for a key, with the key at keyPath.
	accepted := func(t *testing.T, offset int64, method, username, keyPath string) {
		t.Helper()
		want := "Accepted " + method + " for " + username + " from " + host
		if !sshdLogHas(t, offset, want) {
			t.Errorf("sshd logged no %q:\n%s", want, sshdLogSince(t, offset))
		}
		if keyPath != "" && !sshdLogHas(t, offset, keyFingerprint(t, keyPath)) {
			t.Errorf("sshd did not log the key %s (%s):\n%s", keyPath, keyFingerprint(t, keyPath), sshdLogSince(t, offset))
		}
	}

	t.Run("DefaultKey", func(t *testing.T) {
		offset := sshdLogSize(t)
		ok(t, "ssh://root@"+hostPort+newDir(t, "root"))
		accepted(t, offset, "publickey", "root", "/root/.ssh/id_ed25519")
	})

	t.Run("KeyFile", func(t *testing.T) {
		target := "ssh://keyonly@" + hostPort + newDir(t, "keyonly")
		t.Setenv("SSH_KEY_FILE", keyonly)
		offset := sshdLogSize(t)
		ok(t, target)
		accepted(t, offset, "publickey", "keyonly", keyonly)
		// Root's default key is not authorized for keyonly, and no password is tried.
		t.Setenv("SSH_KEY_FILE", "")
		if msg := refused(t, target); strings.Contains(msg, "password") {
			t.Errorf("the error names a password, and none was given: %s", msg)
		}
	})

	t.Run("Agent", func(t *testing.T) {
		sock := startAgent(t, agentonly)
		target := "ssh://agentonly@" + hostPort + newDir(t, "agentonly")
		t.Setenv("SSH_AUTH_SOCK", sock)
		// Not authorized for agentonly: only the agent can log in.
		t.Setenv("SSH_KEY_FILE", "/root/.ssh/id_ed25519")
		offset := sshdLogSize(t)
		ok(t, target)
		accepted(t, offset, "publickey", "agentonly", agentonly)
		t.Setenv("SSH_AUTH_SOCK", "")
		refused(t, target)
		// The agent's key is refused for keyonly: SSH_KEY_FILE must still be offered after it.
		t.Setenv("SSH_AUTH_SOCK", sock)
		t.Setenv("SSH_KEY_FILE", keyonly)
		offset = sshdLogSize(t)
		ok(t, "ssh://keyonly@"+hostPort+newDir(t, "keyonly"))
		accepted(t, offset, "publickey", "keyonly", keyonly)
	})

	t.Run("Password", func(t *testing.T) {
		dir := newDir(t, "pwonly")
		t.Setenv("SSH_PASSWORD", password)
		offset := sshdLogSize(t)
		ok(t, "ssh://pwonly@"+hostPort+dir)
		accepted(t, offset, "password", "pwonly", "")

		t.Setenv("SSH_PASSWORD", "")
		offset = sshdLogSize(t)
		ok(t, itSSHTargetPassword(t, "pwonly", password, newDir(t, "pwonly")))
		accepted(t, offset, "password", "pwonly", "")

		refused(t, itSSHTargetPassword(t, "pwonly", "wrong-pw", newDir(t, "pwonly")))
		t.Setenv("SSH_PASSWORD", "wrong-pw")
		refused(t, "ssh://pwonly@"+hostPort+newDir(t, "pwonly"))
	})

	t.Run("Username", func(t *testing.T) {
		t.Setenv("SSH_USERNAME", "pwonly")
		t.Setenv("SSH_PASSWORD", password)
		offset := sshdLogSize(t)
		ok(t, "ssh://"+hostPort+newDir(t, "pwonly"))
		accepted(t, offset, "password", "pwonly", "")
		// The user in the URI wins over SSH_USERNAME.
		offset = sshdLogSize(t)
		ok(t, "ssh://root@"+hostPort+newDir(t, "pwonly"))
		accepted(t, offset, "publickey", "root", "/root/.ssh/id_ed25519")
	})

	t.Run("Port", func(t *testing.T) {
		// No port: Init adds :22, where nothing listens.
		start := time.Now()
		logs, err := send(t, t.TempDir(), "ssh://root@"+host+newDir(t, "root"))
		if err == nil || !strings.Contains(err.Error(), host+":22") {
			t.Fatalf("send with no port in the URI: got %v, want a dial error for %s:22\n%s", err, host, logs)
		}
		if took := time.Since(start); took > 30*time.Second {
			t.Errorf("the failed dial took %v, more than the 30 s timeout", took)
		}
		t.Logf("no port: %v", err)
	})

	t.Run("HostKey", func(t *testing.T) {
		target := "ssh://root@" + hostPort + newDir(t, "root")
		// Another host's key for [127.0.0.1]:2222.
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		other, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		knownHosts := filepath.Join(t.TempDir(), "known_hosts")
		line := knownhosts.Line([]string{hostPort}, other) + "\n"
		if err = os.WriteFile(knownHosts, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SSH_KNOWN_HOSTS", knownHosts)
		offset := sshdLogSize(t)
		logs, err := send(t, t.TempDir(), target)
		if err == nil || !strings.Contains(err.Error(), "knownhosts: key mismatch") {
			t.Fatalf("send to a host with another key: got %v, want the knownhosts mismatch\n%s", err, logs)
		}
		if sshdLogHas(t, offset, "Accepted ") {
			t.Errorf("sshd accepted a login although the host key did not match:\n%s", sshdLogSince(t, offset))
		}

		missing := filepath.Join(t.TempDir(), "no_known_hosts")
		t.Setenv("SSH_KNOWN_HOSTS", missing)
		if logs, err = send(t, t.TempDir(), target); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("send with SSH_KNOWN_HOSTS=%s (no such file): got %v, want an error that names it\n%s", missing, err, logs)
		}

		t.Setenv("SSH_KNOWN_HOSTS", "ignore")
		ok(t, target)
	})

	t.Run("Redaction", func(t *testing.T) {
		target := itSSHTargetPassword(t, "pwonly", password, newDir(t, "pwonly"))
		work := t.TempDir()
		check := func(what, logs string, err error) {
			t.Helper()
			wantNotInLog(t, logs+fmt.Sprint(err), password)
			if t.Failed() {
				t.Fatalf("%s leaked the password", what)
			}
		}
		logs, err := send(t, work, target)
		if err != nil {
			t.Fatalf("send: %v\n%s", err, logs)
		}
		check("send", logs, err)
		out, logs, err := itRunOut(cmd.ResetListJobInfo, "list", work, "zfs", target)
		if err != nil {
			t.Fatalf("list: %v\n%s", err, logs)
		}
		check("list", out+logs, err)
		logs, err = itClean(t, work, "--dry-run", target)
		if err != nil {
			t.Fatalf("clean --dry-run: %v\n%s", err, logs)
		}
		check("clean --dry-run", logs, err)
		logs, err = send(t, work, strings.Replace(target, password, password+"-wrong", 1))
		if err == nil {
			t.Fatalf("send with a wrong password: no error\n%s", logs)
		}
		check("a send with a wrong password", logs, err)
		// Nor anything the runs left in the working directory.
		err = filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && bytes.Contains(data, []byte(password)) {
				t.Errorf("%s holds the password", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("BadURI", func(t *testing.T) {
		// No path, and a path that is a regular file: ErrInvalidURI.
		file := filepath.Join(newDir(t, "root"), "a-file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{"ssh://root@" + hostPort, "ssh://root@" + hostPort + file} {
			if logs, err := send(t, t.TempDir(), target); !errors.Is(err, backends.ErrInvalidURI) {
				t.Errorf("send to %s: got %v, want %v\n%s", target, err, backends.ErrInvalidURI, logs)
			}
		}
		// A path that does not exist: an error, and the backend does not make it.
		missing := filepath.Join(base, "no-such-dir")
		if logs, err := send(t, t.TempDir(), "ssh://root@"+hostPort+missing); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("send to the missing %s: got %v, want %v\n%s", missing, err, fs.ErrNotExist, logs)
		}
		if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("after a send to the missing %s: stat says %v, want it still missing", missing, err)
		}
	})
}

// startAgent starts an ssh-agent with the key at keyPath and returns its
// socket. The agent stops when the test ends.
func startAgent(t *testing.T, keyPath string) string {
	t.Helper()
	// Not t.TempDir(): a unix socket path must stay short.
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	if err = agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_ = agent.Wait()
	})
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err = os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("ssh-agent made no socket %s in 10 s", sock)
		}
	}
	add := exec.Command("ssh-add", keyPath)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add %s: %v: %s", keyPath, err, out)
	}
	return sock
}
