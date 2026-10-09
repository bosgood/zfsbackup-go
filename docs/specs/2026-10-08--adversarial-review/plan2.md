# Adversarial review, second pass (2026-10-08) Fix Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix what the second adversarial review of `clean-dry-run` found, run at
`5892fea` after every fix in [plan.md](plan.md) had landed. Two of the three CRITICALs are
holes in that plan's fixes: `clean` still races a finishing send, and `--resume` still
trusts stale checksums. The third lets the devcontainer's sandboxed user switch the
egress firewall off. Fifteen WARNINGs follow. Several of them only matter because the
runbook's production flags neither encrypt nor sign (W7), which makes every
malicious-destination finding reachable.

**Tech Stack:** Go 1.25, vendored deps, in-process e2e tests (`e2e_*_test.go`, fake zfs in
`internal/fakezfs`), the fake S3 in `clean_s3_test.go`, golden scenarios in
`backup/testdata/scenarios`.

Line numbers are as of `5892fea` on `clean-dry-run`; re-check them before editing.

**Repro tests:** the reviewers' tests are saved in [`repro2/`](repro2/) as `.go.txt` (so
they do not compile into the module). The file-name prefix gives the package:
`root--` for the root package, `backup--`, `backends--`, `files--`. To run one against an
unfixed tree:
`mkdir /tmp/x && git archive 5892fea | tar -x -C /tmp/x`, copy the file in without
`<pkg>--` and `.txt`, then
`docker run --rm -v /tmp/x:/src -v /tmp/zfsb-gocache:/root/.cache/go-build -w /src golang:1.25-bookworm go test -count=1 -run <Name> -v ./<pkg>/`.
`root--adv_srv_test.go.txt` is a helper (fake S3 with an interceptor) that
`root--adv_clean_test.go.txt` and `root--adv_prefix_test.go.txt` need.

---

## Ground rules

Same as [plan.md](plan.md#ground-rules): one commit per task in the existing message
style; TDD with each ported test seen failing on `5892fea` first; `make test-run` for the
fast loop and the whole package after any in-process cobra test; `make scenarios` and a
justified `make scenarios-update` when planner output changes on purpose; never gate a
commit on a piped test run; stage files by name; `make fmt-check` and `make test-docker`
before calling it done.

Port the repro tests under the repo's naming (`TestE2E...` in the root package,
`TestClean...`/`TestPlan...` in `backup`, `TestRedactURI` cases in `backends`), drop the
`t.Logf`-only probes, keep the assertions.

## Findings

Severity: **CRITICAL** means a wrong or unrestorable backup while reporting success, data
deleted, or a security boundary bypassed. **WARNING** wedges runs, leaks secrets, accepts
hostile input in a narrower way, or makes `plan` disagree with `send`. **NOTE** is
cosmetic, narrow, or documentation.

| #   | Sev      | Repro (in `repro2/`)                                                                 | Summary                                                                                                                                                                                                                                                                                                                                                                           | Task |
| --- | -------- | ------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---- |
| C1  | CRITICAL | `TestAdvCleanRacesFinishingSend` (root, both `--cleanLocal` values)                  | `Clean` runs `syncCache` (`backup/clean.go:103`) and `List("")` (`:111`) before `TryLock` (`:203`). A send that uploads its last volume and manifest and drops its lock in between has volumes in `allObjects` but in no decoded manifest: they are deleted as orphans. With `--cleanLocal` its cached manifest is removed too (`:235`).                                          | 1    |
| C2  | CRITICAL | `TestAdvRev2ResumeStartOverKeepsStaleCache`, `TestAdvRev2ResumeFromKKeepsStaleCache` | `discardPartialManifests` (`backup/backup.go:470`) runs only without `--resume`. A `--resume` run that starts over (`:1256`, `:1263`, `:1289`) or resumes from volume k rewrites volumes but leaves the earlier attempt's SHA-256s cached; if it fails before `saveManifest`, the next `--resume` publishes them. Exit 0; restore fails.                                          | 2    |
| C3  | CRITICAL | run in the devcontainer image as uid 1000 (see Task 3)                               | `.devcontainer/init-firewall.sh:102-119` sets every policy to ACCEPT and flushes before the GitHub fetch; only ERR is trapped (`:120`). `sudo init-firewall.sh & sleep 0.3; kill -TERM $!` leaves egress open. Every legitimate re-run is open for about a second.                                                                                                                | 3    |
| W1  | WARNING  | `TestAdvRev2AutoRestoreFullOverExistingChain` (passes at `4eee1c3`, fails at HEAD)   | Regression from `2a54b31`: `fullOrAny` (`backup/restore.go:194`) always takes the full. When the local dataset already has the incremental's parent, real zfs refuses a full stream into a dataset with snapshots.                                                                                                                                                                | 4    |
| W2  | WARNING  | `TestAdvRev2SignKeyRotationWedgesSmartSend`                                          | `VerifyEnd` accepts exactly `--signFrom`'s fingerprint (`files/volumeinfo.go:169`). After a key rotation every manifest signed by the old key is rejected (and re-downloaded every run by `readCachedManifest`): smart sends fail forever, a chain spanning the rotation cannot be restored. Same for first enabling `--signFrom` on a destination with unsigned manifests.       | 10   |
| W3  | WARNING  | `TestAdvRev2PromptFuncPanics` (files, both subtests)                                 | `pgp.PromptFunc` (`pgp/pgp.go:56`) panics. One symmetrically encrypted object under `manifests` (or an old manifest encrypted to a still-locked key after an `--encryptTo` rotation) crashes list, send, clean and receive; a send dies holding its lock.                                                                                                                         | 6    |
| W4  | WARNING  | `TestAdvSecManifestSubstitution` (root)                                              | `readCachedManifest` (`backup/sync.go:203`) never checks that the decoded manifest is the one its object name promises. A validly signed old manifest copied over the newest one's name makes `receive @b` restore `@a`, exit 0. `send` has this check (`sameStream`); `receive` does not.                                                                                        | 5    |
| W5  | WARNING  | `TestAdvSecPipeUnverifiedBytesReachZFS` (root)                                       | With `--maxFileBuffer 0` a volume is streamed into `zfs receive` (`backup/restore.go:424-426`) before its SHA-256 and signature are checked. Real zfs commits on the stream's END record, before the PGP end. The send-path reviewer could not confirm the commit against the fake zfs (`TestAdvRev2PipeReceiveFeedsSwappedVolume` passes); the bytes reaching zfs are confirmed. | 9    |
| W6  | WARNING  | `TestAdvSecManifestCompressorExecutes` (root; got uid 0)                             | Without `--signFrom`, `receive` runs the manifest's `Compressor` as a program (`files/volumeinfo.go:307`, `exec.CommandContext(ctx, compressor, "-c", "-d")`). Predates the branch.                                                                                                                                                                                               | 8    |
| W7  | WARNING  | read `docs/runbook-first-backup.md:13`                                               | The runbook's `FLAGS` has no `--encryptTo`/`--signFrom`, and no key-setup step exists. A copy-paste first backup uploads plaintext, unauthenticated streams, so the signature fixes never apply and W6/W8 are open to anyone who can write the bucket.                                                                                                                            | 16   |
| W8  | WARNING  | `TestAdvSecForgedManifestDecodedBeforeVerify` (files)                                | `readManifest` (`backup/list.go:223-231`) decodes the whole JSON before `VerifyEnd`. A 130 KB forged manifest, signed by the forger's own key, allocates 516 MiB before rejection.                                                                                                                                                                                                | 7    |
| W9  | WARNING  | `TestAdvRedactBypass`, `TestAdvSecRedactURI` (backends)                              | `RedactURI` (`backends/backends.go:291`) returns `ssh://u:/secret@h/x`, `u:1234/secret@`, `u:99?pw@`, `u:?secret@`, `u:#secret@` unchanged: `url.Parse` reads `u:` / `u:1234` as host:port, `User` is nil, and the "@ is in the path" shortcut fires.                                                                                                                             | 11   |
| W10 | WARNING  | `TestAdvForceNestedDeletesOwnManifest` (root)                                        | `nestedUnder` (`backup/clean.go:274`) drops the root destination's own volumes when a nested destination's directory equals the dataset's first component (`s3://bucket` backs up `tank/data`, `s3://bucket/tank/` is another destination). The set then looks incomplete; `--force` (`:318`) deletes its valid manifest.                                                         | 12   |
| W11 | WARNING  | `TestAdvManifestPrefixCollision` (root)                                              | `syncCache` lists `j.ManifestPrefix` with no separator (`backup/sync.go:132`): from `s3://bucket`, `List("manifests")` also returns `manifests-old/...`, every volume there is downloaded into the cache, decoding fails, and every run on the root fails. Fails safe; permanent denial of service and disk fill.                                                                 | 12   |
| W12 | WARNING  | `TestAdvrev2PlanFullResumeDisagreesWithSend` (root)                                  | `Scenario.run` (`backup/plan.go:365-373`) builds `jobInfo` without `Resume`; `planSmartSnapshots` reads it in the explicit-full branch (`:121`). `plan --full --resume` says "run again with --resume" and exits 2; `send --full --resume` completes the set.                                                                                                                     | 13   |
| W13 | WARNING  | `TestAdvrev2PlanResumeWithManifestsIgnored` (root)                                   | `cmd/plan.go:166` (`--manifests`) and the `default:` branch (`:196`) never set `sc.Completable`, so `plan --resume --manifests` reports "out of sync" where `send --resume` completes.                                                                                                                                                                                            | 13   |
| W14 | WARNING  | `TestAdvrev2PlanNamesOnlyZone` (root)                                                | Without `--schedule location=`, `location()` (`backup/plan_fixtures.go`) reads sanoid names as UTC. `AdoptCreationTimes` fixes only snapshots a manifest names; on a New York host the adopted monthly then sorts after newer hourlies. Live pool: INCR; names-only capture: NOOP. The Warning does not mention the zone.                                                         | 14   |
| W15 | WARNING  | `TestAdvrev2FutureManifestSilencesBackups` (root; logs the behaviour, passes)        | A manifest whose snapshot is dated after everything on the pool (forged, from another host, or a bad clock) makes every send print "Nothing new to back up" and exit 0 (`backup/plan.go:220`, `:249`; `ageExceeded` goes negative); `plan` says `checks: OK`.                                                                                                                     | 15   |
| N1  | NOTE     | code (`cmd/clean.go:52`, `backup/clean.go:224-238`)                                  | `--cleanLocal` help says it deletes local files; it also deletes the remote volumes of those manifests. The command's Short/Long text no longer describes what `clean` deletes.                                                                                                                                                                                                   | 17   |
| N2  | NOTE     | code (`backup/backup.go:440`)                                                        | A send blocked by `clean`'s lock (even a `--dry-run` clean, which also locks) says "Another send of %s is running".                                                                                                                                                                                                                                                               | 17   |
| N3  | NOTE     | code (`backup/clean.go:235` vs `:381`)                                               | `--cleanLocal` removes local manifests before the remote deletes. A failed or killed delete phase leaves their volumes unreachable to later cleans (storage leak, no data loss).                                                                                                                                                                                                  | 1    |
| N4  | NOTE     | code (`backends/aws_s3_backend.go:374`, `needsRestore`)                              | Intelligent-Tiering `ARCHIVE_ACCESS`/`DEEP_ARCHIVE_ACCESS` objects report `INTELLIGENT_TIERING`; no restore is issued and the GET fails `InvalidObjectState`. Check `ArchiveStatus`.                                                                                                                                                                                              | 17   |
| N5  | NOTE     | code (`backends/aws_s3_backend.go`, RestoreObject call)                              | A RestoreObject error that is not an `awserr.Error` is dropped; the key stays queued with a nil `x-amz-restore` and the wait loop polls until killed.                                                                                                                                                                                                                             | 17   |
| N6  | NOTE     | `TestAdvRev2ExternalDecompressorSignatureRace` with `-race`, `--compressor gzip`     | `SignatureError` is set in os/exec's stdin-copy goroutine and read in `VolumeInfo.Read` with no happens-before (`files/volumeinfo.go:143-158`, `:307`). A decompressor that exits before stdin EOF means the signature is never checked.                                                                                                                                          | 6    |
| N7  | NOTE     | `TestAdvRev2ResumeWithoutSecretRingNeverResumes`                                     | `--resume` with `--encryptTo` and no secret ring cannot decrypt the cache; `openpgp: incorrect key` is not a `KeyError`, so it "starts over" with a Warning every time (and feeds C2).                                                                                                                                                                                            | 2    |
| N8  | NOTE     | `TestAdvRev2UnknownSignerWithoutSignFrom` (files)                                    | Without `--signFrom`, a manifest signed by a key in neither ring fails with "not in --signFrom's ring" (`files/volumeinfo.go:165`), naming a flag the user did not pass; list and clean used to work on such hosts.                                                                                                                                                               | 10   |
| N9  | NOTE     | `TestAdvrev2SkipEmptySideSilentlyIgnored` (backup)                                   | `skip=..X` and `skip=X..` parse to an empty range with a nil error (`backup/plan_fixtures.go:525`, `:529`).                                                                                                                                                                                                                                                                       | 17   |
| N10 | NOTE     | `TestAdvrev2CoverageMissesMonthlyPrunedBetweenRuns` (backup)                         | `Scenario.Run` records only `Advance`'s pruned result (`backup/plan.go:326-330`); with a run interval longer than retention, monthlies taken and pruned between runs are never judged. The 46537b1 fix covered `skip=` outages only.                                                                                                                                              | 17   |
| N11 | NOTE     | `TestAdvrev2ManifestsFileNoAdoption` (root)                                          | `--manifests` with epochs next to a names-only capture never calls `AdoptCreationTimes` (`cmd/plan.go:166`): FULL `source-pruned` where send does INCR.                                                                                                                                                                                                                           | 13   |
| N12 | NOTE     | `TestAdvSecCleanForceManifestNameTraversal` (root)                                   | `clean --force` deletes a manifest name computed from the (possibly unsigned) manifest's content (`backup/clean.go:330`); `FileBackend.Delete` joins without containment (`backends/file_backend.go:111`). `VolumeName: a/../../victim` deletes another destination's manifest. Volume names are safe (only listed candidates are judged).                                        | 12   |
| N13 | NOTE     | read `docs/runbook-first-backup.md:28`, `:238`                                       | "Fulls on the 1st of March and September" holds only for a chain that started in March or September. "Tolerates about two months of missed runs" is `monthly=3`; the runbook's policy is `monthly=6`.                                                                                                                                                                             | 16   |
| N14 | NOTE     | in-container probe                                                                   | The firewall's self-check ("example.com blocked") passes with no firewall at all on this host; a failed check exits with the rules as they are; IPv6 goes unfiltered when ip6tables is missing; DNS to the resolvers still allows tunnelling and the header does not say so.                                                                                                      | 3    |
| N15 | NOTE     | read `.devcontainer/devcontainer.json:21`                                            | `"CLAUDE_CODE_VERSION": "latest"` overrides the Dockerfile's `2.1.289` pin for VS Code / `devcontainer up`.                                                                                                                                                                                                                                                                       | 3    |
| N16 | NOTE     | code (`cmd/list.go`)                                                                 | `list a,b` silently uses `Destinations[0]`, the sibling of `89e8fa0`'s fix for `clean`.                                                                                                                                                                                                                                                                                           | 17   |

Promotion: W9 was found by two reviewers but by the same persona (Security Auditor), so it
is not promoted. W4/W6/W8 depend on an unsigned or attacker-writable destination; with W7
fixed they need the bucket's write credentials _and_ (for W6) no `--signFrom`.

Still open from earlier plans and not repeated here: send-path Tasks 10-15
(`docs/specs/2026-10-05--send-path-review/plan.md`: volsize/compressor validation, signal
handling, `flock`, `clean` reclaiming partials, the `isOpened` race, docs), the
backends-bugs plan (`docs/specs/2026-10-02--backends-bugs/plan.md`: file/ssh atomic
uploads, path containment, ssh auth; N12's backend half belongs to its containment task),
the `dev.nix` hash pin.

## Decisions made in this plan (review these first)

1. **`clean` takes the send locks before it reads the destination.** It still needs the
   dataset set to know which locks to take, so: read once to learn the datasets, lock
   them, then re-run `syncCache` and `List("")` and build every decision from the second
   read. If the second read names a dataset the first did not, lock it too and read
   again (bounded: three rounds, then fail "the destination keeps changing; run clean
   again"). Rejected: one global lock shared by send and clean (serialises unrelated
   datasets' sends).
2. **The resume cache describes only what the current run keeps, at every destination,
   before the first volume is written.** Every start-over branch in `tryResume` calls
   `discardPartialManifests`; a partial resume rewrites the cache with `volumes[:keep]`
   and no `EndTime` at every destination. An unreadable cache under `--encryptTo` with no
   secret ring is a configuration error, not a reason to start over: `--resume` with
   `--encryptTo` requires `--secretKeyRingPath` (N7).
3. **The devcontainer firewall never passes through ACCEPT.** A re-run builds the new
   allowlist into a temporary ipset while the old rules stay enforced (GitHub is already
   allowed, so the fetch works), then `ipset swap`s it in and rebuilds the chain with the
   policies held at DROP. TERM/INT/HUP are trapped into `fail_closed` from the first
   change. The self-check probes a host that was reachable before enforcement (recorded
   first) and fails closed on a failed check; IPv6 up without working ip6tables is fatal.
4. **`receive --auto` picks the backup that applies to the local dataset.** Among backups
   of the target snapshot: an incremental whose parent chain reaches a snapshot the local
   dataset has wins; otherwise the full; otherwise the first (the existing walk computes
   the chain). Task 1 of plan.md's case (no local snapshots, old full retired) still gets
   the full.
5. **A manifest read for object name N must be the manifest named N.** After decoding,
   `readManifest`'s callers that know the object name require
   `decoded.ManifestObjectName() == objectName`; a mismatch is an error naming both. Done
   once in `readCachedManifest`, so receive, list, clean, send and resume all get it.
6. **`PromptFunc` never panics.** It returns an error, and `Extract` maps that error (and
   `pgperrors.ErrKeyIncorrect`) to a `files.KeyError`.
7. **Manifests are read through a 64 MiB limit** before decoding. Hitting the limit is an
   error. 64 MiB is about 400x the largest real manifest the e2e suite produces; record
   the measured largest in the commit.
8. **A manifest's `Compressor` is only run if it is a bare name from a fixed list**
   (`gzip`, `pigz`, `bzip2`, `pbzip2`, `lbzip2`, `xz`, `pxz`, `lzma`, `zstd`, `pzstd`,
   `lz4`, `lzop`) or equals `receive --compressor` when the user passed one. Anything else
   (a path, an unknown name) fails with "the manifest names compressor X; pass
   `--compressor X` to receive if you trust it". `send` is unchanged: the user chose the
   compressor there.
9. **`receive --maxFileBuffer 0` with `--signFrom` is refused** with a message explaining
   that streaming hands unverified bytes to `zfs receive`. Without `--signFrom` it stays
   allowed (there is nothing to verify against) and the flag's help says so. Buffering
   with verification remains the default (5).
10. **Signatures are accepted from a set of trusted signers.** New repeatable flag
    `--trustSigner <email|fingerprint>`; the set is `--signFrom`'s key plus every
    `--trustSigner`, all of which must be in the public ring. The resume cache keeps the
    strict single-key match (`sameStream` already compares `SignKeyFingerprint`). Without
    `--signFrom` and `--trustSigner`, a signature from a key in neither ring is reported
    as "signed by X, whose public key is in neither ring; add it or pass --trustSigner",
    and still rejected. Enabling `--signFrom` on a destination of unsigned manifests
    stays an error; the runbook says how to start a new chain instead. **This adds a
    flag — confirm before implementing.**
11. **A last backup newer than everything on the pool stops the send with exit 2**
    ("the destination's last backup of X is dated D, after every snapshot on the pool;
    is another host writing this prefix, or was the clock wrong?"), and `plan` reports
    the same as a failed check. Only when that snapshot is not on the pool; a backup of
    the newest pool snapshot stays NOOP.
12. **Names in a capture are dated in the host's local zone by default** (sanoid names
    are local time), overridable with `--schedule location=`. The golden scenarios pin
    `location=UTC` so they do not depend on the machine running them. The names-only
    Warning names the zone used.
13. **The runbook encrypts and signs.** `FLAGS` gains `--encryptTo`, `--signFrom` and both
    keyring paths, with a key-generation and key-backup step before the first send, and a
    note that `receive`, `clean`, `list` and `plan` need the same flags.

---

### Task 1: `clean` locks before it reads (C1, N3)

**Files:** `backup/clean.go:95-240`, `backup/clean.go:381` (delete phase),
`clean_s3_test.go`.

- Split the read (sync, list, decode, local-only scan) into a function returning the
  decoded manifests, local-only manifests, `allObjects` and the dataset set.
- Read; lock every dataset (busy ones as today); read again; if the dataset set grew, lock
  the new ones and read again; at most three rounds (Decision 1).
- N3: collect the local manifests to delete during the decision phase and remove them only
  after every remote delete has succeeded.

Test: port `TestAdvCleanRacesFinishingSend` → `TestCleanS3SparesSendFinishingDuringClean`
(needs the interceptor from `root--adv_srv_test.go.txt`; fold it into the existing fake S3
in `clean_s3_test.go` as an optional `onList` hook). Both `--cleanLocal` values; assert no
deletion and that the finished set restores. Add an N3 case: a backend whose `Delete`
fails, then assert the local manifest is still there.

Commit: `clean: take the send locks before reading the destination`.

### Task 2: `--resume` keeps only what it keeps (C2, N7)

**Files:** `backup/backup.go:470`, `:958-1000` (`discardPartialManifests`,
`saveManifest`), `:1240-1310` (`tryResume`), `cmd/root.go:280-320`, `e2e_resume_test.go`.

- Every start-over return in `tryResume` calls `discardPartialManifests(j, dests)`.
- On a partial resume, right after `j.Volumes = volumes[:keep]`, `saveManifest(ctx, j,
false)` at every destination before any volume is sent.
- N7: `--resume` + `--encryptTo` without `--secretKeyRingPath` fails flag validation.

Tests: port `TestAdvRev2ResumeStartOverKeepsStaleCache` and
`TestAdvRev2ResumeFromKKeepsStaleCache` → `TestE2EResumeStartOverDiscardsCache`,
`TestE2EResumeFromKRewritesCache` (assert the final `receive` from dest1 succeeds), and
`TestAdvRev2ResumeWithoutSecretRingNeverResumes` → a flag-validation test.

Commit: `send: --resume discards or rewrites the cache before sending anything`.

### Task 3: firewall never opens (C3, N14, N15)

**Files:** `.devcontainer/init-firewall.sh`, `.devcontainer/devcontainer.json:21`,
`Makefile` (`devcontainer-firewall-check`).

- Decision 3. Keep `fail_closed`; add `trap ... TERM INT HUP` before the first change.
- `devcontainer-firewall-check` gains the kill test: start a re-run as the `dev` user,
  `kill -TERM` it after 0.3s, assert `iptables -S` still has DROP policies and a blocked
  probe stays blocked. Also assert a re-run never shows ACCEPT policies (sample
  `iptables -S` in a loop while it runs).
- Self-check: record a reachable-before-enforcement host (`api.github.com` is allowed, so
  use a host outside the list that answered before, e.g. the first of `google.com`,
  `example.com` that returns) and require it to be blocked after. Failure → `fail_closed`.
- IPv6 up and ip6tables unusable → `fail_closed`. Header comment: DNS to the resolvers is
  allowed and can tunnel.
- N15: delete the `CLAUDE_CODE_VERSION` build arg from `devcontainer.json`.

Test: `make devcontainer-firewall-check` (fails on `5892fea` with the kill step).

Commit: `devcontainer: the firewall reloads without ever opening`.

### Task 4: `receive --auto` picks the backup that applies (W1)

**Files:** `backup/restore.go:100-200` (`fullOrAny` and its callers), `e2e_receive_test.go`.

- Decision 4. Replace `fullOrAny` with a chooser that takes the local dataset's snapshot
  names (already fetched for the walk) and prefers an incremental whose chain reaches one.

Test: port `TestAdvRev2AutoRestoreFullOverExistingChain` →
`TestE2EReceiveAutoIncrementalOntoExistingParent`; keep `TestE2EReceiveAutoPrefersFull`
green.

Commit: `receive: --auto applies the incremental when the parent is already there`.

### Task 5: a manifest is the one its name promises (W4)

**Files:** `backup/sync.go:203-230`, `e2e_receive_test.go`.

- Decision 5, in `readCachedManifest` after both successful reads. A mismatch on the
  cached copy is "unreadable" (re-download once); on the fresh download it is an error.

Test: port `TestAdvSecManifestSubstitution` → `TestE2EReceiveRejectsSubstitutedManifest`.

Commit: `receive: reject a manifest stored under another set's name`.

### Task 6: PGP never panics; signature result is synchronised (W3, N6)

**Files:** `pgp/pgp.go:56`, `files/volumeinfo.go:120-170`, `:290-320`.

- Decision 6.
- N6: for an external decompressor, check the signature in `Close()` after `cmd.Wait()`;
  fail if the stdin copy did not reach EOF.

Tests: port `TestAdvRev2PromptFuncPanics` → `TestExtractSymmetricMessageIsKeyError`
(both subtests); `TestAdvRev2ExternalDecompressorSignatureRace` →
`TestE2EReceiveExternalDecompressorVerifiesSignature`, run under `make test-race`.

Commit: `files: a message no decrypted key opens is a KeyError, not a panic`.

### Task 7: bounded manifest decode (W8)

**Files:** `backup/list.go:223-240`, `files/`.

- Decision 7: `io.LimitReader(r, maxManifestBytes+1)`, error if more was read.

Test: port `TestAdvSecForgedManifestDecodedBeforeVerify` → assert an error and
`runtime.MemStats` growth well under the old 516 MiB.

Commit: `list: read at most 64 MiB of a manifest`.

### Task 8: a manifest cannot choose the program receive runs (W6)

**Files:** `files/volumeinfo.go:290-310`, `cmd/receive.go`, `e2e_receive_test.go`.

- Decision 8. Add `--compressor` to `receive` (empty = take the manifest's if allowed).

Test: port `TestAdvSecManifestCompressorExecutes` → `TestE2EReceiveRefusesManifestCompressorPath`;
add a case that `--compressor gzip` sends still restore.

Commit: `receive: run only known decompressors named by a manifest`.

### Task 9: streaming receive and `--signFrom` (W5)

**Files:** `cmd/receive.go:108`, `cmd/root.go` (validation).

- Decision 9.

Test: `TestAdvSecPipeUnverifiedBytesReachZFS` → `TestReceiveRefusesStreamingWithSignFrom`
(flag validation).

Commit: `receive: --maxFileBuffer 0 cannot be combined with --signFrom`.

### Task 10: trusted signers (W2, N8) — confirm Decision 10 first

**Files:** `files/volumeinfo.go:160-175`, `files/jobinfo.go` (a trusted-fingerprint set,
`json:"-"`), `cmd/root.go:150,280-320`, `backup/backup.go` (`sameStream` unchanged),
`docs/runbook-first-backup.md` (a "Rotating the signing key" section).

Tests: port `TestAdvRev2SignKeyRotationWedgesSmartSend` →
`TestE2ESignKeyRotationWithTrustSigner` (fails without the flag, passes with
`--trustSigner <old>`); `TestAdvRev2UnknownSignerWithoutSignFrom` → message assertion.

Commit: `send, receive: --trustSigner accepts manifests from a rotated key`.

### Task 11: `RedactURI` with a port-shaped password (W9)

**Files:** `backends/backends.go:280-300`, `backends/backends_test.go`.

- Take the "@ in the path" shortcut only when the raw authority (`rest` up to the first
  `/?#`) contains no `:` and no `@`. Otherwise use the blunt fallback.

Test: add all inputs from `TestAdvRedactBypass` and `TestAdvSecRedactURI` to
`TestRedactURI`.

Commit: `backends: RedactURI hides a password that looks like a port`.

### Task 12: clean's view of nested destinations and manifest names (W10, W11, N12)

**Files:** `backup/clean.go:270-335`, `backup/sync.go:130-140`, `clean_s3_test.go`.

- W10: when deciding whether a set is missing a volume, look in the full listing
  (`allObjects`), not the nested-filtered candidates; and never put an object a decoded
  manifest of this destination names into the nested filter.
- W11: after `List(j.ManifestPrefix)`, keep only names whose next byte after the prefix is
  the manifest separator (`|`); others belong to another prefix.
- N12: `--force` deletes the manifest object it actually read (carry the object name
  through decoding), not one computed from the content.

Tests: port `TestAdvForceNestedDeletesOwnManifest` (add a `--force` row and a
no-"missing volume" assertion to `TestCleanS3Prefixes`), `TestAdvManifestPrefixCollision`,
`TestAdvSecCleanForceManifestNameTraversal`.

Commit: `clean: nested destinations, manifests-* siblings, and --force deletes what it read`.

### Task 13: `plan` honours `--resume` everywhere (W12, W13, N11)

**Files:** `backup/plan.go:365-373`, `cmd/plan.go:160-200`, `e2e_plan_test.go`.

- `Resume: s.JobInfo.Resume` in `Scenario.run`'s `jobInfo`.
- `sc.Completable = jobInfo.Resume` in the `--manifests` and default branches.
- N11: `sc.AdoptCreationTimes()` in the `--manifests` branch.

Tests: port the three `TestAdvrev2Plan*`/`ManifestsFileNoAdoption` tests as
`TestE2EPlan...`, each comparing `plan` against `send -n`.

Commit: `plan: --resume and creation-time adoption on every input path`.

### Task 14: names are local time (W14)

**Files:** `backup/plan_fixtures.go` (`location()`), `cmd/plan.go`,
`backup/testdata/scenarios/*`, `docs/runbook-first-backup.md`.

- Decision 12. Pin `location=UTC` in every golden scenario first (no output change), then
  switch the default.

Test: port `TestAdvrev2PlanNamesOnlyZone` with `TZ=America/New_York` set for the
subprocess / `time.Local` swapped for the test.

Commit: `plan: date sanoid names in the host's zone unless location= says otherwise`.

### Task 15: a future-dated last backup stops the send (W15)

**Files:** `backup/plan.go:210-260`, `backup/plan_checks.go`, `backup/backup.go`
(error surfacing), `e2e_plan_test.go`.

- Decision 11.

Test: turn `TestAdvrev2FutureManifestSilencesBackups` into an assertion: send exits
non-zero with the message, `plan` reports a failed check.

Commit: `send, plan: refuse a last backup newer than the pool`.

### Task 16: runbook encrypts and signs (W7, N13)

**Files:** `docs/runbook-first-backup.md`.

- Decision 13: key generation (`gpg --quick-gen-key`, export to the two ring files), where
  to keep an offline copy of the secret key, `PGP_PASSPHRASE`, `FLAGS` with
  `--encryptTo`/`--signFrom`/both ring paths, and every later command using `$FLAGS`.
- N13: "fulls land on the 1st of the month your chain started, then every six months";
  name the retention in the missed-runs sentence and give the `monthly=6` figure.
- Mention Task 10's key rotation once it lands.

Commit: `docs: the runbook encrypts and signs the first backup`.

### Task 17: notes batch (N1, N2, N4, N5, N9, N10, N16)

- N1 `cmd/clean.go`: `--cleanLocal` help: "...and delete their volumes at the
  destination"; refresh Short/Long to describe what `clean` deletes now (unreferenced
  volumes of datasets that have manifests here).
- N2 `backup/backup.go:440`: "Another send or a clean of %s is running".
- N4 `backends/aws_s3_backend.go:374`: treat `ArchiveStatus` `ARCHIVE_ACCESS` /
  `DEEP_ARCHIVE_ACCESS` as needing a restore (no `Days` for these).
- N5 same file: return a RestoreObject error unless it is `RestoreAlreadyInProgress`.
- N9 `backup/plan_fixtures.go:525,529`: an empty side of `skip=` is an error.
- N10 `backup/plan.go:326-330`: `Advance` reports what it took before pruning; record that.
- N16 `cmd/list.go`: reject a comma-separated destination like `clean` does.

Each with a test where it has behaviour (N4/N5 against the fake S3, N9, N10, N16).

Commit per pair is fine: `clean, send: messages`, `backends: S3 restore edge cases`,
`plan: skip= and coverage`, `list: one destination`.

---

## Suggested order

1 (clean race) and 2 (resume cache) first: they lose data under the runbook's own cron
usage. Then 16 (runbook encrypts) before any first real backup. Then 5, 6, 7, 8, 9
(hostile-destination hardening; independent, subagent-friendly), 4, 11, 12, 13 + 14
(golden review together), 15, 3, 17. Task 10 waits on Decision 10's confirmation.

## Verification checklist

- Every ported test fails on `5892fea` and passes after its task (scratch copy via
  `git archive 5892fea`).
- `make scenarios` clean, with every golden change explained.
- `make devcontainer-firewall-check` passes, including the kill step.
- `make fmt-check`, `make test-docker`, `make test-race` green.
- Memory note `adversarial-review-2026-10-08-round2` updated with what landed.
