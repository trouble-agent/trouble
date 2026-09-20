# trouble — build entry points (SPEC-INDEX §7; the modules land area by area).
#
# Every binary is version-stamped at link time (SPEC-12 §3.4): an unstamped build
# is visibly degraded rather than silently fine — /health.json reports
# status="degraded" with detail.reason="unstamped_build" and `trouble install`
# refuses to enable the unit unless --force.

GO ?= go
BIN ?= bin
DIST ?= dist

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
GIT_SHA    ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%S.000Z)

LIFECYCLE_PKG = github.com/trouble-agent/trouble/internal/lifecycle
LDFLAGS = -s -w \
	-X $(LIFECYCLE_PKG).Version=$(VERSION) \
	-X $(LIFECYCLE_PKG).GitSHA=$(GIT_SHA) \
	-X $(LIFECYCLE_PKG).BuildTime=$(BUILD_TIME)

# The release matrix (TRBL-002): exactly these four (goos/goarch) pairs, both
# shipped binaries each, plus a checksum manifest. A release without checksums is
# not a release, so `release` writes $(DIST)/manifest.json and exits non-zero if
# any target failed to build.
RELEASE_TARGETS = linux/amd64 linux/arm64 darwin/arm64 windows/amd64

# sha256sum on Linux, shasum on the BSDs/macOS. Resolved once at parse time.
SHA256 := $(shell command -v sha256sum 2>/dev/null || command -v shasum 2>/dev/null || echo sha256sum)

# The host-measured gates (TRBL-004). Each one asserts a number the HOST
# produces — amortized ledger throughput, a per-call derivation budget — so each
# fences itself through internal/loadfence: at a load_avg of 45 or more the miss
# is reported as an explicit SKIP naming the observed load_avg and the measured
# value instead of a red that would misreport host load as a regression. Below
# the fence the same run fails exactly as it always has.
#
# The list lives here (one place) so `check-host` can run these gates verbosely:
# `go test` hides the output of a skipped test without -v, and the whole point of
# the fence is that the SKIP verdict is VISIBLE and is not read as a failure.
# HOST_GATE_NAMES is the report's guard against this list and the -run pattern
# drifting apart — a listed gate with no verdict fails the step instead of
# silently not running.
HOST_MEASURED_GATES = TestAmortizedThroughput|TestPerLineRegression|TestDeriveNeverBlocks|TestCollectorLongLineIsTruncated
HOST_GATE_PKGS = ./internal/ledger/... ./internal/research/... ./internal/sentinel/...
HOST_GATE_NAMES = TestAmortizedThroughput TestPerLineRegression TestDeriveNeverBlocks TestCollectorLongLineIsTruncated

.PHONY: all build bin release test race vet fmt schema schema-check conformance check check-host smoke smoke-e2e ac-matrix clean

all: build

# Compile every package (no linking of the commands' stamped binaries).
build:
	$(GO) build ./...

# The two shipped binaries, stamped (SPEC-12 §3.4 build line).
bin:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/troubled ./cmd/troubled
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/trouble ./cmd/trouble
	@echo "built $(BIN)/troubled $(BIN)/trouble $(VERSION) $(GIT_SHA)"

# The cross-platform release matrix (TRBL-002). One deterministic tree —
# $(DIST)/<goos>-<goarch>/<binary>[.exe] — for the four targets above, stamped
# exactly like `bin` (SPEC-12 §3.4: CGO_ENABLED=0, -trimpath, -ldflags).
# Sequential on purpose: no make -j, no shell backgrounding. Every target is
# attempted so the log names all failures, the recipe still exits non-zero, and
# the manifest is written ONLY when every target built (a partial manifest would
# read like a release). The unstamped-build invariant is untouched: GIT_SHA stays
# `unknown` at HEAD-less builds and /health.json keeps reporting degraded.
release:
	@set -u; \
	rm -rf $(DIST); \
	mkdir -p $(DIST); \
	failed=""; \
	for t in $(RELEASE_TARGETS); do \
	  goos=$${t%/*}; goarch=$${t#*/}; \
	  out=$(DIST)/$$goos-$$goarch; \
	  ext=""; if [ "$$goos" = windows ]; then ext=".exe"; fi; \
	  mkdir -p $$out; \
	  if CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $$out/troubled$$ext ./cmd/troubled \
	     && CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $$out/trouble$$ext ./cmd/trouble; then \
	    echo "release: ok   $$goos/$$goarch -> $$out/troubled$$ext $$out/trouble$$ext"; \
	  else \
	    echo "release: FAIL $$goos/$$goarch (go build exited non-zero)"; \
	    failed="$$failed $$t"; \
	  fi; \
	done; \
	if [ -n "$$failed" ]; then \
	  echo "release: FAILED (no manifest written) for:$$failed"; \
	  exit 1; \
	fi; \
	mf=$(DIST)/manifest.json; \
	{ \
	  echo '{'; \
	  echo "  \"version\": \"$(VERSION)\","; \
	  echo "  \"git_sha\": \"$(GIT_SHA)\","; \
	  echo "  \"build_time\": \"$(BUILD_TIME)\","; \
	  echo '  "artifacts": ['; \
	} > $$mf; \
	first=1; \
	for t in $(RELEASE_TARGETS); do \
	  goos=$${t%/*}; goarch=$${t#*/}; \
	  out=$(DIST)/$$goos-$$goarch; \
	  ext=""; if [ "$$goos" = windows ]; then ext=".exe"; fi; \
	  for b in troubled trouble; do \
	    f=$$out/$$b$$ext; \
	    sha=$$($(SHA256) $$f | cut -d' ' -f1); \
	    sz=$$(wc -c < $$f | tr -d ' '); \
	    if [ $$first -eq 0 ]; then echo ',' >> $$mf; fi; \
	    first=0; \
	    printf '    {"name": "%s", "goos": "%s", "goarch": "%s", "path": "%s", "sha256": "%s", "size": %s}' \
	      "$$b$$ext" "$$goos" "$$goarch" "$$f" "$$sha" "$$sz" >> $$mf; \
	  done; \
	done; \
	printf '\n  ]\n}\n' >> $$mf; \
	echo "release: wrote $$mf ($$(grep -c '"name"' $$mf) artifacts, $(VERSION) $(GIT_SHA))"

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
# two subsystem gates that own a cross-spec contract, plus the host-measured gates
# verbosely (so their fence verdict — PASS or an explicit SKIP — is part of the
# check output rather than hidden by `go test`'s non-verbose mode).
check:
	python3 specs/tools/selfcheck.py
	$(MAKE) schema-check
	$(MAKE) vet
	$(GO) test -count=1 ./internal/...
	$(MAKE) check-host

# The host-measured gates, verbosely, with their fence verdicts echoed. A failure
# below the fence exits non-zero; an explicit SKIP (host past the fence) exits 0 —
# a SKIP is not a failure, that is what the fence is for (TRBL-004). The step also
# fails if a listed gate produced no verdict at all, so the gate list and the -run
# pattern cannot drift apart silently.
check-host:
	@out=$$(mktemp); \
	echo "host-measured gates — fence: internal/loadfence.FenceLoadAvg, this host: load_avg $$(cut -d' ' -f1 /proc/loadavg)"; \
	$(GO) test -count=1 -v -run '$(HOST_MEASURED_GATES)' $(HOST_GATE_PKGS) > $$out 2>&1; rc=$$?; \
	grep -E '^--- (PASS|SKIP|FAIL): |load_avg' $$out | sed 's/^/  /'; \
	missing=""; \
	for g in $(HOST_GATE_NAMES); do \
	  grep -qE "^--- (PASS|SKIP|FAIL): $$g " $$out || missing="$$missing $$g"; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "host-measured gates: NO VERDICT for$$missing — HOST_GATE_NAMES and HOST_MEASURED_GATES have drifted"; \
	  rm -f $$out; exit 1; \
	fi; \
	skips=$$(grep -c '^--- SKIP: ' $$out || true); \
	echo "host-measured gates: rc=$$rc, $$skips explicit SKIP verdict(s) (SKIP is not a failure; FAIL below the fence is)"; \
	rm -f $$out; exit $$rc

# Boot the real daemon against a throwaway state root and prove the health
# surface, the stall checker and the config explain dump all answer (see
# internal/app/e2e_test.go for the same run in-process).
smoke: bin
	$(BIN)/trouble --version
	$(BIN)/trouble config explain --json | head -5
	$(BIN)/trouble topology | head -5

# The live operator smoke: 24 assertions against the two shipped binaries, from
# the version triple to the checker's exit 0 → exit 8 transition (tests/e2e/).
smoke-e2e: bin
	bash tests/e2e/cli_smoke.sh $(BIN)

# The v0.1 exit gate: the AC matrix over the SPEC-INDEX contract.
ac-matrix:
	python3 specs/tools/ac_matrix.py

clean:
	rm -rf $(BIN) $(DIST)
