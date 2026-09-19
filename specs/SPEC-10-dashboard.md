# SPEC-10 — dashboard: routes, auth, scopes, CSRF, live updates (trouble v0.1)

Spec: SPEC-10
Area prefix: TROUBLE-DASHBOARD
Package: internal/dashboard
Consumed types: SubsystemHealth, HealthResponse, SourceLiveness, RuntimeWatermarks, Token, Scope, AutonomyGates, Incident, Group, IssueRef, Breaker, Evidence, Record, Actor, RecordKind, SensorHealth, Rule, GapRecord, Promotion, SpawnRequest, ResearchOutcome, SkillCandidate
Local types: identityProvider, principal, scopeSet, route, dashError, csrfValue, tokenFile, pageData, incidentRow, groupRow, ruleRow, breakerRow, timelineEntry, budgetPanel, healthStrip, stallState
ACs: AC-16, AC-19, AC-26, AC-30
PRD: §04c, §11

## 1. Purpose

`internal/dashboard` is the read-mostly face of the ledger: one embedded HTTP server in the same binary as
the daemon, serving server-rendered pages and htmx HTML fragments from an in-memory index that SPEC-01
already maintains. It owns four things and nothing else:

1. **The v0.1 route set** (§2.1) — the eight read surfaces, three write actions, seven polling partials,
   the embedded static assets, and one deterministic not-found behaviour.
2. **Auth**: Bearer header or cookie only, three scopes (`read`, `write`, `autonomy`), CSRF on every POST,
   a bind matrix with default-deny off loopback.
3. **Live updates**: 2 s polling of partials with a self-describing stale-render guard, sized so AC-19's
   "live incident appears within 2 s" holds at p100 with a stated margin.
4. **Budget discipline**: no handler mmaps or full-scans the ledger; every read goes through the SPEC-01
   §3.4 index; the dashboard's slice of the ≤80 MB steady-RSS budget is ≤6 MB.

The dashboard is **not** a writer of the ledger. SPEC-INDEX §3.4 allocates record kinds to SPEC-03/04
(event, group, gap, canary), SPEC-05 (incident, verify, breaker) and SPEC-12 (config, lifecycle); the
dashboard emits none. Every click it accepts is turned into a call on the owning subsystem, which writes
the auditable record (§4.2). That single rule is what makes AC-19's "all read-only actions work without
write grants" and AC-26's "ledger + dashboard show every step" both true without a second write path.

Non-negotiables honored here: dashboard `:7644` (config-driven, startup bind preflight fails loud),
state-root-adjacent secret handling (0600), zero external CDN, zero fleet paths, no shell for the agent.

## 2. Interface

### 2.1 Route table — the complete v0.1 route set (§1.Q, 21 rows)

`(method, path)` is matched against this table only; Go 1.26 `http.ServeMux` patterns (`"GET /incidents"`),
no regex routes, no third-party mux. `{id}` is an exact `<prefix>_<ULID>` match.

| # | Method | Path | Auth scope | Response type | Data source |
|---|---|---|---|---|---|
| 1 | GET | `/` | read | HTML page | index counters (`IncidentsOpen`, `GroupsOpen`, `EventsPerMin`) + `HealthResponse` (§2.9/§3.3), including its `subsystems` table: one row per late-landing subsystem with built/refused, the refusal code and its reason (SPEC-12 §3.3a), so a degraded status names its cause on the page instead of leaving it to be inferred from a missing route |
| 2 | GET | `/incidents?page_token=&page_size=` | read | HTML page | `Index.OpenIncidents(page_token,page_size)` (kind `incident`, page-token grammar owned by SPEC-01 §2.3a) + `Index.LastEventAge(sig)`; the page footer carries `next_page_token`, and a token naming a dropped generation renders the reset banner instead of an error |
| 3 | GET | `/incidents/{id}` | read | HTML page | `Index.IncidentStory(inc)` — incident, group, `Evidence`, ledger records, `IssueRef`, `tsk_` board row, `ResearchOutcome`, `SpawnRequest`, `Promotion`, `SkillCandidate` |
| 4 | GET | `/groups?page_token=&page_size=&rank=` | read | HTML page | `Index.Groups(rank,page_token,page_size)` (kind `group`); ranked-by-rate stays index-only per §2.9 — the token continues a long list, it never re-ranks — and the page footer carries `next_page_token` |
| 5 | GET | `/groups/{id}` | read | HTML page | `Index.Group(grp)` + `Index.EventsBySig(sig,window)` counters (never payload scans) |
| 6 | GET | `/rules` | read | HTML page | SPEC-03 rule-set snapshot + `Index.RuleStats(name)` (last fire, fire count, suppressed count) + `SensorHealth` |
| 7 | GET | `/breakers` | read | HTML page | `Index.Breakers()` (kind `breaker`, SPEC-05 registry) |
| 8 | GET | `/health.json` | read¹ | JSON `HealthResponse` | §2.9 aggregator; **also consumed by the external stall checker (SPEC-12 §3)** |
| 9 | POST | `/api/incidents/{id}/ack` | write | HTML fragment (200) | `ladder.Ack(ctx, inc, actor, reason, until)` → refreshed `incident-rows` |
| 10 | POST | `/api/incidents/{id}/close` | write | HTML fragment (200) | `ladder.Close(ctx, inc, actor, reason, resolution)` → refreshed `incident-rows` |
| 11 | POST | `/api/autonomy` | autonomy | HTML fragment (200) | `lifecycle.SetAutonomy(ctx, gates, actor)` → refreshed `health-strip` |
| 12 | GET | `/partials/health` | read | HTML fragment | §2.9 aggregator (strip fields only) |
| 13 | GET | `/partials/incidents?since=<seq>` | read | HTML fragment | `Index.OpenIncidentsSince(seq,limit)` |
| 14 | GET | `/partials/incidents/{id}/timeline?since=<seq>` | read | HTML fragment | `Index.RecordsForIncident(inc,since,limit)` |
| 15 | GET | `/partials/groups?since=<seq>` | read | HTML fragment | `Index.GroupsSince(seq,limit)` |
| 16 | GET | `/partials/rules` | read | HTML fragment | same source as #6, table rows only |
| 17 | GET | `/partials/breakers` | read | HTML fragment | same source as #7, table rows only |
| 18 | GET | `/partials/budget` | read | HTML fragment | `RuntimeWatermarks` + autonomy-mode burn counters |
| 19 | GET | `/static/{asset}` | read | embedded bytes | `app.css` (7.4 KB), `app.js` (3.1 KB), `htmx.min.js` (47,755 B, sha256 `e1f2a3…` pinned in §3.3) via `go:embed`; `ETag` + `Cache-Control: private, max-age=3600` |
| 20 | GET | `/partials/{name}` (unmatched name) | read | HTML fragment (404) | whitelist lookup fails → TROUBLE-DASHBOARD-008 |
| 21 | any | unmatched `(method,path)` | — | static 404 (browser) / JSON error (API) | TROUBLE-DASHBOARD-009, §2.1.3 |

¹ `/health.json` is the one route that can run without a token: it is served unauthenticated **only when the
listener is bound to loopback** and only when `dashboard.health_loopback_exempt=true` (default). Reason: the
external stall checker must be able to distinguish "daemon dead" (connection refused) from "token store
broken" (503 + TROUBLE-DASHBOARD-013) without holding a credential. On a non-loopback bind the exemption is
forced off and `read` scope is required. The body never carries payload, message, stack, token material or
DSN material — only `HealthResponse` fields.

**AC-16 coverage map.** Incidents render on `/` (open-incident table) and on `/incidents` +
`/incidents/{id}`; groups render on `/groups` and `/groups/{id}`; breakers render on `/breakers` and
`/partials/breakers`; the issue desk and flow surfaces (the `IssueRef` rows, the `tsk_` board row, the
`Promotion` and its PR link) render inside the `/incidents/{id}` story panel — one page per bug is the
design. A standalone `/issues` index is not part of the §1.Q v0.1 route set and carries no AC of its own.

#### 2.1.1 Write-action bodies

| Route | Body (max 4096 B, §2.8) | Accepted content types |
|---|---|---|
| ack | `{"reason":"…","until":"15m","expected_state":"verifying","ledger_seq":41207}` | `application/json`, `application/x-www-form-urlencoded` |
| close | `{"reason":"…","resolution":"fixed\|false_positive\|wontfix","expected_state":"verifying","ledger_seq":41207}` | same |
| autonomy | `{"mode":"shadow\|assisted\|full","kill_switch":false,"grants":["rule:io-pressure"],"reason":"…"}` | same |

`expected_state` + `ledger_seq` are the compare-and-set pair rendered into the fragment; on mismatch the
dashboard returns **409** with TROUBLE-DASHBOARD-010 (`detail:"stale_view"`) plus the refreshed fragment, so
a double-tap on a phone cannot double-close an incident. The ladder owns the CAS (SPEC-05 §2).

#### 2.1.2 Partial fragment contract (exact HTML)

Each partial returns one root element and nothing else — no `<html>`, `<head>`, `<body>`, no inline
`<script>`; ≤8 KB uncompressed; `Cache-Control: no-store`; `Content-Type: text/html; charset=utf-8`. The
root element always carries the stale-render guard attributes of §2.6.

| Partial | Root element returned |
|---|---|
| `/partials/health` | `<div id="health-strip" data-seq data-stall-s data-status data-mode data-kill data-denied data-csrf data-rl hx-get="/partials/health" hx-trigger="every 1s" hx-swap="outerHTML" hx-sync="this:replace">ok · seq 41207 · stall 1.2s · shadow · kill off · denied 3 / csrf 0 / rl 0</div>` |
| `/partials/incidents` | `<tbody id="incident-rows" data-seq data-count data-stall-s>` + one `<tr>` per open incident: `<td><a href="/incidents/inc_…">title</a></td><td>severity</td><td>state</td><td>rung</td><td>age</td>` |
| `…/{id}/timeline` | `<ol id="incident-timeline" data-seq>` + one `<li>` per ledger record: `<li data-kind="tool_call" data-actor="dash-write@laptop">09:14:03 tool_call config.set check_mode · diff 1 entry</li>` |
| `/partials/groups` | `<tbody id="group-rows" data-seq>` + `<tr>` per group: sig short, title, count, rate/min, last seen, release range |
| `/partials/rules` | `<tbody id="rule-rows" data-seq>` + `<tr>` per rule: name, source, entry rung, last fire, suppressed, breaker state |
| `/partials/breakers` | `<tbody id="breaker-rows" data-seq>` + `<tr>` per breaker: scope, state, open until, trips, reason |
| `/partials/budget` | `<div id="budget-panel" data-seq>` — RSS/binary bytes, ledger bytes, spool bytes, events/min, open groups/incidents, worktrees, agent/play burn, version+git_sha strip |

A GET partial route is an ordinary GET: it requires `HX-Request` to be absent OR present (both are served),
so `curl` and the stall checker can consume the same fragments a browser polls. htmx swaps with
`hx-swap="innerHTML settle:0s"` for `tbody` targets and `outerHTML` for `#health-strip` / `#budget-panel`.

#### 2.1.3 Not-found and wrong-method behaviour (TROUBLE-DASHBOARD-009)

One deterministic rule covers both: any request whose `(method, path)` pair is not a row of §2.1 is answered
**404** with TROUBLE-DASHBOARD-009, and a known path with an unsupported method also returns **404** (never
405) with the `Allow` header listing the methods the path does accept. Determinism beats HTTP pedantry here:
a single code path means the error surface has no variant to get wrong, and the body never echoes the
requested path (no reflected-XSS surface).

- Browser (`Accept` contains `text/html`): a **static** 1-line HTML document served from an embedded
  `404.html` (≤400 B) — it is never produced by `html/template`, so a broken template set cannot break the
  error path (proved in §7 by pointing the 404 handler at a deliberately broken template set).
- API (`Accept` contains `application/json`) or `HX-Request: true`: `{"error":{"code":"TROUBLE-DASHBOARD-009","message":"route not found"},"ts":"…"}`.
- Every 404/405 increments `dash_notfound_total` and is logged at WARN with the peer IP and `(method,path)`
  length only. No ledger record (the dashboard emits no kinds).

### 2.2 Auth: token, scopes, refusals

**Token material (two carriers, no third):**

1. `Authorization: Bearer <token>` header.
2. Cookie `trouble_dash=<token>` with `Path=/; HttpOnly; SameSite=Lax`, `Secure` when the effective scheme is
   https or the bind is non-loopback, `Max-Age=43200`, no `Domain` attribute (host-only: a sibling service on
   the same host can never receive it).

**Never a URL.** A token presented in the path or in a query string is refused. On every route the dashboard
inspects the raw query string and path for the parameter names
`token, access_token, auth, apikey, api_key, key, trouble_token, tdt` — case-insensitive — and for any path
segment that matches the token grammar `^tdt_[A-Za-z0-9_-]{43}$`. A hit is refused **before** authentication
evaluation with **400** + TROUBLE-DASHBOARD-005, even when the request would otherwise authenticate, and the
offending value is never logged (length + parameter name only). This is the one refusal that is deliberately
louder than success: an ingested URL is already in a browser history, a proxy log and a chat scrollback, and
a 200 would let the leak go unnoticed.

**Scope model.** Three scopes, one linear hierarchy, expanded once at token load:
`autonomy ⇒ write ⇒ read`. `Scope` values are exactly `read | write | autonomy` (SPEC-TYPES §3.12).

| Scope required | Routes |
|---|---|
| read | 1–8, 12–20 |
| write | ack (9), close (10) |
| autonomy | autonomy (11) |

Missing material → **401** + TROUBLE-DASHBOARD-001. Present but unknown/revoked/malformed/wrong-length/
duplicate-carrier-with-different-values → **401** + TROUBLE-DASHBOARD-002, and counted as an auth failure for
the throttle. Authenticated but under-scoped → **403** + TROUBLE-DASHBOARD-003; the `Allow`-equivalent is the
`X-Trouble-Required-Scope` response header so the UI can explain the refusal without a second round trip.
Token comparison is `subtle.ConstantTimeCompare` over `sha256(token)[:32]` hex; the plaintext is never held
in a variable beyond the request that presented it.

**Refusals that are not scope failures:** `dashboard.read_only=true` refuses all POSTs with **403** +
TROUBLE-DASHBOARD-010 (`detail:"read_only"`); clearing the kill-switch (`kill_switch:false` while it is
`true`) requires `dashboard.allow_resume=true` or returns **403** + TROUBLE-DASHBOARD-010
(`detail:"resume_disabled"`); selecting `mode:"full"` requires `dashboard.allow_full=true` or returns 403 +
TROUBLE-DASHBOARD-010 (`detail:"full_disabled"`) — `full` is merge-by-policy, so it is a config decision, not
a phone click. Enabling the kill-switch (`kill_switch:true`) is **always** allowed with an `autonomy` token.
`ack` and `close` are always allowed with a `write` token, including while the kill-switch is on: they are
precisely the human decisions the freeze exists to wait for.

**Read-only usability without write grants (AC-19).** A `read`-scope token renders every page and partial
in full, including every ack/close/autonomy control — the controls render `disabled` with
`data-requires="write"` or `"autonomy"` and an inline reason, never hidden (a hidden control reads as "this
feature does not exist"). UI state is presentation only: the server re-checks the scope on every POST and
returns **403** + TROUBLE-DASHBOARD-003 even when a client forges the request. That is the assertion the
`integration/e2e_dashboard_test.go` case in §7 makes.

### 2.3 CSRF — every POST

Applies to routes 9, 10, 11 without exception. Four checks, evaluated in order; the first failure returns
**403** + TROUBLE-DASHBOARD-004 and no state change:

1. **Ambient-credential rule.** A POST carrying `Authorization: Bearer` **and** any `Cookie` header is
   refused outright. Mixing the two credential paths is how session-fixation and CSRF bypasses are built;
   the dashboard refuses the mixture rather than guessing which one the caller meant.
2. **Origin binding.** When `Origin` is present it must byte-equal `dashboard.public_origin`, or — for a
   loopback bind — the `scheme://Host` of the request. A mismatch is refused. Absent `Origin` + absent
   `Referer` is accepted only for the Bearer path (non-browser clients); a browser POST without either header
   is refused.
3. **Same-Site cookie policy.** The auth cookie and the CSRF cookie are both `SameSite=Lax`, so no modern
   browser attaches either to a cross-site POST at all; this check is the belt to the Origin braces and is
   asserted in §7 by header inspection.
4. **Double-submit + binding.** The shell renders `<meta name="trouble-csrf" content="<v>">` and the
   embedded `app.js` copies it into the `X-Trouble-CSRF` header on every htmx request. The POST must satisfy
   all three: header present; header value byte-equal to cookie `trouble_csrf`; and header value equal to
   `b64url(hmac_sha256(k_csrf, token_id + "|" + yyyymmddhh))` computed server-side for **that** token ID.
   The cookie is `HttpOnly=false` (the template must read it), `Secure` whenever the auth cookie is,
   `Max-Age=3600`, no `Domain`. `k_csrf` is 32 random bytes generated **at process start and held only in
   memory** — a restart invalidates every outstanding CSRF value, and there is no persisted CSRF key file to
   steal. Values from the current or the previous hour are accepted, so a value is valid ≤2 h and a leaked
   one expires without an operator action.

A Bearer-only POST from `curl` or the CLI therefore needs only checks 1–2 to hold (no cookie → no ambient
authority); the double-submit rule is satisfied vacuously because the browser-only cookie half is absent.

### 2.4 Bind matrix and trust zones

`dashboard.bind` default `127.0.0.1`, `dashboard.port` default `7644` (config-driven; startup bind preflight
fails loud with TROUBLE-LIFECYCLE-003 on collision, per SPEC-12).

| Bind | Zone | Mandate required | Auth | Health exemption |
|---|---|---|---|---|
| `127.0.0.1` / `::1` | loopback | none | token (or header/cookie per §2.2) | yes (default) |
| configured LAN/tailnet address | lan / tailnet | `dashboard.mandate = "proxy"` **or** `dashboard.mandate = "tailnet"` | token, `Secure` cookies forced on | no |
| any non-loopback address | public | `dashboard.mandate = "proxy"` + `dashboard.proxy_trusted = true` + `dashboard.project_scope` non-empty | token, TLS terminated by the mandated reverse proxy | no |

Startup validation, all fail-loud before the listener opens:

- Non-loopback bind with no mandate → refuse to start, **TROUBLE-DASHBOARD-006**, exit non-zero. There is no
  warn-and-continue path: an admin UI exposed by accident is indistinguishable from an admin UI exposed on
  purpose.
- Non-loopback bind with more than one configured `[[projects]]` entry and an empty `dashboard.project_scope`
  → refuse to start with TROUBLE-LIFECYCLE-001 (`detail:"project_scope_required"`). v0.1 tokens carry no
  project dimension (SPEC-INDEX §6.4, single-tenant per host); per-project scoping is mandatory *before* any
  multi-tenant exposure, so the configuration that would create that exposure cannot boot.
- Live bind is re-checked per request: a request arriving on a non-loopback connection while the config says
  loopback (a listener handed over by a proxy) is refused with **503** + TROUBLE-DASHBOARD-006.
- `X-Forwarded-For` is honored **only** when `dashboard.proxy_trusted=true` and the peer address is inside
  `dashboard.proxy_cidrs`. Otherwise the peer address is the client identity used by the rate limiter and by
  every log line; an untrusted client can never forge its own bucket.

### 2.5 Identity seam (token impl in v0.1; the second impl is a named seam)

```go
// package dashboard — unexported by construction: no third party can implement it in v0.1.
type identityProvider interface {
    Name() string                                  // "token" (v0.1) | "tailscale" | "proxy-header"
    Identify(r *http.Request) (principal, error)   // error ⇒ TROUBLE-DASHBOARD-013
}

type principal struct {
    ID     string   // token label, e.g. "dash-write@laptop" — becomes Actor.ID on writes (§4.2)
    Scopes scopeSet // expanded read|write|autonomy set
}
```

`dashboard.identity = "token"` selects the v0.1 implementation (store, hashing, rotation: §3.2). The
tailnet/proxy-header provider (`"tailscale"`, `"proxy-header"`) is **the named seam** — the interface above is
its entire v0.1 surface, no v0.1 implementation ships, and selecting it returns **503** +
TROUBLE-DASHBOARD-013 (`detail:"impl_absent"`) at the login boundary. The seam exists because T4's real
answer to "auth from a phone" is tailnet identity, and adding it as a second `Identify` implementation must
not touch the route table, the CSRF rules, or the actor mapping. **(v1.0 hand-off.)**

### 2.6 Live updates: 2 s partial polling, with a stale-render guard (SSE is a v1.0 hand-off)

**v0.1 mechanism:** htmx polling of the §2.1.2 partials. Content partials (13–18) poll `every 2s` — this is
the mandated interval. The `#health-strip` partial (12) polls `every 1s` and is the *accelerator*: when its
`data-seq` changes, `app.js` dispatches a `troubleSeq` DOM event that the content partials listen to via
`hx-trigger="every 2s, troubleSeq from:body"`, so a ledger append refreshes the visible rows immediately
instead of up to 2 s of latency. Sampling remains the source of truth and the accelerator never substitutes
for it — the same doctrine as SPEC-03 §3.2's PSI triggers. With the accelerator stopped, AC-19 still passes
at p50 and p95; with it running, the p100 guarantee below holds. Every polled element sets
`hx-sync="this:replace"` so a slow response is dropped instead of queued (no request pile-up when the daemon
is loaded), and `app.js` suspends all polling while `document.visibilityState === "hidden"` for ≥10 s and
forces one refresh on `visibilitychange` (a phone in a pocket must not burn tailnet traffic).

**Stale-render guard.** Every partial root element carries three attributes written by the renderer:
`data-seq` (the ledger seq the fragment was rendered from), `data-rendered-ts` (RFC3339 UTC ms), and
`data-stall-s` (seconds since the ledger last advanced, i.e. `HealthResponse.ledger_stall_s`). The banner
`#stale-banner` becomes visible when **either** (a) the last three consecutive `#health-strip` responses
carried an identical `data-seq` **and** the latest `data-stall-s` ≥ `dashboard.stall_alert_s` (default
`90s` = 3× the SPEC-12 30 s heartbeat), or (b) any poll returned 429/5xx, or (c) no poll has succeeded for
5× the poll interval. Identical seqs alone are normal on a quiet host — the signal is a *growing stall
counter*, which is the same positive-evidence rule as SPEC-05's verification. The banner is advisory: the
authoritative stall alarm is SPEC-12's external checker hitting `/health.json`; a phone browser is not a
pager.

**AC-19 verification (trigger → first partial containing the incident ≤2000 ms).** The test drives a real
trigger, then polls `/partials/incidents?since=<seq>` and timestamps the first response whose body contains
the incident ID. Budget, with the p99 targets this spec commits to:

| Step | Bound |
|---|---|
| trigger → incident record appended and inserted into the index | ≤50 ms (the `fsync` rides the ≤200 ms group-commit window and is **off** the render path) |
| index change → `#health-strip` poll returns the new seq | ≤1000 ms (1 s accelerator interval) |
| `troubleSeq` → refreshed `/partials/incidents` fragment | ≤20 ms render |
| **worst case** | **≤1070 ms** (930 ms margin against AC-19) |

**(v1.0 hand-off):** `GET /events` as a Server-Sent Events stream is the reserved route for a future
push transport (SPEC-INDEX §3.3 defers SSE). It is not implemented in v0.1, not polled by v0.1 clients, and
no `EventSource` code ships.

### 2.7 Render stack and static assets

- `html/template` only (never `text/template`, never `templ`): the whole template set is parsed once at
  startup from `go:embed` FS into a single `*template.Template`; a parse or execute-time define collision is
  a startup failure (refuse to boot) rather than a 500 on every page.
- `vendored htmx.min.js` (47,755 B measured on this fleet's schedulerd) at
  `internal/dashboard/static/htmx.min.js`, embedded with `//go:embed static/*`. No build chain: no Node,
  no bundler, no codegen step, no `go generate` in CI.
- Dark/lean CSS in one embedded `app.css` (target ≤8 KB): CSS custom properties for the palette, one column
  below 640 px, tap targets ≥44 px, `prefers-color-scheme` respected but dark by default, no web fonts.
- Mobile-first because the primary client is a phone on the tailnet: `viewport` meta, no hover-only
  affordances, tables collapse to definition lists under 640 px, and every page renders readable with JS
  disabled (progressive enhancement: partials poll, the page itself is complete server-side).
- **No external CDN and no third-party request of any kind** (no fonts, no icons, no analytics, no
  source maps). Reason beyond offline safety: an admin UI that fetches from a CDN leaks the existence,
  timing and identity of the admin surface to a third party, and a host that is diagnosing a network fault
  must not depend on the network. This is asserted by test: every `src`/`href` in the rendered HTML is
  same-origin (§7).
- Response compression: gzip only (stdlib `compress/gzip`), enabled for HTML ≥1 KB; the JSONL-measured
  3.0× ratio (1,064 B → 350 B per event) is the sizing input, not a promise.

### 2.8 Security headers, rate limiting, payload caps

Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, and `Cache-Control: no-store` (no admin data in a shared phone browser's
cache, and no stale form on the back button). Every HTML response carries
`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self';
connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'` — all JS and CSS live in
embedded files, so `'self'` suffices with no inline script and no inline style.

| Limit | Value | Failure |
|---|---|---|
| POST body | `dashboard.max_body_bytes` = 4096, enforced by `http.MaxBytesReader` before any decode | **413** + TROUBLE-DASHBOARD-011 |
| Request headers | `MaxHeaderBytes` = 16 KiB | connection closed before routing |
| Read / write / idle timeouts | 10 s / 10 s / 60 s (`WriteTimeout` sized for 100 concurrent partial renders) | connection closed |
| Read-scope requests | `dashboard.rate.read_rps` = 20/s, burst 60, per token **and** per client IP | **429** + `Retry-After` + TROUBLE-DASHBOARD-012 |
| Write requests | `dashboard.rate.write_rps` = 5/s, burst 10, per token | 429 + `Retry-After` + TROUBLE-DASHBOARD-012 |
| Auth failures | 10 failures for one presented credential inside 60 s → that credential is throttled for 60 s (a request presenting no grammar-valid credential is counted and throttled per client IP — §2.8a) | 429 + `Retry-After` + TROUBLE-DASHBOARD-012 |
| Metadata in responses | never the token, its hash, the CSRF secret, or a DSN key | — |

Retry-After is the integer seconds until the bucket refills (≥1). Two phones polling seven partials on the
2 s/1 s schedule consume ≈9 req/s, comfortably inside the read bucket.

### 2.8a Auth-failure throttle identity (amendment)

The auth-failure throttle counts failures per **throttle identity**, resolved per request in this order:

1. A request that carries exactly one credential carrier value (the `Authorization: Bearer` header or the
   `trouble_dash` cookie) whose value satisfies the §2.2 token grammar `^tdt_[A-Za-z0-9_-]{43}$` is keyed on
   **that credential** (`sha256(value)[:8]`, hex — the plaintext is never retained).
2. Every other request — no material at all, a value that cannot be a token, or two carriers with different
   values — is keyed on the **client IP** (§2.4 resolves the peer, trusted-proxy `X-Forwarded-For` included).

`auth_fail_limit` failures for one identity inside `auth_fail_window` throttle **that identity** for the
window; a throttled identity answers **429 + `Retry-After` + TROUBLE-DASHBOARD-012**
(`detail:"auth_failure_throttle"`), which is distinguishable from the **401** an auth rejection carries, and a
successful authentication clears its identity's failure history. Two consequences are normative:

* **A valid token is never throttled by failures it did not make.** Ten failures from a mistyped or rotated
  token throttle that token, never the client IP — an operator holding a good token on the same phone, behind
  the same proxy or the same NAT keeps serving. (Pre-amendment, one bad token locked the whole IP out for 60 s
  with a 429 that read like an auth rejection.)
* **An unauthenticated flood is still throttled**, because a request that presents no grammar-valid credential
  is counted against, and blocked by, its client IP.

Residual, stated rather than implied: failures spread across many *distinct* grammar-valid credentials are not
IP-throttled. The token space (§3.2, 32 random bytes) is what makes that a non-threat; the throttle is a
brake on repetition, not a credential-search bound. The failure map is bounded (`maxThrottleKeys`) and pruned
by window, so credential-keyed entries cannot grow without limit (§2.9).

### 2.9 Memory budget and the no-scan rule

Measured on this host for this stack: **7.99 MB** binary (`net/http` + `html/template` + `go:embed` of
~100 KB assets), **8.36 MB** (+ `godbus/dbus/v5` + `golang.org/x/sys`), **12.05 MB** (+ pure-Go
`modernc.org/sqlite`); the fleet's comparable daemon is **22.4 MB binary / 35.9 MB RSS** with the identical
htmx + Go-template + embedded-asset dashboard, and its `htmx.min.js` is 47,755 B. All builds are
`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`.

**The dashboard's allocation inside the fleet budget (steady RSS ≤80 MB, load ≤192 MB, per §1.H):**

| Item | Ceiling |
|---|---|
| Dashboard RSS delta, steady | ≤6 MB |
| Dashboard RSS delta, 100 concurrent partial renders | ≤12 MB |
| Parsed template set | ≤1.5 MB |
| Per-request buffers | 64 KiB × in-flight requests (the HTML writer streams to the socket; no full-page buffer beyond 256 KiB) |
| Ledger-derived state held by the dashboard | **0 bytes** — it holds read-only snapshots of the SPEC-01 index, not a copy |

**No-scan rule (normative):** no handler may `mmap`, `open`, `ReadFile`, `bufio.Scan` or otherwise walk a
ledger file. Every read named in §2.1 goes through the SPEC-01 §3.4 in-memory index (§3.3 lists the exact
accessors this spec requires). A view that would need a scan is paginated over the index instead, and a
`since=<seq>` fetch is served from the index's bounded ring, never by re-reading the file. Two consequences
the budget depends on: (a) the index is published as an immutable snapshot via `atomic.Pointer` so a render
never takes the writer's lock (the writer publishes at most every 100 ms, or on group-commit completion);
(b) render cost is O(rows on the page), capped by `dashboard.page_limit` (default 100 rows, max 500).

**Pagination follows the same rule.** `/incidents` and `/groups` carry SPEC-01 §2.3a page tokens straight
into the index: a page renders at most `page_size` rows (default 500, max 5000) with no file access, and a
token whose generation the retention sweep has dropped renders the reset banner with a **200**, because the
dashboard must never turn "the file aged out" into a visible error. The optional `hub` stanza of
`HealthResponse` (SPEC-13 §3.1) is rendered in the same health strip as the ledger watermarks — one surface,
one strip, no second endpoint.

**Worst-case self-pressure.** RSS `≥ dashboard.mem_pressure_pct` (default 80%) of `MemoryHigh` for 60 s
stops accepting *new* partial polls with **503** + `Retry-After: 5` while still serving pages,
`/health.json` and POSTs. The daemon's own memory pressure is itself a recordable event (SPEC-12 owns the
record); the dashboard's contribution is to shed poll load, not to die.

## 3. Data model

### 3.1 Local (package-private) types

None of the types below appear in an exported signature, an HTTP body, a ledger payload or a TOML schema —
the reason they are local and not in SPEC-TYPES §3.

```go
type scopeSet uint8                                  // bit 0 read, bit 1 write, bit 2 autonomy
type route struct {                                  // one row of §2.1, the only source of routes
    Method, Path string
    Scope        Scope                              // "" = no auth (§2.1 row 8 loopback exemption is runtime)
    Handler      http.HandlerFunc
}
type dashError struct {                              // every refusal path builds one of these
    Code    string        // TROUBLE-DASHBOARD-NNN
    HTTP    int
    Message string
    Detail  string        // machine-readable: read_only | resume_disabled | stale_view | project_scope_required
}
type csrfValue struct { Value string; Hour string }  // derived value + the hour window it belongs to
type tokenFile struct { Version int; Tokens []Token } // on-disk wrapper; Token is shared (SPEC-TYPES §3.12)
type pageData struct {                               // shell + page model
    Title, Nav string
    Seq   uint64
    CSRF  string
    CSRFC string                                     // fragment payloads only
    Scope Scope
    ReadOnly bool
    Error *dashError
}
type incidentRow struct { Inc string; Title string; Severity Severity; State LadderState; Rung Rung; AgeS float64; Sig string }
type groupRow struct { ID, Sig, Title string; Count uint64; RateMin float64; LastSeenTS string; ReleaseRange []string }
type ruleRow struct { Name string; Source SigSource; EntryRung Rung; LastFireTS string; Suppressed uint64; Breaker BreakerState }
type breakerRow struct { Scope string; State BreakerState; OpenUntil string; Trips int; Reason string }
type timelineEntry struct { TS, Kind, ActorID, Summary string; Inc string }
type budgetPanel struct { RW RuntimeWatermarks; Autonomy AutonomyGates; Version, GitSHA string }
type healthStrip struct { Seq uint64; StallS float64; Status, Mode string; Kill bool; Denied, CSRF, RL uint64 }
type stallState struct { LastSeq uint64; Repeat int; LastOK string; Banner bool }
```

### 3.1a First paint and the footer stamp (amendment)

A page is rendered once and then kept live by the §2.6 polls, so the FIRST response must already be
self-consistent — a value a poll later corrects is a lie on the operator's screen:

* **The budget panel is seeded on every page render** from the same source row 18 polls
  (`RuntimeWatermarks` + the version accessor, §2.1 row 18). A render that cannot read a watermark still
  renders the panel (empty field, never a fabricated number), but it never renders zeroes for counters the
  page's own header block is showing in the same response.
* **The footer's build stamp reads the version accessor the health surface reports** (§2.9, `/health.json`'s
  `version`/`git_sha`). It is never a template field left unpopulated: an empty stamp in a footer means the
  build is genuinely unstamped, exactly as `/health.json` says.

### 3.2 Token store (hashed at rest, 0600, plaintext shown once)

`dashboard.token_file` default `~/.config/trouble/dashboard-tokens.json` — deliberately **not** inside the
state root (SPEC-INDEX §6.4/SPEC-TYPES §6.1): the state root is the ledger's home, it is rotated, compacted
and backed up, and credentials must never ride along in a ledger backup. File mode **0600**, parent dir
**0700**, ownership = the daemon's user, checked at startup (TROUBLE-LIFECYCLE-013 on a wrong mode).

```json
{"version":1,"tokens":[
 {"id":"dash-read@phone","hash":"a3f1c9d2e5b74a8c0f1e2d3c4b5a6978","scopes":["read"],"created_ts":"2026-09-16T09:00:00.000Z","revoked":false,"last_used_ts":"2026-09-16T09:14:03.221Z"},
 {"id":"dash-write@laptop","hash":"b71e0c93a5d24f68c1b9e7d0a2f4c856","scopes":["write"],"created_ts":"2026-09-16T09:01:00.000Z","revoked":false,"last_used_ts":""}
]}
```

- **Plaintext form:** `tdt_` + 43 chars of base64url(32 CSPRNG bytes) = 47 chars. The `tdt_` prefix makes a
  leaked token greppable by the secret scanner; SPEC-02's rule set gains a named rule
  `dashboard_token` = `tdt_[A-Za-z0-9_-]{43}` (targets: `event_msg`, `journal_tail`, `header`, `config_snapshot`).
- **At rest:** `sha256(token)[:32]` hex only (`Token.Hash`). Plaintext is printed exactly once at creation
  by `trouble dashboard token create --label <label> --scopes read|write|autonomy` and is unrecoverable.
  Creation is CLI-only: **v0.1 has no token-management route**, which is why §2.1 contains no `/api/tokens`.
- **Reload semantics:** the store is `stat`ed per request; an mtime/size change triggers a re-read into a new
  immutable set swapped via `atomic.Pointer`. A parse/IO failure is **fail-closed**: every route except
  loopback `/health.json` returns **503** + TROUBLE-DASHBOARD-013 (`detail:"token_store_invalid"`) — no
  last-known-good fallback, because "the credential file is broken" must not silently keep a rotated-away
  token alive. In-flight requests keep the set they loaded.
- **Rotation/revocation:** `trouble dashboard token rotate <label>` mints a new label
  (`<label>+<yyyymmdd>`) and revokes the old entry in the same atomic file rewrite (temp + `fsync` +
  `rename`) — **no grace window**, because a grace window is an undocumented second credential. Revoked
  entries stay in the file (audit) with `revoked:true`. `LastUsedTS` is written at most once per 60 s per
  token (write amplification control; a crash loses ≤60 s of `LastUsedTS`, never a credential).

### 3.3 Read API required from SPEC-01 §3.4 and the health aggregator

The dashboard requires exactly these index accessors — all O(rows returned), none touching a file:

```
IncidentStory(inc string) (IncidentStory, error)     // bounded: 1 incident, ≤200 records, ≤20 KiB
OpenIncidents(limit, cursor int) ([]incidentRow, uint64 /*seq*/)
OpenIncidentsSince(seq uint64, limit int) ([]incidentRow, uint64)
Groups(rank string, limit int) ([]groupRow, uint64)  // rank = rate | count | trend
GroupsSince(seq uint64, limit int) ([]groupRow, uint64)
Group(grp string) (Group, uint64)
EventsBySig(sig string, window Duration) (uint64, float64)  // counters only, never payloads
RecordsForIncident(inc string, since uint64, limit int) ([]timelineEntry, uint64)
RuleStats(name string) (lastFireTS string, fires, suppressed uint64)
Breakers() ([]breakerRow, uint64)
LastEventAge(sig string) (float64, bool)
```

`HealthResponse` (shared type, SPEC-TYPES §3.12) is assembled once per request by
`BuildHealth(ctx)` inside `internal/dashboard` from: SPEC-12 (version, git_sha, build_time, uptime,
heartbeat, autonomy gates), SPEC-03 (`[]SensorHealth`), SPEC-04 (`[]SourceLiveness`), SPEC-05
(`[]Breaker`), SPEC-01 (`ledger_last_seq`, `ledger_last_ts`, `ledger_stall_s`, `RuntimeWatermarks`).
It is the same struct `/` renders and the stall checker parses; there is no second health shape.

### 3.4 Config keys (all with defaults; no fleet value anywhere)

| Key | Default | Meaning |
|---|---|---|
| `dashboard.bind` | `127.0.0.1` | listener address (§2.4) |
| `dashboard.port` | `7644` | listener port (config-driven; preflight fails loud) |
| `dashboard.mandate` | `""` | `proxy` \| `tailnet` — required off loopback |
| `dashboard.proxy_trusted` / `dashboard.proxy_cidrs` | `false` / `[]` | trust `X-Forwarded-For` only from these peers |
| `dashboard.public_origin` | `""` | exact `scheme://host[:port]` used for Origin binding and CSRF |
| `dashboard.project_scope` | `[]` | per-project scoping; required with >1 project off loopback |
| `dashboard.identity` | `token` | identity seam selector (§2.5) |
| `dashboard.token_file` | `~/.config/trouble/dashboard-tokens.json` | 0600 secret store (§3.2) |
| `dashboard.poll_ms` / `dashboard.strip_poll_ms` | `2000` / `1000` | content-partial / accelerator intervals (§2.6) |
| `dashboard.stall_alert_s` | `90` | stall banner threshold (3× the 30 s heartbeat) |
| `dashboard.health_loopback_exempt` | `true` | `/health.json` exemption on a loopback bind |
| `dashboard.read_only` | `false` | refuse all POSTs (010) |
| `dashboard.allow_resume` / `dashboard.allow_full` | `false` / `false` | gate kill-switch clearing / `mode:"full"` |
| `dashboard.page_limit` | `100` (max `500`) | rows per page/partial |
| `dashboard.max_body_bytes` | `4096` | POST body cap |
| `dashboard.rate.read_rps` / `read_burst` | `20` / `60` | read bucket |
| `dashboard.rate.write_rps` / `write_burst` | `5` / `10` | write bucket |
| `dashboard.auth_fail_limit` / `auth_fail_window` | `10` / `60s` | auth-failure throttle, keyed by credential when one is presented and by client IP otherwise (§2.8a) |
| `dashboard.mem_pressure_pct` | `80` | share of `MemoryHigh` that sheds poll load |

## 4. Wiring

### 4.1 Dependencies, imports, startup/shutdown

Imports (one-way, never reversed): `internal/types`, `internal/ledger` (index read API only — never the
writer), `internal/sensors`, `internal/sentinel`, `internal/ladder`, `internal/research`, `internal/flow`,
`internal/issues`, `internal/lifecycle`. Nothing imports `internal/dashboard` except `cmd/trouble`.

Startup sequence (each step fails loud, in order):

1. Load + validate config (SPEC-12 precedence: flag > env > file > default); `dashboard.bind`/`mandate`/
   `project_scope` validation per §2.4 (TROUBLE-DASHBOARD-006, TROUBLE-LIFECYCLE-001).
2. Bind preflight on `:7644`; collision → TROUBLE-LIFECYCLE-003, exit non-zero (never silently pick another
   port; the reviewer's measured `:8643` collision on this host is exactly what this check exists for).
3. Load + verify the token store (mode 0600 → TROUBLE-LIFECYCLE-013; parse → TROUBLE-DASHBOARD-013 at first
   use, fail-closed per §3.2). Reject any token whose plaintext byte-equals a configured project
   `PublicKey`/`SecretKey` (§4.3).
4. Parse the embedded template set; register the route table from §2.1 (a duplicate `(method,path)` is a
   startup panic by construction — the table is the only registration path).
5. Generate `k_csrf` (in-memory only), start the rate limiter and the request/log counters.
6. Serve. `SIGTERM`: stop accepting, drain in-flight requests for ≤5 s, close the listener. No ledger state
   exists to flush — the dashboard is a reader.

### 4.2 Write actions → owning subsystem → ledger record → actor

The dashboard never appends to the ledger; each action is a call on the subsystem that owns the record kind
(SPEC-INDEX §3.4). The actor is always the human on the other end of the token: `Actor.Kind = human`,
`Actor.ID` = the token label (`Token.ID`), `Actor.Version`, `Actor.GitSHA`, `Actor.BuildTime` = `""`
(SPEC-TYPES §3.1: `""` for human).

| Action | Call | Record kind | Payload | Actor |
|---|---|---|---|---|
| ack | `ladder.Ack(ctx, inc, actor, reason, until)` | `incident` (SPEC-05) | `{transition:"acknowledged", reason, until, via:"dashboard"}` | `{kind:"human", id:"dash-write@laptop"}` |
| close | `ladder.Close(ctx, inc, actor, reason, resolution)` | `incident` (SPEC-05) | `{transition:"closed", reason, resolution, via:"dashboard"}` | same |
| autonomy | `lifecycle.SetAutonomy(ctx, gates, actor)` | `config` (SPEC-12) | `{autonomy:AutonomyGates}` incl. `ChangedBy`, `ChangedTS` | same |

Every click is therefore auditable by reading the ledger alone: `dash-write@laptop` closed
`inc_01J9…` at `2026-09-16T09:14:05.221Z` with reason `false positive`. `/partials/incidents/{id}/timeline`
renders exactly those rows, which is also how AC-26's "ledger + dashboard show every step" is satisfied for
human steps. Refusals that never reach the subsystem (401/403/404/413/429) are visible in the
`#health-strip` counters (`denied`, `csrf`, `rl`) and in the daemon's journal at WARN with the token label —
the dashboard emits no ledger kind of its own, by SPEC-INDEX §3.4.

### 4.3 The dashboard token is never an ingestion key

Three enforced separations, because "one credential for both surfaces" is how an admin UI becomes an
ingestion bypass:

1. **Different grammars:** ingestion keys are 32 hex (DSN `PublicKey`/`SecretKey`, SPEC-TYPES §3.6);
   dashboard tokens are `tdt_` + 43 base64url chars.
2. **Generate-time refusal:** `trouble dashboard token create` refuses to mint a token whose plaintext
   byte-equals any configured project key (CLI exit non-zero, TROUBLE-DASHBOARD-002,
   `detail:"equals_ingestion_key"`), so pasting a DSN key into the token field cannot succeed.
3. **Load-time refusal:** the daemon re-checks every token hash at startup and refuses to serve with
   TROUBLE-DASHBOARD-002 (`detail:"equals_ingestion_key"`) if the file was hand-edited to contain one. A
   Bearer value that matches a project key is refused as an auth failure (TROUBLE-DASHBOARD-002) and counted
   by the §2.8 throttle; ingestion routes (SPEC-04) never accept a `tdt_` token.

## 5. Errors

All thirteen codes exist in SPEC-TYPES §5 inside the TROUBLE-DASHBOARD range 001–013 (SPEC-INDEX §3.5); no
code outside the range is emitted. Classes are as cataloged. Cross-area codes referenced
(TROUBLE-LIFECYCLE-001/003/013, TROUBLE-LADDER-012) belong to their owning specs.

| Code | Class | HTTP | Trigger | Body |
|---|---|---|---|---|
| TROUBLE-DASHBOARD-001 | permanent | 401 | no token (no Bearer, no cookie) and the route is not the loopback health exemption | `{"error":{"code":…,"message":"authentication required"}}` |
| TROUBLE-DASHBOARD-002 | permanent | 401 | token unknown, revoked, malformed, wrong length, two carriers with different values, or equal to an ingestion key | `{"error":{"code":…,"message":"authentication failed"}}` (never says which check failed) |
| TROUBLE-DASHBOARD-003 | permanent | 403 | authenticated but under-scoped | JSON + `X-Trouble-Required-Scope` header |
| TROUBLE-DASHBOARD-004 | permanent | 403 | POST failed Origin binding, double-submit, token binding, or mixed Bearer+cookie | JSON, `detail` names the failed check |
| TROUBLE-DASHBOARD-005 | permanent | 400 | token present in the path or query string | JSON + WARN log (param name + length only) |
| TROUBLE-DASHBOARD-006 | permanent | 500 at startup / 503 per request | non-loopback bind without mandate, or a non-loopback request on a loopback listener | startup: process exit + journal line |
| TROUBLE-DASHBOARD-007 | transient | 500 | template execute failed at render time | static error fragment, template set left intact, counter `dash_render_errors_total` |
| TROUBLE-DASHBOARD-008 | permanent | 404 | partial name not in the whitelist, or a registered fragment absent from the compiled set | JSON or static fragment |
| TROUBLE-DASHBOARD-009 | permanent | 404 | no `(method,path)` row matches (including wrong method on a known path) | static HTML (browser) / JSON (API), never echoes the path |
| TROUBLE-DASHBOARD-010 | permanent | 403 / 409 | `read_only`, kill-switch clearing without `allow_resume`, `mode:"full"` without `allow_full`, or a stale-view CAS mismatch (409, `detail:"stale_view"`) | JSON + refreshed fragment on 409 |
| TROUBLE-DASHBOARD-011 | permanent | 413 | POST body > `dashboard.max_body_bytes` | JSON + `Retry-After` omitted (not a rate condition) |
| TROUBLE-DASHBOARD-012 | transient | 429 | read/write bucket empty, or the §2.8a auth-failure throttle is active for this request's identity | JSON + `Retry-After: <seconds ≥1>` |
| TROUBLE-DASHBOARD-013 | permanent | 503 | identity seam impl absent, token store unreadable/unparsable (fail-closed per §3.2) | JSON; loopback `/health.json` keeps serving |

Ledger mirror rule (SPEC-INDEX §5.3): a refusal that reaches a subsystem is recorded by that subsystem in its
own kind's `payload.error_code`; the dashboard's own refusals are counted and logged as §4.2 states.

## 6. Edge cases

1. **Incident disappears between render and poll** (compaction or close): a partial for an unknown incident
   returns **200** with an empty-state fragment — a poll racing a legitimate state change is not an error.
   Only a POST against an unknown ID is a failure, and it returns **404** carrying the ladder's
   TROUBLE-LADDER-012 (cross-area code reference, SPEC-INDEX §5 rule 2) so the UI can say "already closed".
2. **Double-tap / two phones on one incident:** the fragment's `expected_state` + `ledger_seq` make the
   second POST a **409** + TROUBLE-DASHBOARD-010 (`stale_view`) with a refreshed fragment. No double-close.
3. **Mid-request token file replacement:** the request keeps the set it loaded; the next request sees the new
   set. A rotation cannot half-apply within one request.
4. **Kill-switch on:** reads, partials, ack/close and enabling it keep working; only the loop stops
   (SPEC-05's checkpoint semantics). The strip renders `kill on` in the error color plus `data-kill="true"`.
5. **Template execute failure on one page:** that page returns 500 + TROUBLE-DASHBOARD-007 as a static
   fragment; the rest of the dashboard keeps serving. A *parse* failure is a startup failure (§2.7) — the two
   are deliberately different severities.
6. **Phone tab left open for a day:** polling suspends when hidden ≥10 s; on return, one immediate refresh
   plus a resumed 2 s/1 s schedule; `Cache-Control: no-store` forces a fresh render rather than a cached page.
7. **Very long strings** (10 KB incident title, `</script>`-bearing fingerprint): everything is passed as
   template *data*; `html/template`'s contextual escaping applies in element and attribute context; there is
   no `template.HTML` conversion anywhere in the dashboard. A title is truncated at 200 runes at render time
   for layout, never truncated in the ledger.
8. **Clock skew between phone and host:** every timestamp rendered is RFC3339 UTC from the server; ages are
   computed server-side; no client clock influences a rendered value or an authorization decision.
9. **Non-UTF-8 or invalid UTF-8 payload text:** rendered through `strings.ToValidUTF8(s, "\uFFFD")` before
   escaping — SPEC-02 is the scrubber, not the renderer, and the dashboard must not be the place where
   invalid bytes become an XSS or a blank page.
10. **Rate limiter fairness:** buckets are keyed by token ID and by client IP; an authenticated hammering
    client cannot starve another token, and an unauthenticated flood cannot consume an authenticated bucket.
11. **`since=<seq>` larger than the index's newest seq** (a client resuming after a boot): returns the
    current top `page_limit` rows with `data-seq` set to the current seq, which the client treats as a full
    resync rather than a gap.
12. **Index snapshot older than the request's `since`** (writer paused): the fragment is still correct — the
    `data-stall-s` attribute is the signal, and the stall banner (§2.6) is the user-visible consequence.
13. **Health aggregator partial failure** (a sensor subsystem has no sample yet): the corresponding
    `SensorHealth`/`SourceLiveness` entry reports `enabled:true, degraded:true, reason:"no sample yet"` —
    never an omitted array element, because an absent entry reads as "healthy" on a dashboard.
14. **Reverse-proxy path rewriting** without `dashboard.public_origin` set: Origin binding then falls back to
    the request `Host` on loopback only; off loopback an empty `public_origin` is a startup failure
    (TROUBLE-LIFECYCLE-001), because a wrong Origin rule silently protects nothing.

## 7. Testing

All tests are `internal/dashboard` package tests plus one end-to-end test that starts the real daemon
(`go test -race -count=1 ./internal/dashboard/...`). Numeric thresholds are pass/fail, not advisory.

| Test file | Cases | Pass thresholds |
|---|---|---|
| `routes_test.go` | table completeness (21 rows, each row has a scope or is the documented health exemption); every `(method,path)` reachable through a real `http.ServeMux`; duplicate registration fails; **every route answers 404 for a query-string token** (`?token=`, `?sdt=` etc.); no route accepts a token in a path segment | 0 unregistered rows; 0 routes reachable with a URL-borne token (**AC-19/AC-16** basis) |
| `auth_test.go` | matrix: absent token (001), 6 malformed forms (002), revoked (002), 3 scope refusals (003), both carriers equal, both carriers different (002), loopback health exemption on/off, non-loopback request on a loopback listener (006), ingestion-key-equal token (002), identity impl absent (013), token store unreadable (013, and `/health.json` still 200 on loopback), the §2.8a throttle: 10 failures for one credential then a **valid** token on the same client IP (200), an unseen credential still 401, a no-material flood throttled per client IP | 30+ cases, all codes/statuses exact |
| `csrf_test.go` | missing header, wrong header, cookie≠header, value from 3 hours ago, value bound to a different token ID, Origin mismatch, Bearer+cookie mixture, correct browser POST, correct curl Bearer POST | every failure → 403 + 004 with no state change; both legal paths → 200 |
| `partials_test.go` | each of the 7 partials returns its exact root `id`, carries `data-seq`/`data-rendered-ts`/`data-stall-s`, contains no `<html>`/`<body>`/`<script>`, and is ≤8 KB with 200 rows of fixtures; the whitelist rejects an unknown name (008) | 7/7 exact; 0 inline scripts |
| `render_test.go` | AC-16 golden render: fixture index with incidents, groups, issue refs, board row, breakers → `/`, `/incidents`, `/incidents/{id}`, `/groups`, `/groups/{id}`, `/rules`, `/breakers` each contain the expected entities; every `src`/`href` in every rendered page is same-origin (no CDN); 404 path served with a deliberately broken template set still returns 200-byte static HTML; first-paint budget: the server-rendered panel's `incidents open`/`groups open` equal the page's own header counters and its byte/rate/version rows equal what `/partials/budget` serves; the footer stamp on all seven pages equals the `version`/`git_sha` **`/health.json` reports** | **AC-16** renders all four entity classes; 0 external origins; 404 independent of templates; 0 zero-valued first-paint rows; footer stamp identical to the health surface |
| `live_test.go` | **AC-19 timing**: trigger a fixture incident, poll `/partials/incidents?since=` at the real intervals, record `ts_response − ts_trigger` of the first fragment containing the ID; 20 iterations; assert p100 ≤2000 ms and p50 ≤1100 ms; assert the strip-accelerator-off variant still passes p95 ≤2000 ms; assert the stall banner appears when the writer is paused ≥`stall_alert_s` | p100 ≤2000 ms (**AC-19**), p95 ≤2000 ms without the accelerator |
| `budget_test.go` | boot with a fixture index of 10,000 groups / 50,000 records; 100 rps of `/partials/incidents` for 60 s; RSS delta sampled every 250 ms; a panicking ledger-accessor stub proves **0** file opens during renders | RSS delta ≤12 MB (steady ≤6 MB), p99 render ≤20 ms, 0 ledger file opens |
| `integration/e2e_dashboard_test.go` | start the daemon, ack/close/autonomy against the mock ladder + lifecycle, assert the recorded `Actor{kind:human, id:<token label>}` and the ledger record kind; `read`-scope token can hit every GET and partial and gets 003 on all three POSTs; `/health.json` parsed into `HealthResponse` with `ledger_last_seq` advancing | **AC-19** read-only clause; 3/3 posts refused; actor ID exact |
| `pagination_test.go` | **AC-30** (dashboard half): `/incidents` and `/groups` walked with `page_token` + `page_size` over a 10,000-incident fixture index — page-size stability, no row repeated, no row skipped, `next_page_token` empty exactly at the end; a token minted before a simulated generation drop renders the reset banner with **200**; every render opens 0 ledger files (§2.9) | every row exactly once per walk; the drop case is 200 + banner, never 500; 0 file opens |

## 8. hilo impact

- **Packages/files created:** `internal/dashboard/` — `server.go` (listener, timeouts, graceful drain),
  `routes.go` (the §2.1 table, the only registration path), `auth.go`, `tokens.go`, `csrf.go`, `identity.go`,
  `ratelimit.go`, `render.go` (template set + partials), `health.go` (`BuildHealth`), `view.go` (row models),
  `templates/` (9 files: shell + 7 page bodies + `404.html`), `static/` (`htmx.min.js`, `app.css`, `app.js`).
- **Fan-out (this package's dependencies):** `internal/types`, `internal/ledger` (index read API only),
  `internal/sensors`, `internal/sentinel`, `internal/ladder`, `internal/research`, `internal/flow`,
  `internal/issues`, `internal/lifecycle` — 9 internal packages, all one-way (subsystems import
  `internal/types`; nothing imports back).
- **Fan-in:** exactly one producer — `cmd/trouble`'s `serve` command constructs and starts the handler. No
  `internal/*` package imports `internal/dashboard`, so the package cannot participate in a cycle and a
  change here cannot ripple into SPEC-01..09, 11 or 12 at compile time.
- **Blast radius:** greenfield repository `~/trouble` — no fleet repository, service, port or
  credential is touched. The only cross-repo effects are at runtime and are configuration, not code: the
  `:7644` default listener and the SPEC-12 unit that supervises the process.
- **Interface surfaces others depend on:** `dashboard.Serve(ctx, cfg, deps) error`,
  `dashboard.BuildHealth(ctx) (types.HealthResponse, error)` (the stall checker's payload),
  `dashboard.ValidateConfig(cfg) error` (called by SPEC-12's config validation) and the required-index-accessor
  list in §3.3 (the contract SPEC-01 must satisfy). Changing any of these four is a cross-spec change and
  requires editing this file and SPEC-12 in the same commit.
