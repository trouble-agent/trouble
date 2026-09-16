# trouble — build entry points (SPEC-INDEX §7; the modules land area by area).
#
# Every binary is version-stamped at link time (SPEC-12 §3.4): an unstamped build
# is visibly degraded rather than silently fine — /health.json reports
# status="degraded" with detail.reason="unstamped_build" and `trouble install`
# refuses to enable the unit unless --force.

GO ?= go
BIN ?= bin

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
GIT_SHA    ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%S.000Z)

LIFECYCLE_PKG = github.com/totalwindupflightsystems/trouble/internal/lifecycle
LDFLAGS = -s -w \
	-X $(LIFECYCLE_PKG).Version=$(VERSION) \
	-X $(LIFECYCLE_PKG).GitSHA=$(GIT_SHA) \
	-X $(LIFECYCLE_PKG).BuildTime=$(BUILD_TIME)

.PHONY: all build bin test race vet fmt schema schema-check conformance check smoke clean

all: build

# Compile every package (no linking of the commands' stamped binaries).
build:
	$(GO) build ./...

# The two shipped binaries, stamped (SPEC-12 §3.4 build line).
bin:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/troubled ./cmd/troubled
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/trouble ./cmd/trouble
	@echo "built $(BIN)/troubled $(BIN)/trouble $(VERSION) $(GIT_SHA)"

test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

race:
	CGO_ENABLED=0 $(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

# Regenerates internal/registry/schema/*.json from the shipped descriptors.
schema:
	$(GO) run ./internal/registry/schemagen/gen internal/registry/schema

# A descriptor edit without a regenerated schema is a build failure (SPEC-06 §3.2).
schema-check: schema
	git diff --exit-code internal/registry/schema/

# The one conformance gate (SPEC-06 §2.4).
conformance:
	$(GO) test ./internal/registry/... -count=1 -race
	$(GO) test ./internal/registry/testkit/... -run 'TestHarnessRejectsNonIdempotent|TestHarnessRejectsNoCheckMode|TestSchemaDialectClosed' -count=1

# The v0.1 exit gate: the spec-suite self-consistency loop (SPEC-INDEX §7) plus the
# two subsystem gates that own a cross-spec contract.
check:
	python3 specs/tools/selfcheck.py
	$(MAKE) schema-check
	$(MAKE) vet
	$(GO) test -count=1 ./internal/...

# Boot the real daemon against a throwaway state root and prove the health
# surface, the stall checker and the config explain dump all answer (see
# internal/app/e2e_test.go for the same run in-process).
smoke: bin
	$(BIN)/trouble --version
	$(BIN)/trouble config explain --json | head -5
	$(BIN)/trouble topology | head -5

clean:
	rm -rf $(BIN)
