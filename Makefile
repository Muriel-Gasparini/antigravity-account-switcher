SHELL := /bin/bash
MODULE := github.com/Muriel-Gasparini/antigravity-account-switcher
CMD_PKG := ./cmd/antigravity-account-switcher
BINARY := bin/antigravity-account-switcher

GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.6 run ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w \
	-X main.Version=$(VERSION) \
	-X main.Commit=$(COMMIT) \
	-X main.Date=$(DATE)

.PHONY: all build build-static test test-race test-cover lint tidy clean run wrap help

all: build

## build: Compiles the single binary with CGO_ENABLED=0
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(CMD_PKG)
	@echo "Built $(BINARY) (CGO_ENABLED=0)"

## build-static: Compiles fully static stripped binary
build-static:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS) -extldflags '-static'" -o $(BINARY) $(CMD_PKG)
	@echo "Built static $(BINARY) (CGO_ENABLED=0)"

## test: Runs unit tests
test:
	go test -v -timeout=60s ./...

## test-race: Runs tests with Go data race detector (TSan)
test-race:
	go test -v -race -timeout=300s ./...

## test-cover: Runs test suite and generates HTML coverage report
test-cover:
	go test -v -race -coverprofile=coverage.txt -covermode=atomic ./...
	go tool cover -html=coverage.txt -o coverage.html
	@echo "Coverage report generated at coverage.html"

## lint: Runs code linters (go vet and golangci-lint)
lint:
	go vet ./...
	$(GOLANGCI_LINT)

## tidy: Ensures go.mod and go.sum consistency
tidy:
	GOTOOLCHAIN=local go mod tidy
	go mod verify

## clean: Removes build outputs and test artifacts
clean:
	rm -rf bin/ dist/ *.db *.db-wal *.db-shm coverage.txt coverage.html

## install: Installs the binary to ~/.local/bin
install: build
	@mkdir -p $(HOME)/.local/bin
	cp $(BINARY) $(HOME)/.local/bin/
	@echo "Installed $(BINARY) to $(HOME)/.local/bin/antigravity-account-switcher"

## uninstall: Removes the binary from ~/.local/bin
uninstall:
	rm -f $(HOME)/.local/bin/antigravity-account-switcher
	@echo "Uninstalled from $(HOME)/.local/bin/"

## run: Runs the switcher CLI directly
run:
	go run $(CMD_PKG) serve

## wrap: Executes wrapped command under switcher supervisor (e.g. make wrap CMD=agy)
wrap:
	go run $(CMD_PKG) wrap -- $(CMD)

## launch: Launches Antigravity IDE in ~/tools/Antigravity with coupled switcher
launch: build
	./scripts/launch-antigravity.sh
