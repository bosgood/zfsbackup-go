OpenPGP keyring fixtures for the `pgp`, `files` and e2e tests: `ed25519-*` (Ed25519 + cv25519 subkey), `ed25519-pass-*` (same, secret key protected with the passphrase `zfsbackup-test`) and `rsa-*` (RSA 2048).
Regenerate with `make pgp-fixtures` (runs `generate.sh` with the GnuPG in the Go image).
These are public test keys: never use them for anything real.
