# Dockerfile for zfsbackup-go. Multi-stage:
#
#   toolchain     Go toolchain + build tools shared by the stages below.
#   devcontainer  Interactive dev environment with Claude Code sandboxed behind
#                 an egress firewall. Built by .devcontainer/devcontainer.json
#                 (VS Code "Reopen in Container", `devcontainer up`) or
#                 `make devcontainer-build`.
#   test          Reproducible unit-test runner, and the default target:
#
#                   docker build -t zfsbackup-test .     (or: make test-docker)
#                   docker run --rm zfsbackup-test
#
# The `test` stage's default command runs the unit tests via `go test ./...`.
# The ZFS integration tests in integration_test.go are gated behind the
# `integration` build tag, so they do NOT run here -- they shell out to
# `zfs`/`zpool` and expect a populated `tank/data@c` dataset, which needs ZFS +
# root on the host. To run them, build with `-tags integration` in a suitable
# environment.
#
# The cloud backend tests (backends/) use in-process mocks/servers by default
# and need no external services -- the ones that DO want a real endpoint
# self-skip when their env vars (AWS_S3_CUSTOM_ENDPOINT, AZURE_CUSTOM_ENDPOINT,
# GCS_FAKE_SERVER, B2_*) are unset.
#
# GO_VERSION must satisfy the `go` directive in go.mod. The official golang
# images set GOTOOLCHAIN=local, so an older image refuses to build a newer
# go.mod ("go.mod requires go >= X") instead of downloading a toolchain.
# `make test-docker` and `make devcontainer-build` derive it from go.mod; the
# default below is what plain `docker build` and devcontainer.json use, so bump
# it whenever go.mod's `go` line changes.
ARG GO_VERSION=1.25

###############################################################################
FROM golang:${GO_VERSION}-bookworm AS toolchain

WORKDIR /src

# gox, the cross-compilation tool used by the Makefile build targets. It lands
# in $GOPATH/bin (/go/bin), which is on PATH, so `make build` works.
RUN go install github.com/mitchellh/gox@v1.0.1

###############################################################################
# Dev container: a non-root user, Claude Code, and the tooling its egress
# firewall needs (.devcontainer/init-firewall.sh). Nothing from the repo is
# copied in; devcontainer.json bind-mounts the checkout at /workspaces/zfsbackup-go.
FROM toolchain AS devcontainer

ARG USERNAME=dev
ARG USER_UID=1000
ARG USER_GID=1000
# Pinned to the release the development host ran when this was last bumped
# (`claude --version`); bump it here and rebuild, or override with
# --build-arg CLAUDE_CODE_VERSION=latest for the newest release.
ARG CLAUDE_CODE_VERSION=2.1.289
ARG TZ=UTC
ENV TZ=${TZ} \
    DEVCONTAINER=true

RUN apt-get update && apt-get install -y --no-install-recommends \
        aggregate \
        dnsutils \
        gh \
        ipset \
        iptables \
        iproute2 \
        jq \
        less \
        man-db \
        nano \
        procps \
        ripgrep \
        sudo \
        unzip \
        vim \
        zstd \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Non-root user that Claude Code runs as. Its only sudo right is the firewall
# script. /go (GOPATH: module cache + installed tools) is handed over so
# `go install`/`go get` work without root.
RUN groupadd --gid "${USER_GID}" "${USERNAME}" \
    && useradd --uid "${USER_UID}" --gid "${USER_GID}" --create-home --shell /bin/bash "${USERNAME}" \
    && chown -R "${USERNAME}:${USERNAME}" /go

# Directories backed by named volumes in devcontainer.json (Claude config +
# credentials, shell history, Go build cache). Created here with the right
# owner so Docker seeds new volumes with that ownership.
RUN mkdir -p /commandhistory "/home/${USERNAME}/.claude" "/home/${USERNAME}/.cache" /workspaces/zfsbackup-go \
    && touch /commandhistory/.bash_history \
    && chown -R "${USERNAME}:${USERNAME}" /commandhistory "/home/${USERNAME}" /workspaces \
    && printf '%s\n' \
        'export HISTFILE=/commandhistory/.bash_history' \
        "export PROMPT_COMMAND='history -a'" \
        >> "/home/${USERNAME}/.bashrc"

# The bind-mounted checkout may be owned by a different uid than ${USERNAME}
# (Linux hosts before updateRemoteUserUID runs, some Docker Desktop setups);
# without this git refuses to touch it ("dubious ownership").
RUN git config --system --add safe.directory /workspaces/zfsbackup-go

USER ${USERNAME}
WORKDIR /workspaces/zfsbackup-go
ENV HOME=/home/${USERNAME} \
    PATH=/home/${USERNAME}/.local/bin:${PATH} \
    CLAUDE_CONFIG_DIR=/home/${USERNAME}/.claude \
    EDITOR=vim \
    VISUAL=vim

# gopls for the VS Code Go extension and Claude Code's LSP support. Pinned to
# the newest release that builds with GO_VERSION (gopls@latest already wants
# Go 1.26, which GOTOOLCHAIN=local refuses); bump it together with GO_VERSION.
# The build cache it leaves behind is dropped to keep the image (and the
# seeded cache volume) small.
ARG GOPLS_VERSION=v0.21.1
RUN go install "golang.org/x/tools/gopls@${GOPLS_VERSION}" && go clean -cache

# Claude Code via the native installer (no Node.js needed). CLAUDE_CODE_VERSION
# is an explicit x.y.z (the ARG above), `latest`, or `stable`. Rebuild to pick
# up a new release; the built-in auto-updater also works because
# downloads.claude.ai is on the firewall allowlist.
RUN curl -fsSL https://claude.ai/install.sh | bash -s -- "${CLAUDE_CODE_VERSION}" \
    && claude --version

# Egress firewall, applied on every container start by postStartCommand. Last,
# so editing the script does not invalidate the tool-install layers above.
USER root
COPY .devcontainer/init-firewall.sh /usr/local/bin/init-firewall.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh \
    && echo "${USERNAME} ALL=(root) NOPASSWD: /usr/local/bin/init-firewall.sh" > "/etc/sudoers.d/${USERNAME}-firewall" \
    && chmod 0440 "/etc/sudoers.d/${USERNAME}-firewall"
USER ${USERNAME}

###############################################################################
# Unit-test runner (default target). Dependencies are vendored (vendor/ is
# checked in), so there is no module download step: `go build`/`go test` run in
# vendor mode and the build needs no network beyond pulling the base image.
FROM toolchain AS test

COPY . .

# Pre-compile all packages and test binaries so `docker run` is fast and
# offline. A compile error fails the image build here, not at `docker run`.
RUN go build ./... && go test -run '^$' ./... >/dev/null

# Run the unit tests. The integration tests are excluded via build tags.
CMD ["go", "test", "./..."]
