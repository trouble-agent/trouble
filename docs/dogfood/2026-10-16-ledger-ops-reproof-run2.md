# Dogfood run 2 — 2026-10-16 — install-docs drift, HEAD-hint allocator, and the TRBL-100/101 re-proof at HEAD

Sibling of `2026-10-16-ledger-ops-reproof.md` (run 1, same tick
`2026-10-16-11-33-24`). Run 1 swept the ledger-ops CLI surface and filed
TRBL-104..108; this run started independently on the same delta
(c190bdb..6bf0ade) and, per the same-tick-sibling rule, files only the
genuinely-new findings (TRBL-109..111) and records re-proof extensions as
sub-evidence of run 1's rows — no re-filing.

## What this run independently re-proved (extends run 1's rows)

- **TRBL-104 extended**: the default-root ledger carries 7 `trouble-ledger-cli`
  boot notes from THIS tick alone, written by verb invocations whose `--config`
  did not resolve (see TRBL-111) — the write-on-read behavior is not confined
  to a scratch ledger; it has already written into the fleet's live default
  ledger.
- **TRBL-105 extended**: the refusal fires even ~1s after `SIGINT` (daemon
  still draining), so the docs' "run this after it stops" guidance needs a
  wait-for-exit step; and after full exit the verbs DO read (status rc=0) —
  the post-shutdown happy path works.
- **TRBL-106 ROOT CAUSE PROVEN BYTE-EXACT** (run 1 observed; this run localized):
  the writer hashes `canonicalPayloadJSON` over the payload map as the CALLER
  built it — `[]types.ConfigValue` structs encode in DECLARED field order
  (key,value,source,source_ref,redacted) — while Verify decodes each line into
  `map[string]any` (recordHeader.Payload → decodeRecord) and re-marshals, which
  Go emits with SORTED keys (key,redacted,source,source_ref,value). Repro with
  the real seq-2 payload from the fresh scratch ledger: re-marshal = d1206f7e…,
  stored hash = eaa9299b… (no match), struct-order recomputation =
  eaa9299b… (EXACT match). The stored hash is honest; the verifier's
  re-canonicalization is the defect. Affects every payload ≥4096B that embeds
  Go structs — at HEAD that is exactly the config boot snapshot
  (`internal/lifecycle/explain.go` WriteConfigRecord). Corroborated on the
  fleet DEFAULT ledger: all 9 hash-mismatch seqs (2,5,8,189,287,484,596,679,803)
  are kind=config, and the default root holds exactly 9 config records ≥4096B —
  the false-positive set is 100% of the affected class, zero false negatives
  observed. Same deterministic pair reproduced on a VIRGIN fresh-install ledger
  in the bunker leg (agent seqs 2 and 1110, both config-kind).
- **TRBL-101 re-proof at HEAD**: torn-shaped fragments (open-object cut, the
  byte shape `tornShaped` accepts — verified with a copied predicate probe:
  `{"seq":123,…,"kind":"event",` + newline ⇒ io.ErrUnexpectedEOF ⇒ torn) are
  healed at boot (`lifecycle{op:"recover_torn"}` note, fragments removed,
  ledger stays jq-clean), while complete-on-the-wire garbage
  (`not-json-at-all`) correctly survives as 011 corrupt and is never rewritten.
  Healing is real and correctly discriminating.
- **TRBL-103 re-proof at HEAD** (run 1 drove it too): boot4 log line
  `WARN dashboard token store: skipping malformed token entry index=0
  id=probe-b-badhash reason="malformed entry (empty id or bad hash shape)"` —
  the fail-closed quarantine works.

## Fresh findings (filed as TRBL-109..111)

1. **TRBL-109 (P1) install-docs placeholder drift**: deploy/README.md's
   quickstart teaches `replace-with-a-new-32-lowercase-hex-character-key`, but
   examples/config.toml ships `0123456789abcdef0123456789abcdef`. Verbatim
   quickstart on a fresh bunker agent: boot healthy in 2s, first event
   `401 {"causes":["unknown_key"],"detail":"unknown public key"}`; patching the
   key that is ACTUALLY in the file → `200` in 1s; wrong-key → `401` (fail-closed
   auth itself is correct). Every verbatim user eats a 401 with no hint why.
2. **TRBL-110 (P0) HEAD-hint-seeded allocation**: with the scratch file at max
   seq 394 and HEAD hint 628, the first post-boot allocation was 629 — the
   allocator continued from the HINT, seeding a real seq hole (verify:
   `seq_holes=[{from:395,to:628}]`). SPEC-01 §3.4 rule 7 ("HEAD is never a
   source of truth") is violated on the write path. The 10-13 run's fake-tail
   result (99999 NOT adopted) tested a different input variable; the stale-HEAD
   variable flips the outcome. Nothing warns or heals at boot.
3. **TRBL-111 (P2) default-root fallback + write-on-read amplification**:
   verb invocations whose config failed to resolve silently opened the DEFAULT
   root and appended boot notes there (7 on the fleet ledger this tick);
   ~25 verb invocations across the tick produced 61 boot notes on one scratch
   ledger. A monitoring loop at 1/min would write 1440 records/day into the
   ledger it monitors.

## Install leg (fresh machine)

bunker-mvp agent **df-trouble-1016** (bunker3/4 unreachable — same as 10-10;
spawn retried once after a dockerd-start grace failure, second attempt OK,
TTL 90m, destroyed after evidence collection). Ubuntu 24.04, clean $HOME,
documented quickstart verbatim: anonymous https clone 2s → go1.26.9
toolchain (agent image carried go; downloaded 1.26.9 per go.mod) → `make bin`
64s, stamped 6bf0ade → config edit per README → boot 2s, health OK → first
event 401 (TRBL-109) → rescue patch → 200 → ledger contains the event
(seq 2215) → bad-key 401 → clean SIGINT stop → post-stop `ledger verify` on
the virgin ledger reproduced the seq-2 config hash mismatch. INSTALL_TOTAL_
SECONDS=81. jq missing on the agent image (noted, non-blocking; proof step
ran via python). Fresh-install verdict: the product installs and boots green;
the docs' key placeholder is the one crack (TRBL-109).

## Performance (Step 2b)

hyperfine n=20 warm on the 752KB scratch day-file: `ledger verify --json`
289.6ms ± 8.2ms (user 48ms + sys 12.9ms ≈ 61ms CPU; the ~200ms wall-CPU gap is
the verb's own fsync-window group-commit wait — corroborating TRBL-104's
write-on-read); `ledger status --json` 278.8ms ± 18.5ms; `ledger compact
--dry-run` 307.2ms ± 25.3ms; single-shot verify after page-cache drop 0.29s.
Nothing a user would feel beyond the write-on-read overhead itself; **no PERF
row** (perf law: an unfelt win is not a finding). The 4.4s outlier run 1 saw
did not reproduce here.

## What a fresh user meets, end to end (the honest ladder)

install docs 90% faithful (key placeholder crack) → build+boot green in <90s →
first event 401 on the doc path (TRBL-109) → 200 once the real key is used →
ledger proof works → CLI ops work but write (TRBL-104/111) → verify flags the
product's own boot snapshots (TRBL-106) → a stale HEAD poisons the seq chain
silently (TRBL-110). The core engine (ingest, auth fail-closed, scrub, torn
heal, token quarantine) behaved correctly on every leg; the audit-spine tooling
is where the trust leaks are.

## Cleanup

Scratch config/state (`~/.config/trouble-df1016`, `~/.local/state/trouble-
df1016`) left in place pending the commit of this report (removed after push —
see cleanup note in the tick report); /tmp probe scripts are throwaway. Bunker
agent destroyed (archived per daemon policy). No repo visibility, permission,
or scheduler state touched. No credentials committed: the scratch project key
is a throwaway random hex, never printed to repo files.
