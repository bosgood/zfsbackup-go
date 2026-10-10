# clean: Clean

Read this for any change to `backup/clean.go`.

Diagram 9 in [../architecture.md](../architecture.md).

## Rule

`Clean` deletes only an object it can name as a backup volume of a dataset that a manifest at this destination names. Everything else stays.

## Flow (`backup.Clean`, `backup/clean.go`)

1. `prepareBackend`, `getCacheDir`.
2. `readForClean`: list all objects, sync and read the manifests.
3. Take the send lock of each dataset. A busy dataset is left alone.
4. If the read found a new dataset, read again (bounded rounds).
5. Objects but no manifests → refuse. Tell the user to check `--manifestPrefix`.
6. Local-only cached manifests: `--cleanLocal` deletes them later. Else keep their volumes. One cached under another `--manifestPrefix` (`foreign`) is never deleted. One that does not decode stops the run in both modes (`unreadableLocalManifestError`): the cache holds every prefix's and key's manifests, so it may be another job's.
7. Candidates = objects that are not manifests, not in a nested destination, parse as a volume name, belong to a known non-busy dataset, and are not of a backup set that a manifest under another `--manifestPrefix` at the destination names (`otherPrefixSets`: by name only, those manifests are never read).
   - The name is cut at its LAST `.manifest`: a prefix or snapshot name may hold `.manifest`.
   - A name cannot be split for sure (a prefix may hold the separator; a pool may be named `to`). So `otherPrefixSets` indexes every reading: the last 2 parts as a full set, and the last 4 as an incremental when the 2nd to last part is `to`. `p2|tank|a|to|b.manifest.gz` gives `|tank|a|to|b` AND `|to|b`. The extra key is on purpose. It can only spare more volumes, never delete more.
   - Which manifests are ours: the same test `syncCache` uses (`isManifestObject`). So a manifest of our prefix written with another `--separator` (`manifests#tank#a...`) is read as ours, and its orphans are candidates. A sibling prefix (`manifests-old|...`, or `manifests/...`, since `-` and `/` can be in a dataset name) counts as another prefix's.
8. For each manifest: all volumes present, or no `--force` → keep its volumes (warn if broken). Missing volumes with `--force` → delete the manifest and its volumes.
9. `--dry-run` → log the deletes. Else 5 workers call `backend.Delete` with backoff, then remove local cache files.

## Flags

`cleanLocal`, `cleanDryRun`, and `--force` on `jobInfo.Force`. Standalone vars live in `cmd/clean.go`.

## Tests

- `backend` is built from the URI by `backends.GetBackendForURI`, so it cannot be mock-injected. Tests use the real `file://` backend against a temp dir and set `config.WorkingDir` to a temp cache dir (`backup/clean_test.go`, `clean3_test.go`).
- S3 paths without network: root `clean_s3_test.go` uses an `httptest.Server` for ListObjectsV2 plus `AWS_S3_CUSTOM_ENDPOINT`.
