GO            ?= go
GOLANGCI_LINT ?= golangci-lint

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: all build test lint vet fmt check clean

all: check

## build: compile every package and command into bin/
build:
	@mkdir -p bin
	$(GO) build -o bin/ ./...

## test: run the whole test suite, uncached
test:
	$(GO) test ./... -count=1

## lint: run golangci-lint over every package
lint:
	$(GOLANGCI_LINT) run ./...

## vet: run go vet over every package
vet:
	$(GO) vet ./...

## fmt: format every Go file in place
fmt:
	gofmt -s -w .

## check: the gate every change must pass — lint, then the whole suite
check: lint test

clean:
	rm -rf bin/ build/ coverage.txt
