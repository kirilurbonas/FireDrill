BINARY := bin/firedrill
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/kirilurbonas/FireDrill/pkg/version.Version=$(VERSION)"

.PHONY: build test lint e2e clean

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/firedrill

test:
	go test ./... -count=1

e2e:
	go test ./... -count=1 -tags e2e -run E2E -v

lint:
	golangci-lint run ./...

clean:
	rm -rf bin evidence
