# Integration tests, round two: harden the six, add the production shape

Date: 2026-10-11
Branch: `clean-dry-run`
Source: `intent.md` in this directory. Audit of `../2026-10-11--integration-test-inc/plan.md`.

## Goal

Harden the six `TestIntegrationS3*` tests (task 1) and add seven more (tasks 2 to 8).
Every test is a `TestIntegrationS3*` function, so the entrypoint's default `RUN`
runs all of them.

Encryption coverage after this plan: plaintext volumes (`TestIntegrationS3`,
`Chain`, `Bookmark`, `ListClean`, `Resume`, `Rollover`, `ProdShape`, ...), GPG
volumes (`EncryptedChain`, `EncryptedResume`), and ZFS native encryption
(`NativeEncryption`: raw, plaintext from an encrypted dataset, raw + GPG).
Production (`backup3/enc/...`) is native-encrypted; task 8 mirrors it.

One rule becomes code (task 9): **a chain keeps one stream kind.** An
incremental must carry the same `--raw` as the backup of its source at the
destination. A full starts a new chain and may change the kind. Decided
2026-10-11: no mixed-heritage chains.

| Task | Test                                | Covers                                                                                                                           |
| ---- | ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| 1    | the six existing tests              | restores from an empty cache; volume bytes are PGP; `guid` match; resume with pgzip; sharper `list` filters                      |
| 2    | `TestIntegrationS3Rollover`         | a window-elapsed full; restore picks the new chain; the old chain still restores; a receiver on the old chain                    |
| 3    | `TestIntegrationS3ProdShape`        | production flags, 3-way ties, a 5-part dataset name, `--snapshotPrefix`, `--fullPath`, `--lastPath`                              |
| 4    | `TestIntegrationS3EncryptedResume`  | resume with rings: re-sent volumes get new ciphertext and new hashes; a tampered volume                                          |
| 5    | `TestIntegrationS3ReceiveSafety`    | receive without `-F` onto a changed dataset; `--properties` + `--unmounted`; `--origin`                                          |
| 6    | `TestIntegrationS3TwoDestinations`  | the upload chain to two S3 prefixes; `complete-partial`; destinations out of sync                                                |
| 7    | `TestIntegrationS3ManifestPrefix`   | two `--manifestPrefix` jobs in one bucket: `list`, `clean` spares the other prefix                                               |
| 8    | `TestIntegrationS3NativeEncryption` | ZFS native encryption: `--raw` chain + bookmark, key not loaded, restore needs the key, plaintext send, raw + GPG, a forgotten `--raw` is refused |
| 9    | raw chain consistency (code)        | `send` refuses an incremental whose `--raw` differs from its source backup's; `plan` agrees; `receive --auto` refuses a mixed chain |
| 10   | docs                                | README table, `testing.md`, context docs, runbook, napkin, CHANGELOG                                                             |

## Facts that shape the plan

Checked on 2026-10-11 in the code. Do not re-derive them.

1. **A restore reads the manifest from the local cache.** `syncCache`
   (`backup/sync.go:132`) lists the manifests at the destination and downloads
   only those whose `md5(name)` is NOT a file in the cache. It never re-reads a
   cached name. `readCachedManifest` (`sync.go:294`) then reads the cache. So a
   receive that shares `--workingDirectory` with the send reads what the send
   wrote locally. Volumes always come from the destination. To prove a restore
   from the destination alone, receive with `t.TempDir()` as `work`.
2. **A received snapshot keeps the source's `guid`.** `zfs get -H -p -o value
guid <ds>@<snap>` on both sides gives the same value when the stream
   arrived intact. `zfs.GetSnapshotGUID` wraps it; the tests can call `zfs get`
   directly through `zfsRun`. A received snapshot also keeps the creation
   time: `TestIntegrationS3Chain` step 3 only passes because of it.
3. **Volume bytes.** With `--encryptTo` and `--signFrom`, `files/volumeinfo.go`
   calls `openpgp.Encrypt` (`golang.org/x/crypto/openpgp`, vendored). The
   first packet of the object is a public-key encrypted session key:
   `packet.NewReader(r).Next()` returns a `*packet.EncryptedKey`. Signed-only
   volumes start with a one-pass signature packet instead. The manifest is
   the same: `.manifest.gz.pgp`.
4. **Ties.** `snapshot()` (`integration_s3_test.go`) waits for a new second on
   purpose. `zfs snapshot ds@a ds@b ds@c` in ONE call takes them atomically,
   with one creation second: that is how sanoid's monthly, daily and hourly tie
   on the 1st. `zfs list -S creation` orders ties as it likes.
   `TestTieOrderIndependence` covers the planner; nothing covers the full path
   on real zfs.
5. **The planner reads only manifests and snapshots.** `planSmartSnapshots`
   (`backup/plan.go`) is pure. `ageExceeded = newest snapshot time − last full
time > --fullIfOlderThan`; a full rolls when a `_monthly` newer than the
   destination's last backup exists. So one run with `--fullIfOlderThan 1s`
   among runs with `4320h` makes the window elapse for real, on snapshots
   seconds apart. The flag may change between runs.
6. **`snapshotMatches`** (`backup/backup.go:158`) needs BOTH the prefix and the
   suffix. A manual snapshot named `pre-upgrade_monthly` matches the suffix and
   not `--snapshotPrefix autosnap_`: with the prefix it is skipped, without it
   it is the newest `_monthly`.
7. **Receive naming.** `--fullPath` (`-d`) restores `<pool>/plain/user/app/navidrome`
   into `<target>/plain/user/app/navidrome`; `--lastPath` (`-e`) into
   `<target>/navidrome` (`AutoRestore`, `backup/restore.go`, and
   `GetZFSReceiveCommand`). `AutoRestore` lists the COMPUTED dataset to find
   local snapshots. `--unmounted` is `-u`; `--origin X` is `-o origin=X`;
   `--force` is `-F`. `--force` leaks between in-process runs: pass
   `--force=false` where the test needs no `-F`.
8. **`backupThatApplies`** (`restore.go:224`) prefers an incremental whose
   chain reaches a local snapshot, else the full. Its comment: zfs refuses a
   full stream into a dataset that has snapshots. After a rollover, the
   backups of m4 are one incremental from m3, and the backup of m3 is a full.
9. **Two destinations.** `send` takes `uri1,uri2` as one argument. The upload
   is a chain: destination 2 gets a volume after destination 1 uploaded it
   (`docs/context/send.md`). A set at some destinations only is
   `partialSet`; `PartialSetCompletable` (`backup/backup.go:100`) lists the
   lagging destination for every volume name. All present → the smart plan
   is `complete-partial` and `Backup` uploads the manifest only. One missing →
   the planner goes on; with equal last fulls and a newer `_monthly` it
   returns the error `want to do an incremental backup but last incremental
backup at destinations do not match`. With nothing newer it is a NOOP: to
   hit the error, take a new `_monthly` first.
10. **`--manifestPrefix`** is a root flag (default `manifests`). `clean` deletes
    only volumes of sets that a manifest under ITS prefix names and spares any
    set a manifest under another prefix names by name (`otherPrefixSets`,
    `docs/context/clean.md` rule 7). Objects but no manifests under the prefix
    → `clean` refuses (rule 5). The cache holds every prefix's manifests.
11. **`list --before X`** keeps manifests whose `BaseSnapshot.CreationTime` is
    strictly before X; `--after` strictly after (`backup/list.go:89,93`).
    Format `2006-01-02T15:04:05`, local zone (UTC in the container).
12. **Resume with pgzip.** The counter cuts a volume at `--volsize` of
    COMPRESSED bytes and pgzip holds several MiB in its blocks (napkin). Random
    data does not compress, but a 2.5 MiB cut of the RAW stream may leave no
    complete volume. Use a 16 MiB file and a 12 MiB cut, and assert
    `Resuming from volume N` with N ≥ 2, not an exact N.
13. **Environment.** The pool is a 2 GiB file. `destroyAfter` destroys in the
    order given: clones and children first (`-r`). The entrypoint's
    `-test.timeout` is 45m. Each itBucket gets its own bucket (MinIO) or
    prefix (real S3). A VM run stalls when the Mac sleeps: rerun before you
    debug.
14. **`--raw` is a `send` flag only** (`cmd/send.go:88`, `jobInfo.Raw` →
    `zfs send -w` in `GetZFSSendCommand`). `receive` has no raw flag: zfs
    detects a raw stream. Nothing in `backup/` reads `Raw` after the send.
    The manifest records it (no `json:"-"`); object names do not mark it. The
    smart planner never checks that the links of a chain agree on `--raw`:
    a run that forgets `--raw` sends a plaintext incremental on top of a raw
    full, and the program says nothing.
15. **An encrypted dataset without a prompt:** `zfs create -o
encryption=aes-256-gcm -o keyformat=raw -o keylocation=file:///<path>
<pool>/enc`, with 32 random bytes in the file (mode 0600). Children made
    with `zfs create -p` inherit the key; `<pool>/enc` is the encryption root,
    as `backup3/enc` is in production. `zfs load-key -r <pool>/enc` reads the
    file again. `zfs unload-key -r <pool>/enc` needs every dataset of the
    tree unmounted first (`zfs unmount <ds>`, deepest first). With the key
    unloaded: `zfs list` and `zfs send -w` work; `zfs send` without `-w`
    fails. The container is privileged and root; no tty is needed.
16. **A raw receive** makes the target its own encryption root with the
    sender's key; `keystatus` is `unavailable` and `zfs mount` fails until
    `zfs load-key`. `guid` and `creation` are readable without the key, so
    `wantSnapshots` and `wantGUIDs` work before the key is loaded;
    `compareDirs` needs `load-key` + `mount`. Pass `--unmounted` on raw
    receives so no mount is attempted.
17. **Where a flipped `--raw` is caught today, and where not.** `sameStream`
    (`backup/backup.go:706`) compares the whole `zfs send` command line
    (`-w` included) of a cached or copied manifest with the current run: a
    `--resume` or a `complete-partial` with the other flag fails with
    `option mismatch` (`TestE2EResumeCompleteRefusesOtherVariant`). Nothing
    compares a NEW incremental with the backup it continues from:
    `planSmartSnapshots` returns an incremental at two sites
    (`reasonExplicitIncremental`, `reasonNewerCandidate`) without looking at
    `destBackups[idx][0].Raw`, and an explicit `-i` send (`cmd/send.go:250`)
    never reads the destination's manifests at all (`refuseExistingSet` lists
    one object name). `AutoRestore` walks `ParentSnap` without comparing
    `Raw` between a link and its parent. The planner's doc says it reads only
    the smart options and `VolumeName` from `jobInfo`; task 9 adds `Raw`.
18. **Planner fixtures.** `Scenario.parseManifest` (`backup/plan_fixtures.go:388`)
    reads `<base> [epoch]` or `<source> [epoch] to <base> [epoch]` per line;
    no other words. `Scenario.run` records the manifest a step sends with
    `addManifest`; the flags file is parsed with the same `pflag` set as
    `send` (`loadFlags`, ~line 221; `AddSmartFlags` is shared), so `--raw`
    must be registered there for `plan` to see it. The fake `zfs send`
    (`internal/fakezfs/fakezfs.go:177`) accepts `-w` and ignores it, so e2e
    tests can send `--raw` without real zfs.

## Assumptions to verify on the VM

Each is a behavior of real zfs the plan relies on. When one fails, do not
weaken the test: record what happened at the end of this file, keep the test
red, and report. It is a finding about the program, not about the test.

- **A1.** `zfs receive -F` of a FULL stream into a dataset that has snapshots
  fails with `destination has snapshots` and destroys nothing. Task 2 step R3.
- **A2.** `zfs receive -d` creates the intermediate datasets (`create_parents`
  in libzfs) when the target dataset exists. Task 3. If it does not on zfs
  2.4.4, `zfs create -p` the parents in the test and note it.
- **A3.** `zfs receive` without `-F` into a dataset changed after its last
  snapshot fails with `destination has been modified` and keeps the change.
  Task 5 step S1.
- **A4.** A full stream sent with `-p` carries the local `mountpoint`. Task 5
  step S2 records what the receive does with it; the `--unmounted` variant
  must not mount anything.
- **A5.** `zfs receive -o origin=<src>@m1` of an incremental m1→m2 into a new
  dataset makes a clone of `<src>@m1` with snapshot m2. Task 5 step S3.
- **A6.** A raw receive keeps `keyformat=raw`, and `zfs load-key -L
file:///<path> <restored>` opens the received dataset with the sender's key
  file (`-L` overrides the recorded `keylocation`). Task 8 step N4.
- **A7.** Retired. Task 9 refuses a mixed chain in `send` and in `receive
  --auto`, so zfs never sees a plaintext incremental on a raw chain. Task 8
  step N6 asserts the refusal instead.
- **A8.** A plaintext stream received under `<pool>/enc` inherits `encryption`
  and `encryptionroot` from the parent. Task 8 step N7.

## Rules for this repo

Read `.claude/napkin.md` first. The rules that matter here:

- Build and test through the Makefile. `make integration-image` compiles and
  vets the tagged test without the VM: the fast loop. `make integration-vm
RUN=TestIntegrationS3Rollover` runs one test. `make test-docker`, `make
fmt-check` (after `git add`) and `make lint` before "done".
- Find code with `semble search` (concepts) and `cs` (symbols), as described
  in `.claude/skills/codesearch/SKILL.md`. Do not use grep to locate code.
- In-process cobra runs leak flag values. `itRunOut` resets; still pass bool
  flags explicitly where a value matters (`--force=false`, `--dry-run=false`).
- Two sends that must give identical volumes, or any volume-count assertion:
  `--compressor ""`. Only the pgzip subtest of task 1 uses the default.
- A test that passes the first time proves nothing. Each task names the break
  that must make it fail. Run each break in a scratch copy (`rsync -a
--exclude .git --exclude tmp ./ /tmp/zb-break/`, `make -C /tmp/zb-break
integration-vm RUN=...`). Record the proofs in the commit message.
- Never shape production code around a test environment's limit. Task 9 is
  the ONE production change in this plan. Everything else: an assumption that
  fails is reported, not fixed here.
- Assert on ZFS state and bucket state before log lines. Log assertions only
  where the behavior is not visible elsewhere.
- New tests go in new files, `//go:build integration`, `package main`.
  Another session may edit this tree: `git status` before you stage, stage by
  name, never `git add -A`.
- One `CHANGELOG.md` line per task a user can see, `* [ADD] test: ...`.
- Keep `snapshot()`'s global `lastSnapshot` discipline: tests run in series,
  never `t.Parallel()`.

## Layout

```
integration_s3_test.go               helpers added (task 1): wantGUIDs, snapshotTie, wantPGPVolume, objectBytes
integration_s3_chain_test.go         task 1 edits (fresh-cache restores, PGP check, GUIDs)
integration_s3_bookmark_test.go      task 1 edits
integration_s3_ops_test.go           task 1 edits (second dataset, sharp --before)
integration_s3_resume_test.go        task 1 edits (Gzip subtest)
integration_s3_rollover_test.go      TestIntegrationS3Rollover (task 2)
integration_s3_prodshape_test.go     TestIntegrationS3ProdShape (task 3)
integration_s3_encresume_test.go     TestIntegrationS3EncryptedResume (task 4)
integration_s3_safety_test.go        TestIntegrationS3ReceiveSafety (task 5)
integration_s3_twodest_test.go       TestIntegrationS3TwoDestinations (task 6)
integration_s3_prefix_test.go        TestIntegrationS3ManifestPrefix (task 7)
integration_s3_native_test.go        TestIntegrationS3NativeEncryption (task 8)
backup/plan.go                       task 9: sameStreamKind check at both incremental return sites
backup/backup.go                     task 9: refuseMixedChain for explicit -i sends
backup/restore.go                    task 9: AutoRestore refuses a chain whose links differ in Raw
backup/plan_fixtures.go              task 9: `raw` word on a manifests line; --raw in the flags file
backup/plan_test.go                  task 9: unit tests of the planner check
backup/testdata/scenarios/raw-chain-kept/, raw-chain-flag-forgotten/   task 9 goldens
e2e_raw_test.go                      task 9: fake-zfs e2e of send, explicit -i, and receive --auto
docs/context/send.md, receive.md, plan.md, docs/runbook-first-backup.md   task 10
integration/README.md                test table (task 10)
docs/context/testing.md              layer 4 text (task 10)
.claude/napkin.md, CHANGELOG.md      (each task)
```

## Task 1: harden the six

Files: `integration_s3_test.go` and the five test files.

New helpers in `integration_s3_test.go`:

- `wantGUIDs(t, src, restored string, snaps ...string)`: for each name,
  `zfsRun("get", "-H", "-p", "-o", "value", "guid", ds+"@"+name)` on both
  datasets must be equal. Call it right after every `wantSnapshots` of a
  restored dataset, in every test, with the snapshots that dataset has.
- `snapshotTie(t, dataset string, kinds ...string) []string`: one timestamp
  (after the same wait as `snapshot`), one `zfs snapshot` call with one
  `dataset@autosnap_<ts>_<kind>` per kind. Returns the names in the order
  given. Task 3 uses it; add it here so the helper file changes once.
- `objectBytes(t, client, bucket, key) []byte`: `GetObject` and read all.
- `wantPGPVolume(t, data []byte)`: `packet.NewReader(bytes.NewReader(data)).Next()`
  must return a `*packet.EncryptedKey`; any other type or an error fails.

Edits:

1. **Fresh-cache restores.** In `TestIntegrationS3Chain` the "whole chain into
   an empty dataset" step, in `TestIntegrationS3EncryptedChain` the
   `restoredEnc` step (with rings), in `TestIntegrationS3Bookmark` the
   `restoredBm` step, and in `TestIntegrationS3Resume` the `restore(restoredRes2)`
   step: pass `t.TempDir()` as `work`. `restore` in the resume test takes a
   `work` parameter for that.
2. **Volume bytes are PGP.** In `TestIntegrationS3EncryptedChain`, after the
   name check: `wantPGPVolume(objectBytes(prefix + manifestFor(daily3).Volumes[0].ObjectName))`
   and the same for the manifest object of daily3 (the one name under the
   prefix that holds `daily3` and `.manifest`).
3. **GUIDs.** `wantGUIDs` after every `wantSnapshots` of a restored dataset.
   For clones compare with `c.src` (the clone shares the snapshot GUIDs of
   its origin; still compare against `c.src`, not the clone).
4. **Resume with pgzip.** `t.Run("Gzip", ...)` at the end of
   `TestIntegrationS3Resume`: its own bucket, dataset `srcresgz`, `work`,
   `flakyZFS(t, 12<<20, 5)`, `a.bin` 16 MiB, args `--volsize 1 src@snap
target` (default compressor). Steps: arm, the send fails with `flaky zfs`;
   the cached partial for `snap` lists N ≥ 1 volumes and the bucket got the
   same N names (reuse the existing comparison); `--resume` succeeds and logs
   `Resuming from volume <N+1>: <N> of` (build the string from N); fresh-work
   `receive -F`; `wantSnapshots`, `wantGUIDs`, `compareDirs`.
5. **Sharper list.** In `TestIntegrationS3ListClean`, before the table: create
   `<pool>/srcops2`, one 64 KiB file, one `monthly` snapshot, one smart send
   (same flags, same target). Rows: `nil` → 2 keys, `srcops` chain as before
   and `srcops2` one full; `--volumeName c.src` → 1 key; `--volumeName
<pool>/s*` → 2 keys; `--before <creation of c.daily3>` → `srcops` holds
   monthly and daily2 only (fact 11; take the time with `zfs get -H -p -o
value creation` and format it in UTC). Keep the `hourLater` rows. The rest
   of the test keeps working: `clean` and the restores look at `c.src` only;
   `wantObjects` lists must include `srcops2`'s objects (take `healthy` after
   the second dataset's send).
6. **Resume stall.** Add one comment line to `flakyZFS`'s doc: the 5 s stall
   is a timing guess; a stalled VM fails the "same 2 volumes" assertion.
   Rerun before you debug. No code change.

Break-the-test proofs:

- Edit 1: after `buildChain`, `deleteObject` the daily3 manifest at S3 and run
  the fresh-cache restore: it must fail. Then the SAME restore with the shared
  `work`: it must succeed. That pair is the hole this edit closes; record both.
- Edit 2: send one link with `--encryptTo ""` (signed only). `wantPGPVolume`
  must fail on a one-pass signature packet.
- Edit 3: `zfs snapshot restored@<name>` of a NEW snapshot with a chain name
  on the restored side (destroy the received one first): `wantGUIDs` must fail.
- Edit 4: run the Gzip resume without `--resume`: no `Resuming from volume`.
- Edit 5: drop the `srcops2` send: the `nil → 2 keys` row must fail.

## Task 2: a window-elapsed full

File: `integration_s3_rollover_test.go`. Dataset `<pool>/srcroll`. Flags
`base := --fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly
--volsize 1 --compressor ""`, plus `--fullIfOlderThan 4320h` (`long`) or
`--fullIfOlderThan 1s` (`short`) per send (fact 5).

1. `a.bin` 3 MiB; m1 = `monthly`, then `hourly`. Send `long`: full of m1
   (`manifestFor`: no source).
2. `b.bin` 1 MiB; m2 = `monthly`, `hourly`. Send `long`: m2 from m1.
3. `c.bin` 1 MiB; m3 = `monthly`, `hourly`. Send **`short`**: a FULL of m3
   (`window-elapsed`). Assert no source. The log holds `window-elapsed`
   (`Smart backup plan:` is Info; `itRunOut` sets `--logLevel debug`).
4. `d.bin` 1 MiB; m4 = `monthly`, `hourly`. Send `long`: m4 from m3.
5. `itList`: m1<-, m2<-m1, m3<-, m4<-m3.
6. **R1, new chain:** fresh work, `receive --auto -F srcroll target
restoredNew`. `wantSnapshots(m3, m4)`; `wantGUIDs`; `compareDirs(/srcroll)`.
7. **R2, old chain:** fresh work, `receive --auto -F srcroll@m2 target
restoredOld`. `wantSnapshots(m1, m2)`; `wantGUIDs`; compare with a clone
   of `srcroll@m2` (`cloneOf`, registered in `destroyAfter` before `srcroll`).
8. **R3, a receiver on the old chain:** `receive --auto -F srcroll@m4 target
restoredOld`. Expected (A1): an error; `wantSnapshots(restoredOld, m1,
m2)` unchanged; the log holds `destination has snapshots`. Record the
   exact message in the notes: it is zfs's, and a clearer one from `receive`
   is a candidate follow-up, not part of this plan.
9. **Noop:** send `long` again → `ErrNoOp`.
10. **Old chain retired:** `deleteObject` the m1 and m2 manifests at S3. With a
    FRESH work (the cache of `work` still holds them, napkin: clean treats a
    cached copy as live), `itClean(--dry-run)` logs `would delete <n>` where
    n = the volume count of m1 + m2 (sum of `len(Volumes)` from `manifestFor`
    taken BEFORE the delete); then `itClean()`; `objectNames` = the m3 and m4
    sets exactly. R1 again into `restoredNew2` from a fresh work: m3, m4.

Break-the-test proofs:

- Step 3 with `long`: m3 becomes an incremental from m2 and step 6 gets four
  snapshots.
- Step 8: expect success. Must fail (A1). If A1 does not hold, see Assumptions.

## Task 3: the production shape

File: `integration_s3_prodshape_test.go`. Dataset `ds := <pool>/plain/user/app/navidrome`
(`zfs create -p`; the mountpoint inherits `/<pool>/plain/user/app/navidrome`).
`destroyAfter(<pool>/restoredD, <pool>/restoredE, <pool>/plain)`; `-r` takes
the children. (`<pool>/enc` is task 8's encrypted root; this dataset is plain.) Flags: `--snapshotPrefix autosnap_ --fullIfOlderThan 4320h
--fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly --volsize 1
--compressor ""`.

1. `a.bin` 3 MiB. `M1 := snapshotTie(ds, "monthly", "daily", "hourly")[0]`
   (one call, one second). Two more `snapshotTie(ds, "daily", "hourly")` and
   one `snapshot(ds, "hourly")`. Send: full of M1.
2. `b.bin` 1 MiB. `M2` tie of three; a `daily`+`hourly` tie. Send: M2 from M1.
3. **A manual snapshot with the suffix and not the prefix:**
   `zfsRun("snapshot", ds+"@pre-upgrade_monthly")`. Send: `ErrNoOp`
   (`nothing-newer`). That is fact 6.
4. `c.bin` 1 MiB. `M3` tie of three. Send: M3 from M2. `itList`: three
   entries, the `pre-upgrade` name in none of them.
5. **`--fullPath`:** `zfs create <pool>/restoredD`. Fresh work, `receive --auto
-F --fullPath ds target <pool>/restoredD`. Expected dataset
   `<pool>/restoredD/plain/user/app/navidrome` (fact 7, A2):
   `wantSnapshots(M1, M2, M3)`; `wantGUIDs`; `compareDirs(/<pool>/plain/user/app/navidrome, /<pool>/restoredD/plain/user/app/navidrome)`.
6. **`--lastPath`:** `zfs create <pool>/restoredE`. Fresh work, `receive --auto
-F --lastPath ds target <pool>/restoredE`. Dataset `<pool>/restoredE/navidrome`;
   the same three checks.
7. **Second run onto the `-d` target:** `d.bin` 1 MiB; `M4` tie; send; then
   step 5's command again (same fresh work is fine). `wantSnapshots(M1..M4)`:
   `AutoRestore` listed the computed name and sent one step.
8. `objectNames`: every key holds `plain/user/app/navidrome` (the `/` of the
   dataset name is in the object key) and `clean --dry-run` from a fresh work
   logs `would delete 0 objects`.

Break-the-test proofs:

- Step 3 without `--snapshotPrefix`: the send is an incremental of
  `pre-upgrade_monthly` from M2, not a noop.
- Step 1 with `snapshot()` three times instead of `snapshotTie`: the test
  still passes. Then this is NOT a proof: instead assert in step 1 that the
  three names share one `creation` (`zfs get -H -p -o value creation`), so
  the tie is real. That assertion is the proof that the test exercises a tie.

## Task 4: resume with encryption

File: `integration_s3_encresume_test.go`. Dataset `<pool>/srcencres`;
`rings := writeRings(t, newKey(t))`; `cut := 5 << 19`, `flakyZFS(t, cut, 5)`.
Args: `--volsize 1 --compressor "" <rings...> src@snap target`.

1. A small full of `first` (64 KiB) with rings: the complete set `clean`
   needs (as `TestIntegrationS3Resume` does).
2. `a.bin` 4 MiB; `snap`. Arm, send: fails with `flaky zfs`. The cached
   partial lists 2 volumes; the bucket got the same 2 names. `wantPGPVolume`
   on the first of them.
3. `--resume`: logs `Resuming from volume 3: 2 of`. `m := manifestFor(snap)`.
   `wantPGPVolume` on `m.Volumes[2]`. Fresh work, `receive -F` with rings into
   `restoredER`; `wantSnapshots`, `wantGUIDs`, `compareDirs`.
4. **New ciphertext, new hash:** record `h1 := m.Volumes[0].SHA256Sum` (read
   the field name in `files.VolumeInfo`). `deleteObject` volume 1 and the
   manifest. `--resume`: logs `Volume <vol1> missing at` and `Resuming from
volume 1: 0 of`. `m2 := manifestFor(snap)`: `m2.Volumes[0]` hash ≠ `h1`
   (a new session key). Fresh work, `receive -F` with rings into
   `restoredER2`: the restore uses the new manifest. `wantGUIDs`, `compareDirs`.
5. **A tampered volume:** `objectBytes` of `m2.Volumes[1]`, flip one byte in
   the middle, `putObject` it back. Fresh work, `receive -F` with rings into
   `restoredER3`: must fail; `wantNoDataset(restoredER3)`. The log names the
   volume. Put the original bytes back; the same receive succeeds.
6. **A bookmark source with rings:** `zfs bookmark src@snap src#snap`,
   `zfs destroy src@snap`; `e.bin` 1 MiB; `snap2 := snapshot(src, "daily")`;
   `send ... -i src#snap src@snap2`; `manifestFor(snap2).IncrementalSnapshot.Bookmark`
   is true. Fresh work, `receive --auto -F src target restoredER4` with rings:
   `wantSnapshots(snap, snap2)`; `wantGUIDs` for both (the sender has a
   bookmark of `snap`: compare `snap`'s GUID with `zfs get guid src#snap`).

Break-the-test proofs:

- Step 4: assert the hash is EQUAL. Must fail.
- Step 5: skip the flip. The "must fail" assertion fails.

## Task 5: receive safety

File: `integration_s3_safety_test.go`. Dataset `<pool>/srcsafe` with
`-o mountpoint=/<pool>/srcsafe`. Snapshots m1 (`monthly`), m2 (`daily`).
Explicit sends: `--volsize 1 --compressor "" srcsafe@m1` (full) and
`-i srcsafe@m1 srcsafe@m2`.

**S1, no `-F` onto a changed dataset:**

1. `receive --force=false srcsafe@m1 target restoredS`. Write
   `/<pool>/restoredS/local.txt`.
2. `receive --force=false srcsafe@m2 target restoredS`: an error (A3);
   `wantSnapshots(restoredS, m1)`; `local.txt` still there.
3. `receive -F srcsafe@m2 target restoredS`: `wantSnapshots(m1, m2)`;
   `local.txt` is GONE. Assert that: it documents what `-F` costs.

**S2, `--properties` and the mountpoint:**

4. Second bucket. `send --properties --volsize 1 --compressor ""
srcsafe@m1 target2`. Write `/<pool>/srcsafe/after.bin` (live data newer
   than m1, present in no snapshot).
5. `receive --unmounted -F srcsafe@m1 target2 restoredP`: `zfs get -H -o value
mounted restoredP` is `no`; `/<pool>/srcsafe/after.bin` exists. Log
   `zfs get mountpoint restoredP` in the notes (A4).
6. `t.Run("Mounted", ...)`: `receive -F srcsafe@m1 target2 restoredP2`
   without `--unmounted`. Assert `/<pool>/srcsafe/after.bin` still exists.
   Record `mountpoint` and `mounted` of `restoredP2`. If `after.bin` is
   hidden, the restored dataset mounted over the live one: keep the test red
   and report (Assumptions).
   Cleanup: `zfs unmount` / `destroy -r -f` handles an overmount; if
   `destroyAfter` fails here, `zfs set mountpoint=none` first in a
   `t.Cleanup` registered BEFORE `destroyAfter`.

**S3, `--origin`:**

7. `receive --origin srcsafe@m1 -F srcsafe@m2 target restoredO`
   (A5). `zfs get -H -o value origin restoredO` is `srcsafe@m1`;
   `wantSnapshots(restoredO, m2)`; `wantGUIDs(m2)`; `compareDirs(/srcsafe
minus after.bin)`: compare with a clone of `srcsafe@m2` instead. Register
   `restoredO` in `destroyAfter` BEFORE `srcsafe` (a clone holds its origin).

Break-the-test proofs:

- S1 step 2 with `-F`: the error assertion fails.
- S2 step 5 without `--unmounted` and with the assertion `mounted == no`:
  must fail (or A4 does not hold; record).

## Task 6: two destinations

File: `integration_s3_twodest_test.go`. One `itBucket`; `d1 := target + "d1"`,
`d2 := target + "d2"` (with a real-S3 prefix the join is `<prefix>d1`; use a
`/` between when `prefix == ""`). `both := d1 + "," + d2`. Dataset
`<pool>/srctwo`. Flags: production suffixes (`_monthly`/`_monthly`),
`--fullIfOlderThan 4320h --volsize 1 --compressor ""`.

1. `a.bin` 3 MiB; M1. Send to `both`. `objectNames(d1) == objectNames(d2)`
   (compare the lists with the `d1/`, `d2/` prefix stripped), 1 manifest,
   3+ volumes.
2. `b.bin` 1 MiB; M2. Send: M2 from M1 at both. Lists equal.
3. **`complete-partial`:** `deleteObject` the M2 manifest at d2. Send to
   `both`: the log holds `complete-partial`; the lists are equal again; the
   volume objects at d2 kept their `LastModified` (take it with `HeadObject`
   before and after: a re-upload changes it).
4. **Out of sync:** `deleteObject` the M2 manifest AND `Volumes[0]` at d2.
   `c.bin`; M3. Send to `both`: an error whose text holds `last incremental
backup at destinations do not match` (fact 9). No new object at either
   destination.
5. **Each destination restores on its own:** fresh work, `receive --auto -F
srctwo d1 restoredT1` → M1, M2. Fresh work, `receive --auto -F srctwo d2
restoredT2` → M1 only (d2 has no complete M2 set). `wantGUIDs` for both.
6. **Repair:** send to `both` with `--resume`? No: `--resume` completes a
   partial whose volumes are present; here one is gone. Out of scope; note
   it in the test as the operator's job (`clean --force` at d2, then send).

Break-the-test proofs:

- Step 3 with a volume deleted too: `complete-partial` does not appear.
- Step 4 without the M3 snapshot: the send is a NOOP (fact 9), so the
  error assertion fails. Keep the snapshot; this proves the order matters.

## Task 7: two manifest prefixes in one bucket

File: `integration_s3_prefix_test.go`. One bucket. Dataset `<pool>/srcpfx`,
M1 (`monthly`), M2 (`daily`). `jobA := --manifestPrefix jobA`, `jobB :=
--manifestPrefix jobB`. Explicit sends.

1. Send `srcpfx@M1` with `jobA`; send `srcpfx@M1` with `jobB`; send `-i
srcpfx@M1 srcpfx@M2` with `jobA` only. `objectNames`: 3 manifests (two
   under `jobA`, one under `jobB`); the volume names start with the prefix
   (read how `BackupVolumeObjectName` builds them; assert on
   `manifestFor`'s names, not on a guess).
2. `itList(jobA)`: M1<-, M2<-M1. `itList(jobB)`: M1<- only.
   `itList` without a prefix (default `manifests`): fails or 0 keys; record
   which (objects but no manifests).
3. `itClean(--dry-run, jobA)`: `would delete 0 objects`. Same for `jobB`.
4. Orphans: `putObject` a `vol77` of jobA's M2 set and a `vol77` of jobB's M1
   set (names from `manifestFor` with the prefix flag; `manifestFor` must take
   the prefix: extend it with `extra ...string` passed to `backupsAt`'s
   `ManifestPrefix`, or add `manifestForPrefix`). `itClean(jobA)`: jobA's
   orphan is gone, jobB's orphan and every jobB volume stay. `itClean(jobB)`:
   jobB's orphan is gone; jobA's sets untouched.
5. Fresh work, `receive --auto -F srcpfx target restoredB` with `jobB`:
   M1 only. With `jobA` into `restoredA`: M1, M2. `wantGUIDs`.

Break-the-test proofs:

- Step 4: `itClean` with no prefix flag: the run must FAIL (fact 10, rule 5),
  and delete nothing.
- Step 4: remove the `jobB` orphan assertion's "stay" check and delete jobB's
  orphan by hand first: the check must fail. Proves it reads the bucket.

## Task 8: ZFS native encryption

File: `integration_s3_native_test.go`. This mirrors production:
`backup3/enc` is an encryption root and `navidrome` a descendant.

Setup:

- `keyFile`: 32 random bytes in `t.TempDir()`, mode 0600.
- `root := <pool>/enc`: `zfs create -o encryption=aes-256-gcm -o keyformat=raw
-o keylocation=file://<keyFile> root`. `ds := root + "/user/app/navidrome"`
  with `zfs create -p`. Mountpoint inherits `/<pool>/enc/user/app/navidrome`.
- `destroyAfter(<restored datasets...>, root)`. `destroy -r -f` works with
  keys unloaded. Register clones before `root`.
- Helper `loadAndMount(t, dataset)`: `zfs load-key -L file://<keyFile>
dataset`, then `zfs mount dataset`.
- Helper `prop(t, dataset, name) string`: `zfs get -H -o value <name> <dataset>`.
- Three buckets: `target` (raw chain), `target2` (plaintext), `target3`
  (raw + GPG). Smart flags: `smart := --fullIfOlderThan 4320h
--fullSnapshotSuffix _monthly --incrementalSnapshotSuffix _monthly --volsize
1 --compressor ""`. `raw := append(smart, "--raw")`.

**N1, raw full.** `a.bin` 3 MiB; `M1 := snapshotTie(ds, "monthly", "daily",
"hourly")[0]`. Send `raw ds target`. `manifestFor(M1).Raw == true`; no source.

**N2, the key is not loaded on the backup host** (a reboot before the cron
run). `b.bin` 1 MiB; `M2` tie. `zfs unmount ds`, `zfs unmount root`, `zfs
unload-key -r root`; `prop(root, "keystatus") == "unavailable"`. Send `raw ds
target`: succeeds, M2 from M1, `Raw == true`. Then `send --volsize 1
--compressor "" -i ds@M1 ds@M2 target2` (no `--raw`): must FAIL; `target2`
holds 0 manifests. Then `zfs load-key -r root`, `zfs mount root`, `zfs mount
ds`.

**N3, a raw restore needs no key to land, and opens nothing without it.**
Fresh work, `receive --auto -F --unmounted ds target restoredRaw`.
`wantSnapshots(restoredRaw, M1, M2)`; `wantGUIDs(ds, restoredRaw, M1, M2)`;
`prop(restoredRaw, "keystatus") == "unavailable"`; `prop(restoredRaw,
"encryptionroot") == restoredRaw`; `zfs mount restoredRaw` FAILS (run it with
`exec.Command`, expect an error). That is the proof the objects at S3 are ZFS
ciphertext: without the key nothing opens.

**N4, with the key.** `loadAndMount(restoredRaw)` (A6).
`compareDirs(/<ds>, /<pool>/restoredRaw)`.

**N5, raw incremental from a bookmark.** `zfs bookmark ds@M2 ds#M2`, `zfs
destroy ds@M2`. `c.bin` 1 MiB; `M3` tie. Send `raw ds target`: M3 from M2,
`IncrementalSnapshot.Bookmark == true`, `Raw == true`. Fresh work, `receive
--auto -F --unmounted ds target restoredRaw` (it is mounted and has the key;
one step). `wantSnapshots(M1, M2, M3)`; `wantGUIDs` (for M2 compare with `zfs
get guid ds#M2`); `compareDirs`.

**N6, a forgotten `--raw` is refused** (task 9; do task 9 first). `d.bin`
1 MiB; `M4` tie. Send `smart ds target` WITHOUT `--raw`: must FAIL with an
error that holds `mix raw and plaintext`; `objectNames(target)` unchanged
(nothing uploaded); `itList(target)[ds]` still has three entries, all `Raw ==
true`. The explicit form too: `send --volsize 1 --compressor "" -i ds@M3
ds@M4 target` (no `--raw`) must fail the same way, nothing uploaded. Then send
`raw ds target`: M4 from M3, `Raw == true`. Fresh work, `receive --auto -F
--unmounted ds target restoredRaw`: `wantSnapshots(M1..M4)`, `wantGUIDs(M4)`,
`compareDirs`. A new chain may change kind: `send --full` of M4 without
`--raw` to a fourth bucket succeeds with `Raw == false` (one assertion; no
restore needed).

**N7, plaintext send of the encrypted dataset** (key loaded). `send --volsize
1 --compressor "" ds@M1 target2`: `Raw == false`. Fresh work: `receive -F ds@M1
target2 <pool>/restoredPlain`: `prop(restoredPlain, "encryption") == "off"`;
`compareDirs` with a clone of `ds@M1` (`cloneOf`; a clone of an encrypted
dataset mounts because the key is loaded). Fresh work: `receive -F ds@M1
target2 <pool>/enc/restoredIn` (A8): `prop(..., "encryption") ==
"aes-256-gcm"`, `prop(..., "encryptionroot") == root`; `compareDirs` with the
same clone.

**N8, raw + GPG.** `rings := writeRings(t, newKey(t))`. `send --raw <rings>
--volsize 1 --compressor "" ds@M3 target3`. `wantPGPVolume` on
`manifestFor(target3, M3).Volumes[0]` and on the manifest object. Fresh work:
`receive -F --unmounted ds@M3 target3 restoredBoth` WITHOUT rings: fails,
`wantNoDataset`. With rings: `wantSnapshots(M3)`; `prop(keystatus) ==
"unavailable"`; `loadAndMount`; `compareDirs` with a clone of `ds@M3`.

**N9.** Fresh work, `itClean(--dry-run, target)`: `would delete 0 objects`.

Break-the-test proofs:

- N1 without `--raw`: N3's `zfs mount restoredRaw` succeeds (a plaintext full
  lands as an unencrypted dataset) and the "must fail" assertion fails.
- N2: `zfs load-key -r root` before the plaintext send: it succeeds and the
  "must FAIL" assertion fails.
- N6: run it against a tree without task 9 (`git stash` the task 9 commit in
  the scratch copy): the plaintext send succeeds and the "must FAIL"
  assertion fails.
- N8: receive with another key's rings. Must fail.

## Task 9: a chain keeps one stream kind (production change)

Files: `backup/plan.go`, `backup/backup.go`, `backup/restore.go`,
`backup/plan_fixtures.go`, tests and goldens as in Layout.

Rule: an incremental backup must have the same `Raw` as the backup of its
source at the destination. A full is a new chain: no check. Only `Raw` is
compared; `-p`, `-R`, `-D` change bytes, not the stream's heritage.

The error, one wording in every place, with a stable substring the tests
assert on: `... would mix raw and plaintext streams ...`. Shape:

```
cannot continue the chain of <volume>: the backup of <source> was sent
<with --raw|without --raw>, this run is <without --raw|with --raw>; it would
mix raw and plaintext streams. Run with <the source's setting>, or start a
new chain with --full.
```

1. **Planner** (`backup/plan.go`). A helper `sameStreamKind(jobInfo
   *files.JobInfo, source *files.JobInfo) error` that returns the error above
   when `source.Raw != jobInfo.Raw`. Call it before BOTH incremental returns
   (`reasonExplicitIncremental`: source manifest `destBackups[0][0]`;
   `reasonNewerCandidate`: the same, it is `lastBackup[0]`'s manifest). Check
   every destination that has backups, not only index 0: the planner already
   errors when their last backups differ, so one loop over `destBackups` is
   enough. `complete-partial` needs no check: `copyManifest` → `sameStream`
   covers it (fact 17). Update the doc comment: the planner reads the smart
   options, `Raw`, and `VolumeName`.
2. **Explicit `-i` sends** (`backup/backup.go`). `refuseMixedChain(ctx,
   jobInfo, dests)`: when `jobInfo.IncrementalSnapshot.Name != ""`, for each
   destination read its backups with `getBackupsForTarget` and find the
   manifest whose `BaseSnapshot.Equal(&jobInfo.IncrementalSnapshot)` (name
   and creation time; a bookmark keeps the snapshot's creation time). Found
   and `Raw` differs → the error above. Not found at a destination → `Infof`
   "no backup of <source> at <dest>; the stream kind cannot be checked there"
   and go on: the user may have sent the full elsewhere. Call it in `Backup`
   right after the `validateSnapShotExists` of the incremental snapshot
   (which fills its creation time), before the pipeline. Smart sends run it
   too: the manifests are in the cache by then, so it is one listing. Note
   in the function's doc that `--dry-run` returns before the lock and does
   not run it.
3. **`receive --auto`** (`backup/restore.go`). In the `ParentSnap` walk of
   `AutoRestore`, before `jobToRestore = jobToRestore.ParentSnap`: if
   `jobToRestore.Raw != jobToRestore.ParentSnap.Raw`, `Errorf` and return an
   error that holds `mix raw and plaintext` and names both backups. Nothing
   is received before the walk ends, so no partial state.
4. **Fixtures.** `parseManifest`: a trailing word `raw` on a line sets
   `Raw: true` (`<base> raw`, `<source> to <base> raw`); any other extra word
   stays an error. `loadFlags`: register `--raw` on the fixture flag set
   (bound to `jobInfo.Raw`) so a flags file and `plan --raw` work.
   `Scenario.run`: the manifest it records carries `Raw: jobInfo.Raw`.
5. **Unit tests** (`backup/plan_test.go`): last backup raw + run plain →
   error; last backup plain + run raw → error; raw + raw → incremental; a
   `window-elapsed` full with the other flag → full, no error; two
   destinations, one empty → no panic, the check uses the one with backups.
6. **Goldens.** `raw-chain-kept`: manifests `m1 raw`, `m1 to m2 raw`, flags
   `--raw --fullIfOlderThan 4320h --fullSnapshotSuffix _monthly
   --incrementalSnapshotSuffix _monthly`, snapshots with m3 → `INCR m3 from
   m2`. `raw-chain-flag-forgotten`: the same without `--raw` → the error
   line. `make scenarios-update`, review the two new `expected.txt`.
7. **e2e** (`e2e_raw_test.go`, fake zfs, `file://`): `send --raw --full`
   of a; then smart send without `--raw` with b newer → error holds `mix raw
   and plaintext`, the destination has one manifest; explicit `-i tank/data@a
   tank/data@b` without `--raw` → the same; with `--raw` → b from a, two
   manifests. `send --full` of b without `--raw` → a third manifest, `Raw
   false` (a new chain). `receive --auto`: build a mixed destination by hand
   with the manifest-writing helper of `TestE2EReceiveRejectsSubstitutedManifest`
   (a raw full of a, a plaintext incremental a→b) and assert `receive --auto
   tank/data` fails with `mix raw and plaintext` and `FAKEZFS_RECEIVE_LOG`
   recorded no receive. `TestE2ESequenceMatchesPlanner` must still pass:
   `plan` and `send` give the same error.
8. `make test-docker`, `make scenarios`, `make lint`, `make fmt-check`.

Break-the-test proofs (for a production change the proofs are the tests
failing on the OLD code): run the unit test, the goldens and the e2e against
a scratch copy without the change. Every "must fail" case must pass there
(the mixed send succeeds). Record it in the commit message.

## Task 10: docs and napkin

Files: `integration/README.md`, `docs/context/testing.md`, `.claude/napkin.md`,
`docs/runbook-first-backup.md`.

- README: the table grows to thirteen tests, one line each, `RUN=` name. Note
  the assumptions A1 to A8 and what the VM showed.
- `testing.md`, layer 4 and the `integration-docker` row: "thirteen
  `TestIntegrationS3*` tests" and one sentence per group (plain, GPG, native).
- Runbook: one short section on native encryption: pass `--raw` on EVERY
  run of a raw chain; a run without it is refused (N6, task 9); a new chain
  starts with `--full`; a raw restore needs `zfs load-key` with the sender's
  key (N4).
- `docs/context/send.md`: the planner table gets the row "incremental whose
  `--raw` differs from its source's backup → error (mixed chain)"; the
  `planSmartSnapshots` text says it reads `Raw` too. `receive.md`: step 5 of
  `AutoRestore` refuses a chain whose links differ in `Raw`. `plan.md`: the
  `raw` word in manifests files and `--raw` in flags.
- Napkin: one line each for what the VM taught (A1 to A8 outcomes), and:
  "A test that restores with the sender's `work` proves nothing about the
  manifest at the destination." Replace the napkin line that says nothing
  checks `--raw` across a chain.

## Order and commits

One commit per task, 1 to 10. Run them in this order: 1, 9, 2, 3, 4, 5, 6,
7, 8, 10. Task 9 is independent of the VM (`make test-docker` and `make
scenarios` prove it) and task 8 step N6 needs it. Tasks 2 to 8 depend on
task 1's helpers.
Do task 1 first and run ALL six tests in the VM before task 2. Then one task
at a time, each run in the VM before the next. `make integration-image` is
the compile check between VM runs.

## Done when

- `make integration-vm` on this Mac passes all thirteen tests, twice in a row,
  or a task's test is red ONLY because an assumption (A1 to A8) did not hold,
  and the notes below say which and show the zfs output.
- Every break-the-test proof failed as described and was undone; the commit
  messages say so.
- Task 9's unit tests, goldens and e2e fail on the old code and pass on the
  new; `make scenarios` is green with the two new goldens.
- `make test-docker`, `make fmt-check` (after `git add`) and `make lint` pass.
- The CHANGELOG lines (task 9 gets `[CHANGE] send: ...` and `[CHANGE]
  receive: ...`) and the docs of task 10 are in.

## Out of scope, noted for later

- A clearer `receive` error when a full stream meets a dataset with snapshots
  (task 2 step R3). Report; do not add it here.
- `receive` defaulting to `-u`, or a warning, when the stream carries
  properties (task 5 step S2). Report; do not add it here.
- The same one-kind rule for `-p`, `-R`, `-D` and `--compressor zfs` (`-c`).
  Task 9 covers `Raw` only. A follow-up may generalize `sameStreamKind` to
  the whole `zfs send` flag set, as `sameStream` does for resume.
- `--dry-run` of an explicit `-i` send does not run `refuseMixedChain` (it
  returns before the lock and the backends).
- Native encryption with `keyformat=passphrase` (a prompt; the test uses
  `raw` + a key file), `zfs change-key` between links, and a raw receive into
  a target whose parent is encrypted with ANOTHER key.
- Replaying the 2330-snapshot fixture onto the pool and comparing `send
--dry-run` with `plan --snapshots`. Medium value; ~1 min of `zfs snapshot`.
- `--intermediary` (`-I`) chains and `--replication` (`-R`) with child datasets.
- Repair of out-of-sync destinations (task 6 step 6).

## Execution notes

(Fill in as you go: the outcome of A1 to A5 with zfs output, where the code
differs from the plan, and the break-proof results.)

### Task 1 (2026-10-11)

All six pass in the VM (zfs 2.4.4, MinIO); the Gzip resume cut left 3 volumes.

- `manifestFor` reads through `itList` now and takes `extra ...string` (rings,
  `--manifestPrefix`): `backupsAt` has no keys and opens no encrypted manifest.
- `wantGUIDs` compares the bookmark `src#name` when `src@name` is gone
  (`TestIntegrationS3Bookmark`: daily2 and daily3 are bookmarks on src).
- `wantPGPVolume` takes the object name too, for the message.
- `cutPartial` (resume file) holds the cached-partial vs. bucket comparison;
  the plain test keeps its stream-bytes check and `len == 2`.
- **Fact 1 is half right.** With the daily3 manifest DELETED at S3, the
  shared-`work` restore fails too (`could not find snapshot provided`):
  receive takes manifest NAMES from the destination listing. Only the CONTENT
  of a name already cached comes from the cache. Proof used instead: the
  daily3 manifest OVERWRITTEN with garbage at S3. Fresh work → `could not read
  the manifest ... unexpected EOF`; shared work → success.

Break proofs (scratch copy, all failed as planned): garbage manifest (above);
signed-only volumes → `starts with a *packet.OnePassSignature`; a new
`restoredAll@daily3` → guid mismatch; Gzip without `--resume` → no `Resuming
from volume 4: 3 of`; no `srcops2` send → the `nil`, `s*` and `--before`
rows fail.

### Task 9 (2026-10-11)

Done in the order 1, 9, 2-8 as the revised plan says. Where the code differs:

- One helper, `MixedChainError(volume, source, raw)` (`backup/plan.go`), holds
  the wording for the planner and for `refuseMixedChain`. `AutoRestore` has
  its own sentence (it names both backups and both settings); both hold
  `mix raw and plaintext`.
- `--raw` (`-w`) is registered in `ParseSmartFlags` and on `planCmd`, NOT in
  `AddSmartFlags`: `send` registers `--raw` itself, so a shared registration
  would define it twice.
- `manifestFor(volume, plan)` keeps its signature (eight callers);
  `Scenario.run` sets `Raw` on the manifest it records.
- `TestE2ENextRunScenarios` replays each golden's manifests with real sends;
  it now passes `--raw` for a `raw` line (it failed on the two new goldens).
- e2e: `TestE2EChainKeepsStreamKind` (smart, explicit `-i`, a plaintext full
  starts a new chain) and `TestE2EReceiveAutoRefusesMixedChain` (the mixed
  chain made with `reforgeManifest`).

Proofs on the old behavior (scratch copies, the three checks off, fixtures
kept): the unit test fails 4 ways, `raw-chain-flag-forgotten` differs from its
golden, both e2e tests fail (the mixed send and the mixed receive succeed).
With only `refuseMixedChain` off, the explicit `-i` case fails by itself.
