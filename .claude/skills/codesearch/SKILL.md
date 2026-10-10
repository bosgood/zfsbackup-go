---
name: codesearch
description: |
  Locate code in this repo with two tools, chosen by what you already know.
  Use `semble` (vector search) when you only know a general term or an area
  of functionality and need an entry point. Use `cs` (codespelunker) when you
  know a symbol name and need its declaration or its usages. Use these before
  grep for any task that must find code. Subagents must use them too.
---

# codesearch

Two tools. Pick the tool by what you know.

| You know...                                                        | Use      | Why                                                                                            |
| ------------------------------------------------------------------ | -------- | ---------------------------------------------------------------------------------------------- |
| A concept or area: "retry an upload", "prune manifests"            | `semble` | Vector search over code chunks. Finds the entry point even when the code uses different words. |
| An exact identifier: `Clean`, `ResetReceiveJobInfo`, `cleanDryRun` | `cs`     | Boolean and regex search with declaration and usage filters.                                   |

## Rules

1. No symbol name yet? Start with `semble`. Take the best `file:lines` hit. Then switch to `cs`.
2. Symbol name known? Use `cs`. Do not use grep for declarations or usages.
3. Use grep only for what these tools do not cover well: Makefile targets, YAML keys, exact multi-line strings.
4. Always add `--color never` to `cs`. Always add `2>/dev/null` to `semble` (it prints index warnings on stderr).
5. Read the code at the hit before you make a claim about it.

## semble: find an entry point

```sh
semble search "<what the code does, in plain words>" -k 5 --max-snippet-lines 0 --format text 2>/dev/null
```

Output: one `file:start-end` per result, best first. Read the hit, or hand the symbol to `cs`.

Options:

- `-k N` returns N results (default 5). Raise it when the first page is off-target.
- `--max-snippet-lines 10` shows the signature plus body. `0` shows locations only.
- `--format json` (the default) adds score and chunk content.
- `--content docs` or `--content all` also searches markdown and config files.
- `semble find-related <file> <line> -k 5 --max-snippet-lines 0 --format text 2>/dev/null` finds code similar to a known location: siblings, tests, duplicates.
- `semble clear index` drops the index cache. Use it when results look stale after a large refactor. The index rebuilds on the next search.

Repo note: `.sembleignore` at the repo root excludes `vendor/`. Without it most hits are vendored SDK code. Keep that file.

## cs: find a symbol

`cs` matches case-insensitive substrings by default. A search for `Clean` also returns `cleanManifest` and `TestCleanX`. For an exact symbol add `-c` and a regex with a word boundary.

```sh
cs --color never -c -i go -x vendor --only-declarations '/func Clean\(/'   # where it is defined
cs --color never -c -i go -x vendor --only-usages '/\bClean\(/'           # where it is called
cs --color never -c -i go -x vendor,_test.go --only-usages '/\bClean\(/'  # callers, tests excluded
cs --color never -i go -x vendor -s 5 cleanDryRun                          # loose match, 5 snippets per file
cs --color never -i go -x vendor -f json --result-limit 10 cleanDryRun     # structured output
cs --color never -i go -x vendor --snippet-mode grep -C 2 cleanDryRun      # grep-style lines with context
```

Flag meanings: `-c` case sensitive. `-i go` only `.go` files (drop it to search markdown under `docs/`). `-x vendor` skips vendored code. `-s N` snippets per file.

Query syntax:

- Terms are ANDed. `"exact phrase"` for phrases.
- `a OR b`, `a NOT b`, `(a OR b) NOT c`. NOT binds to the next term only.
- `/regex/` for regular expressions. `term~1` for fuzzy match.
- `file:clean` filters by filename substring. `path:backup/` filters by path substring.
- `-c` makes the search case sensitive. Use it to separate exported from unexported names.

Ranking notes: the default ranker is structural. Test files rank lower unless the query mentions tests. Add `file:_test` to target tests.

## Worked example

Task: "Where does `clean` decide what to delete?"

```sh
semble search "clean decides which backup volumes to delete" -k 5 --max-snippet-lines 0 --format text 2>/dev/null
# -> backup/clean.go:171-176   (func Clean)
cs --color never -c -i go -x vendor --only-usages '/\bClean\(/'
# -> cmd/clean.go:54 and the tests that call it
```

Then read the region and continue.

## Subagents

Subagents do not inherit the skills you loaded. They get only their prompt plus `AGENTS.md`. When you spawn one (Explore, general-purpose, Plan, claude) for any task that locates code, put this in its prompt:

> Use `semble search` for concepts and `cs` for symbols, as described in `.claude/skills/codesearch/SKILL.md`. Read that file first. Do not use grep to locate code.
