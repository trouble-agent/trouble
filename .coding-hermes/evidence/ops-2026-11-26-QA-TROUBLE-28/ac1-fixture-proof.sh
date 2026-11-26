#!/usr/bin/env bash
# AC1 fixture proof: a store whose LAST LINE has no trailing newline,
# then one canonical append -> the result must be exactly two physical
# lines (no glue), both valid JSON.
set -u
D=/home/kara/.hermes/cache/scratch/ops-2026-11-26
F="$D/fixture-torn-append.jsonl"
REC="$D/fixture-record.json"

printf '{"tick":"fixture-pre","n":1}' > "$F"   # no trailing newline
printf '{"tick":"fixture-new","n":2}' > "$REC"

echo "before: last_byte=$(tail -c 1 "$F" | od -An -c | tr -d ' ') lines=$(wc -l < "$F")"
python3 /home/kara/.hermes/scripts/qa_ledger_append.py "$F" "$REC" || exit 1
echo "after_lines=$(wc -l < "$F")"
python3 /home/kara/.hermes/scripts/jsonl-line-guard.py "$F"; guard_rc=$?
echo "guard_rc=$guard_rc"
echo "ac1_fixture_ok=$([ "$guard_rc" -eq 0 ] && [ "$(wc -l < "$F")" -eq 2 ] && echo true || echo false)"
