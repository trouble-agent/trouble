# trouble — integration report (dogfood run, 2026-09-17)

**Verdict: 🟡 PROMISING-BUT-ROUGH.** The subsystems that boot are genuinely good;
the documented path to a *usable* instance does not work. A fresh user cannot
boot the shipped example config, cannot authenticate to the dashboard from a
stock config, and cannot enable the ingest on-ramp at all.

This document is the record of a real use run: build the binaries, boot the
daemon, drive the dashboard and the ingest plane as a user would, and write down
what happened. It is written so that a later reader can answer "does this
project actually work, and how much of it?" without re-running anything.

- Build under test: `c363daf` (`make bin`, stamped, 1.2 s warm).
- Host: control box, kernel 7.0.0-30-generic, go1.26.5, loadavg ~7.
- Session shape: throwaway state root at `~/dogfood-trouble/state`, ports
  7643/7644, autonomy `shadow`, no real data touched.
- Findings filed on the board: **TRBL-005 … TRBL-010** (plus the install row).

---

## 1. Promise vs. reality (one paragraph)

`trouble` promises (README) an "open-source, self-hosted incident brain for a
fleet of hosts and apps: sensors, an embedded Sentry-compatible sentinel, a
dedup core, an append-only audit ledger, a ladder that plays and researches
fixes, an issue desk, a flow lane, a dashboard and a skill loop" — with the
generic-JSON on-ramp advertised as "one curl from any runtime".

Reality after ~2 h of real use: the **sensors, ledger, ladder and dashboard
work** (74 live incidents from real host sensors, a jq-able JSONL ledger with a
clean durability story, scope-enforced dashboard writes). The **sentinel
ingest plane, the issue desk and the skill loop are not built at all** on the
shipped configuration, and the operator-facing surfaces say nothing about it —
`/health.json` reports `status="degraded"` with `detail.sensor_degraded=inotify`
(a missing rules directory) while three of five subsystems are absent.

So: "half a product, booting green, with an excellent spec suite behind it."

---

## 2. What actually worked (evidence)

| Step | Result |
|---|---|
| `make bin` | two stamped binaries in 1.2 s; `--version` reports the stamped triple |
| daemon boot, corrected config | `rule set loaded from the sensors rules=9 gen=1` → `dashboard listening addr=127.0.0.1:7644 identity=token loopback=true` |
| `GET /health.json` (loopback, no token needed) | 200 with the frozen `HealthResponse` shape, live sensor rows, `runtime_watermarks`, `autonomy{shadow}`, a real breaker |
| sensors on a live host | psi 24→407 events, dbus 259, disk 10, inotify 4, timers 4 — real observations, not fixtures |
| ladder | 74 incidents created from `dbus_unit_failed` + psi rules; storm breaker tripped and recorded (`21 incidents inside 5m0s`) |
| `trouble dashboard token create` | minted `tdt_`+43 (47 chars), store keeps 64-hex sha256 only, `list` never leaks the plaintext |
| dashboard pages | `/`, `/incidents`, `/incidents/{id}`, `/groups`, `/rules`, `/breakers` all 200 (15–32 KB), real rows, incident timeline + poll fragments |
| dashboard auth | anonymous = 401; `?token=` in the URL = 400; read token on a POST = 403 `TROUBLE-DASHBOARD-003 insufficient scope` |
| token hot reload | a token minted after boot is accepted immediately (no restart needed) |
| `check-stall` | exit 0 against a live daemon; `class= seq_age=0s breaches=0` |
| clean drain | SIGTERM → `stage="shutdown"` heartbeat, no corruption, ledger intact |
| the ledger itself | plain JSONL, `seq`/`rec_id`/`sig`/`inc`/`origin`/`actor`(stamped)/`payload`, `redactions` count — `jq`/`grep`-able exactly as advertised |

The design artefacts are real: `SPEC-01..13`, `docs/sentinel-compat.md` with
drift tests, error-code catalog, the "fail closed" scrubbing contract. The
engineering discipline is visible in the running product.

## 3. The four blockers on the documented path

These are the reason the verdict is not SHIPPABLE. Each has a board row.

### 3.1 The shipped example config does not parse (TRBL-005)

The path `deploy/README.md` and `docs/cmd.md` both point at —
"copy `examples/config.toml`" — refuses to boot:

```
$ bin/troubled --config ~/dogfood-trouble/config.toml
level=ERROR msg="boot refused" err="TROUBLE-LIFECYCLE-001: key \"verify.zone_windows\": bad zone_windows pair \"{\""   (exit 13)
```

`examples/config.toml:65` uses TOML inline-table syntax
(`zone_windows = { loopback = "10m", ... }`); the parser
(`internal/lifecycle/config.go:433`) accepts a map **or** the space-separated
string, and `SPEC-12:167` pins the string form
`loopback=10m lan=15m tailnet=20m public=30m`. The example's value reaches the
string branch as a scalar starting with `{`, so the first pair is `"{"`.

**Fix used to continue the run** (in the scratch config only — the repo was not
touched):

```toml
verify.zone_windows = "loopback=10m lan=15m tailnet=20m public=30m"
```

### 3.2 A stock config produces a dashboard nobody can log into (TRBL-006)

With the example config (no `dashboard.token_file` key) the daemon serves, the
CLI mints a token, and that token is refused — on a daemon started *after* the
token existed:

```
GET /incidents  -> 401 {"error":{"code":"TROUBLE-DASHBOARD-002","message":"authentication failed"}}
```

The token's hash matches `sha256(plaintext)` exactly, so mint and store agree.
The cause is a literal `~`: `dashboard.DefaultConfig()` sets
`TokenFile: "~/.config/trouble/dashboard-tokens.json"`
(`internal/dashboard/config.go:58`), `internal/app/dashdeps.go:43` only
overrides it when the lifecycle key is non-empty, and nothing expands `~` inside
the dashboard package. `os.Stat` then fails and
`LoadTokenStore`/`readFileIntoSet` treat **a missing file as a valid empty
token set** — fail-closed, silently, with no log line and no health signal.

Proof (same build, same config, same token, only one line added):

```toml
[dashboard]
bind = "127.0.0.1:7644"
token_file = "/home/user/.config/trouble/dashboard-tokens.json"   # absolute
```

```
GET /incidents -> 200, 15388 bytes, <title>Incidents · trouble</title>
```

`tests/e2e/cli_smoke.sh` passes because it hands the daemon an absolute
`token_file` in its own fixture config; the shipped example does not.

### 3.3 Three of five subsystems are silently not built (TRBL-007)

Every boot writes one `subsystem_not_built` lifecycle record per refused
subsystem (12 records across 4 boots in this run):

```
sentinel | no sentinel projects configured
issues   | TROUBLE-ISSUES-003: config_invalid: driver github needs owner and repo (SPEC-09 §3.9.1)
skills   | TROUBLE-SKILLS-001: config_invalid: exactly one of source_path or source_url must be set
```

Consequences, measured:

* nothing listens on `ingest.bind` — `POST http://127.0.0.1:7643/api/1/event/`
  is connection-refused (`ss` shows only 7644 bound), so the README's headline
  on-ramp is unreachable;
* `/health.json` has **no** `subsystems` key; its `detail` names only the
  inotify sensor;
* the dashboard shows nothing about it;
* the daemon exits 0 and serves, so the boot "looks fine".

There is also no operator-facing way to declare a project: no CLI verb, no
config key in `trouble config explain` — sentinel's project set is only ever
passed in-process (`SubsystemOptions.SentinelProjects`). Enabling the product's
front door is therefore not a configuration task; it is a code change.

### 3.4 No quickstart, and two doc strings that disagree with the code (TRBL-008)

* `README.md` documents `go build` / `go test` and nothing else — no install, no
  run, no config, no first event. Running the thing means finding
  `docs/cmd.md` and `deploy/README.md` on your own.
* `examples/trouble.env` advertises the dashboard bearer as
  `TROUBLE_DASHBOARD_TOKEN=dsh_...`; the CLI mints `tdt_`+43
  (`cli_smoke.sh:115`). The env file is also never read as a dashboard
  credential (the token store plus `IngestionKeys` are the only sources).
* `docs/cmd.md` says the store keeps "only `sha256(token)[:32]`"; the store
  holds 64 hex chars (`cli_smoke.sh:121` asserts 64).

---

## 4. What it looks like when you finally get in (real data, real use)

Once the two config lines above are in place, the product *is* usable, and this
is the part worth keeping:

```
GET /incidents     200  15416 B   "open incidents (74)" — sig, severity, state, rung, age, last event
GET /incidents/inc_01M2R1VPDHYPDFTV98XJRP914K   200  5246 B
GET /partials/incidents/<inc>/timeline          200   453 B  data-seq data-rendered-ts data-stall-s
GET /partials/incidents?since=146               200  7469 B  data-count="42"
GET /partials/budget                            200   -> binary 13.7 MB, rss 31.2 MB, ledger 824.7 KB,
                                                             events 205.0/min, groups open 96, incidents open 74
GET /partials/health                            200   -> status degraded, seq 758, stall 0.31s, shadow, kill off
                                                        denied 2 / csrf 0 / rl 0
```

The htmx contract is visible in the markup and behaves: every fragment carries
`data-seq`, `data-rendered-ts` and `data-stall-s`, so a stale render is
detectable from the DOM (`#stale-banner`, `data-stalled`) — that is the SPEC-10
stale-render guard working.

Writes are correctly gated: a read token gets 403 `insufficient scope` on
`POST /api/incidents/{id}/ack`, every POST route needs the CSRF token from
`<meta name="trouble-csrf">`, and autonomy was `shadow` throughout so nothing
mutated the host.

### 4.1 Two rough edges found in this state (TRBL-010)

* the page footer renders `trouble v () — read-mostly dashboard (SPEC-10)`
  (empty version) while `/health.json` and the budget fragment both carry
  `c363daf`;
* the first server render of the budget panel after a boot shows blanks and
  zeros (`binary`/`rss`/`ledger`/`events` empty, incidents open 0) in the same
  response whose header says `open incidents (74)`; ~9 s later the polling
  fragment carries the real numbers. The data is available in
  `runtime_watermarks` at that moment — it is a first-paint wiring issue, not a
  measurement gap.

### 4.2 Volume behaviour at idle (TRBL-009)

Six minutes on an idle host, throwaway state root:

| measure | value |
|---|---|
| ledger records | 778 (684 event, 74 incident, 16 lifecycle, 4 config) |
| ledger bytes | 843 KB |
| `events_per_min` (health) | 204 |
| groups open / incidents open | 96 / 74 |
| **distinct sigs / inKeys** | **21 / 21** |
| incidents per identity | 3–4 |

The same sig *and* the same inKey re-opens a fresh incident instead of folding
(e.g. `dbus:sha256v1:c158b3e1eac71459` at 16:02:10, 16:03:19, 16:05:41), and one
psi sig produced 137 event records in six minutes. Extrapolated: ~180k
records/day and ~200 MB/day of *append-only, git-distributed, auto-filed*
ledger from one idle host, before any real incident. The storm breaker does fire
(`rule:dbus_unit_failed: 21 incidents inside 5m0s`), so the suppression layer is
partly alive — the fold is the suspect.

---

## 5. Time-to-first-success and friction count

| milestone | time from start |
|---|---|
| binaries built | ~11 min (mostly reading README/specs/docs) |
| daemon serving `/health.json` | ~12 min (after repairing `zone_windows`) |
| first authenticated dashboard page | ~27 min (after diagnosing the `~` token path) |
| first event through the documented on-ramp | **never — not configurable in v0.1** |

**Frictions hit: 10** (4 of them first-success blockers):

1. README has no install/run/first-event path (had to find `docs/cmd.md`).
2. `examples/config.toml` does not parse → boot refusal (TRBL-005).
3. `state_root` under `/tmp` is refused — *correct and well-messaged*
   (`TROUBLE-LIFECYCLE-004: resolves under forbidden root "/tmp"`), noted as a
   good behaviour, but it invalidates the obvious scratch-dir habit.
4. default rules directory absent → `inotify` sensor degraded on every stock
   boot (`watch path .../config/rules.d: no such file or directory`), never
   mentioned in README/docs.
5. no way to declare a project → ingest plane cannot be enabled (TRBL-007).
6. CLI-minted token 401s on a stock config (TRBL-006).
7. footer version renders empty (TRBL-010).
8. first budget render blank/zero (TRBL-010).
9. auth-failure throttle keys on IP: after earlier failures, a request carrying
   a *valid* token returns `429 TROUBLE-DASHBOARD-012 auth_failure_throttle`
   for the rest of the 60 s window (TRBL-010).
10. doc strings that disagree with the code: `dsh_` vs `tdt_`,
    `sha256[:32]` vs 64 hex (TRBL-008).

---

## 6. Trustworthiness

Good:

* clean drain on SIGTERM, `stage="shutdown"` heartbeat, exit 0, no torn lines;
* the ledger survives restarts and re-opens over an existing state root
  (74 incidents persisted across four boots);
* scope enforcement, CSRF, hash-only token storage, URL-borne token refused
  with 400, `X-Frame-Options`/`no-store`/`Referrer-Policy` on responses;
* `/health.json` is real: sensor rows with `enabled/degraded/reason`,
  `sources[]` liveness with `alive`, `autonomy` grants, a breaker with
  `open_until` and a reason string;
* `check-stall` uses the ledger sequence, not the heartbeat — a stale
  `stage="shutdown"` heartbeat left behind by the previous instance did **not**
  produce a false exit 8 immediately after a restart (checked deliberately;
  no finding filed).

Bad:

* **the false-green class**: `status="degraded"` with a sensor detail while
  three of five subsystems are absent — the health surface understates its own
  incompleteness, which is the exact failure mode the project's own doctrine
  ("never observe itself into a green lie") exists to prevent;
* silent failure when the token store path does not resolve (empty set, no log);
* volume at idle (TRBL-009) makes the ledger's "one record per observation"
  assumption expensive in practice.

---

## 7. Installability leg — SKIPPED (explicit)

The ephemeral-bunker install test on `agent-host-3` **did not run**: `bunker
spawn --server agent-host-3 --ttl 2h` failed twice with

```
bunker: spawn agent: deadline_exceeded: context deadline exceeded
```

Host reachable (`HOST_OK`), `bunkerd` active, Docker 26.1.5, `bunker list` shows
no agent registered for either attempt, and no half-spawned `bunker-*` home dir
was left behind (newest home dir predates the attempts), so there was nothing to
clean up. Recorded as the board row **TRBL-011** rather than a silent pass.

Two installability observations that this leg would have tested, gathered from
the repo instead (both are findings, not substitutes for the bunker run):

* **the repo has no git remote** (`git remote -v` is empty), so a fresh machine
  cannot `git clone` it at all — a fresh-user install path has to start from an
  archive/copy today. It also means the documented install path is `make bin`
  plus `trouble install` (systemd), and `make bin` is not in the README;
* there is **no Dockerfile / compose / release matrix yet** (that is TRBL-002,
  already on the board), and `packaging/` holds only polkit rules — so the
  install surface a new user meets is "Go toolchain + make + systemd".

---

## 8. What a maintainer should fix with one hour

1. **TRBL-005** — make the shipped example config boot (one line), and add a
   test that boots `examples/config.toml` itself.
2. **TRBL-006** — expand `~` in the token-store path (or always pass the
   resolved path), and log a WARN when the store loads empty.
3. **TRBL-007** — put the refused subsystems into `/health.json` and make the
   project set configurable; until then the product's front door is decorative.
4. **TRBL-008** — a 15-line README quickstart, exercised by a script.

Everything else on the board is a real but lesser claim.

---

## 9. Reproduction script (no credentials, throwaway state)

```bash
cd ~/trouble && make bin
mkdir -p ~/dogfood-trouble/state && cd ~/dogfood-trouble
cp ~/trouble/examples/config.toml config.toml
sed -i 's|^zone_windows = .*|zone_windows = "loopback=10m lan=15m tailnet=20m public=30m"|' config.toml      # TRBL-005
# state_root/config_path/environment_file -> the scratch dir; then:
printf '[dashboard]\ntoken_file = "%s/.config/trouble/dashboard-tokens.json"\n' "$HOME" >> config.toml        # TRBL-006
bin/troubled --config config.toml &                       # daemon
curl -s localhost:7644/health.json | jq .status
T=$(bin/trouble dashboard token create --config config.toml --label local --scopes read,write 2>/dev/null)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $T" localhost:7644/incidents   # 200
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:7643/api/1/event/ -d '{}'              # 000 (TRBL-007)
```

Tokens above are throwaway values minted per run; nothing in this document is a
live credential.
