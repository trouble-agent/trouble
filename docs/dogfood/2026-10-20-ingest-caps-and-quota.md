# trouble dogfood 2026-10-20 — run 3: the ingest caps, the gzip guard and the quota loss policy

**Tick:** `trouble-dogfood-2026-10-20-00-18-40` (third angle of the same tick) · **Build:** `a4362ff`
(stamped from HEAD, merged fix included) · **Scratch:** `~/.cache/df1020r3` / `df1020r3b`, ports
7649/7650 and 7651/7652, `quota_epm` lowered to 5 / 3 to reach the quota window with a handful of
requests · **Lane:** read-only.

**Why this surface:** run 1 and run 2 swept the ledger verbs and the install path; the dogfood log has
**zero** prior coverage of `gzip`, `quota`, oversize bodies or `Retry-After`. This is the one documented
behavior a real operator meets on a bad day (a chatty app, a gzip bomb, an SDK retry storm), and every
claim in it is machine-checkable against the repo's own code table.

## Verified clean (the oracle is the repo's own §-tables and its `X-Sentry-Error` contract)

| probe (documented shape) | expected | observed |
|---|---|---|
| `POST /api/1/event/?sentry_key=…` baseline | 200 | 200 `{"id":…}` |
| body not JSON | 400 + 019 + `causes` | 400, `X-Sentry-Error: TROUBLE-SENTINEL-019`, `["generic_json"]` |
| `message` not a string | 400 + causes | 400 + `["message_not_string","message_or_exception_required","message_shape_expected"]` |
| gzip 409 KB compressed (>200 KB cap) | 413 + 002 | 413, `X-Sentry-Error: 002`, cause `too_large` |
| gzip bomb 300 KB→~300 B (~1000:1) | 413 + 003, cause `compression_ratio` | exactly that |
| gzip, decompressed 1,048,577 | 413 + 003, cause `decompressed_cap` | exactly that |
| `Content-Encoding: br` | 415 + 022, cause `unsupported_encoding` | exactly that |
| `quota_epm=3`, requests 1-3 then 4-5 | 200, then 95% proactive 200+header, then 429 + `Retry-After` + 010 | 200, 200+`X-Sentry-Rate-Limits: 58:error:project:quota_epm:`, then 429 + `Retry-After: 58` + `X-Sentry-Error: TROUBLE-SENTINEL-010` |
| ledger for every refused item | `disposition=dropped_quota`, `error_code=TROUBLE-SENTINEL-014` | 3/3 and 6/6 records, plus `disposition=admitted` for the landed ones |

Two of my initial readings were **false alarms**, killed by reading the code before filing (recorded in
`diagnostics.md` item 6 so no later run re-chases them): the proactive header looked "one event early"
until the baseline request was counted against the window (the rule is `used*100 >= quota*95`,
`internal/sentinel/quota.go:19`/`:108`); and `/health.json` returning null quota counters is not a defect
— those counters live on the dashboard's project-runtime view (§9.2), a different surface, which my
scratch instance refuses without a token (QA-TROUBLE-19, known).

## The one gap — TRBL-115 (P3)

The byte-exact decompressed-cap boundary the docs teach is only half-reachable through the documented
curl on-ramp:

```
gzip body, decompressed == 1,048,576   ->  400  X-Sentry-Error: TROUBLE-SENTINEL-019  ["message_too_long"]
gzip body, decompressed == 1,048,577   ->  413  X-Sentry-Error: TROUBLE-SENTINEL-003  ["decompressed_cap"]
```

`docs/operations.md` §"Envelope size refusals" and `docs/sentinel-compat.md` §3 promise "a body of
exactly 1048576 decompressed bytes is accepted (200)". On the generic-JSON route the dialect's own field
cap refuses first (`genericMaxMessage = 16 * 1024`, `internal/sentinel/genericjson.go:15`, cause at
`:45`), so the accept side of the boundary has no documented payload that reaches it. The transport's
refusal side is correct and byte-exact.

**Not exercised, stated plainly:** the envelope route's accept side (a legal at-cap envelope needs
several ≤256KB items summing to exactly 1,048,576 decompressed). That is the closing criterion in the
row; nothing here claims the transport accept side is broken.

## What this run did not touch

The collectors (`go-panic` / `py-traceback` / `node-reject` over journald and file tails), the canary
ladder, the in-flight concurrency cap (64) and the per-IP budget (600/min) — all documented, none driven
this tick. They remain the next unexercised slice of the same chapter.
