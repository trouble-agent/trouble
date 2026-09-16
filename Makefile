# trouble — build entry points (SPEC-INDEX §7; the modules land area by area).

GO ?= go

.PHONY: build test race vet fmt schema schema-check conformance

build:
	$(GO) build ./...

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
