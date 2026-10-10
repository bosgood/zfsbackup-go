# Manifest reads: bound the memory (plan)

Read `intent.md` in this directory first.

## Rules for this repo

- Read `.claude/napkin.md` first.
- Build and test through the Makefile: `make test-one RUN=... PKG=./files/`,
  `make test-docker`, `make fmt-check`, `make lint`.
- Prove each new test: break the code in a scratch copy
  (`git ls-files -co --exclude-standard | tar -cf - -T - | tar -x -C /tmp/x`)
  and see the test fail.
- Find code with `semble search` and `cs` (`.claude/skills/codesearch/SKILL.md`).

## Step 1: measure (no behavior change)

Add `files/manifest_memory_test.go`:

- Read a manifest of 200,000 real-looking volumes (reuse the builder of
  `TestReadManifestLargeSend`). Record `runtime.MemStats.TotalAlloc` before and
  after `ReadManifest`, and log the bytes per JSON byte.
- Read a gzip bomb just over `MaxManifestBytes` (lower the limit with
  `setManifestLimit`). Record the same.
- Read a signed manifest with a forged signer. Record the same.

Write the numbers into `intent.md` in place of the estimates. Commit.

The tests in steps 2 and 3 assert on these numbers with a margin, for example
"under 2 times the JSON size", not on exact bytes.

## Step 2: pass 1, verify before decode (option B)

In `files/manifest.go`:

1. Open the cached file ONCE (`os.Open`). Keep the `*os.File`.
   - Do not open the path twice. Another `clean` or `list` can replace the cache
     file (`WriteFileAtomic` renames over it) between the two passes. Then pass 2
     would decode a file that pass 1 did not check. An open file descriptor keeps
     the old file.
2. Pass 1: build the extract chain on the file (PGP + gzip, as `Extract` does),
   copy it to `io.Discard` through a counter, stop at `MaxManifestBytes + 1`.
   - Over the limit → `ErrManifestTooLong`, as today.
   - A signature error at EOF → the same error as today (`KeyError` or the PGP
     error), so `unreadableManifestError` and `unreadableLocalManifestError` keep
     their advice.
3. `f.Seek(0, io.SeekStart)`, build the chain again, and decode (step 3, or
   `readAtMost` + `json.Unmarshal` until step 3 lands).

`Extract` takes a path today (`ExtractLocal`). Add a way to extract from an open
`*os.File`; keep `ExtractLocal` for other callers.

Tests (`files/manifest_limit_test.go`):

- A bomb over the limit: `ErrManifestTooLong`, and `TotalAlloc` under a few MiB.
- A manifest signed by an untrusted key, under the limit: the same error as
  today, and `TotalAlloc` under a few MiB.
- Every existing `ReadManifest` test passes unchanged.

Commit. This step alone closes the security finding.

## Step 3: pass 2, decode piece by piece (option C, with D)

Replace `readAtMost` + the two `json.Unmarshal` calls with a streaming decode:

```go
dec := json.NewDecoder(r)
// '{', then for each key: "Volumes" → stream; any other key → RawMessage
for dec.More() {
    key := nextKey(dec)
    if !strings.EqualFold(key, "Volumes") { // encoding/json matches keys case-insensitively
        var raw json.RawMessage
        dec.Decode(&raw)
        rest[key] = raw
        continue
    }
    // '[' ... ']': decode one *VolumeInfo at a time, check count and null
}
// json.Unmarshal(marshal(rest)) into JobInfo for the small fields
```

The output MUST be the same as `encoding/json`'s for every input that today's
code accepts. Points that differ easily:

- Key matching is case-insensitive (`"volumes"` fills `Volumes`).
- A key that occurs twice: the last one wins.
- `"Volumes": null` → nil slice. A volume that is `null` → error (as
  `manifestVolumes` does today).
- Top-level `null`, or not an object → the same error as today.
- Trailing data after the object: today `json.Unmarshal` rejects it. The decoder
  must check `dec.More()` / EOF after the object.

Option D: count volumes against a byte budget (for example, the decoded size of
each `VolumeInfo` added up, under `MaxManifestBytes`), so 4 million `{}` volumes
are rejected. Do not require `ObjectName`: check first that no old version wrote
a manifest without it.

Tests:

- An equivalence test: for every manifest the test suite builds, plus a table of
  edge cases (the list above), the old decoder and the new one give
  `reflect.DeepEqual` results or both fail.
- A fuzz test (`FuzzReadManifestEquivalence`) with the same check. Run it for a
  short time in `make test-docker`; keep the corpus in `files/testdata/fuzz/`.
- The memory test of step 1: a real manifest costs under about 1.5 times its
  decoded structs.

Remove `readAtMost` if nothing else uses it.

## Step 4: docs

- `docs/context/data-model.md`: how `ReadManifest` reads (two passes, streaming).
- `files/manifest.go`: the `MaxManifestBytes` comment ("a reader allocates about
  three times this much") gets the measured number.
- `CHANGELOG.md`: one `[FIX]` line.
