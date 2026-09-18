package lifecycle

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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

	Dashboard DashboardConfig `toml:"dashboard"`

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

	Spool struct {
		BudgetBytes     int64  `toml:"budget_bytes"`
		GapReserveBytes int64  `toml:"gap_reserve_bytes"`
		Fsync           string `toml:"fsync"`
		FsyncWindowMS   int    `toml:"fsync_window_ms"`
	} `toml:"spool"`

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

// Resolved is the output of Resolve: the typed config, every ConfigValue with
// provenance, plus the non-fatal records that must be mirrored into the ledger.
type Resolved struct {
	Config     Config
	Values     []types.ConfigValue
	Conflicts  []types.ConfigValue // one row per conflict (code 002)
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
	c.Spool.BudgetBytes = 268435456
	c.Spool.GapReserveBytes = 2097152
	c.Spool.Fsync = "group"
	c.Spool.FsyncWindowMS = 200
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
		{"spool.budget_bytes", "spool", "budget_bytes", c.Spool.BudgetBytes, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Spool.BudgetBytes = i; return err }},
		{"spool.gap_reserve_bytes", "spool", "gap_reserve_bytes", c.Spool.GapReserveBytes, func(cfg *Config, v any) error { i, err := asInt64(v); cfg.Spool.GapReserveBytes = i; return err }},
		{"spool.fsync", "spool", "fsync", c.Spool.Fsync, func(cfg *Config, v any) error { s, err := asString(v); cfg.Spool.Fsync = s; return err }},
		{"spool.fsync_window_ms", "spool", "fsync_window_ms", c.Spool.FsyncWindowMS, func(cfg *Config, v any) error { i, err := asInt(v); cfg.Spool.FsyncWindowMS = i; return err }},
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
	fileVals, err := parseTOMLFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("%w: %v", types.CodeLifecycle001, err)
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
				if !equalValues(win.Value, known[k].defaultVal) {
					resolved.Conflicts = append(resolved.Conflicts, types.ConfigValue{
						Key:       k,
						Value:     "TROUBLE-LIFECYCLE-002",
						Source:    "conflict",
						SourceRef: fmt.Sprintf("%s vs default", win.SourceRef),
						Redacted:  false,
					})
				}
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
		case "projects":
			// The `projects` row is the one config value whose SOURCE shape is a
			// table carrying credentials, and the explain dump plus the boot
			// config record are where a value is echoed. It is rendered instead
			// of copied: what was declared, and whether a secret is set, never
			// the secret itself (§3.1).
			cv.Value = resolved.Config.projectsSummary()
		}
	}

	return resolved, nil
}

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
	if c.Checker.StateFile == "" {
		c.Checker.StateFile = filepath.Join(c.StateRoot, "checker.state.json")
	}
	if c.Checker.AlarmFile == "" {
		c.Checker.AlarmFile = filepath.Join(c.StateRoot, "checker.alarm")
	}
	if c.Ingest.AdvertisedHost == "" && isLoopbackBind(c.Ingest.Bind) {
		c.Ingest.AdvertisedHost = "localhost"
	}
	return c
}

func isLoopbackBind(bind string) bool {
	host, _, err := splitHostPort(bind)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
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
func parseTOMLFile(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := make(map[string]any)
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
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			arraySection = ""
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
				return nil, fmt.Errorf("%s: key %q: %w", path, arraySection+"."+key, err)
			}
			row[key] = v
			continue
		}
		if section != "" {
			key = section + "." + key
		}
		v, err := parseTOMLValue(valStr)
		if err != nil {
			return nil, fmt.Errorf("%s: key %q: %w", path, key, err)
		}
		vals[key] = v
	}
	return vals, sc.Err()
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
