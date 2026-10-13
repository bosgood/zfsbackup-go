# PGP integration tests: what remains

Status on 2026-10-13. The plan's tasks 0 to 8 are done (see the notes at the
end of `plan.md`). An adversarial review of the uncommitted work found four
warnings; all four are fixed (below). These items are still open.

## Open

1. **Run the whole suite.** `make integration-vm` (all eighteen
   `TestIntegrationS3*`), then `make lint` and `make vet`. Only
   `RUN=TestIntegrationS3PGP` and `PKG=./files/` ran after the review fixes.
   This is the last unchecked line of the plan's checklist.
2. **Break-the-code proofs for the review fixes.** Each new check passed the
   first time. In a scratch copy under `~/.cache/` (the VM sees only `~`):
   - Rotation: make `signatureError` return another message for the untrusted
     signer. `TestIntegrationS3PGPRotation` must go red at the last receive.
   - Interop: put the volumes of the gpg-decrypted manifest out of order (drop
     the `SortFunc` and reverse them). `TestIntegrationS3PGPInterop` must go
     red at `zfs receive`.
   - Volume bound: lower `most` in `files.TestEncryptedVolumeStreamBytes` by
     one byte. The encrypted subtests must go red (they sit at exactly 1.5 MiB).
3. **Mark the spec EXECUTED** in the napkin once items 1 and 2 are done.

## Review notes not fixed (author's choice)

- `TestIntegrationS3PGPRotation` takes the key flags off `c.smart` with
  `c.smart[:len(c.smart)-len(oldKeys)]`. That works only while `buildChain`
  puts `extra` last. Build the flags explicitly.
- `gpgRunner` (`integration_s3_pgp_test.go`) is a copy of `gpgHome`
  (`files/volumeinfo_gpg_test.go`). They are in different packages; the copies
  can drift.
- `wantPGPObjects` accepts an object when `.pgp` is anywhere in its name.
  Check the position of the extension.
- The wrong-passphrase test checks only that the bucket stays empty. It does
  not check that `zfs send` never started, or that the working directory has
  no cache.
- `resetRootFlags` reads `PGP_PASSPHRASE` again (`cmd/root.go`). Its comment
  ("init read it already") is hard to read. The passphrase stays in a global
  for the life of the process (true before this work too).
- `testdata/pgp/README.md`: say that the fixture keys (and the committed
  passphrase `zfsbackup-test`) are for tests only.

## Fixed in the review (for reference)

- The runbook's Ed25519 commands are now tested with gpg 2.4.4 and a
  passphrase: `TestIntegrationS3PGPInterop/runbook`. The runbook no longer
  says `generate.sh` uses the same commands (it uses `sign`, not `sign,cert`).
- `files.TestEncryptedVolumeStreamBytes` pins the stream bytes in one volume
  at `--volsize 1`: 1 MiB plain, 1.5 MiB encrypted (one copy step late).
  `TestIntegrationS3EncryptedResume` and
  `TestE2EResumeDoesNotTrustOtherDestinationsCache` depend on that bound.
- The receive without `--trustSigner` in `TestIntegrationS3PGPRotation` must
  fail with "signed by <old>, want <new>" and `--trustSigner`.
- `TestIntegrationS3PGPInterop` takes the volume list from the manifest gpg
  decrypted, not from zfsbackup, and wants exactly one manifest. Each subtest
  also restores with zfsbackup.
