# Third high-risk pass: fixes

Date: 2026-10-11
Branch: `clean-dry-run` at `3814a53`
Source: the third review pass over `CHANGELOG.md` (chat, 2026-10-10). The facts it
verified are in `.claude/napkin.md` under "third high-risk pass".

## Goal

Close six areas the pass found. Each area gets a test first. The test must fail
on the current code. Then the fix makes it pass. Area 6 of the pass (the lock
across users) is out of scope: the user decided not to handle multi-user access.

| #   | Area                                                           | Outcome wanted                                                                                        |
| --- | -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| 1   | Explicit `receive` of a snapshot that is already local         | Exit 0 and a notice when the snapshot is there. An error, no `zfs receive`, when a same-named snapshot has another creation time, with or without `--force`. |
| 2   | Wrong-key manifests get the "delete that object" advice        | A key error names the key problem and never advises deleting. The cached copy stays.                 |
| 3   | `--resume` when every byte is skipped but the cache is not final | The resume finalizes the set from the kept volumes and uploads its manifest.                       |
| 4   | Glacier restore window is a fixed 3 days                       | The window grows with the bytes to download. An env var overrides it.                                 |
| 5   | A sibling prefix that starts with a symbol is read as ours     | `clean` skips it and spares its sets.                                                                 |
| 7   | No fixture of a manifest written by an older version           | A committed destination written by `master` reads back in `list`, `receive --auto`, `clean` and smart `send`. |

## Not in scope

- Area 6: `lockfile.isRunning` treats EPERM as a dead holder. Decided out.
- The 1 GiB manifest memory design (`docs/specs/2026-10-10--manifest-read-memory/`).
- Any change to `plan`.
