GO ?= go
BIN ?= meshcontrol
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: all build test test-race vet bench clean cross

all: build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

bench:
	$(GO) test -run '^$$' -bench . ./...

cross:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/meshcontrol-linux-amd64 .
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/meshcontrol-linux-arm64 .
	GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/meshcontrol-darwin-arm64 .
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/meshcontrol-windows-amd64.exe .

clean:
	rm -f $(BIN) $(BIN).exe
	rm -rf dist
