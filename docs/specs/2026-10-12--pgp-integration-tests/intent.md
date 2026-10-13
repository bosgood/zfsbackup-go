# Intent: integration tests for OpenPGP keys (Ed25519 and RSA)

Branch `pgp-ed25519` (replayed onto master 2026-10-12) moved the OpenPGP code
to `github.com/ProtonMail/go-crypto` so that `--encryptTo` and `--signFrom`
accept Ed25519/cv25519 keyrings. It added unit tests (`pgp/`, `files/`), a
GnuPG interop test (`files/volumeinfo_gpg_test.go`, skipped without `gpg`)
and one e2e test against `file://` with the fake zfs.

Nothing exercises an Ed25519 key with real `zfs send`/`zfs receive` and an
S3 destination. The three integration tests that use GPG today
(`TestIntegrationS3EncryptedChain`, `TestIntegrationS3EncryptedResume`,
`TestIntegrationS3NativeEncryption` N8) all generate an RSA key in Go
(`newKey`) and always encrypt AND sign. Keys exported by GnuPG (the shape a
user has), a passphrase-protected secret key, encrypt-only, sign-only, a
signing-key rotation across algorithms, and `gpg` reading what we uploaded
are untested on that path.

We want `make integration-vm` to prove, on real zfs against MinIO:

1. A keyring GnuPG exported (`testdata/pgp/*.asc`: `ed25519`,
   `ed25519-pass`, `rsa`) encrypts, signs, lists and restores a backup.
2. The passphrase path (`PGP_PASSPHRASE`) works and a wrong passphrase
   fails before anything is uploaded.
3. Encrypt-only and sign-only backups restore; a tampered signed volume
   does not.
4. A signing-key rotation from RSA to Ed25519 with `--trustSigner` keeps
   smart sends and `receive --auto` working across the rotation.
5. Stock `gpg` decrypts and verifies the uploaded objects, and the
   decrypted volumes feed `zfs receive` directly.

The RSA coverage that existed before (Go-generated keys) stays as it is.
The new tests take RSA through the same table as Ed25519, so both key kinds
get the coverage that only the fake-zfs e2e tests had.
