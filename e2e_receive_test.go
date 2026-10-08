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

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"

	"github.com/someone1/zfsbackup-go/files"
)

// newKey returns a new RSA key for backup@example.com. Keys from NewEntity need SHA256 as their
// preferred hash before the vendored openpgp encrypts to them.
func newKey(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("backup", "", "backup@example.com", &packet.Config{RSABits: 2048})
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

// writeRings writes an armored public and secret key ring holding keys, and returns their paths
// as send/receive flags.
func writeRings(t *testing.T, keys ...*openpgp.Entity) []string {
	t.Helper()
	dir := t.TempDir()
	var flags []string
	for _, ring := range []struct {
		flag, kind string
		private    bool
	}{
		{"--publicKeyRingPath", openpgp.PublicKeyType, false},
		{"--secretKeyRingPath", openpgp.PrivateKeyType, true},
	} {
		var buf bytes.Buffer
		w, err := armor.Encode(&buf, ring.kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range keys {
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
		path := filepath.Join(dir, ring.flag[2:])
		if err = ioutil.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		flags = append(flags, ring.flag, path)
	}
	return append(flags, "--encryptTo", "backup@example.com", "--signFrom", "backup@example.com")
}

// sentSet sends tank/data@a (3 MiB of stream, 1 MiB volumes) to env.dest and returns its
// volumes' object names, in order.
func (env *e2eEnv) sentSet(t *testing.T, args ...string) []string {
	t.Helper()
	t.Setenv("FAKEZFS_STREAM_BYTES", fmt.Sprint(3<<20))
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	args = append([]string{"--volsize", "1", "--compressor", ""}, args...)
	if logs, err := env.send(append(args, "tank/data@a", "file://"+env.dest)...); err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}
	var vols []string
	for name := range destObjects(t, env.dest) {
		if _, _, _, n, ok := files.ParseBackupVolumeObjectName(name, "|"); ok {
			for int64(len(vols)) < n {
				vols = append(vols, "")
			}
			vols[n-1] = name
		}
	}
	return vols
}

// receiveFails runs a receive that must fail, not hang or retry for --maxRetryTime (12h by
// default, which these runs keep).
func (env *e2eEnv) receiveFails(t *testing.T, args ...string) {
	t.Helper()
	t.Setenv("FAKEZFS_RECEIVE_LOG", filepath.Join(t.TempDir(), "receive.log"))
	args = append(args, "tank/data@a", "file://"+env.dest, "restored/data")
	if logs, err := guarded(t, func() (string, error) { return env.receive(args...) }); err == nil {
		t.Errorf("receive succeeded:\n%s", logs)
	}
}

func TestE2EReceiveFailsOnMissingVolume(t *testing.T) {
	for _, pipe := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipe=%v", pipe), func(t *testing.T) {
			env := newE2EEnv(t)
			vols := env.sentSet(t)
			if len(vols) < 3 {
				t.Fatalf("want at least 3 volumes, got %q", vols)
			}
			if err := os.Remove(filepath.Join(env.dest, vols[1])); err != nil {
				t.Fatal(err)
			}
			if pipe {
				env.receiveFails(t, "--maxFileBuffer", "0")
			} else {
				env.receiveFails(t)
			}
		})
	}
}

func TestE2EReceiveFailsOnUndecryptableVolume(t *testing.T) {
	env := newE2EEnv(t)
	k1, k2 := newKey(t), newKey(t)
	env.sentSet(t, writeRings(t, k1)...)
	// The manifest is encrypted too: give receive k1 for it, but only k2 for the volumes, by
	// swapping vol2 for one encrypted to k2.
	other := newE2EEnv(t)
	other.dest = filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other.dest, 0700); err != nil {
		t.Fatal(err)
	}
	vols := other.sentSet(t, writeRings(t, k2)...)
	data, err := ioutil.ReadFile(filepath.Join(other.dest, vols[1]))
	if err != nil {
		t.Fatal(err)
	}
	if err = ioutil.WriteFile(filepath.Join(env.dest, vols[1]), data, 0600); err != nil {
		t.Fatal(err)
	}
	env.receiveFails(t, writeRings(t, k1)...)
}

func TestE2EReceiveFailsFastOnHashMismatch(t *testing.T) {
	env := newE2EEnv(t)
	vols := env.sentSet(t)
	path := filepath.Join(env.dest, vols[0])
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err = ioutil.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	env.receiveFails(t)
}

// TestE2EReceiveAutoPrefersFull: the runbook restarts a chain with `send --full` of a monthly
// that was already sent as an incremental. `receive --auto` of that snapshot must restore from
// its full: one zfs receive while the old chain is still there, and success once the old
// chain's full has been retired (the incremental's parent is gone).
func TestE2EReceiveAutoPrefersFull(t *testing.T) {
	for _, retire := range []bool{false, true} {
		name := "keep-old-full"
		if retire {
			name = "old-full-retired"
		}
		t.Run(name, func(t *testing.T) {
			env := newE2EEnv(t)
			t.Setenv("FAKEZFS_STREAM_BYTES", "4096")
			env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
				{Name: "b", CreationTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
				{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			})
			for _, args := range [][]string{
				{"tank/data@a"},
				{"-i", "a", "tank/data@b"},
				{"tank/data@b"},
			} {
				args = append([]string{"--compressor", ""}, args...)
				if logs, err := env.send(append(args, "file://"+env.dest)...); err != nil {
					t.Fatalf("send %v: %v\n%s", args, err, logs)
				}
			}
			if retire {
				removed := 0
				for name := range destObjects(t, env.dest) {
					if name == "manifests|tank/data|a.manifest.gz" || strings.HasPrefix(name, "tank/data|a.zstream") {
						if err := os.Remove(filepath.Join(env.dest, name)); err != nil {
							t.Fatal(err)
						}
						removed++
					}
				}
				if removed < 2 {
					t.Fatalf("retired %d objects of the full of a, want its manifest and a volume", removed)
				}
				if err := os.RemoveAll(filepath.Join(env.work, "cache")); err != nil {
					t.Fatal(err)
				}
			}
			receiveLog := filepath.Join(t.TempDir(), "receive.log")
			t.Setenv("FAKEZFS_RECEIVE_LOG", receiveLog)
			logs, err := guarded(t, func() (string, error) {
				return env.receive("--auto", "tank/data@b", "file://"+env.dest, "restored/data")
			})
			if err != nil {
				t.Fatalf("receive --auto of b failed although a full of b is at the destination: %v\n%s", err, logs)
			}
			got, _ := ioutil.ReadFile(receiveLog)
			if n := strings.Count(string(got), "\n"); n != 1 {
				t.Errorf("want 1 zfs receive (the full of b), got %d\n%s", n, logs)
			}
		})
	}
}
