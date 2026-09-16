# gpsync — build, test and packaging targets.
#
# Release artifacts are produced by .github/workflows/release.yml; these
# targets are what that workflow (and a developer) run locally.

MODULE  := github.com/gmizrahi/gpsync
BINARY  := gpsync
BIN     := bin
DIST    := dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# gpsync-tray is Windows-only: cmd/gpsync-tray sits entirely behind a
# //go:build windows constraint, so plain ./... builds skip it.
WIN_ENV := CGO_ENABLED=0 GOOS=windows GOARCH=amd64

.PHONY: all build test vet lint check windows tray dist deploy clean

all: check

## build: host binary for local development, written to bin/
build:
	@mkdir -p $(BIN)
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/$(BINARY) ./cmd/gpsync

## test: full test suite, no cache
test:
	go test ./... -count=1

vet:
	go vet ./...

## lint: golangci-lint if installed, otherwise a no-op with a hint
lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || \
		echo "golangci-lint not installed — skipping (https://golangci-lint.run)"

## check: what CI runs, and what to run before publishing
check: vet test build

## windows: cross-compile the CLI (no cgo, so this works from any host)
windows:
	@mkdir -p $(DIST)
	$(WIN_ENV) go build -ldflags "$(LDFLAGS)" -o $(DIST)/gpsync.exe ./cmd/gpsync

## tray: cross-compile the tray app; -H=windowsgui suppresses its console window
tray:
	@mkdir -p $(DIST)
	$(WIN_ENV) go build -ldflags "$(LDFLAGS) -H=windowsgui" -o $(DIST)/gpsync-tray.exe ./cmd/gpsync-tray

## dist: both Windows binaries, ready for packaging
dist: windows tray
	@ls -la $(DIST)

# Release artefacts for every supported platform. The tray app is Windows
# only; Linux and macOS get the CLI, which serves the same dashboard.
UNIX_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

## dist-all: binaries and archives for every supported platform
dist-all: dist
	@set -e; for t in $(UNIX_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -ldflags "$(LDFLAGS)" -o $(DIST)/gpsync-$$os-$$arch/gpsync ./cmd/gpsync; \
		cp README.md LICENSE $(DIST)/gpsync-$$os-$$arch/; \
		mkdir -p $(DIST)/gpsync-$$os-$$arch/packaging; \
		cp packaging/* $(DIST)/gpsync-$$os-$$arch/packaging/; \
		tar -czf $(DIST)/gpsync-$(VERSION)-$$os-$$arch.tar.gz -C $(DIST) gpsync-$$os-$$arch; \
		rm -rf $(DIST)/gpsync-$$os-$$arch; \
	done
	@mkdir -p $(DIST)/gpsync-windows-amd64
	@cp $(DIST)/gpsync.exe $(DIST)/gpsync-tray.exe README.md LICENSE $(DIST)/gpsync-windows-amd64/
	@cd $(DIST) && { command -v zip >/dev/null 2>&1 \
		&& zip -qr gpsync-$(VERSION)-windows-amd64.zip gpsync-windows-amd64 \
		|| python3 -m zipfile -c gpsync-$(VERSION)-windows-amd64.zip gpsync-windows-amd64; } \
		&& rm -rf gpsync-windows-amd64
	@cd $(DIST) && { command -v sha256sum >/dev/null 2>&1 \
		&& sha256sum gpsync-$(VERSION)-*.tar.gz gpsync-$(VERSION)-*.zip \
		|| shasum -a 256 gpsync-$(VERSION)-*.tar.gz gpsync-$(VERSION)-*.zip; } > checksums.txt
	@ls -la $(DIST)

## deploy: copy a fresh build into a local install directory
## Usage: make deploy GPSYNC_DEPLOY_DIR=/mnt/c/GPSync
deploy: dist
	@test -n "$(GPSYNC_DEPLOY_DIR)" || { echo "set GPSYNC_DEPLOY_DIR=<install dir>"; exit 1; }
	cp $(DIST)/gpsync.exe $(DIST)/gpsync-tray.exe "$(GPSYNC_DEPLOY_DIR)/"
	"$(GPSYNC_DEPLOY_DIR)/gpsync.exe" --version

clean:
	rm -rf $(DIST) $(BIN)
