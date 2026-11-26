# ops tick 2026-11-26-07-18-55 — QA-TROUBLE-28 evidence

Tick: trouble-ops-2026-11-26-07-18-55, actor ops, namespace trouble.
Row: QA-TROUBLE-28 (HARNESS/QA-LEDGER machine-parseability of ~/.hermes/qa/ledger.jsonl).

## Live state at fire time (all numbers re-measured this fire)

- Store: ~/.hermes/qa/ledger.jsonl, 3,100,439 bytes at measurement, mtime 2026-11-26 02:16:34 -05.
  Two writers landed DURING the tick (02:16 logsey-qa record, 02:23 +590 bytes) — the store is live.
- Damage (recurrence named by the COS note 2026-11-25T00:55Z, now re-measured): ONE pretty-printed
  JSON object (json.dumps indent=2) written by asce-qa-2026-11-23-15-39-10 refire-census cycle 4
  (ts 2026-11-23T16:23:24Z) spans PHYSICAL lines 995-1020; 25 of those 26 lines fail a per-line
  json.loads parse (line 1018 is a bare JSON string and parses). Full guard capture:
  guard-pre-repair-full.txt (45 findings over 1051 lines, rc=1, naming 995-1020 minus 1018).
- Gate arm 1 (final byte): `tail -c 1 | od -An -c` printed `\n` — trailing newline PRESENT.

## Repair (bounded, reversible: timestamped pre-repair backup kept)

repair-ledger-pretty-block.py: block [995,1020] re-serialized to ONE compact newline-terminated
line; atomic os.replace after a concurrent-write re-check (size+mtime unchanged).
- backup: /home/kara/.hermes/qa/ledger.jsonl.pre-repair-20261126T072513Z (3,101,039 bytes)
- lines 1050 -> 1025; post-repair per-line parse: 0 invalid; final byte newline.
- Multiset verify (verify-repair.py): stream-parse the BACKUP vs per-line parse the LIVE store —
  1025 objects before == 1025 after, canonical-sha256 multiset EQUAL. rc=0.

## Guard shipped (AC2 mechanics)

~/.hermes/scripts/jsonl-line-guard.py (copy in this dir): per-line integrity guard for ANY JSONL
store. Exits 1 naming file+line when a physical line holds 2+ JSON values (torn-append glue), does
not parse (pretty-printed fragment), or the final byte is not a newline. Proven:
- live PRE-repair store: rc=1 naming the real damage (guard-pre-repair-full.txt);
- live POST-repair store: `GUARD PASS ... 1026 lines, 1025 objects` rc=0 (repair-and-proofs.txt);
- synthetic glued fixture: rc=1 naming line 2 (2+ values on one line) + missing final newline;
- synthetic healthy fixture: rc=0. (make-fixtures.py, repair-and-proofs.txt)
Two guard defects were caught and fixed during self-test: raw_decode does not skip leading
whitespace (false-flagged indented bare-string line 1018), and the multi-value check fired one
iteration too late (glued lines were not flagged at all) — the fixture battery is what caught both.

## Canonical appender shipped (AC1 mechanics)

~/.hermes/scripts/qa_ledger_append.py (copy in this dir): compact json + trailing newline + fsync;
if the store's last byte is not a newline it terminates the torn line first, so the new record
lands on its OWN physical line. AC1 fixture (ac1-fixture-proof.sh): store whose last line has NO
trailing newline + one canonical append -> exactly TWO physical lines, both valid, guard rc=0,
ac1_fixture_ok=true. First run of this proof caught the helper gluing (write used the pre-newline
buffer) — fixed and re-proven.

## Cadence (AC2 scheduling leg)

Cron job 480e3d4466d9 "QA ledger JSONL line guard", enabled, deliver=local,
schedule 7 8 * * * HOST LOCAL (next run 2026-11-26T08:07:00-05:00): runs the guard on the live
store daily; GUARD PASS answers one line; a FAIL quotes the offending line numbers and names the
writer tick, and repairs stay with the owning lane. A torn append therefore cannot survive a full
QA cycle (~daily) without the guard naming it. model/provider snapshots left null deliberately:
the prompt is model-agnostic and null resolves to the live gateway default at fire time; the fleet
cron watchdog catches provider failures.

## Arm grading (literal)

- AC1 appender single-line behavior: PROVEN by fixture (two physical lines from a torn store).
  The historical appender population (per-tick inline scripts) cannot be fixed retroactively; the
  canonical helper is the durable write path going forward.
- AC2 guard exists, exits non-zero naming line numbers, reusable for any JSONL store: PROVEN.
  "Runs often enough": cron 480e3d4466d9 wired (daily); observed-across-a-full-cycle: PENDING
  (first fire 2026-11-26T08:07-05).
- AC3 guard reports exactly lines 759/766 against ledger.jsonl.pre-repair-20261115T092218Z:
  UNMEASURABLE AS WRITTEN — that backup file no longer exists (pruned; only the two 2026-11-12
  pre-repair backups survive, both parse clean: 672 and 693 valid lines, 0 invalid). The arm's
  intent (guard detects REAL historical damage and names the lines) is proven instead against the
  live 995-1020 damage pre-repair.
- GATE ARM final byte newline: PROVEN (was already `\n` pre-repair; verified again post-repair).

Row status: kept PENDING on AC3 (fixture gone) + AC2 scheduling observation; not closed this fire.

## Environment deltas (disclosed)

- NEW host files: ~/.hermes/scripts/jsonl-line-guard.py, ~/.hermes/scripts/qa_ledger_append.py.
- NEW cron: 480e3d4466d9 (delete to revert).
- NEW store backup: ~/.hermes/qa/ledger.jsonl.pre-repair-20261126T072513Z.
- Live store repaired in place (multiset-equal, see above).
- Evidence digests: see the board event detail (sha256 over this dir, computed post-scan).
