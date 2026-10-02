# Plan: destructive-operation fixes (clean prefix, re-send guard, resume verification, pipeline hang)

Companion to [intent.md](intent.md). Written for an implementing agent. Line
numbers are as of commit `fe088b1` on `clean-dry-run`; re-check them before
editing. All code references are to the module `github.com/someone1/zfsbackup-go`
(the bosgood fork's working tree).

## Ground rules

- Work on branch `clean-dry-run` (current). One commit per task below; each
  commit must leave `make test-run PKG=./...` green. Run the full suite with
  `make test-docker` before declaring a task done (~3 min; the tree may be
  edited once `transferring context ... done` appears in its log).
- Fast loop: `make test-run PKG=./backup/ RUN=TestName` (host toolchain),
  `go test -run TestE2E ./` for the root-package e2e tests. Always run the
  whole root package (`make test-run PKG=.`) before trusting an e2e result:
  in-process cobra runs leak state (`RootCmd.SetArgs`, package-global flags)
  and `Execute` can `os.Exit(255)` the whole binary silently. After every
  in-process run: `RootCmd.SetArgs(nil); cmd.ResetSendJobInfo()` (or
  `ResetReceiveJobInfo()` for `clean`, which shares the receive JobInfo).
- Follow TDD: write the failing test first (it should reproduce the bug),
  then fix. Each task lists its test.
- Do not fix the other review findings (ENOSPC at flush, planner chain holes,
  truncated cache manifest, receive `-d -i`, timezone linking, unauthenticated
  restore, pipe-mode deadlock) unless the task says so. Note them in the
  commit message if you touch adjacent code.
- Salvage first: `/tmp/zfsb-verify/scratch_clean_s3_test.go` (fake S3
  server + in-process `clean --dry-run`) and
  `/tmp/zfsb-verify/scratch_verify_test.go` (`TestScratchUploadFailureHangs`,
  `TestScratchVolumeBoundariesReproducible`, `backupGoroutines()` stack
  dumper) are working reproductions. If the directory still exists, port them
  into the tree as the tests below before it is deleted. If it is gone, the
  fake-S3 server is reproduced in Task 1.

## Shared background

- Object names are produced by `files.JobInfo.ManifestObjectName()` and
  `BackupVolumeObjectName()` (`files/jobinfo.go:216-235`): a manifest is
  `<ManifestPrefix>|<volume>|<snap>[|<incr>].manifest[.gz][.pgp]`, a volume is
  `<volume>|<snap>[|<incr>].zstream[.gz][.pgp].volN`. `ManifestPrefix`
  defaults to `manifests` (flag `--manifestPrefix`, cmd/root.go). Separator
  is `|` (`--separator`).
- `backends.Backend` (`backends/backends.go:35`) has `Init, Upload, List,
PreDownload, Download, Delete, Close`. `List(ctx, prefix)` returns names
  relative to the destination. The four object-store backends each keep a
  `prefix` field set in `Init` as `strings.Join(uriParts[1:], "/")` and use
  it as `a.prefix + name` everywhere:
  - `backends/aws_s3_backend.go:106-114` (Init), `:238` Upload, `:277` Delete,
    `:297` PreDownload, `:359` Download, `:382/:395` List.
  - `backends/gcs_backend.go:76, 111, 126, 136, 150, 162`.
  - `backends/azure_backend.go:87, 139, 226, 238, 258, 266`.
  - `backends/backblaze_b2_backend.go:75, 114, 136, 146, 161, 164`.
  - `file://` and `ssh://` use filesystem paths and are unaffected.
- `backup.Clean` (`backup/clean.go:42`) builds its backend from
  `jobInfo.Destinations[0]` via `prepareBackend`, syncs the cache
  (`backup/sync.go:73 syncCache`), decodes manifests, `List(ctx, "")`s the
  whole destination, drops names with `HasPrefix(name, ManifestPrefix)`
  (`clean.go:124`), subtracts every volume named in a manifest, and deletes
  the remainder. Local-only cached manifests count as live unless
  `--cleanLocal`. `Clean` takes no lock. `TestCleanDryRun`
  (`backup/clean_test.go:39`) shows the `file://` + temp `config.WorkingDir`
  pattern.
- The cache directory is `<WorkingDir>/cache/<md5(destination URI string)>/`
  (`backup/sync.go:59 getCacheDir`, `backup/backup.go:470 saveManifest`,
  `backup/backup.go:640 tryResume`). It is keyed on the URI **string**, so
  `s3://b/p` and `s3://b/p/` have different caches today.
- `backup.Backup` (`backup/backup.go:236`): dry-run short-circuit, then
  `tryResume` (`:640-713`, reads only `Destinations[0]`'s cache), then the
  lock (`os.TempDir()/zfsbackup.<md5(volume)>.lck`), snapshot validation,
  then the pipeline: `sendStream` (`:511`) → `startCh` → forwarder goroutine
  (`:316-339`, `maniwg.Add(1)` per volume) → `stepCh` → one
  `retryUploadChainer` (`:715`) per destination (a `delete://` backend is
  appended when `MaxFileBuffer != 0`, `:348`) → manifest-writer goroutine
  (`:369-409`, `maniwg.Done()` at `:393` per volume, saves the partial manifest to the
  cache) → final-manifest goroutine (`:411-426`, `maniwg.Wait()` at `:414` with no
  context, then pushes the manifest into `stepCh`). `group.Wait()` at `:428`.
- `planSmartSnapshots` (`backup/plan.go:92`) is pure; the explicit-full
  branch (`:101-106`) ignores `destBackups`. `ProcessSmartOptions`
  (`backup/backup.go:58`) gathers `destBackups` via `getBackupsForTarget`
  (`:142`), which uses only manifests present at the destination.
- e2e harness: `e2e_test.go` (root package). `newE2EEnv(t)` gives a fake
  `zfs` (`internal/fakezfs`, knobs `FAKEZFS_SNAPSHOTS`, `FAKEZFS_STREAM_BYTES`,
  `FAKEZFS_LOG`), a temp working dir and a `file://` destination;
  `env.send(args...)` runs `send` in-process and returns logs+error;
  `newestBackup(t, volume, target)` reads the newest manifest back.
  Upload-failure injection for `file://`: create a regular FILE at
  `<dest>/<first path component of the volume name>` so the backend's
  `MkdirAll` fails while manifests (`manifests|...`) still upload.
  Hang detection: run `env.send` in a goroutine, `select` with a 30s timeout,
  on timeout dump `runtime.Stack(buf, true)` filtered to
  `zfsbackup-go/backup.` frames and `t.Fatal`.

## Task 1: object-store prefixes are directories, and `clean` only deletes what it can name

### 1a. Normalize the prefix in all four object-store backends

In each `Init`, after computing `prefix` from `uriParts[1:]`:

```go
prefix = strings.Trim(prefix, "/")   // collapse "p", "p/", "/p/", "p//"
if prefix != "" { prefix += "/" }
```

Put this in one helper in `backends/backends.go`, e.g.
`func objectPrefix(uriPath string) string`, and call it from S3, GCS, Azure
and B2. `s3://bucket` and `s3://bucket/` both give `""`.

Also add a helper `func stripPrefix(prefix, key string) (string, bool)` that
returns `(key[len(prefix):], true)` only when `HasPrefix(key, prefix)`, and
make every `List` skip (and `Debugf`) keys for which it returns `false`
instead of `strings.TrimPrefix`. With a `/`-terminated prefix `p/` cannot
match `photos/...`, and a key that does not start with the prefix cannot be
returned by S3 anyway, so this is belt-and-braces.

**Compatibility note (write it in the commit message and the runbook):**
a destination previously used _without_ a trailing slash wrote keys as
`pmanifests|...` and `ptank|...` (prefix concatenated with no separator).
After this change that URI lists under `p/` and sees nothing. Add to each
`Init` a one-time check: `List` with the _raw_ old prefix (no slash) and, if
it returns a key that starts with `<rawprefix><ManifestPrefix>` — pass the
manifest prefix through `BackendConfig` — return an error naming the object
and telling the user to move the objects to `<rawprefix>/`. Cheap (one
paged list call, only when the raw prefix is non-empty) and it prevents a
"where did my backups go" surprise. Do NOT try to be clever and fall back to
the old layout.

Cache key: `getCacheDir` hashes the URI string. Normalize the URI string
before hashing so both spellings share one cache. Add
`backends.CanonicalURI(uri string) string` that, for the four object-store
schemes, rewrites the path with the same `objectPrefix` rule
(`s3://b/p` → `s3://b/p/`, `s3://b` → `s3://b`), and leaves other schemes
untouched. Call it once at the CLI layer where destinations are parsed
(cmd/send.go `updateJobInfo`, cmd/clean.go, cmd/receive.go, cmd/list.go,
cmd/plan.go:120 and :160; grep `Destinations =` / `strings.Split(args[1], ",")`) so
`getCacheDir`, `saveManifest`, `tryResume` and `prepareBackend` all see the
canonical form. Users who always wrote the trailing slash keep their cache.

Tests (`backends/aws_s3_backend_test.go` has `mockS3Client` with
`ListObjectsV2WithContext`; extend it or add a table test):

- `TestS3PrefixNormalization`: `s3://b`, `s3://b/`, `s3://b/p`, `s3://b/p/`,
  `s3://b//p//` → `prefix` is `""`, `""`, `p/`, `p/`, `p/`; `Upload` key for
  volume `x` is `p/x`.
- `TestS3ListStripsPrefixOnly`: mock returns keys `p/a`, `p/b`; `List(ctx, "")`
  returns `a`, `b` with no leading slash.
- `TestS3LegacyLayoutRejected`: mock has `pmanifests|tank|s.manifest.gz`;
  `Init` of `s3://b/p` returns an error mentioning that key.
- Same normalization table for GCS/Azure/B2 (`Init` alone, no client call
  needed if the client is injected as in the existing tests).
- `TestCanonicalURI` in `backends/backends_test.go`.

### 1b. `clean` deletes only names it can prove are backup volumes under this destination

In `backup/clean.go`, after `List(ctx, "")` and the manifest filter:

1. Keep only objects that parse as a volume this tool wrote: add
   `files.ParseBackupVolumeObjectName(name, separator) (volume, base, incr
string, volNum int64, ok bool)` in `files/jobinfo.go` next to
   `BackupVolumeObjectName`, and use it. Anything that does not parse is
   logged at Notice as `Skipping unrecognized object %s (not a backup volume
written by this tool).` and never deleted. The manifest filter at
   `clean.go:124` stays but becomes `HasPrefix(name, ManifestPrefix +
Separator)`; a name with a leading `/` fails both tests and is skipped.
2. Refuse to run when the destination has objects but **no manifests at all**
   (remote or cached): `clean` has nothing to protect and is almost
   certainly pointed at the wrong URI. Return an error:
   `destination %s holds %d objects but no manifests; refusing to clean. If
this is intended, delete the objects manually.` No flag override: this
   state means the user's URI is wrong or the manifests are already gone,
   and in neither case should a bulk delete be one flag away.
3. Print, in dry-run and real runs alike, the count of objects skipped as
   unrecognized, so the user sees "clean found 3 objects it does not
   understand" instead of silence.

Tests:

- Port `/tmp/zfsb-verify/scratch_clean_s3_test.go` into the root package as
  `clean_s3_test.go`: an `httptest.Server` that answers ListObjectsV2 (XML
  below), env `AWS_S3_CUSTOM_ENDPOINT=<srv.URL>`, `AWS_REGION=us-east-1`,
  `AWS_ACCESS_KEY_ID=x`, `AWS_SECRET_ACCESS_KEY=y`, then in-process
  `RootCmd.SetArgs([]string{"clean", "--workingDirectory", t.TempDir(),
"--dry-run", uri})`. The server must also answer `GET /bucket/<key>` for
  the manifest download (`syncCache` downloads every remote manifest), so
  seed it with a real gz manifest produced by `files.CreateManifestVolume`
  or by a prior `file://` send copied into the map. Cases:
  - backups at `p/`, clean `s3://bucket/p` (no slash): 0 "Would delete"
    lines, both objects protected, exit 0.
  - backups at `media-photos/`, clean `s3://bucket/media`: the list request
    is for prefix `media/`, returns nothing, and `clean` errors with the
    "no manifests" refusal (after 1b.2), not a deletion.
  - a stray object `p/random.bin` with a valid manifest present: reported
    as unrecognized, not deleted.
  - backend returns `/manifests|...` (simulate a buggy backend with a mock
    `Backend` if `Clean` can take one; otherwise cover via
    `ParseBackupVolumeObjectName` unit tests): never deleted.

  ListObjectsV2 XML the fake needs (from the scratch test):

  ```
  <?xml version="1.0" encoding="UTF-8"?>
  <ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
    <Name>bucket</Name><Prefix>{prefix}</Prefix><KeyCount>{n}</KeyCount>
    <MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
    <Contents><Key>{key}</Key><Size>1</Size></Contents>...
  </ListBucketResult>
  ```

  Requests are path-style: `GET /bucket?list-type=2&prefix=...`. `Init`
  issues one list with `max-keys=0`; answer it with an empty result.

- `TestParseBackupVolumeObjectName` in `files/`: round-trips
  `BackupVolumeObjectName` for full and incremental, with `.gz`, `.pgp`,
  both, and neither; rejects `manifests|x.manifest.gz`, `random.bin`,
  `/tank|s.zstream.vol1`.
- Extend `backup/clean_test.go` with the "no manifests → refuse" case on
  `file://`.

### 1c. Docs

`docs/runbook-first-backup.md`: set `URI=s3://bucket/prefix/` in the flags
block with a sentence that a missing trailing slash is now equivalent, and
replace the paragraph at lines 96-97 with: plain `clean` never deletes
manifests or objects it does not recognize as this tool's volumes, refuses to
run against a destination with no manifests, and only `--force` deletes
whole broken sets. Add the legacy-layout note from 1a.

## Task 2: `send` refuses to overwrite an existing backup set

Two layers.

### 2a. Planner: explicit `--full` of an already-backed-up full is a no-op

`backup/plan.go:101-106`: before returning `PlanFull` for `jobInfo.Full`,
scan `destBackups`: if **every** destination already has a manifest whose
`BaseSnapshot.Equal(fullBase)` and `IncrementalSnapshot.Name == ""`, return
`Plan{Action: PlanNoop, Reason: reasonAlreadyBackedUp}` (new constant,
string `already-backed-up`). If only some destinations have it, return an
error `destinations are out of sync: %s already has a full of %s, %s does
not` — the same posture as the existing out-of-sync error. Update
`Plan.String()` if needed; `selectSmartSnapshots` already maps `PlanNoop` to
`ErrNoOp`, which `Execute` exits 0 with `Nothing new to back up.`

Add cases to `TestPlanSmartSnapshotsReasons` (`backup/plan_test.go:128`):
explicit full already at one destination; at both; at none. Add a
`--full` variation to an existing scenario in `backup/testdata/scenarios/`
if the scenario format supports per-run flag overrides (check
`plan_fixtures_test.go`); otherwise the table test is enough.

### 2b. Pipeline: refuse if the manifest object already exists at any destination

In `Backup` (`backup/backup.go`), after `prepareBackend` for each real
destination and before `sendStream` starts (move the backend-preparation
loop above the lock if needed, or add a pre-flight loop that prepares,
checks, and closes; simplest is to do the check inside the existing loop
before `retryUploadChainer` is wired, since nothing has been streamed yet):

```go
name := jobInfo.ManifestObjectName()
existing, err := backend.List(ctx, name)
// error → return it
if slices.Contains(existing, name) {
    return fmt.Errorf("backup set %s already exists at %s; refusing to overwrite. Delete the manifest and run clean to re-send", name, destination)
}
```

Skip the `delete://` backend. This covers manual `send tank@snap dest`,
`--full`, and `--resume` after the earlier attempt actually finished (its
cached partial manifest still exists, but the destination has the final
one). Note that `List(ctx, name)` is a prefix match: check equality on the
returned names, since `…manifest.gz` is not a prefix of anything else but
be explicit.

Do this check **before** `tryResume` overwrites `jobInfo.Volumes`, and
before the lock, so the error path never touches the cache. Concretely:
prepare backends first (they are needed for Task 3 too), run the existence
check, then `tryResume`, then lock. Keep `usedBackends` for the pipeline
rather than re-initializing.

Tests (root package, e2e):

- `TestE2ERefusesToOverwrite`: `send --full` a snapshot to `file://`,
  record the manifest and volume bytes, run the identical `send --full`
  again: non-zero exit, log contains `already exists`, every object at the
  destination is byte-identical to the recording, the cached manifest is
  unchanged.
- `TestE2EExplicitFullAlreadyBackedUpIsNoop`: with the smart flags
  (`--full --fullSnapshotSuffix _monthly`), second run exits 0 with
  `Nothing new to back up.` and uploads nothing (destination listing
  unchanged). This exercises 2a; 2b is the backstop for non-smart sends.
- Port `TestScratchVolumeBoundariesReproducible` as a **documented,
  skipped-by-default** test (`t.Skip("volume boundaries are not
reproducible; see docs/specs/2026-09-29--destructive-ops-fixes")`) so the
  root cause stays visible; making the split deterministic is out of scope.

## Task 3: `--resume` verifies skipped volumes at every destination

`tryResume` (`backup/backup.go:640`) currently: reads the cached partial
manifest for `Destinations[0]`, checks compressor/encrypt/sign/zfs command
line, sets `j.Volumes` and `j.StartTime`. Change it to take the prepared
backends (from Task 2b's reordering) and, after the option checks:

1. **Destination set must match.** Compare `originalManifest.Destinations`
   with `j.Destinations` as sets after dropping `delete://` from both (the
   cached manifest was saved after `Backup` appended it at `:348`). On
   mismatch return `cannot resume: original destinations %v != current %v.
Run without --resume to start over` — the new destination has none of
   the volumes and the safe answer is a fresh send.
2. **Every skipped volume must exist at every destination.** For each real
   destination, `List(ctx, j.BackupVolumeObjectName(0)` prefix without the
   `.volN` suffix)`once — i.e. list with prefix`<volume>|<snap>[|incr].zstream`— build a set, and find the longest prefix of`originalManifest.Volumes`(sorted by`VolumeNumber`, contiguous from 1, which is what
`TotalBytesStreamedAndVols`at`files/jobinfo.go`already assumes) whose`ObjectName`is present at all destinations. Truncate`j.Volumes`to
that prefix. Log at Notice:`Resuming from volume %d: %d of %d cached
   volumes verified at all destinations`and, when truncation happened,`volume %s missing at %s; it and later volumes will be re-sent`.
3. If the truncation leaves zero volumes, log `nothing verifiable to
resume; starting over` and proceed as a fresh send (this is what the
   user wants after a remote `clean` ate the orphans).

Because `TotalBytesStreamedAndVols` sums `ZFSStreamBytes` of the kept
volumes, the skip count follows automatically. Volumes at the destination
beyond the truncation point (from the old attempt) get overwritten by the
new attempt's same-numbered objects; none is referenced by a final manifest,
so that is acceptable, and `clean` removes any extras once the manifest
lands. Say so in a comment.

Keep the existing `--resume` busy-loop (short stream → `io.CopyN` EOF loop
at `backup.go:534-544`) out of scope but add `if written == 0 { return
fmt.Errorf("zfs stream ended before the %d bytes recorded in the cached
manifest were skipped; run without --resume") }` since it is a two-line
guard in the same function and the reviewer reproduced the spin. Test it.

Tests (root package, e2e; `FAKEZFS_STREAM_BYTES` large enough for ≥3
volumes with `--volsize 1`, check `newE2EEnv` for how it sets the size):

- `TestE2EResumeReSendsMissingVolumes`: run a send that fails after volume
  2 (inject the upload failure by creating the blocking FILE **after** two
  volumes exist: simplest is to run once successfully, then delete the
  final manifest from the destination, keep the cached partial manifest by
  copying the cache, and delete `…vol2` from the destination). Run `send
--resume`: exit 0, log says `volume …vol2 missing`, destination has all
  volumes, the final manifest's volumes all exist and their sizes match the
  objects. Then `receive` (there is an `env.receive` helper in the scratch
  test at `/tmp/zfsb-verify/scratch_verify_test.go:84`; port it) restores
  without error — this is the property that matters.
- `TestE2EResumeRejectsNewDestination`: cached manifest for `file://A`,
  resume with `file://A,file://B`: non-zero exit, `original destinations`
  in the log, nothing written to B.
- `TestE2EResumeShortStreamFails`: cached manifest records more bytes than
  `FAKEZFS_STREAM_BYTES` yields: non-zero exit within the 30s hang guard.

## Task 4: a failed send exits, releases the lock, and leaves consistent state

### 4a. Make every wait cancellable

`backup/backup.go`:

- Final-manifest goroutine (`:411-426`): replace `maniwg.Wait()` with

  ```go
  done := make(chan struct{})
  go func() { maniwg.Wait(); close(done) }()
  select {
  case <-done:
  case <-ctx.Done():
      return ctx.Err()
  }
  ```

  and make `stepCh <- manifestVol` a `select` on `ctx.Done()` too.

- `retryUploadChainer` worker (`:755`): `out <- vol` must be
  `select { case out <- vol: case <-ctx.Done(): return ctx.Err() }`. Also
  the `for vol := range in` loop should select on `ctx.Done()` between
  items (it already checks at the top of each iteration; fine).
- Forwarder goroutine (`:316-339`) already selects on `ctx.Done()`; its
  `maniwg.Add(1)` after a cancelled send is harmless once the waiter is
  cancellable.
- `sendStream` (`:511`): change `defer cout.Close()` (`:604`) in the `cmd.Wait()`
  goroutine to propagate the exit error: `err := cmd.Wait();
cout.CloseWithError(err); return err`. Then a `zfs send` that dies
  mid-stream surfaces as a read error in the splitter (which returns it)
  instead of an `io.EOF` that the splitter treats as a clean finish
  (`:576-588`) and pushes a truncated last volume down the pipeline.
  Also wrap the error: `fmt.Errorf("zfs send failed: %w: %s", err,
buf.String())` so stderr reaches the log once, not twice.
- After `group.Wait()` returns an error, `Backup` must still `Close` the
  backends (move the close loop into a `defer` right after `usedBackends`
  is populated) and the lock `defer` already runs.

### 4b. Do not leave a poisoned cache behind

On a failed send, the cached partial manifest is what `--resume` reads and
what `clean` treats as live. With Task 3 the resume path verifies it, and
with Task 2b a completed set is never overwritten, so the partial manifest
is safe to keep for resume. No cache change needed; state this in the
commit message so the reviewer's "partial manifest replaces the good cached
copy" scenario is explicitly closed by 2b, not by 4b.

Tests (root package):

- Port `TestScratchUploadFailureHangs` as `TestE2EUploadFailureExits`: with
  `--maxRetryTime 2s --maxBackoffTime 1s` (check flag names in
  `cmd/send.go:137-148`) and the blocking-FILE injection, `send` returns
  non-zero within 30s (use the goroutine + timeout + stack-dump guard),
  the lock file under `os.TempDir()` is gone, and no final manifest exists
  at the destination.
- `TestE2EZFSSendFailureExits`: add a `FAKEZFS_FAIL_AFTER_BYTES` knob to
  `internal/fakezfs` (write N bytes to stdout, print `cannot send: I/O
error` to stderr, exit 1). `send` exits non-zero within 30s, log contains
  the stderr text, no final manifest at the destination, no volume from
  the truncated tail is referenced by the cached partial manifest (compare
  `TotalBytesStreamedAndVols` against N). Then `send --resume` (Task 3
  code path) completes and `receive` succeeds when the fake is switched
  back to a full-length stream.
- Existing `TestE2EExitCodes` must stay green.

## Task 5: runbook and interim warnings

- Apply 1c.
- In section 5 (`--resume`), state what resume now verifies and that a
  resume with a different destination list is refused.
- Add a short "What `send` refuses to do" list: overwrite an existing set,
  resume against a changed destination list, skip volumes it cannot see.
- Remove the interim warnings once Tasks 1-4 are merged: "Don't run
  `clean`", "Always include the trailing slash", "Don't use `--full` on a
  monthly that's already backed up as a full". Until then they stand.
- Update `.claude/napkin.md` `clean` facts (they say plain `clean` "never
  deletes manifests"; qualify with the fixed prefix behaviour and the new
  refusals) and the memory note `adversarial-review-2026-09-29.md` marking
  which findings are fixed.

## Order and independence

Tasks 1a/1b, 2a, and 4a are independent and can be done in parallel
worktrees. 2b and 3 both reorder `Backup`'s prologue (prepare backends
before `tryResume` and the lock); do 2b first, then 3 on top. 4b is a
no-code note. 5 last. Suggested commit sequence: 1a, 1b+1c, 4a, 2a, 2b, 3, 5.

## Out of scope (tracked in the 2026-09-29 review memory)

Deterministic volume splitting; ENOSPC at final `bufw.Flush()`
(`files/volumeinfo.go:327` ignores the error — a one-line fix, but it
belongs with a test of its own); planner chain-hole walk; truncated cache
manifest re-download; `receive -d -i`; timezone-sensitive `linkManifests`;
manifest signature verification; pipe-mode (`--maxFileBuffer 0`) deadlock.
