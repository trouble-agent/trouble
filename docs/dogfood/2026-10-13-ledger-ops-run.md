# Dogfood run 2026-10-13 (second sibling) — the ledger operations surface

Target: `trouble` at HEAD `42e1a51` (stamped build `42e1a51 2026-10-13T10:12:32 [stamped]`,
`bin/` rebuilt this run — verdicts are HEAD-attributed). This run took the surface no
prior dogfood entry touched: the ledger operations path an on-call operator actually
lives in — `docs/operations.md` §2-4 (daily checks, recovery, compaction), the
second-writer refusal, torn-line recovery, dedup fold, and the hub archive verb.

Sibling context: a same-tick sibling run (538f484, `2026-10-13-skills-loop-run.md`)
drove the SPEC-11 skill loop and the fresh-machine install leg on bunker-mvp. That leg
is NOT duplicated here; this run's local scratch instance re-proves the loopback
quickstart path only as scaffolding for the ledger-ops drive.

## What was exercised (real use, not tests)

Scratch instance: config `~/.config/trouble-df1013/config.toml`, state root
`~/.local/state/trouble-df1013`, per the canonical quickstart in `deploy/README.md`
(copied `examples/config.toml` + `trouble.env`, 0600/0700 modes, real project key,
loopback binds 7643/7644).

1. Boot + health + first event + ledger proof: `POST /api/1/event/?sentry_key=` →
   200 `{"id":...}`, event + group records land in `2026-10-13.jsonl`. Health
   `ledger_last_seq` advanced with the file throughout (1215 at final stop).
2. Dedup fold: 4 identical events → 1 group aggregate with
   `counters.suppressed=3`, one incident for the sig. Correct.
3. Second-writer refusal: started a second `troubled` against the live state root →
   boot refused exit 13, `TROUBLE-LEDGER-005 (write)` naming the LOCK's pid/version/
   git_sha/boot_ts. Exactly per SPEC-01 §3.4. Verified three separate times.
4. Crash + torn-tail recovery: SIGKILL'd the daemon, appended a torn fragment
   (`{"seq":99999,"kind":"torn","broken` — no newline) to the live day file,
   restarted. The boot healed the fragment (file re-terminated), seq allocator
   continued at 75 — the fake seq 99999 was NOT adopted (the heal happens before
   allocation; HEAD-as-hint worked). Restart persistence: token minted after a
   later restart validated against the dashboard (200 with Bearer).
5. Restart ladder: 4 boots total recorded as `lifecycle{op:boot}` notes
   (seq 1, 78, 985, +final), each carrying the recovered `last_seq`. Clean SIGINT
   drain closes listeners and exits; ports free.
6. Operator CLI verbs against the live daemon: `config explain` (7.1 ms),
   `topology --json`, `check-stall` (exit 0, class= seq_age=0s breaches=0),
   `hub status --json` (standalone profile, honest `enabled:false`),
   `hub dedup --key` (correctly refuses on the standalone profile: redis not
   configured, exit 2), `hub archive --dry-run` (correctly refuses 011
   `not_closed` on a live file with no sidecar), `sensors probe --json`,
   `dashboard token create/list` (mint printed once, store mode 0600, anchored to
   the state root per TRBL-085 — `config explain` shows the derived path).
7. Perf (Step 2b): ingest 243 ms ±33 ms warm (hyperfine, 10 runs), dashboard
   health sub-5 ms, CLI verbs 6-7 ms. Nothing user-noticeable → NO perf row.

## Findings (read-only lane — NOT filed; for the foreman)

### DF-LEDGER-1 (P1) — `trouble ledger status/verify/compact` do not exist; SPEC-01 §2.4/§4.5 and docs/operations.md teach verbs that cannot run

SPEC-01 §2.4 tables the three read-only ledger CLI seams with an exit-code
contract (status 0/1, verify 0/1/2, compact 0/1). §4.5 promises
`trouble ledger status --json` prints LedgerStatus with §3.8 trigger values.
`docs/operations.md` §2 builds the DAILY CHECKS table on them (`trouble ledger
status --json`, `trouble ledger verify`), §4 teaches `trouble ledger compact
--day D [--dry-run]`.

Reality at HEAD 42e1a51: `cmd/trouble/main.go` switch has no `ledger` case —
`trouble ledger status --json` exits 2 "unknown command". The machinery all
exists and is well-tested as a library (`Ledger.Status()`, `Ledger.Verify()` /
`VerifyReport`, `Compact`/`CompactDryRun`, `types.LedgerStatus`,
`internal/ledger/tier.go`'s trigger printer) but has ZERO non-test callers:
`grep -rn '\.Verify(' cmd/ internal/app/` is empty (only registry/skills/testkit
use their own Verify). An operator following docs/operations.md §2 on day one
cannot run a single listed daily check. Fix direction: wire the three verbs in
cmd/trouble over the existing package API (the exit-code contract is already
specified), or rewrite operations.md §2/§4 until the seam lands.

### DF-LEDGER-2 (P1) — a healed mid-file torn fragment is unreportable: corrupt but never counted anywhere an operator can see

Driven live: a torn fragment healed by boot N leaves an unparseable line MID-file
(line 74 of the day file in this run). Source-level, `walkLines`
(internal/ledger/recover.go:82-88) only marks the FINAL line as tail, and
`scanFile` (internal/ledger/index.go:609-613) maps a non-tail unparsable line to
`corrupt`, never `torn` — so the heal loop (recover.go:208, gated on
`p.Torn == 0`) can never fire a `recover_torn` boot note for it. Observed:
4 boots over the corrupted file produced zero `recover_torn` records (only
`op:boot` notes), health.json carries no torn/corrupt fields, and
`types.IndexStats.TornLines/CorruptLines` (spec01.go:48-49) has no consumer
outside `internal/ledger` — the dashboard never renders it. The corrupt line
persists at line 74 to this run's end; `Ledger.Verify()` WOULD count it
(`CorruptLines++`, `rep.OK=false`) but DF-LEDGER-1 means no operator can invoke
it. Fix direction: count mid-file unparsable lines into IndexStats (the index
already tracks them — scanFile returns `corrupt`) and surface the pair on
health.json next to `ledger_degraded`, or make the heal loop also record a
`lifecycle{op:"corrupt_line"}` note at boot.

### DF-LEDGER-3 (P2) — `hub archive --dry-run` reads the live file's identity from a HEAD field the writer never writes

`cmd/trouble/hub.go:268` derives `LiveFile` from `HEAD`'s `file` field
(`liveFileFromHead`). The writer's HEAD record (internal/ledger/writer.go:32,
`headFile`) has NO `file` field — it writes day/gen/part — so
`liveFileFromHead` always returns `""` on a HEAD the writer produced, and
`hub archive` refuses 011 `not_closed` on every real state root until `--force`.
The refusal is honest (better a wrong refusal than guessing the live file), but
the verb can never succeed non-forced against data written by the daemon's own
HEAD format. Fix direction: read day/gen/part from HEAD and re-derive the
partName, or have `writeHead` include the file name.

### Honest-failure legs that PASSED (no row needed)

- Unknown `--config` key refused by name (LIFECYCLE-001) — matches docs/cmd.md.
- `check-stall` exit contract held (0 healthy).
- `hub dedup` refusal on standalone: message names the missing key
  (`server.redis.url is not configured (profile "standalone")`), exit 2 — good
  failure UX.
- Token mint → restart → authenticated /incidents 200; store 0600; plaintext
  shown once; list shows hash only.

## Perf summary (no PERF rows filed)

| op | warm | notes |
|---|---|---|
| ingest POST (group commit) | 243 ms ±33 ms (n=10) | matches prior runs' 0.17-0.4 s |
| `trouble config explain` | 7.1 ms ±0.7 ms | |
| `trouble hub status --json` | 6.5 ms ±0.2 ms | |
| dashboard health | <5 ms | prior-run consistent |

Nothing slow enough that a user would notice.

## Verdict

🟡 PROMISING-BUT-ROUGH for the ledger-ops surface specifically: the core
(engine: append, dedup, recovery, lock, rotation, SIGINT drain) is solid,
honest, and fast — every failure leg failed loudly and correctly. But the
operator-facing seam the specs and docs PROMISE for exactly this surface is
absent (DF-LEDGER-1), and the one recovery event this run injected is invisible
to every shipped surface (DF-LEDGER-2). The library is shippable; the on-call
story is not yet.
