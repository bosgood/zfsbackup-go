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
