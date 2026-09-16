# sentinel compatibility matrix

The ingestion surface of `internal/sentinel` (SPEC-04), what it accepts, and
every place where it deliberately differs from upstream Sentry or from the SPEC-04
text. Everything here is derived from the code's own tables: `TestCompatMatrix`
(`compat_test.go`) renders the tables below from the same values the server uses
and fails if this document drifts from them.

## 1. Routes (listener `sentinel.bind`, default `127.0.0.1:7643`)

| Method | Path | Purpose | Accepted auth | Success |
|---|---|---|---|---|
| POST | `/api/{project_id}/envelope/` | primary: newline-framed Sentry envelope | `X-Sentry-Auth`, envelope `dsn` header, `?sentry_key=` | `200 {"id":"<event_id>"}` |
| POST | `/api/{project_id}/store/` | legacy compat (deprecated upstream): accepted, counted, `X-Sentry-Deprecated: store` | same three forms | `200 {"id":…}` |
| POST | `/api/{project_id}/event/` | generic JSON (trouble's own dialect, never a Sentry route) | `?sentry_key=`, `X-Sentry-Auth` | `200 {"id":…}` |
| GET | `/api/{project_id}/` | DSN probe (reachability + project state) | optional | `200 {"id","slug","enabled","status","canary_last_ts"}` |

Any other path → `404` with an **empty** body and the `ingest_404_total` counter:
probe traffic must never be able to write a ledger record. Any other method on
those four paths → `405` + `TROUBLE-SENTINEL-021`.

## 2. Item types (`type` in an item header)

| Item type | Policy | Ledger effect | Counter |
|---|---|---|---|
| `event` | parse → scrub → sig → group | `event` record + `group` record | `events_total` |
| `client_report` | parsed, never dropped | `event` record with `payload.item_type="client_report"` and `payload.client_report`; **no group, no quota** | `client_reports_total` |
| `session`, `transaction`, `profile`, `replay` | accepted and dropped with a counter | none (boot 200) | `items_dropped_total{type}` |
| `attachment`, `minidump` | accepted and dropped with a counter; the body is consumed, never buffered past `max_item_bytes` | none (200) | `items_dropped_total{type}` |
| `check_in`, anything else | 200-and-drop | none | `unknown_items_total` |

An unsupported item type **never** fails the envelope. `SentryEvent.ItemTypes`
records every type seen, including dropped ones.

The `client_report` timestamp is accepted in both forms the SDK contract allows
— an ISO DateTime string (`"2026-09-16T09:15:00Z"`) or a UNIX timestamp in
seconds as a JSON number, fractional included (`1642153010.09`, the form
sentry-javascript sends). A missing, `null` or unrecognized value (a boolean, an
object, `1e9`, a number outside `int64`) leaves the report on the server clock;
the item is still parsed and never dropped, because §3.2's point is that
SDK-side attrition is visible data rather than silence (§1.3).
`TestClientReportTimestampForms` drives every one of those shapes through the
HTTP path.

Binary item types (`attachment`, `minidump`, `profile`, `replay`) require an
explicit `length`; without it the envelope is `400` + `TROUBLE-SENTINEL-001`
(cause `length_required`).

## 3. Encodings and sizes

| Content-Encoding | Accepted | Notes |
|---|---|---|
| `identity`, absent | yes | |
| `gzip`, `x-gzip` | yes | the SDK default |
| `deflate`, `br`, `zstd`, anything else | no | `415` + `TROUBLE-SENTINEL-022` (cause `unsupported_encoding`) |

| Limit | Default | Failure |
|---|---|---|
| compressed envelope | 200 KB | `413` + `TROUBLE-SENTINEL-002` |
| decompressed envelope | 1 MB — refused at exactly the cap; `cap + 64KB` is the decompressor's memory bound, not an accepted payload | `413` + `TROUBLE-SENTINEL-003` (cause `decompressed_cap`) |
| compression ratio | > 100:1 once output passes 100 KB | `413` + `TROUBLE-SENTINEL-003` (cause `compression_ratio`); a payload that breaches both bounds reports whichever one the stream crosses first |
| single item | 256 KB | `413` + `TROUBLE-SENTINEL-002` (cause `item_too_large`) |
| header / item-header line | 8 KB | `400` + `TROUBLE-SENTINEL-001` |
| body line (log lines) | 64 KB | truncated, `truncated_total` |
| in-flight requests | 64 | queued ≤1s, then `429` + `1:error:global:overloaded:` |
| per IP | 600/min, burst 60 | `429` + `1:error:ip:overloaded:` |

The decompressed cap is enforced **at the cap**: a body of exactly `1048576`
decompressed bytes is accepted (200) and `1048577` is refused, on `identity` and
`gzip` alike — the 200 KB compressed cap is checked first and only masks this
boundary for a payload that does not actually compress. `cap + 64KB` is the
limited reader's *memory* bound — the headroom that makes "over the cap"
detectable without buffering the rest of the stream (§6.1) — so the window
between the cap and cap + 64KB is a refusal, never a payload.
`TestDecompressedCapBoundary` asserts the boundary against this table's number.

## 4. SDK / client compatibility

| Client | Status | Notes |
|---|---|---|
| sentry-go (envelope) | supported | fixture `testdata/envelopes/sentry-go-event.envelope` |
| sentry-python 2.x (envelope with `event` + `client_report` + `session`) | supported | `client_report` parsed; `session` dropped |
| @sentry/node 8.x (envelope) | supported | |
| pre-2020 SDKs (`application/x-www-form-urlencoded` with `sentry_data`) | supported on `/store/` | legacy, counted |
| `curl` / any runtime (generic JSON) | supported on `/event/` | documented form: `?sentry_key=<pubkey>` |
| SDK sessions, transactions, attachments, minidumps, replays, profiles | accepted and dropped | deferred planes; the type is still recorded |
| 16-hex DSN secrets (older Sentry) | **refused** | `TROUBLE-SENTINEL-006`, cause `secret_length`: only 32-hex secrets are accepted |
| `X-Sentry-Auth` scheme token other than `Sentry` | refused | `TROUBLE-SENTINEL-006` |
| query-string key from a non-loopback peer | refused unless `allow_query_key_non_loopback = true` | `TROUBLE-SENTINEL-006`, cause `query_key_remote` |

Auth-form precedence: `x_sentry_auth` > `envelope_dsn` > query. Two forms that
resolve to different projects are `TROUBLE-SENTINEL-006` (cause
`conflicting_auth_forms`), never a silent pick.

## 5. Known divergences from the SPEC-04 text

Each of these is a place where the shipped code cannot follow the spec literally,
with the reason. Every one of them is asserted by a test.

1. **`scrubber` interface shape.** SPEC-04 §2.2 sketches
   `Scrub(b []byte, targets []string) (types.ScrubResult, error)`. SPEC-02 ships
   `ScrubBytes(ctx, target, projectID, b)`, which is the real call site (it needs
   the project id for the shield and per-target budgets). Sentinel consumes
   `ScrubBytes` directly; `var _ scrubber = (*scrub.Engine)(nil)` in `server.go`
   proves the seam compiles.
2. **`ledgerSink` interface shape.** The spec sketches
   `Append(types.Record) error; LastSeq() uint64` and names `*ledger.Writer`,
   which does not exist. The shipped ledger surface is
   `Append(ctx, types.RecordDraft) (types.Record, error)` plus
   `Status().LastSeq`. Sentinel consumes that plus an *optional*
   `ScanFrom(seq, yield)` reader used for the boot rebuild; `LedgerSink` (in
   `server.go`) is the exported adapter SPEC-12 mounts.
3. **Four `Config` fields are additions** (marked `[SPEC-12-resolved]`):
   `SpoolDir`, `HostID`, `Actor`, `Zone`. The spec's `Config` has no state root,
   host or actor because SPEC-12 owns them, but sentinel cannot write a record or
   place the spool without them.
4. **Masking rule 13 (`:PORT`) is applied before rule 11 (`\b\d{3,}\b`).** In the
   table's literal order rule 11 shadows rule 13 for every bare port (a 3+ digit
   run at a word boundary), which contradicts §3.3's own note that "ports are
   masked after `file:LINE`". The relative order of every other rule is untouched.
5. **An extra go-panic continuation pattern.** Go prints argument-less frames as
   a bare `pkg.Func()` line, which none of the five pinned continuation patterns
   match; under the pinned END rule the event would close at the first frame and
   lose the stack. The added pattern matches only a whitespace-free, dotted,
   parenthesised symbol line.
6. **py-traceback's exception line reads a wider shape.** The pinned START
   pattern lists the stdlib class suffixes (`Error`, `Exception`, `Warning`,
   `Exit`, `Fault`), but a traceback's last line is its exception line whatever
   the class is called (`app.queue.PoolExhausted: …`). The wide form is accepted
   only for a qualified or suffix-matching class, so an ordinary `INFO: ready`
   line still ends the event.
7. **A blank line inside an open event is skipped, not an END.** A Go dump puts a
   blank line between the panic message and the `goroutine 1 [running]:` block,
   and §3.5's END rule explicitly excludes the goroutine header — which only makes
   sense if the blank line does not close the event. Blank lines count as activity
   for the silence timeout.
8. **`item_overrun_total` follows §6.4 over §3.1.** §3.1 says a
   length-prefixed body must be followed by exactly one LF (else `400`); §6.4 says
   an item `length` shorter than its JSON body is data, not a rejection. For an
   *understated* length the bytes up to the next LF are discarded, the item
   parses and `item_overrun_total` increments; a body *shorter* than its declared
   length is still `TROUBLE-SENTINEL-001`.
9. **`X-Sentry-Error` on a quota drop is `TROUBLE-SENTINEL-010`, while the event
   record's `payload.error_code` is `TROUBLE-SENTINEL-014`.** §3.9's header table
   pins 010 for "project quota, drop-with-counter" (011 for the disk budget) and
   §5 pins 014 for the loss policy itself; both are emitted, each in its place.
10. **Canary injection targets `sentinel.bind`.** SPEC-12 owns the listener, so
    `InjectCanary` posts to `loopbackAddr(cfg.Bind)` — the loopback form of the
    bind address — and the injection travels the real HTTP path.
11. **`ReleaseDiff` derives its own `from`.** §3.4's `fixed_in` needs a `from`
    release; the §2.2 signature (`ReleaseCoverage(projectID, release)`) has no
    such parameter, so `ReleaseDiff` takes the newest release observed for the
    project that orders before `to`.
12. **Collector events are attributed to one project** — `canary_project` when
    configured, else the first configured project. Journald and file sources are
    host-level and carry no project id.
13. **The duplicate-event window is bounded.** §6.6 pins a 10-minute window with
    no memory bound; sentinel caps the map at `maxDupEntries` (65,536 ids),
    evicting the oldest, so memory cannot be driven by a hostile client. At a
    sustained rate above ~109 events/s the oldest ids are forgotten before the
    10-minute mark, and an SDK retry of such an id is counted twice (logged by
    `duplicate_events_total`, never silently merged).
14. **`SourceLiveness`, `ProjectRuntime`, `CollectorParser`, `SentryEvent`,
    `ClientReport`, `DiscardCount` and `RateLimitDecision` are declared in
    `internal/types` by this spec** (§3.6/§3.10 own them). `SourceLiveness` is
    declared alongside them because SPEC-04 produces it and SPEC-10 consumes it;
    it must not be redeclared.

## 6. Ledger payload schema

`event` records (`kind="event"`, `sig` = the sentinel sig of the canonical
bytes):

| Key | Meaning |
|---|---|
| `item_type` | `event`, `client_report` |
| `native_id` | the SDK's `event_id` (32 hex) |
| `event` | the scrubbed `types.SentryEvent` (nested object) |
| `digest` | full 32-byte digest hex — the grouping truth |
| `norm_version` | 1 |
| `auth_form` | `x_sentry_auth` / `envelope_dsn` / `query_sentry_key` / `generic_json_query` / `""` (collectors) |
| `source_kind` | `envelope` / `store` / `generic_json` / `collector` / `canary` / `spool_replay` |
| `zone` | `loopback` / `lan` / `public` |
| `disposition` | `admitted` / `sampled` / `dropped_quota` / `spooled` / `dropped_spool_full` |
| `error_code` | present on a loss-policy outcome (`TROUBLE-SENTINEL-014`/`015`) |
| `partial`, `flush_reason` | collector assembly cut short (`timeout`, `superseded`, `end`, `size_cap`, `file_rotated`, `file_truncated`, `file_removed`) |
| `parser`, `collector_source` | collector parser name and `journal:<unit>` / `file:<path>` |
| `client_report` | parsed `ClientReport` (`client_report` items only) |
| `truncated`, `invalid_level`, `fingerprint_fallback`, `clock_skew_s`, `legacy_store`, `item_types` | the §3/§5 signals that are counters on the record rather than separate records |
| `quota_limit` | the project's `quota_epm` at decision time |

`group` records: `op` (`create` / `flush` / `release`), `digest`, `sig`,
`group_id`, `title`, `count`, `counters`, `first_seen_ts`, `last_seen_ts`,
`release_range`, `norm_version`, `events_upper_seq`, `aggregate`,
`sample_rate`, and `regression` (`confirmed` / `unconfirmed`) on a regression
verdict.

`gap` records: `cause`, `sensor="sentinel"`, `scope`, `est_lost`, `from_ts`,
`to_ts`. Causes emitted here: `canary_missing`, `cursor_invalid`, `parser_error`,
`source_error`, `collector_attach_failed`, `spool_drop_oldest`, `scrub_refused`,
`file_rotated`, `file_truncated`, `file_removed`, `ingest_reject_storm`.

`canary` records: `phase` (`injected` / `observed`), `iteration` (injections),
`event_id`, `project`, `sig`.

## 7. Operational reads

* `ProjectRuntime(project)` — quota usage, loss counters, client-report
  discards, observed auth forms, canary state, disk usage vs budget.
* `Sources()` — one `SourceLiveness` per configured source plus `sentinel`
  itself (alive = a canary observation inside `2 × canary_interval`).
* `ReleaseCoverage(project, release)` — `(canaryOK, lastEventTS)`; coverage
  without a canary is UNKNOWN, never "fixed".
* `Groups()` / `GroupBySig(sig)` / `SigOf(event)` — the read views SPEC-05 and
  SPEC-10 use.
