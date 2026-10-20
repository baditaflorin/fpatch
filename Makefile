# fpatch: build, test and cross-build helpers. Everything here is plain `go`
# plus POSIX shell; nothing needs network access.

BINARY    := fpatch
PKG       := ./cmd/fpatch
# VERSION may carry a leading "v" (VERSION=v0.1.0); the binary, `fpatch
# --version` and the archive names all use it without the "v".
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VER       := $(patsubst v%,%,$(VERSION))
LDFLAGS   := -s -w -X main.version=$(VER)
# GNU tar can be told to leave no timestamps, owners or ordering behind, which
# makes the archives (and so checksums.txt) reproducible; BSD tar cannot.
TAR_FLAGS := $(shell tar --version 2>/dev/null | grep -q 'GNU tar' && echo '--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner')
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
	@echo "make dist        archives for $(PLATFORMS) plus checksums.txt in dist/"
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

# dist cross-builds static binaries for every platform in PLATFORMS and packs
# each into dist/<tool>_<version>_<os>_<arch>.tar.gz together with LICENSE and
# README.md (all at the top level of the archive), then writes dist/checksums.txt
# as "<sha256>  <file>" lines. It only builds locally: publishing a release is a
# separate, deliberate step (see CONTRIBUTING.md). Plain Go, tar and gzip, plus
# sha256sum or shasum, whichever exists.
dist: clean-dist
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BINARY)_$(VER)_$${os}_$${arch}; \
		stage=dist/.stage-$$name; \
		rm -rf $$stage; mkdir -p $$stage; \
		echo "building $$name"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$stage/$(BINARY) $(PKG) || exit 1; \
		cp LICENSE README.md $$stage/; \
		( cd $$stage && tar $(TAR_FLAGS) -cf - $(BINARY) LICENSE README.md | gzip -n -9 > ../$$name.tar.gz ) || exit 1; \
		rm -rf $$stage; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum $(BINARY)_*.tar.gz > checksums.txt; else shasum -a 256 $(BINARY)_*.tar.gz > checksums.txt; fi
	@echo "wrote dist/checksums.txt"

.PHONY: clean-dist
clean-dist:
	rm -rf dist

clean: clean-dist
	rm -f $(BINARY)
