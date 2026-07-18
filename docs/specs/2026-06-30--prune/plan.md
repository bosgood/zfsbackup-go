# `zfsbackup prune` subcommand

## Context

`zfsbackup` can `list` backup sets and `clean` orphaned objects (destination
objects not referenced by any manifest), but there is no way to retire *old*
backups based on age. We want a `prune` subcommand that deletes backup sets
older than a given age.

Per the intent, `prune` is deliberately narrow: it operates on **completed,
managed** backups by removing their **manifest** objects from the destination.
Once a manifest is gone, that backup set's data volumes are no longer referenced
by any manifest, so the existing `clean` command reclaims them. This keeps the
risky bulk-data deletion in one place (`clean`) and makes `prune` a small,
auditable "mark for deletion" step.

```shell
zfsbackup prune --olderThan 720h --dry-run   # preview what would be pruned
zfsbackup prune --olderThan 720h             # delete manifests older than 30d
zfsbackup clean <uri>                        # reclaim the now-orphaned volumes
```

## Existing pieces to reuse

The backend + cache machinery already exists in `backup/`:

- `prepareBackend` / `getCacheDir` / `syncCache` (`backup/sync.go`) — set up the
  backend and download all manifests into the local cache.
- `readManifest` (`backup/list.go:215`) — decode a cached manifest into a
  `*files.JobInfo`; `BaseSnapshot.CreationTime` is the age field to filter on.
- `backend.List(ctx, prefix)` / `backend.Delete(ctx, name)`
  (`backends/backends.go:35`) — list/delete by **real** destination object name.
- The errgroup-of-5 concurrent delete loop at the bottom of `Clean`
  (`backup/clean.go`) — the prune delete loop should mirror it.

### Mapping decoded manifest → real object name (key detail)

`syncCache` stores manifests in the cache under an md5 of the **real** object
name (`backup/sync.go:85`), and `JobInfo.ManifestPrefix`/keys are `json:"-"`
(not persisted), so a decoded manifest can't cheaply recompute its own object
name. Therefore prune deletes using the names from `backend.List`, mapping each
to its cached copy by md5 — not by recomputing `ManifestObjectName()`:

```go
manifestObjs, err := backend.List(ctx, jobInfo.ManifestPrefix) // real names
// (syncCache has already populated the cache for every one of these)
for _, objName := range manifestObjs {
    cacheName := fmt.Sprintf("%x", md5.Sum([]byte(objName)))
    decoded, _ := readManifest(ctx, filepath.Join(localCachePath, cacheName), jobInfo)
    if decoded.BaseSnapshot.CreationTime.Before(cutoff) {
        toDelete = append(toDelete, objName) // delete the real object name
    }
}
```

## Approach

### `cmd/prune.go` (new — mirror `cmd/clean.go`)

- Package-level `var pruneOlderThan time.Duration` and `var pruneDryRun bool`.
- `pruneCmd` with `Use: "prune [flags] uri"`, `PreRunE: validatePruneFlags`,
  `RunE` that sets `jobInfo.Destinations = []string{args[0]}` and calls
  `backup.Prune(cmd.Context(), &jobInfo, pruneOlderThan, pruneDryRun)`.
- `init()`:
  ```go
  RootCmd.AddCommand(pruneCmd)
  pruneCmd.Flags().DurationVar(&pruneOlderThan, "olderThan", 0,
      "Prune backup sets whose base snapshot is older than this duration (e.g. 720h). Required.")
  pruneCmd.Flags().BoolVarP(&pruneDryRun, "dry-run", "n", false,
      "Do not delete anything; only log what would be pruned.")
  ```
- `validatePruneFlags`: require exactly one positional arg (else `cmd.Usage()` +
  `errInvalidInput`, matching clean/list); require `pruneOlderThan > 0`; call
  `loadReceiveKeys()` (manifests may be encrypted/signed — needed to decode them,
  same as clean/list).
- Add a `ResetPruneJobInfo()` test helper mirroring `ResetListJobInfo`
  (`cmd/list.go:122`) if integration tests need it.

### `backup/prune.go` (new)

`func Prune(pctx context.Context, jobInfo *files.JobInfo, olderThan time.Duration, dryRun bool) error`

1. `prepareBackend` + `getCacheDir` + `syncCache` (same opening as `Clean`,
   `backup/clean.go:44-66`). Ignore the `localOnlyFiles` return — prune only
   acts on what's at the destination.
2. Compute `cutoff := time.Now().Add(-olderThan)`.
3. `manifestObjs, err := backend.List(ctx, jobInfo.ManifestPrefix)`.
4. For each real object name, read its cached decoded manifest (md5 mapping
   above) and select it when `decoded.BaseSnapshot.CreationTime.Before(cutoff)`.
   Log each selection with `Noticef` (visible at default level) including volume
   name + snapshot time.
5. **Dry-run:** log `"Dry-run: would prune %d backup set(s)."` plus a
   `"Would prune %s."` line per object, then `return nil` — short-circuiting
   before any delete, exactly like `Clean`'s dry-run block.
6. **Live:** delete the selected manifest objects via the same
   errgroup-of-5 + exponential-backoff loop used in `Clean`
   (`backup/clean.go`, bottom). Only manifest objects are deleted here.
7. On success, `Noticef` reminding the user to run `clean` to reclaim the
   now-orphaned data volumes (the data is not removed by prune itself).

### Documentation
- Update `README.md` usage section to list `prune` alongside `clean`/`list`,
  including the "run `clean` afterward to reclaim space" note.

## Design decision: manifests-only (per intent)

Prune deletes **only manifest objects**, then relies on `clean` to remove the
orphaned data volumes. Rationale: concentrates bulk data deletion in `clean`
(already audited, already has `--force`/dry-run), and keeps `prune` a small
reversible-until-clean step. The alternative (prune deletes data volumes too)
duplicates `clean`'s delete logic and is out of scope.

## Hazard to call out: incremental chains

Deleting a base/older manifest while a **newer incremental** that depends on it
still exists will, after `clean`, remove the base's data volumes and break the
incremental's restore chain. The age filter alone does not detect this.

Recommended for v1: keep the simple age filter but **log a warning** when a
pruned manifest is a parent of a retained manifest (reuse `linkManifests`
from `list.go:168` to detect parent/child relationships, or at minimum warn
that prune does not verify incremental dependencies). Honoring the chain
(refusing to prune a manifest that still has retained children unless
`--force`) can be a follow-up. Decide with the maintainer before implementing.

## Files to modify / add
- `cmd/prune.go` (new)
- `backup/prune.go` (new)
- `README.md` (usage docs)

## Verification
- `go build ./...` and `go vet ./...` succeed.
- `zfsbackup prune --help` shows `--olderThan` and `-n, --dry-run`.
- `zfsbackup prune --olderThan 720h` with no uri / with a bad duration fails
  validation with usage output.
- Against a `file://` test target seeded with manifests of varying snapshot
  ages: `prune --olderThan <d> -n` logs the correct "Would prune" set and leaves
  the destination unchanged (compare `backend.List` before/after); without `-n`
  the expected manifest objects are gone and a following `clean` removes the
  orphaned volumes.
- Add `backup/prune_test.go` using the `mockBackend` in `backup/backup_test.go`:
  assert `Delete` is called only for manifests older than the cutoff, and never
  when `dryRun` is true.
