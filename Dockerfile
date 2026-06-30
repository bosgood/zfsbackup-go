# Dockerfile for running zfsbackup-go's unit tests in a reproducible container.
#
#   docker build -t zfsbackup-test .
#   docker run --rm zfsbackup-test
#
# The default command runs every package's unit tests EXCEPT the root `main`
# package. That root package (integration_test.go) shells out to `zfs`/`zpool`
# and expects a populated `tank/data@c` dataset, so it can only run on a host
# with ZFS + root and is not a unit test. Everything else (backends/, backup/)
# uses in-process mocks/servers and needs no external services -- the cloud
# backend tests that DO want a real endpoint self-skip when their env vars
# (AWS_S3_CUSTOM_ENDPOINT, AZURE_CUSTOM_ENDPOINT, GCS_FAKE_SERVER, B2_*) are
# unset.
#
# go.mod declares `go 1.18`; this image is newer and backward compatible
# (verified building/testing the module under Go 1.25).
FROM golang:1.23-bookworm

WORKDIR /src

# Download dependencies first so they are cached independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the rest of the source.
COPY . .

# Pre-compile packages and test binaries so `docker run` is fast and offline.
RUN go build ./... && go test -count=0 ./... >/dev/null 2>&1 || true

# Run the unit tests, excluding the root `main` (integration) package.
CMD ["sh", "-c", "go test $(go list ./... | grep -v '/zfsbackup-go$')"]
