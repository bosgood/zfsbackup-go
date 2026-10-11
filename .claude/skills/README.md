# Agent Skills

Each of the subdirectories is a separate agent skill. Use the table below to decide when to use them.

## Directory

* `napkin`: Always active. Read `.claude/napkin.md` as the first call of a session; add a rule to it when you learn something the hard way.
* `codesearch`: Locating code. Use `semble` (vector search) when you only know a concept or area and need an entry point; use `cs` (codespelunker) when you know a symbol and need its declaration or usages. Use before grep. Subagents must use it too.
* `code-style`: Choosing how to write software.
* `adversarial-reviewer`: A critical review of recent changes through hostile reviewer personas. Use before merging, or when a review seems too agreeable.
* `adversarial-pr-review`: Review a GitHub PR or branch with a security / correctness / test panel, refute each finding, then post the review via `gh`.
