# SPEC-12 — lifecycle: unit, watchdog chain, upgrades, config, topology (trouble v0.1)

Spec: SPEC-12
Area prefix: TROUBLE-LIFECYCLE
Package: internal/lifecycle
Consumed types: ConfigValue, Heartbeat, ForwardEnvelope, SpoolEntry, Topology, TopologyDecision, Duration, HealthResponse, SubsystemHealth, SensorHealth, SourceLiveness, AutonomyGates, Breaker, RuntimeWatermarks, Record, RecordKind, Origin, Actor, GapRecord, Evidence, Sig, Severity, ProfileConfig, HubStatus
Local types: explainRow, unitTemplate, bindProbe, stallVerdict, spoolSegment, spoolState, upgradePlan, secretFileCheck, zoneWindow
ACs: AC-14, AC-18, AC-25, AC-26, AC-27, AC-28, AC-29
PRD: §09, §11, §12

## 1. Purpose

`internal/lifecycle` owns everything that decides whether the daemon is *allowed to run, allowed to serve,
and replaceable without losing work*: config resolution with per-value provenance, the state root and its
file modes, the systemd unit pair and the escalation path that does not depend on trouble, the heartbeat
and the external ledger-stall checker, version stamping, the bind preflight, rename-over upgrades with
park/resume, and the hub↔satellite forward/spool path with the T1..T5 topology decisions.

Three invariants hold across the whole spec:

1. **Trouble never reports its own death.** Every alarm path leaves the process (`sd_notify` watchdog,
   `OnFailure=`, the external checker, the escalation unit) and the escalation path is writable while the
   daemon is dead.
2. **Process liveness is not health.** The externally checked signal is *ledger sequence advance*, which
   is why §3.3 makes sequence advance unconditional even on an idle host.
3. **Every topology is the same code path with different configuration.** T1 is a hub with zero satellites;
   nothing in T1 is a fork.

AC-25 is partial in v0.1 (SPEC-INDEX §6.1): this spec ships the hub/satellite configuration split, the
forward path, the spool and ack-based trim; the separately packaged light-mode binary is a v1.0 hand-off and
its only in-scope trace is §3.7.

AC coverage: **AC-14/AC-18** are exercised by the bind matrix, the forward path and the spool (§3.5, §3.7); **AC-25** by the satellite forward + pull-down mechanics (§3.7); **AC-26** by the kill-switch persistence, the autonomy record and the watchdog chain (§3.2, §3.6). Sections no AC reaches are **design constraints**: the config precedence and `explain` contract (§3.3), the unit/sandbox decisions (§2, §3.1), the upgrade park/resume rules (§3.6) and the refusal classes (§5, §6) are obligations of this subsystem rather than user-visible acceptance criteria — the loop records them as constraints per SPEC-INDEX §7 step 2.

## 2. Interface

### 2.1 CLI surface (all paths, ports, unit names and hosts are configuration)

| Command | Contract |
|---|---|
| `trouble install [--scope user\|system] [--root DIR] [--check] [--dry-run] [--force]` | Render + write `lifecycle.unit_name` and the escalate unit into the configured unit dir; `--root DIR` writes into a temp unit root for tests. On a real scope: `systemctl daemon-reload`, `enable --now` (unless `--dry-run`). Exit 13 on any refusable condition. |
| `trouble upgrade [--to PATH\|VERSION] [--rollback] [--wait DURATION]` | §3.6. Never truncates a live binary. Refuses while a park is impossible. |
| `trouble config explain [--key K] [--json]` | The resolved-config dump: `[]ConfigValue`, one row per resolved key, with `source` + `source_ref`. `--json` is the machine form; default is an aligned table. Secret-class keys print `[REDACTED:config]`. There is no reveal flag. |
| `trouble topology` | Prints `[]TopologyDecision` for the configured topology plus the derived zone table. |
| `trouble check-stall [--health-url URL] [--state-root DIR] [--json]` | The external stall checker (§3.3). Exit 0 ok · 8 liveness-surface stale/unreadable · 9 ledger sequence stall. Writes only `checker.alarm` and stdout. |
| `trouble escalate --unit NAME` | Invoked by `trouble-escalate@.service` only. Reads systemd state + journal for NAME, delivers via `escalate.channels`. Touches no ledger, no state-root write path, no socket. |
| `trouble --version` + `troubled --version` | `version git_sha build_time` triple from §3.4. |

`trouble init` (DSN generation) is SPEC-04's command; this spec constrains it exactly once: the DSN host it
generates is `ingest.advertised_host`, never the bind address (§3.2 bind preflight). `trouble hub …`
(profile status, archival, dedup probe, drain) is SPEC-13's command group; this spec constrains it once
too: `trouble hub status` reports through the `HealthResponse` contract of §3.3 and the one health surface
of §2.2, never through an endpoint of its own.

### 2.2 HTTP surface

| Listener (config) | Route | Owner |
|---|---|---|
| `dashboard.bind` (default `127.0.0.1:7644`) | `GET /health.json` → `HealthResponse` | route table SPEC-10 §2; contract + field semantics + the checker's consumption rules are §3.3 here |
| `ingest.bind` (default `127.0.0.1:7643`) | `POST /api/{project_id}/envelope/` (the forward path uses this route, §3.7) | SPEC-04 |

There is exactly one health surface. A second health endpoint is how the ALL-GREEN class returns, so §3.3
also defines the file-based fallback (`heartbeat.json`) for the case where the dashboard listener is wedged
while the daemon lives.

### 2.3 systemd units (templates embedded via `//go:embed`, installed by `trouble install`)

| Unit | Role |
|---|---|
| `trouble.service` | the daemon: `Type=notify`, `WatchdogSec=60`, `Restart=always`, `StartLimitIntervalSec=0`, `OnFailure=trouble-escalate@%n.service` (§3.5) |
| `trouble-escalate@.service` | oneshot escalator, instantiated with `%n` of the failing unit; no dependency on trouble |
| `trouble-stall.service` + `trouble-stall.timer` | the external checker on `checker.interval`; `OnFailure=trouble-escalate@%n.service` |

Unit **names** are config (`lifecycle.unit_name`, `lifecycle.escalate_unit`, `lifecycle.checker_unit`), so a
hub and a satellite can coexist on one host with two state roots.

### 2.4 Go surface

```go
// internal/lifecycle — package lifecycle

// config (§3.1)
func Resolve(args []string, env []string, cfgPath string) (Resolved, error) // flag > env > file > default
func Explain(r Resolved, keys []string) ([]types.ConfigValue, error)       // the dump; secrets redacted
func WriteConfigRecord(w ledger.Writer, r Resolved) error                  // one `config` record per boot/reload

// state root (§3.2)
func CheckStateRoot(cfg Config) (StateRoot, error)   // 0700, owned, local fs, not /tmp → 004/005
func CheckSecretFiles(cfg Config) ([]types.ConfigValue, error) // 0600 set → 013
func PreflightBinds(cfg Config) ([]bindProbe, error) // resolve + bind every listener → 003

// version + heartbeat + health (§3.3, §3.4)
func Version() (version, gitSHA, buildTime string, unstamped bool)
func Actor(kind types.ActorKind, id string) types.Actor
func HeartbeatLoop(ctx context.Context, cfg Config, w ledger.Writer, sensors func() map[string]string) error
func Health(cfg Config, in HealthInputs) types.HealthResponse
func StallCheck(ctx context.Context, cfg Config) (stallVerdict, error)

// units (§3.5), upgrades (§3.6), forwarding (§3.7)
func RenderUnits(cfg Config) ([]unitTemplate, error)      // 006 on a non-renderable ExecStart
func AuditUnits(cfg Config, scope Scope) ([]types.ConfigValue, error) // OnFailure present → 016
func Upgrade(ctx context.Context, cfg Config, plan upgradePlan) error  // 011 on park failure
func CheckSchemaCompat(stateRoot string, maxSupported int) error       // 012
func ForwardLoop(ctx context.Context, cfg Config) error                // spool → envelope → ack → trim
func TopologyDecisions(cfg Config) []types.TopologyDecision
func ZoneOf(bind, source string, auth AuthForm) string                 // loopback | lan | tailnet | public
```

`internal/ledger` does **not** import this package: the app wiring calls `lifecycle.Actor(...)` once and
injects the resulting `Actor` into the ledger writer constructor (§8).

### 2.5 Config key naming (mechanical, no hand-written mappings)

`a.b_c` → env `TROUBLE_A_B_C` → flag `--a-b-c`. Env vars are read only with the `TROUBLE_` prefix. A dot
becomes a dash and an underscore is kept: `state_root` is `--state_root`, `secrets.environment_file` is
`--secrets-environment_file`, `lifecycle.unit_name` is `--lifecycle-unit_name`. Every registered key of §3.1
is flag-addressable in that form, and the flag source beats env, file and default (§3.1).

The shipped unit's `ExecStart` renders exactly one argument pair, `--config <path>`; `RenderUnits` scans every
argument it renders with the SPEC-02 mandatory rule set and refuses an `ExecStart` that carries secret-shaped
material (TROUBLE-LIFECYCLE-006). That scan is the install-time half of the argv-secret control (§3.2); §2.5a
states the boot-time half and the whole surface `troubled` accepts.

### 2.5a What the daemon accepts on argv

`troubled` owns four argv forms itself (`--config <path>`, `-v`, `--version`, `-h`/`--help`) and forwards
every other argument to the resolver, so the surface a harness or a second instance can use without editing a
file is:

| argv | Meaning |
|---|---|
| `--config <path>` / `--config=<path>` | The config file resolution reads. The operator spelling of the `config_path` key; `--config_path <path>` is the same selection in the §2.5 mechanical form. The last of the two spellings on argv wins. |
| `-v` | debug logging. |
| `--version` | the §3.4 version triple, exit 0. |
| `-h`, `--help` | the daemon's real surface (these flags plus the per-key form), exit 0. |
| `--<key> <value>`, `--<key>=<value>`, bare `--<key>` | any registered key of §3.1, spelled by the §2.5 rule. A bare `--<key>` is the value `true`. |

Rules, pinned:

- **The registry adjudicates a key flag, never the daemon binary.** An unknown key is TROUBLE-LIFECYCLE-001
  naming the argument and the daemon exits 13; it is never ignored and never read as a positional, because a
  typo that runs on a default is worse than a boot that refuses. A one-dash token that is not `-v` or `-h` is
  a usage error (exit 2): the per-key surface is spelled with two dashes.
- **`--config` is consumed before resolution and nothing else is.** The file is an input to resolution rather
  than a key resolution can already have read. Its mechanical twin `--config_path` selects the file *and*
  stays a resolved row, so `trouble config explain` shows that a flag — not the file — chose the path.
- **Five keys cannot be set from argv.** Three of them are the tables, whose value is a declaration rather
  than a scalar: `projects` (§3.1a), `issues` and `skills` (§3.1b) — a scalar value is refused by name with
  001. The other two are refused by the argv-secret control of §3.2 rule 2 before the daemon can serve:
  `dashboard.token_file` and `hub.token`. Their flag NAMES match the mandatory `cli_flag_secret` rule
  (SPEC-02 §3.3 rule 9) and the token that follows the name is taken as its value, so a path and a real token
  are indistinguishable to that rule. Both are set from the file or the environment
  (`TROUBLE_DASHBOARD_TOKEN_FILE`, `TROUBLE_HUB_TOKEN`), which is what the shipped unit does with
  `EnvironmentFile=`.

`internal/app`'s tests reach the flag source through `BootOptions.Args`, which is why §2.5 and this section
could disagree with `cmd/troubled` for as long as they did; the surface is pinned by the argv tests of
`cmd/troubled/main_test.go` (§7) and the two halves of the split are `splitDaemonArgs` (the daemon's own
flags) and `lifecycle.Resolve` (everything else).

## 3. Data model

### 3.1 Config resolution, provenance, redaction

Precedence: **flag > env > file > default**. Exactly one `ConfigValue` exists per key, and it names the
source that won. The `Config` struct is a single flat Go struct with `toml`/`json` tags; every key below is
real, has a default, and appears in the explain dump.

| Key | Default | Why this default |
|---|---|---|
| `state_root` | `${XDG_STATE_HOME:-~/.local/state}/trouble` | SPEC-TYPES §6.1; never `/tmp` |
| `config_path` | `${XDG_CONFIG_HOME:-~/.config}/trouble/config.toml` | user-scope install |
| `secrets.environment_file` | `${XDG_CONFIG_HOME:-~/.config}/trouble/trouble.env` | 0600 EnvironmentFile, never argv |
| `lifecycle.systemd_scope` | `user` when `XDG_RUNTIME_DIR` is set, else `system` | this fleet's units are user units (measured) |
| `lifecycle.unit_name` / `escalate_unit` / `checker_unit` | `trouble.service` / `trouble-escalate@.service` / `trouble-stall.service` | one name, config-overridable for two instances per host |
| `lifecycle.sandbox` | `standard` | §3.5 decision table |
| `lifecycle.user` | `""` (user scope: the invoking uid) | system scope: a dedicated static system user |
| `lifecycle.heartbeat_path` | `<state_root>/heartbeat.json` | 0600, top-level file |
| `lifecycle.heartbeat_interval` | `30s` | brief §1.O |
| `lifecycle.heartbeat_stale_after` | `90s` | 3× interval: two consecutive misses tolerated |
| `lifecycle.idle_heartbeat_interval` | `60s` | forces ledger seq advance on an idle host (§3.3) |
| `lifecycle.watchdog_sec` | `60s` | rendered into `WatchdogSec=`; the daemon pings at `watchdog_sec/2` |
| `lifecycle.drain_timeout` | `30s` | internal SIGTERM budget; `TimeoutStopSec=90` is the systemd backstop |
| `lifecycle.upgrade_ready_timeout` | `30s` | READY deadline after an upgrade restart |
| `lifecycle.rollback_depth` | `2` | `backups/bin/` retains this many previous binaries |
| `lifecycle.self_rss_warn` | `80MB` | RSS budget (steady ≤80MB); above it `/health.json` is `degraded` + one `lifecycle` record |
| `lifecycle.clock_skew_tolerance` | `5s` | cross-host comparison floor (SPEC-INDEX §6.5) |
| `lifecycle.stall.max_seq_age` | `300s` | the checker's stall threshold |
| `checker.interval` | `60s` | checker cadence |
| `checker.confirm_runs` | `2` | consecutive breaches before the out-of-band channel fires |
| `checker.state_file` | `<state_root>/checker.state.json` | 0600; last seq/ts seen, breach count |
| `checker.alarm_file` | `<state_root>/checker.alarm` | 0600 append-only; the daemon mirrors entries into the ledger on boot |
| `checker.alarm_command` | `[]` | operator argv array, `exec`-style, no shell |
| `ingest.bind` / `dashboard.bind` | `127.0.0.1:7643` / `127.0.0.1:7644` | brief §1.H; config-driven |
| `ingest.advertised_host` | `localhost` when the bind is loopback, else `""` (required) | DSN host must be reachable from the reporter; never a bind address |
| `ingest.auth.loopback_dsn` | `true` | pubkey-DSN alone is accepted on loopback |
| `ingest.auth.nonloopback_mode` | `token` (`token`\|`proxy`) | brief §1.P bind matrix |
| `ingest.auth.public_require_proxy` | `true` | a public bind without proxy mode is refused at preflight |
| `dashboard.auth.transport` | `cookie` (browser) with `Bearer` always accepted | token never in a URL |
| `hub.mode` | `hub` (`hub`\|`satellite`) | T1 = hub with zero satellites |
| `hub.url` / `hub.forward_project_id` / `hub.token` | `""` / `""` / from EnvironmentFile | satellite→hub target + the hub-side reserved project |
| `hub.protocol_version` | `1` | forward envelope header |
| `hub.forward_batch_records` / `hub.forward_batch_bytes` | `200` / `524288` | below the 1MB decompressed cap with headroom (SPEC-04 §2) |
| `hub.retry_base` / `hub.retry_max` | `2s` / `5m` | exponential backoff with ±20% jitter |
| `hub.dedup_lru` | `65536` | bounded in-memory idempotency-key LRU at the hub; also the degraded fallback for the light-hub Redis gate (SPEC-13 §3.4) |
| `server.profile` | `standalone` (`standalone`\|`light-hub`) | the server profile (§3.7a); `light-hub` requires `server.redis.url` + `server.duckbrain.namespace` and is refused on a satellite |
| `spool.budget_bytes` | `268435456` (256MB) | brief §1.P |
| `spool.gap_reserve_bytes` | `2097152` (2MB) | drop-oldest never touches this: loss notices must survive |
| `spool.fsync` / `spool.fsync_window_ms` | `group` / `200` | same group-commit window as the ledger |
| `verify.zone_windows` | `loopback=10m lan=15m tailnet=20m public=30m` | zone-aware windows; never below a rule's `verify_window` |
| `escalate.channels` | `[]` | ordered argv arrays; empty ⇒ install check fails with 016 |
| `escalate.timeout` | `10s` per channel | |
| `fs.forbidden_state_roots` | `/tmp`, `/var/tmp` | prefix control, §3.2 |
| `fs.remote_types` | `nfs,nfs4,cifs,smb,sshfs,fuse.sshfs` | a remote state root is refused (004) |

`ConfigValue` rows (JSON, the exact `trouble config explain --json` shape):

```json
[{"key":"ingest.bind","value":"0.0.0.0:7643","source":"file","source_ref":"/home/op/.config/trouble/config.toml:14","redacted":false},
 {"key":"ingest.bind","value":"127.0.0.1:7643","source":"flag","source_ref":"--ingest-bind","redacted":false},
 {"key":"hub.token","value":"[REDACTED:config]","source":"env","source_ref":"TROUBLE_HUB_TOKEN","redacted":true},
 {"key":"spool.budget_bytes","value":268435456,"source":"default","source_ref":"builtin","redacted":false}]
```

Rules, pinned:

- **Redaction list** = every key whose path matches `secrets.*`, `*.token`, `*.secret`, `*_key`, or
  `dsn.secret`. A redacted `ConfigValue.Value` is the literal string `"[REDACTED:config]"`; `source_ref`
  is preserved because a source reference is never a value. `*.public_key` is deliberately NOT redacted:
  a DSN public key is the only unredacted credential in the system (SPEC-02 §3).
- **No reveal mode.** No flag, env var or config key prints a secret value; a config value can only be set,
  redacted-echoed, or rejected. Tests assert the fixture secret string appears zero times in the marshalled
  dump.
- **Unknown key in the file ⇒ TROUBLE-LIFECYCLE-001**, fatal (exit 13): a typo must not silently run with a
  default. **Unknown key from the environment ⇒ ignored but recorded**, with a `config` record carrying
  `payload.hint` when the name is within edit distance 2 of a known key (a stray `TROUBLE_*` var must not
  kill the daemon, and a typo must not be silent).
- **Conflicts (two sources, different values) ⇒ TROUBLE-LIFECYCLE-002**, recorded once per boot with both
  `source_ref`s, and **never fatal**: the resolved value is the higher-precedence one. The code is class
  `permanent` because a second attempt resolves identically, and non-fatal by decision because env-vs-file
  divergence is an operator's normal world.
- **Boot snapshot**: one `config` record (kind `config`, SPEC-12-owned) is written at boot and on every
  SIGHUP reload, carrying the full redacted dump. Any key whose `source` is `env` is recorded with
  `payload.env_source=true`, so a surprise environment override is always legible in the ledger.

### 3.1a Declaring projects: the `[[projects]]` array of tables

The sentinel's project set is configuration, not an embedded default: nothing listens for an app until the
operator declares that app. Each row is one project (SPEC-04's `Project` type, SPEC-TYPES §3.6):

```toml
[[projects]]
id = "1"                                    # numeric-as-string 1..2^31-1: the DSN path element
slug = "payment-api"                        # display name; defaults to the id
public_key = "<32 lowercase hex>"            # the only unredacted credential in the system
secret = "<32 lowercase hex>"                # optional; only needed when ingest.auth.loopback_dsn = false
auth_forms = ["x_sentry_auth"]               # optional: the forms this project may use
quota_epm = 600                              # default 600 (SPEC-04 §2.2)
disk_budget_bytes = 2147483648               # default 2 GiB
loss_policy = "drop-with-counter"            # sample | drop-with-counter | spool-if-light
enabled = true                               # a declared project is enabled unless it says otherwise
```

Rules, pinned:

- **The declaration is the operator's statement** about the sentinel's project set. It reaches the sentinel
  through the composition root, which builds the server from exactly these rows.
- **A minimal declaration is a complete one.** `id` + `public_key` is enough: a zero `quota_epm` or
  `disk_budget_bytes` takes the default above, an absent `slug` is the id, and an absent `enabled` is enabled.
- **A malformed declaration is fatal** (TROUBLE-LIFECYCLE-001, refused before the bind preflight opens a
  listener): a non-numeric `id`, a `public_key` or `secret` that is not exactly 32 lowercase hex, a negative
  quota or budget, an unknown `loss_policy`, a duplicate `id` or `public_key`, or an unknown key inside a
  table. A project is never silently dropped — an ingest listener that knows no project is worse than a boot
  that refuses to start.
- **No declaration is not a malformed declaration**: with no `[[projects]]` table the sentinel is not built,
  the ingest port stays closed, the refusal is recorded and `/health.json` degrades (§3.3a). That is the
  honest state of an instance with no app pointed at it.
- **The explain dump renders it, never echoes it**: the `projects` row of `trouble config explain` and of the
  boot `config` record carries `<n> declared: <id>/<slug> public_key <hex> secret (set|none)` per project. A
  declared secret is reported as `secret (set)` and never printed, exactly like every other secret-class value
  (§3.1 redaction rule). The public key stays unredacted by design: it is a submit-only credential. The
  rendering avoids the `secret=` / `secret:` spelling deliberately — the mandatory `env_assign` and
  `kv_secret_assign` scrub rules match that shape, and the boot `config` record travels through the ledger's
  persistence-boundary re-scan, which refuses (`TROUBLE-SCRUB-008`) a record whose text still matches a
  mandatory rule.
- **Duplicate public keys are refused** rather than resolved to one project: a key that maps to two projects
  makes the ingest audit trail ambiguous.

### 3.1b Declaring the optional subsystems: the `[issues]` and `[skills]` tables

Two subsystem tables are registered config keys: `issues` (the issue desk of SPEC-09 §3.4) and `skills` (the
skill loop of SPEC-11 §2). They are the two namespaces whose keys this spec does not enumerate: every key
under `issues.` belongs to SPEC-09 §3.4, and every key under `skills.` — plus a root-level `[[signers]]`,
which SPEC-11 §2 also accepts there — belongs to SPEC-11 §2. Registration is mechanical in the §2.5 sense and
takes no per-key mapping: `[issues]` and `[skills]` are two registered keys, each resolves to one
`ConfigValue`, and the key names inside them are validated by the subsystem that owns the table.

Rules, pinned:

- **One resolved value per table.** The file's lines that declare a root table — `[issues]`,
  `[issues.drivers.github]`, `[skills]`, `[[signers]]`, or a root-level dotted key such as
  `issues.enabled = true` — are collected verbatim and resolved as ONE value, exactly as `[[projects]]`
  resolves to one `projects` row (§3.1a). The text is carried, never re-serialized: the composition root
  hands it to the subsystem's own loader, so the owning package's decoder reads the operator's own bytes and
  applies the defaults of its own §3.4/§2 table. This spec's `internal/lifecycle` imports no subsystem
  (§4.5/§8).
- **A registered table is not a prefix wildcard.** Only a dotted child of a registered table name belongs to
  it: a top-level `issues_note` is still an unknown file key (TROUBLE-LIFECYCLE-001, §3.1).
- **A scalar source cannot express a table.** `TROUBLE_ISSUES=1` or `--skills=on` is refused by name
  (`issues is a table ([issues])`), never silently ignored — the §3.1 wrong-value-type rule applied to a
  table.
- **A table is declared once.** A repeated `[issues]` header, or one reopened after its own sub-table, is
  TROUBLE-LIFECYCLE-001 naming the repeated header: the reader refuses it there rather than folding two
  declarations into one document and leaving the subsystem to report a line number from a fragment.
- **Provenance, never a value.** The row of `trouble config explain` (and of the boot `config` record when
  the value was sourced from the file) carries the winning source — `file` with the config path, or the
  `builtin` default — and its value names the declared keys (`declared: enabled, drivers.github.owner`) or
  `not declared`. It never carries a value of the table: a value in these tables can be a credential path,
  and the redaction list for those belongs to the subsystem that owns the table (SPEC-09 §3.4 prints
  `token_file` and `api_key_file` with `Redacted=true`).
- **A declaration that cannot be built is refused, loudly, as that subsystem.** The composition root resolves
  the table through the owning package's loader; a declaration the loader refuses — an unknown key inside the
  table, an enabled desk with no enabled driver, an enabled loop with neither source — leaves that subsystem
  NOT built and records the refusal with the subsystem's own code (TROUBLE-ISSUES-003 / TROUBLE-SKILLS-001)
  and the key its reason names, so `status="ok"` is unreachable for that boot (§3.3a). The compiled default is
  never substituted for a declaration that was written: that is the silent-default failure §3.1 refuses for a
  scalar key, and it does not become legal for a table.
- **No table is not a refusal.** With no `[issues]`/`[skills]` table anywhere, each subsystem keeps its
  compiled default, which ships OFF and is BUILT (SPEC-09 §3.4a / SPEC-11 §2a): a stock boot stays complete
  and reads `ok`.
- **An explicit value from the composition root wins.** A subsystem config passed in by an embedding caller
  is kept as it stands: the file is the operator's statement, the caller's own value is not overwritten by it
  — the precedence §3.1a pins for the sentinel's project set.

### 3.2 State root, secret-file modes, bind preflight

```
<state_root>/                      0700  <daemon uid>:<daemon gid>
├── ledger/                        0700
│   ├── YYYY-MM-DD.jsonl           0600   (SPEC-01 owns naming inside this dir)
│   └── YYYY-MM-DD.N.gen.jsonl     0600
├── spool/                         0700
│   ├── forward/                   0700
│   │   ├── 0000000001.fwd         0600   append-only segments of SpoolEntry
│   │   └── forward.state          0600   {ack_hub_seq, ack_local_seq, local_map(lru≤4096), dropped_total, bytes}
│   └── issues/                    0700   (SPEC-09 spool-and-replay)
├── worktrees-meta/                0700   (metadata only — NEVER a worktree base)
│   └── <tsk_id>.json              0600   {pid, worktree, task_id, branch, started_ts, state}
├── skills-local/                  0700   (candidates/ + pulls/, SPEC-11)
├── backups/                       0700
│   ├── bin/                       0700   previous binaries (rename-over rollback)
│   └── YYYY-MM-DD/                0700   0600 files: config snapshots, file.patch rollback
├── heartbeat.json                 0600   top-level (§3.3)
├── checker.state.json             0600   top-level (checker's own state)
├── checker.alarm                  0600   top-level (append-only alarm log)
└── escalate.log                   0600   top-level append-only escalation attempts
```

- **Modes are enforced, not documented.** `0700` for every directory under the root, `0600` for every file.
  A wrong mode on the state root is TROUBLE-LIFECYCLE-005; wrong mode on a declared secret-bearing file is
  TROUBLE-LIFECYCLE-013; both are fatal at boot (exit 13) because a wrong mode is silently exploitable.
- **The 0600 set** = `secrets.environment_file`, `config_path` **whenever it contains any redacted-class key**,
  `*spool*`, `*backups*`, `ledger/*.jsonl`, and the three top-level files. `CheckSecretFiles` walks the
  declared list, `os.Stat`s each, and reports every offender in one pass (never the first only).
- **`/tmp` is never a state location.** `CheckStateRoot` refuses a resolved state root or spool dir whose
  cleaned absolute path is under any `fs.forbidden_state_roots` entry, or resolves through a symlink chain
  ending there. This is a **prefix test and not a size/device test** because on the reference host `/tmp` is
  not a separate mount — it falls through to `/` with 549GB free, `rw`, no `noexec` (measured) — so a fall-through
  write would succeed and a flood would fill the root filesystem. Refusal is TROUBLE-LIFECYCLE-004.
- **Remote filesystems refused.** A state root whose `statfs` type is in `fs.remote_types` is refused with
  TROUBLE-LIFECYCLE-004: group-commit fsync durability is a local-filesystem promise.
- **Two daemons, one state root** = configuration error (004/005); the single-writer invariant is
  SPEC-01's single-writer rule and SPEC-INDEX §6.4. Two instances on one host use two state roots.

**Bind preflight** — runs before any listener serves, in this exact order:

1. `state_root` + modes (004/005) → 2. config resolve (001/002) → 3. ledger open (so the refusal is
   auditable) → 4. **bind preflight (003)** → 5. sensors → 6. `sd_notify(READY=1)`.

The preflight resolves every configured bind (`ingest.bind`, `dashboard.bind`), rejects an empty host or
port 0, rejects two listeners resolving to the same `host:port`, refuses a non-loopback `ingest.bind` with an
empty `ingest.advertised_host` (the classic silent-no-report: an unreachable DSN host), refuses a
non-private `ingest.bind` unless `ingest.auth.nonloopback_mode=proxy` or `ingest.auth.public_require_proxy=false`,
then **attempts each bind with `SO_REUSEADDR`/`SO_REUSEPORT` unset** so a live listener is detected honestly
(`EADDRINUSE`), and keeps the resulting listener fds for the servers (no close-then-reopen race). Any failure
is TROUBLE-LIFECYCLE-003 with the concrete address, port, errno, and the `ss -tlnp` line for that port as an
operator hint; the process writes one `lifecycle` `boot_refused` record and exits 13 **having served zero
HTTP responses**. `trouble config explain` prints the derived DSN + zone table so the advertised-host
mismatch is visible before a deploy.

**Secrets never in argv.** Three controls, all enforced:

1. `RenderUnits` refuses to render an `ExecStart` containing any argument that matches the SPEC-02 mandatory
   rule set (no second pattern dialect) → TROUBLE-LIFECYCLE-006; the shipped unit passes only `--config`.
2. At boot the daemon scans `/proc/self/cmdline` with the same rule set; **any hit is
   TROUBLE-LIFECYCLE-013 and the daemon refuses to start** (exit 13) because `/proc/<pid>/cmdline` is
   world-readable.
3. Every child process (`journalctl`, escalators, alarm commands) gets `exec.Cmd.Env` built from an explicit
   allowlist, so the daemon never re-exports its own secret-bearing environment.

`/proc/self/environ` is the EnvironmentFile mechanism and is not refused; on this kernel it is `0400` owned by
the process uid, so same-uid processes can read it — a residual risk that is closed by `lifecycle.user`
running the daemon as its own uid in system scope. The mode check (TROUBLE-LIFECYCLE-013) is what makes the
0600 promise real, and it is verified at every boot, not at install time only.

**The dashboard token never appears in a URL.** `dashboard.auth.transport` accepts `Authorization: Bearer` or
a `Cookie`; the dashboard listener refuses any request carrying a token-shaped query parameter with
SPEC-10 §2's token-in-URL refusal, and no config key can enable query tokens. The ingestion listener keeps
`?sentry_key=` because that is a documented Sentry SDK form (SPEC-04 §2) — the two listeners have
deliberately different dialects, and the ingest form carries a project public key, never the dashboard token.
A test renders every dashboard route with a fixture token and asserts zero occurrences in the output.

### 3.3 Heartbeat, `/health` contract, and the external stall checker

**Heartbeat file** — `<state_root>/heartbeat.json`, 0600, written every `lifecycle.heartbeat_interval` (30s)
by a goroutine that never blocks on the ledger writer, using `write <path>.tmp → fsync → rename` so a reader
never observes a partial file. Content is the `Heartbeat` type, field-for-field:

```json
{"ts":"2026-09-16T09:14:33.000Z","pid":41207,"version":"0.1.0","git_sha":"9c1f0ab","ledger_last_seq":41207,"ledger_last_ts":"2026-09-16T09:13:31.004Z","sensors":{"psi":"2026-09-16T09:14:30.001Z","journald":"2026-09-16T09:14:29.550Z","dbus-system":"2026-09-16T09:14:31.220Z","dbus-user:1000":"2026-09-16T09:14:31.221Z","disk":"2026-09-16T09:14:00.000Z","timers":"2026-09-16T09:14:00.000Z"}}
```

Staleness: `now - ts > lifecycle.heartbeat_stale_after` (90s) ⇒ TROUBLE-LIFECYCLE-008. The heartbeat is a
**secondary** signal and never the alarm by itself: a wedged ledger writer keeps heartbeating happily, which
is exactly why the primary signal is sequence advance.

**Sequence advance is made unconditional (the idle-tick rule).** On a quiet host the ledger can legitimately
produce no records for hours, which would make "seq not advancing" a false alarm. The daemon therefore writes
one `lifecycle` record with `payload:{"stage":"idle_tick"}` whenever `now - last_seq_ts >=
lifecycle.idle_heartbeat_interval` (60s). Cost: ≤1440 records/day against a measured 515k rec/s amortized
group-commit writer — free. Consequence: **any `seq` age above `lifecycle.stall.max_seq_age` (300s) is a real
stall**, and the checker can be a simple threshold test with no heuristics.

**`/health.json` contract** — `HealthResponse` field-for-field, with the producer and the checker's use:

| Field | Producer | Checker use |
|---|---|---|
| `status` `ok\|degraded\|stalled` | lifecycle assembly: `degraded` on any degraded sensor/subsystem probe or `rss > lifecycle.self_rss_warn`; `stalled` when the writer's own lag exceeds `max_seq_age` | advisory only; the checker derives from the numbers, never from this string |
| `version`, `git_sha`, `build_time` | §3.4 ldflags | logged in every alarm line (which binary was running) |
| `uptime_s` | monotonic since READY | a small uptime with a stale seq ⇒ crash-loop, not a hang |
| `ledger_last_seq` | ledger writer, monotonic per file | **primary stall test**: equals the previous run's value at a seq age above threshold ⇒ 009 |
| `ledger_last_ts` | writer | `seq_age = now - ledger_last_ts`; threshold `lifecycle.stall.max_seq_age` |
| `ledger_stall_s` | writer, computed on the monotonic clock (immune to wall-clock steps) | cross-check against `seq_age`; a divergence > `clock_skew_tolerance` is reported in the alarm `detail` |
| `sensors[]` | SPEC-03 | a sensor whose `last_success_ts` age exceeds its expectation is named in the alarm |
| `sources[]` | SPEC-05 liveness model | zones: the checker asserts at least one expected source per configured zone is alive |
| `autonomy` | SPEC-05 gates | mode + kill-switch state are logged with every alarm (alarms during `full` with the kill-switch on are expected to be quiet) |
| `breakers[]` | SPEC-03/05 | open breakers explain a legitimately quiet ledger, so the checker names them instead of guessing |
| `runtime_watermarks` | lifecycle + ledger + flow | `spool_bytes` vs `spool_budget_bytes` (>90% ⇒ alarm `detail.spool_pressure`), `rss_bytes`, `binary_bytes`, `worktrees` |
| `subsystems[]` | the composition root's live subsystem set (§3.3a) | a refused subsystem is the reason `degraded` is not an empty signal: it names the plane that is absent (ingest, issue desk, skill loop) instead of leaving an operator to infer it from a missing route |

`GET /health.json` requires the read scope (Bearer or cookie) and is served on the dashboard listener. Its
field names are frozen here; SPEC-10 owns the route row and the template that renders them.

**The external checker** (v0.1 = a config-driven external command, the fleet cron shape: the operator's cron
or the shipped `trouble-stall.timer` invokes `trouble check-stall` every `checker.interval`). It alarms on
**LEDGER-SEQUENCE STALL** (TROUBLE-LIFECYCLE-009), never on process liveness. The exact algorithm:

1. Read `<state_root>/checker.state.json` (last `seq`, last `ts`, `breach_count`).
2. `GET <health_url>` with the read token from the 0600 EnvironmentFile (Bearer; never a URL parameter).
   - **Transport/HTTP failure** → read `heartbeat.json` directly. Fresh (`< stale_after`) ⇒ alarm
     `detail.class=liveness_surface_unreadable` (the daemon lives, the dashboard does not serve — the
     operator's phone is blind); stale ⇒ alarm `detail.class=heartbeat_stale`. Both are
     TROUBLE-LIFECYCLE-008, exit 8.
3. Parse `HealthResponse`. `seq_age = now - ledger_last_ts`.
   - `seq_age > lifecycle.stall.max_seq_age` **and** `ledger_last_seq == state.seq` (unchanged since the
     previous run) ⇒ `breach_count++`, alarm class `ledger_stall`, TROUBLE-LIFECYCLE-009, exit 9.
   - Otherwise `breach_count = 0`, exit 0, nothing written.
4. Persist `{seq, ts, breach_count}` atomically. Append one line to `checker.alarm` (0600) on every breach,
   with `{ts, class, code, seq, seq_age_s, breach_count, version, git_sha}`.
5. **Out-of-band**: when `breach_count >= checker.confirm_runs` (2), run each `escalate.channels` entry (an
   argv array, `exec`-style, no shell) and, if the operator configured `checker.alarm_command`, run it too.
   A channel that cannot run at all ⇒ TROUBLE-LIFECYCLE-010 and a line in `escalate.log`.

Detection bound (normative): worst case `max_seq_age + confirm_runs × checker.interval` = 300s + 2×60s =
**≤420s from stall to out-of-band alarm**, target ≤360s; a test asserts the bound over a table of
`{stall_start, run_ts}` pairs. The checker writes **no ledger record** — the daemon is the only ledger writer,
so on its next boot the daemon reads `checker.alarm` and mirrors the entries into the ledger as `lifecycle`
records with `payload.error_code`. If the daemon never returns, the out-of-band alarm has already fired: that
is the whole point of the split.

`trouble-stall.service` is a oneshot, so a non-zero exit always enters the failed state and always fires
`OnFailure=trouble-escalate@%n.service`. That is the escalation trigger that does not depend on
`StartLimitIntervalSec=0` semantics (§6, crash-loop case).

### 3.3a Per-subsystem built/refused block (`subsystems[]`)

`/health.json` carries one row per late-landing subsystem — `sentinel`, `issues`, `research`, `flow`,
`skills`, in that order — so an instance whose ingest plane, issue desk or skill loop did not build can never
present itself as healthy. The field is `HealthResponse.Subsystems`; the row type is `SubsystemHealth`
(SPEC-TYPES §3.12):

| Field | Meaning |
|---|---|
| `name` | the subsystem: `sentinel`, `issues`, `research`, `flow`, `skills`. A row exists for every one of the five, always |
| `built` | true only while the composition root holds that subsystem's live member |
| `refused` | true when the boot recorded a refusal for it: the same event that writes the `lifecycle` record with `payload.stage="subsystem_not_built"` |
| `code` | the refusal's own `TROUBLE-*-NNN` code, when the error carries one (empty otherwise) |
| `reason` | the refusal's own detail string — a config key or an unwired driver, never a secret |

Rules, pinned:

- **Status rule.** Any row with `built=false` makes the response `degraded`, and `stalled` keeps its
  precedence over it. `status="ok"` is therefore reachable only when all five subsystems are live: an
  instance that refused a subsystem never reports a green light, whatever its sensors say. This is the
  §3.3 anti-pattern ("never observe itself into a green lie") enforced at the assembly, not left to a
  reader of the route table.
- **Never a claim it cannot prove.** `built=false` is the default and is emitted for every row the
  composition root cannot vouch for, so a health surface served before the subsystems land reports five
  unbuilt rows rather than omitting the block. An unknown subsystem state is never rendered as healthy.
- **One truth per refusal.** The rows are recorded at the same call site that writes the
  `subsystem_not_built` record, so the audit trail and the live surface cannot disagree about what the boot
  refused. Reading the ledger for the refusal and the health surface for the status must never be able to
  tell two different stories.
- **`detail` keys.** `detail.subsystem_refused` carries the code of a refused row (its reason text when the
  refusal has no code), `detail.subsystem` names that row, and `detail.subsystem_unbuilt` names a row that
  is neither built nor refused. The block is the per-subsystem authority; these single-valued keys carry the
  last refused row in block order, exactly as `detail.sensor_degraded` carries one sensor.
- **Ordering.** Rows are ordered by the build order above, never by map iteration: two boots that refused the
  same subsystems produce byte-identical blocks.

### 3.4 Version stamping

Three package-level variables in `internal/lifecycle`, set at link time, never at run time:

| Variable | ldflags (exact) | Fallback when unset |
|---|---|---|
| `Version` | `-X github.com/totalwindupflightsystems/trouble/internal/lifecycle.Version=$(git describe --tags --always --dirty)` | `0.0.0-dev` |
| `GitSHA` | `-X github.com/totalwindupflightsystems/trouble/internal/lifecycle.GitSHA=$(git rev-parse --short=7 HEAD)` | `unknown` |
| `BuildTime` | `-X github.com/totalwindupflightsystems/trouble/internal/lifecycle.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)` | `1970-01-01T00:00:00.000Z` |

Build line (Makefile `build` target, applied to **every** binary — `troubled`, `trouble`):

```
go build -trimpath -ldflags="-s -w -X .../internal/lifecycle.Version=$(VERSION) \
  -X .../internal/lifecycle.GitSHA=$(GIT_SHA) -X .../internal/lifecycle.BuildTime=$(BUILD_TIME)" ./cmd/...
```

Rules:

- **An unstamped build is visibly degraded, not silently fine.** With `GitSHA=="unknown"` the daemon sets
  `/health.json` `status="degraded"` with `detail.reason="unstamped_build"`, writes one `lifecycle` record,
  and `trouble install` refuses to enable the unit (TROUBLE-LIFECYCLE-006) unless `--force`. This is the
  fleet's own "dev (unknown)" scar converted into a startup consequence.
- **Every record's `actor` field** is populated from this triple by `lifecycle.Actor(kind, id)` at the
  injection point (§8): `{"kind":"daemon","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab",
  "build_time":"2026-09-16T09:00:00.000Z"}`. `Actor.GitSHA` on a ledger record is what makes post-incident
  forensics able to say *which binary acted*, including across an upgrade boundary in the same file.
- **The heartbeat file, `/health.json`, `trouble --version`, the unit `Description=`, and a
  `X-Trouble-Build` drop-in `Environment=`** carry the same triple, so `systemctl status` and the dashboard
  agree with the ledger.
- **Forwarding preserves `Actor` verbatim.** That is what makes the cross-host version matrix (a satellite at
  an older sha vs the hub) derivable from the hub ledger alone.

### 3.5 Unit files

`internal/lifecycle/units/trouble.service.tmpl` (rendered placeholders `__EXEC__`, `__CONFIG__`,
`__ENVFILE__`, `__MEM_HIGH__`, `__MEM_MAX__`, `__WATCHDOG__`, `__STATE_ROOT__`, `__DESC__`):

```ini
[Unit]
Description=__DESC__
Documentation=https://github.com/totalwindupflightsystems/trouble
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0
OnFailure=trouble-escalate@%n.service

[Service]
Type=notify
NotifyAccess=main
WatchdogSec=__WATCHDOG__
Restart=always
RestartSec=5
TimeoutStopSec=90
TimeoutStartSec=30
KillMode=mixed
KillSignal=SIGTERM
EnvironmentFile=-__ENVFILE__
Environment=TROUBLE_STATE_ROOT=__STATE_ROOT__
ExecStart=__EXEC__ --config __CONFIG__
ExecReload=/bin/kill -HUP $MAINPID
MemoryHigh=__MEM_HIGH__
MemoryMax=__MEM_MAX__
MemorySwapMax=0
OOMScoreAdjust=-500
LimitNOFILE=8192
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=read-only
ReadWritePaths=__STATE_ROOT__
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
```

`internal/lifecycle/units/trouble-escalate@.service.tmpl`:

```ini
[Unit]
Description=trouble escalation for %i (out-of-band; does not depend on trouble)
[Service]
Type=oneshot
TimeoutStartSec=30
EnvironmentFile=-__ENVFILE__
ExecStart=__EXEC__ escalate --unit %i --config __CONFIG__
```

**Escalation dependency rule (normative):** `trouble-escalate@.service` has no `Requires=`/`After=` on the
daemon unit, does not open the ledger, does not contact the dashboard, and does not write inside the state
root except appending `escalate.log`. It reads the failure facts from systemd
(`systemctl show %i -p ActiveState,Result,ExecMainStatus`) plus `journalctl -u %i -n 50`, then delivers via
`escalate.channels`. "Trouble is down" is precisely the case it exists for.

**Sandbox decision set** — decided here, one row per flag, with the measured fleet fact that **no fleet unit
currently uses `PrivateTmp`, `ProtectSystem` or `DynamicUser` (0 occurrences measured)**, so every one of these
is new surface for this fleet and each is proven by an install-time check plus a live test (§7):

| Flag | Decision | Reason + consequence |
|---|---|---|
| `PrivateTmp` | **yes** | It closes the world-writable-`/tmp` class for a daemon that ingests hostile payloads. The measured interaction (a worktree under `/tmp` inside a private namespace is invisible to a foreman spawned outside trouble) is defused at the source: no worktree, spool, ledger, backup or skill path may resolve under `/tmp` (the §3.2 prefix preflight refuses it), and SPEC-08's argument builder refuses a `/tmp` path for any value it passes to a spawn. `/proc/pressure`, `/proc/<pid>`, the journal socket and D-Bus are unaffected by this flag. |
| `ProtectSystem` | **`full` in the `standard` profile; `strict` opt-in with rendered `ReadWritePaths=`** | `strict` makes `/usr`, `/boot`, `/etc` read-only, which **breaks `file.patch` and `config.set` on a real `/etc` target unless that path is carved out**. Under `strict`, `trouble install` renders `ReadWritePaths=<state_root> <every sandbox.read_write_paths entry>` and cross-validates that the registry's allowed write paths ⊆ that list, failing install with TROUBLE-LIFECYCLE-006 rather than surfacing a runtime POLICY_REFUSED surprise. Worktree visibility: a worktree created by the repo-owning component at `<repo>/.worktrees/<task_id>` is readable by the daemon only when `<repo>` is inside `ReadWritePaths` (or the profile is `standard`), so `flow.hotfix.allowed_repos` roots are required entries under `strict`; the install check enforces exactly that, because a daemon that cannot read the worktree it must verify is an inert feature. |
| `ReadWritePaths` | **rendered, never hand-written**: `standard` = `{state_root}`; `strict` = `{state_root} ∪ sandbox.read_write_paths`, and `ReadWritePaths` is evaluated **after** `ProtectHome`, so the carve-out is what keeps a `$HOME`-relative state root writable. Every configured `file.patch`/`config.set` target outside the state root must be listed. |
| `ProtectHome` | **`read-only` for a user-scope install, `yes` for a system-scope install** (with the state-root carve-out either way) | A user install keeps the state root under `$HOME`; a system install has no reason to read user homes. |
| `DynamicUser` | **no** | `DynamicUser=yes` allocates an ephemeral uid per start: it breaks ownership of the 0700 state root and the 0600 secret files, breaks the worktree ownership that the repo-owning component and the spawned foreman must agree on, breaks stable ledger/backup ownership across restarts, and sets `HOME=/run/...` so a `%h`-relative state root would not resolve. Instead: user scope runs as the invoking uid; system scope runs as a **dedicated static system user** (`lifecycle.user`, e.g. `troubled`) with `StateDirectory=trouble` so the 0700/0600 promises are stable and the `DynamicUser` class of bug cannot occur. |
| `MemoryHigh=192M` / `MemoryMax=256M` | **both, soft then hard** | The 64MB `MemoryMax` + `OOMScoreAdjust=-500` pairing was a contradiction (cgroup OOM ignores the score) and 64MB is below a measured comparable daemon (35.9MB RSS idle). Soft-throttle at 192MB, hard-kill at 256MB, budget asserted as steady RSS ≤80MB / load ≤192MB (binary 8–15MB measured). |
| `MemorySwapMax=0` | **0** | A swap-thrashing health daemon is worse than a killed one, and without it `MemoryHigh` throttling does not actually bind. |
| `OOMScoreAdjust=-500` | **kept** | Host-contention protection only, paired with the hard cap so trouble cannot export its own pressure to the gateway/foremen. |
| `KillMode=mixed` + `KillSignal=SIGTERM` + `TimeoutStopSec=90` | **pinned** | SIGTERM reaches the main process only, so the daemon's own drain runs (park in-flight plays, flush the ledger group-commit, close listeners, `sd_notify STOPPING=1`); a hung daemon cannot orphan the journald child, because `TimeoutStopSec` escalates SIGKILL to the whole cgroup. Internal `lifecycle.drain_timeout=30s` is the budget; 90s is the backstop. |
| cgroup v2 | **precondition, checked** | `trouble install --check` verifies `/sys/fs/cgroup/cgroup.controllers` exists; on a cgroup-v1 host the memory directives are inert, the install reports `degraded` with `detail.reason="cgroup_v1"`, and the memory policy is advisory only. |

`sd_notify` discipline: `READY=1` after the bind preflight and the first successful ledger write;
`WATCHDOG=1` every `lifecycle.watchdog_sec/2` (30s) from a dedicated goroutine that fails to ping within the
window ⇒ TROUBLE-LIFECYCLE-007 recorded and logged before systemd's SIGABRT;
`STATUS=` carries `version seq=<n> sensors=<n>/<n>`; `STOPPING=1` on drain entry; `RELOADING=1` around a
SIGHUP config reload.

### 3.6 Upgrades: rename-over, park/resume, schema compatibility, downgrade refusal

`trouble upgrade` in order, with every step recorded as a `lifecycle` record:

1. **Stage**: place the new binary at `<bin>.new` (a fresh inode), verify `sha256` against the release
   manifest, and run `<bin>.new --self-check`: config parse (001), state-root modes (004/005),
   `schema_version` compatibility (012), unit render (006). A staged binary that cannot pass its own
   self-check never replaces a working one.
2. **Park**: ask the ladder to park in-flight plays (SPEC-05 `park`), wait for the park acknowledgement, flush
   the ledger with a final group-commit fsync, and record `{"stage":"upgrade","step":"park","parked":N}`.
   **A park that cannot be persisted aborts the upgrade** with TROUBLE-LIFECYCLE-011 and no rename: losing an
   in-flight play is worse than running an old binary for another day.
3. **Rename**: `chmod 0755 <bin>.new` then `os.Rename(<bin>.new, <bin>)`. **Never `cp` onto the live path and
   never open it for writing** — writing a running binary returns `ETXTBSY` ("Text file busy", measured trap);
   `rename()` over it is legal and leaves the running process mapped to the old inode. The previous binary is
   hardlinked into `backups/bin/<version>-<git_sha>` first, `rollback_depth` deep.
4. **Restart**: `systemctl --user restart <unit>` (or `systemctl restart` for a system install), the scope and
   the systemctl path being configuration.
5. **Resume**: the new process boots, rebuilds the ledger index, re-adopts parked plays (SPEC-05 `resume`),
   re-checks — never re-applies — an already-applied mutating call (idempotency makes a re-check safe, SPEC-06),
   records `{"stage":"upgrade","step":"resume","from_version":..,"to_version":..}`, and reaches READY.
6. **Rollback**: if READY does not arrive within `lifecycle.upgrade_ready_timeout` (30s), the unit's
   `OnFailure=` fires the escalate unit and `trouble upgrade --rollback` restores `backups/bin/<previous>` by
   the same rename recipe.

**Spawned foremen survive the restart.** A spawned foreman is a separate process (SPEC-08). `worktrees-meta/
<tsk_id>.json` records `{pid, worktree, task_id, branch, started_ts, state}`; on boot the daemon re-adopts a
spawn whose `/proc/<pid>` exists and whose worktree path is present, and otherwise writes an explicit orphan
record (`flow` kind, `payload.stage="orphan"`) instead of guessing. Ownership of the verify window therefore
survives a daemon death, which is the difference between a re-hydrated incident and an abandoned one.

**`schema_version` compatibility** (`Record.SchemaVersion`, 1 in v0.1):

- **Forward (read)**: a reader accepts any `schema_version ≤ max_supported`, ignores unknown payload keys and
  preserves them on pass-through, and never rewrites an existing record (the ledger is append-only).
- **Forward (write)**: a schema bump is additive only — new keys, no renames, no removals — so an older reader
  degrades to "fewer fields known", never to a mis-read.
- **Newer than supported**: `schema_version > max_supported` on the local ledger ⇒ SPEC-01's
  newer-schema refusal; on the forward path ⇒ TROUBLE-LIFECYCLE-014 and the satellite parks the batch (§3.7).
- **Downgrade refused**: a binary whose `max_supported` is lower than the highest `schema_version` present in
  the current ledger generation **refuses to start** with TROUBLE-LIFECYCLE-012 (exit 13) unless
  `lifecycle.migrate.downgrade_ok=true` (default `false`). Rationale: the mixing of writer generations in one
  append-only file is not recoverable by a tolerant reader — the older binary would write records that are
  permanently missing the newer required fields, and neither generation can tell which records those are.

### 3.7 Topology table, forwarding and the bounded spool

One wire format, one route, one auth form, one version header, one idempotency mechanism. Two forward
*dialects* would mean two auth dialects, two size-cap implementations and two rate-limit implementations —
the exact class of bug the judges measured in this fleet.

| Row | The ONE decision | What it buys | Config keys that implement it |
|---|---|---|---|
| **T1** single laptop/box: hub with zero satellites | Every record carries `origin{host_id, hub_id, source}` from record #1, and the satellite path is the *same code* with an empty satellite set | T1 is a configuration of the T2..T5 design, not a fork; the dashboard, ledger, dedup and outlets are identical | `hub.mode=hub`, `hub.url=""`, `ingest.bind=127.0.0.1:7643`, `ingest.auth.loopback_dsn=true`, `origin.host_id`, `origin.hub_id=""` |
| **T1→T2** remote app → public VM | **Bind-address-scoped auth**: loopback = DSN pubkey only; non-loopback = per-project token **or** a mandated reverse proxy; public = proxy mandated | A public bind cannot exist without an explicit auth decision; the DSN's public key stays a submit-only credential | `ingest.auth.loopback_dsn`, `ingest.auth.nonloopback_mode`, `ingest.auth.public_require_proxy`, `ingest.advertised_host`, `dashboard.auth.transport`, `dashboard.auth.identity_provider` (the token impl is v0.1; the seam's second impl is a named interface seam, not specified here) |
| **T2→T3** LAN hub + light satellites | **One forwarding protocol = the sentinel wire format** + version header + idempotency key | No second dialect: size caps, gzip-bomb guard, quotas, 429/`Retry-After`/`X-Sentry-Rate-Limits` and DSN auth are inherited unchanged from SPEC-04 | `hub.mode=satellite`, `hub.url`, `hub.forward_project_id`, `hub.token` (EnvironmentFile), `hub.protocol_version`, `hub.forward_batch_records`, `hub.forward_batch_bytes`, `hub.dedup_lru` |
| **T3→T4** off-LAN over a tailnet, phone dashboard | **Bounded queues + a token session model**: the spool has a hard byte budget with a defined eviction policy, and browser access is a cookie/Bearer session, never a URL token | A satellite cannot fill a disk while the hub is away, and a phone cannot leak a token through history | `spool.budget_bytes`, `spool.gap_reserve_bytes`, `spool.fsync`, `spool.fsync_window_ms`, `dashboard.auth.transport` (cookie), `dashboard.auth.session_ttl`, `verify.zone_windows` |
| **T4→T5** many machines, edge proxies, regional hubs | **Origin fields in the sig/ledger path + a proxy key class**: `origin{host_id, hub_id, source}` on every record, a reserved credential class for a forwarding proxy, and per-zone verification | Federatable incident identity (`sig` + origin hub + hub-local seq) without a data-model rewrite; a proxy is a client of the same protocol with its own key class | `origin.host_id` (stable-id derivation, not hostname), `origin.hub_id`, `ingest.auth.proxy_key_class`, `hub.proxy_trust_header`, `verify.zone_windows.<zone>`, `lifecycle.clock_skew_tolerance` |

`trouble topology` prints these as `[]TopologyDecision`; one row:

```json
{"from":"T2","to":"T3","decision":"one forwarding protocol: the sentinel envelope wire format carries forwarded records; there is no second hub/satellite dialect","config_keys":["hub.mode","hub.url","hub.forward_project_id","hub.protocol_version","spool.budget_bytes"]}
```

**The forward path (satellite → hub).** The satellite is a full daemon in `hub.mode=satellite`: it samples,
ingests and records locally, then forwards. Its local store **is** the spool: `spool/forward/NNNNNNNNNN.fwd`,
append-only segments whose lines are `SpoolEntry` values (`kind` = `forward` for a record batch, and `issue` /
`spawn` / `skill` for SPEC-09/08/11 outbound work). There is no second local store, because the hub is the only
ledger writer — one durable queue, one eviction policy, one ack.

- **Wire format**: `POST <hub.url>/api/<hub.forward_project_id>/envelope/` with
  `Content-Type: application/x-sentry-envelope`, whole-envelope gzip, DSN auth for the hub-side reserved
  project, and one envelope item of type `trouble_forward` whose payload is the batch of complete `Record`s.
  Envelope headers carry `trouble-protocol-version: 1`, `host_id`, `hub_id`, `idempotency_key`, `ack`. A hub
  that does not know the item type answers 200-and-counter (SPEC-04 §3.5's unknown-item rule), so a version
  skew degrades to a counted drop at the hub and a parked batch at the satellite — never a partial write and
  never a rejected envelope.
- **Batch sizing**: `hub.forward_batch_records` (200) and `hub.forward_batch_bytes` (512KB decompressed) keep
  every batch under the 1MB decompressed cap with headroom, so the forward path can never trip
  SPEC-04's decompressed-cap guard.
- **Idempotency key** (deterministic, three derivations, all pinned):
  `incident-scoped = sig + "|" + strconv(norm_version) + "|" + host_id`;
  `record-scoped = "rec:" + first_local_seq + "-" + last_local_seq + "|" + host_id`;
  `non-sig record = rec_id + "|0|" + host_id`.
  The v0.1 satellite sends **record-scoped** batches. The incident-scoped derivation is the in-scope
  wire capability behind AC-25's "incident forwarded up to hub"; the light-mode binary that would emit an
  incident-only forward is a v1.0 hand-off. The hub keeps a bounded LRU of seen keys
  (`hub.dedup_lru=65536`, rebuilt at boot from the ledger's origin fields): a repeat answers `200 {"id": <original rec_id>}`
  and inserts nothing; a repeat with different content is counted as a conflict and both are kept
  (the key is not content-derived for the incident form).
- **Ack-based trimming**: a 200 carries the header `X-Trouble-Ack: <hub_seq>;<local_seq_high>;<host_id>`, and
  the body stays the Sentry-shaped `{"id":"<rec_id>"}` so no SDK-facing contract changes. The satellite trims
  every sealed segment whose `to_seq <= local_seq_high` and persists `{ack_hub_seq, ack_local_seq}` in
  `forward.state`.
- **Retry/backoff**: `next_try = now + min(hub.retry_base × 2^attempts, hub.retry_max)` with ±20% jitter; a 429
  raises the delay to at least `Retry-After` (the satellite spools on 429 — it has a disk queue, which is
  precisely SPEC-04's `spool-if-light` semantics applied on the satellite side, where SDKs would discard);
  5xx/timeout/TLS → backoff; 400 or a protocol-version refusal → stop forwarding, set `/health.json`
  `status=degraded` with `detail.reason="forward_refused"`, keep the spool and retry at `hub.retry_max`.
- **Crash safety**: a sealed segment ends with a footer line
  `{"kind":"segment_end","records":N,"from_seq":a,"to_seq":b,"crc32":"<hex>"}`. A segment without a footer is
  open: its torn last line is truncated on read (SPEC-01's tolerant reader; the fleet jq-law) and replayed up
  to the last complete line. A sealed segment whose CRC fails is dropped wholesale and replaced by a `gap`
  record with `EstLost` taken **exactly** from the footer, not estimated. Group-commit fsync with
  `spool.fsync_window_ms=200` matches the ledger, so the stated crash-loss window is the same ≤200ms on both
  sides; `spool.fsync=never` is refused unless `fs.allow_no_fsync=true` and then `/health.json` is `degraded`.
- **Bounded queue, drop-oldest-with-ledger-note**: when a write would exceed `spool.budget_bytes` (256MB), the
  **oldest sealed segment is deleted entire** (never a partial segment — that would break framing) and a `gap`
  record is queued with `cause="spool_overflow"`, `FromTS`/`ToTS` from the segment header and `EstLost` from
  its footer. `spool.gap_reserve_bytes` (2MB) is excluded from the budget so the loss notices themselves
  cannot be evicted: loss must stay visible. Each eviction records TROUBLE-LIFECYCLE-015.
  Capacity arithmetic for the operator: a ledger line is ~1KB raw / ~350B gzip (measured), so 256MB of
  compressed spool is ≈730k records ≈63 days at 8 events/min.
- **The hub mints canonical ids**: the hub assigns `Record.RecID` (`ev_`+ULID), sets
  `Origin.HubID` to its own host_id, preserves `Origin.HostID`, `Origin.Source`, `Sig`, `Actor` and
  `payload` verbatim, and stamps `payload.local_rec_id` + `payload.local_seq` (set by the satellite) so the
  local↔hub mapping is **derivable from the canonical ledger alone**. The satellite's own mapping in
  `forward.state.local_map` (bounded LRU 4096) is therefore a cache, not state: losing it costs a dashboard
  link, not correctness. The hub also records a `gap` with `cause="satellite_seq_gap"` whenever a batch's
  `from_seq > last_local_seq + 1` for that host — which is how a drop-oldest eviction that raced an ack still
  becomes visible in the canonical ledger.
- **Zone-aware verification windows**: `ZoneOf(bind, source, authForm)` returns
  `loopback | lan | tailnet | public`, recorded in `Evidence.Zone` and `SourceLiveness.Zone` (SPEC-05). The
  window is `max(rule.verify_window, verify.zone_windows[zone])` — a zone never *shrinks* a rule's window, and
  per-source liveness age (`SourceLiveness.MaxAgeS`) is scaled by the same zone factor. Multi-host
  verification states WHICH sources in WHICH zones must be quiet (`SourcesExpected`); a source that is
  expected-but-missing makes the result `invalid`, never `passed` (SPEC-05, evidence-tuple verification).
- **Clock skew**: cross-host comparisons are wall-clock RFC3339 UTC; the hub measures skew against the
  satellite as `|record.ts - hub_receive_ts|` for a freshly forwarded batch. Skew beyond
  `lifecycle.clock_skew_tolerance` (5s) records TROUBLE-LIFECYCLE-017 and marks verification `invalid` for the
  affected zone (SPEC-INDEX §6.5); all local windows (cooldowns, leases, stabilization, verify) use the
  monotonic clock and are immune to wall-clock steps.
- **v1.0 hand-off, stated once and only here**: the light-mode offload binary and the sentinel proxy binary are
  **v1.0 hand-offs**, and the ONLY in-scope trace of either is this forward/spool path — the protocol, the
  version header, the idempotency key, the spool segments, the ack and the idempotent hub intake are specified
  here because the v0.1 full daemon uses every one of them satellite-side. No numbered section of this suite
  defines a separate light or proxy process, and the proxy's per-tenant spool isolation is not specified.

### 3.7a Server profiles in the topology table: standalone | light-hub (SPEC-13)

The forward path above is profile-independent — a satellite spools and relays identically in both profiles.
What a profile decides is how a **hub** absorbs load and where its closed generations live. The keys and
semantics belong to SPEC-13; this section fixes their place inside the lifecycle model so config
resolution, provenance, `/health.json` and the topology table stay one system rather than two.

| Aspect | `standalone` (default) | `light-hub` |
|---|---|---|
| `[server] profile` | `"standalone"` | `"light-hub"` |
| Ingestion plumbing | in-process path: scrub → sig → ledger `Append` | Redis stream + consumer group → ledger `Append` (SPEC-13 §3.3) |
| Dedup gate | bounded in-memory LRU (`hub.dedup_lru` 65536) + record-scoped idempotency | Redis `SET NX` on the same idempotency key, 24h TTL, with that LRU as the degraded fallback (SPEC-13 §3.4) |
| Archival tier | none — local generations until retention | closed generations exported to the DuckBrain namespace, then droppable (SPEC-13 §3.5) |
| External dependencies | zero | Redis (queue + dedup gate), DuckBrain (archival) |
| `/health.json` | no `hub` stanza (`HubStatus.Enabled=false`) | `hub` stanza present; `status=degraded` + `detail.reason` when the queue or the archive tier is down |
| Config keys | `server.profile` | `server.profile` + `server.redis.*` (url, stream, group, maxlen, dedup_ttl, require_redis) + `server.duckbrain.*` (namespace, endpoint, archive_interval, keep_local_generations) |

Five rules keep the two profiles one system:

1. **The profile resolves like any other key** (`flag > env > file > default`, §3.1) and appears in the boot
   `config` record with its provenance. There is no profile-specific config file and no second resolution
   path.
2. **Validation is a boot gate, not a runtime discovery**: `light-hub` without `server.redis.url` or
   `server.duckbrain.namespace` refuses to start (TROUBLE-HUB-001, exit 13), and `hub.mode=satellite`
   refuses `light-hub` outright — a satellite's durable queue is its spool, and a second queue would be a
   second truth.
3. **A live profile switch is refused** (TROUBLE-HUB-013): SIGHUP reloads other keys and the pending switch
   is reported in `/health.json` until a restart. Transport plumbing is not hot-swappable, and stating that
   is cheaper than pretending otherwise.
4. **Nothing else in the topology table moves.** The T1..T5 row decisions (bind-scoped auth, one forwarding
   protocol, bounded queues and token sessions, origin fields + proxy key class) are unchanged: a
   `light-hub` hub is still the T3/T4/T5 hub, with a faster intake and an archival tier.
5. **The watchdog chain is untouched** (§3.3, §3.5): readiness, heartbeat and the stall alarm never depend
   on Redis or DuckBrain, because a hub whose liveness signal lives in an external service cannot report
   that service's outage.

`trouble topology` gains one row for the profile — a `TopologyDecision` naming `server.profile` and its
required keys — so the decision is printed beside the T-level decisions instead of being discovered in a
config file. The profile keys are also the reason `/health.json` carries one optional `hub` stanza: the
one-health-surface rule of §2.2 forbids a second endpoint, so the profile reports through the existing one.

## 4. Wiring

### 4.1 Boot sequence (each step gated on the previous)

```
argv/env/file/default resolve (001/002) → state root + modes (004/005) → secret-file modes (013)
→ /proc/self/cmdline secret scan (013) → schema_version compat (012) → ledger open + index rebuild (008)
→ config record written → bind preflight, listeners held (003) → sensors start → forwarded-batch replay
→ checker.alarm mirror into the ledger → sd_notify READY=1 → serve
```

No step after `bind preflight` runs if the preflight refuses; the only artifact written in that case is the
`lifecycle` `boot_refused` record.

### 4.2 Shutdown / drain (SIGTERM)

```
SIGTERM → sd_notify STOPPING=1 + STATUS=parking → stop accepting new work → ladder park in-flight plays
→ final ledger group-commit flush (+fsync) → spool group-commit flush → final heartbeat with stage="shutdown"
→ close listeners → journald child reaped → exit 0
```
Budget: `lifecycle.drain_timeout` (30s); a drain that overruns is SIGKILLed at `TimeoutStopSec=90` and the
next boot recovers from the ledger + spool. A SIGKILLed drain is a supported path, not a lossy one:
park state is in the ledger, and an applied mutating call is never re-applied on resume (SPEC-06).

### 4.3 The watchdog chain (four independent links, none inside the daemon)

| Link | Signal | Fails when | Alarm path |
|---|---|---|---|
| 1. systemd watchdog | `WatchdogSec=60`, ping every 30s | the process hangs without pinging | SIGABRT + restart; `OnFailure=` fires for the failed state; TROUBLE-LIFECYCLE-007 recorded by the daemon before the abort if it can still write |
| 2. heartbeat file | 30s atomic replace, stale after 90s | the daemon dies, or (never) a blocking heartbeat goroutine | read by the checker directly (link 4's file fallback) → TROUBLE-LIFECYCLE-008 |
| 3. ledger sequence | idle tick every ≤60s makes advance unconditional | a wedged ledger writer, a wedged spool, a dead process | the checker's **primary** test → TROUBLE-LIFECYCLE-009 |
| 4. external checker | `trouble-stall.service`/timer every `checker.interval`, exit 9/8 | any of the above | `checker.alarm` + `escalate.channels` + `OnFailure=trouble-escalate@%n.service` — out-of-band, by construction outside trouble |

`trouble-escalate@.service` is the terminal link: independent of the daemon, the ledger, the dashboard, and
the state root's write path (it appends `escalate.log` only).

### 4.4 Forwarding wiring

```
sensors/sentinel (satellite) → scrub (SPEC-02, mandatory; refuse to persist without it) → SpoolEntry append
→ segment seal (footer+CRC) → ForwardLoop: reader batches (≤200 recs / ≤512KB) → gzip envelope
→ POST /api/<hub.forward_project_id>/envelope/ with version + idempotency headers
→ hub: dedup LRU → mint ev_ ids → hub ledger append (single writer) → payload.local_* stamped
→ 200 + X-Trouble-Ack → segment trim → forward.state ack persisted
```

Failure branches: 429 → Retry-After-honoring backoff; 5xx/timeout → exponential backoff; 400/014 → stop and
degrade; budget exceeded → drop-oldest + gap + 015.

### 4.5 Dependency direction

`internal/lifecycle` imports `internal/types` (all shared types), `internal/ledger` (read `last_seq`, write
records through the injected writer), `internal/scrub` (the arg/env/cmdline scans reuse the mandatory rule
set), and stdlib only. It is imported by `cmd/troubled`, `cmd/trouble`, `internal/dashboard` (health
assembly) and the app wiring. **No subsystem imports it**, and `internal/ledger` does not import it — the
`Actor` triple is injected (constructor argument) precisely to keep the graph acyclic.

## 5. Errors

All codes are in SPEC-TYPES §5 and inside the TROUBLE-LIFECYCLE-001..017 range. Three of them (003, 004, 005)
are used as umbrella codes for their lifecycle stage with the enumerated conditions below; no new code is
minted and the range is unchanged.

| Code | Class | Trigger (enumerated) | Behaviour | Operator remedy |
|---|---|---|---|---|
| TROUBLE-LIFECYCLE-001 | permanent | config file unparsable, unknown key, or wrong value type | exit 13, nothing served | fix `config_path`; `trouble config explain` names the key |
| TROUBLE-LIFECYCLE-002 | permanent | same key resolved from two sources with different values | recorded once per boot with both `source_ref`s, **not fatal**; higher precedence wins | inspect the record; remove the losing source |
| TROUBLE-LIFECYCLE-003 | permanent | bind preflight refused: address in use, unroutable/empty bind, duplicate listener pair, non-loopback bind with empty `ingest.advertised_host`, public bind without proxy mode | exit 13 before serving; one `boot_refused` record; errno + `ss -tlnp` hint | fix the bind or the auth mode |
| TROUBLE-LIFECYCLE-004 | permanent | state root missing/unwritable/not owned/on a remote fs/resolving under a forbidden root (`/tmp`, `/var/tmp`), or spool dir likewise | exit 13 | fix the path or the mount |
| TROUBLE-LIFECYCLE-005 | permanent | state root or a state dir mode is not 0700 (or ownership is not the daemon's) | exit 13 | `trouble install --check` lists every offender |
| TROUBLE-LIFECYCLE-006 | transient | unit render/install/repair failed: non-renderable `ExecStart`, unsupported directive, unwritable unit dir, sandbox/registry path cross-validation mismatch, unstamped build without `--force` | install fails; existing units untouched | fix the reported directive or path |
| TROUBLE-LIFECYCLE-007 | transient | this process failed to ping `sd_notify WATCHDOG=1` inside `watchdog_sec` | recorded + `STATUS=` before systemd's abort; restart follows | inspect the stall that blocked the ping goroutine |
| TROUBLE-LIFECYCLE-008 | transient | heartbeat file stale beyond `heartbeat_stale_after`, or the daemon's liveness surface is unreadable (`/health.json` unreachable while the heartbeat is fresh) | checker exit 8, alarm line with `detail.class` | distinguish dashboard-wedged from daemon-dead via `detail.class` |
| TROUBLE-LIFECYCLE-009 | permanent | ledger sequence stall: `seq_age > max_seq_age` with an unchanged `ledger_last_seq` across two checker runs | checker exit 9; escalation after `confirm_runs` | inspect the writer, the spool, and the disk |
| TROUBLE-LIFECYCLE-010 | transient | escalation hook failed on every configured channel (`trouble-escalate@` or the checker's channel run) | line in `escalate.log`, exit non-zero | fix at least one channel; an empty channel list is an install failure (016) |
| TROUBLE-LIFECYCLE-011 | transient | upgrade park failed, or resume is not possible from the ledger | **upgrade aborted before the rename**; old binary keeps running and keeps serving | fix the park blocker, retry |
| TROUBLE-LIFECYCLE-012 | permanent | `schema_version` in the current ledger generation exceeds this binary's `max_supported` | refuse to start, exit 13, ledger untouched | run the newer binary, or set `lifecycle.migrate.downgrade_ok=true` deliberately |
| TROUBLE-LIFECYCLE-013 | permanent | a secret-bearing file is not 0600, or secret-shaped material is present in `/proc/self/cmdline` | exit 13 (argv case: refuse to start) | `chmod 600`; move the value into the EnvironmentFile |
| TROUBLE-LIFECYCLE-014 | permanent | forward protocol version unsupported (upstream or downstream) | satellite stops forwarding, `/health.json` degraded, spool retained | align `hub.protocol_version` by upgrading one side |
| TROUBLE-LIFECYCLE-015 | transient | spool budget exceeded → oldest sealed segment deleted | `gap` record with exact `EstLost` + ledger note | raise `spool.budget_bytes` or restore the hub |
| TROUBLE-LIFECYCLE-016 | permanent | unit is missing its escalation wiring (`OnFailure=` absent, escalate unit uninstalled, or `escalate.channels` empty) | `trouble install --check` fails non-zero and names the missing piece | `trouble install --force` after configuring a channel |
| TROUBLE-LIFECYCLE-017 | transient | clock skew across hosts beyond `clock_skew_tolerance` | record + verification `invalid` for the affected zone | fix time sync on the offending host |

## 6. Edge cases

1. **Crash-loop with `StartLimitIntervalSec=0`.** Rate-limiting is deliberately disabled (a daemon that is
   restartable must always restart), which means a binary that exits immediately never enters the failed state
   on its own. Covered by link 4: the checker's `/health.json` failure plus a stale heartbeat produces
   TROUBLE-LIFECYCLE-008, and `trouble-stall.service` is a oneshot whose non-zero exit always fires
   `OnFailure=`. Two escalation triggers exist so that neither depends on the daemon's ability to report.
   `uptime_s` rising with a flat `ledger_last_seq` is the signature.
2. **`Text file busy`.** Opening a running binary for writing returns `ETXTBSY`. The upgrade recipe (stage to
   `<bin>.new`, `chmod`, `rename`) is the only path in the codebase that replaces a binary, and a regression
   test asserts that `open(live, O_WRONLY|O_TRUNC)` fails while the rename path succeeds.
3. **Upgrade while a foreman is spawned.** The foreman is a separate process and survives; state is re-adopted
   from `worktrees-meta/<tsk_id>.json` (pid + worktree path present ⇒ adopt), otherwise an explicit orphan
   record is written. The verify window continues after the restart.
4. **Upgrade availability.** A restart interrupts ingestion for the duration of process start plus ledger
   index rebuild; the SDK retries and the idempotency key dedups at the hub. Normative target: READY within
   30s (`lifecycle.upgrade_ready_timeout`), asserted in tests; a longer outage is escalated, not silent.
5. **A quiet host.** Without the idle-tick rule, "seq not advancing" would false-alarm on an idle box; with
   it, `seq` advances at least every 60s and any 300s silence is a genuine stall. This is a design
   consequence, not a tuning choice.
6. **A wedged ledger writer with a healthy heartbeat.** The heartbeat goroutine never takes the writer's lock,
   so it keeps writing while the writer is blocked: exactly the case in which process liveness lies and
   sequence advance tells the truth.
7. **Wall-clock steps.** `seq` and `ledger_stall_s` (monotonic) carry the stall decision; a wall-clock step
   shows up as a `seq_age`/`ledger_stall_s` divergence reported in the alarm `detail`, and a cross-host skew
   beyond tolerance is 017 with verification `invalid` for that zone.
8. **Hub down for days.** The spool is bounded, so the satellite degrades by evicting oldest-first and
   reporting each eviction exactly; the 2MB gap reserve guarantees the loss notices themselves survive.
9. **Eviction race against an ack.** A segment may be evicted before its ack arrives; the hub then sees
   `from_seq > last_local_seq + 1` and records `cause="satellite_seq_gap"`. Loss stays visible in the
   canonical ledger even when it happened on the satellite.
10. **Ack lost, batch re-sent.** The idempotency key makes the hub answer 200 with the original `rec_id` and
    insert nothing; the retried batch trims on the new ack.
11. **`norm_version` differs across hosts.** Two normalizations are two signature spaces and are never merged
    (SPEC-TYPES §6.3); the idempotency key carries `norm_version`, so a v1 record and a v2 record with the same
    bytes are distinct, and the dashboard shows them as related only.
12. **`/tmp` misconfiguration.** On the reference host `/tmp` is not a separate mount (falls through to `/`,
    549GB free, `rw`, no `noexec`), so a spool there would silently work and a flood would fill the root
    filesystem. The prefix preflight refuses it with 004; the private-tmp namespace is the second layer.
13. **A stray `TROUBLE_*` environment variable.** Ignored and recorded, with a near-miss hint when the name is
    within edit distance 2 of a real key; a boot-time env-sourced key is always visible in the `config` record.
14. **Two daemons, one state root.** Refused (004/005) and separately guarded by SPEC-01's single-writer check;
    a hub and a satellite on one host use two state roots and two unit names.
15. **Sandbox flag unsupported by the host's systemd.** `trouble install` verifies with
    `systemd-analyze verify` + `systemctl show`; an unknown directive fails the install with 006 and the
    offending line, rather than leaving a unit that will not load on boot.
16. **A public bind configured by accident.** `ingest.auth.public_require_proxy=true` refuses at preflight
    (003) unless proxy mode is explicit — the bind matrix is enforced before the socket exists, not by
    documentation.
17. **An unconfigured escalation channel.** Treated as a wiring hole (016) reported by `trouble install
    --check` with a non-zero exit, because a watchdog with no alarm channel is the ALL-GREEN class wearing a
    unit file.
18. **Dashboard token in a URL.** Refused on the dashboard listener (SPEC-10 §2) and impossible to
    enable by configuration; the ingestion listener keeps the documented `?sentry_key=` form for SDK parity.
    The two dialects are deliberate and tested separately.
19. **An upgrade that flips `server.profile`** (§3.6 rename-over + restart): the boot gate of §3.7a re-runs
    before the listener binds, so the upgraded daemon either comes up clean or comes up refused-with-a-code
    (TROUBLE-HUB-001/003) — the upgrade path never serves a profile it did not validate. Parked plays resume
    identically in both profiles, because the profile changes transport plumbing and nothing else
    (SPEC-13 §1.2).
20. **A satellite whose hub is upgraded into `light-hub`.** The satellite is untouched: it keeps spooling and
    relaying against the same route and the same protocol version, and the hub's Redis hop is invisible to
    it. This is the profile's whole point — a hub-side plumbing change is not a fleet-wide upgrade.
21. **A `light-hub` hub restored from a host backup** (SPEC-12 §3.6): `hub/archive/markers.jsonl` travels
    with the state root, so a restored host knows what was exported and never drops a generation whose
    archive it cannot prove (TROUBLE-HUB-014). Redis is not backed up and does not need to be: an empty
    stream plus the satellite's un-acked spool is the correct post-restore state.

## 7. Testing

Files and pass thresholds (all numbers normative regressions):

| Test file | Cases | Threshold |
|---|---|---|
| `internal/lifecycle/config_test.go` | 4×4 precedence matrix over 3 keys (flag/env/file/default) — each resolves to the expected value **and** `source`/`source_ref`; conflict ⇒ 002 with both refs and a successful start; unknown file key ⇒ 001; unknown env key ⇒ ignored + hint record; type error ⇒ 001 | 100% row coverage; zero secrets in the marshalled dump (fixture `sk_live_fixture_0001` count = 0) |
| `cmd/troubled/main_test.go` | the §2.5a argv surface: the daemon's own flags in both dash spellings, a key flag forwarded verbatim, an unknown key refused by name (001/exit 13), a one-dash token refused (exit 2), the file→flag precedence with the losing file recorded (002), and the surface INVENTORY — every registered key driven through `--<spelling>` with the five non-addressable keys asserted by name and reason (three tables, two refused by the argv scan); a secret-shaped flag value driven through a real `/proc/self/cmdline`; the pre-fix `flag.FlagSet` kept as the control that `--state_root` must not die in | every registered key either resolves with `source=flag` + `source_ref=--<spelling>` or is in the named exempt set (0 silent skips); `--state_root <dir>` reaches the state-root gate (004/exit 13) instead of the flag package (exit 2); a secret-shaped flag value still exits 13 with 013, and a control value passes the same scan |
| `internal/lifecycle/subsystem_test.go` | the §3.1b tables as registered keys: a documented `[issues]`/`[skills]` opt-in resolves, carries the table text verbatim and the declared key names, and produces ONE `ConfigValue` per table (no per-sub-key row); a root-level dotted key declares the table; an absent table resolves to the `builtin` default `not declared`; the file-sourced row keeps `file` provenance and carries no value of the table (a declared `api_key_file` path appears zero times in the marshalled dump); a top-level typo, an unregistered sibling table and a repeated table are 001 (the last naming the repeated header); a scalar source (`TROUBLE_ISSUES`, `--skills`) is refused by name | every case asserted; 100 % of the table's declared keys named in the row; 0 values of a table anywhere in the explain dump |
| `internal/app/subsystems_config_test.go` | the composition root's file→subsystem path: a valid `[issues]` opt-in builds a desk whose configured driver is the one that answers the boot probe, and a valid `[skills]` opt-in builds a loop holding the configured source; an invalid file (desk on with no enabled driver, loop on with no source) boots BOTH refused, each row carrying its own code and a reason naming the key, `status` not `ok`, one `subsystem_not_built` record per row; an unknown key inside a table is refused by name; the shipped example with its own documented opt-in applied to its own bytes boots `ok` | 12 table-driven resolver rows + 3 boots; 0 subsystems built from a refused declaration; 0 silent defaults |
| `internal/lifecycle/stateroot_test.go` | 0700/0755/0700-wrong-owner matrix; forbidden-root refusal; remote-fs refusal; missing dir; read-only dir | 004/005 selected exactly; every offender reported in one pass |
| `internal/lifecycle/secretfile_test.go` | each member of the 0600 set at 0644/0600/0604; config file containing a secret-class key at 0644 | 013 per offender; class 0600 with no secret-class key is accepted |
| `internal/lifecycle/argv_test.go` | cmdline containing a mandate-shaped secret ⇒ refuse to start (013); env allowed; child-env allowlist: spawn a helper and grep its `/proc/<pid>/environ` | exit 13; child env contains 0 secret-shaped values |
| `internal/lifecycle/bind_test.go` | occupy a port then boot; duplicate `host:port`; non-loopback bind with empty advertised host; public bind without proxy; happy path | 003 + exit 13 + **0 HTTP responses served** in every refusal case |
| `internal/lifecycle/heartbeat_test.go` | fake clock over 2h ⇒ 240 writes, drift <100ms; 10k concurrent reads never observe invalid JSON; heartbeat write while the ledger writer is blocked ⇒ heartbeat still fresh | drift <100ms; 0 parse errors; blocked-writer case passes |
| `internal/lifecycle/stallcheck_test.go` | httptest `/health.json`: probe/consume advancing seq ⇒ exit 0; frozen seq with `seq_age` 400s ⇒ 009/exit 9; HTTP 500 + fresh heartbeat ⇒ 008/exit 8; HTTP 500 + stale heartbeat ⇒ 008/exit 8 with a different `detail.class` | detection bound ≤420s over a `{stall_start, run_ts}` table; `breach_count` gating at 2 |
| `internal/lifecycle/upgrade_test.go` | ETXTBSY regression (naive write fails, rename path succeeds); park failure ⇒ 011 **and the binary byte-identical to before**; 3 parked plays + 1 applied mutating call ⇒ resume re-checks, never re-applies; READY timeout ⇒ escalate | 0 re-applications; park-failure ⇒ 0 renames |
| `internal/lifecycle/schema_test.go` | v2 record with a v1 binary ⇒ 012/exit 13; tolerant read preserves unknown payload keys; downgrade refusal; forward `protocol_version=2` ⇒ 014; `schema_version>max` on the local ledger ⇒ SPEC-01's refusal | exit 13, ledger bytes unchanged (sha256 of every file compared) |
| `internal/lifecycle/forward_test.go` | httptest hub on the sentinel route: 200-record/512KB batch bounds; gzip; item type `trouble_forward`; duplicate batch ⇒ same `rec_id`, hub ledger count unchanged; 429 + `Retry-After: 3` ⇒ next attempt ≥3s; 1MB budget vs 4×256KB ⇒ oldest evicted, 1 exact `EstLost` gap, 2MB reserve intact; torn last line + bad CRC cases; `from_seq` hole ⇒ `satellite_seq_gap` | batch ≤200 recs and ≤512KB decompressed; 0 duplicate hub records; eviction `EstLost` exact (footer-derived); gap records always present |
| `internal/lifecycle/units_test.go` | render both units; `systemd-analyze verify` on a temp unit root; `systemd-analyze security` score reported; `OnFailure=` present ⇒ 016 absent/present cases; `trouble install --root <tmp>` writes only into the temp root | verify clean; `--root` writes 0 files outside the temp root |
| `tests/e2e/ac14_ac18_remote_matrix.sh` | two network namespaces with veth + a non-loopback bind: one Go Sentry-SDK poster, one `curl` JSON poster, same bug class; one project floods | one group/one incident; a 429 with `Retry-After` + `X-Sentry-Rate-Limits` carries SPEC-04's format while the other project still gets 200; `/health.json` `sources[]` shows both, zone `lan` |
| `tests/e2e/ac25_forward_spool.sh` | hub+satellite over the namespace link; kill the hub; emit 10k events; restart the hub | spool ≤ budget at all times; evictions produce 015 + exact gaps; after flush `spool_bytes` <1MB; ack trimming observed; ≤2 records of residue |
| `tests/e2e/ac26_lifecycle_visibility.sh` | autonomy=full scripted run; kill-switch flip mid-stage; `systemctl --user restart` of an unrelated gateway unit | every step has a `lifecycle`/`config` record with the acting `Actor` triple; 0 records with `actor.kind=="human"`; the gateway restart does not interrupt trouble (uptime monotonic, ledger seq continuous) |
| `tests/e2e/escalation.sh` | `kill -9` the daemon; swap in a binary that exits 1; `systemctl show trouble.service -p OnFailure` | escalation line in `escalate.log` ≤420s in both failure modes; the checker path fires without the daemon alive |
| `tests/e2e/unit_sandbox.sh` | under `strict`: write inside `ReadWritePaths` succeeds, outside fails `EROFS`; `PrivateTmp`: a file written to `/tmp` inside the unit is absent on the host; cgroup v2: `memory.max` == 268435456 | 1 assertion per flag; cgroup-v1 host reports `degraded` with `detail.reason="cgroup_v1"` |
| `internal/lifecycle/profile_test.go` | `server.profile` precedence (flag/env/file/default) with provenance; `light-hub` missing `server.redis.url` or `server.duckbrain.namespace` → TROUBLE-HUB-001 + exit 13 + **0 HTTP responses**; `hub.mode=satellite` + `light-hub` → 001; SIGHUP flip of `server.profile` → 013 with the running profile intact; the `trouble topology` profile row | every refusal happens before the first bind; a refused reload leaves the running profile unchanged |
| `tests/e2e/ac28_route_spool.sh` | hub + satellite over the namespace link with `[sentinel.routes]` configured: a `direct` class stays local (route A, satellite ledger only), a `proxy` class relays (route B, hub canonical ledger); hub killed mid-batch → spool grows, replay after restart → exactly one hub record per event; `origin.route` asserted on both sides | 0 events lost, 0 duplicates; 100 % of records carry the resolved route; the spool never exceeds its budget |
| `internal/app/readybudget_test.go` | the boot-budget derivation over synthetic load values (monotone in load, never below the base, capped at 4x, exactly the base with no host signal, a core count below 1 coerced to one core), a load sweep asserting the monotonicity and the cap as properties, and the verdict composer: the failure text, the SKIP text, the threshold just below the fence, the drain wording, and the composed message for a forced low budget | every case asserted; both texts carry the observed `load_avg` and the elapsed seconds; 0 SKIPs below 4 runnable threads per core |

Regression numbers pinned: heartbeat drift <100ms/2h · stall detection ≤420s · spool budget accuracy ±1% ·
batch ≤200 records and ≤512KB decompressed · upgrade READY ≤30s · steady RSS ≤80MB after an upgrade ·
zero duplicate forwarded records · zero secret occurrences in any explain dump or rendered dashboard route ·
AC-28: 100 % of records carry `origin.route`, 0 lost, 0 duplicated across a hub outage · AC-29: identical
incident/verify sequences in `standalone` and `light-hub` for the same stream, 0 lost records across a Redis
outage.

### 7a Host-derived test-time budgets

The boot tests in `internal/app` wait on a live boot (`daemon_test.go`, `subsystems_config_test.go`, `shipped_example_test.go`, `config_projects_test.go`). That boot measures ~22-25 s on a quiet 16-core host, so a bare wall-clock deadline turns host load into a verdict: the identical commit passes on an idle box and reds out while the same box carries fleet load. Every host-measured wait in that package is therefore derived from a measured host signal — the 1-minute load average — and a budget only ever bounds "still booting":

- **Derivation.** `base × clamp(1 + loadPerCore, 1, 4)`, with `loadPerCore = loadAvg1 / NumCPU()`. `loadAvg1` is field 0 of `/proc/loadavg`, and 0 on any read or parse error; 0 keeps the base budget and never selects the extreme-load verdict. The multiplier is the first-order descheduling model for a CPU-bound single-threaded boot sharing the box: `L` other runnable threads per core stretch it by ~`1 + L`. The slope is 1, not 1/2, because the measured case says so: at 1.23 runnable threads per core an in-suite boot needed 1.95x its quiet time (48.63 s against the 1.62x a half slope allows), which is the exact failure this policy exists to remove. The multiplier never drops below 1 (a quiet host never tightens a budget) and never exceeds 4 — a ceiling reached at three runnable threads per core, so a budget at the fence is already at its maximum.
- **Bases.** The two quiet-host deadlines the suite shipped with: 30 s for READY, 20 s for the SIGTERM drain. Both scale by the same rule.
- **A settled boot is never bounded by the budget.** The waits poll at 250 ms and return as soon as the boot settles: READY, or `RunDaemon` returning (with an error, or without READY — the last case is reported as a failure, never as success). A refusal is reported within one poll tick, so a budget can neither mask a refusal nor hide a hang behind a longer deadline.
- **An exhausted budget names its numbers.** The failure carries the observed `load_avg`, the elapsed seconds, the base and the derived budget in one message, so a reader can tell host load from a regression.
- **The extreme-load fence.** At or above four runnable threads per core the same expiry is recorded as an explicit `t.Skipf` naming the same figures: such a host cannot separate a slow boot from a descheduled one, and a red test there would misreport the host as a defect. This is the only SKIP on the boot path.
- **Falsification hook.** Two inert environment overrides exist so the fence itself can be driven: `TROUBLE_BOOT_BUDGET_MS` forces the quiet-host base (milliseconds) and `TROUBLE_BOOT_LOAD_OVERRIDE` forces the observed load. A forced 1 ms budget fails the shipped-example boot in 0.26 s carrying its load and elapsed figures; the same forced budget with a forced load of 1000 records the SKIP verdict instead.
- **Scope.** Test-only: this section adds no production code and no error code of its own (every code the boot path reports is already catalogued). The one wait deliberately left alone is the 2 s fragment-poll window in `daemon_test.go` — a negative "must not happen" window, not a boot budget.

## 8. hilo impact

**Created (greenfield `~/trouble`; no fleet repository is touched):**

| Path | Fan-out | Fan-in |
|---|---|---|
| `internal/lifecycle/config.go`, `explain.go` | `internal/types` | `cmd/trouble`, app wiring |
| `internal/lifecycle/stateroot.go`, `secretfile.go`, `bind.go` | `internal/types` | `cmd/troubled`, app wiring |
| `internal/lifecycle/heartbeat.go`, `health.go`, `stallcheck.go` | `internal/types`, `internal/ledger` (read) | `internal/dashboard`, `cmd/trouble check-stall` |
| `internal/lifecycle/version.go` | `internal/types` | every `cmd/*` and the app wiring (Actor injection) |
| `internal/lifecycle/units/*.tmpl` + `units.go` (`//go:embed`) | stdlib `text/template`, `embed` | `cmd/trouble install/upgrade` |
| `internal/lifecycle/upgrade.go`, `schema.go` | `internal/types`, `internal/ledger` | `cmd/trouble upgrade`, app wiring |
| `internal/lifecycle/forward.go`, `spool.go`, `topology.go` | `internal/types`, `internal/scrub` | app wiring (satellite mode), `cmd/trouble topology` |
| (no file here) — `internal/hub` consumes this package | `Resolve`/`Explain` for the `server.profile`, `server.redis.*` and `server.duckbrain.*` keys; `Actor` for its records; `ZoneOf` for the hub-side reserved forward project (SPEC-13 §2.3 mounts no separate config path) | app wiring only — nothing in `internal/lifecycle` imports `internal/hub`, so the profile cannot change config resolution |
| `deploy/README.md`, `examples/config.toml`, `examples/trouble.env` | — | operator docs (fleet values live only here) |

**Dependency direction:** `internal/lifecycle` → `internal/types` + `internal/ledger` + `internal/scrub`.
Nothing imports back; `internal/ledger` does **not** import lifecycle (the `Actor` triple is injected), so the
graph stays acyclic. `internal/types` remains the highest fan-in node and is untouched by this spec.

**Blast radius:** one new leaf-ish package with two consumers (`cmd/`, `internal/dashboard`), plus `deploy/`
and `examples/` artifacts. The units are installed into an operator's systemd configuration directories only
as the result of an explicit `trouble install`; every test renders into a temp unit root (`--root`), so no
host state changes during CI. No existing fleet repo, board, or DB is modified.

**Highest-risk change points for the consistency loop:** the three umbrella error meanings in §5; SPEC-01
must agree on the two top-level state files (`heartbeat.json`, `checker.state.json`) and on
`spool/forward/forward.state`; SPEC-10 owns the `/health.json` route row and `dashboard.auth.*` keys while this
spec owns their semantics; SPEC-08 must consume `sandbox.read_write_paths` for its `/tmp` refusal and
`worktrees-meta` re-adoption.
