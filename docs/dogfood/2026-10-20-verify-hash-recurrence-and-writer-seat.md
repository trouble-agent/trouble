# trouble dogfood — 2026-10-20: the verify fix that never landed, and the writer seat a refusal leaves behind

**Tick:** `trouble-dogfood-2026-10-20-00-18-40` · **Target:** `~/trouble` · **HEAD at run start:** `d081f79`
(`board: close DOC-9 DOC-10 DOC-15 wave`) · **Binaries:** freshly built and stamped from that HEAD
(`make bin` → `d081f79 d081f79 2026-10-20T00:21:59.000Z [stamped]`) · **Lane contract:** read-only —
no code changed, no row closed, no visibility/permission touched; findings filed as rows.

## Promise (null hypothesis)

> A fresh user can follow `deploy/README.md`'s canonical quickstart on a bare host, build the stamped
> binaries, boot `troubled`, receive a first event through the documented loopback on-ramp, and then
> maintain the ledger with the documented daily checks (`trouble ledger status` / `verify`) and the
> recovery/compaction runbook in `docs/operations.md` §§1–6 — and those checks tell the truth about a
> healthy ledger.

**Verdict: 🟡 PROMISING-BUT-ROUGH.** Every leg of the promise *worked* (build, boot, ingest, auth
ladder, hole detection, hole trail, config refusal, read-only status/verify, read-only compaction
refusal) — and then one of the documented daily checks told a lie on a brand-new ledger, and the
refusal path of another verb left durable state behind. The engine is sound; the audit-spine tooling
and its closure trail are where the trust leaks are.

## What was driven (fresh state root, two machines)

| leg | command | result |
|---|---|---|
| build | `make bin` (warm cache) | 1.4 s → stamped `d081f79` |
| boot | `troubled --config <cfg>` | boot→first `health.json` 1.59/1.83/1.60 s (3 runs) |
| first event | `POST /api/1/event/?sentry_key=<from the config>` | `{"id":"…"}` · 0.34 s cold, 0.41–0.47 s warm |
| dashboard face | `GET /health.json` | 7.8 ms ± 0.6 (n=10) |
| daily check 1 | `trouble ledger status --json` | 20 ms class; `writer_pid` = the real LOCK holder |
| daily check 2 | `trouble ledger verify` | **rc=1, `seq 2: payload hash mismatch`** on a VIRGIN ledger → TRBL-106-R2 |
| retention | `trouble ledger compact --day <today> --dry-run` | refuses (TROUBLE-LEDGER-007) **and leaves a LOCK behind** → TRBL-112 |
| retention | `trouble ledger compact --day <older-day> --dry-run` | works: 437→437 lines, 428 kept + 9 aggregates, 12 ms |
| recovery | delete one mid-file record, restart | `seq_holes=[{from:20,to:20}]`; trail gets `lifecycle{op:hole,from:20,to:20}` ✅ |
| config refusal | `trouble ledger status --config <missing>` | rc=13 `config not found`; default root untouched (22 files before/after) ✅ (TRBL-111 re-proven) |
| read-only | `status`/`verify` with the daemon live | no `trouble-ledger-cli` records written, no `.idx`/`HEAD` rewrite, `compact` refused `writer-live` ✅ (TRBL-104/105 re-proven) |
| hub verbs | `trouble hub status` | mounted and honest: `profile: standalone (enabled=false) … state=cold` ✅ (TRBL-065 re-proven) |

## Finding 1 — TRBL-106-R2 (P1): `verify` reports a deterministic false positive at HEAD

`trouble ledger verify` on a **virgin** state root whose only writer is the shipped daemon returns
`rc=1` with `payload_hash_mismatches=1` — `seq 2: payload hash mismatch` — and the count grows by one
per daemon boot (after five boots: seq 2, 10004, 10067, 10086, 10107). Reproduced on the control host
**and** on a fresh ephemeral bunker agent that had never seen the repo.

The record is honest; the verifier is not. An independent probe (`hashprobe.py`, no product code)
splices the reserved `payload_sha256` member out of the *raw stored payload bytes* and hashes what
remains:

```
seq 2 stored payload_sha256 = 318d8e8e57a48537
raw-stored-bytes-minus-sha  = 318d8e8e57a48537   <-- exact match
recursive sorted-key re-marshal (what verify does) = 571d2947a9469af3
```

That is the TRBL-106 mechanism byte-for-byte (struct field order vs Go's map key sort). Why a closed
row is live: the fix commit `98ba880` (which adds `verifyPayloadHashLine`/`payloadBytesWithoutHash`
plus a 487-line regression test) is **not an ancestor of main** — `git merge-base --is-ancestor
98ba880 HEAD` → rc=1, `git branch -r --contains 98ba880` → `origin/wt/TRBL-106` only, and at HEAD the
symbols exist nowhere (`grep -rn verifyPayloadHashLine` → 0 hits) while `internal/ledger/recover.go`
still runs the map re-hash block.

User impact: `docs/operations.md` §2 is the daily-check runbook, and its "no holes or corruption"
row reads "non-zero exit; the report names the files". A healthy deployment reports corruption on
every scheduled check, forever, indistinguishably from a real one.

## Finding 2 — TRBL-112 (P2): a refused `compact` leaves the writer seat behind

With **no daemon running**: `ledger status` → `writer_pid=-1` (honest). Then a refusing, `--dry-run`
`compact` (`rc=1`, TROUBLE-LEDGER-007) **creates** `<state_root>/ledger/LOCK` naming its own pid and
leaves it on disk. `ledger status` then reports that pid as the writer, and `kill -0` shows the pid is
dead. Mechanically: `cmd/trouble/ledger.go:301` opens the ledger (writer seat) before the
refuse/dry-run decision, and `internal/ledger/readonly.go:176` sets `st.WriterPID = readLockPID(root)`
with no liveness check — while the same file's `CheckWriterHeld` (lines 44–62) applies exactly that
kill-0 check. Same path, doc/contract side: `docs/operations.md` §4 never says the *active* day cannot
be compacted, and the refusal reuses TROUBLE-LEDGER-007 whose documented meaning (SPEC-TYPES) is
"compaction (generation rewrite) failed" (`docs/operations.md` §4 names 009 for that case).

## Finding 3 — TRBL-113 (P2): the closure that graded a proxy

TRBL-106 reads `complete`, closed with "verified fix on wt/TRBL-106 (98ba880)" — verification on the
worktree branch, merge-back never done. Census at HEAD: two unmerged remote fix branches
(`wt/TRBL-106`, `wt/TRBL-109`); TRBL-109's premise *is* fixed (main carries an equivalent docs fix,
`d64b496`, and this run's verbatim quickstart proved the fixed path), so the rule must grade the fix's
content, not the branch tip.

## Finding 4 — TRBL-114 (P2): no `docs/qa/use-cases.md` catalog

The dogfood/QA procedure starts from a catalog of documented user goals with spec anchors and stable
`UC-`/`REG-` ids. It does not exist: `docs/qa/` holds one file, and no `UC-`/`REG-` id appears anywhere
in the repo — so coverage is re-derived from prose every tick and no defect has a durable regression
case to re-run.

## Re-proofs that passed (delta `6bf0ade..d081f79`)

* **TRBL-104/105 (read-only ledger verbs)** — while the writer was live: zero `trouble-ledger-cli`
  records, no sidecar/HEAD rewrite, `writer_pid` = the LOCK holder, `compact` refused `writer-live`
  (rc=1), `status`/`verify` exit contract per `--help`.
* **TRBL-107 (hole trail)** — deleting one mid-file record: `verify` reports `seq_holes=[{from:20,to:20}]`
  and the next batch writes `lifecycle{op:hole,from:20,to:20}`; after a HEAD-hint-inflated restart the
  trail records `hole{from:302,to:9999}` + `seq_recovered`, so the created gap is honest.
* **TRBL-111 (config refusal)** — rc=13 `config not found`; the default state root held 22 files
  before and after (no more write-on-read into a foreign ledger).
* **TRBL-110 (HEAD hint)** — resolved as *not* a defect: SPEC-01 §3.4 rule 7 itself defines
  `seq_alloc = max(HEAD.last_seq, highest seq in the highest-generation file) + 1` (and
  `docs/operations.md` §3 repeats it), so the observed `10000` after a `9999` hint is spec-conformant;
  what the 2026-10-16 finding actually caught — the created gap riding silently — is now recorded in
  the trail. Filed as a resolved false alarm here rather than re-opened.

## Step 2b — what it costs (no perf row filed)

`verify` 28.2 ms ± 4.4 (n=10, warm, user 23.8 ms — compute-bound, no waits); `status` ~20 ms;
`compact --dry-run` on a 437-line day 12 ms; `POST /event/` 0.34 s cold / 0.41–0.47 s warm
(matches the documented ~0.65 s single-event figure and its group-commit explanation in README);
`/health.json` 7.8 ms; boot→health 1.6–1.8 s; SIGTERM drain 0.11–1.15 s. Nothing here is slow enough
for a user to notice — **no PERF row**, said plainly. (Load context: 16 vCPU, loadavg 15.6 at
measurement time, so these are pessimistic numbers.)

## Install leg (fresh machine, verbatim quickstart)

`bunker-mvp` (bunker3/2/4 unreachable — `ssh` timeouts, same as the previous ticks), agent
`df-trouble-1020`, spawned as ephemeral, destroyed after collection. The repo has no remote by design,
so the checkout was staged as a tar over ssh (`.git` included, which is what makes the build stamped):

* toolchain on the bare agent: `git 2.43.0`, `make 4.3`, `curl 8.5.0`, `cc 13.3.0`, **`go 1.22.2`**,
  **no `jq`** — the quickstart's prerequisites are `curl` + `jq`, so the documented ledger check has
  no interpreter on a bare Debian agent (the check still works via `grep`; non-blocking, recorded).
* build at `d081f79` → stamped binaries; boot 2 s; first event **200** with the *shipped* placeholder
  key read back out of the config, 0.352 s (`TRBL-109`'s docs fix holds on a clean box).
* the fresh box reproduced **`seq 2: payload hash mismatch`** on a virgin ledger (independent
  corroboration of TRBL-106-R2) and the HEAD-hint allocation walk (2840…2842 then 10000…10002 with
  `hole{2843,9999}` recorded).
* **the harness leg could not run**: `~/.hermes/scripts/bunker-qa.sh` aborted at
  `line 1061: UI_PORTS: unbound variable`, shipped a 0-byte battery, and correctly refused to report
  `LAUNCHED`; its agent was destroyed. The install proof above was therefore hand-walked per the
  skill's ephemeral procedure — the fresh-machine install premise IS proven (build+boot+first event on
  a box that had never seen the repo), but the CI/chaos cells of the QA battery were not run this tick,
  and that generator break is a fleet-harness defect, not this project's.

## Left behind

* board rows: `TRBL-106-R2` (P1, recurrence of the complete TRBL-106), `TRBL-112`, `TRBL-113`,
  `TRBL-114` (P2) — filed through `boardctl create`, ids from `board_next_id.py`, duplicate-censused,
  `boardctl validate` OK.
* this report, the diagnostics section and the usage-skill pitfalls; the run entry in
  `.coding-hermes/dogfood-log.md`.
* evidence scripts (scratch, outside the repo): `/home/kara/df1020/{setup,first-event,readonly-proof,probe2,perf,compactprobe,lockprobe,pidprobe}.sh`,
  `hashprobe.py`, `snap/verify-*.json`, `snap/perf.txt`.

## What to tell the maintainer first, with one hour

Land `98ba880` on main and prove it the way this run did: build HEAD stamped, boot a virgin state
root once, `trouble ledger verify` → rc 0. Then make the close-out step assert
`git merge-base --is-ancestor <fix-sha> origin/main` — the same hour that fixes the tooling prevents
the next silent regression of exactly this shape.
