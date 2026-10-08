# Adversarial review (2026-10-08) Fix Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix what the 2026-10-08 full-branch adversarial review found. Five bugs can leave
a destination unrestorable, make the documented recovery path fail at restore time, let
`plan` contradict `send`, accept a forged manifest, or stop every send forever. Nine more
wedge runs, leak credentials, or make the planner's simulation lie. All of them were
re-verified in this session (see `intent.md`): the reviewer tests in
`/tmp/advrev-{send,clean,plan,sec}` fail on `4eee1c3` exactly as claimed.

**Tech Stack:** Go 1.25, vendored deps, in-process e2e tests (`e2e_*_test.go`, fake zfs in
`internal/fakezfs`), the fake S3 in `clean_s3_test.go`, golden scenarios in
`backup/testdata/scenarios`.

Line numbers are as of `4eee1c3` on `clean-dry-run`; re-check them before editing.

---

## Ground rules

- Branch `clean-dry-run`. One commit per task. Commit messages follow the existing style
  (`send: ...`, `receive: ...`, `clean: ...`, `plan: ...`, `files: ...`, `backends: ...`,
  `docs: ...`, `devcontainer: ...`).
- TDD: each task names a test that must fail on the unfixed code. Run it and watch it fail
  before fixing anything. A test that passes on the first run is wrong (napkin, 2026-10-02).
  The reviewer tests are a starting point; port them under the repo's naming
  (`TestE2E...` in the root package, `TestClean...`/`TestPlan...` in `backup`), drop the
  `t.Logf`-only probes, and keep their assertions.
- Fast loop: `make test-run PKG=./backup/ RUN='TestName'` (also `./files/`, `./backends/`,
  `./zfs/`, and `PKG=.` for the root e2e tests). After any in-process cobra test, run the
  whole package: `RootCmd.SetArgs` and the flag globals leak (napkin).
- Golden scenarios: `make scenarios`; when a task changes planner output on purpose,
  `make scenarios-update`, then read the diff and justify every changed line in the commit
  message.
- Never gate a commit on a piped test run (napkin, 2026-10-08): run the test to a file,
  check its status, then commit in a separate call.
- Stage files by name. Another session may edit this tree.
- Before declaring the plan done: `make fmt-check` and `make test-docker`.

## Findings

Severity: **CRITICAL** means a wrong or unrestorable backup while reporting success, a
forged input accepted, or every send stopped. **WARNING** wedges runs, leaks secrets, or
makes a check lie. **NOTE** is cosmetic or narrow.

| #   | Sev      | Verified by                                                              | Summary                                                                                                                                                                                                                                                                                                                                                                  | Task |
| --- | -------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---- |
| C1  | CRITICAL | `TestAdvrevResumeTrustsStaleCacheAfterFreshAttempt` (send copy)          | A run without `--resume` never discards the cached partial manifest. It rewrites same-size volumes at dest1 (PGP output differs per run), fails at dest2 before any volume finishes the pipeline, and `--resume` then keeps dest1's new bytes under the old SHA-256s. Exit 0; restore from dest1 fails the checksum.                                                     | 3    |
| C2  | CRITICAL | `TestAdvRevAutoRestorePrefersFull` (clean copy), both subtests           | `receive --auto` matches the manifest by snapshot name only and takes the first in creation order, which is the incremental (`backup/restore.go:101`; `readAndSortManifests` at `backup/list.go:156` has no tie rule). After the runbook's `send --full` restart, restore walks the old chain and fails once the old full is gone.                                       | 1    |
| C3  | CRITICAL | `TestAdvE2ENamesOnlyCaptureWithDestination` (plan copy)                  | A capture without creation times dates snapshots from their names (`zfs/zfs.go:252`); manifests hold the real time, seconds later; `SnapshotInfo.Equal` compares times, so the last backup looks gone from the pool: `plan` says FULL `source-pruned` and exits 2 while `send -n` does an INCR.                                                                          | 5    |
| C4  | CRITICAL | `TestAdvrevUnsignedForgedManifestStopsBackups` (send copy)               | `files/volumeinfo.go:134` checks a signature only `if v.pgpr.IsSigned`. An unsigned manifest encrypted to the public key, claiming a full from 2099, makes every smart send print "Nothing new to back up" and exit 0. Also: `readManifest` (`backup/list.go:222`) stops at the end of the JSON value, before the EOF where a present signature would be verified.       | 4    |
| C5  | CRITICAL | `TestAdvrevStaleLegacyLockWedges`, `TestAdvRevLegacyLockInTmpBlocksSend` | `legacyLockHolder` (`backup/backup.go:389`) treats any `/tmp/zfsbackup.<md5>.lck` whose pid is alive as a running old-version send and never removes it. `1\n` planted by any local user, or a stale pid reused after a reboot, blocks every send of that dataset forever with a misleading error. `clean` has the same probe (`backup/clean.go:239`).                   | 2    |
| W1  | WARNING  | `TestAdvrevRedactLeaks` (sec copy) + code                                | `backends.RedactURI` returns the URI unchanged when the password holds a `/` (it cuts the authority at the first `/`). Raw URIs are logged at `cmd/plan.go:124,166`, `cmd/send.go:221,224`, `cmd/receive.go:208,211`, `backup/clean.go:121`. A second helper, `backup.redactURI` (`backup/sync.go:43`), parses with `net/url` and so fails on a different set of inputs. | 6    |
| W2  | WARNING  | `TestAdvRevCleanLocalSparesRunningSendState` (clean copy)                | `clean --cleanLocal` deletes local-only manifests (`backup/clean.go:160-171`) before the per-dataset lock loop (`:225`), so it destroys a running send's resume state. Those datasets are not even in the lock set, since local-only manifests are not decoded.                                                                                                          | 7    |
| W3  | WARNING  | `TestAdvRevS3PreDownloadDeepArchive` (clean copy)                        | `PreDownload` (`backends/aws_s3_backend.go:276`) restores only storage class `GLACIER`; `DEEP_ARCHIVE` volumes are fetched directly, get `InvalidObjectState`, and are retried for `--maxRetryTime` (12h default). `*resp.Restore` (`:337`) is dereferenced without a nil check.                                                                                         | 8    |
| W4  | WARNING  | code (`backup/backup.go:905-918`, `hashObject`)                          | Completing a partial set hashes every volume at each lagging destination (`verifyVolumesAt`). That is one download per completion, but if those volumes are in an archive class the download fails and smart mode plans `complete-partial` again on every cron run: the dataset is wedged until someone intervenes.                                                      | 8    |
| W5  | WARNING  | `TestAdvSkipOrderDependsOnLocation` (plan copy)                          | `skip=` is parsed as it is read (`backup/plan_fixtures.go:403`) with whatever `location=` has been seen so far; `from=`/`until=` are deferred. The runbook puts `location=` last, so a `skip=` added before it is read as UTC.                                                                                                                                           | 9    |
| W6  | WARNING  | `TestAdvSkipDateOnlyLastDayRuns` (plan copy)                             | A date-only `skip=` end is midnight, so the 01:00 run on the last day happens. The spec's own example (`plan_fixtures.go:355`, `skip=2026-12-24..2027-01-03`, "inclusive") has this shape.                                                                                                                                                                               | 9    |
| W7  | WARNING  | `TestAdvCoverageMissesMonthlyPrunedDuringOutage` (plan copy)             | `checkCoverage` (`backup/plan_checks.go:313`) takes its candidates from the snapshots some run saw. A monthly taken and pruned inside a `skip=` outage is never a candidate, so coverage reports January and February missing but not December.                                                                                                                          | 10   |
| W8  | WARNING  | `TestAdvE2ESnapshotsOfOtherDataset` (plan copy)                          | `plan --snapshots` of `tank/OTHER` for volume `tank/data` prints a plan and `checks: OK`. The listing checks rows against each other (`zfs/zfs.go:226`) but never against the volume argument.                                                                                                                                                                           | 5    |
| W9  | WARNING  | `.devcontainer/init-firewall.sh` read                                    | Flush (`:58`) comes before the GitHub fetch (`:91`) and the DROP policies (`:125`). First start: a fetch failure exits under `set -e` with ACCEPT policies: open. Re-run: policies are already DROP, the flush removes the allow rules, the fetch fails, the script dies: locked out.                                                                                    | 11   |
| N1  | NOTE     | shown in C3's output ("29d24h")                                          | `formatDays` (`backup/plan_checks.go:521`) rounds the remainder to 24h.                                                                                                                                                                                                                                                                                                  | 12   |
| N2  | NOTE     | code                                                                     | Stale `// hasFullOf` comment above `partialSet` (`backup/plan.go:597`); `Backup`'s doc comment and `nolint` sit above `pendingVolumes` (`backup/backup.go:329`).                                                                                                                                                                                                         | 12   |
| N3  | NOTE     | `docs/runbook-first-backup.md:76,238,261`                                | Copy-paste commands hard-code `location=America/New_York` though line 66 says to pass your own zone.                                                                                                                                                                                                                                                                     | 12   |
| N4  | NOTE     | `docs/runbook-first-backup.md:128`                                       | Says `GLACIER_IR` "cannot be read at all". Glacier Instant Retrieval objects are readable with a plain GET; only `GLACIER` and `DEEP_ARCHIVE` need a restore.                                                                                                                                                                                                            | 12   |
| N5  | NOTE     | `cmd/plan.go:74-89`                                                      | `plan` has no `--resume`, so the runbook's `--resume` recovery steps cannot be previewed (`PartialSetCompletable` returns true on `Resume`).                                                                                                                                                                                                                             | 12   |
| N6  | NOTE     | `backup/plan.go:106-140`                                                 | With a partial set, `--full` errors "destinations are out of sync" unless `--resume`; `--fullIfOlderThan` completes it when the volumes are there. Document; do not unify.                                                                                                                                                                                               | 12   |
| N7  | NOTE     | `cmd/clean.go:40`, `backup/clean.go:86`                                  | `clean a,b` parses both and cleans `Destinations[0]` only.                                                                                                                                                                                                                                                                                                               | 12   |
| N8  | NOTE     | `backup/restore.go:475`                                                  | The receive path logs "Could not kill zfs **send** command". The "unbuffered channel" half of this note is wrong: every channel in `restore.go` is buffered. Dropped.                                                                                                                                                                                                    | 12   |
| N9  | NOTE     | `dev.nix:21,25`                                                          | `rev` defaults to `"master"` and `hash` to `lib.fakeHash`, so `nix-build dev.nix` fails unless both are passed. The comment says it defaults to the branch tip.                                                                                                                                                                                                          | 12   |
| N10 | NOTE     | `.devcontainer/init-firewall.sh:21-39,63-66,75-76`                       | Allowed egress is wider than the sandbox's purpose: port 53 to any host, every GitHub range, `sentry.io`, `statsig.com`, `vscode.blob.core.windows.net`, the host `/24`.                                                                                                                                                                                                 | 11   |
| N11 | NOTE     | `Dockerfile:51`                                                          | `CLAUDE_CODE_VERSION=latest`.                                                                                                                                                                                                                                                                                                                                            | 12   |

Already planned elsewhere, not repeated here: the `flock` lock (send-path Task 12; its
line "the legacy `os.TempDir()` probe keeps using pid semantics" is superseded by Task 2
below), `clean` reclaiming superseded partials (send-path Task 13; Task 7 below only
reorders), SSH `known_hosts` warnings and the README note on `SSH_PASSWORD` (backends-bugs
Task 10 items 2-3; its item 1 is replaced by Task 6 below).

## Decisions made in this plan (review these first)

1. **A run without `--resume` discards the set's cached partial manifest before it
   sends anything.** The cache describes an attempt this run is abandoning. The only
   alternative, hashing every verified volume on `--resume`, downloads the whole prefix
   on every resume. Consequence, stated in the runbook: `--resume` after a plain run that
   failed before its first volume completed starts over, by design.
2. **`receive` prefers the full of a snapshot** at every place a snapshot name is looked
   up (the explicit name, the "latest" default, and the parent walk, which already does).
   The ordering rule is the one `send` adopted in `6f96362`.
3. **A names-only capture used with a real destination adopts creation times from the
   manifests** for snapshots with the same name, and `plan` says so once at Warning. A
   names-only capture cannot tell a recreated snapshot from the original, so this is the
   best answer available; the warning tells the user to capture `creation` when it
   matters. Refusing was rejected: the runbook promotes names-only JSON captures.
4. **`--signFrom` requires a valid signature on everything it reads**, and `--encryptTo`
   requires encryption. A manifest that is unsigned, signed by another key, or altered is
   rejected before its JSON is used. The reader drains the message to its end so that
   the signature is actually checked. Encrypted-only manifests written before this change
   with `--signFrom` do not exist: `CreateManifestVolume` signs whenever `SignKey` is set.
5. **The legacy `/tmp` lock probe is removed**, from `send` and `clean`. See the
   assumption in `intent.md`. The `lockfile` package stays for the working-directory lock
   until send-path Task 12 replaces it.
6. **One redaction helper, `backends.RedactURI`.** It parses with `net/url` first. When
   that fails the string has no reliable structure, so the fallback is blunt: if it
   contains `@`, drop everything between `://` and the last `@`. Over-redacting a log
   line is fine; leaking a password is not.
7. **Schedule keys are parsed after the whole spec is read**, so key order never matters.
   A date-only `skip=` end means "through the end of that day": `Until` becomes the next
   midnight in the scenario's location and `skipped` treats `Until` as exclusive. A
   `skip=` end with a time keeps its exact meaning.
8. **The simulation records every snapshot the schedule creates**, not only the ones a
   run saw, and `coverage:` judges all of them from the first run on.
9. **The firewall is deterministic**: policies are reset to ACCEPT before the flush, the
   allowlist is built, then enforced; any failure after the flush traps into a fail-closed
   state (loopback only) with the reason on stderr, and the script says how to re-run.

---

### Task 1: `receive` prefers the full backup of a snapshot (C2)

**Files:** `backup/restore.go:88-109`, `backup/list.go:156-163`, `e2e_receive_test.go`.

- `readAndSortManifests`: order by volume, then base creation time ascending, then
  incrementals before fulls (so the full is the last, "newest", entry of a tie). Mirror
  `sortBackupsNewestFirst`'s comment: every reader sees the same order whatever the
  destination lists.
- The name lookup at `restore.go:101`: among manifests whose `BaseSnapshot.Name` matches,
  take the one with `IncrementalSnapshot.Name == ""` if there is one. Write it as a small
  `fullOrAny(matches)` helper and use it for the "latest snapshot" default too, which
  today takes `volumeSnaps[len-1]` and is only right because of the sort.
- `cmd/receive.go` `--auto` help text: "...restores from the full backup of that
  snapshot when there is one".

Test, port `TestAdvRevAutoRestorePrefersFull` → `TestE2EReceiveAutoPrefersFull`
(`e2e_receive_test.go`): send `a` (full), `a→b` (incremental), `b` (full, the runbook's
restart); subtest `keep-old-full` expects exactly one `zfs receive` (`FAKEZFS_RECEIVE_LOG`
line count), subtest `old-full-retired` removes `a`'s manifest and volumes and the cache,
and expects success. On `4eee1c3` the first gets 2 receives and the second fails "could
not find parent snapshot".

Commit: `receive: --auto restores a snapshot from its full backup when there is one`.

### Task 2: drop the legacy `/tmp` lock probe (C5)

**Files:** `backup/backup.go:380-399,477-484`, `backup/clean.go:239-249`,
`backup/clean_test.go:315-350` (`TestCleanHonoursLegacySendLock`), `e2e_test.go` or
`e2e_failure_test.go`, `docs/runbook-first-backup.md` (the lock paragraph).

- Delete `legacyVolumeLockPath`, `legacyLockHolder`, both call sites, and
  `TestCleanHonoursLegacySendLock`. `TestCleanHonoursSendLockAcrossTMPDIR` stays.
- Runbook: the lock lives under `--workingDirectory/locks/`; nothing in `/tmp` is read.

Test: `TestE2ESendIgnoresTmpLockFile` (root package): write `1\n` to
`os.TempDir()/zfsbackup.<md5 of "tank/data">.lck` (compute the name in the test; the
helper is gone), run two sends, both succeed. On `4eee1c3` both fail with "an older
version of zfsbackup (pid 1) is sending". Add the same planted file to
`TestCleanHonoursSendLockAcrossTMPDIR`'s setup and assert `clean` still deletes the
orphan.

Commit: `send, clean: stop honouring the pre-dc37d16 lock in /tmp`.

### Task 3: a fresh attempt invalidates the cached partial manifest (C1)

**Files:** `backup/backup.go:486-492` (between `refuseExistingSet` and the `Resume`
branch), `backup/backup.go:964-1000` (`saveManifest`), `backup/backup.go:1236-1238`
(`tryResume`), `e2e_resume_test.go`, `docs/runbook-first-backup.md:181-196`.

- Extract `partialManifestCachePath(j, uri) string` =
  `filepath.Join(cacheDirFor(uri), md5hex(j.ManifestObjectName()))`. `CreateManifestVolume`
  sets `ObjectName = j.ManifestObjectName()` (`files/volumeinfo.go:540`), so `saveManifest`
  and `tryResume` can both use the helper instead of creating a manifest volume to learn
  the name (`tryResume` still needs its temporary volume for the `uploadManifest` path;
  keep that, drop only the md5 computation).
- In `Backup`, when `!jobInfo.Resume`, after `refuseExistingSet` and before the pipeline:
  for every destination, remove the partial cache file if it exists, logging at Notice
  "Discarding the cached state of an earlier attempt at <set>; this run starts over.
  Use --resume to continue it instead." Ignore `IsNotExist`; any other error fails the run
  (a cache the run cannot clear would be trusted later).
- Runbook (`:181-196`): `--resume` continues only the most recent attempt; a plain run in
  between abandons the earlier one.

Test, port `TestAdvrevResumeTrustsStaleCacheAfterFreshAttempt` →
`TestE2EResumeIgnoresCacheOfAbandonedAttempt` (`e2e_resume_test.go`). Keep its shape:
PGP keys, `--compressor ""`, `--volsize 1`, two `file://` destinations, 3 MiB stream;
attempt A completes and is trimmed to look interrupted; attempt B (no `--resume`) fails at
dest2 (a file at `<dest2>/tank`) after rewriting dest1; `--resume` then runs, and a
`receive` from dest1 must succeed. Additionally assert that `--resume` logged
"No previous manifest file exists" or "Nothing verifiable to resume" (B cleared the
cache). On `4eee1c3` resume exits 0 and the receive fails with a SHA-256 mismatch on vol1.

Commit: `send: a run without --resume discards the cached partial manifest first`.

### Task 4: `--signFrom` requires a signature, `--encryptTo` requires encryption (C4)

**Files:** `files/volumeinfo.go:127-144` (`Read`), `:209-235` (`Extract`), a new
`VerifyEnd` method; `backup/list.go:215-229` (`readManifest`); `files/volumeinfo_test.go`
or `files/pgp_test.go`; `e2e_receive_test.go` (has `newKey`, `writeRings`).

- `Extract`, right after `openpgp.ReadMessage`: if `j.SignKey != nil && !pgpReader.IsSigned`
  → `fmt.Errorf("%s is not signed, and --signFrom requires a signature", v.ObjectName)`;
  if `j.EncryptKey != nil && !pgpReader.IsEncrypted` → "...is not encrypted, and
  --encryptTo requires encryption". Name the file for `ExtractLocal` callers (use
  `v.filename` when `ObjectName` is empty).
- Factor the EOF checks in `Read` into `func (v *VolumeInfo) signatureError() error`:
  `SignatureError != nil` → it; `SignedBy == nil` → "signed by a key not in --signFrom's
  ring" (fix the "ths" typo); and `SignedBy` must be the key `j.SignKey` names (compare
  `PrimaryKey.Fingerprint` with `j.SignKey`'s), otherwise "signed by <fp>, want <fp>".
  `Read` calls it at EOF as today.
- `VerifyEnd() error`: `io.Copy(io.Discard, v.r)` then `signatureError()`. No-op when
  `v.pgpr == nil`. `readManifest` calls it after `Decode` and before returning the
  manifest. This is what makes a _present_ signature verified at all (the 2026-09-29
  note: `json.Decoder` stops before EOF).
- The volume path (`receive`) reads to EOF through `Read`, so it already runs the EOF
  check; `VerifyEnd` is for readers that stop early.

Tests:

- `files`: `TestExtractRequiresSignature` — a manifest volume written with `EncryptKey`
  only is rejected by `Extract` with `SignKey` set; written with both, accepted.
  `TestExtractRequiresEncryption` — signed-only volume rejected when `EncryptKey` is set.
  `TestVerifyEndRejectsOtherSigner` — sign with key A, read with `SignKey` = key B (only
  B's public key in the ring): `VerifyEnd` fails "signed by ..., want ...". The JSON
  decodes fine before that call, which is the point of `VerifyEnd`.
- Root: port `TestAdvrevUnsignedForgedManifestStopsBackups` →
  `TestE2ESignFromRejectsForgedManifest`: after a signed send, plant an unsigned manifest
  encrypted to the public key claiming a 2099 full; the next smart send must fail with an
  error mentioning "not signed" and must not print "Nothing new to back up". On `4eee1c3`
  it prints exactly that and exits 0.

Commit: `files: --signFrom requires a verified signature; --encryptTo requires encryption`.

### Task 5: `plan --snapshots` checks the dataset and adopts manifest times (C3, W8)

**Files:** `zfs/zfs.go:143-164,197-260` (`ParseSnapshotList`, `listing`),
`backup/plan_fixtures.go:235-245` (`ReadSnapshots`), `cmd/plan.go:143-175`,
`e2e_test.go` (`env.plan`), `backup/plan_fixtures_test.go`, `zfs/zfs_test.go`,
`docs/runbook-first-backup.md` (step 1 capture advice).

- `zfs`: add `type Listing struct { Dataset string; Snapshots []files.SnapshotInfo;
NameDated []string }` and `ParseListing(r, loc) (*Listing, error)`; `ParseSnapshotList`
  becomes `ParseListing(...).Snapshots`. `listing.add` appends to `NameDated` when
  `creation == ""`.
- `Scenario`: fields `CaptureDataset string` and `NameDated []string`, set by
  `ReadSnapshots`.
- `cmd/plan.go`, after `ReadSnapshots`: if `sc.CaptureDataset != "" && != sc.Volume` →
  `errInvalidInput` with "--snapshots lists tank/OTHER, not tank/data". After the
  destinations are read (the `len(args) == 2` case only): for each name in `NameDated`,
  find a manifest at any destination whose `BaseSnapshot.Name` or
  `IncrementalSnapshot.Name` equals it and set the snapshot's `CreationTime` from it (re-sort
  newest-first afterwards). Warning, once: "The capture has no creation times: <n> taken
  from the manifests at the destination, the rest from the snapshot names. Capture
  `zfs list -H -p -o name,creation -t snapshot,bookmark -S creation <ds>` to plan
  exactly what send will do." Put the adoption in `backup` as
  `(s *Scenario) AdoptCreationTimes()` so it is unit-testable.
- Runbook: name the exact capture command above as the one to use with a destination.

Tests:

- `zfs`: `ParseListing` returns `Dataset` and `NameDated` for text and JSON inputs.
- `backup`: `TestScenarioAdoptCreationTimes` — one name-dated snapshot, one with an
  epoch; a manifest for each with a time 7s later; only the name-dated one changes.
- Root, port `TestAdvE2ENamesOnlyCaptureWithDestination` →
  `TestE2EPlanNamesOnlyCaptureMatchesLivePool`: `plan` from the capture must equal `plan`
  from the live pool (both INCR) and the logs must contain the Warning. On `4eee1c3` the
  capture plan is FULL `source-pruned`, exit 2.
- Root, port `TestAdvE2ESnapshotsOfOtherDataset` → `TestE2EPlanRejectsOtherDataset`.

Commit: `plan: refuse a capture of another dataset; date name-only snapshots from the
manifests`.

### Task 6: one URI redaction helper, used everywhere (W1)

**Files:** `backends/backends.go:278-291`, `backup/sync.go:43-62` (delete `redactURI`,
keep `joinURI` on `backends.RedactURI`), `cmd/plan.go:124,166`, `cmd/send.go:221,224`,
`cmd/receive.go:208,211`, `backup/clean.go:121`, `backends/backends_test.go`.

- `RedactURI` per Decision 6: `url.Parse` → if `u.User != nil` return `u.Redacted()`,
  else the input; on parse error, if the string contains `@`, return
  `scheme + "://" + rest[strings.LastIndex(rest, "@")+1:]`, else the input.
- `rg -n 'Errorf|Warningf|Noticef|Infof|Debugf' cmd backup backends | rg -i 'uri|target|destination'`
  and route every raw URI through `RedactURI`; the eight sites above are the known ones.

Test: `TestRedactURI` table: `ssh://u:p@h/x` → `ssh://u:xxxxx@h/x`; `ssh://u:pa/ss@h/x`
→ contains neither `pa/ss` nor `pa`; `ssh://u:p?w@h/x`, `ssh://u:p#w@h/x` likewise;
`file:///x`, `s3://b/p/`, `s3://b` unchanged. `TestSSHInitErrorRedacts` (port of the
sec copy's `Init` check). Plus a cmd-level test: `plan` with an `ssh://u:pa/ss@h/x`
destination that fails must not log `pa/ss` (`captureLogs`).

Commit: `backends: one RedactURI that never returns a password; use it in every log`.

### Task 7: `clean --cleanLocal` honours the send lock before deleting local state (W2)

**Files:** `backup/clean.go:128-171,219-262`, `backup/clean_test.go`.

- Decode local-only manifests under `--cleanLocal` too (best effort: an undecodable one is
  deleted as today, after the lock check cannot apply to it). Add their datasets to the
  lock set.
- Move the lock loop before the local-only deletion. For a busy dataset, skip its
  local-only manifests with the existing "appears to be running ... leaving its volumes
  alone" Notice extended with "and its cached manifest".
- Honour `--dry-run` as today.

Test, port `TestAdvRevCleanLocalSparesRunningSendState` →
`TestCleanLocalSparesRunningSendState` (`backup/clean_test.go`): a live set at the
destination, a local-only partial for the same dataset, the dataset's lock held by
`os.Getppid()`; `Clean(ctx, j, true, false)` must leave the partial. On `4eee1c3` it
deletes it. A second case: lock released → partial deleted.

Commit: `clean: --cleanLocal leaves a running send's cached manifest alone`.

### Task 8: S3 restore thaws `DEEP_ARCHIVE`; partial-set completion thaws first (W3, W4)

**Files:** `backends/aws_s3_backend.go:276-351`, `backup/backup.go:881-934`
(`verifyVolumesAt`), `clean_s3_test.go` (`newFakeS3`), a new `e2e_s3_test.go` or the
existing root S3 tests, `docs/runbook-first-backup.md:125-131`.

- `PreDownload`: restore when `StorageClass` is `GLACIER` **or** `DEEP_ARCHIVE`
  (`s3.ObjectStorageClassDeepArchive`). `GLACIER_IR` needs nothing. Skip
  `RestoreObject` when `resp.Restore` already says `ongoing-request="false"` (restored
  copy present). In the wait loop treat a nil `Restore` as "not yet", not a panic.
- `verifyVolumesAt`: before hashing, if the backend implements `PreDownload`, call it
  with the manifest's volume names (the `receive` path already does this). Then, on a
  download error, fail with the existing message plus "If these volumes are in an archive
  storage class, restore them first or complete the set by hand".
- Runbook `:125-131`: `GLACIER` and `DEEP_ARCHIVE` are restored first (billable, hours);
  `GLACIER_IR` reads directly.

Tests:

- Port `TestAdvRevS3PreDownloadDeepArchive` → `TestE2ES3ReceiveRestoresDeepArchive`:
  the fake answers HEAD with `x-amz-storage-class: DEEP_ARCHIVE`, 403 `InvalidObjectState`
  on GET until a `?restore` POST was seen for that key, then serves it. Assert one
  restore request per volume and a successful receive (`--maxRetryTime 20s`). On
  `4eee1c3` no restore is issued and the receive fails after the retry window.
- `TestS3PreDownloadNilRestoreHeader`: HEAD without `x-amz-restore` during the wait loop
  must not panic.
- `TestE2ES3CompletePartialThawsFirst`: a partial set at a fake S3 destination with
  `GLACIER` volumes; the smart run that completes it must issue restores before hashing.

Commit: `backends/s3: restore DEEP_ARCHIVE too, nil-safe wait; send thaws before verifying
a partial set`.

### Task 9: `skip=` is order-independent and a date-only end covers its day (W5, W6)

**Files:** `backup/plan_fixtures.go:340-360` (spec comment), `:370-470`
(`ParseScheduleSpec`, `parseTime`), `backup/plan.go:327-335` (`skipped`),
`backup/plan_fixtures_test.go`, `docs/runbook-first-backup.md:85`.

- Collect `skip=` values as strings in the loop; parse them after `location=` is known,
  next to `from=`/`until=`.
- `parseTime` returns whether the value was date-only (a third return, or a small
  `parsedTime{t, dateOnly}`); for a `skip=` end that is date-only, `Until = t.AddDate(0,0,1)`
  and `skipped` uses `at.Before(r.Until)`. For an end with a time, keep `!at.After(r.Until)`
  by storing the exact instant plus one nanosecond... no: store `Until` exclusive in both
  cases (`t.Add(time.Nanosecond)` for the timed form keeps "inclusive of that instant").
- Spec comment: "`skip=2026-12-24..2027-01-03` — no runs from the 24th through the 3rd;
  a date means the whole day, a time means that instant".
- Napkin: replace "a date alone means midnight, so give the run's hour".

Tests, port both reviewer tests → `TestScheduleSkipOrderIndependent`,
`TestScheduleSkipDateOnlyCoversLastDay`. Then `make scenarios`: the only scenario with
`skip=` (`monthly-only-year-2-month-outage`) uses timed ends, so no golden should change;
if one does, stop and look.

Commit: `plan: skip= ranges parse after location=, and a date-only end covers its day`.

### Task 10: `coverage:` sees snapshots pruned during an outage (W7)

**Files:** `backup/plan.go:296-322` (`Run`), `backup/plan_checks.go:309-355`
(`checkCoverage`), `backup/plan_fixtures_test.go` or `plan_checks_test.go`,
`backup/testdata/scenarios/*/expect-violations` as needed.

- `Simulation` gets `Created []files.SnapshotInfo`: in `Run`, after each
  `Schedule.Advance`, append snapshots not seen before (by `snapshotID`). Also seed it with
  the initial `s.Snapshots`.
- `checkCoverage` candidates = `Created` ∪ snapshots any step saw (keep the union so a
  no-schedule run still works), still filtered by the first run's horizon.

Test, port `TestAdvCoverageMissesMonthlyPrunedDuringOutage` →
`TestCoverageNamesMonthlyPrunedDuringOutage`: expect `autosnap_2026-12-01_00:00:00_monthly`
in the violations. Then `make scenarios`: any scenario with an outage longer than its
`monthly=` retention gains violations; add them to that scenario's `expect-violations`
only after reading each one and confirming it names a monthly that really was never sent.

Commit: `plan: coverage judges every snapshot the schedule took, not only those a run saw`.

### Task 11: deterministic firewall (W9, N10)

**Files:** `.devcontainer/init-firewall.sh`, `Makefile` (`devcontainer-build` already
exists; add `devcontainer-firewall-check` that runs the script twice in a throwaway
container with `--cap-add NET_ADMIN --cap-add NET_RAW` and expects both runs to succeed).

- Right after saving the Docker DNS rules: `iptables -P INPUT ACCEPT; -P FORWARD ACCEPT;
-P OUTPUT ACCEPT` (and the ip6tables equivalents), then flush. A re-run can now fetch.
- `trap fail_closed ERR` after the flush: `fail_closed` sets the three policies to DROP,
  flushes, allows loopback only, prints "firewall failed: <reason>; network is closed; fix
  and re-run sudo /usr/local/bin/init-firewall.sh", exits 1. Capture the failing command
  via `BASH_COMMAND` for the reason. Clear the trap before the final verification block,
  which already dies with a message of its own.
- N10, cheap part only: restrict port 53 to the resolvers in `/etc/resolv.conf` (and
  127.0.0.11 when present). Leave the GitHub ranges, Sentry, Statsig and the VS Code CDN
  as they are, but list them in the header comment as known-wide exits.

Test: the Makefile target above, run by hand (the napkin records that this host's
containers cannot reach api.anthropic.com; that is a Warning in the script, not a
failure). Record the run's output in the commit message.

Commit: `devcontainer: firewall resets policies before the flush and fails closed`.

### Task 12: notes batch (N1-N9, N11)

One commit per bullet where the touched area differs; `docs:` bullets can share one.

- N1 `formatDays`: round the whole duration to hours first, then split
  (`d = d.Round(time.Hour)`). Test: `29d23h30m` → `30d`; `29d23h29m` → `29d23h`.
- N2 comments: move `// hasFullOf` to `hasFullOf`; move `Backup`'s doc comment and
  `nolint` back above `func Backup`.
- N3, N4, N6 runbook: `location=<your zone>` in the copy-paste commands with one note
  that the examples assume `America/New_York`; `GLACIER_IR` reads directly (Task 8
  rewrites that paragraph, do it there); a sentence under the restart section that
  `--full` with a partial set says "out of sync" and `--resume` completes it.
- N5 `plan --resume`: a bool flag bound to `jobInfo.Resume`, help "Plan as `send --resume`
  would: complete a backup set missing at some destinations". Test: an e2e `plan --resume`
  of a partial set shows `complete-partial` where plain `plan` does not (the
  `interruptedSend` helper plus a manifest removal gives the state).
- N7 `clean a,b`: `validateCleanFlags` rejects a comma in the argument with "clean takes
  one destination". Test in `cmd`.
- N8 receive log text: "zfs receive command".
- N9 `dev.nix`: pin `rev` to the current tip and `hash` to its real value (the user must
  run `nix-build` to get it; nix is not on this host) **or** change the comment to say
  both must be passed. Do the comment now; leave a TODO for the hash.
- N11 Dockerfile: `CLAUDE_CODE_VERSION` pinned to the version `claude --version` reports on
  the host at implementation time, with a comment on how to bump.

---

## Suggested order

1 (receive), 2 (legacy lock), 3 (resume cache), 4 (signatures), 5 (plan capture), 7
(cleanLocal), 6 (redaction), 9 and 10 (schedule/coverage, golden review together), 8 (S3),
11 (firewall), 12 (notes). Tasks 1-4 are independent of each other and of the rest; a
subagent per task works for them.

## Verification checklist

- Every ported test fails on `4eee1c3` (check with a scratch copy:
  `git archive 4eee1c3 | tar -x -C /tmp/zfsb-base`, copy the test file in, run in the
  pinned image) and passes after its task.
- `make scenarios` clean, with every golden change explained in a commit message.
- `make fmt-check`, `make test-docker` green.
- `docs/runbook-first-backup.md` reflects Tasks 2, 3, 5, 8, 12.
- Memory note `adversarial-review-2026-10-08` updated with what landed.

## Implementation notes (2026-10-08)

Executed the same day, 16 commits `1119fb1..0b50855` on `clean-dry-run`. Tasks 1-4 ran as
subagents in worktrees and were cherry-picked (one conflict, in `tryResume`, between Tasks 3
and 4). Every ported test was run and seen failing on the unfixed code first, except the
`plan --resume` test, which could not compile without the flag. `make scenarios` changed no
golden (Tasks 9 and 10), `make fmt-check` and `make test-docker` are green. Deviations:

- Task 1: the "latest snapshot" default still takes the newest base's name and resolves it
  through the same `fullOrAny` lookup, rather than a second call; same result.
- Task 2: the planted lock names `os.Getppid()`, not pid 1. As a normal user, signal 0 to
  pid 1 is EPERM, which `nightlyone/lockfile` reads as "not running", so a pid-1 plant made
  the unfixed code pass on the host (the reviewer tests only failed as root in Docker). The
  second send is a real incremental (two snapshots); a smart no-op never reaches the lock.
- Task 3: `partialManifestCachePath` reuses `cachedManifestName`; "No previous manifest
  file exists, nothing to resume; starting over." moved from Info to Notice so that it shows
  at the default level (and so the test could see it: Info is filtered once `processFlags`
  has run).
- Task 4: verifying signatures in `readManifest` turned `TestE2EResumeRefusesRotatedKey`
  into a start-over (the cache signed by the old key became "unreadable"). The four
  key/signature messages are a typed `files.KeyError`, and `tryResume` refuses on it
  ("option mismatch: the interrupted attempt's manifest ...") while a corrupt cache still
  starts over. `TestVerifyEndRejectsOtherSigner` has two subtests: signer in the ring
  ("signed by A, want B") and signer not in the ring. The PGP code is `pgp/`, not `files/pgp.go`.
- Task 6: `url.URL.Redacted` keeps the username (`ssh://u:xxxxx@h/x`), so the existing
  `TestRedactURI` cases that expected the user dropped were updated. The blunt fallback also
  applies when the URI parses but holds more than one `@`.
- Task 8: the poll interval is `AWS_S3_RESTORE_POLL_INTERVAL` (default 1m; tests use 10ms),
  since the wait loop slept a fixed minute. "Restored" means the `x-amz-restore` header is
  present and not `ongoing-request="true"` (the existing mock returns an empty header for
  done). The three tests are in `e2e_s3_test.go`; the fake S3 in `clean_s3_test.go` gained
  storage classes, `?restore` and PUT.
- Task 9: `TestRunSkipsOutages` built its range by hand; it now sets the exclusive `Until`.
- Task 11: `devcontainer-firewall-check` mounts the working tree's script over the image's
  copy and runs as root, so it needs no rebuild and no sudo. On this host the resolver seen in
  the container is Docker Desktop's `0.250.250.200`.
- Task 12: `TestE2EPlanResumePreviewsCompletion` removes a manifest and a volume at the
  second destination: plain `plan` then does not offer `complete-partial`, `plan --resume`
  does. `dev.nix` got the comment and a TODO; the hash is still unpinned (no nix here).
