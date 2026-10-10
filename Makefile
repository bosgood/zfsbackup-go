TARGETS="freebsd/amd64 linux/amd64"
COMMIT_HASH=`git rev-parse --short HEAD 2>/dev/null`
# Go toolchain for the Docker images, taken from go.mod's `go` directive so the
# images cannot drift from the module (see the GO_VERSION note in Dockerfile).
GO_VERSION := $(shell sed -nE 's/^go ([0-9]+(\.[0-9]+)*).*/\1/p' go.mod)
DOCKER_BUILD = docker build --build-arg GO_VERSION=$(GO_VERSION)

check: lint test test-race e2e

# Rewrite every tracked non-vendor .go file with gofmt -s (fmt-check reports them).
fmt:
	git ls-files "*.go" | grep -v ^vendor/ | xargs gofmt -s -l -w

# golangci-lint in Docker, pinned so new linters do not appear by surprise. .golangci.yml is v2 format.
GOLANGCI_LINT_IMAGE=golangci/golangci-lint:v2.12.2
lint:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GOLANGCI_LINT_IMAGE) golangci-lint run

get:
	go get -v -d -t ./...

test:
	go test ./...

# The root package's e2e tests take about 20 minutes under -race, past go test's 10m default.
test-race:
	go test -race -timeout 45m ./...

# Integration tests are gated behind the `integration` build tag and are NOT
# run by `test`/`test-race`. They require a host with ZFS (zfs/zpool binaries,
# loaded kernel module, root) and a `tank/data@c` dataset to copy from.
integration:
	go test -tags integration -v ./...

test-docker:
	$(DOCKER_BUILD) --target test -t zfsbackup-test . && docker run --rm zfsbackup-test

# Tests that need a full disk: the working directory's temp/ is a 40 KiB tmpfs. Plain `go test`
# skips them (they need ZFSBACKUP_ENOSPC_WORK).
test-enospc:
	$(DOCKER_BUILD) --target test -t zfsbackup-test . && \
	docker run --rm --tmpfs /work/temp:size=40k -e ZFSBACKUP_ENOSPC_WORK=/work zfsbackup-test \
		go test -count=1 -run 'ENOSPC' -v .

# Dev container with Claude Code sandboxed behind an egress firewall; see
# .devcontainer/. `devcontainer-build` only builds the image (plain docker);
# the other targets drive it with the devcontainer CLI
# (npm install -g @devcontainers/cli), which also works without VS Code.
devcontainer-build:
	$(DOCKER_BUILD) --target devcontainer -t zfsbackup-devcontainer .

devcontainer-up:
	devcontainer up --workspace-folder .

devcontainer-claude: devcontainer-up
	devcontainer exec --workspace-folder . claude

devcontainer-shell: devcontainer-up
	devcontainer exec --workspace-folder . bash

# Run the working tree's firewall script in a throwaway container: a first run,
# a re-run that must never show an ACCEPT policy, a re-run by the dev user
# killed after 0.3s that must leave the network closed, and a re-run from
# there (.devcontainer/firewall-check.sh). Needs the devcontainer image.
devcontainer-firewall-check:
	docker run --rm --user root --cap-add NET_ADMIN --cap-add NET_RAW \
		-v "$(CURDIR)/.devcontainer/init-firewall.sh":/usr/local/bin/init-firewall.sh:ro \
		-v "$(CURDIR)/.devcontainer/firewall-check.sh":/usr/local/bin/firewall-check.sh:ro \
		zfsbackup-devcontainer /usr/local/bin/firewall-check.sh

# Run a single test against the supported toolchain without rebuilding the
# image, e.g. `make test-one RUN=TestSelectSmartSnapshots PKG=./backup/`.
GO_IMAGE=golang:$(GO_VERSION)-bookworm
RUN?=.
PKG?=./...
# GOCACHE_DIR holds the Go build cache between runs; without it every run compiles from scratch.
GOCACHE_DIR?=/tmp/zfsb-gocache
test-one:
	mkdir -p "$(GOCACHE_DIR)"
	docker run --rm -v "$(CURDIR)":/src -v "$(GOCACHE_DIR)":/root/.cache/go-build -w /src $(GO_IMAGE) go test -run '$(RUN)' -v $(PKG)

# Report files that `gofmt -s` would rewrite, using the pinned toolchain. The
# `fmt` target above iterates over an undefined DIRS and so checks nothing.
# Checks every tracked .go file outside vendor/ (so not the tmp/ scratch clones) with the pinned toolchain.
# Fails if git lists no files: inside a git worktree the .git link points outside the mount.
# Fails if gofmt does: a file that does not parse is not "formatted".
fmt-check:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) \
		sh -c 'files=$$(git -c safe.directory=/src ls-files "*.go" | grep -v ^vendor/); [ -n "$$files" ] || { echo "fmt-check: no .go files listed (a git worktree? its .git link does not resolve in the container)"; exit 1; }; out=$$(echo "$$files" | xargs gofmt -s -l) || { echo "$$out"; echo "^ gofmt failed (a file that does not parse?)"; exit 1; }; [ -z "$$out" ] || { echo "$$out"; echo "^ gofmt -s would rewrite these files"; exit 1; }'

build:
	${GOPATH}/bin/gox -ldflags="-w -s -X github.com/someone1/zfsbackup-go/config.GitCommit=${COMMIT_HASH}" -osarch=${TARGETS}

build-dev:
	${GOPATH}/bin/gox -ldflags="-X github.com/someone1/zfsbackup-go/config.GitCommit=${COMMIT_HASH}" -osarch=${TARGETS} -output="{{.Dir}}_{{.OS}}_{{.Arch}}-${COMMIT_HASH}"

# Run a subset of the tests on the host, e.g.
#   make test-run PKG=./backup/ RUN='TestPlan|TestSchedule'
PKG ?= ./...
RUN ?= .
test-run:
	go test -count=1 -run '$(RUN)' -v $(PKG)

# Golden plan scenarios (backup/testdata/scenarios/*/expected.txt).
# `scenarios-update` rewrites every expected.txt; review the diff before committing.
scenarios:
	go test -count=1 -run 'TestScenarios|TestTieOrder' -v ./backup/

scenarios-update:
	go test -count=1 -run TestScenarios ./backup/ -update

# Plan smart backups with the working tree's code, e.g.
#   make plan ARGS="--fullIfOlderThan 4320h --fullSnapshotSuffix _monthly \
#     --incrementalSnapshotSuffix _monthly --snapshots snaps.txt tank/data"
plan:
	go run . plan $(ARGS)

# End-to-end tests: the real send pipeline against a file:// destination, with
# the test binary standing in for zfs (internal/fakezfs).
e2e:
	go test -count=1 -run TestE2E -v .

# Print a command's --help from the working tree's code, for the README's help
# blocks, e.g. `make help CMD=send` (CMD empty prints the root help).
help:
	go run . $(CMD) --help
