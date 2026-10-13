# Integration tests: `clean` against real ZFS and MinIO

Proposal of 2026-10-12. Not executed. A `plan.md` follows when a scenario set is picked.

## What `clean` must do

`clean` deletes the backup volumes at ONE destination that no manifest there
lists. Those volumes are leftovers: a send that was cut, an upload that failed
after some volumes were stored, a chain whose manifests an operator removed.
After `clean`, the destination holds exactly the objects the manifests name,
plus everything `clean` does not own.

The effect, as a contract (`backup/clean.go`, `docs/context/clean.md`):

| It deletes                                                                                                                      | It never deletes                                                                            |
| ------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| an object that parses as a backup volume of a dataset that a manifest at this destination names, when no such manifest lists it | a manifest                                                                                  |
| with `--force`: a broken set (manifest and its remaining volumes) when a listed volume is gone                                  | an object that does not parse as a volume (`notes.txt`)                                     |
| with `--cleanLocal`: a cached manifest that is not at the destination, and the volumes it lists                                 | a volume of a dataset no manifest here names (another destination seen from above)          |
|                                                                                                                                 | anything under a nested destination (a sub-prefix with its own manifests)                   |
|                                                                                                                                 | a volume of a set that a manifest under another `--manifestPrefix` names                    |
|                                                                                                                                 | anything of a dataset whose send lock is held (a send runs on this host)                    |
|                                                                                                                                 | a cached manifest of another `--manifestPrefix`, or one it cannot decode (it stops instead) |
|                                                                                                                                 | anything when the destination has objects but 0 manifests under the prefix (it refuses)     |
|                                                                                                                                 | anything with `--dry-run`                                                                   |

Other facts a test can lean on:

- `clean` never runs `zfs`. The dataset need not exist on the host.
- `clean` takes one destination. A comma list is refused before it connects.
- `clean` has no `--separator` flag. A manifest written with another separator is still read as ours when it starts with the prefix.
- A manifest the run cannot read (no key, too long) stops the run with nothing deleted. Its cached copy stays.
- Deletes run on 5 workers with backoff. Cached copies of deleted manifests go only after the objects are gone.

## What the fourteen tests cover today

| Where                           | Already proved                                                                                                                                                               |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ListClean`                     | healthy destination = 0 deletes; one orphan deleted; `notes.txt` skipped; a broken set kept without `--force`, deleted with it, manifest included; restores after each clean |
| `Resume`                        | the volumes of a cut send are protected by the cached partial, orphans without that cache                                                                                    |
| `Rollover`                      | manifests of a retired chain deleted by hand, `clean` takes their volumes                                                                                                    |
| `ManifestPrefix`                | each prefix deletes its own orphans only; a set both prefixes name is spared; the default prefix with 0 manifests refuses                                                    |
| `NativeEncryption`, `ProdShape` | dry-run 0 on a consistent destination                                                                                                                                        |

Unit and e2e tests (`backup/clean*_test.go`, `clean_s3_test.go` on an httptest S3) cover
the lock, the bounded rounds, nested prefixes, separators, foreign and unreadable cached
manifests, a failed delete. None of that runs against a real S3 listing or real zfs streams.

## Proposed scenarios

Each is a `TestIntegrationS3Clean*` test, its own bucket, the shared helpers
(`itClean`, `objectNames`, `putObject`, `manifestFor`, `requireVolumes`,
`flakyZFS`). Ranked by the size of the gap.

### 1. `--cleanLocal` abandons a cut send (new flag coverage)

No integration test runs `--cleanLocal`. It is the only clean mode that deletes
something a `--resume` would use.

Setup: a complete full of `src@first`. Then the `flakyZFS` wrapper cuts a send
of `src@daily` after 2 volumes (as `Resume` does). The cache holds a partial
manifest; S3 holds 2 orphan-looking volumes.

Steps and expected effect:

1. `clean` (no flags): Notice "There are 1 local manifests not found in the destination, use --cleanLocal"; 0 deletes; the 2 volumes stay.
2. `clean --force`: still 0 deletes. Notice "--force leaves it alone, --cleanLocal deletes it". `--force` is about broken sets AT the destination.
3. `clean --cleanLocal --dry-run`: "Would delete local manifest <path>", "Dry-run: would delete 2 objects"; S3 and the cache dir unchanged (compare listings and `cache/*/*` before and after).
4. `clean --cleanLocal`: the 2 volumes gone, the cache file gone, the full of `first` intact (`requireVolumes`).
5. `clean --dry-run`: 0 deletes. A normal `send -i first daily` then gives a complete set; `receive --auto` with an empty working dir restores both snapshots, GUIDs equal.

### 2. An encrypted destination without the key deletes nothing (safety)

`EncryptedChain` proves `list` and `receive` with the wrong key. `clean` is the
one command that writes, and it was never run on an encrypted destination.

Setup: the GPG chain of `EncryptedChain` (encrypted and signed manifests). Plant
one orphan volume of the newest set.

1. `clean` with no rings: an error that names the manifest and says it is encrypted to a key this run lacks; exit non-zero; S3 unchanged; the orphan stays; the cached copy of the manifest stays (`keepsCachedCopy`).
2. `clean --force` with no rings: the same. `--force` must not turn "cannot read" into "broken set".
3. `clean` with another key's rings: the same refusal.
4. `clean` with the right rings: the orphan goes, every manifest's volumes stay, `receive --auto` with the rings and an empty working dir restores the chain.

### 3. Prefixes are directories: nested and sibling destinations (real listing)

`TestCleanS3Prefixes` proves this on an httptest fake. MinIO's real
`ListObjectsV2` semantics (prefix, pagination, keys with `|` and `@`) are what
production sees.

Setup in one bucket: `s3://b/run/` holds a full of `src`; `s3://b/run/sub/`
holds a full of the same `src`; `s3://b/other/` holds a full of `src2`. Plant
one orphan of `src` under each of the three prefixes.

1. `clean s3://b/run/`: deletes the orphan under `run/` only. Notice "sub/ holds its own manifests, so it is another destination" for every `sub/` object. Nothing under `other/` is listed at all.
2. `clean s3://b/run/sub/`: deletes its orphan only.
3. `clean s3://b/` (the bucket root): 0 manifests under the prefix at the root → refuses, deletes nothing.
4. `clean s3://b/run/,s3://b/other/`: refused before it connects ("clean takes one destination").
5. Every manifest's volumes present at the end; `receive` from `run/` and from `other/` works.

### 4. A chain sent with another `--separator`

`send --separator '#'` writes `manifests#src#snap.manifest.gz` and
`src#snap.zstream.gz.vol1`. `clean` has no separator flag and runs with the
default `|`.

1. `clean --dry-run` reads those manifests as ours (`isManifestObject`): 0 deletes, and NO "Skipping unrecognized object" for the volumes.
2. Plant an orphan with `#` and one with `|` of the same dataset. `clean` deletes both: both separators are in use.
3. The `#` manifests are not deleted; restore works.

### 5. More than 1000 objects (listing pagination, the delete pool)

`docs/specs/2026-10-12--integration-test-large/intent.md` lists "S3 listings
past 1000 keys" as out of scope. `clean` is the command where a short listing
is dangerous twice: a volume the listing misses is "missing" (a broken set),
and an orphan it misses stays.

Setup: a full of `src` with `--volsize 1 --compressor ""` (3 volumes). Plant
1100 orphan volumes of the same set (`vol100`..`vol1199`, a few bytes each;
`putObject` on MinIO does this in seconds).

1. `clean --dry-run`: "would delete 1100 objects"; no "missing volume" warning (the listing saw all 3 real volumes).
2. `clean`: the 1100 gone, the 3 volumes and the manifest left; `list` shows one backup; `receive` works.
3. Optional: `cmd.SignalContext` plus SIGTERM while the deletes run; the run stops with an error; a second `clean` finishes; the real set is never touched.

### 6. Repair after `--force` at one of two destinations

`TwoDestinations` ends with "repairing d2 is the operator's job (clean --force
at d2, then send)". That path is not run.

Setup: `send` of `m1` then `m2` to `d1,d2`. Delete one volume of `m2` at `d2`.

1. `clean --force d2`: the `m2` set and its manifest are gone at `d2`; the cached copy of that manifest is gone too (else the next send would believe `d2` holds `m2`).
2. `send -i m1 m2 d2`: a new complete `m2` set at `d2`.
3. Smart `send` to `d1,d2`: a noop, both in sync. `receive --auto` from `d2` with an empty working dir restores `m1` and `m2`.

### Optional, smaller value

- **7. Busy dataset.** The unit test holds the lock file with a live pid. The integration version adds only a real listing. In-process `send` and `clean` at once share cobra globals, so it must hold the lock as the unit test does (`<work>/locks/<md5(dataset)>.lck`, pid of `os.Getppid()`), then: Notice "A send of X appears to be running", X's orphan and cached partial stay, another dataset's orphan goes.
- **8. Legacy key layout.** For `s3://b/p/`, an older version wrote keys `p<dataset>|...`. Plant one. Expect the Warning "written by an older version outside of" and the object kept. `e2e_old_destination_test.go` covers it with the fake.
- **9. A denied delete.** A MinIO user without `s3:DeleteObject` (needs `mc admin` in the compose file). `clean --cleanLocal` must fail, keep the local manifest, and a later run with rights must finish. `TestCleanLocalKeepsManifestWhenDeleteFails` covers it on the fake.
- **10. A versioned bucket.** `clean` on a bucket with versioning leaves delete markers; `list`, `clean --dry-run` (0) and `receive` must behave as on an unversioned bucket.

## Suggested grouping

| Test                              | Scenarios | Approx. time on the VM         |
| --------------------------------- | --------- | ------------------------------ |
| `TestIntegrationS3CleanLocal`     | 1, 6      | 20 s (two cut sends)           |
| `TestIntegrationS3CleanEncrypted` | 2         | 10 s                           |
| `TestIntegrationS3CleanLayout`    | 3, 4, 8   | 10 s                           |
| `TestIntegrationS3CleanMany`      | 5         | 20 s (1100 PUTs, 1100 DELETEs) |

Every scenario ends with a restore into an empty working directory, as the
other tests do, so "clean kept what the manifests need" is proved by zfs, not
by a listing.

## Out of scope

The bounded read rounds ("the manifests keep changing"), an unreadable cached
manifest (needs a hand-made file; the unit tests do it), Azure and GCS.
