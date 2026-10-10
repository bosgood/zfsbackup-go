# Agent Skills

Each of the subdirectories is a separate agent skill. Use the table below to decide when to use them.

## Directory

* `finding-documentation`: Finding documentation for libraries and APIs, use whenever beginning a new coding task or when asked to find documentation.
* `codesearch`: Locating code. Use `semble` (vector search) when you only know a concept or area and need an entry point; use `cs` (codespelunker) when you know a symbol and need its declaration or usages. Use before grep. Subagents must use it too.
