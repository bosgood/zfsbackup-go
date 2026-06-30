# Dockerfile for running zfsbackup-go's unit tests in a reproducible container.
#
#   docker build -t zfsbackup-test .
#   docker run --rm zfsbackup-test
#
# The default command runs the unit tests via `go test ./...`. The ZFS
# integration tests in integration_test.go are gated behind the `integration`
# build tag, so they do NOT run here -- they shell out to `zfs`/`zpool` and
# expect a populated `tank/data@c` dataset, which needs ZFS + root on the host.
# To run them, build with `-tags integration` in a suitable environment.
#
# The cloud backend tests (backends/) use in-process mocks/servers by default
# and need no external services -- the ones that DO want a real endpoint
# self-skip when their env vars (AWS_S3_CUSTOM_ENDPOINT, AZURE_CUSTOM_ENDPOINT,
# GCS_FAKE_SERVER, B2_*) are unset.
#
# go.mod declares `go 1.18`; this image is newer and backward compatible
# (verified building/testing the module under Go 1.25).
FROM golang:1.23-bookworm

WORKDIR /src

# Install gox, the cross-compilation tool used by the Makefile build targets.
# It lands in $GOPATH/bin (/go/bin), which is on PATH, so `make build` works.
RUN go install github.com/mitchellh/gox@v1.0.1

# Download dependencies first so they are cached independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the rest of the source.
COPY . .

# Pre-compile packages and test binaries so `docker run` is fast and offline.
RUN go build ./... && go test -count=0 ./... >/dev/null 2>&1 || true

# Run the unit tests. The integration tests are excluded via build tags.
CMD ["go", "test", "./..."]
