#!/usr/bin/env bash
# tests/e2e/spec_suite_battery.sh — the QA-runnable spec-suite battery (TRBL-001, AC-31 wave).
#
# Runs from the repo root with no arguments and no dependencies beyond bash + python3
# (stdlib only, no network). Every assertion goes THROUGH the suite's own tools — this
# script never re-implements the spec parser.
#
#   check 1  specs/tools/selfcheck.py exits 0 with `RESULT: PASS` and lists AC-31
#   check 2  the AC universe is DERIVED from the matrix (selfcheck's coverage table has one
#            row per matrix row and reaches the matrix's highest AC — a hardcoded range fails)
#   check 3  specs/tools/ac_matrix.py exits 0, lists AC-31, finds no spec-declares-AC mismatch
#   check 4  matrix -> metadata drift (selfcheck step 2b) reports no failure and no UNMAPPED row
#
# Exit 0 = all four checks passed; non-zero at the first failure.

set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT="$(pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SELFCHECK="specs/tools/selfcheck.py"
AC_MATRIX="specs/tools/ac_matrix.py"
OUT="$TMP/selfcheck.txt"
AC_TXT="$TMP/ac_matrix.txt"
AC_JSON="$TMP/ac_matrix.json"
CURRENT=""
CURRENT_TOOL=""
PASSED=0

pass() {
    printf '  PASS  %s\n' "$1"
    PASSED=$((PASSED + 1))
}

fail() {
    printf '  FAIL  %s\n' "$1" >&2
    printf '\nFAILED after %s passing check(s) — command under test: %s\n' "$PASSED" "$CURRENT_TOOL" >&2
    exit 1
}

printf 'spec suite battery — %s\n' "$ROOT"

# ---------------------------------------------------------------- check 1
CURRENT="selfcheck runs and passes"
CURRENT_TOOL="python3 $SELFCHECK"
set +e
python3 "$SELFCHECK" >"$OUT" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || { grep -E '^FAILURES|^  x |^RESULT:' "$OUT" >&2 || true; fail "$CURRENT: exit $rc (expected 0)"; }
grep -q '^RESULT: PASS' "$OUT" || fail "$CURRENT: no 'RESULT: PASS' line"
grep -qE '^  AC-31 ' "$OUT" || fail "$CURRENT: AC-31 is missing from the AC coverage table"
pass "$CURRENT"

# ---------------------------------------------------------------- check 2
CURRENT="the AC universe is derived from the matrix, not from a literal range"
CURRENT_TOOL="python3 $AC_MATRIX --json"
set +e
python3 "$AC_MATRIX" --json >"$AC_JSON" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "$CURRENT: ac_matrix --json exited $rc"
ROWS_JSON="$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["rows"]))' "$AC_JSON")"
ROWS_CHECK="$(grep -cE '^  AC-[0-9]+ ' "$OUT")"
[ "$ROWS_JSON" = "$ROWS_CHECK" ] || \
    fail "$CURRENT: selfcheck's table has $ROWS_CHECK rows, the matrix has $ROWS_JSON"
TOP_JSON="$(python3 -c 'import json,sys; rows=json.load(open(sys.argv[1]))["rows"]; print(sorted(rows, key=lambda r: int(r["ac"].split("-")[1]))[-1]["ac"])' "$AC_JSON")"
TOP_CHECK="$(grep -oE '^  AC-[0-9]+' "$OUT" | tail -1 | tr -d ' ')"
[ "$TOP_JSON" = "$TOP_CHECK" ] || \
    fail "$CURRENT: selfcheck stops at $TOP_CHECK while the matrix reaches $TOP_JSON"
[ -n "$TOP_JSON" ] || fail "$CURRENT: the matrix has no AC rows"
pass "$CURRENT ($ROWS_CHECK ACs, last $TOP_CHECK)"

# ---------------------------------------------------------------- check 3
CURRENT="ac_matrix exits 0, lists AC-31 and reports no spec-declares-AC mismatch"
CURRENT_TOOL="python3 $AC_MATRIX"
set +e
python3 "$AC_MATRIX" >"$AC_TXT" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || { grep -E '^AC-31|^index ACs|mismatch' "$AC_TXT" >&2 || true; fail "$CURRENT: exit $rc (expected 0)"; }
grep -qE '^AC-31 ' "$AC_TXT" || fail "$CURRENT: no AC-31 row in the report"
grep -q 'spec-declares-AC mismatches: none' "$AC_TXT" || \
    fail "$CURRENT: ac_matrix reports a spec-declares-AC mismatch"
pass "$CURRENT"

# ---------------------------------------------------------------- check 4
CURRENT="matrix -> metadata drift (selfcheck step 2b)"
CURRENT_TOOL="python3 $SELFCHECK (step 2b)"
if grep -q 'does not declare it' "$OUT"; then
    grep 'does not declare it' "$OUT" >&2
    fail "$CURRENT: a spec named in an AC row does not declare that AC"
fi
if grep -q 'UNMAPPED' "$OUT"; then
    grep 'UNMAPPED' "$OUT" >&2
    fail "$CURRENT: an AC row has no owning spec"
fi
if grep -qE '^  x ' "$OUT"; then
    grep -E '^  x ' "$OUT" >&2
    fail "$CURRENT: selfcheck reported hard failures"
fi
pass "$CURRENT"

printf '\nRESULT: PASS — %s/4 checks green\n' "$PASSED"
