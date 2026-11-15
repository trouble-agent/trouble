#!/usr/bin/env bash
# Prepare a release candidate without tagging or publishing it.
#
# Usage: scripts/release.sh --dry-run --version vX.Y.Z
#        scripts/release.sh --version vX.Y.Z
#
# The non-dry-run path only creates local artifacts under dist/. Tagging,
# pushing, registry publication, and GitHub Release creation remain owner-gated.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GO=${GO:-go}
MAKE=${MAKE:-make}
DRY_RUN=0
VERSION=""

fail() {
  printf 'release: ERROR: %s\n' "$*" >&2
  exit 2
}

usage() {
  printf '%s\n' \
    'usage: scripts/release.sh [--dry-run] --version vX.Y.Z' \
    '' \
    'Prepare release artifacts without creating or moving tags and without publishing.'
}

while (($#)); do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --version)
      (($# >= 2)) || fail "--version requires vX.Y.Z"
      VERSION=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1"
      ;;
  esac
done

[[ -n "$VERSION" ]] || fail "--version vX.Y.Z is required"
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "version must match vX.Y.Z: $VERSION"

cd "$ROOT"
command -v git >/dev/null 2>&1 || fail "git is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"

HEAD_SHA=$(git rev-parse HEAD 2>/dev/null) || fail "not a git checkout"
ORIGIN_URL=$(git remote get-url origin 2>/dev/null || true)
[[ -n "$ORIGIN_URL" ]] || fail "origin remote is required for release provenance"

if [[ -n "$(git status --porcelain=v1 --untracked-files=all)" ]]; then
  fail "working tree is dirty; commit all changes before preparing a release"
fi

if git tag --list "$VERSION" | grep -Fqx "$VERSION"; then
  fail "local tag already exists: $VERSION (tags are never moved or deleted)"
fi

REMOTE_TAGS=$(git ls-remote --tags "$ORIGIN_URL" "refs/tags/$VERSION" "refs/tags/$VERSION^{}" 2>/dev/null || true)
[[ -z "$REMOTE_TAGS" ]] || fail "remote tag already exists: $VERSION (tags are never moved or deleted)"

ci_green() {
  command -v gh >/dev/null 2>&1 || {
    printf 'exact-SHA CI: unavailable (gh is not installed)\n' >&2
    return 1
  }

  local result
  result=$(gh run list --commit "$HEAD_SHA" --limit 100 \
    --json headSha,status,conclusion \
    --jq 'any(.[]; .headSha == "'"$HEAD_SHA"'" and .status == "completed" and .conclusion == "success")' 2>/dev/null) || return 1
  [[ "$result" == true ]]
}

if ! ci_green; then
  fail "no completed green CI run exists for exact HEAD SHA $HEAD_SHA"
fi

if ! grep -q '^## \[Unreleased\][[:space:]]*$' CHANGELOG.md; then
  fail "CHANGELOG.md is missing the exact [Unreleased] section"
fi

if ((DRY_RUN)); then
  printf 'release: DRY RUN (working tree will not be changed)\n'
  printf 'release: version              %s\n' "$VERSION"
  printf 'release: source SHA           %s\n' "$HEAD_SHA"
  printf 'release: origin               %s\n' "$ORIGIN_URL"
  printf 'release: planned checks       clean tree, absent local/remote tag, exact-SHA green CI, [Unreleased]\n'
  printf 'release: planned provenance   clean detached clone at %s\n' "$HEAD_SHA"
  printf 'release: planned build        make release (8 binaries + manifest.json)\n'
  printf 'release: planned archives     trouble-%s-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64}.tar.gz\n' "$VERSION"
  printf 'release: planned checksums     dist/checksums.txt\n'
  printf 'release: publication          owner authorization required; no tag, push, or publication is performed\n'
  exit 0
fi

TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/trouble-release.XXXXXX")
trap 'rm -rf "$TMP_ROOT"' EXIT
STAGE="$TMP_ROOT/source"
DIST_STAGE="$TMP_ROOT/dist"

printf 'release: cloning clean source at %s\n' "$HEAD_SHA"
git clone --no-local --no-hardlinks "$ROOT" "$STAGE" >/dev/null 2>&1
git -C "$STAGE" checkout --detach "$HEAD_SHA" >/dev/null 2>&1
[[ "$(git -C "$STAGE" rev-parse HEAD)" == "$HEAD_SHA" ]] || fail "clean clone did not resolve exact HEAD SHA"
[[ -z "$(git -C "$STAGE" status --porcelain=v1 --untracked-files=all)" ]] || fail "clean clone is dirty"

mkdir -p "$DIST_STAGE"
printf 'release: building the eight binaries from the detached source\n'
VERSION="$VERSION" GIT_SHA="$HEAD_SHA" DIST="$DIST_STAGE" "$MAKE" -C "$STAGE" release >/dev/null

for target in linux-amd64 linux-arm64 darwin-arm64 windows-amd64; do
  ext=""
  [[ "$target" == windows-amd64 ]] && ext=.exe
  [[ -s "$DIST_STAGE/$target/troubled$ext" ]] || fail "missing daemon binary for $target"
  [[ -s "$DIST_STAGE/$target/trouble$ext" ]] || fail "missing CLI binary for $target"
  archive="trouble-${VERSION}-${target}.tar.gz"
  printf 'release: archiving %s\n' "$archive"
  tar -czf "$DIST_STAGE/$archive" -C "$DIST_STAGE" "$target"
done

[[ -s "$DIST_STAGE/manifest.json" ]] || fail "make release did not produce dist/manifest.json"
(
  cd "$DIST_STAGE"
  files=(manifest.json trouble-"${VERSION}"-linux-amd64.tar.gz trouble-"${VERSION}"-linux-arm64.tar.gz \
    trouble-"${VERSION}"-darwin-arm64.tar.gz trouble-"${VERSION}"-windows-amd64.tar.gz \
    linux-amd64/troubled linux-amd64/trouble linux-arm64/troubled linux-arm64/trouble \
    darwin-arm64/troubled darwin-arm64/trouble windows-amd64/troubled.exe windows-amd64/trouble.exe)
  sha256sum "${files[@]}" > checksums.txt
)

rm -rf "$ROOT/dist"
cp -a "$DIST_STAGE" "$ROOT/dist"

printf 'release: prepared dist/ (8 binaries, 4 archives, manifest.json, checksums.txt)\n'
printf 'release: publication is not performed; owner authorization is still required for tag/push/release\n'
