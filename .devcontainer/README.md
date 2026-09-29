# Dev container: Claude Code sandbox

A dev container that runs Claude Code against this repo with the same Go
toolchain as `make test-docker`, as a non-root user, behind an egress firewall.
It is the `devcontainer` stage of the top-level `Dockerfile`; nothing here
duplicates the toolchain setup.

## Use

VS Code: **Dev Containers: Reopen in Container**, then run `claude` in the
integrated terminal (or use the Claude Code extension, which is preinstalled).

CLI, without VS Code (needs `npm install -g @devcontainers/cli`):

```sh
make devcontainer-claude   # devcontainer up + exec claude
make devcontainer-shell    # a bash shell in the same container
make devcontainer-build    # just build the image (plain docker, no CLI needed)
```

The first `claude` run asks you to log in. Open the printed URL in a browser
on the host and paste the code back. Credentials and settings land in the
`zfsbackup-go-claude-config-*` volume and survive rebuilds. `ANTHROPIC_API_KEY`
is deliberately not passed through from the host; add it to `containerEnv` or
`remoteEnv` in `devcontainer.json` if you want key-based auth instead.

## What the sandbox does

- **Non-root.** Everything runs as `dev` (uid 1000). Its only sudo right is
  `init-firewall.sh`. The repo is bind-mounted at `/workspaces/zfsbackup-go`,
  so edits are visible on the host immediately; nothing else on the host is
  reachable.
- **Egress allowlist.** `init-firewall.sh` runs on every container start and
  default-denies all traffic except DNS, the Docker host network, GitHub
  (ranges from `api.github.com/meta`), Anthropic/Claude Code endpoints, the Go
  module proxy and checksum DB, and the VS Code marketplace. Everything else is
  rejected immediately. IPv6 is blocked outright. Edit `ALLOWED_DOMAINS` in the
  script and rebuild (or re-run it with `sudo`) to allow more. Blanket outbound
  SSH is not allowed; `git@github.com` still works because GitHub's ranges are.
  There is no Node.js in the image and `registry.npmjs.org` is not allowlisted,
  so `npx`-based MCP servers need both added before they can run in here.
- **Persistent state** in named volumes: Claude config/credentials
  (`~/.claude`), bash history, and `~/.cache` (Go build cache).

The container has Docker's `NET_ADMIN`/`NET_RAW` capabilities so root can
program iptables; the `dev` user cannot.

## Updating Claude Code

The image installs the newest release at build time via the native installer
(`CLAUDE_CODE_VERSION` build arg, default `latest`; `stable` or `x.y.z` also
work). Rebuild the container to bump it. The auto-updater inside the container
works too, because `downloads.claude.ai` is allowlisted, but that update lives
in the container's writable layer and is lost on rebuild.

## Go version

The image's Go version must satisfy `go.mod`'s `go` line (the golang images
set `GOTOOLCHAIN=local`). `make devcontainer-build` / `make test-docker` read it
from `go.mod`; VS Code and `devcontainer up` use the `GO_VERSION` default in
`Dockerfile`, so bump that default when `go.mod` changes.
