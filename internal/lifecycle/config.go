package lifecycle

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Config is the single flat resolved config (SPEC-12 §3.1) plus the nested
// DashboardConfig that lifecycle resolves and the composition root maps onto
// dashboard.Config.
type Config struct {
	StateRoot  string `toml:"state_root"`
	ConfigPath string `toml:"config_path"`

	Secrets struct {
		EnvironmentFile string `toml:"environment_file"`
	} `toml:"secrets"`

	Lifecycle struct {
		SystemdScope          string         `toml:"systemd_scope"`
		UnitName              string         `toml:"unit_name"`
		EscalateUnit          string         `toml:"escalate_unit"`
		CheckerUnit           string         `toml:"checker_unit"`
		Sandbox               string         `toml:"sandbox"`
		User                  string         `toml:"user"`
		HeartbeatPath         string         `toml:"heartbeat_path"`
		HeartbeatInterval     types.Duration `toml:"heartbeat_interval"`
		HeartbeatStaleAfter   types.Duration `toml:"heartbeat_stale_after"`
		IdleHeartbeatInterval types.Duration `toml:"idle_heartbeat_interval"`
		WatchdogSec           types.Duration `toml:"watchdog_sec"`
		DrainTimeout          types.Duration `toml:"drain_timeout"`
		UpgradeReadyTimeout   types.Duration `toml:"upgrade_ready_timeout"`
		RollbackDepth         int            `toml:"rollback_depth"`
		SelfRSSWarn           string         `toml:"self_rss_warn"` // e.g. "80MB"
		ClockSkewTolerance    types.Duration `toml:"clock_skew_tolerance"`
	} `toml:"lifecycle"`

	Stall struct {
		MaxSeqAge types.Duration `toml:"max_seq_age"`
	} `toml:"stall"`

	Checker struct {
		Interval     types.Duration `toml:"interval"`
		ConfirmRuns  int            `toml:"confirm_runs"`
		StateFile    string         `toml:"state_file"`
		AlarmFile    string         `toml:"alarm_file"`
		AlarmCommand []string       `toml:"alarm_command"`
	} `toml:"checker"`

	Ingest struct {
		Bind           string `toml:"bind"`
		AdvertisedHost string `toml:"advertised_host"`
		Auth           struct {
			LoopbackDSN        bool   `toml:"loopback_dsn"`
			NonloopbackMode    string `toml:"nonloopback_mode"`
			PublicRequireProxy bool   `toml:"public_require_proxy"`
		} `toml:"auth"`
	} `toml:"ingest"`

	// Projects is the `[[projects]]` array-of-tables (SPEC-12 §3.1a): the
	// config surface that declares the sentinel's project set. nil means the
	// operator declared none, which is a supported configuration — the sentinel
	// refuses, records why and never opens the ingest port (§3.3a).
	Projects []ProjectConfig `toml:"projects"`

	// IssuesTable and SkillsTable are the two optional subsystem tables
	// (SPEC-12 §3.1b): the `[issues]` desk and the `[skills]` loop as the
	// operator declared them. A zero value means the file declared no such
	// table, which is a supported configuration — the subsystem then keeps its
	// compiled default (SPEC-09 §3.4a / SPEC-11 §2a).
	IssuesTable SubsystemTable `toml:"-"`
	SkillsTable SubsystemTable `toml:"-"`
	// LLMTable is the agent stage's `[llm]` table (SPEC-05 §4.3a, declared per
	// SPEC-12 §3.1c). It is registered exactly like the two subsystem tables and
	// decoded by internal/llm's own strict decoder; unlike them it is core config,
	// so a declaration that cannot be built is a file-key refusal.
	LLMTable SubsystemTable `toml:"-"`

	// Routes is the `[sentinel.routes]` surface (SPEC-04 §3.10a), registered
	// leaf by leaf (`sentinel.routes.default`, `sentinel.routes.per_class`,
	// TRBL-061) so each resolves with the ordinary precedence (flag > env >
	// file > default), carries provenance and has its own `trouble config
	// explain` row. PerClass is the sig-prefix → route-mode table consumed by
	// internal/sentinel's resolver; this package carries it as declared, and
	// the sentinel's boot validation is the only judge of the values.
	Routes struct {
		Default  string            `toml:"default"`
		PerClass map[string]string `toml:"per_class"`
	} `toml:"routes"`

	Dashboard DashboardConfig `toml:"dashboard"`

	// Sensors is the SPEC-03 §4 detection-plane surface, resolved here key by
	// key (SPEC-12 §3.1e). Every scalar key of internal/sensors' config switch
	// is a registered leaf: before that, a `sensors.*` file key was an unknown
	// file key (TROUBLE-LIFECYCLE-001), so the whole plane — PSI intervals,
	// journald units/queue, D-Bus managers, disk mounts, timers, inotify
	// paths/depth, rules dir/debounce, rate limits — was untunable from a
	// config file, unreachable from the flag surface and invisible in
	// `trouble config explain` (TRBL-036).
	//
	// The defaults below are internal/sensors' own compiled defaults, pinned on
	// both sides so a host with no `[sensors]` table runs the documented
	// posture (SPEC-03 §4, §3.8a). Validation stays the sensors package's:
	// these fields carry the resolved values, and the sensor decoder is the
	// only judge of what a value may be.
	Sensors struct {
		SampleFoldWindow types.Duration `toml:"sample_fold_window"`
		MergeWindow      types.Duration `toml:"merge_window"`
		PSI              struct {
			Enabled        bool           `toml:"enabled"`
			SampleInterval types.Duration `toml:"sample_interval"`
			Window         types.Duration `toml:"window"`
		} `toml:"psi"`
		Journald struct {
			Enabled             bool           `toml:"enabled"`
			Units               []string       `toml:"units"`
			FollowAll           bool           `toml:"follow_all"`
			FollowAllMaxEntries int            `toml:"follow_all_max_entries"`
			Queue               int            `toml:"queue"`
			QueueBytes          int            `toml:"queue_bytes"`
			MaxEntry            int            `toml:"max_entry"`
			ProbeInterval       types.Duration `toml:"probe_interval"`
		} `toml:"journald"`
		DBus struct {
			Enabled           bool           `toml:"enabled"`
			UserManagers      []string       `toml:"user_managers"`
			PingInterval      types.Duration `toml:"ping_interval"`
			ReconcileInterval types.Duration `toml:"reconcile_interval"`
			OomdProbeInterval types.Duration `toml:"oomd_probe_interval"`
		} `toml:"dbus"`
		Disk struct {
			Enabled  bool           `toml:"enabled"`
			Mounts   []string       `toml:"mounts"`
			Interval types.Duration `toml:"interval"`
		} `toml:"disk"`
		Timers struct {
			Enabled  bool           `toml:"enabled"`
			Interval types.Duration `toml:"interval"`
		} `toml:"timers"`
		Inotify struct {
			Enabled         bool           `toml:"enabled"`
			RecheckInterval types.Duration `toml:"recheck_interval"`
			MaxDepth        int            `toml:"max_depth"`
			// Paths is the ONE key of the §4 surface whose value is a
			// declaration rather than a scalar: a list of tables, one per
			// watched path, written as `[[sensors.inotify.paths]]` with
			// path/mask/recursive/max_depth/rule. It is carried here verbatim —
			// internal/sensors validates every row — and it is refused by name
			// from a scalar source (a flag or an environment variable), exactly
			// like the four registered tables (SPEC-12 §2.5a).
			Paths []map[string]any `toml:"paths"`
		} `toml:"inotify"`
		Rules struct {
			Dir            string         `toml:"dir"`
			ReloadDebounce types.Duration `toml:"reload_debounce"`
		} `toml:"rules"`
		Limits struct {
			RulePerMin     int `toml:"rule_per_min"`
			SourcePerMin   int `toml:"source_per_min"`
			GlobalPerMin   int `toml:"global_per_min"`
			IncidentsPer5m int `toml:"incidents_per_5m"`
		} `toml:"limits"`
	} `toml:"sensors"`

	// HealthURL is the health surface the stall checker and the upgrade READY
	// wait consume (SPEC-12 §2.1/§3.3). Empty means "derive it from
	// dashboard.bind".
	HealthURL string `toml:"health_url"`

	Hub struct {
		Mode                string         `toml:"mode"`
		URL                 string         `toml:"url"`
		ForwardProjectID    string         `toml:"forward_project_id"`
		Token               string         `toml:"token"`
		ProtocolVersion     int            `toml:"protocol_version"`
		ForwardBatchRecords int            `toml:"forward_batch_records"`
		ForwardBatchBytes   int            `toml:"forward_batch_bytes"`
		RetryBase           types.Duration `toml:"retry_base"`
		RetryMax            types.Duration `toml:"retry_max"`
		DedupLRU            int            `toml:"dedup_lru"`
	} `toml:"hub"`

	// Server is the SPEC-13 §2.1 `[server]` surface: the server profile
	// (`standalone` | `light-hub`) and the keys its two dependencies — Redis
	// (queue + dedup gate) and DuckBrain (archival tier) — need. Every leaf key
	// is registered individually like any other key, because SPEC-12 §3.7a
	// rule 1 pins the profile as ordinary config: no profile-specific file, no
	// second resolution path, and the registry stays the only whitelist (an
	// unknown key inside `[server]` is still TROUBLE-LIFECYCLE-001).
	//
	// The runtime half of the profile (dial, consumer group, XACK seam, dedup
	// gate, archival) lives in internal/hub (SPEC-13 §2.3) and is not part of
	// this struct: what is resolved here is what a boot can decide BEFORE a
	// connection is opened. Config.ServerProfile projects this struct onto the
	// shared types.ProfileConfig.
	Server struct {
		Profile string `toml:"profile"`
		HubID   string `toml:"hub_id"`
		Redis   struct {
			URL                string         `toml:"url"`
			PasswordEnv        string         `toml:"password_env"`
			Stream             string         `toml:"stream"`
			Group              string         `toml:"group"`
			Consumer           string         `toml:"consumer"`
			MaxLen             int64          `toml:"maxlen"`
			DedupPrefix        string         `toml:"dedup_prefix"`
			DedupTTL           types.Duration `toml:"dedup_ttl"`
			BatchRecords       int            `toml:"batch_records"`
			BatchBytes         int            `toml:"batch_bytes"`
			BlockMS            int            `toml:"block_ms"`
			ClaimMinIdle       types.Duration `toml:"claim_min_idle"`
			DialTimeout        types.Duration `toml:"dial_timeout"`
			ReadTimeout        types.Duration `toml:"read_timeout"`
			WriteTimeout       types.Duration `toml:"write_timeout"`
			RequireRedis       bool           `toml:"require_redis"`
			RequirePersistence bool           `toml:"require_persistence"`
			CheckPolicy        bool           `toml:"check_policy"`
			FailoverGrace      types.Duration `toml:"failover_grace"`
		} `toml:"redis"`
		DuckBrain struct {
			Enabled           bool           `toml:"enabled"`
			Namespace         string         `toml:"namespace"`
			Endpoint          string         `toml:"endpoint"`
			ArchiveInterval   types.Duration `toml:"archive_interval"`
			ArchiveBatchFiles int            `toml:"archive_batch_files"`
			KeepLocalGens     int            `toml:"keep_local_generations"`
			VerifyAfterWrite  bool           `toml:"verify_after_write"`
			Gzip              bool           `toml:"gzip"`
		} `toml:"duckbrain"`
	} `toml:"server"`

	Spool struct {
		BudgetBytes     int64  `toml:"budget_bytes"`
		GapReserveBytes int64  `toml:"gap_reserve_bytes"`
		Fsync           string `toml:"fsync"`
		FsyncWindowMS   int    `toml:"fsync_window_ms"`
	} `toml:"spool"`

	// Flow is the SPEC-08 §3.9a dispatch queue's bound set, resolved as five
	// ordinary registry keys (SPEC-12 §3.1d). Before these keys the queue's
	// limits were compile-time constants in `internal/flow`, so an operator
	// could neither tune them nor read them out of `trouble config explain`.
	// The defaults are exactly the constants §3.9a states, so a host that sets
	// none of them keeps the documented posture.
	Flow struct {
		SpoolMaxEntries  int            `toml:"spool_max_entries"`
		SpoolTTL         types.Duration `toml:"spool_ttl"`
		SpoolMaxAttempts int            `toml:"spool_max_attempts"`
		ReplayEvery      types.Duration `toml:"replay_every"`
		ReplayBatch      int            `toml:"replay_batch"`
	} `toml:"flow"`

	Verify struct {
		ZoneWindows map[string]types.Duration `toml:"zone_windows"`
	} `toml:"verify"`

	Escalate struct {
		Channels [][]string     `toml:"channels"`
		Timeout  types.Duration `toml:"timeout"`
	} `toml:"escalate"`

	FS struct {
		ForbiddenStateRoots []string `toml:"forbidden_state_roots"`
		RemoteTypes         []string `toml:"remote_types"`
	} `toml:"fs"`

	Origin struct {
		HostID string `toml:"host_id"`
		HubID  string `toml:"hub_id"`
	} `toml:"origin"`

	Migrate struct {
		DowngradeOK bool `toml:"downgrade_ok"`
	} `toml:"lifecycle.migrate"`
}

// DashboardConfig holds the dashboard.* keys (SPEC-10 §3.4) resolved by
// lifecycle and passed to dashboard by the composition root.
type DashboardConfig struct {
	Bind string `toml:"bind"`
	Port int    `toml:"port"`
	Auth struct {
		Transport        string         `toml:"transport"`
		IdentityProvider string         `toml:"identity_provider"`
		SessionTTL       types.Duration `toml:"session_ttl"`
	} `toml:"auth"`
	Mandate              string   `toml:"mandate"`
	ProxyTrusted         bool     `toml:"proxy_trusted"`
	ProxyCIDRs           []string `toml:"proxy_cidrs"`
	PublicOrigin         string   `toml:"public_origin"`
	ProjectScope         []string `toml:"project_scope"`
	TokenFile            string   `toml:"token_file"`
	PollMS               int      `toml:"poll_ms"`
	StripPollMS          int      `toml:"strip_poll_ms"`
	StallAlertS          int      `toml:"stall_alert_s"`
	HealthLoopbackExempt bool     `toml:"health_loopback_exempt"`
	ReadOnly             bool     `toml:"read_only"`
	AllowResume          bool     `toml:"allow_resume"`
	AllowFull            bool     `toml:"allow_full"`
	PageLimit            int      `toml:"page_limit"`
	MaxBodyBytes         int64    `toml:"max_body_bytes"`
	Rate                 struct {
		ReadRPS    float64 `toml:"read_rps"`
		ReadBurst  int     `toml:"read_burst"`
		WriteRPS   float64 `toml:"write_rps"`
		WriteBurst int     `toml:"write_burst"`
	} `toml:"rate"`
	AuthFailLimit  int            `toml:"auth_fail_limit"`
	AuthFailWindow types.Duration `toml:"auth_fail_window"`
	MemPressurePct int            `toml:"mem_pressure_pct"`
	// ReportAuthTimeoutSeconds bounds the reverse-proxy auth exchange when the
	// mandated proxy terminates TLS (SPEC-10 §2.4).
	ReportAuthTimeoutSeconds int `toml:"report_auth_timeout_s"`
}

// ProjectConfig is one row of the `[[projects]]` array-of-tables (SPEC-12
// §3.1a): the config surface that declares the sentinel's project set. Its keys
// are the part of types.Project an operator decides; the parts sentinel derives
// (AuthForms folded from the ledger, CreatedTS) are not configurable and stay
// absent here.
//
// `secret` is optional: it is required only when the operator turns the
// loopback DSN form off (ingest.auth.loopback_dsn = false, SPEC-12 §3.1).
type ProjectConfig struct {
	ID              string   `toml:"id"`                // numeric-as-string, 1..2^31-1; path element in the DSN
	Slug            string   `toml:"slug"`              // display name; defaults to the id
	PublicKey       string   `toml:"public_key"`        // 32 lowercase hex
	Secret          string   `toml:"secret"`            // 32 lowercase hex, optional
	SecretKey       string   `toml:"secret_key"`        // alias of secret (SPEC-04 §2.2 spells it secret_key)
	AuthForms       []string `toml:"auth_forms"`        // x_sentry_auth | query_sentry_key | envelope_dsn
	QuotaEPM        int      `toml:"quota_epm"`         // events per minute
	DiskBudgetBytes int64    `toml:"disk_budget_bytes"` // per-project disk budget
	LossPolicy      string   `toml:"loss_policy"`       // sample | drop-with-counter | spool-if-light
	Enabled         bool     `toml:"enabled"`           // a declared project is enabled unless it says otherwise

	// enabledSet records that the DECLARATION said something about `enabled`.
	// Only with it can a false value mean "disabled": without it, a false means
	// "not mentioned", and a declared project is enabled (the documented
	// default). The distinction is why the parser, not the zero value, decides.
	enabledSet bool
}

// Defaults a minimal `id` + `public_key` declaration inherits (SPEC-04 §2.2).
const (
	DefaultProjectQuotaEPM   = 600
	DefaultProjectDiskBudget = 2147483648 // 2GiB
)

// advertisedHostLoopbackDefault is the DSN host derived in postResolve when the
// operator declares no `ingest.advertised_host` on a loopback bind (SPEC-12
// §3.1/§3.1g). It is a value the sentinel accepts for exactly that case
// (SPEC-04 §2.3a), which is what makes the documented default usable; the
// string is named here so the projection that carries it onto the explain row
// and the derivation cannot drift apart.
const advertisedHostLoopbackDefault = "localhost"

// SubsystemTable is one optional subsystem table — `[issues]` or `[skills]` —
// exactly as the config file declared it (SPEC-12 §3.1b).
//
// It is carried as TEXT rather than as a resolved typed config on purpose: the
// keys inside these tables belong to the subsystem that owns them (SPEC-09 §3.4
// and §3.4a, SPEC-11 §2 and §2a), and this package imports no subsystem
// (SPEC-12 §4.5/§8). The composition root hands the text to the subsystem's own
// loader, which is also where a declaration that cannot be turned into a
// subsystem is refused with that subsystem's own code.
//
// A zero value means the file declared no such table — a supported
// configuration, not a hole: the subsystem keeps its compiled default, which
// ships OFF (SPEC-09 §3.4a / SPEC-11 §2a).
type SubsystemTable struct {
	// Text is the verbatim TOML text of the table, its header lines included,
	// so `[issues] ...` and `[issues.drivers.github] ...` arrive as ONE document
	// the subsystem's own TOML decoder reads.
	Text string
	// Keys are the keys the declaration carried, relative to the table root and
	// sorted (`enabled`, `drivers.github.owner`). They are what the explain row
	// reports: the names, never a value.
	Keys []string
}

// Declared reports whether the file carried the table at all.
func (t SubsystemTable) Declared() bool { return strings.TrimSpace(t.Text) != "" }

// Summary renders the table's `trouble config explain` row and its boot-record
// entry. It names the keys the declaration carried and never a value: a value in
// these tables can be a credential path, and the redaction list for those belongs
// to the subsystem that owns the table (SPEC-09 §3.4 redacts `token_file` and
// `api_key_file`; SPEC-12 §3.1 redacts by its own key rules, which do not reach
// this table's key names).
func (t SubsystemTable) Summary() string {
	if !t.Declared() {
		return "not declared"
	}
	if len(t.Keys) == 0 {
		return "declared: no keys"
	}
	return "declared: " + strings.Join(t.Keys, ", ")
}

// setSubsystemTable is a registered table key's setter. The only accepted source
// shape is the table document the file reader builds for `[issues]`/`[skills]`; a
// scalar — an env var or a flag cannot express a table — is refused BY NAME
// instead of silently leaving the compiled default in place, which is the §3.1
// rule for a wrong value type applied to a table.
func setSubsystemTable(dst *SubsystemTable, name string, v any) error {
	switch x := v.(type) {
	case nil:
		*dst = SubsystemTable{}
		return nil
	case SubsystemTable:
		*dst = x
		return nil
	case string:
		if strings.TrimSpace(x) == "" {
			*dst = SubsystemTable{}
			return nil
		}
		return fmt.Errorf("%s is a table ([%s]); %q is not a declaration", name, name, x)
	default:
		return fmt.Errorf("%s is a table ([%s]), got %T", name, name, v)
	}
}

// tableRoot reports the registered TABLE key that owns a flattened file key, if
// any (SPEC-12 §3.1b). A table key is registered with a SubsystemTable default,
// which is how it is told apart from a scalar key without a second list of names
// to keep in sync with the registry.
func tableRoot(known map[string]keyMeta, key string) (string, bool) {
	root, rest, ok := strings.Cut(key, ".")
	if !ok || root == "" || rest == "" {
		return "", false
	}
	m, ok := known[root]
	if !ok {
		return "", false
	}
	if _, isTable := m.defaultVal.(SubsystemTable); !isTable {
		return "", false
	}
	return root, true
}

// absorbTables folds every file key that lives under a registered subsystem
// table into that table's one resolved value (SPEC-12 §3.1b): the keys of
// `[issues]` and `[skills]` are the subsystems' own, so they are collected for
// the owner instead of being checked against this package's flat allowlist. The
// owner refuses an unknown one by name, with the code that names the failure.
func absorbTables(fileVals map[string]any, docs map[string]string, known map[string]keyMeta) error {
	declared := map[string][]string{}
	for k := range fileVals {
		root, ok := tableRoot(known, k)
		if !ok {
			continue
		}
		declared[root] = append(declared[root], strings.TrimPrefix(k, root+"."))
		delete(fileVals, k)
	}
	for root, keys := range declared {
		text := docs[root]
		if strings.TrimSpace(text) == "" {
			// Unreachable: a flattened key under a table root is written by a
			// line the reader attributed to that root. It is a guard rather than
			// a branch — dropping the declaration here would silently run the
			// subsystem on its compiled default, the exact failure §3.1 refuses.
			return fmt.Errorf("%w: key %q: the text of table [%s] was not captured", types.CodeLifecycle001, root, root)
		}
		sort.Strings(keys)
		fileVals[root] = SubsystemTable{Text: text, Keys: keys}
	}
	return nil
}

// Resolved is the output of Resolve: the typed config, every ConfigValue with
// provenance, plus the non-fatal records that must be mirrored into the ledger.
type Resolved struct {
	Config     Config
	Values     []types.ConfigValue
	Conflicts  []types.ConfigValue // one row per conflict (code 002): only two operator sources can disagree
	UnknownEnv []types.ConfigValue // unknown TROUBLE_* env vars with optional hint
	Warnings   []error
}

// keyMeta describes one known config key, its default and how to set it.
type keyMeta struct {
	key        string
	section    string
	name       string
	defaultVal any
	setter     func(*Config, any) error
}

func defaults() *Config {
	c := &Config{}
	c.StateRoot = defaultStateRoot()
	c.ConfigPath = defaultConfigPath()
	c.Secrets.EnvironmentFile = defaultEnvFile()
	c.Lifecycle.SystemdScope = defaultSystemdScope()
	c.Lifecycle.UnitName = "trouble.service"
	c.Lifecycle.EscalateUnit = "trouble-escalate@.service"
	c.Lifecycle.CheckerUnit = "trouble-stall.service"
	c.Lifecycle.Sandbox = "standard"
	c.Lifecycle.HeartbeatPath = ""
	c.Lifecycle.HeartbeatInterval = "30s"
	c.Lifecycle.HeartbeatStaleAfter = "90s"
	c.Lifecycle.IdleHeartbeatInterval = "60s"
	c.Lifecycle.WatchdogSec = "60s"
	c.Lifecycle.DrainTimeout = "30s"
	c.Lifecycle.UpgradeReadyTimeout = "30s"
	c.Lifecycle.RollbackDepth = 2
	c.Lifecycle.SelfRSSWarn = "80MB"
	c.Lifecycle.ClockSkewTolerance = "5s"
	c.Stall.MaxSeqAge = "300s"
	c.Checker.Interval = "60s"
	c.Checker.ConfirmRuns = 2
	c.Checker.StateFile = ""
	c.Checker.AlarmFile = ""
	c.Checker.AlarmCommand = nil
	c.Ingest.Bind = "127.0.0.1:7643"
	c.Ingest.AdvertisedHost = ""
	c.Ingest.Auth.LoopbackDSN = true
	c.Ingest.Auth.NonloopbackMode = "token"
	c.Ingest.Auth.PublicRequireProxy = true
	c.HealthURL = ""
	// SPEC-03 §3.8a: the count-preserving fold for repeated identical sampled
	// observations is ON by default, and its window defaults to the per-rule
	// cooldown default (5m) so a continuing condition persists records no faster
	// than the ladder can act on them. This default MUST match internal/sensors'
	// own compiled default — both sides pin it ("5m") so a drift is caught.
	c.Sensors.SampleFoldWindow = "5m"
	// SPEC-03 §4/§3.1e: the rest of the detection plane's defaults, copied from
	// internal/sensors' own compiled defaults (its defaultConfig()). The two
	// sides are pinned by tests in both packages, so a host that declares no
	// `[sensors]` table runs exactly the documented posture and a drift is
	// caught on whichever side moved. `sensors.rules.dir` is deliberately left
	// empty here: it is derived from the RESOLVED state root in postResolve,
	// so an operator's `state_root` moves the rules directory with it.
	c.Sensors.MergeWindow = "5s"
	c.Sensors.PSI.Enabled = true
	c.Sensors.PSI.SampleInterval = "2s"
	c.Sensors.PSI.Window = "2s"
	c.Sensors.Journald.Enabled = true
	c.Sensors.Journald.Units = []string{}
	c.Sensors.Journald.FollowAll = false
	c.Sensors.Journald.FollowAllMaxEntries = 2000000
	c.Sensors.Journald.Queue = 8192
	c.Sensors.Journald.QueueBytes = 32 << 20
	c.Sensors.Journald.MaxEntry = 65536
	c.Sensors.Journald.ProbeInterval = "30s"
	c.Sensors.DBus.Enabled = true
	c.Sensors.DBus.UserManagers = []string{"self"}
	c.Sensors.DBus.PingInterval = "30s"
	c.Sensors.DBus.ReconcileInterval = "300s"
	c.Sensors.DBus.OomdProbeInterval = "10m"
	c.Sensors.Disk.Enabled = true
	c.Sensors.Disk.Mounts = []string{"/"}
	c.Sensors.Disk.Interval = "60s"
	c.Sensors.Timers.Enabled = true
	c.Sensors.Timers.Interval = "60s"
	c.Sensors.Inotify.Enabled = true
	c.Sensors.Inotify.Paths = []map[string]any{}
	c.Sensors.Inotify.RecheckInterval = "900s"
	c.Sensors.Inotify.MaxDepth = 8
	c.Sensors.Rules.Dir = ""
	c.Sensors.Rules.ReloadDebounce = "500ms"
	c.Sensors.Limits.RulePerMin = 120
	c.Sensors.Limits.SourcePerMin = 600
	c.Sensors.Limits.GlobalPerMin = 1200
	c.Sensors.Limits.IncidentsPer5m = 25
	c.Dashboard.Bind = "127.0.0.1:7644"
	c.Dashboard.Port = 7644
	c.Dashboard.Auth.Transport = "cookie"
	c.Dashboard.Auth.IdentityProvider = ""
	c.Dashboard.Auth.SessionTTL = "24h"
	c.Dashboard.TokenFile = ""
	c.Dashboard.PollMS = 2000
	c.Dashboard.StripPollMS = 1000
	c.Dashboard.StallAlertS = 90
	c.Dashboard.HealthLoopbackExempt = true
	c.Dashboard.PageLimit = 100
	c.Dashboard.MaxBodyBytes = 4096
	c.Dashboard.Rate.ReadRPS = 20
	c.Dashboard.Rate.ReadBurst = 60
	c.Dashboard.Rate.WriteRPS = 5
	c.Dashboard.Rate.WriteBurst = 10
	c.Dashboard.AuthFailLimit = 10
	c.Dashboard.AuthFailWindow = "60s"
	c.Dashboard.MemPressurePct = 80
	c.Hub.Mode = "hub"
	c.Hub.URL = ""
	c.Hub.ForwardProjectID = ""
	c.Hub.Token = ""
	c.Hub.ProtocolVersion = 1
	c.Hub.ForwardBatchRecords = 200
	c.Hub.ForwardBatchBytes = 524288
	c.Hub.RetryBase = "2s"
	c.Hub.RetryMax = "5m"
	c.Hub.DedupLRU = 65536
	// SPEC-13 §2.1: the standalone profile is the default and it needs neither
	// dependency, so every server.redis.*/server.duckbrain.* default below is
	// inert until the operator selects light-hub. server.redis.url and
	// server.duckbrain.namespace default to "" on purpose: SPEC-13 §2.1 makes
	// them REQUIRED for light-hub, and a non-empty default would make that
	// requirement (and its TROUBLE-HUB-001 refusal) unreachable.
	c.Server.Profile = "standalone"
	c.Server.HubID = ""
	c.Server.Redis.URL = ""
	c.Server.Redis.PasswordEnv = "TROUBLE_REDIS_PASSWORD"
	c.Server.Redis.Stream = "trouble:ingest"
	c.Server.Redis.Group = "ledger-writers"
	c.Server.Redis.Consumer = ""
	c.Server.Redis.MaxLen = 1000000
	c.Server.Redis.DedupPrefix = "trouble:dedup:"
	c.Server.Redis.DedupTTL = "24h"
	c.Server.Redis.BatchRecords = 256
	c.Server.Redis.BatchBytes = 524288
	c.Server.Redis.BlockMS = 1000
	c.Server.Redis.ClaimMinIdle = "60s"
	c.Server.Redis.DialTimeout = "2s"
	c.Server.Redis.ReadTimeout = "5s"
	c.Server.Redis.WriteTimeout = "2s"
	c.Server.Redis.RequireRedis = false
	c.Server.Redis.RequirePersistence = true
	c.Server.Redis.CheckPolicy = true
	c.Server.Redis.FailoverGrace = "30s"
	c.Server.DuckBrain.Enabled = false
	c.Server.DuckBrain.Namespace = ""
	c.Server.DuckBrain.Endpoint = ""
	c.Server.DuckBrain.ArchiveInterval = "1h"
	c.Server.DuckBrain.ArchiveBatchFiles = 8
	c.Server.DuckBrain.KeepLocalGens = 2
	c.Server.DuckBrain.VerifyAfterWrite = true
	c.Server.DuckBrain.Gzip = true
	c.Spool.BudgetBytes = 268435456
	c.Spool.GapReserveBytes = 2097152
	c.Spool.Fsync = "group"
	c.Spool.FsyncWindowMS = 200
	// SPEC-12 §3.1d: the SPEC-08 §3.9a queue's five bounds. Every default is
	// the number that section already pins (256 entries, a 72 h TTL, a 5
	// attempt retry budget, a 5 s drain cadence, 100 entries per drain), so the
	// documented defaults and the resolvable defaults cannot drift apart.
	c.Flow.SpoolMaxEntries = 256
	c.Flow.SpoolTTL = "72h"
	c.Flow.SpoolMaxAttempts = 5
	c.Flow.ReplayEvery = "5s"
	c.Flow.ReplayBatch = 100
	c.Verify.ZoneWindows = map[string]types.Duration{
		"loopback": "10m",
		"lan":      "15m",
		"tailnet":  "20m",
		"public":   "30m",
	}
	c.Escalate.Channels = nil
	c.Escalate.Timeout = "10s"
	c.FS.ForbiddenStateRoots = []string{"/tmp", "/var/tmp"}
	c.FS.RemoteTypes = []string{"nfs", "nfs4", "cifs", "smb", "sshfs", "fuse.sshfs"}
	c.Origin.HostID = ""
	c.Origin.HubID = ""
	c.Migrate.DowngradeOK = false
	return c
}

func defaultStateRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base != "" {
		return filepath.Join(base, "trouble")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/var/lib/trouble"
	}
	return filepath.Join(home, ".local", "state", "trouble")
}

func defaultConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base != "" {
		return filepath.Join(base, "trouble", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/etc/trouble/config.toml"
	}
	return filepath.Join(home, ".config", "trouble", "config.toml")
}

func defaultEnvFile() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base != "" {
		return filepath.Join(base, "trouble", "trouble.env")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/etc/trouble/trouble.env"
	}
	return filepath.Join(home, ".config", "trouble", "trouble.env")
}

func defaultSystemdScope() string {
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		return "user"
	}
	return "system"
}

func asString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case types.Duration:
		return string(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case int:
		return strconv.Itoa(x), nil
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			s, err := asString(e)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), nil
	default:
		return "", fmt.Errorf("expected string, got %T", v)
	}
}

func asStringSlice(v any) ([]string, error) {
	switch x := v.(type) {
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			s, err := asString(e)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	case []string:
		return x, nil
	case string:
		if x == "" {
			return nil, nil
		}
		return strings.Split(x, ","), nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("expected array, got %T", v)
	}
}

// setRouteModeMap is the `sentinel.routes.per_class` setter: the sig-prefix →
// route table of SPEC-04 §3.10a / SPEC-TYPES §3.15.3. It accepts BOTH the raw
// file form (map[string]any, what the TOML reader and a flag/env string
// produce) and the typed form (map[string]string, what an embedder hands over),
// and it REPLACES the table rather than merging, so a lower-precedence source
// can never leave a stale prefix behind a resolved one. Values are carried as
// declared; route-name validation is the sentinel's boot refusal
// (TROUBLE-SENTINEL-023).
func setRouteModeMap(cfg *Config, v any) error {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]string:
		for k, val := range x {
			out[k] = val
		}
	case map[string]any:
		for k, val := range x {
			s, err := asString(val)
			if err != nil {
				return fmt.Errorf("per_class[%q]: %v", k, err)
			}
			out[k] = s
		}
	case string:
		out, err := parseRouteModeString(x)
		if err != nil {
			return err
		}
		cfg.Routes.PerClass = out
		return nil
	case nil:
		// keep the empty table
	default:
		return fmt.Errorf("expected per_class table, got %T", v)
	}
	cfg.Routes.PerClass = out
	return nil
}

// parseRouteModeString decodes the scalar spellings of the per_class table:
// a flag/env value can never carry the file's inline-table syntax structurally,
// so the string forms are the ordinary comma pair list —
// `psi:io_pressure=direct,sentinel:sha256v1=proxy` — plus the file's own
// inline-table text (`{ "psi:io_pressure" = "direct" }`), which the reader
// hands through verbatim as a bare string. Empty means no entries.
func parseRouteModeString(s string) (map[string]string, error) {
	out := map[string]string{}
	t := strings.TrimSpace(s)
	if t == "" {
		return out, nil
	}
	if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
		t = strings.TrimSpace(t[1 : len(t)-1])
		if t == "" {
			return out, nil
		}
	}
	// Quote-aware comma split, then one `key = value` per part. Bare values
	// are accepted the way the TOML reader accepts bare strings.
	for _, part := range splitArray(t) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" || v == "" || !ok {
			return nil, fmt.Errorf("per_class entry %q is not prefix=route", part)
		}
		out[k] = v
	}
	return out, nil
}

// asInotifyPathRows is the `sensors.inotify.paths` declaration: the list of
// tables written as `[[sensors.inotify.paths]]`, each carrying
// path/mask/recursive/max_depth/rule (SPEC-03 §4). The value is a DECLARATION,
// not a scalar, so a flag or an environment variable cannot express it and the
// scalar forms are refused by name — the same rule the four registered tables
// follow (SPEC-12 §2.5a). What a row must contain is internal/sensors' own
// check: this reader only carries the rows.
func asInotifyPathRows(v any) ([]map[string]any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []map[string]any:
		// The array-of-tables reader's own type: `[[sensors.inotify.paths]]`
		// parses into exactly this, and a nil slice is the empty set.
		return x, nil
	case []any:
		if len(x) == 0 {
			return nil, nil
		}
		rows := make([]map[string]any, 0, len(x))
		for _, e := range x {
			row, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("expected a list of tables, got an element of type %T", e)
			}
			rows = append(rows, row)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("is a list of tables: declare one [[sensors.inotify.paths]] table per watched path (path/mask/recursive/max_depth/rule) in the config file — a scalar value cannot express it")
	}
}

func asBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		return strconv.ParseBool(x)
	default:
		return false, fmt.Errorf("expected bool, got %T", v)
	}
}

func asInt(v any) (int, error) {
	switch x := v.(type) {
	case int64:
		return int(x), nil
	case int:
		return x, nil
	case string:
		i, err := strconv.ParseInt(x, 10, 64)
		return int(i), err
	default:
		return 0, fmt.Errorf("expected int, got %T", v)
	}
}

func asInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("expected int64, got %T", v)
	}
}

// asFloat accepts the numeric forms a TOML value can take for a float key.
func asFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	default:
		return 0, fmt.Errorf("expected float, got %T", v)
	}
}

func asDuration(v any) (types.Duration, error) {
	s, err := asString(v)
	if err != nil {
		return "", err
	}
	return types.Duration(s), nil
}

// asPositiveInt is asInt plus the bound rule the SPEC-12 §3.1d keys carry: a
// queue bound of zero (or less) is not a bound, so it is refused by name at
// resolution — the same TROUBLE-LIFECYCLE-001 class every other bad key value
// is — rather than resolved into an unbounded queue.
func asPositiveInt(v any) (int, error) {
	i, err := asInt(v)
	if err != nil {
		return 0, err
	}
	if i <= 0 {
		return 0, fmt.Errorf("must be a positive integer, got %d", i)
	}
	return i, nil
}

// asPositiveDuration is asDuration plus the bound rule: the value must be a
// parsable Go duration string AND strictly positive. `types.Duration.Std()`
// answers 0 for both an empty and an unparsable value, so a bare positivity
// check on the parsed result would accept garbage as "zero"; parsing here names
// the offending text instead.
func asPositiveDuration(v any) (types.Duration, error) {
	d, err := asDuration(v)
	if err != nil {
		return "", err
	}
	if d == "" {
		return "", fmt.Errorf("must be a positive duration, got an empty value")
	}
	std, perr := time.ParseDuration(string(d))
	if perr != nil {
		return "", fmt.Errorf("must be a Go duration string (for example 72h, 5m, 30s), got %q", string(d))
	}
	if std <= 0 {
		return "", fmt.Errorf("must be a positive duration, got %q", string(d))
	}
	return d, nil
}

func applyToMapStringDuration(m map[string]types.Duration, v any) error {
	switch x := v.(type) {
	case map[string]types.Duration:
		for k, d := range x {
			m[k] = d
		}
		return nil
	case map[string]any:
		for k, e := range x {
			d, err := asDuration(e)
			if err != nil {
				return err
			}
			m[k] = d
		}
		return nil
	case string:
		// "loopback=10m lan=15m ..."
		for _, part := range strings.Fields(x) {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				return fmt.Errorf("bad zone_windows pair %q", part)
			}
			m[kv[0]] = types.Duration(kv[1])
		}
		return nil
	default:
		return fmt.Errorf("expected map, got %T", v)
	}
}

// setProjects is the `projects` key's setter: the only accepted source shape is
// the array of tables the file parser builds for `[[projects]]` (SPEC-12 §3.1a).
// Every key inside a declaration is validated HERE, so an unknown key inside a
// row is refused by name exactly like an unknown top-level key (§3.1): a typo in
// a project's key must not silently leave that project without the value.
func setProjects(cfg *Config, v any) error {
	switch x := v.(type) {
	case nil:
		cfg.Projects = nil
		return nil
	case string:
		// A flag or env var can only carry a scalar; a project set is a table.
		if strings.TrimSpace(x) == "" {
			cfg.Projects = nil
			return nil
		}
		return fmt.Errorf("projects is an array of tables ([[projects]]); %q is not a declaration", x)
	case []map[string]any:
		out := make([]ProjectConfig, 0, len(x))
		for i, row := range x {
			p, err := projectRow(row)
			if err != nil {
				return fmt.Errorf("projects[%d]: %w", i, err)
			}
			out = append(out, p)
		}
		cfg.Projects = out
		return nil
	case []any:
		out := make([]ProjectConfig, 0, len(x))
		for i, e := range x {
			row, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("projects[%d]: expected a table, got %T", i, e)
			}
			p, err := projectRow(row)
			if err != nil {
				return fmt.Errorf("projects[%d]: %w", i, err)
			}
			out = append(out, p)
		}
		cfg.Projects = out
		return nil
	default:
		return fmt.Errorf("expected an array of tables ([[projects]]), got %T", v)
	}
}

// projectRow converts one declaration into a ProjectConfig, rejecting unknown
// keys and applying the two defaults a minimal declaration inherits (a declared
// project is enabled, and its slug defaults to its id).
func projectRow(row map[string]any) (ProjectConfig, error) {
	p := ProjectConfig{Enabled: true}
	for k, v := range row {
		switch k {
		case "id":
			s, err := asString(v)
			if err != nil {
				return p, fmt.Errorf("id: %w", err)
			}
			p.ID = strings.TrimSpace(s)
		case "slug":
			s, err := asString(v)
			if err != nil {
				return p, fmt.Errorf("slug: %w", err)
			}
			p.Slug = strings.TrimSpace(s)
		case "public_key":
			s, err := asString(v)
			if err != nil {
				return p, fmt.Errorf("public_key: %w", err)
			}
			p.PublicKey = strings.TrimSpace(s)
		case "secret", "secret_key":
			s, err := asString(v)
			if err != nil {
				return p, fmt.Errorf("%s: %w", k, err)
			}
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if p.Secret != "" && p.Secret != s {
				return p, fmt.Errorf("secret and secret_key disagree; declare one")
			}
			p.Secret = s
		case "auth_forms":
			ss, err := asStringSlice(v)
			if err != nil {
				return p, fmt.Errorf("auth_forms: %w", err)
			}
			p.AuthForms = ss
		case "quota_epm":
			i, err := asInt(v)
			if err != nil {
				return p, fmt.Errorf("quota_epm: %w", err)
			}
			p.QuotaEPM = i
		case "disk_budget_bytes":
			i, err := asInt64(v)
			if err != nil {
				return p, fmt.Errorf("disk_budget_bytes: %w", err)
			}
			p.DiskBudgetBytes = i
		case "loss_policy":
			s, err := asString(v)
			if err != nil {
				return p, fmt.Errorf("loss_policy: %w", err)
			}
			p.LossPolicy = strings.TrimSpace(s)
		case "enabled":
			b, err := asBool(v)
			if err != nil {
				return p, fmt.Errorf("enabled: %w", err)
			}
			p.Enabled = b
			p.enabledSet = true
		default:
			return p, fmt.Errorf("unknown projects key %q", k)
		}
	}
	return p, nil
}

// projectsSummary renders the declared project set for the explain dump and the
// boot config record. It never carries a secret: a declared secret is reported
// as present-but-unstated (SPEC-12 §3.1 — a config value can be set,
// redacted-echoed or rejected, never revealed), which is why the `projects` row
// is rendered rather than copied from its source value.
//
// The rendering says "secret (set)" and not "secret=<x>" on purpose: the
// mandatory env_assign / kv_secret_assign scrub rules match SECRET followed by
// `=` or `:`, and the boot config record travels through the ledger's
// persistence-boundary re-scan, which refuses a record whose text still matches
// a mandatory rule (TROUBLE-SCRUB-008). A summary that spells `secret=` never
// reaches the ledger at all.
func (c Config) projectsSummary() string {
	if len(c.Projects) == 0 {
		return "none declared"
	}
	parts := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		s := p.ID
		if p.Slug != "" {
			s += "/" + p.Slug
		}
		s += " public_key " + p.PublicKey
		if p.Secret != "" {
			s += " secret (set)"
		} else {
			s += " secret (none)"
		}
		if !p.Enabled {
			s += " disabled"
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("%d declared: %s", len(c.Projects), strings.Join(parts, ", "))
}

// ProjectsSet is the declared project set in the shape the sentinel consumes
// (SPEC-12 §3.1a): defaults applied, then validated. A malformed declaration is
// an ERROR — the boot refuses with TROUBLE-LIFECYCLE-001 rather than dropping
// the project, because a dropped project is an ingest plane that silently knows
// no project while the port is open.
//
// No declaration is not a malformed declaration: it returns an empty set and the
// sentinel's own refusal path reports it (SPEC-12 §3.3a).
func (c Config) ProjectsSet() ([]types.Project, error) {
	if len(c.Projects) == 0 {
		return nil, nil
	}
	out := make([]types.Project, 0, len(c.Projects))
	ids := make(map[string]bool, len(c.Projects))
	keys := make(map[string]bool, len(c.Projects))
	for i, p := range c.Projects {
		where := fmt.Sprintf("projects[%d]", i)
		if !validProjectID(p.ID) {
			return nil, fmt.Errorf("%w: %s: id %q must be numeric-as-string 1..2^31-1 (SPEC-12 §3.1a)",
				types.CodeLifecycle001, where, p.ID)
		}
		if ids[p.ID] {
			return nil, fmt.Errorf("%w: %s: duplicate id %q", types.CodeLifecycle001, where, p.ID)
		}
		ids[p.ID] = true
		if !validHexKey(p.PublicKey) {
			return nil, fmt.Errorf("%w: %s: public_key %q must be exactly 32 lowercase hex (SPEC-12 §3.1a)",
				types.CodeLifecycle001, where, p.PublicKey)
		}
		if keys[p.PublicKey] {
			return nil, fmt.Errorf("%w: %s: duplicate public_key %q (one key resolves to one project)",
				types.CodeLifecycle001, where, p.PublicKey)
		}
		keys[p.PublicKey] = true
		if p.Secret != "" && !validHexKey(p.Secret) {
			return nil, fmt.Errorf("%w: %s: secret must be exactly 32 lowercase hex (SPEC-12 §3.1a)",
				types.CodeLifecycle001, where)
		}
		if p.QuotaEPM < 0 {
			return nil, fmt.Errorf("%w: %s: quota_epm must be a positive integer or 0 for the default",
				types.CodeLifecycle001, where)
		}
		if p.DiskBudgetBytes < 0 {
			return nil, fmt.Errorf("%w: %s: disk_budget_bytes must be non-negative", types.CodeLifecycle001, where)
		}
		if p.LossPolicy != "" && !types.LossPolicy(p.LossPolicy).Valid() {
			return nil, fmt.Errorf("%w: %s: loss_policy %q is not one of sample|drop-with-counter|spool-if-light",
				types.CodeLifecycle001, where, p.LossPolicy)
		}
		out = append(out, c.projectOf(p))
	}
	return out, nil
}

// projectOf applies the declaration's defaults (a slug is the id when unset; a
// zero quota or disk budget takes the SPEC-04 §2.2 default) and maps it onto the
// shared type.
func (c Config) projectOf(p ProjectConfig) types.Project {
	proj := types.Project{
		ID:         p.ID,
		Slug:       p.Slug,
		PublicKey:  p.PublicKey,
		SecretKey:  p.Secret,
		AuthForms:  append([]string(nil), p.AuthForms...),
		QuotaEPM:   p.QuotaEPM,
		DiskBudget: p.DiskBudgetBytes,
		LossPolicy: types.LossPolicy(p.LossPolicy),
		Enabled:    p.Enabled || !p.enabledSet,
	}
	if proj.Slug == "" {
		proj.Slug = proj.ID
	}
	if proj.QuotaEPM == 0 {
		proj.QuotaEPM = DefaultProjectQuotaEPM
	}
	if proj.DiskBudget == 0 {
		proj.DiskBudget = DefaultProjectDiskBudget
	}
	return proj
}

// validProjectID reports whether id is numeric-as-string 1..2^31-1 (the shape
// the DSN path element and the sentinel's project index require).
func validProjectID(id string) bool {
	if id == "" || len(id) > 10 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n >= 1 && n <= 2147483647
}

// validHexKey reports whether s is exactly 32 lowercase hex.
func validHexKey(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// registry lists every known key, its default and a typed setter.
func registry(c *Config) []keyMeta {
	return []keyMeta{
		{"state_root", "", "state_root", c.StateRoot, func(cfg *Config, v any) error { s, err := asString(v); cfg.StateRoot = s; return err }},
		{"config_path", "", "config_path", c.ConfigPath, func(cfg *Config, v any) error { s, err := asString(v); cfg.ConfigPath = s; return err }},
		{"secrets.environment_file", "secrets", "environment_file", c.Secrets.EnvironmentFile, func(cfg *Config, v any) error { s, err := asString(v); cfg.Secrets.EnvironmentFile = s; return err }},
		{"lifecycle.systemd_scope", "lifecycle", "systemd_scope", c.Lifecycle.SystemdScope, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.SystemdScope = s; return err }},
		{"lifecycle.unit_name", "lifecycle", "unit_name", c.Lifecycle.UnitName, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.UnitName = s; return err }},
		{"lifecycle.escalate_unit", "lifecycle", "escalate_unit", c.Lifecycle.EscalateUnit, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.EscalateUnit = s; return err }},
		{"lifecycle.checker_unit", "lifecycle", "checker_unit", c.Lifecycle.CheckerUnit, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.CheckerUnit = s; return err }},
		{"lifecycle.sandbox", "lifecycle", "sandbox", c.Lifecycle.Sandbox, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.Sandbox = s; return err }},
		{"lifecycle.user", "lifecycle", "user", c.Lifecycle.User, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.User = s; return err }},
		{"lifecycle.heartbeat_path", "lifecycle", "heartbeat_path", c.Lifecycle.HeartbeatPath, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.HeartbeatPath = s; return err }},
		{"lifecycle.heartbeat_interval", "lifecycle", "heartbeat_interval", c.Lifecycle.HeartbeatInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Lifecycle.HeartbeatInterval = d
			return err
		}},
		{"lifecycle.heartbeat_stale_after", "lifecycle", "heartbeat_stale_after", c.Lifecycle.HeartbeatStaleAfter, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Lifecycle.HeartbeatStaleAfter = d
			return err
		}},
		{"lifecycle.idle_heartbeat_interval", "lifecycle", "idle_heartbeat_interval", c.Lifecycle.IdleHeartbeatInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Lifecycle.IdleHeartbeatInterval = d
			return err
		}},
		{"lifecycle.watchdog_sec", "lifecycle", "watchdog_sec", c.Lifecycle.WatchdogSec, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Lifecycle.WatchdogSec = d; return err }},
		{"lifecycle.drain_timeout", "lifecycle", "drain_timeout", c.Lifecycle.DrainTimeout, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Lifecycle.DrainTimeout = d; return err }},
		{"lifecycle.upgrade_ready_timeout", "lifecycle", "upgrade_ready_timeout", c.Lifecycle.UpgradeReadyTimeout, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Lifecycle.UpgradeReadyTimeout = d
			return err
		}},
		{"lifecycle.rollback_depth", "lifecycle", "rollback_depth", c.Lifecycle.RollbackDepth, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Lifecycle.RollbackDepth = i; return err }},
		{"lifecycle.self_rss_warn", "lifecycle", "self_rss_warn", c.Lifecycle.SelfRSSWarn, func(cfg *Config, v any) error { s, err := asString(v); cfg.Lifecycle.SelfRSSWarn = s; return err }},
		{"lifecycle.clock_skew_tolerance", "lifecycle", "clock_skew_tolerance", c.Lifecycle.ClockSkewTolerance, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Lifecycle.ClockSkewTolerance = d
			return err
		}},
		{"stall.max_seq_age", "stall", "max_seq_age", c.Stall.MaxSeqAge, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Stall.MaxSeqAge = d; return err }},
		{"checker.interval", "checker", "interval", c.Checker.Interval, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Checker.Interval = d; return err }},
		{"checker.confirm_runs", "checker", "confirm_runs", c.Checker.ConfirmRuns, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Checker.ConfirmRuns = i; return err }},
		{"checker.state_file", "checker", "state_file", c.Checker.StateFile, func(cfg *Config, v any) error { s, err := asString(v); cfg.Checker.StateFile = s; return err }},
		{"checker.alarm_file", "checker", "alarm_file", c.Checker.AlarmFile, func(cfg *Config, v any) error { s, err := asString(v); cfg.Checker.AlarmFile = s; return err }},
		{"checker.alarm_command", "checker", "alarm_command", c.Checker.AlarmCommand, func(cfg *Config, v any) error { ss, err := asStringSlice(v); cfg.Checker.AlarmCommand = ss; return err }},
		{"ingest.bind", "ingest", "bind", c.Ingest.Bind, func(cfg *Config, v any) error { s, err := asString(v); cfg.Ingest.Bind = s; return err }},
		{"ingest.advertised_host", "ingest", "advertised_host", c.Ingest.AdvertisedHost, func(cfg *Config, v any) error { s, err := asString(v); cfg.Ingest.AdvertisedHost = s; return err }},
		{"ingest.auth.loopback_dsn", "ingest", "loopback_dsn", c.Ingest.Auth.LoopbackDSN, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Ingest.Auth.LoopbackDSN = b; return err }},
		{"ingest.auth.nonloopback_mode", "ingest", "nonloopback_mode", c.Ingest.Auth.NonloopbackMode, func(cfg *Config, v any) error { s, err := asString(v); cfg.Ingest.Auth.NonloopbackMode = s; return err }},
		{"ingest.auth.public_require_proxy", "ingest", "public_require_proxy", c.Ingest.Auth.PublicRequireProxy, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Ingest.Auth.PublicRequireProxy = b
			return err
		}},
		{"health_url", "", "health_url", c.HealthURL, func(cfg *Config, v any) error { s, err := asString(v); cfg.HealthURL = s; return err }},
		{"projects", "", "projects", "", setProjects},
		{"issues", "", "issues", SubsystemTable{}, func(cfg *Config, v any) error { return setSubsystemTable(&cfg.IssuesTable, "issues", v) }},
		{"skills", "", "skills", SubsystemTable{}, func(cfg *Config, v any) error { return setSubsystemTable(&cfg.SkillsTable, "skills", v) }},
		{"llm", "", "llm", SubsystemTable{}, func(cfg *Config, v any) error { return setSubsystemTable(&cfg.LLMTable, "llm", v) }},
		// ---- the SPEC-03 §4 detection-plane surface (SPEC-12 §3.1e) ----
		// Every key of internal/sensors' own config switch is registered
		// leaf-by-leaf, so each resolves with ordinary precedence (flag > env >
		// file > default), carries provenance and gets its own `trouble config
		// explain` row. Before this only the fold window was registered: any
		// other `sensors.*` file key was refused as an unknown file key
		// (TROUBLE-LIFECYCLE-001), which made the whole plane untunable from a
		// config file (TRBL-036). The defaults here are the sensors package's
		// own, and its decoder remains the only validator of these values.
		{"sensors.sample_fold_window", "sensors", "sample_fold_window", c.Sensors.SampleFoldWindow, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.SampleFoldWindow = d
			return err
		}},
		{"sensors.merge_window", "sensors", "merge_window", c.Sensors.MergeWindow, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Sensors.MergeWindow = d; return err }},
		{"sensors.psi.enabled", "sensors", "enabled", c.Sensors.PSI.Enabled, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Sensors.PSI.Enabled = b; return err }},
		{"sensors.psi.sample_interval", "sensors", "sample_interval", c.Sensors.PSI.SampleInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.PSI.SampleInterval = d
			return err
		}},
		{"sensors.psi.window", "sensors", "window", c.Sensors.PSI.Window, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Sensors.PSI.Window = d; return err }},
		{"sensors.journald.enabled", "sensors", "enabled", c.Sensors.Journald.Enabled, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Sensors.Journald.Enabled = b; return err }},
		{"sensors.journald.units", "sensors", "units", c.Sensors.Journald.Units, func(cfg *Config, v any) error {
			ss, err := asStringSlice(v)
			cfg.Sensors.Journald.Units = ss
			return err
		}},
		{"sensors.journald.follow_all", "sensors", "follow_all", c.Sensors.Journald.FollowAll, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Sensors.Journald.FollowAll = b
			return err
		}},
		{"sensors.journald.follow_all_max_entries", "sensors", "follow_all_max_entries", c.Sensors.Journald.FollowAllMaxEntries, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Journald.FollowAllMaxEntries = i
			return err
		}},
		{"sensors.journald.queue", "sensors", "queue", c.Sensors.Journald.Queue, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Sensors.Journald.Queue = i; return err }},
		{"sensors.journald.queue_bytes", "sensors", "queue_bytes", c.Sensors.Journald.QueueBytes, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Journald.QueueBytes = i
			return err
		}},
		{"sensors.journald.max_entry", "sensors", "max_entry", c.Sensors.Journald.MaxEntry, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Journald.MaxEntry = i
			return err
		}},
		{"sensors.journald.probe_interval", "sensors", "probe_interval", c.Sensors.Journald.ProbeInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.Journald.ProbeInterval = d
			return err
		}},
		{"sensors.dbus.enabled", "sensors", "enabled", c.Sensors.DBus.Enabled, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Sensors.DBus.Enabled = b; return err }},
		{"sensors.dbus.user_managers", "sensors", "user_managers", c.Sensors.DBus.UserManagers, func(cfg *Config, v any) error {
			ss, err := asStringSlice(v)
			cfg.Sensors.DBus.UserManagers = ss
			return err
		}},
		{"sensors.dbus.ping_interval", "sensors", "ping_interval", c.Sensors.DBus.PingInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.DBus.PingInterval = d
			return err
		}},
		{"sensors.dbus.reconcile_interval", "sensors", "reconcile_interval", c.Sensors.DBus.ReconcileInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.DBus.ReconcileInterval = d
			return err
		}},
		{"sensors.dbus.oomd_probe_interval", "sensors", "oomd_probe_interval", c.Sensors.DBus.OomdProbeInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.DBus.OomdProbeInterval = d
			return err
		}},
		{"sensors.disk.enabled", "sensors", "enabled", c.Sensors.Disk.Enabled, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Sensors.Disk.Enabled = b; return err }},
		{"sensors.disk.mounts", "sensors", "mounts", c.Sensors.Disk.Mounts, func(cfg *Config, v any) error {
			ss, err := asStringSlice(v)
			cfg.Sensors.Disk.Mounts = ss
			return err
		}},
		{"sensors.disk.interval", "sensors", "interval", c.Sensors.Disk.Interval, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Sensors.Disk.Interval = d; return err }},
		{"sensors.timers.enabled", "sensors", "enabled", c.Sensors.Timers.Enabled, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Sensors.Timers.Enabled = b; return err }},
		{"sensors.timers.interval", "sensors", "interval", c.Sensors.Timers.Interval, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Sensors.Timers.Interval = d; return err }},
		{"sensors.inotify.enabled", "sensors", "enabled", c.Sensors.Inotify.Enabled, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Sensors.Inotify.Enabled = b
			return err
		}},
		// The one declaration-valued key of the §4 surface: a list of tables,
		// one per watched path. A scalar source cannot express it, so a flag or
		// an environment value is refused by name (SPEC-12 §2.5a) instead of
		// being flattened into a second, invented syntax, and the file is the
		// source that sets it (SPEC-12 §3.1e). internal/sensors validates every
		// row: a table without a path is TROUBLE-SENSORS-* at decode time.
		{"sensors.inotify.paths", "sensors", "paths", c.Sensors.Inotify.Paths, func(cfg *Config, v any) error {
			rows, err := asInotifyPathRows(v)
			if err != nil {
				return err
			}
			cfg.Sensors.Inotify.Paths = rows
			return nil
		}},
		{"sensors.inotify.recheck_interval", "sensors", "recheck_interval", c.Sensors.Inotify.RecheckInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.Inotify.RecheckInterval = d
			return err
		}},
		{"sensors.inotify.max_depth", "sensors", "max_depth", c.Sensors.Inotify.MaxDepth, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Inotify.MaxDepth = i
			return err
		}},
		{"sensors.rules.dir", "sensors", "dir", c.Sensors.Rules.Dir, func(cfg *Config, v any) error { s, err := asString(v); cfg.Sensors.Rules.Dir = s; return err }},
		{"sensors.rules.reload_debounce", "sensors", "reload_debounce", c.Sensors.Rules.ReloadDebounce, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Sensors.Rules.ReloadDebounce = d
			return err
		}},
		{"sensors.limits.rule_per_min", "sensors", "rule_per_min", c.Sensors.Limits.RulePerMin, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Limits.RulePerMin = i
			return err
		}},
		{"sensors.limits.source_per_min", "sensors", "source_per_min", c.Sensors.Limits.SourcePerMin, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Limits.SourcePerMin = i
			return err
		}},
		{"sensors.limits.global_per_min", "sensors", "global_per_min", c.Sensors.Limits.GlobalPerMin, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Limits.GlobalPerMin = i
			return err
		}},
		{"sensors.limits.incidents_per_5m", "sensors", "incidents_per_5m", c.Sensors.Limits.IncidentsPer5m, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Sensors.Limits.IncidentsPer5m = i
			return err
		}},
		{"dashboard.bind", "dashboard", "bind", c.Dashboard.Bind, func(cfg *Config, v any) error { s, err := asString(v); cfg.Dashboard.Bind = s; return err }},
		{"dashboard.port", "dashboard", "port", c.Dashboard.Port, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.Port = i; return err }},
		{"dashboard.mandate", "dashboard", "mandate", c.Dashboard.Mandate, func(cfg *Config, v any) error { s, err := asString(v); cfg.Dashboard.Mandate = s; return err }},
		{"dashboard.proxy_trusted", "dashboard", "proxy_trusted", c.Dashboard.ProxyTrusted, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Dashboard.ProxyTrusted = b; return err }},
		{"dashboard.proxy_cidrs", "dashboard", "proxy_cidrs", c.Dashboard.ProxyCIDRs, func(cfg *Config, v any) error { ss, err := asStringSlice(v); cfg.Dashboard.ProxyCIDRs = ss; return err }},
		{"dashboard.public_origin", "dashboard", "public_origin", c.Dashboard.PublicOrigin, func(cfg *Config, v any) error { s, err := asString(v); cfg.Dashboard.PublicOrigin = s; return err }},
		{"dashboard.project_scope", "dashboard", "project_scope", c.Dashboard.ProjectScope, func(cfg *Config, v any) error {
			ss, err := asStringSlice(v)
			cfg.Dashboard.ProjectScope = ss
			return err
		}},
		{"dashboard.token_file", "dashboard", "token_file", c.Dashboard.TokenFile, func(cfg *Config, v any) error { s, err := asString(v); cfg.Dashboard.TokenFile = s; return err }},
		{"dashboard.poll_ms", "dashboard", "poll_ms", c.Dashboard.PollMS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.PollMS = i; return err }},
		{"dashboard.strip_poll_ms", "dashboard", "strip_poll_ms", c.Dashboard.StripPollMS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.StripPollMS = i; return err }},
		{"dashboard.stall_alert_s", "dashboard", "stall_alert_s", c.Dashboard.StallAlertS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.StallAlertS = i; return err }},
		{"dashboard.health_loopback_exempt", "dashboard", "health_loopback_exempt", c.Dashboard.HealthLoopbackExempt, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Dashboard.HealthLoopbackExempt = b
			return err
		}},
		{"dashboard.read_only", "dashboard", "read_only", c.Dashboard.ReadOnly, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Dashboard.ReadOnly = b; return err }},
		{"dashboard.allow_resume", "dashboard", "allow_resume", c.Dashboard.AllowResume, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Dashboard.AllowResume = b; return err }},
		{"dashboard.allow_full", "dashboard", "allow_full", c.Dashboard.AllowFull, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Dashboard.AllowFull = b; return err }},
		{"dashboard.page_limit", "dashboard", "page_limit", c.Dashboard.PageLimit, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.PageLimit = i; return err }},
		{"dashboard.max_body_bytes", "dashboard", "max_body_bytes", c.Dashboard.MaxBodyBytes, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Dashboard.MaxBodyBytes = i; return err }},
		{"dashboard.rate.read_rps", "dashboard", "read_rps", c.Dashboard.Rate.ReadRPS, func(cfg *Config, v any) error { f, err := asFloat(v); cfg.Dashboard.Rate.ReadRPS = f; return err }},
		{"dashboard.rate.read_burst", "dashboard", "read_burst", c.Dashboard.Rate.ReadBurst, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.Rate.ReadBurst = i; return err }},
		{"dashboard.rate.write_rps", "dashboard", "write_rps", c.Dashboard.Rate.WriteRPS, func(cfg *Config, v any) error { f, err := asFloat(v); cfg.Dashboard.Rate.WriteRPS = f; return err }},
		{"dashboard.rate.write_burst", "dashboard", "write_burst", c.Dashboard.Rate.WriteBurst, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.Rate.WriteBurst = i; return err }},
		{"dashboard.auth_fail_limit", "dashboard", "auth_fail_limit", c.Dashboard.AuthFailLimit, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.AuthFailLimit = i; return err }},
		{"dashboard.auth_fail_window", "dashboard", "auth_fail_window", c.Dashboard.AuthFailWindow, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Dashboard.AuthFailWindow = d; return err }},
		{"dashboard.mem_pressure_pct", "dashboard", "mem_pressure_pct", c.Dashboard.MemPressurePct, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Dashboard.MemPressurePct = i; return err }},
		{"dashboard.auth.transport", "dashboard", "transport", c.Dashboard.Auth.Transport, func(cfg *Config, v any) error { s, err := asString(v); cfg.Dashboard.Auth.Transport = s; return err }},
		{"dashboard.auth.identity_provider", "dashboard", "identity_provider", c.Dashboard.Auth.IdentityProvider, func(cfg *Config, v any) error {
			s, err := asString(v)
			cfg.Dashboard.Auth.IdentityProvider = s
			return err
		}},
		{"dashboard.auth.session_ttl", "dashboard", "session_ttl", c.Dashboard.Auth.SessionTTL, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Dashboard.Auth.SessionTTL = d; return err }},
		{"hub.mode", "hub", "mode", c.Hub.Mode, func(cfg *Config, v any) error { s, err := asString(v); cfg.Hub.Mode = s; return err }},
		{"hub.url", "hub", "url", c.Hub.URL, func(cfg *Config, v any) error { s, err := asString(v); cfg.Hub.URL = s; return err }},
		{"hub.forward_project_id", "hub", "forward_project_id", c.Hub.ForwardProjectID, func(cfg *Config, v any) error { s, err := asString(v); cfg.Hub.ForwardProjectID = s; return err }},
		{"hub.token", "hub", "token", c.Hub.Token, func(cfg *Config, v any) error { s, err := asString(v); cfg.Hub.Token = s; return err }},
		{"hub.protocol_version", "hub", "protocol_version", c.Hub.ProtocolVersion, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Hub.ProtocolVersion = i; return err }},
		{"hub.forward_batch_records", "hub", "forward_batch_records", c.Hub.ForwardBatchRecords, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Hub.ForwardBatchRecords = i; return err }},
		{"hub.forward_batch_bytes", "hub", "forward_batch_bytes", c.Hub.ForwardBatchBytes, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Hub.ForwardBatchBytes = i; return err }},
		{"hub.retry_base", "hub", "retry_base", c.Hub.RetryBase, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Hub.RetryBase = d; return err }},
		{"hub.retry_max", "hub", "retry_max", c.Hub.RetryMax, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Hub.RetryMax = d; return err }},
		{"hub.dedup_lru", "hub", "dedup_lru", c.Hub.DedupLRU, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Hub.DedupLRU = i; return err }},
		// ---- the SPEC-04 §3.10a sensor-transport route surface (SPEC-12
		// leaf registration, TRBL-061) ----
		// `routes.default` and `routes.per_class` are ordinary keys, not a
		// table: each resolves with the ordinary precedence (flag > env >
		// file > default), carries its own provenance and has its own
		// `trouble config explain` row (--sentinel-routes-default,
		// TROUBLE_SENTINEL_ROUTES_PER_CLASS). The value vocabulary
		// (auto|direct|proxy and the sig-prefix map) is validated by
		// internal/sentinel at boot (TROUBLE-SENTINEL-023), the same split the
		// sensors plane uses (this package resolves, the owning package
		// judges).
		{"sentinel.routes.default", "sentinel.routes", "default", c.Routes.Default, func(cfg *Config, v any) error { s, err := asString(v); cfg.Routes.Default = s; return err }},
		{"sentinel.routes.per_class", "sentinel.routes", "per_class", map[string]string{}, setRouteModeMap},
		// SPEC-13 §2.1/§2.1.1 `[server]` surface. The profile and the two
		// dependency blocks are registered leaf by leaf, so the profile resolves
		// with the same precedence, provenance and redaction as every other key
		// (SPEC-12 §3.7a rule 1) and `trouble config explain` lists each one.
		{"server.profile", "server", "profile", c.Server.Profile, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.Profile = s; return err }},
		{"server.hub_id", "server", "hub_id", c.Server.HubID, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.HubID = s; return err }},
		{"server.redis.url", "server", "url", c.Server.Redis.URL, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.Redis.URL = s; return err }},
		{"server.redis.password_env", "server", "password_env", c.Server.Redis.PasswordEnv, func(cfg *Config, v any) error {
			s, err := asString(v)
			cfg.Server.Redis.PasswordEnv = s
			return err
		}},
		{"server.redis.stream", "server", "stream", c.Server.Redis.Stream, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.Redis.Stream = s; return err }},
		{"server.redis.group", "server", "group", c.Server.Redis.Group, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.Redis.Group = s; return err }},
		{"server.redis.consumer", "server", "consumer", c.Server.Redis.Consumer, func(cfg *Config, v any) error { s, err := asString(v); cfg.Server.Redis.Consumer = s; return err }},
		{"server.redis.maxlen", "server", "maxlen", c.Server.Redis.MaxLen, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Server.Redis.MaxLen = i; return err }},
		{"server.redis.dedup_prefix", "server", "dedup_prefix", c.Server.Redis.DedupPrefix, func(cfg *Config, v any) error {
			s, err := asString(v)
			cfg.Server.Redis.DedupPrefix = s
			return err
		}},
		{"server.redis.dedup_ttl", "server", "dedup_ttl", c.Server.Redis.DedupTTL, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Server.Redis.DedupTTL = d; return err }},
		{"server.redis.batch_records", "server", "batch_records", c.Server.Redis.BatchRecords, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Server.Redis.BatchRecords = i
			return err
		}},
		{"server.redis.batch_bytes", "server", "batch_bytes", c.Server.Redis.BatchBytes, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Server.Redis.BatchBytes = i; return err }},
		{"server.redis.block_ms", "server", "block_ms", c.Server.Redis.BlockMS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Server.Redis.BlockMS = i; return err }},
		{"server.redis.claim_min_idle", "server", "claim_min_idle", c.Server.Redis.ClaimMinIdle, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.Redis.ClaimMinIdle = d
			return err
		}},
		{"server.redis.dial_timeout", "server", "dial_timeout", c.Server.Redis.DialTimeout, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.Redis.DialTimeout = d
			return err
		}},
		{"server.redis.read_timeout", "server", "read_timeout", c.Server.Redis.ReadTimeout, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.Redis.ReadTimeout = d
			return err
		}},
		{"server.redis.write_timeout", "server", "write_timeout", c.Server.Redis.WriteTimeout, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.Redis.WriteTimeout = d
			return err
		}},
		{"server.redis.require_redis", "server", "require_redis", c.Server.Redis.RequireRedis, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.Redis.RequireRedis = b
			return err
		}},
		{"server.redis.require_persistence", "server", "require_persistence", c.Server.Redis.RequirePersistence, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.Redis.RequirePersistence = b
			return err
		}},
		{"server.redis.check_policy", "server", "check_policy", c.Server.Redis.CheckPolicy, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.Redis.CheckPolicy = b
			return err
		}},
		{"server.redis.failover_grace", "server", "failover_grace", c.Server.Redis.FailoverGrace, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.Redis.FailoverGrace = d
			return err
		}},
		// server.duckbrain.enabled is the operator switch for a standalone host
		// that wants the archival tier anyway; under profile=light-hub archival
		// is implied on (SPEC-13 §2.1), which is why the light-hub gate requires
		// server.duckbrain.namespace regardless of this key.
		{"server.duckbrain.enabled", "server", "enabled", c.Server.DuckBrain.Enabled, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.DuckBrain.Enabled = b
			return err
		}},
		{"server.duckbrain.namespace", "server", "namespace", c.Server.DuckBrain.Namespace, func(cfg *Config, v any) error {
			s, err := asString(v)
			cfg.Server.DuckBrain.Namespace = s
			return err
		}},
		{"server.duckbrain.endpoint", "server", "endpoint", c.Server.DuckBrain.Endpoint, func(cfg *Config, v any) error {
			s, err := asString(v)
			cfg.Server.DuckBrain.Endpoint = s
			return err
		}},
		{"server.duckbrain.archive_interval", "server", "archive_interval", c.Server.DuckBrain.ArchiveInterval, func(cfg *Config, v any) error {
			d, err := asDuration(v)
			cfg.Server.DuckBrain.ArchiveInterval = d
			return err
		}},
		{"server.duckbrain.archive_batch_files", "server", "archive_batch_files", c.Server.DuckBrain.ArchiveBatchFiles, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Server.DuckBrain.ArchiveBatchFiles = i
			return err
		}},
		{"server.duckbrain.keep_local_generations", "server", "keep_local_generations", c.Server.DuckBrain.KeepLocalGens, func(cfg *Config, v any) error {
			i, err := asInt(v)
			cfg.Server.DuckBrain.KeepLocalGens = i
			return err
		}},
		{"server.duckbrain.verify_after_write", "server", "verify_after_write", c.Server.DuckBrain.VerifyAfterWrite, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.DuckBrain.VerifyAfterWrite = b
			return err
		}},
		{"server.duckbrain.gzip", "server", "gzip", c.Server.DuckBrain.Gzip, func(cfg *Config, v any) error {
			b, err := asBool(v)
			cfg.Server.DuckBrain.Gzip = b
			return err
		}},
		{"spool.budget_bytes", "spool", "budget_bytes", c.Spool.BudgetBytes, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Spool.BudgetBytes = i; return err }},
		{"spool.gap_reserve_bytes", "spool", "gap_reserve_bytes", c.Spool.GapReserveBytes, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Spool.GapReserveBytes = i; return err }},
		{"spool.fsync", "spool", "fsync", c.Spool.Fsync, func(cfg *Config, v any) error { s, err := asString(v); cfg.Spool.Fsync = s; return err }},
		{"spool.fsync_window_ms", "spool", "fsync_window_ms", c.Spool.FsyncWindowMS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Spool.FsyncWindowMS = i; return err }},
		// SPEC-08 §3.9a × SPEC-12 §3.1d: the flow's durable dispatch queue is
		// bounded by five keys registered here leaf by leaf, so the queue's
		// limits resolve with the ordinary precedence (flag > env > file >
		// default), the ordinary provenance and one `trouble config explain`
		// row each — instead of being compile-time constants in internal/flow.
		// Each bound is refused when it is not strictly positive: a zero bound
		// is not a bound, and resolving it would hand the daemon an unbounded
		// queue (the refusal is the TROUBLE-LIFECYCLE-001 class this resolver
		// already carries for a bad key value).
		{"flow.spool_max_entries", "flow", "spool_max_entries", c.Flow.SpoolMaxEntries, func(cfg *Config, v any) error {
			i, err := asPositiveInt(v)
			cfg.Flow.SpoolMaxEntries = i
			return err
		}},
		{"flow.spool_ttl", "flow", "spool_ttl", c.Flow.SpoolTTL, func(cfg *Config, v any) error {
			d, err := asPositiveDuration(v)
			cfg.Flow.SpoolTTL = d
			return err
		}},
		{"flow.spool_max_attempts", "flow", "spool_max_attempts", c.Flow.SpoolMaxAttempts, func(cfg *Config, v any) error {
			i, err := asPositiveInt(v)
			cfg.Flow.SpoolMaxAttempts = i
			return err
		}},
		{"flow.replay_every", "flow", "replay_every", c.Flow.ReplayEvery, func(cfg *Config, v any) error {
			d, err := asPositiveDuration(v)
			cfg.Flow.ReplayEvery = d
			return err
		}},
		{"flow.replay_batch", "flow", "replay_batch", c.Flow.ReplayBatch, func(cfg *Config, v any) error {
			i, err := asPositiveInt(v)
			cfg.Flow.ReplayBatch = i
			return err
		}},
		{"verify.zone_windows", "verify", "zone_windows", c.Verify.ZoneWindows, func(cfg *Config, v any) error { return applyToMapStringDuration(cfg.Verify.ZoneWindows, v) }},
		{"escalate.channels", "escalate", "channels", c.Escalate.Channels, func(cfg *Config, v any) error {
			switch x := v.(type) {
			case []any:
				out := make([][]string, len(x))
				for i, e := range x {
					switch y := e.(type) {
					case []any:
						row := make([]string, len(y))
						for j, ee := range y {
							s, err := asString(ee)
							if err != nil {
								return err
							}
							row[j] = s
						}
						out[i] = row
					case string:
						out[i] = strings.Fields(y)
					default:
						return fmt.Errorf("expected argv array, got %T", e)
					}
				}
				cfg.Escalate.Channels = out
				return nil
			case [][]string:
				cfg.Escalate.Channels = x
				return nil
			case string:
				if x == "" {
					cfg.Escalate.Channels = nil
					return nil
				}
				cfg.Escalate.Channels = [][]string{strings.Fields(x)}
				return nil
			case nil:
				cfg.Escalate.Channels = nil
				return nil
			default:
				return fmt.Errorf("expected channels array, got %T", v)
			}
		}},
		{"escalate.timeout", "escalate", "timeout", c.Escalate.Timeout, func(cfg *Config, v any) error { d, err := asDuration(v); cfg.Escalate.Timeout = d; return err }},
		{"fs.forbidden_state_roots", "fs", "forbidden_state_roots", c.FS.ForbiddenStateRoots, func(cfg *Config, v any) error {
			ss, err := asStringSlice(v)
			cfg.FS.ForbiddenStateRoots = ss
			return err
		}},
		{"fs.remote_types", "fs", "remote_types", c.FS.RemoteTypes, func(cfg *Config, v any) error { ss, err := asStringSlice(v); cfg.FS.RemoteTypes = ss; return err }},
		{"origin.host_id", "origin", "host_id", c.Origin.HostID, func(cfg *Config, v any) error { s, err := asString(v); cfg.Origin.HostID = s; return err }},
		{"origin.hub_id", "origin", "hub_id", c.Origin.HubID, func(cfg *Config, v any) error { s, err := asString(v); cfg.Origin.HubID = s; return err }},
		{"lifecycle.migrate.downgrade_ok", "lifecycle.migrate", "downgrade_ok", c.Migrate.DowngradeOK, func(cfg *Config, v any) error { b, err := asBool(v); cfg.Migrate.DowngradeOK = b; return err }},
	}
}

// Resolve builds a Resolved config from flags, env, file and defaults.
func Resolve(args []string, env []string, cfgPath string) (Resolved, error) {
	def := defaults()
	known := make(map[string]keyMeta)
	for _, m := range registry(def) {
		known[m.key] = m
	}

	resolved := Resolved{Config: *def}
	values := make(map[string]types.ConfigValue)

	// defaults
	for _, m := range registry(def) {
		values[m.key] = types.ConfigValue{
			Key:       m.key,
			Value:     m.defaultVal,
			Source:    "default",
			SourceRef: "builtin",
			Redacted:  redacted(m.key),
		}
	}

	// file
	if cfgPath == "" {
		cfgPath = def.ConfigPath
	}
	fileVals, tableDocs, err := parseTOMLFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("%w: %v", types.CodeLifecycle001, err)
	}
	// A registered subsystem table absorbs every file key under its name
	// (SPEC-12 §3.1b) before the allowlist check below: `[issues]` and
	// `[skills]` are one resolved value each, and their keys are the owning
	// subsystem's, not this package's.
	if err := absorbTables(fileVals, tableDocs, known); err != nil {
		return Resolved{}, err
	}
	for k, fv := range fileVals {
		if _, ok := known[k]; !ok {
			return Resolved{}, fmt.Errorf("%w: unknown file key %q", types.CodeLifecycle001, k)
		}
		values[k] = types.ConfigValue{
			Key:       k,
			Value:     fv,
			Source:    "file",
			SourceRef: cfgPath,
			Redacted:  redacted(k),
		}
	}

	// env
	rev := make(map[string]string, len(known))
	for k := range known {
		rev[envName(k)] = k
	}
	envMap := parseEnv(env, rev, known, &resolved)
	for k, ev := range envMap {
		values[k] = types.ConfigValue{
			Key:       k,
			Value:     ev,
			Source:    "env",
			SourceRef: envName(k),
			Redacted:  redacted(k),
		}
	}

	// flags
	flagMap, err := parseArgs(args, known)
	if err != nil {
		return Resolved{}, err
	}
	for k, fv := range flagMap {
		values[k] = types.ConfigValue{
			Key:       k,
			Value:     fv,
			Source:    "flag",
			SourceRef: "--" + flagName(k),
			Redacted:  redacted(k),
		}
	}

	// detect conflicts: a lower-precedence source also set the key to a different value
	for k, win := range values {
		if win.Source == "default" {
			continue
		}
		lower := lowerSources(win.Source)
		for _, src := range lower {
			if src == "default" {
				// A compiled default is not a source: it is the absence of one
				// (SPEC-12 §3.1f). A file that sets `ingest.bind` away from the
				// builtin default is ordinary configuration, not a two-source
				// disagreement, so `default` is never a conflict counterparty.
				// The deviation is legible on the ordinary ConfigValue row
				// instead: its `source` names the source that set the key and
				// `trouble config explain` shows the builtin value for any key
				// no source set.
				continue
			}
			var other types.ConfigValue
			switch src {
			case "file":
				if v, ok := fileVals[k]; ok {
					other = types.ConfigValue{Key: k, Value: v, Source: "file", SourceRef: cfgPath}
				}
			case "env":
				if v, ok := envMap[k]; ok {
					other = types.ConfigValue{Key: k, Value: v, Source: "env", SourceRef: envName(k)}
				}
			case "flag":
				if v, ok := flagMap[k]; ok {
					other = types.ConfigValue{Key: k, Value: v, Source: "flag", SourceRef: "--" + flagName(k)}
				}
			}
			if other.Source != "" && !equalValues(win.Value, other.Value) {
				resolved.Conflicts = append(resolved.Conflicts, types.ConfigValue{
					Key:       k,
					Value:     "TROUBLE-LIFECYCLE-002",
					Source:    "conflict",
					SourceRef: fmt.Sprintf("%s vs %s", win.SourceRef, other.SourceRef),
					Redacted:  false,
				})
			}
		}
	}

	// apply values to Config
	for _, m := range registry(&resolved.Config) {
		cv := values[m.key]
		if err := m.setter(&resolved.Config, cv.Value); err != nil {
			return Resolved{}, fmt.Errorf("%w: key %q: %v", types.CodeLifecycle001, m.key, err)
		}
		cv.Value = redactValue(cv)
		resolved.Values = append(resolved.Values, cv)
	}

	// post-resolve path defaults that depend on state_root
	resolved.Config = postResolve(resolved.Config)
	for i := range resolved.Values {
		cv := &resolved.Values[i]
		switch cv.Key {
		case "lifecycle.heartbeat_path":
			cv.Value = resolved.Config.Lifecycle.HeartbeatPath
		case "checker.state_file":
			cv.Value = resolved.Config.Checker.StateFile
		case "checker.alarm_file":
			cv.Value = resolved.Config.Checker.AlarmFile
		case "sensors.rules.dir":
			// Same shape as the two checker paths above: the default is derived
			// from the resolved state root, so the row must carry the derived
			// path (what the sensors package will watch), never the empty
			// marker the registry started from.
			cv.Value = resolved.Config.Sensors.Rules.Dir
		case "ingest.advertised_host":
			// SPEC-12 §3.1g: the registry starts from the empty marker and
			// postResolve derives the loopback name from the resolved bind, so
			// the row carries the DSN host the sentinel will actually bake into
			// every app's DSN — an operator reading `config explain`, or the
			// boot `config` record, never sees a blank where the daemon uses
			// "localhost".
			cv.Value = resolved.Config.Ingest.AdvertisedHost
		case "projects":
			// The `projects` row is the one config value whose SOURCE shape is a
			// table carrying credentials, and the explain dump plus the boot
			// config record are where a value is echoed. It is rendered instead
			// of copied: what was declared, and whether a secret is set, never
			// the secret itself (§3.1).
			cv.Value = resolved.Config.projectsSummary()
		case "issues":
			// Same rule for the two subsystem tables (§3.1b): the row names the
			// keys the declaration carried and never a value, because a value of
			// these tables can be a credential path whose redaction list belongs
			// to the subsystem that owns the table.
			cv.Value = resolved.Config.IssuesTable.Summary()
		case "skills":
			cv.Value = resolved.Config.SkillsTable.Summary()
		case "llm":
			// Same rule as the two subsystem tables: the row names the declared
			// keys and never a value — a base_url or a key_ref is a value.
			cv.Value = resolved.Config.LLMTable.Summary()
		}
	}

	return resolved, nil
}

// lowerSources returns the precedence ladder below src — including `default`,
// which sits at the bottom of the ladder but is not a source: the conflict scan
// skips it, because a value that differs from its compiled default is ordinary
// configuration (SPEC-12 §3.1f).
func lowerSources(src string) []string {
	switch src {
	case "flag":
		return []string{"env", "file", "default"}
	case "env":
		return []string{"file", "default"}
	case "file":
		return []string{"default"}
	}
	return nil
}

func equalValues(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	as, _ := asString(a)
	bs, _ := asString(b)
	return as == bs
}

func redactValue(cv types.ConfigValue) any {
	if cv.Redacted {
		return "[REDACTED:config]"
	}
	return cv.Value
}

func redacted(key string) bool {
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "secrets.") {
		return true
	}
	if strings.HasSuffix(lower, ".token") || strings.HasSuffix(lower, ".secret") || strings.HasSuffix(lower, "_key") {
		return true
	}
	if lower == "dsn.secret" {
		return true
	}
	if strings.HasSuffix(lower, ".public_key") {
		return false
	}
	return false
}

func postResolve(c Config) Config {
	if c.Lifecycle.HeartbeatPath == "" {
		c.Lifecycle.HeartbeatPath = filepath.Join(c.StateRoot, "heartbeat.json")
	}
	// SPEC-03 §4/§3.1e: the rules directory is derived from the RESOLVED state
	// root (internal/sensors does the same: `<state_root>/../config/rules.d`),
	// so an operator who moves the state root moves the watched rules with it.
	// An explicitly configured `sensors.rules.dir` is non-empty here and wins.
	if c.Sensors.Rules.Dir == "" {
		c.Sensors.Rules.Dir = filepath.Join(filepath.Dir(c.StateRoot), "config", "rules.d")
	}
	if c.Checker.StateFile == "" {
		c.Checker.StateFile = filepath.Join(c.StateRoot, "checker.state.json")
	}
	if c.Checker.AlarmFile == "" {
		c.Checker.AlarmFile = filepath.Join(c.StateRoot, "checker.alarm")
	}
	// SPEC-12 §3.1g: with no declared host the DSN host is the loopback name,
	// and that is a value the sentinel ACCEPTS only because this derivation is
	// conditioned on the bind being loopback (SPEC-04 §2.3a). The two halves are
	// one contract: a loopback listener is reachable from this host alone, so
	// every reporter that can reach it resolves "localhost" to it; any other
	// bind leaves the key empty and the §3.2 preflight refuses the boot rather
	// than bake an unreachable host into a DSN.
	if c.Ingest.AdvertisedHost == "" && isLoopbackBind(c.Ingest.Bind) {
		c.Ingest.AdvertisedHost = advertisedHostLoopbackDefault
	}
	return c
}

// isLoopbackBind reports whether the ingest bind is a loopback address. It
// classifies on the same predicate as the §3.2 preflight (isLoopbackHost), so
// the derivation in postResolve and the bind check agree on every spelling:
// 127/8, ::1, and the loopback names, bracketed or bare (splitHostPort leaves
// the brackets on an IPv6 literal).
func isLoopbackBind(bind string) bool {
	host, _, err := splitHostPort(bind)
	if err != nil {
		return false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return isLoopbackHost(host)
}

func envName(key string) string {
	return "TROUBLE_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(key, ".", "_"), "-", "_"))
}

func flagName(key string) string {
	return strings.ReplaceAll(key, ".", "-")
}

func parseEnv(env []string, rev map[string]string, known map[string]keyMeta, r *Resolved) map[string]any {
	out := make(map[string]any)
	for _, e := range env {
		if !strings.HasPrefix(e, "TROUBLE_") {
			continue
		}
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		key, ok := rev[name]
		if !ok {
			key = envToKey(name)
		}
		if m, ok := known[key]; ok {
			out[key] = coerceEnvValue(parts[1], m.defaultVal)
		} else {
			cv := types.ConfigValue{Key: key, Value: parts[1], Source: "env_unknown", SourceRef: name}
			if hint := nearestKey(key, known); hint != "" {
				cv.Value = map[string]any{"value": parts[1], "hint": hint}
			}
			r.UnknownEnv = append(r.UnknownEnv, cv)
		}
	}
	return out
}

func envToKey(name string) string {
	s := strings.TrimPrefix(name, "TROUBLE_")
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, "_", ".")
}

func coerceEnvValue(s string, def any) any {
	switch def.(type) {
	case bool:
		b, _ := strconv.ParseBool(s)
		return b
	case int, int64:
		i, _ := strconv.ParseInt(s, 10, 64)
		return i
	case []string, [][]string:
		return strings.Split(s, ",")
	}
	return s
}

func nearestKey(key string, known map[string]keyMeta) string {
	best := ""
	bestd := 3
	for k := range known {
		d := editDistance(key, k)
		if d < bestd {
			bestd = d
			best = k
		}
	}
	if bestd <= 2 {
		return best
	}
	return ""
}

func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func parseArgs(args []string, known map[string]keyMeta) (map[string]any, error) {
	out := make(map[string]any)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		key := strings.TrimPrefix(arg, "--")
		key = strings.ReplaceAll(key, "-", ".")
		var val string
		if strings.Contains(key, "=") {
			parts := strings.SplitN(key, "=", 2)
			key = parts[0]
			val = parts[1]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			val = args[i+1]
			i++
		} else {
			val = "true"
		}
		if _, ok := known[key]; !ok {
			return nil, fmt.Errorf("%w: unknown flag %q", types.CodeLifecycle001, arg)
		}
		out[key] = val
	}
	return out, nil
}

// parseTOMLFile is a minimal TOML subset reader: sections, key = value,
// strings, ints, bools, arrays of strings, and arrays of tables (`[[name]]`).
//
// An array of tables parses into []map[string]any: every `[[name]]` header
// starts a NEW row which the keys after it fill. That is the shape the
// `[[projects]]` declarations of SPEC-12 §3.1a arrive in.
//
// The second return value holds, per ROOT table name, the verbatim text of every
// line that declares that root, header lines included. A root table whose name is
// a registered key (SPEC-12 §3.1b) is handed to its owner as exactly that text,
// so the owner's own decoder reads the file's own bytes: no re-serialization, and
// no key of a subsystem table is flattened into this package's allowlist.
func parseTOMLFile(path string) (map[string]any, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	vals := make(map[string]any)
	docs := make(map[string]string)
	// seen tracks the `[name]` headers, because TOML forbids declaring a table
	// twice — and forbids reopening one after its sub-table was declared. The
	// subset reader would otherwise fold both declarations into one document and
	// let the SUBSYSTEM refuse the operator's file with a line number from a
	// fragment; naming the repeated table here is the §3.1 answer (the file is
	// not parseable, and the refusal names the key).
	seen := make(map[string]bool)
	section := ""
	arraySection := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			name := strings.Trim(line, "[]")
			rows, _ := vals[name].([]map[string]any)
			vals[name] = append(rows, map[string]any{})
			section, arraySection = "", name
			appendTableDoc(docs, sectionRoot(name), line)
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.Trim(line, "[]")
			if prev, dup := repeatedTable(seen, name); dup {
				return nil, nil, fmt.Errorf("%s: table [%s] is declared after [%s]: a table is declared once", path, name, prev)
			}
			seen[name] = true
			section = name
			arraySection = ""
			appendTableDoc(docs, sectionRoot(section), line)
			continue
		}
		if !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])
		if arraySection != "" {
			rows, _ := vals[arraySection].([]map[string]any)
			if len(rows) == 0 {
				continue // unreachable: the header above always appends a row
			}
			row := rows[len(rows)-1]
			v, err := parseTOMLValue(valStr)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: key %q: %w", path, arraySection+"."+key, err)
			}
			row[key] = v
			appendTableDoc(docs, sectionRoot(arraySection), line)
			continue
		}
		root := ""
		if section != "" {
			key = section + "." + key
			root = sectionRoot(section)
		} else {
			// A root-level dotted key (`issues.enabled = true`) declares the
			// table `issues` exactly as the `[issues]` header does; a plain root
			// key (`state_root = ...`) belongs to no table.
			root = dottedRoot(key)
		}
		v, err := parseTOMLValue(valStr)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: key %q: %w", path, key, err)
		}
		vals[key] = v
		appendTableDoc(docs, root, line)
	}
	return vals, docs, sc.Err()
}

// sectionRoot is the root table of a section header: the first component of its
// name — `issues` for both `[issues]` and `[issues.drivers.github]`.
func sectionRoot(name string) string {
	root, _, _ := strings.Cut(name, ".")
	return root
}

// repeatedTable reports whether a `[name]` header repeats one already declared,
// directly or as a sub-table of it: TOML declares a table once, and a table may
// not be (re)opened after one of its sub-tables exists. It returns the earlier
// header to name in the refusal.
func repeatedTable(seen map[string]bool, name string) (string, bool) {
	for prev := range seen {
		if prev == name || strings.HasPrefix(prev, name+".") {
			return prev, true
		}
	}
	return "", false
}

// dottedRoot is the root table a root-level key declares, when it declares one:
// a dotted key names a table, a plain key does not.
func dottedRoot(key string) string {
	root, rest, ok := strings.Cut(key, ".")
	if !ok || root == "" || rest == "" {
		return ""
	}
	return root
}

// appendTableDoc accumulates one root table's verbatim text, in file order.
func appendTableDoc(docs map[string]string, root, line string) {
	if root == "" {
		return
	}
	docs[root] += line + "\n"
}

func parseTOMLValue(s string) (any, error) {
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}, nil
		}
		var out []any
		for _, part := range splitArray(inner) {
			v, err := parseTOMLValue(strings.TrimSpace(part))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	if (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) ||
		(strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`)) {
		return unquote(s), nil
	}
	if s == "true" {
		return true, nil
	}
	if s == "false" {
		return false, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	// Bare string (TOML allows bare keys but we accept bare values too for simple strings)
	return s, nil
}

func splitArray(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	quoteChar := rune(0)
	for _, r := range s {
		if inQuote {
			cur.WriteRune(r)
			if r == quoteChar {
				inQuote = false
			}
			continue
		}
		if r == '"' || r == '\'' {
			inQuote = true
			quoteChar = r
			cur.WriteRune(r)
			continue
		}
		if r == ',' {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if s[len(s)-1] != q {
		return s
	}
	return strings.ReplaceAll(s[1:len(s)-1], "\\"+string(q), string(q))
}

func splitHostPort(s string) (string, string, error) {
	if s == "" {
		return "", "", fmt.Errorf("empty bind")
	}
	i := strings.LastIndex(s, ":")
	if i < 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("missing port in %q", s)
	}
	return s[:i], s[i+1:], nil
}
