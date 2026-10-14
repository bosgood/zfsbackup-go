TARGETS="freebsd/amd64 linux/amd64"
COMMIT_HASH=`git rev-parse --short HEAD 2>/dev/null`
# Go toolchain for the Docker images, taken from go.mod's `go` directive so the
# images cannot drift from the module (see the GO_VERSION note in Dockerfile).
# Only X.Y: `go mod tidy` writes `go 1.26.0`, and golang:1.26.0-bookworm would pin the first patch release.
GO_VERSION := $(shell sed -nE 's/^go ([0-9]+\.[0-9]+).*/\1/p' go.mod)
DOCKER_BUILD = docker build --build-arg GO_VERSION=$(GO_VERSION)

check: lint test test-race e2e

# Rewrite every tracked non-vendor .go file with gofmt -s (fmt-check reports them).
fmt:
	git ls-files "*.go" | grep -v ^vendor/ | xargs gofmt -s -l -w

# golangci-lint in Docker, pinned so new linters do not appear by surprise. .golangci.yml is v2 format.
# LINT_ARGS adds flags, e.g. LINT_ARGS="--enable-only errorlint,nilerr" to try a linter.
GOLANGCI_LINT_IMAGE=golangci/golangci-lint:v2.12.2
lint:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GOLANGCI_LINT_IMAGE) golangci-lint run $(LINT_ARGS)

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

# Only build the `integration` image (it compiles and vets the test); works on any Docker host.
integration-image:
	$(DOCKER_BUILD) --target integration -t zfsbackup-integration .

# ZFS integration tests in Docker: s3:// against MinIO, file:// and ssh:// (an sshd
# in the test container). Needs a Linux host whose kernel has the zfs module (see
# integration/README.md); on a Mac use integration-vm. RUN picks tests (a Go regexp),
# e.g. RUN=TestIntegrationS3/Pipe; the default is TestIntegration(S3|File|SSH).
# `down -v` runs when the test fails too; the target keeps the test's status.
# INTEGRATION_AWS=1 runs against real AWS S3 instead of MinIO (integration/compose.aws.yaml):
# set AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and ZFSBACKUP_IT_BUCKET.
INTEGRATION_AWS_COMPOSE = $(if $(INTEGRATION_AWS),-f integration/compose.aws.yaml)
INTEGRATION_COMPOSE ?= -f integration/compose.yaml $(INTEGRATION_AWS_COMPOSE)
INTEGRATION_RUN = $(if $(filter .,$(RUN)),TestIntegration(S3|File|SSH),$(RUN))
integration-docker:
	GO_VERSION=$(GO_VERSION) RUN='$(INTEGRATION_RUN)' COVER='$(COVER)' docker compose $(INTEGRATION_COMPOSE) up \
		--build --abort-on-container-exit --exit-code-from zfs --no-attach mc; \
	status=$$?; docker compose $(INTEGRATION_COMPOSE) down -v; exit $$status

# The NixOS Lima VM with the zfs module and Docker (integration/lima.yaml,
# integration/nixos/): the way to run integration-docker on a Mac. The first
# integration-vm-up takes several minutes (image, nixos-rebuild, restart).
# In the VM the test runs the VM's own zfs userland (integration/compose.nixos.yaml).
LIMA_VM ?= zfsbackup
integration-vm-up:
	integration/vm-up.sh '$(LIMA_VM)'

# With INTEGRATION_AWS=1 the shell gets this environment (--preserve-env), for the AWS variables.
integration-vm: integration-vm-up
	limactl shell $(if $(INTEGRATION_AWS),--preserve-env) '$(LIMA_VM)' -- bash -c 'cd "$(CURDIR)" && make integration-docker RUN="$(RUN)" COVER="$(COVER)" \
		INTEGRATION_COMPOSE="-f integration/compose.yaml -f integration/compose.nixos.yaml $(INTEGRATION_AWS_COMPOSE)" \
		ZFS_USERLAND="$$(dirname "$$(readlink -f "$$(command -v zfs)")")"'

integration-vm-shell: integration-vm-up
	limactl shell '$(LIMA_VM)'

integration-vm-down:
	limactl stop '$(LIMA_VM)'

integration-vm-delete:
	limactl delete --force '$(LIMA_VM)'

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
# TESTFLAGS adds go test flags, e.g. TESTFLAGS=-race.
GO_IMAGE=golang:$(GO_VERSION)-bookworm
RUN?=.
PKG?=./...
# GOCACHE_DIR holds the Go build cache between runs; without it every run compiles from scratch.
GOCACHE_DIR?=/tmp/zfsb-gocache
test-one:
	mkdir -p "$(GOCACHE_DIR)"
	docker run --rm -v "$(CURDIR)":/src -v "$(GOCACHE_DIR)":/root/.cache/go-build -w /src $(GO_IMAGE) go test $(TESTFLAGS) -run '$(RUN)' -v $(PKG)

# Module maintenance in the pinned image (the host cannot reach proxy.golang.org).
# MODCACHE_DIR holds the module cache between runs.
MODCACHE_DIR?=/tmp/zfsb-modcache
GO_MOD_RUN = docker run --rm -v "$(CURDIR)":/src -w /src -v "$(GOCACHE_DIR)":/root/.cache/go-build \
	-v "$(MODCACHE_DIR)":/go/pkg/mod -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod $(GO_IMAGE)

# Upgrade every module.
# The first `go get` avoids "ambiguous import cloud.google.com/go/compute/metadata".
mod-update:
	mkdir -p "$(GOCACHE_DIR)" "$(MODCACHE_DIR)"
	$(GO_MOD_RUN) sh -c 'go get cloud.google.com/go/storage@latest google.golang.org/api@latest cloud.google.com/go/compute@latest \
		&& go get -u -t ./... && go mod tidy'

# One module at a specific version, e.g. make mod-get MOD=github.com/klauspost/pgzip@e1b5b318410c
mod-get:
	@[ -n "$(MOD)" ] || { echo "mod-get: set MOD=path@version"; exit 1; }
	mkdir -p "$(GOCACHE_DIR)" "$(MODCACHE_DIR)"
	$(GO_MOD_RUN) sh -c 'go get $(MOD) && go mod tidy'

# Rebuild vendor/ from go.mod and check the sums.
mod-vendor:
	mkdir -p "$(GOCACHE_DIR)" "$(MODCACHE_DIR)"
	$(GO_MOD_RUN) sh -c 'go mod tidy && go mod vendor && go mod verify'

# Known vulnerabilities reached by our code (govulncheck@latest needs Go 1.26).
vulncheck:
	mkdir -p "$(GOCACHE_DIR)" "$(MODCACHE_DIR)"
	$(GO_MOD_RUN) go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Report files that `gofmt -s` would rewrite, using the pinned toolchain.
# Checks every tracked .go file outside vendor/ (so not the tmp/ scratch clones) with the pinned toolchain.
# Fails if git lists no files: inside a git worktree the .git link points outside the mount.
# Fails if gofmt does: a file that does not parse is not "formatted".
fmt-check:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) \
		sh -c 'files=$$(git -c safe.directory=/src ls-files "*.go" | grep -v ^vendor/); [ -n "$$files" ] || { echo "fmt-check: no .go files listed (a git worktree? its .git link does not resolve in the container)"; exit 1; }; out=$$(echo "$$files" | xargs gofmt -s -l) || { echo "$$out"; echo "^ gofmt failed (a file that does not parse?)"; exit 1; }; [ -z "$$out" ] || { echo "$$out"; echo "^ gofmt -s would rewrite these files"; exit 1; }'

# Rewrite files in place with `gofmt -s`, using the pinned toolchain, e.g.
#   make fmt-write FILES="backup/sync.go e2e_list_test.go"
fmt-write:
	@[ -n "$(FILES)" ] || { echo "fmt-write: set FILES"; exit 1; }
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) gofmt -s -w $(FILES)

# Build an older revision (Linux, pinned image) to tmp/zfsbackup-<REV>, e.g. `make build-rev REV=master`.
# The revision's vendor/ is used, so no network is needed. A "/" in REV (origin/master) is "-" in
# the file name. REV must name a commit: it is checked before any path is made from it.
REV_NAME = $(subst /,-,$(REV))
build-rev:
	@[ -n "$(REV)" ] || { echo "build-rev: set REV"; exit 1; }
	@git rev-parse --verify --quiet '$(REV)^{commit}' >/dev/null || { echo "build-rev: REV=$(REV) is not a commit"; exit 1; }
	rm -rf 'tmp/build-$(REV_NAME)' && mkdir -p 'tmp/build-$(REV_NAME)'
	git archive '$(REV)' | tar -x -C 'tmp/build-$(REV_NAME)'
	docker run --rm -v "$(CURDIR)/tmp/build-$(REV_NAME)":/src -v "$(GOCACHE_DIR)":/root/.cache/go-build -w /src \
		-e GOTOOLCHAIN=local -e GOFLAGS=-mod=vendor $(GO_IMAGE) go build -o zfsbackup .
	cp 'tmp/build-$(REV_NAME)/zfsbackup' 'tmp/zfsbackup-$(REV_NAME)'
	rm -rf 'tmp/build-$(REV_NAME)'

# Write a destination with the build of REV (see build-rev) to testdata/manifests/<REV>-<short hash>/.
# Commit the result with a README.md; never regenerate a committed fixture to match new code.
fixture-old: build-rev
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) \
		sh testdata/manifests/make-fixture.sh 'tmp/zfsbackup-$(REV_NAME)' "testdata/manifests/$(REV_NAME)-$$(git rev-parse --short '$(REV)')"

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

# test-run under the race detector, for a subset that test-race (~20 min) is too slow for.
test-race-run:
	go test -race -count=1 -run '$(RUN)' -v $(PKG)

# Compile every package and its tests on the host, integration files included.
vet:
	go vet -tags integration ./...

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

# Regenerate the OpenPGP keyring fixtures under testdata/pgp with the GnuPG
# shipped in the Go image (see testdata/pgp/generate.sh).
pgp-fixtures:
	docker run --rm -v "$(CURDIR)":/src -w /src $(GO_IMAGE) bash testdata/pgp/generate.sh

# Statement coverage by package, two profiles, in the pinned image (UTC, like test-one):
# unit = the package tests outside the root package; e2e = the root package's tests
# (the cobra commands against internal/fakezfs and file:// or in-memory s3://). The
# integration tests (build tag) are not included. Writes $(COVERAGE_DIR)/{unit,e2e}.out
# and prints the table of coverage-report.sh (the README's "Test Coverage" section) to
# $(COVERAGE_DIR)/report.md. A failing test does not stop the report: check the output.
COVERAGE_DIR?=tmp/coverage
coverage:
	mkdir -p "$(GOCACHE_DIR)" "$(COVERAGE_DIR)"
	docker run --rm -v "$(CURDIR)":/src -v "$(GOCACHE_DIR)":/root/.cache/go-build -w /src $(GO_IMAGE) sh -c '\
		unit=$$(go list ./... | grep -v "/zfsbackup-go$$"); \
		go test -count=1 -coverpkg=./... -coverprofile=$(COVERAGE_DIR)/unit.out $$unit; \
		go test -count=1 -coverpkg=./... -coverprofile=$(COVERAGE_DIR)/e2e.out .'
	sh coverage-report.sh $(COVERAGE_DIR)/unit.out $(COVERAGE_DIR)/e2e.out | tee $(COVERAGE_DIR)/report.md

# The integration tests (integration-vm, real zfs, MinIO, sshd) as a third profile,
# $(COVERAGE_DIR)/integration.out: COVER=1 builds the test binary with
# -cover -coverpkg=./... and the entrypoint writes the profile in the VM's
# /var/tmp/zfsbackup-it. A failing test does not stop the report; a run that
# writes no profile (a build error, a timeout, an os.Exit) does. Run make coverage
# first: the report has all three columns. Takes the VM for the whole run.
coverage-integration: integration-vm-up
	limactl shell '$(LIMA_VM)' -- sudo rm -f /var/tmp/zfsbackup-it/integration.out
	-$(MAKE) integration-vm COVER=1
	mkdir -p "$(COVERAGE_DIR)"
	limactl shell '$(LIMA_VM)' -- cat /var/tmp/zfsbackup-it/integration.out > $(COVERAGE_DIR)/integration.out.tmp
	mv $(COVERAGE_DIR)/integration.out.tmp $(COVERAGE_DIR)/integration.out
	sh coverage-report.sh $(COVERAGE_DIR)/unit.out $(COVERAGE_DIR)/e2e.out $(COVERAGE_DIR)/integration.out | tee $(COVERAGE_DIR)/report.md
