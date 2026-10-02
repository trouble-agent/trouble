# The dashboard HTTP API

The dashboard is one embedded HTTP server in the daemon binary serving
server-rendered pages and htmx fragments from the in-memory index. This page is
the **integrator-facing route and auth reference** for it: every registrable
`(method, path)` pair, its scope, its request shape and its response shape, with
the wrapping rules (URL-token refusal, the deterministic 404, CSRF, load shed)
that run before or around any handler.

It is **derived from the code, not from a spec**: the route list below is the
`routeTable()` slice in `internal/dashboard/routes.go`, in slice order, and the
behaviour statements are the ones the handlers and wrappers in that package
implement. `specs/SPEC-10-dashboard.md` §2.1 is the normative contract and
`docs/operations.md` §12 is the operational prose; neither lists all 20 rows in
one place, which is why this page exists.

## 1. Provenance and how to verify a row

`internal/dashboard/routes.go` is the only source of routes. `routeTable()` is a
20-element slice; `registerRoutes()` is the only registration path, and it
registers every row into a real `http.ServeMux` with its Go (method, path)
pattern, so a duplicate `(method, path)` is a startup panic by construction. The
router wrapper `ServeHTTP` then matches the request against the same table before
anything reaches the mux. Nothing in the package registers a route any other way;
the embedded templates under `templates/` and the assets under `static/` exist
only because a row serves them.

Three ways to check this page against the code, without opening SPEC-10:

```
# 1. the row count: prints 20
grep -A 24 'func (s \*server) routeTable' internal/dashboard/routes.go \
  | grep -cE '^\s+\{"(GET|POST)"'

# 2. the table's own completeness test (counts 20 rows + the 404 rule)
go test -run TestRouteTableCompleteness ./internal/dashboard/

# 3. registration, scopes, CSRF, 404, partial whitelist
go test -run 'TestRoutesRegisterAndAreReachable|TestScopeHierarchy|TestCSRFMatrix|TestNotFoundAndWrongMethod|TestUnknownPartialIs008|TestNoRouteAcceptsURLToken|TestAuthMatrixAbsentAndMalformed|TestHealthLoopbackExemption' ./internal/dashboard/
```

This page is hand-maintained and, unlike `docs/sentinel-compat.md`, **no drift
check test pins it** — see §11.

## 2. The 20 routes

Rows are listed in `routeTable()` order, and the row numbers below are that
slice's 1-based index (`pageIndex` is row 1, `staticAsset` row 19, the
`/partials/{name}` guard row 20). `{id}`, `{asset}` and `{name}` each match
**exactly one** path segment: a deeper or shallower path is not a row at all and
answers the deterministic 404 of §3.2.

| # | Method | Path | Scope | Handler | Success |
|---|---|---|---|---|---|
| 1 | GET | `/` | `read` | `pageIndex` | `200` full page, "Overview" |
| 2 | GET | `/incidents` | `read` | `pageIncidents` | `200` full page |
| 3 | GET | `/incidents/{id}` | `read` | `pageIncident` | `200` full page, the incident story panel |
| 4 | GET | `/groups` | `read` | `pageGroups` | `200` full page |
| 5 | GET | `/groups/{id}` | `read` | `pageGroup` | `200` full page |
| 6 | GET | `/rules` | `read` | `pageRules` | `200` full page |
| 7 | GET | `/breakers` | `read` | `pageBreakers` | `200` full page |
| 8 | GET | `/health.json` | `read` | `healthJSON` | `200` JSON (`HealthResponse`) |
| 9 | POST | `/api/incidents/{id}/ack` | `write` | `actionAck` | `200` refreshed incident-rows fragment |
| 10 | POST | `/api/incidents/{id}/close` | `write` | `actionClose` | `200` refreshed incident-rows fragment |
| 11 | POST | `/api/autonomy` | `autonomy` | `actionAutonomy` | `200` refreshed health-strip fragment |
| 12 | GET | `/partials/health` | `read` | `partialHealth` | `200` health-strip fragment |
| 13 | GET | `/partials/incidents` | `read` | `partialIncidents` | `200` incident-rows fragment (`?since=`) |
| 14 | GET | `/partials/incidents/{id}/timeline` | `read` | `partialTimeline` | `200` timeline fragment (`?since=`) |
| 15 | GET | `/partials/groups` | `read` | `partialGroups` | `200` group-rows fragment (`?since=`) |
| 16 | GET | `/partials/rules` | `read` | `partialRules` | `200` rule-rows fragment |
| 17 | GET | `/partials/breakers` | `read` | `partialBreakers` | `200` breaker-rows fragment |
| 18 | GET | `/partials/budget` | `read` | `partialBudget` | `200` watermark panel fragment |
| 19 | GET | `/static/{asset}` | `read` | `staticAsset` | `200` bytes + `ETag`, or `304` |
| 20 | GET | `/partials/{name}` | `read` | `partialUnknown` | `404` + `TROUBLE-DASHBOARD-008` (the whitelist guard) |

In groups: 7 pages (1–7), `/health.json` (8), 3 write actions (9–11), the 7
polling partials (12–18), the static assets (19) and the unknown-partial guard
(20). **Row 20 is not a surface** — it is the refusal that makes an unlisted
`/partials/<one-segment>` name a `404` instead of a client-supplied template
name, and it is registered so the path prefix is owned explicitly rather than
falling through to the router's 404.

Two wrappers are applied per row at registration time:

* **Scope** — every row is wrapped in the auth middleware (§4), including the
  `read` rows. The single exception is the runtime `/health.json` loopback
  exemption (§4.3).
* **Load shed** — every row whose path starts with `/partials/` (rows 12–18 and
  20) is additionally wrapped in the memory-pressure shed (§7.4). Pages,
  `/health.json` and the three POSTs keep serving while polls are shed.

## 3. What happens before any handler

### 3.1 A token in a URL is refused first (`400` + `TROUBLE-DASHBOARD-005`)

Every request — before authentication, before row matching — is inspected for
credentials carried in the URL:

* **query parameter names**, case-insensitive: `token`, `access_token`, `auth`,
  `apikey`, `api_key`, `key`, `trouble_token`, `tdt`;
* **any query parameter value** matching the token grammar
  `^tdt_[A-Za-z0-9_-]{43}$`;
* **any path segment** matching the same grammar.

A match answers `400` + `TROUBLE-DASHBOARD-005` with detail
`url_token_param_name` / `url_token_param_value` / `url_token_path_segment`,
even when the request would otherwise authenticate. The offending value is never
logged (parameter name and length only) and never echoed. A `tdt_…` value in a
URL cannot be used as an auth carrier: only the `Authorization` header and the
`trouble_dash` cookie are carriers (§4.1).

### 3.2 A pair that is not a row answers `404`, never `405`

`ServeHTTP` matches `(method, path)` against the table (§2). If no row matches
the path, or the path matches rows but none with this method, the response is
`404` + `TROUBLE-DASHBOARD-009` — **never `405`**. When at least one row matched
the path, the response carries `Allow:` naming those methods (deduplicated);
otherwise no `Allow` header is sent. That `Allow` collection belongs to the
router: a `404` raised *inside* a matched row (a malformed or unknown `{id}` on
rows 3/5/14, an unknown asset on row 19) is the handler's own not-found call with
an empty allow list, so it carries no `Allow` header even though the path is
routable. The body depends on the client:

| Client | Body |
|---|---|
| `HX-Request: true`, or `Accept` containing `application/json` and not `text/html` | JSON refusal envelope (§7.6) with code `TROUBLE-DASHBOARD-009` |
| anything else (a browser) | the embedded static `404.html`, with `Content-Type: text/html`, `Cache-Control: no-store`, `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and the page CSP |

The requested path is never echoed in either body, and a `404` for a non-row is
not the same code as a `404` raised *inside* a matched row (a malformed or
unknown `{id}` on rows 3/5/14, an unknown asset name on row 19, and the unknown
partial of row 20 use the cases listed in §6 and §7).

### 3.3 Panic net

Any handler panic on a request path is recovered and answered `500` +
`TROUBLE-DASHBOARD-013` with detail `handler_panic`; the listener stays up. A
template execution failure (the case the code expects) is `500` +
`TROUBLE-DASHBOARD-007` instead.

## 4. Authentication and scope

### 4.1 Carriers: `Authorization: Bearer` or the `trouble_dash` cookie

Exactly two carriers exist:

* `Authorization: Bearer <token>` — the header must start with `Bearer `;
* cookie `trouble_dash` — host-only, `Path=/`, `HttpOnly`, `SameSite=Lax`,
  `Max-Age=43200`, `Secure` when the effective scheme is https or the bind is
  non-loopback.

A token's plaintext form is `tdt_` + 43 base64url characters (47 bytes); the
store keeps only `sha256(token)[:32]` hex and looks it up constant-time. The
refusals:

| Presented | Answer |
|---|---|
| neither carrier | `401` + `TROUBLE-DASHBOARD-001` |
| both carriers with **different** values | `401` + `TROUBLE-DASHBOARD-002` |
| same value in both carriers | accepted (one identity) |
| malformed, wrong length, unknown or revoked | `401` + `TROUBLE-DASHBOARD-002` |

A token that reaches the dashboard must never be sent in a URL (§3.1). The
dashboard never mints or prints a token: creation is CLI-only, and the store is
re-read per request, so a CLI rotation reaches the running daemon without a
restart.

### 4.2 Scopes, and the `403` that names the missing one

Three scopes exist — `read`, `write`, `autonomy` — and they expand once at token
load in the linear hierarchy **autonomy ⇒ write ⇒ read**. So an `autonomy` token
satisfies every row, a `write` token satisfies the `read` rows too, and a `read`
token satisfies only the `read` rows.

A request whose principal lacks the row's scope answers `403` +
`TROUBLE-DASHBOARD-003`, with `X-Trouble-Required-Scope: read|write|autonomy`
naming what the row wants. The UI renders out-of-scope controls disabled with
their reason rather than hidden, and the scope is re-checked server-side on
every POST, so a forged request from a read-scope session is refused.

### 4.3 The `/health.json` loopback exemption

`GET /health.json` is the one row that can run **without authentication**. The
exemption holds only when all three of these are true: the effective bind is
loopback, `dashboard.health_loopback_exempt` is `true` (the default), and the
request's peer is itself on loopback (after trusted-proxy `X-Forwarded-For`
resolution). On a non-loopback bind the exemption is forced off and the row
requires `read` exactly like the others. Read the row's `read` scope as "the
non-exempt case".

### 4.4 Trust-zone re-check (`503` + `006`)

A listener bound to loopback refuses a non-loopback request
(`503` + `TROUBLE-DASHBOARD-006`, detail `non_loopback_request`), so a proxy that
changes the trust zone cannot silently re-expose the admin surface. Separately,
`X-Forwarded-For` is honored only when `dashboard.proxy_trusted = true` **and**
the peer address is inside `dashboard.proxy_cidrs`; an untrusted client can never
forge the address used for logging, throttling or the rate bucket.

### 4.5 Auth-failure throttle and rate buckets (`429` + `012`)

| Limit | Default | Keyed by | Refusal |
|---|---|---|---|
| auth failures | 10 in 60 s | the presented credential (truncated sha256); a request presenting no grammar-valid credential counts against its client IP | `429` + `TROUBLE-DASHBOARD-012`, detail `auth_failure_throttle` |
| read rate | `read_rps` 20/s, burst 60 | token **and** client IP | `429` + `TROUBLE-DASHBOARD-012`, detail `bucket_empty` |
| write rate | `write_rps` 5/s, burst 10 | token only | `429` + `TROUBLE-DASHBOARD-012`, detail `bucket_empty` |

Every `429` carries `Retry-After` in whole seconds (≥1). Two consequences worth
knowing as a client: a mistyped or rotated token throttles only itself and never
the valid token behind the same NAT, while an unauthenticated flood still stays
throttled per IP; and a write POST consumes the write bucket only **after** CSRF
passes, so a forged cross-site POST cannot burn the operator's write budget.

### 4.6 Token store failures are fail-closed (`503` + `013`)

The 0600 token file is stat'ed per request. When it cannot be read or parsed,
every route except the exempt `/health.json` answers `503` +
`TROUBLE-DASHBOARD-013` (detail `token_store_invalid`) — there is no
last-known-good fallback. A store that is not mode 0600, or that contains a hash
equal to a configured ingestion key, refuses the **boot** instead
(`TROUBLE-LIFECYCLE-013` / `TROUBLE-DASHBOARD-002`, detail
`equals_ingestion_key`). If `dashboard.identity` selects the named-but-absent
`tailscale` or `proxy-header` seam, every authenticated route answers `503` +
`013` with detail `impl_absent`.

### 4.7 `read_only` refuses every POST (`403` + `010`)

With `dashboard.read_only = true`, all three write rows answer `403` +
`TROUBLE-DASHBOARD-010` with detail `read_only`, after auth and CSRF and before
any subsystem call.

## 5. CSRF on every POST

The three POST rows run four checks in order, after authentication and scope and
before the write rate bucket. The first failure is `403` +
`TROUBLE-DASHBOARD-004` and no state changes; `detail` names the failed check so
a UI can explain it without a second round trip.

| # | Check | Failure `detail` |
|---|---|---|
| 1 | **Ambient-credential rule** — a POST carrying `Authorization: Bearer` *and* any `Cookie` header is refused outright | `mixed_credentials` |
| 2 | **Origin binding** — when `Origin` is present it must byte-equal `dashboard.public_origin`, or, on a loopback bind with no public origin configured, the request's own `scheme://Host`; a browser POST with neither `Origin` nor `Referer` is refused | `origin_mismatch`, `missing_origin` |
| 3 | **SameSite** — both cookies are `SameSite=Lax` (a cookie-attribute property, asserted in tests) | — |
| 4 | **Double-submit + binding** — header `X-Trouble-CSRF` must equal the `trouble_csrf` cookie **and** equal `b64url(hmac_sha256(k_csrf, token_id + "\|" + yyyymmddhh))` for the authenticated token | `missing_csrf`, `missing_header`, `cookie_mismatch`, `token_binding` |

What that means for a client:

* **Non-browser (`curl`, a script):** send the `Authorization` header and no
  cookie. Checks 1–2 pass (a credential with no cookie has no ambient
  authority), and check 4 passes vacuously because the browser-only cookie half
  is absent. A Bearer-only POST is legal.
* **Browser:** the page render sets the `trouble_csrf` cookie (not `HttpOnly`,
  `Max-Age=3600`, `SameSite=Lax`, no `Domain`); the client echoes its value in
  `X-Trouble-CSRF` and must send `Origin` (or a `Referer`).

`k_csrf` is 32 random bytes generated at process start and never persisted, so a
restart invalidates every outstanding CSRF value. Values for the current and the
previous hour are accepted, so a value is valid for at most two hours and a
leaked one expires without an operator action. The binding is to the token ID,
so a value derived for another token matches neither candidate.

## 6. POST request bodies

The three write rows accept exactly two content types:

| `Content-Type` | Handling |
|---|---|
| `application/json` (also when the header is **absent**) | decoded as a JSON object |
| `application/x-www-form-urlencoded` | parsed as a form; the **first** value per key wins |
| anything else | `400` + `TROUBLE-DASHBOARD-010`, detail `invalid_body_content_type` |

The body is capped at `dashboard.max_body_bytes` (default **4096**); a body over
the cap is `413` + `TROUBLE-DASHBOARD-011` before any decode. JSON values are
flattened to strings: a string is taken as-is, a boolean as `true`/`false`, a
number as a base-10 integer, an array of strings as a comma-joined value, `null`
is absent, and any other JSON type is `400` + `010` with detail
`invalid_body_json_type`. A malformed JSON body or form body is `400` + `010`
with detail `invalid_body_json` / `invalid_body_form`.

Fields, per row:

| Row | Field | Required | Refusal when wrong |
|---|---|---|---|
| 9 `/api/incidents/{id}/ack` | `reason` | yes | `400` + `010`, detail `invalid_body_reason_required` |
| 9 | `until` | no | passed through to `ladder.Ack` as a `types.Duration`; the dashboard does not validate the duration, the ladder owns its validity |
| 9 | `expected_state`, `ledger_seq` | no | CAS pair — see below; a mismatch is `409` |
| 10 `/api/incidents/{id}/close` | `reason` | yes | `400` + `010`, detail `invalid_body_reason_required` |
| 10 | `resolution` | yes | one of `fixed`, `false_positive`, `wontfix`; anything else is `400` + `010`, detail `invalid_body_resolution` |
| 10 | `expected_state`, `ledger_seq` | no | CAS pair |
| 11 `/api/autonomy` | `mode` | no | one of `shadow`, `assisted`, `full`; `full` additionally needs `dashboard.allow_full`, else `403` + `010`, detail `full_disabled` |
| 11 | `kill_switch` | no | a boolean; unparseable is `400` + `010`, detail `invalid_body_kill_switch`. Setting it is always allowed; **clearing** it needs `dashboard.allow_resume`, else `403` + `010`, detail `resume_disabled` |
| 11 | `grants` | no | a comma-separated list; the empty string sets `[]` |

**Compare-and-set (rows 9 and 10).** A POST that sends `expected_state` and/or
`ledger_seq` (the values the fragment it came from rendered) is refused `409` +
`TROUBLE-DASHBOARD-010` with detail `stale_view` if either no longer holds —
`ledger_seq` is compared against the index's current last sequence. The `409`
JSON body embeds the **refreshed** incident-rows fragment in its `fragment`
field, so a double-tap on a phone cannot double-close an incident and the client
gets the correction in one round trip. The ladder owns the authoritative CAS; the
dashboard's check is the fast path.

**Target validation (rows 9 and 10).** `{id}` is a frozen-prefix ID: `inc_`
(lowercase) followed by a 26-character Crockford base32 ULID (uppercase alphabet,
no `I`, `L`, `O`, `U`), e.g. `inc_01J8ZP4Q0X9V6Y2M3N4P5R6S7T`. A malformed ID is
not a route match and answers the router's `404` + `009` (§3.2). A well-formed but
unknown incident answers `404` with the ladder's own code `TROUBLE-LADDER-012`,
detail `unknown_incident`. Group IDs use `grp_` with the same body shape.

**Author attribution.** Every accepted write is attributed to
`Actor{Kind: human, ID: <token label>}` — the token's own label, with the human
build fields left empty. The response body then carries the subsystem's new view:
the refreshed incident rows for ack/close, the refreshed health strip (with the
gates `SetAutonomy` returned) for autonomy.

## 7. Response shapes

### 7.1 Pages (rows 1–7)

`text/html; charset=utf-8`, `Cache-Control: no-store`, `nosniff`,
`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and the CSP
`default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`.
Every page render also sets the `trouble_csrf` cookie for the authenticated
token, which is what makes the browser POST path of §5 possible. Responses
≥1 KB are gzipped when the client advertised `Accept-Encoding: gzip`; a page
whose render exceeds 256 KiB is refused `500` + `007` rather than streamed.

### 7.2 `/health.json` (row 8)

`application/json`, `Cache-Control: no-store`, `nosniff`,
`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. The body is the single
`HealthResponse` shape the lifecycle assembler and the external stall checker
(`SPEC-12` §3) both consume:

| Field | Meaning |
|---|---|
| `status` | `ok` \| `degraded` \| `stalled` (stalled wins over degraded) |
| `version`, `git_sha`, `build_time` | the build triple; an unstamped build degrades the response |
| `uptime_s` | seconds since process start |
| `ledger_last_seq`, `ledger_last_ts`, `ledger_stall_s` | the writer's position and its lifecycle-provided stall (never recomputed from wall-clock deltas) |
| `sensors[]`, `sources[]` | per-sensor health and per-source liveness; a sensor with no sample yet reports `enabled:true`, `degraded:true`, `reason:"no sample yet"` and is **never omitted** |
| `autonomy` | the autonomy gates (mode, kill switch, grants, changed_by) |
| `breakers[]` | the breaker registry |
| `runtime_watermarks` | binary bytes, RSS, memory high, and the other runtime counters |
| `subsystems[]` | the built/refused block; `built:false` is never a claim of health, and any unbuilt row degrades the status |
| `hub` | the light-hub stanza, **absent** under the standalone profile |
| `detail` | machine-readable causes, e.g. `{"reasons":["unstamped_build"],"subsystem":"…"}` |

The body never carries a payload, message, stack, token or DSN. On a loopback
bind with the exemption on, this is the one route a local consumer (like the
stall checker) can poll without a token.

### 7.3 Write responses (rows 9–11)

A successful write answers `200` with an **HTML fragment**, rendered by the same
template a poll uses, with the full HTML header set of §7.1:

| Row | Body |
|---|---|
| 9 `ack`, 10 `close` | the refreshed incident-rows fragment — the same payload as `GET /partials/incidents` (sequence, stall seconds, count, rows) |
| 11 `autonomy` | the refreshed health-strip fragment — the same payload as `GET /partials/health`, with the new gates |

So a write can be dropped straight into the DOM the way an htmx poll response is.
A `409` (stale view) is a JSON refusal carrying the refreshed fragment in its
`fragment` field instead (§6) — the one case where a write answers JSON to a
form-encoded client.

### 7.4 Partial fragments (rows 12–18, and the guard in row 20)

`text/html; charset=utf-8` with the §7.1 header set, no CSRF cookie. Each
fragment is self-describing for the polling client: a sequence (`data-seq`) and a
stall counter (`data-stall-s`) ride on the fragment, and the template's own
polling attributes carry the schedule (`dashboard.strip_poll_ms`, default 1000 ms
for the health strip; `dashboard.poll_ms`, default 2000 ms for the content
partials).

* **`?since=<seq>`** — accepted by the rows whose content is append-only
  (`/partials/incidents`, `/partials/incidents/{id}/timeline`,
  `/partials/groups`): rows newer than `since` are returned. A `since` that is
  not a base-10 integer is treated as `0`; a `since` **past** the newest sequence
  returns the current top `page_limit` rows (a full resync, deliberately not an
  empty gap).
* **Size cap** — a fragment is capped at 8 KiB. An over-cap table is re-rendered
  with as many complete rows as fit and reports the remainder in a truncation
  marker plus `data-truncated`, because a required incident row is ~129 B and
  200 rows would be ~25 KB; the full set stays reachable a page at a time
  (`dashboard.page_limit`, default 100, clamp 500). The cap is absolute: an
  over-cap fragment is never shipped.
* **Memory-pressure load shed** — when RSS has been at or above
  `dashboard.mem_pressure_pct` (default 80) of `MemoryHigh` for 60 s, **new**
  partial polls answer `503` + `TROUBLE-DASHBOARD-013` with detail
  `mem_pressure` and `Retry-After: 5`. Pages, `/health.json` and the three POSTs
  keep serving.
* **Unknown partial name** — `GET /partials/<one-segment-not-in-rows-12..18>`
  matches row 20 and answers `404` + `TROUBLE-DASHBOARD-008` ("unknown partial"):
  JSON for an htmx/API client, a static
  `<div id="partial-error" data-code="TROUBLE-DASHBOARD-008">unknown partial</div>`
  for a browser. The name is never used as a template name.
* **An incident that disappears between render and poll** (compaction or close)
  answers `200` with an empty-state fragment, not an error: a poll racing a
  legitimate state change is not a failure.
* A **malformed** `{id}` (row 14) is not a route match and answers the router's
  `404` + `009` (§3.2).

### 7.5 Static assets (row 19)

Three embedded assets, served with `ETag` (derived from the bytes,
`"sha256-<32 hex>"`), `Cache-Control: private, max-age=3600`, `nosniff`,
`X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`:

| Asset | `Content-Type` |
|---|---|
| `app.css` | `text/css; charset=utf-8` |
| `app.js` | `text/javascript; charset=utf-8` |
| `htmx.min.js` | `text/javascript; charset=utf-8` |

An `If-None-Match` that contains the asset's ETag answers `304`. Nothing is
fetched from a CDN — the dashboard makes no third-party request. An asset name
that is not in the table is a **matched row that refuses**: the handler calls the
router's not-found path itself with an empty allow list, so the answer is `404` +
`TROUBLE-DASHBOARD-009` with no `Allow` header (§3.2).

### 7.6 The refusal envelope

Every JSON refusal — the router's `404`, a `403`, a `429`, a `409` and the
handler-level `500`s — is this one shape:

```json
{
  "error": { "code": "TROUBLE-DASHBOARD-009", "message": "route not found" },
  "detail": "stale_view",
  "ts": "2026-09-28T12:00:00.000Z",
  "fragment": "<html fragment, only on 409 stale_view>"
}
```

`error.code` is a catalog code (`TROUBLE-DASHBOARD-NNN`, or the owning
subsystem's code when a write seam exposes one, e.g. `TROUBLE-LADDER-012`).
`detail` is present only when the refusal has a machine-readable variant. `ts` is
the server's UTC stamp. Response headers that a client should read:

* `403` + `TROUBLE-DASHBOARD-003` — `X-Trouble-Required-Scope`.
* `429` — `Retry-After: <seconds ≥ 1>`.

## 8. The dashboard refuse codes

All thirteen are catalog codes; the class column is the catalog's own
(`permanent` refusals are not worth retrying, `transient` ones are).

| Code | HTTP | Class | Raised by |
|---|---|---|---|
| `TROUBLE-DASHBOARD-001` | `401` | permanent | no auth material (no Bearer, no `trouble_dash` cookie) |
| `TROUBLE-DASHBOARD-002` | `401` | permanent | auth material invalid, revoked, malformed, two disagreeing carriers, or a store hash equal to an ingestion key |
| `TROUBLE-DASHBOARD-003` | `403` | permanent | under-scoped (`X-Trouble-Required-Scope` names the missing scope) |
| `TROUBLE-DASHBOARD-004` | `403` | permanent | a CSRF check failed (`detail` names which) |
| `TROUBLE-DASHBOARD-005` | `400` | permanent | a token in the URL (query name, query value, or path segment) |
| `TROUBLE-DASHBOARD-006` | `503`/`500` | permanent | runtime: non-loopback request on a loopback listener. Boot (500): non-loopback bind without a mandate |
| `TROUBLE-DASHBOARD-007` | `500` | transient | a template render failed, or a page exceeded its cap |
| `TROUBLE-DASHBOARD-008` | `404` | permanent | unknown partial name (row 20) |
| `TROUBLE-DASHBOARD-009` | `404` | permanent | no row for this `(method, path)`; unknown static asset |
| `TROUBLE-DASHBOARD-010` | `403`/`400`/`409` | permanent | write refused (`read_only`, `resume_disabled`, `full_disabled`), a bad body (`invalid_body_*`), or a stale view (`stale_view`, `409`) |
| `TROUBLE-DASHBOARD-011` | `413` | permanent | POST body over `dashboard.max_body_bytes` |
| `TROUBLE-DASHBOARD-012` | `429` | transient | rate limited (`bucket_empty`) or auth-failure throttled (`auth_failure_throttle`); `Retry-After` set |
| `TROUBLE-DASHBOARD-013` | `503`/`500` | permanent | token store unavailable, identity provider absent (`impl_absent`), the autonomy writer absent (`action_unavailable`), poll load shed (`mem_pressure`), or a recovered handler panic (`handler_panic`) |

## 9. Client recipes

Read a page or a fragment with a token (no cookie, so no CSRF half needed):

```
curl -sS -H "Authorization: Bearer $TDT" http://127.0.0.1:7644/incidents
curl -sS -H "Authorization: Bearer $TDT" http://127.0.0.1:7644/partials/incidents?since=4210
curl -sS http://127.0.0.1:7644/health.json          # loopback + exemption only
```

Acknowledge an incident, once, with the compare-and-set pair the page rendered:

```
curl -sS -X POST -H "Authorization: Bearer $TDT" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"on it","expected_state":"open","ledger_seq":4210}' \
  http://127.0.0.1:7644/api/incidents/inc_01J8ZP4Q0X9V6Y2M3N4P5R6S7T/ack
```

Set the kill switch (always allowed with an `autonomy` token):

```
curl -sS -X POST -H "Authorization: Bearer $TDT" -H 'Content-Type: application/json' \
  -d '{"kill_switch":true}' http://127.0.0.1:7644/api/autonomy
```

What will bite a scripted client: putting the token in a URL (`400` + `005`, even
with a valid header); sending a Bearer header *and* a `Cookie` header together
(`403` + `004`, `mixed_credentials`); posting without a `Content-Type` is fine,
posting with `text/plain` is not; and a `409` arrives as **JSON with an embedded
HTML fragment**, not as a fragment.

## 10. Config keys a client depends on

Defaults from `dashboard.*`; the listener defaults to `127.0.0.1:7644`.

| Key | Default | Client-visible effect |
|---|---|---|
| `bind`, `port` | `127.0.0.1`, `7644` | where the surface exists; a non-loopback bind needs a mandate, a `public_origin` and a project scope |
| `health_loopback_exempt` | `true` | whether `/health.json` needs a token on loopback |
| `page_limit` | `100` (max 500) | rows per page and per partial; also the resync size and the fragment-fit budget |
| `max_body_bytes` | `4096` | POST body cap (`413` + `011` beyond it) |
| `strip_poll_ms` | `1000` | the health-strip poll interval the template emits |
| `poll_ms` | `2000` | the content-partial poll interval |
| `stall_alert_s` | `90` | the advisory stale banner threshold |
| `read_rps` / `read_burst` | `20` / `60` | read bucket (token + IP) |
| `write_rps` / `write_burst` | `5` / `10` | write bucket (token) |
| `auth_fail_limit` / `auth_fail_window` | `10` / `60s` | per-credential throttle |
| `mem_pressure_pct` | `80` | when partial polls start answering `503` |
| `read_only` | `false` | refuses all three POSTs (`403` + `010`, `read_only`) |
| `allow_resume` | `false` | gates clearing the kill switch |
| `allow_full` | `false` | gates `mode:"full"` |
| `public_origin` | unset | required off loopback; the exact `scheme://host[:port]` an `Origin` must equal |
| `proxy_trusted` / `proxy_cidrs` | `false` / unset | whether `X-Forwarded-For` is honored at all |
| `token_file` | `~/.config/trouble/dashboard-tokens.json`; anchored to `<declared state_root>/dashboard-tokens.json` when `state_root` is declared without it (TRBL-085) | the 0600 store; outside the DEFAULT state root on purpose |
| `identity` | `token` | `tailscale` / `proxy-header` select the unimplemented seam → `503` + `013` |

## 11. Known gaps and residuals

1. **No drift check pins this page.** `docs/sentinel-compat.md` is generated from
   the values the server uses and enforced by `compat_test.go`; this page is
   hand-maintained from `routeTable()`, and nothing fails CI if the route table
   grows a row that this page does not list. Closing that needs a Go test in
   `internal/dashboard` (a doc-shape test asserting each `routeTable()` row
   appears here) — a code change, out of scope for this document.
2. **A malformed write body has no code of its own** in the closed `TYPES`
   catalog; it is refused `400` + `TROUBLE-DASHBOARD-010` with
   `detail=invalid_body_<what>`. Recorded here and in `docs/operations.md` §12.
3. **`409` answers JSON to a form-encoded client.** The stale-view correction
   arrives as a JSON envelope with an embedded fragment, so an htmx form client
   needs a JSON-aware error path for that one case.
4. **No routes beyond the 20.** There is no `/issues` index (the issue desk lives
   on the incident story panel), no token-management route (creation is
   CLI-only), no SSE, no metrics endpoint. `/health.json` is the machine-readable
   surface and `docs/operations.md` §12 is its consumer contract.
