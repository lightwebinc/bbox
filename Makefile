# GOWORK=off everywhere on purpose: a workspace would resolve bcommon to a
# local checkout, so a go.mod skew would stay invisible here. What is tested
# is what go.mod pins.
#
# The TypeScript package under host/ needs Node 24 and its dependencies
# installed once (cd host && npm ci). The targets run tsc and node directly,
# so NODE may name a Node 24 binary that is not on PATH:
#
#	make verify NODE=/path/to/node24

.PHONY: verify test go-test ts-test ts-check ts-build module node-check vet fmt-check deps-check licences licences-update vectors vectors-check e2e e2e-client bbox devchain docker-build docker-build-host docker-smoke docker-smoke-host quickstart-check

NODE ?= node
TSC := $(NODE) node_modules/typescript/bin/tsc

VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE          ?= ghcr.io/lightwebinc/bbox
DEVCHAIN_IMAGE ?= ghcr.io/lightwebinc/bbox-devchain
HOST_IMAGE     ?= ghcr.io/lightwebinc/bbox-host

verify: fmt-check vet deps-check licences ts-check vectors-check test

# Go and TypeScript. The Go tests read the TypeScript-written vectors under
# testdata/ts, and the TypeScript tests read the Go-generated ones under
# testdata/vectors: each side decodes the other's bytes.
test: go-test ts-test

go-test:
	GOWORK=off go test -race -count=2 ./...

# The e2e build tag is vetted too, so the client end to end compiles.
vet:
	GOWORK=off go vet ./...
	GOWORK=off go vet -tags e2e ./cmd/bbox/

# The command, and the local chain, into bin/.
bbox:
	GOWORK=off go build -o bin/bbox ./cmd/bbox

devchain:
	GOWORK=off go build -o bin/devchain ./cmd/devchain

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

# The direct dependencies, asserted rather than intended:
#  - go.mod and go.sum are tidy, and no module is replaced;
#  - the direct set is exactly go-sdk and bcommon;
#  - go-sdk resolves to exactly v1.7.1, the version bcommon pins and the
#    vectors were made with. Minimal version selection takes the higher of
#    two pins, so a library asking for a later go-sdk would change what
#    bbox ships without a line changing here.
DEPS_DIRECT := github.com/bsv-blockchain/go-sdk github.com/lightwebinc/bcommon
DEPS_SDK    := github.com/bsv-blockchain/go-sdk v1.7.1

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

# LICENSE-THIRD-PARTY is generated from what the binaries link, so a new
# dependency brings its licence obligation into the file rather than
# leaving it undischarged. licences fails when the file is stale.
licences:
	python3 scripts/gen-third-party-licenses.py . --check

licences-update:
	python3 scripts/gen-third-party-licenses.py .

node-check:
	@$(NODE) -e 'process.exit(process.versions.node.split(".")[0] === "24" ? 0 : 1)' || \
		{ echo "host/ needs Node 24; set NODE=/path/to/node"; exit 1; }

ts-check: node-check
	cd host && $(TSC) --noEmit

# tsc into host/dist, then the deployable bundle into host/bundle.
ts-build: node-check
	cd host && $(NODE) -e "require('node:fs').rmSync('dist', { recursive: true, force: true })" && $(TSC) && $(NODE) scripts/bundle.js

# The host module a reference overlay host loads: one file,
# host/bundle/bbox-module.js, with bcommon inlined and nothing imported but
# @bsv/sdk (the host's own copy) and Node built-ins. It is copied into the
# host's tree and named by absolute path in OVERLAY_MODULES (docs/host.md).
# Its tests (make test) mount that file and run golden vectors through it.
module: ts-build

ts-test: ts-build
	cd host && $(NODE) --test "dist/**/*.test.js"

# The module end to end on a local reference overlay host loading the
# bundle, staged beside the host's node_modules as a deployment places it:
# submit, lookup, a receipt, a sweep, the suppression attack, a priced
# question paid through its 402, a restart and the restored index. Not part
# of verify: it needs Docker (for MySQL) and a reference host directory
# holding its compiled dist/index.js and its node_modules.
#
#	make e2e REFERENCE_HOST=/path/to/reference-host
e2e: ts-build
	@test -n "$(REFERENCE_HOST)" || { echo "set REFERENCE_HOST=/path/to/reference-host"; exit 1; }
	cd host && $(NODE) dist/e2e.js $(abspath $(REFERENCE_HOST))

# The bbox command end to end against two local reference hosts loading the
# bundle, over a local chain (internal/testchain) serving the node's API and
# the header source: send on a stand-in plane and in unicast, both hosts
# agreeing on the box, read and ack, a payment inside an envelope
# internalized, a paid history question (402) and the payee settling it, a
# drop, a host restarted and restored, and a send persisted while a host was
# down and published by the next. Not part of verify: it needs Docker (for
# MySQL) and a reference host directory holding its compiled dist/index.js
# and its node_modules.
#
#	make e2e-client REFERENCE_HOST=/path/to/reference-host
e2e-client: ts-build
	@test -n "$(REFERENCE_HOST)" || { echo "set REFERENCE_HOST=/path/to/reference-host"; exit 1; }
	BBOX_E2E_REFERENCE_HOST=$(abspath $(REFERENCE_HOST)) BBOX_E2E_NODE=$(NODE) \
		GOWORK=off go test -tags e2e -count=1 -v -run 'TestClientE2E$$' ./cmd/bbox/

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

# The images, built and tagged locally, each as $(VERSION) and latest.
# Nothing here pushes. docker-build makes the command and the local chain;
# the host image needs the reference overlay host image it is built on:
#
#	make docker-build-host REFERENCE_HOST_IMAGE=<reference overlay host image>
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
	docker build --build-arg VERSION=$(VERSION) --target devchain -t $(DEVCHAIN_IMAGE):$(VERSION) -t $(DEVCHAIN_IMAGE):latest .
	@docker image ls --format '{{.Repository}}:{{.Tag}}  {{.Size}}' | grep -E '^($(IMAGE)|$(DEVCHAIN_IMAGE)):$(VERSION) '

docker-build-host:
	@test -n "$(REFERENCE_HOST_IMAGE)" || { echo "set REFERENCE_HOST_IMAGE to the reference overlay host image the module is added to"; exit 1; }
	docker build -f host/Dockerfile --build-arg REFERENCE_HOST_IMAGE=$(REFERENCE_HOST_IMAGE) -t $(HOST_IMAGE):$(VERSION) -t $(HOST_IMAGE):latest .
	@docker image ls --format '{{.Repository}}:{{.Tag}}  {{.Size}}' | grep -E '^$(HOST_IMAGE):$(VERSION) '

# The command's image with no network at all: the stamped version, help, a
# first run in a fresh named volume mounted where the documentation says,
# and a read with no hosts refused before any request.
docker-smoke: docker-build
	@set -eu; img=$(IMAGE):$(VERSION); id=bbox-smoke-$$$$; \
	docker volume create $$id-home >/dev/null; trap 'docker volume rm -f $$id-home >/dev/null' EXIT; \
	run() { docker run --rm --network none -v $$id-home:/home/nonroot/.bbox $$img "$$@"; }; \
	echo "== -version"; out=$$(run -version); echo "$$out"; \
	[ "$$out" = "bbox $(VERSION)" ] || { echo "docker-smoke: want bbox $(VERSION)"; exit 1; }; \
	echo "== help"; run help | head -1; \
	echo "== init"; run -network regtest init; \
	echo "== office new"; run office new smoke | grep -E '^(office|topic|host) '; \
	echo "== doctor"; out=$$(run -network regtest doctor); echo "$$out"; \
	echo "$$out" | grep -q '^headers     NOT CONFIGURED' || { echo "docker-smoke: doctor output unexpected"; exit 1; }; \
	echo "== a list with no hosts exits 2 before any request"; \
	rc=0; out=$$(run -header-url http://192.0.2.1 -office smoke_abcdefghij list 2>&1) || rc=$$?; echo "$$out"; \
	[ "$$rc" = 2 ] && echo "$$out" | grep -q 'no overlay host configured' || { echo "docker-smoke: want exit 2 naming hosts, got $$rc"; exit 1; }; \
	echo "docker-smoke: ok ($$img)"

# The host image loads the module with the host's own @bsv/sdk, and refuses
# to start with no office configured.
docker-smoke-host:
	@set -eu; img=$(HOST_IMAGE):$(VERSION); \
	echo "== the module imports from the host's node_modules"; \
	docker run --rm --network none --entrypoint node $$img -e \
		"import('/app/modules/bbox/bbox-module.js').then(m => { if (typeof m.default !== 'function') process.exit(1); console.log('bbox module: default export is the factory') })"; \
	echo "== no BBOX_OFFICES: exit 64 with the reason"; \
	rc=0; out=$$(docker run --rm --network none $$img 2>&1) || rc=$$?; echo "$$out"; \
	[ "$$rc" = 64 ] && echo "$$out" | grep -q 'BBOX_OFFICES is empty' || { echo "docker-smoke-host: want exit 64, got $$rc"; exit 1; }; \
	echo "docker-smoke-host: ok ($$img)"

# QUICKSTART.md, run as written, in the quickstart compose project. Needs
# the three images (docker-build, docker-build-host).
quickstart-check:
	scripts/quickstart-check.sh
