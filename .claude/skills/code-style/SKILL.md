---
name: Code Style
description: A skill for choosing how to write software
---

# Code Style Skill

## Rules

* Do not look up environment variables (e.g. `os.Getenv`) except in the entrypoint of the system. In most cases, this should be configured with the existing configuration system (e.g. `urfav/cli`).
