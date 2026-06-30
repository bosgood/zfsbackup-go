TARGETS="freebsd/amd64 linux/amd64"
COMMIT_HASH=`git rev-parse --short HEAD 2>/dev/null`

check: lint test test-race

fmt:
	@for d in $(DIRS) ; do \
		if [ "`gofmt -s -w $$d/*.go | tee /dev/stderr`" ]; then \
			echo "^ error formatting go files" && echo && exit 1; \
		fi \
	done

lint:
	@if [ "`golangci-lint run | tee /dev/stderr`" ]; then \
		echo "^ golangci-lint errors!" && echo && exit 1; \
	fi

get:
	go get -v -d -t ./...

test:
	go test ./...

test-race:
	go test -race ./...

# Integration tests are gated behind the `integration` build tag and are NOT
# run by `test`/`test-race`. They require a host with ZFS (zfs/zpool binaries,
# loaded kernel module, root) and a `tank/data@c` dataset to copy from.
integration:
	go test -tags integration -v ./...

test-docker:
	docker build -t zfsbackup-test . && docker run --rm zfsbackup-test

build:
	${GOPATH}/bin/gox -ldflags="-w -s -X github.com/someone1/zfsbackup-go/config.GitCommit=${COMMIT_HASH}" -osarch=${TARGETS}

build-dev:
	${GOPATH}/bin/gox -ldflags="-X github.com/someone1/zfsbackup-go/config.GitCommit=${COMMIT_HASH}" -osarch=${TARGETS} -output="{{.Dir}}_{{.OS}}_{{.Arch}}-${COMMIT_HASH}"
