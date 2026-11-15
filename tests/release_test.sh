#!/usr/bin/env bash
# Deterministic refusal and dry-run tests for scripts/release.sh.
# ch:trace row=RELEASE-015 spec=specs/SPEC-INDEX.md#3.3 wave=none:serial test=tests/release_test.sh doc=docs/operations.md evidence=scripts/release.sh witness=none:publication-owner-gated verdict=none:foreman-judge commit=none:foreman-integration memory=none:foreman-write
# STATUS: complete | partial | blocked
# TIER: T2
# EVIDENCE: scripts/release.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$ROOT/scripts/release.sh"
TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/trouble-release-test.XXXXXX")
mkdir -p "$TMP_ROOT/gh-bin"
ln -s "$ROOT/tests/release_gh_fixture.sh" "$TMP_ROOT/gh-bin/gh"
trap 'rm -rf "$TMP_ROOT"' EXIT

fail() {
  printf 'release-test: FAIL: %s\n' "$*" >&2
  exit 1
}

make_fixture() {
  local name=$1
  local repo="$TMP_ROOT/$name"
  mkdir -p "$repo/scripts"
  git init -q "$repo"
  git -C "$repo" config user.email release-test@example.invalid
  git -C "$repo" config user.name release-test
  cp "$SCRIPT" "$repo/scripts/release.sh"
  chmod +x "$repo/scripts/release.sh"
  cp "$ROOT/CHANGELOG.md" "$repo/CHANGELOG.md"
  touch "$repo/README.md"
  git -C "$repo" add scripts/release.sh CHANGELOG.md README.md
  git -C "$repo" commit -qm fixture
  git init -q --bare "$TMP_ROOT/$name-origin.git"
  git -C "$repo" remote add origin "$TMP_ROOT/$name-origin.git"
  git -C "$repo" push -q -u origin HEAD:main
  printf '%s\n' "$repo"
}

DRY_REPO=$(make_fixture dry)
DRY_SHA=$(git -C "$DRY_REPO" rev-parse HEAD)
BEFORE=$(git -C "$DRY_REPO" status --porcelain=v1 --untracked-files=all)
PATH="$TMP_ROOT/gh-bin:$PATH" "$DRY_REPO/scripts/release.sh" --dry-run --version v0.1.1
AFTER=$(git -C "$DRY_REPO" status --porcelain=v1 --untracked-files=all)
[[ "$BEFORE" == "$AFTER" ]] || fail "dry-run changed the fixture working tree"
git -C "$DRY_REPO" diff --exit-code >/dev/null

printf 'release-test: dry-run preserves a byte-identical clean tree\n'

touch "$DRY_REPO/dirty.txt"
if PATH="$TMP_ROOT/gh-bin:$PATH" "$DRY_REPO/scripts/release.sh" --dry-run --version v0.1.1; then
  fail "dirty tree was accepted"
fi
printf 'release-test: dirty tree refused\n'

TAG_REPO=$(make_fixture tag)
TAG_SHA=$(git -C "$TAG_REPO" rev-parse HEAD)
git -C "$TAG_REPO" tag v0.1.1
if PATH="$TMP_ROOT/gh-bin:$PATH" "$TAG_REPO/scripts/release.sh" --dry-run --version v0.1.1; then
  fail "existing local tag was accepted"
fi
printf 'release-test: existing local tag refused\n'

CI_REPO=$(make_fixture ci)
if PATH=/usr/bin:/bin "$CI_REPO/scripts/release.sh" --dry-run --version v0.1.1; then
  fail "missing exact-SHA CI was accepted"
fi
printf 'release-test: missing exact-SHA CI refused\n'

CHANGELOG_REPO=$(make_fixture changelog)
cp "$ROOT/tests/fixtures/no-unreleased-changelog.md" "$CHANGELOG_REPO/CHANGELOG.md"
git -C "$CHANGELOG_REPO" add CHANGELOG.md
git -C "$CHANGELOG_REPO" commit -qm 'remove unreleased section'
CHANGELOG_SHA=$(git -C "$CHANGELOG_REPO" rev-parse HEAD)
git -C "$CHANGELOG_REPO" push -q origin HEAD:main
if PATH="$TMP_ROOT/gh-bin:$PATH" "$CHANGELOG_REPO/scripts/release.sh" --dry-run --version v0.1.1; then
  fail "missing CHANGELOG [Unreleased] was accepted"
fi
printf 'release-test: missing CHANGELOG [Unreleased] refused\n'

printf 'release-test: PASS\n'
