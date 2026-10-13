# Plan: integration tests for OpenPGP keys (Ed25519 and RSA)

Intent: [intent.md](intent.md). Run everything through the Makefile. The
Lima VM (`make integration-vm`) is the way to run these on this Mac.

## What exists today

| Layer | Test | Keys | Modes | Notes |
|---|---|---|---|---|
| unit `pgp/` | `TestLoadEd25519*`, `TestDecryptPassphraseProtectedEd25519Key`, `TestLoadRSARingsStillWork` | fixtures | load, decrypt key | no volumes |
| unit `files/` | `TestVolumePGPRoundTrip` | fixtures ed25519, rsa | encrypt, sign, both | temp-file volumes |
| unit `files/` | `TestVolumePGPTamperDetected`, `TestVolumePGPWrongKeyFails`, `TestVolumePGPRoundTripV6Key` | ed25519, v6 | both | |
| unit `files/` | `TestGPGDecryptsAndVerifiesOurVolume`, `TestWeDecryptGPGEncryptedVolume` | ed25519 only | both | skipped without `gpg` |
| unit `files/pgp_test.go` | `TestExtractRequires*`, `TestVerifyEndRejectsOtherSigner`, ... | Go RSA (`newTestKey`) | both | pre-existing |
| e2e root | `TestE2EEncryptedSend` | fixture ed25519 | both | `file://`, fake zfs |
| e2e root | `TestE2ESign*`, `TestE2EListWithWrongKeyAdvises`, `TestE2EResume*Key*` | Go RSA (`newKey`) | both | fake zfs |
| integration | `TestIntegrationS3EncryptedChain`, `...EncryptedResume`, `...NativeEncryption` N8 | Go RSA (`newKey`) | both | real zfs + MinIO |

Gaps on the integration layer: GnuPG-exported rings, Ed25519 at all,
passphrase, encrypt-only, sign-only, cross-algorithm rotation, `gpg`
interop, encrypted pipe-mode receive.

## Design decisions

- **One new file** `integration_s3_pgp_test.go`, build tag `integration`,
  package `main`. It can use every helper in `integration_s3_test.go` and
  `e2e_receive_test.go` (`newKey`, `writeRings`) because they share the
  package.
- **Test names start with `TestIntegrationS3PGP`.** The default `RUN`
  (`TestIntegrationS3`) matches by prefix, so they join the suite with no
  Makefile change. `RUN=TestIntegrationS3PGP` runs only these.
- **Fixtures, not generated keys.** `testdata/pgp/*.asc` are what GnuPG
  exports (subkeys, preferences, armor as a user has them). The test binary
  runs from `/src` (`integration/entrypoint.sh`) and `make integration` runs
  from the repo root, so the relative path `testdata/pgp/` works as it does
  in `TestE2EEncryptedSend`.
- **Fixture emails**: `ed25519@example.com`, `ed25519-pass@example.com`
  (passphrase `zfsbackup-test`), `rsa@example.com`.
- **Passphrase**: `passphrase` is read from `PGP_PASSPHRASE` once, in
  `cmd/root.go` `init()`. Move that one line into `resetRootFlags()`.
  `init()` calls `resetRootFlags()` already, so production is unchanged, and
  `t.Setenv` + `cmd.ResetSendJobInfo()` makes an in-process run see it.
- **`gpg` in the integration image**: add `gnupg` to the apt line of the
  `integration` stage in `Dockerfile`. The interop test skips when
  `exec.LookPath("gpg")` fails, so a bare `make integration` host still runs.
- **Volumes**: `--volsize 1 --compressor ""` and a 3 MiB file, like the other
  tests: three volumes, and with no compressor the decrypted volumes are
  the raw stream (task 6 pipes them into `zfs receive`).
- **Lint**: `funlen` is 100 lines / 80 statements, `dupl` 100, `lll` 140.
  Table-drive the key kinds and keep each subtest in a helper.
- **Flag leaks**: one `cmd.Reset*JobInfo` per run (the `it*` helpers do it).
  Pass `--maxFileBuffer` explicitly where a test depends on it.

## Tasks

### Task 0: baseline on the merged branch

1. `make integration-image` (compiles and vets the tagged tests).
2. `make integration-vm RUN='TestIntegrationS3EncryptedChain|TestIntegrationS3EncryptedResume'`.
   First run of the Go RSA tests on go-crypto with real zfs. Must be green
   before any new test is written.

### Task 1: helpers (`integration_s3_pgp_test.go`)

```go
// fixtureRings returns the key flags for one testdata/pgp keyring pair.
func fixtureRings(prefix, email string) []string {
	return []string{
		"--publicKeyRingPath", "testdata/pgp/" + prefix + "-public.asc",
		"--secretKeyRingPath", "testdata/pgp/" + prefix + "-secret.asc",
		"--encryptTo", email, "--signFrom", email,
	}
}

// fixtureEntities loads one fixture pair and returns its entities (both rings
// hold one key). Used to build combined rings and to read fingerprints.
func fixtureEntities(t *testing.T, prefix string) (pub, sec *openpgp.Entity)

// combinedRings writes one public and one secret ring that hold every
// fixture in prefixes, and returns the ring flags plus --encryptTo/--signFrom
// for email. writeRings hard-codes backup@example.com, so this is the
// variant that takes the email.
func combinedRings(t *testing.T, email string, prefixes ...string) []string

// wantSignedVolume fails unless data starts with a one-pass signature: the
// first packet openpgp.Sign writes. The counterpart of wantPGPVolume.
func wantSignedVolume(t *testing.T, name string, data []byte)

// fingerprint of an entity's primary key as the manifest records it (%X).
```

`combinedRings` reuses the serialization loop of `writeRings`: refactor
`writeRings` into `writeRingsFor(t, email, keys...)` and keep `writeRings`
as the `backup@example.com` wrapper, so the e2e tests do not change.

### Task 2: `TestIntegrationS3PGPKeys` — every fixture through the whole path

Table: `{prefix, email, passphrase}` = `ed25519`, `ed25519-pass` +
`zfsbackup-test`, `rsa`. For each, a subtest named by prefix (no commas):

1. `t.Setenv("PGP_PASSPHRASE", passphrase)` (empty for the others).
2. Dataset `<pool>/srcpgp<prefix>`, 3 MiB file, one snapshot, one full send
   `--volsize 1 --compressor ""` with `fixtureRings`.
3. Objects: every name contains `.pgp`; `wantPGPVolume` on volume 1 and on
   the manifest (`objectNames`, `objectBytes`, `manifestFor`).
4. The manifest's `EncryptKeyFingerprint` and `SignKeyFingerprint` equal the
   fixture's primary-key fingerprint (`fixtureEntities`).
5. `itList` with the rings shows the one backup.
6. `itReceive` with `t.TempDir()` as work (an empty cache: the manifest comes
   from S3) into `<pool>/restoredPGP<prefix>`; `wantSnapshots`, `wantGUIDs`,
   `compareDirs`.
7. Wrong key: receive with the rings of the next fixture in the table fails,
   `wantNoDataset`, and the log advises `--secretKeyRingPath` and never
   "delete that object" (same assertion as `TestIntegrationS3EncryptedChain`).
8. Passphrase subtest only: with `PGP_PASSPHRASE=wrong` a send fails with
   "Error decrypting private key" and the bucket prefix holds 0 objects
   (`countObjects`). Needs the `resetRootFlags` move from the design notes.

Known, out of scope: an empty passphrase with stdin not a TTY panics in
`validatePassphrase` (pre-existing). Note it in the implementation notes;
do not test it here.

### Task 3: `TestIntegrationS3PGPModes` — encrypt-only, sign-only, pipe

Ed25519 fixture, one dataset, one snapshot; three sends to three prefixes
(`itBucket` once, three sub-prefixes):

- **encrypt-only** (`--encryptTo` only): objects `.pgp`, `wantPGPVolume`;
  receive with the secret ring and no `--signFrom` restores; `wantGUIDs`.
- **sign-only** (`--signFrom` only): `wantSignedVolume` on volume 1 and the
  manifest. Check the object names against what `files` does for a signed,
  unencrypted volume (`cs --only-usages` of the `.pgp` suffix constant) and
  assert that. Receive with the public ring restores. Then flip one byte in
  the middle of volume 2 at S3 (`objectBytes`, `putObject`): receive fails,
  `wantNoDataset` (the stream dies before zfs sees its END record, see the
  napkin), and the log names the signature, not a key-ring problem.
- **pipe**: receive the encrypt-only set with `--maxFileBuffer 0` and the
  secret ring (signing is refused in pipe mode by
  `TestReceiveRefusesStreamingWithSignFrom`; encryption is allowed and is
  untested on real zfs). `wantGUIDs`.

### Task 4: `TestIntegrationS3PGPRotation` — RSA to Ed25519 with `--trustSigner`

The cross-algorithm counterpart of `TestE2ESignKeyRotationWithTrustSigner`
on real zfs:

1. `buildChain(t, pool, work, target, "rot", fixtureRings("rsa", rsa)...)`:
   three links signed and encrypted with the RSA fixture.
2. `rings := combinedRings(t, ed25519, "rsa", "ed25519")`: both keys in
   both rings, `--encryptTo`/`--signFrom` the Ed25519 key.
3. New file, new `_daily` snapshot. Smart send with `rings` and no
   `--trustSigner` fails: the log says "signed by <RSA fpr>, want <Ed25519
   fpr>" and advises `--trustSigner`; the listing is unchanged (3 links).
4. The same send with `--trustSigner <RSA fingerprint>` succeeds: 4 links,
   the new manifest's `SignKeyFingerprint` is the Ed25519 key.
5. `receive --auto` into an empty work dir with `rings` and
   `--trustSigner <RSA fpr>` restores all four snapshots; `wantGUIDs`.
6. The same receive without `--trustSigner` fails; `wantNoDataset`.

### Task 5: `TestIntegrationS3PGPInterop` — `gpg` reads what we uploaded

1. `Dockerfile`, `integration` stage: `gnupg` in the apt install line.
2. Skip unless `exec.LookPath("gpg")` succeeds. Copy the `gpgHome` runner
   from `files/volumeinfo_gpg_test.go` (tests cannot import another
   package's test helpers; the napkin says copy). Parameter: the fixture
   prefix to import. Keep `os.MkdirTemp("", "gpg")` and `gpgconf --kill
   gpg-agent` in the cleanup.
3. Subtests `ed25519` and `rsa`: one full send with `fixtureRings`,
   `--volsize 1 --compressor ""`.
4. For the manifest and every volume: `objectBytes`, write to a temp file,
   `gpg --trust-model always --status-file <f> --decrypt --output <out>`.
   The status file has `DECRYPTION_OKAY`, `GOODSIG` and `VALIDSIG`
   (`--verify` refuses encrypted input, see the napkin).
5. The decrypted manifest is JSON whose `VolumeName` and `BaseSnapshot.Name`
   are the dataset and snapshot.
6. The decrypted volumes, in order, concatenated into `zfs receive -F
   <pool>/restoredGPG<prefix>`: `wantSnapshots`, `wantGUIDs`. With no
   compressor the volumes are the stream itself; this proves the whole
   object is plain OpenPGP that stock tools read.

### Task 6: RSA in the unit interop test

`files/volumeinfo_gpg_test.go`: give `gpgHome` a prefix parameter and run
both tests over `ed25519` and `rsa`. Small, and it closes the only unit gap
where RSA is not beside Ed25519. `make test-one PKG=./files RUN=GPG` (the Go
image has gpg 2.2).

### Task 7: break-the-code proofs

Scratch copy under `~/.cache/` (the VM sees only `~`), then
`make -C <copy> integration-vm RUN=...`. Two proofs:

1. Comment out the `bytes.Equal` fingerprint match in
   `files/volumeinfo.go` (`verifySigner`): Task 4 step 3 must turn red.
2. Drop `t.Setenv` in Task 2's passphrase subtest: it must fail with
   "Error decrypting private key".

Delete the copy after.

### Task 8: docs and notes

- `docs/context/testing.md`: the counts ("fourteen" in the targets table and
  in "Test layers") and the GPG sentence: fixtures, passphrase, encrypt-only,
  sign-only, rotation, `gpg` interop.
- `integration/README.md`: `gnupg` is in the image; `RUN=TestIntegrationS3PGP`.
- `docs/runbook-first-backup.md` lines 56-57 still say the key must be RSA
  because this version reads neither ed25519 nor cv25519. That is false
  since the merge. Say both work and keep the RSA example or switch to the
  README's `ed25519` + `cv25519` example; keep the passphrase steps.
- `.claude/napkin.md`: mark this plan executed; add what bit.
- Implementation notes at the end of this file.

## Run

```
make integration-image                        # compile + vet the tagged tests
make integration-vm RUN=TestIntegrationS3PGP  # the four new tests, a few minutes
make integration-vm                           # the whole suite before "done"
make lint && make vet                         # lll, funlen, dupl, shadow
```

## Out of scope

- Generating keys in `zfsbackup`; v6 keys on real zfs (unit covers v6).
- `receive` of a volume `gpg` encrypted (unit covers it; a stream `gpg` made
  from a file is not a zfs stream).
- The `validatePassphrase` panic without a TTY: a separate fix.
- Azure/Blob (`TestIntegration`) and real AWS runs.

## Checklist

- [x] Task 0 green on the merged branch (after the `EncryptedResume` fix, notes)
- [x] Task 1 helpers; `writeRingsFor` refactor; e2e tests unchanged and green
- [x] Task 2 `TestIntegrationS3PGPKeys` (+ `resetRootFlags` passphrase read)
- [x] Task 3 `TestIntegrationS3PGPModes`
- [x] Task 4 `TestIntegrationS3PGPRotation`
- [x] Task 5 `TestIntegrationS3PGPInterop` (+ `gnupg` in the image)
- [x] Task 6 RSA in the unit interop test
- [x] Task 7 two proofs, scratch copy deleted
- [x] Task 8 docs, napkin, implementation notes
- [ ] `make integration-vm` whole suite green; `make lint`, `make vet`

## Implementation notes (2026-10-12)

- **Task 0 was red.** `TestIntegrationS3EncryptedResume` failed on the merged
  branch, every run: the cut send left 1 volume, not 2. go-crypto's
  `partialLengthWriter` (vendor `openpgp/packet/packet.go`) holds data back
  and writes at most one chunk per `Write`; x/crypto wrote through. The
  volume counter sits under the PGP layer, so after 1 MiB of stream it sees
  less than 974 KiB, and the check after each 512 KiB copy step ends the
  volume one step late: an encrypted volume at `--volsize 1` holds 1.5 MiB of
  stream. Not a production defect (the overshoot is at most one copy step,
  as before). Fix in the test: cut at 3.5 MiB and accept 2 or 3 complete
  volumes. Same root cause, flaky on the fake zfs:
  `TestE2EResumeDoesNotTrustOtherDestinationsCache/partial_manifest` needs a
  volume 3 of a 3 MiB stream (red 3 of 3 at HEAD in Docker); its stream is
  now 5 MiB.
- **`init()` does not call `resetRootFlags()`** (the plan said it did). The
  `PGP_PASSPHRASE` read stays in `init()` and is also in `resetRootFlags()`.
- **Tampered signed volume**: it fails the SHA-256 check against the signed
  manifest before zfs reads it ("<vol> does not match its manifest"); the
  volume's own signature is never reached. The test asserts that message.
- **Manifests are always gzipped** (`InternalCompressor`), also with
  `--compressor ""`: the interop test gunzips what gpg decrypts.
- **Empty bucket cleanup**: `setupS3Bucket`'s cleanup sent a `DeleteObjects`
  with no keys, which MinIO rejects (MalformedXML). The wrong-passphrase
  bucket is empty on purpose; the cleanup now skips an empty delete.
- `writeRingsFor` writes with `SerializePrivateWithoutSigning`: a fixture's
  GnuPG self-signatures stay as exported. newKey's keys are signed in newKey.
- `fixtureRings` takes the prefix only (the email is `<prefix>@example.com`);
  `fixtureEntities` became `fixtureKey` (the secret ring's entity serves as
  both public and secret key).
- **Proofs** (scratch copy under `~/.cache`, deleted): `|| true` on the
  fingerprint match in `signatureError` turns `PGPRotation` red at step 3.
  Without `t.Setenv` the `ed25519-pass` subtest is red through the known
  `validatePassphrase` panic ("inappropriate ioctl for device", no TTY), not
  through "Error decrypting private key"; the panic kills the test binary.
- **Restore proofs** (asked for after the plan; scratch copies deleted).
  (A) Flip byte 100000 of each decrypted volume, after the signature check:
  zfs rejects the stream ("checksum mismatch"); `PGPKeys` (all 3 subtests),
  `PGPModes` and `PGPRotation` turn red. (B) The same, and `backup.Receive`
  returns nil after a failed restore: `wantSnapshots` fails on the missing
  dataset in `PGPKeys` and `PGPModes`; `PGPRotation` fails when `--auto`
  goes on to the next link. `PGPInterop` stays green in both (gpg decrypts,
  not our code). A first try of (B) returned nil inside `receiveStream`: the
  download goroutine then blocks and the run hangs to the 45 min timeout.
