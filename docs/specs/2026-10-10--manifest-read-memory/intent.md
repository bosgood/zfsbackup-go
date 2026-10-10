# Manifest reads: bound the memory

Date: 2026-10-10
Branch: `clean-dry-run`
Source: adversarial review of `699a65c..00c83f6`, finding #4 ("the memory a forged
manifest can force is 16 times larger").

## Problem

`files.ReadManifest` (`files/manifest.go`) does three steps:

1. `ExtractLocal` opens the cached file. Reads go through PGP (when the job has keys)
   and gzip.
2. `readAtMost` reads ALL of the result into one `[]byte`, up to
   `MaxManifestBytes` (1 GiB since fe7c2ee, was 64 MiB).
3. `json.Unmarshal` twice: once to check the `Volumes` array (count, no `null`),
   once to decode.

With `--signFrom`, `Extract` (`files/volumeinfo.go`) rejects an UNSIGNED file at
once, before any read. But a file signed by ANOTHER key (any key the attacker
makes) is only rejected at EOF (`VolumeInfo.signatureError`). So step 2 must hold
the full file in memory before the reader knows that the file is forged.

Callers: `clean`, `list`, `plan`, smart `send`, `receive` (all through
`readCachedManifest` or `readManifest` in `backup/`).

### Memory today (estimates; step 1 of the plan measures them)

| Input                                                                  | Peak                                                                                      |
| ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| Real manifest at the limit (about 2 million volumes)                   | about 3 GiB: `readAtMost` chunks + the joined slice + decoded structs                     |
| Gzip bomb, over the limit                                              | about 1 GiB of chunks, then `ErrManifestTooLong`                                          |
| Forged manifest under the limit, signing on, unsigned                  | O(1): rejected before the read                                                            |
| Forged manifest under the limit, signing on, signed by another key     | up to 1 GiB, then the signature error                                                     |
| Forged manifest of 4 million `{}` volumes (12 MB of JSON), signing off | about 2 GB of empty `VolumeInfo` structs (`MaxManifestVolumes` is `MaxManifestBytes/256`) |

### Threat model

- The attacker can WRITE objects under the manifest prefix at the destination.
- In most setups that attacker can also DELETE the backups, which is worse than
  any memory attack. The memory attack matters when deletes are not possible
  (S3 object lock, versioned buckets, append-only credentials) AND the job signs
  its manifests (`--signFrom`), so the user expects forged manifests to do no harm.
- With signing off, a forged manifest is accepted as real. Bounding memory then
  stops a crash, not a forgery.
- With only `--encryptTo`, anyone with the public key can write a manifest that
  decrypts. Encryption alone is not authentication.

## Goals

1. A forged manifest, of any size, costs O(1) memory when the job signs manifests.
2. A file over the limit costs O(1) memory, signed or not.
3. A real manifest costs about the size of its decoded structs, not three times that.
4. The decoded `JobInfo` is the same as today, for every input today accepts.

## Non-goals

- Smaller decoded structs (for example, `clean` keeping only object names). That
  is a data-model change; do it separately if goal 3 is not enough.
- A different manifest format.
- Fewer manifest writes per send (review finding #2, the per-volume rewrite).

## Options

|     | What                                                                                                                                         | Fixes                                                              | Cost                                                                                           |
| --- | -------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| A   | Reject a decompressed/compressed ratio over about 20                                                                                         | Bombs only partly: the attacker uploads about 50 MiB, not a few KB | About 10 lines. A heuristic                                                                    |
| B   | **Pass 1:** read the stream to `io.Discard`, counting bytes. Signature and size are checked with O(1) memory. Decode only if pass 1 succeeds | Goals 1 and 2                                                      | Small. A second decrypt + gunzip per read: milliseconds for normal manifests, seconds at 1 GiB |
| C   | **Pass 2 piece by piece:** a `json.Decoder` on the stream, one `VolumeInfo` at a time; no `[]byte` of the full file                          | Goal 3                                                             | Medium. Must match `encoding/json` exactly (see plan, step 3)                                  |
| D   | Lower volume cap: count decoded volumes against a byte budget, or require `ObjectName` in every volume                                       | The `{}` row of the table                                          | Small. Can reject a strange but real old manifest                                              |

C without B is WORSE than today: it would build forged structs before the
signature check at EOF.

## Decision

Do B first (it is the security fix, and small), then C (the large-manifest fix).
Do D with C. Do not do A: B makes it unnecessary.
