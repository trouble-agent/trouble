# syntax=docker/dockerfile:1
#
# trouble — container image (TRBL-002 part B; SPEC-12 §3.4 build line,
# SPEC-10 §3.4 dashboard token file, SPEC-13 server profiles).
#
# Stage 1 builds both shipped binaries from source with the SPEC-10/SPEC-12
# pinned flags (CGO_ENABLED=0 -trimpath -ldflags "-s -w") and the three
# lifecycle.Version/GitSHA/BuildTime stamps. An UNSTAMPED build is degraded by
# design — /health.json reports status="degraded" detail.reason="unstamped_build"
# — so GitSHA may never come out as the literal "unknown": the build args are
# overridable, the fallback derives the sha from the build context's .git, and
# the last resort is a literal, obvious placeholder ("nogit00") rather than the
# sentinel the validator treats as unstamped.
#
# Stage 2 is distroless/static-debian12:nonroot: no shell, no package manager,
# no libc beyond the static binary's needs. That is the deliberate choice over
# `scratch` because distroless still ships an /etc/passwd with the nonroot uid
# (65532), a CA bundle and tzdata, so the daemon can run unprivileged and still
# do TLS + local-time arithmetic without a hand-rolled FROM scratch layer.

ARG GO_IMAGE=golang:1.26-bookworm

# --------------------------------------------------------------------------
# Stage 1 — builder
# --------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build
WORKDIR /src

# Stamping inputs. Empty means "derive it": VERSION from git describe,
# GIT_SHA from the build context, BUILD_TIME from the builder clock.
ARG VERSION=
ARG GIT_SHA=
ARG BUILD_TIME=

# The module download layer is cached and only invalidated when the module
# files change — it must come before COPY . . so source edits do not refetch.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN set -eux; \
    git config --global --add safe.directory /src 2>/dev/null || true; \
    version="${VERSION}"; \
    if [ -z "$version" ]; then \
        version="$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)"; \
    fi; \
    sha="${GIT_SHA}"; \
    if [ -z "$sha" ]; then \
        sha="$(git rev-parse --short=7 HEAD 2>/dev/null || true)"; \
    fi; \
    if [ -z "$sha" ] || [ "$sha" = "unknown" ]; then \
        echo "WARN: no GIT_SHA build arg and no usable .git in the context;" >&2; \
        echo "WARN: stamping the literal placeholder 'nogit00' (a build must never carry GitSHA=unknown)." >&2; \
        sha="nogit00"; \
    fi; \
    bt="${BUILD_TIME}"; \
    if [ -z "$bt" ]; then \
        bt="$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"; \
    fi; \
    echo "stamp: version=$version git_sha=$sha build_time=$bt"; \
    LDFLAGS="-s -w -X github.com/totalwindupflightsystems/trouble/internal/lifecycle.Version=$version -X github.com/totalwindupflightsystems/trouble/internal/lifecycle.GitSHA=$sha -X github.com/totalwindupflightsystems/trouble/internal/lifecycle.BuildTime=$bt"; \
    mkdir -p /out/bin /out/stage/data/state; \
    chmod 0700 /out/stage/data/state; \
    chmod 0755 /out/stage/data; \
    tar -tvf /dev/null 2>/dev/null || true; \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/bin/troubled ./cmd/troubled; \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/bin/trouble ./cmd/trouble; \
    /out/bin/trouble --version; \
    file /out/bin/troubled 2>/dev/null || true

# --------------------------------------------------------------------------
# Stage 2 — runtime
# --------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Only the two binaries. --chown is required because distroless has no shell,
# so a later RUN chown is impossible.
COPY --from=build --chown=65532:65532 /out/bin/troubled /usr/local/bin/troubled
COPY --from=build --chown=65532:65532 /out/bin/trouble /usr/local/bin/trouble

# The runtime state dir: /data/state, mode 0700, owned by uid 65532.
#
# MODE AND OWNERSHIP BOTH MATTER, and neither can be fixed with a RUN chmod on
# distroless (no shell). Measured, twice:
#   * the daemon refuses a state_root that is not 0700
#     (TROUBLE-LIFECYCLE-005: `state_root "/data" mode is 0755, want 0700`);
#   * a fresh named volume's ROOT (/data) is created root:root 0755 by the
#     volume driver — only the image directory's CONTENTS are copied in — so
#     /data itself is neither 0700 nor writable by uid 65532.
# Hence state_root is /data/state (see deploy/container/config.toml): the
# subdirectory ships in the image as 0700 + 65532, `COPY` of a directory copies
# its contents with their mode/owner intact, and the daemon writes there
# unprivileged with no host-side chown.
COPY --from=build --chown=65532:65532 /out/stage/data /data

# The container never runs as root: distroless :nonroot already sets
# uid/gid 65532, restated here so `docker inspect` shows it unambiguously.
USER 65532:65532

# 7643 ingest, 7644 dashboard. Never 3000, never 8642/8643 (Hermes gateway),
# never 9090 (the coding-hermes scheduler).
EXPOSE 7643 7644

# HEALTHCHECK: distroless has neither shell nor curl, so the health probe is
# the CLI itself (`trouble check-stall`), which is the SPEC-05 external stall
# checker and therefore exactly the thing a container healthcheck should ask.
HEALTHCHECK --interval=30s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/trouble", "check-stall", "--health-url", "http://127.0.0.1:7644/health.json", "--json"]

# Daemon entry point: bin/troubled --config <path>. The config path is the
# default; compose and `docker run` override it with their own mounted file.
ENTRYPOINT ["/usr/local/bin/troubled"]
CMD ["--config", "/etc/trouble/config.toml"]
