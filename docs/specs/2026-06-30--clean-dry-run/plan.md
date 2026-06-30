# Dry-run support for `zfsbackup clean`

## Context

`zfsbackup clean` deletes objects in a destination that are not referenced by
any manifest, and (with flags) deletes local cache files and broken backup sets.
These deletions are irreversible and there is currently no way to preview them.
We want a `-n` / `--dry-run` flag that reports exactly what *would* be deleted
without performing any mutation, so users can verify before committing.

## Mutating operations to guard

All live in `backup/clean.go` inside `Clean(...)`:

1. **`os.Remove(manifestPath)` — clean.go:102** — deletes local-only manifests
   when `--cleanLocal` is set.
2. **`os.Remove(manifestPath)` — clean.go:164** — deletes the locally cached
   manifest copy of a broken backup set in the `--force` path.
3. **`backend.Delete(ctx, objectPath)` — clean.go:214** — deletes destination
   objects (the primary, remote-data-destroying operation).

Not guarded (intentionally): the temp-manifest `CreateManifestVolume` /
`Close` / `DeleteVolume` dance at clean.go:150-161. `DeleteVolume`
(`files/volumeinfo.go:254`) only removes a local *scratch* file written to the
temp dir; it's needed to compute the broken set's object name so it can be added
to the delete list. It must keep running in dry-run so the preview is accurate.

## Approach

Follow the existing standalone-flag pattern (mirrors `cleanLocal`), not a
`JobInfo` field, since dry-run is clean-specific.

### `cmd/clean.go`
- Add `var dryRun bool` next to `var cleanLocal bool` (line 29).
- Register the flag in `init()`:
  ```go
  cleanCmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false,
      "Do not delete anything; only log what would be deleted.")
  ```
- Pass it through: `return backup.Clean(cmd.Context(), &jobInfo, cleanLocal, dryRun)`.

### `backup/clean.go`
- Change signature to `func Clean(pctx context.Context, jobInfo *files.JobInfo, cleanLocal, dryRun bool) error`.
- **cleanLocal removal (line ~102):** if `dryRun`, `log.AppLogger.Noticef("Would delete local manifest %s.", manifestPath)` and skip `os.Remove`.
- **force-path removal (line ~164):** if `dryRun`, `Noticef("Would delete local cached manifest %s.", manifestPath)` and skip `os.Remove`. Keep the temp-volume create/close/DeleteVolume cleanup unchanged.
- **destination delete (lines ~184-233):** after `allObjects` is finalized, if `dryRun`, log a summary `Noticef("Dry-run: would delete %d objects in destination.", len(allObjects))`, then loop over `allObjects` emitting `Noticef("Would delete %s.", filepath.Join(target, obj))` for each, and `return nil` — short-circuiting before the errgroup/`backend.Delete` machinery (leaves the concurrency code untouched).

### Log level note
Existing per-item delete logs use `Debugf`, which is hidden at the default
(Notice) level. Dry-run messages use **`Noticef`** so the preview is visible by
default — that is the entire purpose of the flag.

## Files to modify
- `cmd/clean.go`
- `backup/clean.go`

## Verification
- `go build ./...` and `go vet ./...` succeed.
- `zfsbackup clean --help` shows the new `-n, --dry-run` flag.
- Against a test target: run `zfsbackup clean -n <uri>` and confirm it logs
  "Would delete ..." lines and the destination/local cache are unchanged
  (e.g. compare `backend.List` output / local cache dir before and after).
  Then run without `-n` and confirm the same objects are actually removed.
- The repo has no `clean_test.go`; `backup/backup_test.go` provides a
  `mockBackend`. Optionally add a `Clean` dry-run test that uses `mockBackend`
  and asserts `Delete` is never called when `dryRun` is true.
