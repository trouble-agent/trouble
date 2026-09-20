#!/usr/bin/env bash
# upgrade_cross_version.sh — run the SPEC-12 §3.6 prev→next upgrade path as an
# OPERATOR would, from two genuinely STAMPED builds, and print the ledger chain.
#
# Why this exists on top of internal/lifecycle/upgrade_version_test.go's
# TestCrossVersionUpgradePath: that test builds and drives two real binaries but
# runs under `go test`, whose process carries NO §3.4 link-time triple — so the
# one thing it cannot show is a record whose actor IS the stamped build. This
# script compiles the package's own test binary with the stamps set on it
# (TROUBLE_TEST_STAMPED=1 picks up TestUpgradeCrossVersionRecordChain) and then
# runs the recipe as a second, independent process against a scratch state root.
#
# Both builds come from THIS tree with different -X flags, which is how a release
# build is made. NO git tag is created and nothing is pushed: tagging is a
# release act the owner performs, and the path this script proves needs two
# stamped builds, not two tags.
#
# Usage: scripts/upgrade_cross_version.sh [scratch-dir]
# Exit:  0 = every assertion held · 1 = an assertion failed · 2 = a precondition
set -uo pipefail

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '   ok   %s\n' "$*"; }
bad()  { printf '   FAIL %s\n' "$*"; FAIL=1; }
FAIL=0

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LIFECYCLE_PKG="github.com/trouble-agent/trouble/internal/lifecycle"
GO=${GO:-go}

# The scratch state root must NOT live under /tmp: the ledger refuses a root
# there by design (SPEC-01 §4.3, SPEC-12 §3.2). $HOME/.local/state is the base
# the project's own tests use.
SCRATCH="${1:-${HOME}/.local/state/trouble-upgrade-cross-version-$$}"
mkdir -p "$SCRATCH"
chmod 700 "$SCRATCH"
trap 'rm -rf "$SCRATCH"' EXIT

stamp() { # stamp BIN VERSION SHA
  $GO build -trimpath \
    -ldflags "-s -w -X ${LIFECYCLE_PKG}.Version=$2 -X ${LIFECYCLE_PKG}.GitSHA=$3 -X ${LIFECYCLE_PKG}.BuildTime=2026-09-19T00:00:00.000Z" \
    -o "$1" ./cmd/troubled 2>/dev/null
}

say "build two stamped binaries from this tree (v0.0.9 → v0.1.0)"
cd "$ROOT" || exit 2
stamp "$SCRATCH/troubled.v0.0.9" "v0.0.9" "deadbee" || { bad "build v0.0.9"; exit 1; }
stamp "$SCRATCH/troubled.v0.1.0" "v0.1.0" "abc1234" || { bad "build v0.1.0"; exit 1; }
PREV=$("$SCRATCH/troubled.v0.0.9" --version)
NEXT=$("$SCRATCH/troubled.v0.1.0" --version)
echo "   v0.0.9: $PREV"
echo "   v0.1.0: $NEXT"
case "$PREV" in v0.0.9\ deadbee*) ok "the previous build reports its own stamp" ;; *) bad "previous build stamp: $PREV" ;; esac
case "$NEXT" in v0.1.0\ abc1234*) ok "the staged build reports its own stamp" ;; *) bad "staged build stamp: $NEXT" ;; esac

say "install the previous release on the live path and run the recipe's ledger half"
LIVE="$SCRATCH/bin/troubled"
mkdir -p "$SCRATCH/bin"
cp "$SCRATCH/troubled.v0.0.9" "$LIVE"
chmod 755 "$LIVE"
LIVE_V=$("$LIVE" --version)
echo "   $LIVE --version: $LIVE_V"
case "$LIVE_V" in v0.0.9\ deadbee*) ok "the live path carries the PREVIOUS release" ;; *) bad "live path reports: $LIVE_V" ;; esac

# The stamped probe lives in the lifecycle package's own test binary: build it
# with the NEW release's triple so the ACTOR on every record it writes is a
# release build, and run it with TROUBLE_TEST_STAMPED=1.
$GO test -c -o "$SCRATCH/lifecycle.test" \
  -ldflags "-X ${LIFECYCLE_PKG}.Version=v0.1.0 -X ${LIFECYCLE_PKG}.GitSHA=abc1234 -X ${LIFECYCLE_PKG}.BuildTime=2026-09-19T00:00:00.000Z" \
  ./internal/lifecycle 2>/dev/null || { bad "build the stamped probe binary"; exit 1; }
TROUBLE_TEST_STAMPED=1 TROUBLE_PROBE_ROOT="$SCRATCH" "$SCRATCH/lifecycle.test" \
  -test.run 'TestUpgradeCrossVersionRecordChain' -test.v 2>&1 | sed 's/^/   /'
PROBE_RC=${PIPESTATUS[0]}
[[ $PROBE_RC -eq 0 ]] && ok "the stamped probe passed" || bad "the stamped probe exited $PROBE_RC"

# The probe renamed the staged build over its own live path ($SCRATCH/live):
# the running release is now v0.1.0, and it says so itself.
PROBE_LIVE="$SCRATCH/live/troubled"
if [[ -x "$PROBE_LIVE" ]]; then
  AFTER_V=$("$PROBE_LIVE" --version 2>/dev/null)
  echo "   $PROBE_LIVE --version (after): $AFTER_V"
  case "$AFTER_V" in v0.1.0\ abc1234*) ok "the upgrade put v0.1.0 on the live path" ;; *) bad "live path after the upgrade reports: $AFTER_V" ;; esac
  BACKUP=$(ls "$SCRATCH"/backups/bin/* 2>/dev/null | head -1)
  [[ -n "$BACKUP" ]] && ok "the previous release is recoverable at $BACKUP" || bad "no backup under $SCRATCH/backups/bin"
else
  bad "the probe left no live binary at $PROBE_LIVE"
fi

say "the upgrade ledger chain (read straight out of the file)"
LEDGER_FILE=$(ls "$SCRATCH"/ledger/*.jsonl 2>/dev/null | head -1)
if [[ -z "$LEDGER_FILE" ]]; then
  bad "no ledger file under $SCRATCH/ledger"
else
  python3 - "$LEDGER_FILE" <<'PY'
import json, sys
path = sys.argv[1]
rows = []
for line in open(path, encoding="utf-8"):
    line = line.strip()
    if not line:
        continue
    try:
        rec = json.loads(line)
    except json.JSONDecodeError:
        continue
    p = rec.get("payload") or {}
    if p.get("stage") == "upgrade":
        rows.append((rec.get("actor", {}), p))
if not rows:
    print("   FAIL no stage=upgrade record in the ledger")
    sys.exit(1)
for actor, p in rows:
    print("   {} step={} from={} to={} parked={} actor={} {}".format(
        "rec", p.get("step"), p.get("from_version"), p.get("to_version"),
        p.get("parked", "-"), actor.get("version"), actor.get("git_sha")))
steps = [p.get("step") for _, p in rows]
print("   chain: " + " -> ".join(steps))
missing = [i for i, (_, p) in enumerate(rows) if not p.get("from_version") or not p.get("to_version")]
if missing:
    print("   FAIL {} record(s) with an empty from_version/to_version".format(len(missing)))
    sys.exit(1)
if [p.get("version") for a, p in rows] and not all(a.get("version") and a.get("git_sha") for a, _ in rows):
    print("   FAIL a record carries an incomplete §3.4 actor triple")
    sys.exit(1)
bad_ver = [(p.get("from_version"), p.get("to_version")) for _, p in rows
           if p.get("from_version") != "v0.0.9" or p.get("to_version") != "v0.1.0"]
if bad_ver:
    print("   FAIL unexpected version pair(s): {}".format(bad_ver))
    sys.exit(1)
print("   ok   every upgrade record is versioned v0.0.9 -> v0.1.0 with a complete actor triple")
PY
  [[ $? -eq 0 ]] && ok "ledger chain verified" || bad "ledger chain verification"
fi

say "result"
if [[ $FAIL -eq 0 ]]; then
  echo "   PASS — the prev-release → HEAD upgrade path is real, versioned and stamped"
else
  echo "   FAIL — see the assertions above"
fi
exit $FAIL
