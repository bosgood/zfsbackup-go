---
name: napkin
description: |
  Maintain a per-repo napkin file that tracks mistakes, corrections, and
  what works. Activates EVERY session, unconditionally. Read the napkin
  before doing anything. Write to it continuously as you work — not just at
  session boundaries. Log your own mistakes, not just user corrections. The
  napkin lives in the repo at `.claude/napkin.md`.
---

# Napkin

`.claude/napkin.md` holds rules that change how you work in this repo.

## Read

Call #1 of every session: `cat .claude/napkin.md`, alone. Apply it silently.
Re-read the relevant section before you do something it warns about.

## Write

Add an entry at once when:

- you hit an error and find the cause,
- the user corrects you,
- you catch your own mistake,
- an approach fails, or a non-obvious one works.

Be specific. "Made an error" is useless. "`grep` here is ugrep and ignores
`--`" is useful. Write the rule, not the story: one or two lines, in the
section it belongs to. Do not add what `CLAUDE.md` or `docs/context/`
already says.

## Do not write

- Review findings, defect lists, branch comparisons, session logs. Put them
  in `docs/specs/<date>--<slug>/` and add a one-line pointer under
  "Open plans" if work remains.
- Facts that the code shows clearly.

## Keep it small

The whole file must fit in one read. Limit: **15 KB** (`wc -c`).
When it is over, or every few sessions:

- merge repeated entries into one rule,
- delete entries that are fixed or now in `CLAUDE.md` / `docs/context/`,
- move history to `docs/specs/`.
