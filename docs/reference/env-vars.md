# Environment variables (`TROUBLE_*`)

Every configuration key in the trouble registry has an environment-variable twin,
derived mechanically. A small set of names is *not* derived — those are read
directly by the subsystem that owns them and are listed separately in
[Literal exceptions](#literal-exceptions). Both halves are documented here; the
drift gate `internal/lifecycle/envdocs_doc_test.go` fails when a production
`TROUBLE_*` literal or a registry key has no row on this page.

Precedence, quoted from `deploy/README.md`:

> Precedence is flag > env > file > default.

The same sentence appears in the `trouble --help` footer ("Config precedence is
flag > env > file > default (SPEC-12 §3.1); every key has a default and
`trouble config explain` shows which source won").

---

## 1. The derivation rule

> A key becomes an environment variable by upper-casing it and replacing `.` and
> `-` with `_`, prefixed with `TROUBLE_` — `internal/lifecycle/config.go:2269`:

```go
func envName(key string) string {
	return "TROUBLE_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, ".", "_"), "-", "_"))
}
```

Worked examples:

| config key | environment variable |
| --- | --- |
| `sensors.journald.queue_bytes` | `TROUBLE_SENSORS_JOURNALD_QUEUE_BYTES` |
| `server.redis.password_env` | `TROUBLE_SERVER_REDIS_PASSWORD_ENV` (this variable names *where* the Redis password lives — see §4) |

The **flag** spelling of a key is the key with `.` replaced by `-`
(`internal/lifecycle/config.go:2272`), and `config explain` reports it as
`--<flag>` in the `source_ref` column (`config.go:2028`, `config.go:2063`):

```go
func flagName(key string) string {
	return strings.ReplaceAll(key, ".", "-")
}
```

Not every key has a registered flag: a flag only wins for keys the invoking
command actually registered on its `flag.FlagSet`. `trouble config explain`
registers `--state_root` (canonical) and `--state-root` (documented alias), and
it accepts `--key` and `--json`; it does **not** register one flag per registry
key, so `--sensors-journald-queue-bytes` is rejected as an undefined flag. Use
the environment variable for those keys, or the config file.

### How the env value is read

`Resolve` builds the reverse map `TROUBLE_X -> key` from the same registry
(`config.go:1991-1996`), coerces each value to the type of the key's default
(`coerceEnvValue`, `config.go:2310`), and stamps the row
`source = "env"`, `source_ref = TROUBLE_…` (`config.go:2004-2010`). An
environment variable therefore overrides a config-file key but is itself
overridden by a flag — exactly the quoted precedence.

---

## 2. The derived surface

156 registry keys derive 156 variable names. This is the generated list from
`trouble config explain --json` on the shipped binary (the gate in §7 re-derives
it from `registry()` and fails on any key whose row is missing):

| config key | environment variable | class |
| --- | --- | --- |
| `state_root` | `TROUBLE_STATE_ROOT` |  |
| `config_path` | `TROUBLE_CONFIG_PATH` |  |
| `secrets.environment_file` | `TROUBLE_SECRETS_ENVIRONMENT_FILE` | secret (redacted) |
| `lifecycle.systemd_scope` | `TROUBLE_LIFECYCLE_SYSTEMD_SCOPE` |  |
| `lifecycle.unit_name` | `TROUBLE_LIFECYCLE_UNIT_NAME` |  |
| `lifecycle.escalate_unit` | `TROUBLE_LIFECYCLE_ESCALATE_UNIT` |  |
| `lifecycle.checker_unit` | `TROUBLE_LIFECYCLE_CHECKER_UNIT` |  |
| `lifecycle.sandbox` | `TROUBLE_LIFECYCLE_SANDBOX` |  |
| `lifecycle.user` | `TROUBLE_LIFECYCLE_USER` |  |
| `lifecycle.heartbeat_path` | `TROUBLE_LIFECYCLE_HEARTBEAT_PATH` |  |
| `lifecycle.heartbeat_interval` | `TROUBLE_LIFECYCLE_HEARTBEAT_INTERVAL` |  |
| `lifecycle.heartbeat_stale_after` | `TROUBLE_LIFECYCLE_HEARTBEAT_STALE_AFTER` |  |
| `lifecycle.idle_heartbeat_interval` | `TROUBLE_LIFECYCLE_IDLE_HEARTBEAT_INTERVAL` |  |
| `lifecycle.watchdog_sec` | `TROUBLE_LIFECYCLE_WATCHDOG_SEC` |  |
| `lifecycle.drain_timeout` | `TROUBLE_LIFECYCLE_DRAIN_TIMEOUT` |  |
| `lifecycle.upgrade_ready_timeout` | `TROUBLE_LIFECYCLE_UPGRADE_READY_TIMEOUT` |  |
| `lifecycle.rollback_depth` | `TROUBLE_LIFECYCLE_ROLLBACK_DEPTH` |  |
| `lifecycle.self_rss_warn` | `TROUBLE_LIFECYCLE_SELF_RSS_WARN` |  |
| `lifecycle.clock_skew_tolerance` | `TROUBLE_LIFECYCLE_CLOCK_SKEW_TOLERANCE` |  |
| `stall.max_seq_age` | `TROUBLE_STALL_MAX_SEQ_AGE` |  |
| `checker.interval` | `TROUBLE_CHECKER_INTERVAL` |  |
| `checker.confirm_runs` | `TROUBLE_CHECKER_CONFIRM_RUNS` |  |
| `checker.state_file` | `TROUBLE_CHECKER_STATE_FILE` |  |
| `checker.alarm_file` | `TROUBLE_CHECKER_ALARM_FILE` |  |
| `checker.alarm_command` | `TROUBLE_CHECKER_ALARM_COMMAND` |  |
| `ingest.bind` | `TROUBLE_INGEST_BIND` |  |
| `ingest.advertised_host` | `TROUBLE_INGEST_ADVERTISED_HOST` |  |
| `ingest.auth.loopback_dsn` | `TROUBLE_INGEST_AUTH_LOOPBACK_DSN` |  |
| `ingest.auth.nonloopback_mode` | `TROUBLE_INGEST_AUTH_NONLOOPBACK_MODE` |  |
| `ingest.auth.public_require_proxy` | `TROUBLE_INGEST_AUTH_PUBLIC_REQUIRE_PROXY` |  |
| `health_url` | `TROUBLE_HEALTH_URL` |  |
| `registry.plays_dir` | `TROUBLE_REGISTRY_PLAYS_DIR` |  |
| `projects` | `TROUBLE_PROJECTS` |  |
| `issues` | `TROUBLE_ISSUES` |  |
| `skills` | `TROUBLE_SKILLS` |  |
| `llm` | `TROUBLE_LLM` |  |
| `sensors.sample_fold_window` | `TROUBLE_SENSORS_SAMPLE_FOLD_WINDOW` |  |
| `sensors.merge_window` | `TROUBLE_SENSORS_MERGE_WINDOW` |  |
| `sensors.psi.enabled` | `TROUBLE_SENSORS_PSI_ENABLED` |  |
| `sensors.psi.sample_interval` | `TROUBLE_SENSORS_PSI_SAMPLE_INTERVAL` |  |
| `sensors.psi.window` | `TROUBLE_SENSORS_PSI_WINDOW` |  |
| `sensors.journald.enabled` | `TROUBLE_SENSORS_JOURNALD_ENABLED` |  |
| `sensors.journald.units` | `TROUBLE_SENSORS_JOURNALD_UNITS` |  |
| `sensors.journald.follow_all` | `TROUBLE_SENSORS_JOURNALD_FOLLOW_ALL` |  |
| `sensors.journald.follow_all_max_entries` | `TROUBLE_SENSORS_JOURNALD_FOLLOW_ALL_MAX_ENTRIES` |  |
| `sensors.journald.queue` | `TROUBLE_SENSORS_JOURNALD_QUEUE` |  |
| `sensors.journald.queue_bytes` | `TROUBLE_SENSORS_JOURNALD_QUEUE_BYTES` |  |
| `sensors.journald.max_entry` | `TROUBLE_SENSORS_JOURNALD_MAX_ENTRY` |  |
| `sensors.journald.probe_interval` | `TROUBLE_SENSORS_JOURNALD_PROBE_INTERVAL` |  |
| `sensors.dbus.enabled` | `TROUBLE_SENSORS_DBUS_ENABLED` |  |
| `sensors.dbus.user_managers` | `TROUBLE_SENSORS_DBUS_USER_MANAGERS` |  |
| `sensors.dbus.ping_interval` | `TROUBLE_SENSORS_DBUS_PING_INTERVAL` |  |
| `sensors.dbus.reconcile_interval` | `TROUBLE_SENSORS_DBUS_RECONCILE_INTERVAL` |  |
| `sensors.dbus.oomd_probe_interval` | `TROUBLE_SENSORS_DBUS_OOMD_PROBE_INTERVAL` |  |
| `sensors.disk.enabled` | `TROUBLE_SENSORS_DISK_ENABLED` |  |
| `sensors.disk.mounts` | `TROUBLE_SENSORS_DISK_MOUNTS` |  |
| `sensors.disk.interval` | `TROUBLE_SENSORS_DISK_INTERVAL` |  |
| `sensors.timers.enabled` | `TROUBLE_SENSORS_TIMERS_ENABLED` |  |
| `sensors.timers.interval` | `TROUBLE_SENSORS_TIMERS_INTERVAL` |  |
| `sensors.inotify.enabled` | `TROUBLE_SENSORS_INOTIFY_ENABLED` |  |
| `sensors.inotify.paths` | `TROUBLE_SENSORS_INOTIFY_PATHS` |  |
| `sensors.inotify.recheck_interval` | `TROUBLE_SENSORS_INOTIFY_RECHECK_INTERVAL` |  |
| `sensors.inotify.max_depth` | `TROUBLE_SENSORS_INOTIFY_MAX_DEPTH` |  |
| `sensors.rules.dir` | `TROUBLE_SENSORS_RULES_DIR` |  |
| `sensors.rules.reload_debounce` | `TROUBLE_SENSORS_RULES_RELOAD_DEBOUNCE` |  |
| `sensors.limits.rule_per_min` | `TROUBLE_SENSORS_LIMITS_RULE_PER_MIN` |  |
| `sensors.limits.source_per_min` | `TROUBLE_SENSORS_LIMITS_SOURCE_PER_MIN` |  |
| `sensors.limits.global_per_min` | `TROUBLE_SENSORS_LIMITS_GLOBAL_PER_MIN` |  |
| `sensors.limits.incidents_per_5m` | `TROUBLE_SENSORS_LIMITS_INCIDENTS_PER_5M` |  |
| `dashboard.bind` | `TROUBLE_DASHBOARD_BIND` |  |
| `dashboard.port` | `TROUBLE_DASHBOARD_PORT` |  |
| `dashboard.mandate` | `TROUBLE_DASHBOARD_MANDATE` |  |
| `dashboard.proxy_trusted` | `TROUBLE_DASHBOARD_PROXY_TRUSTED` |  |
| `dashboard.proxy_cidrs` | `TROUBLE_DASHBOARD_PROXY_CIDRS` |  |
| `dashboard.public_origin` | `TROUBLE_DASHBOARD_PUBLIC_ORIGIN` |  |
| `dashboard.project_scope` | `TROUBLE_DASHBOARD_PROJECT_SCOPE` |  |
| `dashboard.token_file` | `TROUBLE_DASHBOARD_TOKEN_FILE` |  |
| `dashboard.poll_ms` | `TROUBLE_DASHBOARD_POLL_MS` |  |
| `dashboard.strip_poll_ms` | `TROUBLE_DASHBOARD_STRIP_POLL_MS` |  |
| `dashboard.stall_alert_s` | `TROUBLE_DASHBOARD_STALL_ALERT_S` |  |
| `dashboard.health_loopback_exempt` | `TROUBLE_DASHBOARD_HEALTH_LOOPBACK_EXEMPT` |  |
| `dashboard.read_only` | `TROUBLE_DASHBOARD_READ_ONLY` |  |
| `dashboard.allow_resume` | `TROUBLE_DASHBOARD_ALLOW_RESUME` |  |
| `dashboard.allow_full` | `TROUBLE_DASHBOARD_ALLOW_FULL` |  |
| `dashboard.page_limit` | `TROUBLE_DASHBOARD_PAGE_LIMIT` |  |
| `dashboard.max_body_bytes` | `TROUBLE_DASHBOARD_MAX_BODY_BYTES` |  |
| `dashboard.rate.read_rps` | `TROUBLE_DASHBOARD_RATE_READ_RPS` |  |
| `dashboard.rate.read_burst` | `TROUBLE_DASHBOARD_RATE_READ_BURST` |  |
| `dashboard.rate.write_rps` | `TROUBLE_DASHBOARD_RATE_WRITE_RPS` |  |
| `dashboard.rate.write_burst` | `TROUBLE_DASHBOARD_RATE_WRITE_BURST` |  |
| `dashboard.auth_fail_limit` | `TROUBLE_DASHBOARD_AUTH_FAIL_LIMIT` |  |
| `dashboard.auth_fail_window` | `TROUBLE_DASHBOARD_AUTH_FAIL_WINDOW` |  |
| `dashboard.mem_pressure_pct` | `TROUBLE_DASHBOARD_MEM_PRESSURE_PCT` |  |
| `dashboard.auth.transport` | `TROUBLE_DASHBOARD_AUTH_TRANSPORT` |  |
| `dashboard.auth.identity_provider` | `TROUBLE_DASHBOARD_AUTH_IDENTITY_PROVIDER` |  |
| `dashboard.auth.session_ttl` | `TROUBLE_DASHBOARD_AUTH_SESSION_TTL` |  |
| `hub.mode` | `TROUBLE_HUB_MODE` |  |
| `hub.url` | `TROUBLE_HUB_URL` |  |
| `hub.forward_project_id` | `TROUBLE_HUB_FORWARD_PROJECT_ID` |  |
| `hub.token` | `TROUBLE_HUB_TOKEN` | secret (redacted) |
| `hub.protocol_version` | `TROUBLE_HUB_PROTOCOL_VERSION` |  |
| `hub.forward_batch_records` | `TROUBLE_HUB_FORWARD_BATCH_RECORDS` |  |
| `hub.forward_batch_bytes` | `TROUBLE_HUB_FORWARD_BATCH_BYTES` |  |
| `hub.retry_base` | `TROUBLE_HUB_RETRY_BASE` |  |
| `hub.retry_max` | `TROUBLE_HUB_RETRY_MAX` |  |
| `hub.dedup_lru` | `TROUBLE_HUB_DEDUP_LRU` |  |
| `sentinel.routes.default` | `TROUBLE_SENTINEL_ROUTES_DEFAULT` |  |
| `sentinel.routes.per_class` | `TROUBLE_SENTINEL_ROUTES_PER_CLASS` |  |
| `sentinel.dedup_window` | `TROUBLE_SENTINEL_DEDUP_WINDOW` |  |
| `ladder.drive_interval` | `TROUBLE_LADDER_DRIVE_INTERVAL` |  |
| `server.profile` | `TROUBLE_SERVER_PROFILE` |  |
| `server.hub_id` | `TROUBLE_SERVER_HUB_ID` |  |
| `server.redis.url` | `TROUBLE_SERVER_REDIS_URL` |  |
| `server.redis.password_env` | `TROUBLE_SERVER_REDIS_PASSWORD_ENV` |  |
| `server.redis.stream` | `TROUBLE_SERVER_REDIS_STREAM` |  |
| `server.redis.group` | `TROUBLE_SERVER_REDIS_GROUP` |  |
| `server.redis.consumer` | `TROUBLE_SERVER_REDIS_CONSUMER` |  |
| `server.redis.maxlen` | `TROUBLE_SERVER_REDIS_MAXLEN` |  |
| `server.redis.dedup_prefix` | `TROUBLE_SERVER_REDIS_DEDUP_PREFIX` |  |
| `server.redis.dedup_ttl` | `TROUBLE_SERVER_REDIS_DEDUP_TTL` |  |
| `server.redis.batch_records` | `TROUBLE_SERVER_REDIS_BATCH_RECORDS` |  |
| `server.redis.batch_bytes` | `TROUBLE_SERVER_REDIS_BATCH_BYTES` |  |
| `server.redis.block_ms` | `TROUBLE_SERVER_REDIS_BLOCK_MS` |  |
| `server.redis.claim_min_idle` | `TROUBLE_SERVER_REDIS_CLAIM_MIN_IDLE` |  |
| `server.redis.dial_timeout` | `TROUBLE_SERVER_REDIS_DIAL_TIMEOUT` |  |
| `server.redis.read_timeout` | `TROUBLE_SERVER_REDIS_READ_TIMEOUT` |  |
| `server.redis.write_timeout` | `TROUBLE_SERVER_REDIS_WRITE_TIMEOUT` |  |
| `server.redis.require_redis` | `TROUBLE_SERVER_REDIS_REQUIRE_REDIS` |  |
| `server.redis.require_persistence` | `TROUBLE_SERVER_REDIS_REQUIRE_PERSISTENCE` |  |
| `server.redis.check_policy` | `TROUBLE_SERVER_REDIS_CHECK_POLICY` |  |
| `server.redis.failover_grace` | `TROUBLE_SERVER_REDIS_FAILOVER_GRACE` |  |
| `server.duckbrain.enabled` | `TROUBLE_SERVER_DUCKBRAIN_ENABLED` |  |
| `server.duckbrain.namespace` | `TROUBLE_SERVER_DUCKBRAIN_NAMESPACE` |  |
| `server.duckbrain.endpoint` | `TROUBLE_SERVER_DUCKBRAIN_ENDPOINT` |  |
| `server.duckbrain.archive_interval` | `TROUBLE_SERVER_DUCKBRAIN_ARCHIVE_INTERVAL` |  |
| `server.duckbrain.archive_batch_files` | `TROUBLE_SERVER_DUCKBRAIN_ARCHIVE_BATCH_FILES` |  |
| `server.duckbrain.keep_local_generations` | `TROUBLE_SERVER_DUCKBRAIN_KEEP_LOCAL_GENERATIONS` |  |
| `server.duckbrain.verify_after_write` | `TROUBLE_SERVER_DUCKBRAIN_VERIFY_AFTER_WRITE` |  |
| `server.duckbrain.gzip` | `TROUBLE_SERVER_DUCKBRAIN_GZIP` |  |
| `spool.budget_bytes` | `TROUBLE_SPOOL_BUDGET_BYTES` |  |
| `spool.gap_reserve_bytes` | `TROUBLE_SPOOL_GAP_RESERVE_BYTES` |  |
| `spool.fsync` | `TROUBLE_SPOOL_FSYNC` |  |
| `spool.fsync_window_ms` | `TROUBLE_SPOOL_FSYNC_WINDOW_MS` |  |
| `flow.spool_max_entries` | `TROUBLE_FLOW_SPOOL_MAX_ENTRIES` |  |
| `flow.spool_ttl` | `TROUBLE_FLOW_SPOOL_TTL` |  |
| `flow.spool_max_attempts` | `TROUBLE_FLOW_SPOOL_MAX_ATTEMPTS` |  |
| `flow.replay_every` | `TROUBLE_FLOW_REPLAY_EVERY` |  |
| `flow.replay_batch` | `TROUBLE_FLOW_REPLAY_BATCH` |  |
| `verify.zone_windows` | `TROUBLE_VERIFY_ZONE_WINDOWS` |  |
| `escalate.channels` | `TROUBLE_ESCALATE_CHANNELS` |  |
| `escalate.timeout` | `TROUBLE_ESCALATE_TIMEOUT` |  |
| `fs.forbidden_state_roots` | `TROUBLE_FS_FORBIDDEN_STATE_ROOTS` |  |
| `fs.remote_types` | `TROUBLE_FS_REMOTE_TYPES` |  |
| `origin.host_id` | `TROUBLE_ORIGIN_HOST_ID` |  |
| `origin.hub_id` | `TROUBLE_ORIGIN_HUB_ID` |  |
| `lifecycle.migrate.downgrade_ok` | `TROUBLE_LIFECYCLE_MIGRATE_DOWNGRADE_OK` |  |


Rows whose key is secret-class are redacted to `[REDACTED:config]` in
`config explain` output (`internal/lifecycle/explain.go:19`), e.g.
`secrets.environment_file` and `hub.token`.

---

## 3. Literal exceptions

These names are written into non-test Go as literals. Enumerated with:

```
grep -RhoE 'TROUBLE_[A-Z0-9_]+' --include='*.go' --exclude='*_test.go' . | sort -u
```

→ 15 names. Eleven of them are read by code and are **not** the derived twin of
any registry key; four are the derived twin of a key *and* also appear as a
literal (they are called out in the table's last column).

| variable | drives | overrides / wins over | holds a secret | read at |
| --- | --- | --- | --- | --- |
| `TROUBLE_CONFIG_PATH` | `config_path` — which TOML file `Resolve` parses | the `~/.config/trouble/config.toml` default, for the CLI's `--config` fallback and the daemon's own default | no | `cmd/trouble/main.go:416`, `internal/app/daemon.go:1076`; also the derived twin (`config_path`, `config.go:2269`) |
| `TROUBLE_DUCKBRAIN_KEY` | the DuckBrain credential for the `duckbrain` issue driver and the hub's ledger archive | the `~/.config/trouble/duckbrain.key` file (env is tried first) | **yes** — value, never a name | `internal/issues/duckbrain.go:86`, `internal/issues/config.go:120` (`APIKeyEnv` default), `internal/hub/config.go:196` (`KeyEnv` default) → read at `internal/hub/kv.go:90` |
| `TROUBLE_HOST_CALIB` | `internal/loadfence` host-calibration multiples | both measured pilots, pinning `IOMultiple` and `CPUMultiple` to one number | no | `internal/loadfence/calibrate.go:52` (const), `:145` (read) |
| `TROUBLE_HOST_LOAD_OVERRIDE` | `internal/loadfence` load average | the measured `load_avg_1m` (makes the fence falsifiable in CI) | no | `internal/loadfence/loadfence.go:15` (const), `:21` (read) |
| `TROUBLE_REDIS_PASSWORD` | the Redis password for `server.profile = light-hub` | nothing — it is the *default value* of `server.redis.password_env`, i.e. the variable name the daemon resolves at boot | **yes** — value, never a name | `internal/lifecycle/config.go:663` (`PasswordEnv` default); consumed as `cfg.Server.Redis.Password`, which "resolved from password_env at boot, never from argv" (`internal/hub/config.go:69`) |
| `TROUBLE_ROUTER_TOKEN` | the `[flow]` router dispatch credential | the `router.token_file` file (file is tried first, then env) | **yes** — value, never a name | `internal/flow/config.go:44` (`TokenEnv` default) → read at `internal/flow/router.go:176` |
| `TROUBLE_SELF_UNIT` | the journald follow scope when `[sensors.journald] units` is empty and `follow_all` is false | the `troubled.service` fallback unit name | no | `internal/sensors/journald.go:320` |
| `TROUBLE_SENTINEL_BIND` | nothing at runtime | — | no | **illustrative only**: it appears in the `ConfigValue.SourceRef` doc comment (`internal/types/spec03.go:123`, mirrored in `specs/SPEC-TYPES.md:928`) as an example of the env layer's reference form. There is no reader; the sentinel/ingest listener's real bind key is `ingest.bind` → `TROUBLE_INGEST_BIND` (row in §2). Setting `TROUBLE_SENTINEL_BIND` has no effect. |
| `TROUBLE_SENTINEL_ROUTES_PER_CLASS` | the sentinel's per-signature route mode map | the `[sentinel.routes] per_class` file entry; **not** read directly — it goes through the config ladder | no | derived twin of `sentinel.routes.per_class` (`internal/lifecycle/config.go:1699`, comment at `:1693`); no `os.Getenv` site |
| `TROUBLE_STATE_ROOT` | the state root (ledger, spool, heartbeat, checker, token store) | the config file's `state_root`; beaten by `--state_root` | no | derived twin of `state_root` (`config.go:1434`); also the `TRBL-086` home-expansion case at `config.go:1996` |
| `TROUBLE_DASHBOARD_TOKEN` | the plaintext dashboard bearer credential that `trouble dashboard token create/rotate` mints into the 0600 env file | nothing — it is an *output* surface, written by the CLI and read back by the stall checker | **yes** — value, never a name | written `cmd/trouble/main.go:362`, read back `internal/lifecycle/stallcheck.go:78-79`. Loaded into the daemon's process environment by systemd's `EnvironmentFile=-<secrets.environment_file>` (`internal/lifecycle/units/trouble.service.tmpl:19`) |
| `TROUBLE_DASHBOARD_TOKEN_FILE` | the dashboard token **store** path (`dashboard.token_file`) | the config file's `dashboard.token_file`; named in the CLI's refusal message as the env spelling of that key | no | derived twin of `dashboard.token_file` (`internal/lifecycle/config.go:1649`); referenced in the refusal at `cmd/trouble/main.go:320`; notes at `main.go:196` |
| `TROUBLE_GITHUB_TOKEN` | the GitHub issue-driver credential | the `~/.config/trouble/github.token` file (env is tried first) | **yes** — value, never a name | `internal/issues/github.go:82` (fallback), `internal/issues/config.go:96` (`TokenEnv` default) → `ResolveToken` at `internal/issues/config.go:517` |
| `TROUBLE_TEST_MEMLIMIT` | the `internal/loadfence` memory ceiling | every cgroup and rlimit reading, so a harness can falsify the fence without manufacturing a cgroup | no | `internal/loadfence/memcap.go:39` (const), `:87` (read) |
| `TROUBLE_ESCALATE_` (prefix) | the escalation channel facts handed to `trouble escalate --unit <unit>` | nothing — **written by** the daemon into the child's environment, never read from the daemon's own environment | depends on the fact (`TROUBLE_ESCALATE_<KEY>` carries the channel's values) | `internal/lifecycle/ops.go:326` (`childEnv`) |

Two notes that matter when reading the table:

* `TROUBLE_ESCALATE_` is a **prefix**, not a variable. It is the `childEnv`
  allowlist: the daemon re-exports nothing except `PATH`, `HOME`, `LANG`,
  `LC_ALL`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`, `TZ` and the
  `TROUBLE_ESCALATE_<KEY>` facts the escalation child needs (`ops.go:317-328`).
  Do not set it by hand.
* `TROUBLE_TEST_MEMLIMIT`, `TROUBLE_HOST_CALIB` and
  `TROUBLE_HOST_LOAD_OVERRIDE` are test-harness knobs. They are documented
  because the binary honours them, not because an operator should need them.

---

## 4. Secret indirection: `*_env` keys name where the value lives

A secret is never a config **value**. The config holds the *name of the
variable*, and the daemon resolves it through the environment — so the value
still comes from the process environment or a `0600` env file
(`secrets.environment_file`, loaded by systemd's `EnvironmentFile=`), never
from argv, a log line or a record.

| key that names a variable | default variable | consumed by |
| --- | --- | --- |
| `server.redis.password_env` | `TROUBLE_REDIS_PASSWORD` (`internal/lifecycle/config.go:663`) | Redis password for `server.profile = light-hub` (`internal/hub/config.go:69`) |
| `[issues.drivers.<name>] token_env` | `TROUBLE_GITHUB_TOKEN` for the `github` driver (`internal/issues/config.go:96`) | `ResolveToken` — env first, then `token_file` (`internal/issues/config.go:512-534`) |
| `[issues.drivers.<name>] api_key_env` | `TROUBLE_DUCKBRAIN_KEY` for the `duckbrain` driver (`internal/issues/config.go:120`) | `newDuckbrainDriver` (`internal/issues/duckbrain.go:84-92`) |
| `[flow.router] token_env` *(flow config, not a lifecycle registry key)* | `TROUBLE_ROUTER_TOKEN` (`internal/flow/config.go:44`) | `flow/router.go:175-177` — `token_file` first, then env |
| hub archive `key_env` *(archive config, not a lifecycle registry key)* | `TROUBLE_DUCKBRAIN_KEY` (`internal/hub/config.go:196`) | `resolveArchiveKey` (`internal/hub/kv.go:89-91`) — env first, then key file |

Read each of those rows as a **pointer**, not a credential:
`server.redis.password_env = "TROUBLE_REDIS_PASSWORD"` says "the password is in
the variable named `TROUBLE_REDIS_PASSWORD`". The registry key's own value is a
name and is therefore *not* redacted (`internal/lifecycle/profile_test.go:546-577`
pins that); the variable's value is the credential and is never printed.

---

## 5. `config explain` and the source column

```
$ trouble config explain --key sensors.journald.queue_bytes
sensors.journald.queue_bytes  12345       env      TROUBLE_SENSORS_JOURNALD_QUEUE_BYTES
$ trouble config explain --key sensors.journald.queue_bytes   # variable unset
sensors.journald.queue_bytes  33554432    default  builtin
```

* `source` is one of `flag`, `env`, `file`, `default` — the layer that won
  (`internal/types/spec03.go:122`).
* `source_ref` names the winner concretely: `--state_root`, `TROUBLE_STATE_ROOT`,
  `/etc/trouble/config.toml`, or `builtin` (`spec03.go:123`).
* `--json` emits the full `ConfigValue[]`, so a script can assert provenance
  without scraping the aligned table.

The precedence ladder, verified end to end against the built binary on a scratch
state root:

```
$ trouble --config /tmp/p.toml config explain --key state_root
state_root  .../fromfile  file     /tmp/p.toml
$ TROUBLE_STATE_ROOT=.../fromenv trouble --config /tmp/p.toml config explain --key state_root
state_root  .../fromenv   env      TROUBLE_STATE_ROOT
$ TROUBLE_STATE_ROOT=.../fromenv trouble --config /tmp/p.toml \
      config explain --state_root .../fromflag --key state_root
state_root  .../fromflag  flag     --state_root
$ trouble config explain --key state_root
state_root  /home/<user>/.local/state/trouble  default  builtin
```

---

## 6. What the binary does **not** do

* **No `config explain` row, no exit code, no value.** A stray `TROUBLE_*` name
  the registry does not know is ignored — never fatal — and when it is within edit
  distance 2 of a real variable the boot logs exactly one
  `level=WARN msg="unknown env key" name=... did_you_mean=...` line naming the
  variable it resembles, and the boot `config` record carries `payload.hint`
  (`{name, hint}` rows). What it still does **not** do: produce a row in
  `config explain` (a stray name won nothing, so it has no provenance to report),
  exit non-zero, or print the stray *value* — the value is the thing most likely
  to be a secret, so it reaches neither the log nor the record. Verified on the
  built binary: `TROUBLE_INGEST_BINDS=... troubled --state_root <scratch>` prints
  that WARN as its first line and then boots unchanged;
  `TROUBLE_NONSENSE=...` prints nothing at all.
* **Unknown file keys are different.** A key the registry does not know in the
  TOML file is fatal: `TROUBLE-LIFECYCLE-001` (`deploy/README.md:351`).
* **A `TROUBLE_*` name that is not in §2 or §3 is inert.** Only registry-derived
  names and the literals above reach behaviour.

---

## 7. Drift gate

`internal/lifecycle/envdocs_doc_test.go` (Go test, `go test ./internal/lifecycle/`):

1. walks `registry(defaults())` and fails when any key's derived `envName(key)`
   has no row in this page's §2 table;
2. scans every non-test `.go` file for `TROUBLE_[A-Z0-9_]+` literals and fails
   when a name is neither a row in §2/§3 nor the derived twin of a registry key,
   so a new literal cannot ship undocumented;
3. asserts this page still carries the derivation rule and the precedence
   quote, so the file cannot be gutted while the test stays green.
