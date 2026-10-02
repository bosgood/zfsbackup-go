# Ed25519 / Curve25519 PGP Support Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `zfsbackup` load, encrypt to, sign with, decrypt with and verify against Ed25519/Curve25519 OpenPGP keys (the GnuPG >= 2.3 default), without breaking existing RSA keyrings or backups already on disk.

**Architecture:** Replace the deprecated, frozen `golang.org/x/crypto/openpgp` with its maintained API-compatible fork `github.com/ProtonMail/go-crypto/openpgp` (the v1 API, not `openpgp/v2`). The call sites (`pgp/pgp.go`, `cmd/root.go`, `files/jobinfo.go`, `files/volumeinfo.go`) only change their import paths; the on-the-wire message format (SEIPD v1, AES-256, SHA-256 signatures) stays identical because we never set `AEADConfig`. The real work is dependency management (vendored tree, no Go proxy from the host) and the test coverage the crypto path never had.

**Tech Stack:** Go 1.25, `github.com/ProtonMail/go-crypto` v1.5.2 (pulls `golang.org/x/crypto` v0.41.0 and `github.com/cloudflare/circl` v1.6.3), GnuPG 2.2.40 (in the `golang:1.25-bookworm` image) for fixtures and interop tests, Docker via the Makefile.

---

## Background the implementer needs

### Why it fails today

`vendor/golang.org/x/crypto/openpgp/packet/public_key.go:270-295` parses only
algorithms 1/2/3 (RSA), 16 (ElGamal), 17 (DSA), 18 (ECDH) and 19 (ECDSA).
Anything else, including 22 (EdDSA = Ed25519 in OpenPGP), returns
`errors.UnsupportedError("public key type: 22")`. `ReadKeyRing`
(`keys.go:253`) skips such entities; when *every* entity in the file is
unsupported it returns that error, which `cmd/root.go:218-229` logs as
"Could not load public keyring" and exits.

Even an RSA primary with a `cv25519` subkey cannot be used for encryption:
`SerializeEncryptedKey` (`packet/encrypted_key.go`) only handles RSA and
ElGamal and errors with `encrypting a key to public key of type 18`.

### Why go-crypto v1 is a drop-in

Verified against a clone of go-crypto at tag **v1.5.2** (in `tmp/go-crypto`,
gitignored). Every symbol this repo uses exists with the same signature:

| Symbol | Used at |
|---|---|
| `openpgp.ReadArmoredKeyRing(io.Reader) (EntityList, error)` | `pgp/pgp.go` |
| `openpgp.EntityList`, `openpgp.KeyRing`, `openpgp.Entity`, `openpgp.Key` | `pgp/pgp.go`, `files/jobinfo.go` |
| `openpgp.PromptFunction` = `func(keys []Key, symmetric bool) ([]byte, error)` | `pgp.PromptFunc` |
| `openpgp.ReadMessage(r, keyring, prompt, *packet.Config) (*MessageDetails, error)` | `files/volumeinfo.go:210` |
| `MessageDetails.{IsSigned, SignatureError, SignedBy, UnverifiedBody}` | `files/volumeinfo.go:126-133` |
| `openpgp.Encrypt(w, []*Entity, signed *Entity, *FileHints, *packet.Config)` | `files/volumeinfo.go:428` |
| `openpgp.Sign(w, *Entity, *FileHints, *packet.Config)` | `files/volumeinfo.go:432` |
| `packet.Config.{DefaultCompressionAlgo, DefaultCipher, DefaultHash, RSABits}` | `files/volumeinfo.go:207-209, 418-422` |
| `packet.CompressionNone`, `packet.CipherAES256` | same |
| `Entity.PrivateKey.Encrypted`, `PrivateKey.Decrypt([]byte) error` | `cmd/root.go:248-262` |
| `Entity.Identities` (map) with `.UserId.Email`, `Entity.PrimaryKey.KeyIdString()` | `pgp/pgp.go` |

Additional facts that shape the plan:

- go-crypto's v1 API applies **no** algorithm-rejection policy (the
  `Reject*` maps in `packet/config.go` are only consulted by `openpgp/v2`).
  Old RSA/DSA keyrings and SHA-1-self-signed keys still load. Do not migrate
  to `openpgp/v2`; it is a different API and nothing here needs it.
- It parses EdDSA (22), ECDH over Curve25519 (18), and the RFC 9580 v6 forms
  Ed25519 (27) / X25519 (25), plus AEAD-protected secret keys (S2K usage
  253) that newer GnuPG exports.
- With `AEADConfig == nil`, `Encrypt` writes the same SEIPD v1 packets as
  before, so old binaries and `gpg` decrypt new backups.
- `golang.org/x/crypto` v0.41.0 (the version go-crypto v1.5.2 requires)
  still ships `ssh/terminal` (`cmd/root.go:39`), `pkcs12` (Azure adal) and
  `ssh` (`pkg/sftp`), so the bump does not orphan any importer.

### Repo constraints (see `.claude/napkin.md`)

- Dependencies are **vendored** (`vendor/` is committed; the test image has
  no download step). Any go.mod change must be followed by `go mod vendor`
  and the result committed, or every build fails with an inconsistent
  `vendor/modules.txt`.
- The host cannot reach `proxy.golang.org`; containers can. Module
  operations therefore run inside Docker through a Makefile target (new
  `vendor` target below). Never run ad-hoc `go get` on the host.
- Build/test through the Makefile: `make test-docker` (full suite in the
  supported toolchain, ~3 min), `make test-run PKG=./x/ RUN=Name` (fast
  host loop), `make e2e`. `make fmt` before committing.
- `grep` on this host is `ugrep`; use `/usr/bin/grep`. Quote globs
  (`--include='*.go'`). The Bash tool is zsh.
- Working tree may be shared with another session: stage explicit paths.

### Files touched

| File | Change |
|---|---|
| `Makefile` | new `vendor` and `pgp-fixtures` targets |
| `go.mod`, `go.sum`, `vendor/` | add go-crypto, bump x/crypto and friends |
| `pgp/pgp.go` | import path; log key algorithm in debug output |
| `cmd/root.go` | import path (openpgp) |
| `files/jobinfo.go` | import path |
| `files/volumeinfo.go` | import paths (openpgp, packet) |
| `testdata/pgp/generate.sh` + `testdata/pgp/*.asc` | new committed keyring fixtures |
| `pgp/pgp_test.go` | new |
| `files/volumeinfo_test.go`, `files/volumeinfo_gpg_test.go` | new |
| `e2e_test.go` | new `TestE2EEncryptedSend` |
| `README.md` | Encryption/Signing section |
| `.claude/napkin.md`, `docs/specs/2026-09-29--pgp-ed25519/plan.md` | notes |

---

### Task 1: Keyring fixtures generated by GnuPG

Committed, passphrase-less (plus one passphrase-protected) armored keyrings
so unit tests never need `gpg`. Generated inside the Go image so the GnuPG
version is pinned (2.2.40) and the script is reproducible.

**Files:**
- Create: `testdata/pgp/generate.sh`
- Create: `testdata/pgp/README.md`
- Modify: `Makefile` (add `pgp-fixtures` target after `pgp` line of `plan:`)
- Generated: `testdata/pgp/ed25519-public.asc`, `ed25519-secret.asc`, `ed25519-pass-secret.asc`, `rsa-public.asc`, `rsa-secret.asc`

**Step 1: Write the generator**

`testdata/pgp/generate.sh`:

```bash
#!/usr/bin/env bash
# Regenerates the OpenPGP keyring fixtures in this directory with GnuPG.
# Run through `make pgp-fixtures` so the GnuPG version is the one in the
# Go image; the *.asc files are committed and the tests do not need gpg.
#
#   ed25519-*.asc      Ed25519 signing key + cv25519 encryption subkey,
#                      the GnuPG >= 2.3 default. No passphrase.
#   ed25519-pass-*.asc Same shape, secret key protected with $PASSPHRASE.
#   rsa-*.asc          RSA 2048 sign+encrypt, what the tool supported before.
set -euo pipefail
cd "$(dirname "$0")"

PASSPHRASE=zfsbackup-test
export GNUPGHOME
GNUPGHOME="$(mktemp -d)"
trap 'rm -rf "$GNUPGHOME"' EXIT
GPG=(gpg --batch --quiet --pinentry-mode loopback)

# gen NAME EMAIL ALGO SUBALGO PASSPHRASE PREFIX
gen() {
  local name=$1 email=$2 algo=$3 subalgo=$4 pass=$5 prefix=$6 fpr
  "${GPG[@]}" --passphrase "$pass" --quick-generate-key "$name <$email>" "$algo" sign never
  fpr=$("${GPG[@]}" --with-colons --list-keys "$email" | awk -F: '/^fpr/ {print $10; exit}')
  "${GPG[@]}" --passphrase "$pass" --quick-add-key "$fpr" "$subalgo" encr never
  "${GPG[@]}" --armor --export "$email" > "$prefix-public.asc"
  "${GPG[@]}" --passphrase "$pass" --armor --export-secret-keys "$email" > "$prefix-secret.asc"
}

gen "zfsbackup ed25519 test" ed25519@example.com ed25519 cv25519 "" ed25519
gen "zfsbackup ed25519 passphrase test" ed25519-pass@example.com ed25519 cv25519 "$PASSPHRASE" ed25519-pass
gen "zfsbackup rsa test" rsa@example.com rsa2048 rsa2048 "" rsa

"${GPG[@]}" --list-keys --with-subkey-fingerprints
```

`chmod +x testdata/pgp/generate.sh`.

`testdata/pgp/README.md`: three lines saying what the files are, the
passphrase `zfsbackup-test` for `ed25519-pass-secret.asc`, and "regenerate
with `make pgp-fixtures`; never use these keys for anything real".

**Step 2: Makefile target**

Append to `Makefile`:

```makefile
# Regenerate the OpenPGP keyring fixtures under testdata/pgp with the GnuPG
# shipped in the Go image (see testdata/pgp/generate.sh).
pgp-fixtures:
	docker run --rm -v "$(PWD)":/src -w /src golang:$(GO_VERSION)-bookworm bash testdata/pgp/generate.sh
```

**Step 3: Generate and inspect**

Run: `make pgp-fixtures`
Expected: `gpg --list-keys` output showing three keys; for the ed25519 ones
`pub   ed25519 ... [SC]` and `sub   cv25519 ... [E]`; five `*.asc` files in
`testdata/pgp/`, each starting with `-----BEGIN PGP PUBLIC KEY BLOCK-----` or
`-----BEGIN PGP PRIVATE KEY BLOCK-----`.

If Docker leaves the files root-owned (Linux hosts), `chown` them; on this
Mac they come out user-owned.

**Step 4: Commit**

```bash
git add testdata/pgp Makefile
git commit -m "test: add GnuPG-generated ed25519 and rsa keyring fixtures"
```

---

### Task 2: Failing unit tests for the pgp package

These compile against both libraries (no `packet` import, the algorithm id
is a local constant, and only fields that exist in both `Entity` types are
used: `Entity.EncryptionKey`/`SigningKey` are unexported in x/crypto), so
they fail *now* for the right reason and pass after the swap.

**Files:**
- Create: `pgp/pgp_test.go`

**Step 1: Write the tests**

```go
package pgp_test

import (
	"testing"

	"github.com/someone1/zfsbackup-go/pgp"
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
```

Do not assert on `GetCombinedKeyRing().KeysById(...)` in these tests: the
return types differ between the two libraries and the test must compile
against both.

**Step 2: Run to verify they fail for the expected reason**

Run: `make test-run PKG=./pgp/ RUN='TestLoad|TestDecrypt'`
Expected:
- `TestLoadEd25519PublicRing`, `TestLoadEd25519PrivateRing`,
  `TestDecryptPassphraseProtectedEd25519Key` FAIL with
  `openpgp: unsupported feature: public key type: 22`
- `TestLoadRSARingsStillWork`, `TestLoadMissingRing` PASS

If the ed25519 tests pass already, the fixtures are not Ed25519; go back to
Task 1.

**Step 3: Commit the red tests**

```bash
git add pgp/pgp_test.go
git commit -m "pgp: tests for ed25519/cv25519 keyrings (red: x/crypto rejects algo 22)"
```

---

### Task 3: Bump `golang.org/x/crypto` first, alone

Separating the x/crypto bump from the go-crypto addition keeps the
go-crypto commit reviewable and proves the bump itself is harmless to the
SSH backend (`pkg/sftp`) and Azure (`pkcs12`).

**Files:**
- Modify: `Makefile` (add `vendor` target)
- Modify: `go.mod`, `go.sum`, `vendor/**`

**Step 1: Makefile target for module operations**

Append to `Makefile`:

```makefile
# Dependencies are vendored and this host cannot reach proxy.golang.org, so
# module operations run in the Go image. Usage:
#   make vendor                                  # go mod tidy && go mod vendor
#   make vendor GOGET=github.com/x/y@v1.2.3      # go get first, then tidy+vendor
GOGET ?=
vendor:
	docker run --rm -v "$(PWD)":/src -v /tmp/zfsb-gomodcache:/go/pkg/mod -w /src \
		-e GOFLAGS=-mod=mod golang:$(GO_VERSION)-bookworm \
		bash -c 'set -e; if [ -n "$(GOGET)" ]; then go get $(GOGET); fi; go mod tidy; go mod vendor'
```

(`GOFLAGS=-mod=mod` is required: with a `vendor/` directory present Go
defaults to `-mod=vendor` and `go get` refuses to run.)

**Step 2: Bump**

Run: `make vendor GOGET=golang.org/x/crypto@v0.41.0`
Expected: go.mod now has `golang.org/x/crypto v0.41.0`; `x/sys`, `x/term`,
`x/net`, `x/text` bumped as required by its go.mod; `vendor/modules.txt`
regenerated. Check the result:

```bash
git diff --stat go.mod
git diff go.mod
/usr/bin/grep -n 'x/crypto\|x/sys\|x/net\|x/term' go.mod
ls vendor/golang.org/x/crypto/ssh/terminal vendor/golang.org/x/crypto/pkcs12   # must still exist
```

**Step 3: Full build and test in the supported toolchain**

Run: `make test-docker`
Expected: image builds (`go build ./...` inside it is the first real gate),
all packages `ok`. The 60 s backends/backup retry tests are normal.

If `go mod tidy` fails to resolve (e.g. a module now requires a newer
`google.golang.org/grpc` than v1.49), stop and report the exact error; the
fallback is an older go-crypto (each tag's go.mod names its x/crypto
requirement, `git -C tmp/go-crypto show vX.Y.Z:go.mod`) rather than
force-upgrading unrelated cloud SDKs.

**Step 4: Commit**

```bash
git add Makefile go.mod go.sum vendor
git commit -m "deps: bump golang.org/x/crypto to v0.41.0; add make vendor"
```

---

### Task 4: Switch to `github.com/ProtonMail/go-crypto/openpgp`

**Files:**
- Modify: `pgp/pgp.go:27` (import), `pgp/pgp.go:99-111` (debug output)
- Modify: `cmd/root.go:38`
- Modify: `files/jobinfo.go:30`
- Modify: `files/volumeinfo.go:45-46`
- Modify: `go.mod`, `go.sum`, `vendor/**`

**Step 1: Rewrite the imports**

```bash
cd /Users/bosgood/dev/src/github.com/bosgood/zfsbackup-go
sed -i '' \
  -e 's#"golang.org/x/crypto/openpgp"#"github.com/ProtonMail/go-crypto/openpgp"#' \
  -e 's#"golang.org/x/crypto/openpgp/packet"#"github.com/ProtonMail/go-crypto/openpgp/packet"#' \
  pgp/pgp.go cmd/root.go files/jobinfo.go files/volumeinfo.go
/usr/bin/grep -rn 'x/crypto/openpgp' --include='*.go' pgp cmd files backup backends   # expect no output
```

`cmd/root.go` keeps `golang.org/x/crypto/ssh/terminal` (Task 9 may replace
it). Re-sort the import blocks so the new path sits in the third-party
group in alphabetical order (`github.com/ProtonMail/...` sorts before
`github.com/cenkalti/...` because uppercase sorts first); `gofmt` does not
reorder across groups, so do it by hand, then `make fmt`.

**Step 2: Make the debug listing say what kind of key was loaded**

In `pgp/pgp.go` `PrintPGPDebugInformation`, change both `Sprintf` calls to
include the algorithm, which is the first thing to look at when a user
reports "key not found":

```go
debugStr = append(debugStr, fmt.Sprintf("\t%v (algo %d)\n\t%v",
	key.PrimaryKey.KeyIdString(), key.PrimaryKey.PubKeyAlgo, key.Identities))
```

Also fix the package doc: add above `package pgp`:

```go
// Package pgp loads OpenPGP keyrings for the encrypt/sign options. It uses
// github.com/ProtonMail/go-crypto, the maintained fork of the frozen
// golang.org/x/crypto/openpgp, which adds Ed25519/Curve25519 (the GnuPG
// >= 2.3 default), v6 keys and AEAD-protected secret keys.
```

**Step 3: Add the module and vendor it**

Run: `make vendor GOGET=github.com/ProtonMail/go-crypto@v1.5.2`
Expected: `go.mod` gains `github.com/ProtonMail/go-crypto v1.5.2` (direct)
and `github.com/cloudflare/circl v1.6.3 // indirect`;
`vendor/github.com/ProtonMail/go-crypto/openpgp/...` and
`vendor/github.com/cloudflare/circl/...` appear;
`vendor/golang.org/x/crypto/openpgp/` is **removed** by `go mod vendor`
(nothing imports it any more). Verify:

```bash
/usr/bin/grep -n 'ProtonMail\|circl' go.mod
ls vendor/github.com/ProtonMail/go-crypto/openpgp/packet | head -3
test ! -d vendor/golang.org/x/crypto/openpgp && echo "old openpgp gone"
```

**Step 4: Run the red tests**

Run: `make test-run PKG=./pgp/`
Expected: all five tests PASS.

**Step 5: Build and run everything**

Run: `make fmt && make test-docker`
Expected: `ok` for every package. Nothing else in the tree references the
old package (`backends/ssh_backend.go` uses `x/crypto/ssh`, untouched).

**Step 6: Commit**

```bash
git add pgp/pgp.go cmd/root.go files/jobinfo.go files/volumeinfo.go go.mod go.sum vendor
git commit -m "pgp: switch to ProtonMail/go-crypto for ed25519/cv25519 keys"
```

---

### Task 5: Volume encrypt/sign/decrypt/verify round trip

The `files` package has no tests. This adds the one that matters: bytes
written through `CreateBackupVolume` with each key type come back out of
`ExtractLocal` intact, signatures verify, tampering is detected. Internal
test (`package files`) so it can read the unexported temp filename.

**Files:**
- Create: `files/volumeinfo_test.go`

**Step 1: Write the test**

```go
package files

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

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
				if bytes.Contains(raw, payload[:64]) {
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
```

Note for the implementer: the receive side reads through
`pgp.GetCombinedKeyRing()` (a package global), so the *last* `loadKeys` call
decides what can decrypt/verify; `TestVolumePGPWrongKeyFails` relies on
that.

**Step 2: Run**

Run: `make test-run PKG=./files/ RUN=TestVolumePGP`
Expected: all subtests PASS. If `sign` mode fails with
`did not have ths key signature`, the public ring is not being consulted;
check `loadKeys` order. If the tamper test passes trivially with an
`unexpected EOF`, that is acceptable (the MDC check surfaces at EOF).

**Step 3: Commit**

```bash
git add files/volumeinfo_test.go
git commit -m "files: round-trip tests for pgp encrypt/sign with ed25519 and rsa keys"
```

---

### Task 6: GnuPG interoperability

Proves the bytes we write are standard OpenPGP that `gpg` decrypts and
verifies, and that what `gpg` encrypts we can read. Skips when `gpg` is not
on PATH; runs on this host (gpg 2.5.20) and in `make test-docker` (2.2.40).

**Files:**
- Create: `files/volumeinfo_gpg_test.go`

**Step 1: Write the test**

```go
package files

import (
	"bytes"
	"context"
	"io/ioutil"
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
	home := t.TempDir()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("gpg", append([]string{"--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		if err != nil {
			err = &gpgError{err, errb.String()}
		}
		return out.Bytes(), err
	}
	if _, err := run("--import", pgpFixtures+"ed25519-secret.asc"); err != nil {
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

	out, err := run("--trust-model", "always", "--status-fd", "2", "--decrypt", path)
	if err != nil {
		t.Fatalf("gpg --decrypt: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("gpg decrypted %d bytes, want %d", len(out), len(payload))
	}
	// A bad signature makes gpg exit non-zero, so reaching here means the
	// signature verified; the stderr status line confirms it explicitly.
	if _, err = run("--trust-model", "always", "--verify", path); err != nil {
		t.Fatalf("gpg --verify: %v", err)
	}
}

func TestWeDecryptGPGEncryptedVolume(t *testing.T) {
	run := gpgHome(t)
	config.BackupTempdir = t.TempDir()
	pub, sec := loadKeys(t, "ed25519", "ed25519@example.com")
	payload := bytes.Repeat([]byte("from gpg "), 1024)
	plain := filepath.Join(t.TempDir(), "plain")
	if err := ioutil.WriteFile(plain, payload, 0600); err != nil {
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
```

**Step 2: Run on the host and in Docker**

Run: `make test-run PKG=./files/ RUN='TestGPG|TestWeDecrypt'`
Expected: PASS with host gpg 2.5.20.

Run: `make test-docker`
Expected: same tests PASS under gpg 2.2.40 (the image inherits
`buildpack-deps`, which ships gnupg; verified with `docker run --rm
golang:1.25-bookworm which gpg`).

**Step 3: Commit**

```bash
git add files/volumeinfo_gpg_test.go
git commit -m "files: gnupg interoperability tests for pgp volumes"
```

---

### Task 7: End-to-end `send` with an Ed25519 keyring

Runs the real CLI path (`cmd/root.go` keyring loading, `loadSendKeys`, the
pipeline, `file://` backend) with the fake zfs. `zfs receive` is not faked,
so the check decrypts the manifest and volume in-process.

**Files:**
- Modify: `e2e_test.go` (append)

**Step 1: Write the test**

```go
// TestE2EEncryptedSend sends a manual full backup encrypted to and signed by
// an ed25519/cv25519 key, then decrypts what landed at the destination.
func TestE2EEncryptedSend(t *testing.T) {
	env := newE2EEnv(t)
	env.writeSnapshots(t, "tank/data", []files.SnapshotInfo{
		{Name: "a", CreationTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	})
	const email = "ed25519@example.com"
	logs, err := env.send(
		"--publicKeyRingPath", "testdata/pgp/ed25519-public.asc",
		"--secretKeyRingPath", "testdata/pgp/ed25519-secret.asc",
		"--encryptTo", email, "--signFrom", email,
		"tank/data@a", "file://"+env.dest)
	if err != nil {
		t.Fatalf("send: %v\n%s", err, logs)
	}

	// Every object must carry the pgp extension.
	var manifests, volumes []string
	err = filepath.Walk(env.dest, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.Contains(filepath.Base(path), ".pgp") {
			t.Errorf("unencrypted object at destination: %s", path)
		}
		if strings.HasPrefix(strings.TrimPrefix(path, env.dest+"/"), "manifests") {
			manifests = append(manifests, path)
		} else {
			volumes = append(volumes, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || len(volumes) == 0 {
		t.Fatalf("destination has %d manifests and %d volumes", len(manifests), len(volumes))
	}

	// The send loaded the rings into the pgp package; reuse them to read back.
	keys := &files.JobInfo{EncryptKey: pgp.GetPrivateKeyByEmail(email), SignKey: pgp.GetPublicKeyByEmail(email)}
	mv, err := files.ExtractLocal(context.Background(), keys, manifests[0], true)
	if err != nil {
		t.Fatalf("decrypt manifest: %v", err)
	}
	var manifest files.JobInfo
	if err = json.NewDecoder(mv).Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	mv.Close()
	if manifest.VolumeName != "tank/data" || manifest.BaseSnapshot.Name != "a" {
		t.Errorf("manifest = %s@%s", manifest.VolumeName, manifest.BaseSnapshot.Name)
	}

	manifest.EncryptKey, manifest.SignKey = keys.EncryptKey, keys.SignKey
	var total int64
	for _, path := range volumes {
		vv, err := files.ExtractLocal(context.Background(), &manifest, path, false)
		if err != nil {
			t.Fatalf("decrypt %s: %v", path, err)
		}
		n, err := io.Copy(ioutil.Discard, vv)
		vv.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		total += n
	}
	if total != 65536 { // fakezfs default FAKEZFS_STREAM_BYTES
		t.Errorf("decrypted %d stream bytes, want 65536", total)
	}
}
```

Add the imports `encoding/json`, `io`, `path/filepath`, `time` and
`github.com/someone1/zfsbackup-go/pgp` as needed. The `manifests` prefix is
the default `--manifestPrefix`; if the walk finds the manifest elsewhere,
print `find env.dest` once and adjust the prefix check.

**Step 2: Run**

Run: `make test-run PKG=. RUN=TestE2EEncryptedSend`
Expected: PASS; the log contains `Loaded public key ring` and
`Loaded private key ring`.

Then the whole root package, because in-process cobra runs leak state:

Run: `make e2e && make test-run PKG=.`
Expected: PASS (in particular `TestVersion` still passes after the new
test).

**Step 3: Commit**

```bash
git add e2e_test.go
git commit -m "e2e: encrypted and signed send with an ed25519 keyring"
```

---

### Task 8: Documentation

**Files:**
- Modify: `README.md:51-53` (Encryption/Signing), `README.md:167-173` (key export notes)

**Step 1: Edit**

Replace the Encryption/Signing paragraph with:

```markdown
### Encryption/Signing

Volumes and manifests are encrypted and/or signed with OpenPGP
(`github.com/ProtonMail/go-crypto`). The cipher is AES-256 and signatures
use SHA-256. Supported keys: RSA, ECDSA/ECDH on the NIST curves, and
Ed25519 signing keys with Curve25519 (`cv25519`) encryption subkeys, which
is what `gpg --full-generate-key` produces by default since GnuPG 2.3. The
RFC 9580 "v6" Ed25519/X25519 keys that newer tools emit are accepted too.
DSA/ElGamal keys still load but are not recommended.
```

Under the `gpg2 --export` example (line ~169) add:

```markdown
To create a modern key pair for backups:

    gpg --quick-generate-key 'backup@example.com' ed25519 sign never
    gpg --quick-add-key <fingerprint> cv25519 encr never
    gpg --output public.pgp --armor --export backup@example.com
    gpg --output private.pgp --armor --export-secret-key backup@example.com
```

Also update the `docs/runbook-first-backup.md` line about PGP flags only if
it names key types (it does not today; leave it).

**Step 2: Commit**

```bash
git add README.md
git commit -m "docs: ed25519/cv25519 keys are supported for encryption and signing"
```

---

### Task 9 (optional, small): replace deprecated `ssh/terminal`

`cmd/root.go:39` imports `golang.org/x/crypto/ssh/terminal` for
`ReadPassword`. It is a deprecated shim over `golang.org/x/term`, already an
indirect dependency. Doing this drops the last use of a deprecated package
and lets `go mod vendor` remove `vendor/golang.org/x/crypto/ssh/terminal`.

**Files:**
- Modify: `cmd/root.go:39`, `cmd/root.go` `validatePassphrase`
- Modify: `go.mod` (x/term becomes direct), `vendor/`

**Step 1:** change the import to `"golang.org/x/term"` and the call to
`term.ReadPassword(int(os.Stdin.Fd()))`.

**Step 2:** `make vendor` (no `GOGET`); expect `golang.org/x/term` to move
to the direct `require` block and `vendor/golang.org/x/crypto/ssh/terminal`
to disappear.

**Step 3:** `make test-docker` green. Manual check: run
`go run . send --secretKeyRingPath testdata/pgp/ed25519-pass-secret.asc --signFrom ed25519-pass@example.com tank/data@a file:///tmp/x`
with `PGP_PASSPHRASE` unset and confirm the prompt still reads a hidden
passphrase (it will then fail on zfs, which is fine).

**Step 4:** commit `cmd/root.go go.mod go.sum vendor` as
`cmd: read the passphrase with x/term instead of the deprecated ssh/terminal`.

---

### Task 10: Wrap up

1. `make fmt && make test-docker && make e2e` all green; paste the tail of
   the output in the final report.
2. Manual verification with a real keyring exported from the host's GnuPG
   2.5.20 (the fixtures came from 2.2.40):
   ```bash
   export GNUPGHOME=$(mktemp -d)
   gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'real@example.com' default default never
   gpg --armor --export real@example.com > /tmp/pub.asc
   gpg --batch --pinentry-mode loopback --passphrase '' --armor --export-secret-keys real@example.com > /tmp/sec.asc
   go run . send --logLevel debug --publicKeyRingPath /tmp/pub.asc --secretKeyRingPath /tmp/sec.asc \
       --encryptTo real@example.com --signFrom real@example.com -n tank/data@x file:///tmp/nowhere
   ```
   Expected: the debug output lists both keys with `(algo 22)`, and the run
   fails only at the `zfs` step, not at keyring loading. (`default` in
   GnuPG 2.5 is ed25519/cv25519; if the host emits v6 keys that also must
   load.)
3. Append to `.claude/napkin.md` Domain Notes: PGP now uses
   ProtonMail/go-crypto v1 API; `make vendor GOGET=...` is how module
   changes are made; fixtures under `testdata/pgp` regenerate with
   `make pgp-fixtures`; the `files` package tests drive the pipeline
   through `CreateBackupVolume` + `ExtractLocal` with
   `config.BackupTempdir = t.TempDir()`.
4. Record deviations from this plan under an "Implementation notes"
   heading at the end of this file (repo convention, see the plan-harness
   spec).

## Out of scope

- `openpgp/v2` API, AEAD/SEIPD v2 output, or a `--cipher` flag (README TODO
  "Make PGP cipher configurable"): none are needed to support Ed25519 keys,
  and changing the output format would break older readers.
- Generating keys inside `zfsbackup`.
- Replacing the email-based key lookup with fingerprints.
