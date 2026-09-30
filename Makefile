# GOWORK=off everywhere on purpose: a workspace would resolve bcommon to a
# local checkout, so a go.mod skew would stay invisible here. What is tested
# is what go.mod pins.
#
# The TypeScript package under host/ needs Node 24 and its dependencies
# installed once (cd host && npm ci). The targets run tsc and node directly,
# so NODE may name a Node 24 binary that is not on PATH:
#
#	make verify NODE=/path/to/node24

.PHONY: verify test go-test ts-test ts-check ts-build node-check vet fmt-check deps-check vectors vectors-check

NODE ?= node
TSC := $(NODE) node_modules/typescript/bin/tsc

verify: fmt-check vet deps-check ts-check vectors-check test

# Go and TypeScript. The Go tests read the TypeScript-written vectors under
# testdata/ts, and the TypeScript tests read the Go-generated ones under
# testdata/vectors: each side decodes the other's bytes.
test: go-test ts-test

go-test:
	GOWORK=off go test -race -count=2 ./...

vet:
	GOWORK=off go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

# The direct dependencies, asserted rather than intended:
#  - go.mod and go.sum are tidy, and no module is replaced;
#  - the direct set is exactly go-sdk and bcommon;
#  - go-sdk resolves to exactly v1.5.2, the version bcommon pins and the
#    vectors were made with. Minimal version selection takes the higher of
#    two pins, so a library asking for a later go-sdk would change what
#    bbox ships without a line changing here.
DEPS_DIRECT := github.com/bsv-blockchain/go-sdk github.com/lightwebinc/bcommon
DEPS_SDK    := github.com/bsv-blockchain/go-sdk v1.5.2

deps-check:
	@set -eu; \
	if ! GOWORK=off go mod tidy -diff >/dev/null; then \
		echo "deps-check: go.mod or go.sum is not tidy; run go mod tidy"; exit 1; \
	fi; \
	replaced=$$(GOWORK=off go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all); \
	if [ -n "$$replaced" ]; then echo "deps-check: no module may be replaced, found: $$replaced"; exit 1; fi; \
	direct=$$(GOWORK=off go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}{{end}}' all | sed '/^$$/d' | sort); \
	want=$$(printf '%s\n' $(DEPS_DIRECT) | sort); \
	if [ "$$direct" != "$$want" ]; then \
		echo "deps-check: the direct dependencies must be exactly:"; echo "$$want" | sed 's/^/  /'; \
		echo "found:"; echo "$$direct" | sed 's/^/  /'; exit 1; \
	fi; \
	sdk=$$(GOWORK=off go list -m $(firstword $(DEPS_SDK))); \
	if [ "$$sdk" != "$(DEPS_SDK)" ]; then echo "deps-check: go-sdk must resolve to exactly $(lastword $(DEPS_SDK)), found: $$sdk"; exit 1; fi

node-check:
	@$(NODE) -e 'process.exit(process.versions.node.split(".")[0] === "24" ? 0 : 1)' || \
		{ echo "host/ needs Node 24; set NODE=/path/to/node"; exit 1; }

ts-check: node-check
	cd host && $(TSC) --noEmit

ts-build: node-check
	cd host && $(NODE) -e "require('node:fs').rmSync('dist', { recursive: true, force: true })" && $(TSC)

ts-test: ts-build
	cd host && $(NODE) --test "dist/**/*.test.js"

# Write both vector sets: the Go codec's under testdata/vectors, the
# TypeScript codec's under testdata/ts. A change to either is a change to
# the contract's goldens.
vectors: ts-build
	GOWORK=off go run ./cmd/vectors
	cd host && $(NODE) dist/tsvectors.js

# Regenerate both in memory and compare byte for byte; write nothing.
vectors-check: ts-build
	GOWORK=off go run ./cmd/vectors -check
	cd host && $(NODE) dist/tsvectors.js -check
