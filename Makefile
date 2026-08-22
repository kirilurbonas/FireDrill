BINARY := bin/firedrill
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/kirilurbonas/FireDrill/pkg/version.Version=$(VERSION)"

.PHONY: build test lint e2e clean

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/firedrill

test:
	go test ./... -count=1

# -timeout: the e2e suite provisions real containers and clusters; the
# default 10m per package is not enough for the drill package's fleet.
e2e:
	go test ./... -count=1 -tags e2e -run E2E -v -timeout 40m

lint:
	golangci-lint run ./...

clean:
	rm -rf bin evidence
