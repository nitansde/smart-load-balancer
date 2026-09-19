# Smart Load Balancer — CLIProxyAPI plugin build

PLUGIN_ID := smart-load-balancer
VERSION ?= 0.1.0
DIST := dist

GO := go

.PHONY: all build test vet fmt clean dist dist-linux dist-windows dist-darwin package zip help

all: build

## Build the plugin for the host platform
build:
	@set -e; \
	goos=$$($(GO) env GOOS); \
	ext=.so; [ "$$goos" = "darwin" ] && ext=.dylib; [ "$$goos" = "windows" ] && ext=.dll; \
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -o $(PLUGIN_ID)$$ext . && \
	echo "built $(PLUGIN_ID)$$ext"

## Run unit tests for the balancing core
test:
	$(GO) test ./balancer/

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf $(DIST) $(PLUGIN_ID).so $(PLUGIN_ID).dylib $(PLUGIN_ID).dll $(PLUGIN_ID).h

## Cross-compile release artifacts into $(DIST)/
##   dist-linux / dist-windows: buildable on Linux.
##     Requires: aarch64-linux-gnu-gcc (linux/arm64), mingw-w64 (windows/amd64),
##     zig (windows/arm64).
##   dist-darwin: requires a real macOS SDK — run on a Mac with Xcode CLT.
##     (zig's bundled macOS SDK has no frameworks, so cross-compiling darwin
##     dylibs from Linux is not supported.)
## Full 6-platform zips are assembled by the release workflow (ubuntu +
## macos jobs) via the `package` target.
dist: dist-linux dist-windows

dist-linux:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(GO) build -buildmode=c-shared -o $(DIST)/linux-amd64/$(PLUGIN_ID).so .
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc $(GO) build -buildmode=c-shared -o $(DIST)/linux-arm64/$(PLUGIN_ID).so .

dist-windows:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc $(GO) build -buildmode=c-shared -o $(DIST)/windows-amd64/$(PLUGIN_ID).dll .
	CGO_ENABLED=1 GOOS=windows GOARCH=arm64 CC="zig cc -target aarch64-windows-gnu" $(GO) build -buildmode=c-shared -o $(DIST)/windows-arm64/$(PLUGIN_ID).dll .

dist-darwin:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 $(GO) build -buildmode=c-shared -o $(DIST)/darwin-arm64/$(PLUGIN_ID).dylib .
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" $(GO) build -buildmode=c-shared -o $(DIST)/darwin-amd64/$(PLUGIN_ID).dylib .

## Package release zips in the CLIProxyAPI store layout from the already-built
## $(DIST)/<goos>-<goarch>/ libraries (no compilation here):
##   <id>_<version>_<goos>_<goarch>.zip  (library at zip root) + checksums.txt
package:
	@set -e; \
	for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64; do \
		goos=$${target%-*}; goarch=$${target#*-}; \
		ext=.so; [ "$$goos" = "darwin" ] && ext=.dylib; [ "$$goos" = "windows" ] && ext=.dll; \
		zipname="$(PLUGIN_ID)_$(VERSION)_$${goos}_$${goarch}.zip"; \
		rm -f "$(DIST)/$$zipname"; \
		(cd "$(DIST)/$$target" && zip -q -j "../$$zipname" "$(PLUGIN_ID)$$ext"); \
		echo "packed $(DIST)/$$zipname"; \
	done; \
	(cd $(DIST) && sha256sum *.zip > checksums.txt && cat checksums.txt)

## Local shortcut: build what this machine can and package it.
zip: dist package

help:
	@echo "Targets: build test vet fmt clean dist zip"
