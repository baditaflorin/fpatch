# fpatch: build, test and cross-build helpers. Everything here is plain `go`
# plus POSIX shell; nothing needs network access.

BINARY    := fpatch
PKG       := ./cmd/fpatch
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
FUZZTIME  ?= 30s

.PHONY: all build test race vet fmt fmt-check check fuzz dist clean help

all: check build

help:
	@echo "make build       build ./$(BINARY) for this machine"
	@echo "make test        run the tests"
	@echo "make race        run the tests with the race detector"
	@echo "make check       gofmt check, go vet, tests"
	@echo "make fuzz        run every fuzz target for FUZZTIME (default $(FUZZTIME))"
	@echo "make dist        cross-build $(PLATFORMS) into dist/ with SHA256SUMS"
	@echo "make clean       remove build outputs"

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

check: fmt-check vet test

fuzz:
	go test -run '^$$' -fuzz '^FuzzParse$$' -fuzztime $(FUZZTIME) ./internal/spec
	go test -run '^$$' -fuzz '^FuzzReplace$$' -fuzztime $(FUZZTIME) ./internal/patch
	go test -run '^$$' -fuzz '^FuzzUnified$$' -fuzztime $(FUZZTIME) ./internal/diff
	go test -run '^$$' -fuzz '^FuzzShQuote$$' -fuzztime $(FUZZTIME) ./fsys

# dist cross-builds static binaries for every platform in PLATFORMS and writes
# a SHA256SUMS file next to them. It only builds locally; publishing a release
# is a separate, deliberate step (see CONTRIBUTING.md).
dist: clean-dist
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)_$(VERSION)_$${os}_$${arch}; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum $(BINARY)_* > SHA256SUMS; else shasum -a 256 $(BINARY)_* > SHA256SUMS; fi
	@echo "wrote dist/SHA256SUMS"

.PHONY: clean-dist
clean-dist:
	rm -rf dist

clean: clean-dist
	rm -f $(BINARY)
