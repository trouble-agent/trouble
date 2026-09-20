package issues

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/trouble-agent/trouble/internal/types"
)

// DefaultConfig is the SPEC-09 §3.4 default column. Every key has a default and
// no deployment value is compiled in.
//
// `Enabled` is FALSE in the shipped default (SPEC-09 §3.4a): no deployment value
// is compiled in, so no driver this build can construct is usable out of the box
// — the github block has no owner/repo and no token, and the local duckbrain
// driver is off. An enabled desk over those blocks is refused at boot
// (`TROUBLE-ISSUES-003: driver github needs owner and repo (SPEC-09 §3.9.1)`),
// which is the right answer for a desk that was ASKED for and cannot be built,
// and the wrong one for every stock boot that never asked. So the desk is off,
// deliberately and visibly.
//
// The opt-in is one of these two, in the `[issues]` table:
//
//	[issues]
//	enabled = true
//	[issues.drivers.github]
//	owner = "<org>"        # both required, plus a token (env or 0600 file)
//	repo  = "<repo>"
//
// or, local-first, `enabled = true` with `[issues.drivers.duckbrain] enabled =
// true` and a reachable base_url. The driver blocks below are left exactly as
// §3.4 ships them, so ENABLING the desk without the owner/repo pair still fails
// with that named message instead of building a desk that cannot file.
func DefaultConfig() types.IssueDeskConfig {
	return types.IssueDeskConfig{
		Enabled:           false,
		PrimaryDriver:     "github",
		DedupWindow:       "30m",
		QuietClose:        "24h",
		QuietCloseSweep:   "5m",
		OpDeadline:        "45s",
		BodyMaxBytes:      60000,
		TitleMaxChars:     256,
		ReplayInterval:    "5s",
		ReplayBatch:       100,
		HealthcheckEvery:  "60s",
		HealthcheckIdle:   "5m",
		FailAfterProbes:   2,
		SpoolBudgetBytes:  67108864,
		SpoolMaxEntries:   20000,
		SpoolTTL:          "72h",
		SpoolMinRetention: "1h",
		MaxAttemptsPerOp:  20,
		AckDefault:        "24h",
		Caps: types.IssueCaps{
			PerSigCreates:       1,
			PerSigWindow:        "24h",
			PerSigComments:      6,
			CommentMinInterval:  "10m",
			PerProjectCreatesH:  20,
			PerProjectCreatesD:  200,
			PerProjectCommentsH: 200,
			GlobalCreatesH:      100,
			GlobalCreatesD:      1000,
			GlobalCommentsH:     600,
		},
		Drivers: []types.IssueDriverConfig{DefaultGitHubConfig(), DefaultDuckbrainConfig()},
	}
}

// DefaultGitHubConfig is the §3.4 github block (owner/repo empty: the operator
// must set them, or the driver is refused at boot per §3.9.1).
func DefaultGitHubConfig() types.IssueDriverConfig {
	return types.IssueDriverConfig{
		Name:              "github",
		Enabled:           true,
		Primary:           true,
		DedupWindow:       "", // inherits the desk window
		MaxAttempts:       5,
		BaseBackoff:       "1s",
		MaxBackoff:        "60s",
		Jitter:            0.25,
		Timeout:           "15s",
		MinRemaining:      100,
		SearchMinInterval: "6s",
		APIBase:           "https://api.github.com",
		TokenFile:         "~/.config/trouble/github.token",
		TokenEnv:          "TROUBLE_GITHUB_TOKEN",
		Labels:            []string{"trouble", "auto-filed"},
		LabelsExtra:       []string{},
		SeverityLabels: map[string]string{
			"critical": "sev:critical", "high": "sev:high", "medium": "sev:medium",
			"low": "sev:low", "info": "sev:info",
		},
		SourceLabels: true,
	}
}

// DefaultDuckbrainConfig is the §3.4 duckbrain block, disabled by default: it is
// the local-first backend and is enabled when one is reachable.
func DefaultDuckbrainConfig() types.IssueDriverConfig {
	return types.IssueDriverConfig{
		Name:           "duckbrain",
		Enabled:        false,
		MaxAttempts:    5,
		BaseBackoff:    "500ms",
		MaxBackoff:     "5s",
		Jitter:         0.25,
		Timeout:        "10s",
		BaseURL:        "http://127.0.0.1:7645",
		APIKeyFile:     "~/.config/trouble/duckbrain.key",
		APIKeyEnv:      "TROUBLE_DUCKBRAIN_KEY",
		APIKeyHeader:   "Authorization",
		APIKeyScheme:   "Bearer",
		NSPrefix:       "trouble.issues",
		Labels:         []string{"trouble", "auto-filed"},
		LabelsExtra:    []string{},
		SourceLabels:   true,
		SeverityLabels: map[string]string{"high": "sev:high", "critical": "sev:critical"},
	}
}

// deskWire is the TOML shape of the `[issues]` table (§3.4). It exists because
// the frozen config type carries `Drivers []IssueDriverConfig`, which is not the
// shape a TOML document has: the file names drivers as sub-tables.
type deskWire struct {
	Enabled           *bool                 `toml:"enabled"`
	PrimaryDriver     string                `toml:"primary_driver"`
	DedupWindow       string                `toml:"dedup_window"`
	QuietClose        string                `toml:"quiet_close"`
	QuietCloseSweep   string                `toml:"quiet_close_sweep"`
	OpDeadline        string                `toml:"op_deadline"`
	BodyMaxBytes      int                   `toml:"body_max_bytes"`
	TitleMaxChars     int                   `toml:"title_max_chars"`
	ReplayInterval    string                `toml:"replay_interval"`
	ReplayBatch       int                   `toml:"replay_batch"`
	HealthcheckEvery  string                `toml:"healthcheck_interval"`
	HealthcheckIdle   string                `toml:"healthcheck_idle"`
	FailAfterProbes   int                   `toml:"fail_after_probes"`
	SpoolBudgetBytes  int64                 `toml:"spool_budget_bytes"`
	SpoolMaxEntries   int                   `toml:"spool_max_entries"`
	SpoolTTL          string                `toml:"spool_ttl"`
	SpoolMinRetention string                `toml:"spool_min_retention"`
	MaxAttemptsPerOp  int                   `toml:"max_attempts_per_op"`
	AckDefault        string                `toml:"ack_default"`
	Caps              *capsWire             `toml:"caps"`
	Drivers           map[string]driverWire `toml:"drivers"`
}

type capsWire struct {
	PerSigCreates       *int   `toml:"per_sig_creates"`
	PerSigWindow        string `toml:"per_sig_window"`
	PerSigComments      *int   `toml:"per_sig_comments"`
	CommentMinInterval  string `toml:"comment_min_interval"`
	PerProjectCreatesH  *int   `toml:"per_project_creates_h"`
	PerProjectCreatesD  *int   `toml:"per_project_creates_d"`
	PerProjectCommentsH *int   `toml:"per_project_comments_h"`
	GlobalCreatesH      *int   `toml:"global_creates_h"`
	GlobalCreatesD      *int   `toml:"global_creates_d"`
	GlobalCommentsH     *int   `toml:"global_comments_h"`
}

type driverWire struct {
	Enabled           *bool             `toml:"enabled"`
	Primary           *bool             `toml:"primary"`
	Mirror            *bool             `toml:"mirror"`
	DedupWindow       string            `toml:"dedup_window"`
	MaxAttempts       *int              `toml:"max_attempts"`
	BaseBackoff       string            `toml:"base_backoff"`
	MaxBackoff        string            `toml:"max_backoff"`
	Jitter            *float64          `toml:"jitter"`
	Timeout           string            `toml:"timeout"`
	MinRemaining      *int              `toml:"min_remaining"`
	SearchMinInterval string            `toml:"search_min_interval"`
	Owner             string            `toml:"owner"`
	Repo              string            `toml:"repo"`
	APIBase           string            `toml:"api_base"`
	TokenFile         string            `toml:"token_file"`
	TokenEnv          string            `toml:"token_env"`
	Labels            []string          `toml:"labels"`
	LabelsExtra       []string          `toml:"labels_extra"`
	SeverityLabels    map[string]string `toml:"severity_labels"`
	SourceLabels      *bool             `toml:"source_labels"`
	BaseURL           string            `toml:"base_url"`
	APIKeyFile        string            `toml:"api_key_file"`
	APIKeyEnv         string            `toml:"api_key_env"`
	APIKeyHeader      string            `toml:"api_key_header"`
	APIKeyScheme      string            `toml:"api_key_scheme"`
	NSPrefix          string            `toml:"ns_prefix"`
}

// wireDoc is the document the loader accepts: `[issues]` (optionally plus the
// `[issues.drivers.*]` sub-tables, which TOML renders inside it).
type wireDoc struct {
	Issues *deskWire `toml:"issues"`
}

// undecodedKeys lists, sorted, the keys a decoded document carried that no field
// of the wire shape claimed: exactly the unknown keys a strict config decode
// refuses, refused by NAME so the operator is told which one is wrong.
func undecodedKeys(md toml.MetaData) []string {
	bad := md.Undecoded()
	if len(bad) == 0 {
		return nil
	}
	out := make([]string, 0, len(bad))
	for _, k := range bad {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}

// LoadConfig decodes the `[issues]` table over the defaults of §3.4 and applies
// the §3.4/§3.8 clamps. A zero-length document yields the defaults.
func LoadConfig(doc []byte) (types.IssueDeskConfig, error) {
	cfg := DefaultConfig()
	if len(doc) == 0 {
		return cfg, ValidateConfig(cfg)
	}
	// The strict decode: a key this build does not know is REFUSED by name, never
	// decoded past. SPEC-12 §3.1's rule for the config file ("a typo must not
	// silently run with a default") reaches this table too — the table's keys are
	// the desk's own surface (SPEC-09 §3.4), and the composition root hands the
	// file's bytes straight to this loader (SPEC-12 §3.1b), so an unknown key has
	// no other place to be caught.
	var w wireDoc
	md, err := toml.Decode(string(doc), &w)
	if err != nil {
		return cfg, newErr(types.CodeIssues003, ReasonConfig, 0, false, "issues config: %v", err)
	}
	if bad := undecodedKeys(md); len(bad) > 0 {
		return cfg, newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"issues config: unknown key %q (known keys are SPEC-09 §3.4's)", bad[0])
	}
	if w.Issues == nil {
		return cfg, ValidateConfig(cfg)
	}
	iw := w.Issues
	if iw.Enabled != nil {
		cfg.Enabled = *iw.Enabled
	}
	if iw.PrimaryDriver != "" {
		cfg.PrimaryDriver = iw.PrimaryDriver
	}
	assign := func(dst *types.Duration, src string) {
		if src != "" {
			*dst = types.Duration(src)
		}
	}
	assign(&cfg.DedupWindow, iw.DedupWindow)
	assign(&cfg.QuietClose, iw.QuietClose)
	assign(&cfg.QuietCloseSweep, iw.QuietCloseSweep)
	assign(&cfg.OpDeadline, iw.OpDeadline)
	assign(&cfg.ReplayInterval, iw.ReplayInterval)
	assign(&cfg.HealthcheckEvery, iw.HealthcheckEvery)
	assign(&cfg.HealthcheckIdle, iw.HealthcheckIdle)
	assign(&cfg.SpoolTTL, iw.SpoolTTL)
	assign(&cfg.SpoolMinRetention, iw.SpoolMinRetention)
	assign(&cfg.AckDefault, iw.AckDefault)
	if iw.BodyMaxBytes > 0 {
		cfg.BodyMaxBytes = iw.BodyMaxBytes
	}
	if iw.TitleMaxChars > 0 {
		cfg.TitleMaxChars = iw.TitleMaxChars
	}
	if iw.ReplayBatch > 0 {
		cfg.ReplayBatch = iw.ReplayBatch
	}
	if iw.FailAfterProbes > 0 {
		cfg.FailAfterProbes = iw.FailAfterProbes
	}
	if iw.SpoolBudgetBytes > 0 {
		cfg.SpoolBudgetBytes = iw.SpoolBudgetBytes
	}
	if iw.SpoolMaxEntries > 0 {
		cfg.SpoolMaxEntries = iw.SpoolMaxEntries
	}
	if iw.MaxAttemptsPerOp > 0 {
		cfg.MaxAttemptsPerOp = iw.MaxAttemptsPerOp
	}
	if iw.Caps != nil {
		c := iw.Caps
		if c.PerSigCreates != nil {
			cfg.Caps.PerSigCreates = *c.PerSigCreates
		}
		if c.PerSigComments != nil {
			cfg.Caps.PerSigComments = *c.PerSigComments
		}
		if c.PerProjectCreatesH != nil {
			cfg.Caps.PerProjectCreatesH = *c.PerProjectCreatesH
		}
		if c.PerProjectCreatesD != nil {
			cfg.Caps.PerProjectCreatesD = *c.PerProjectCreatesD
		}
		if c.PerProjectCommentsH != nil {
			cfg.Caps.PerProjectCommentsH = *c.PerProjectCommentsH
		}
		if c.GlobalCreatesH != nil {
			cfg.Caps.GlobalCreatesH = *c.GlobalCreatesH
		}
		if c.GlobalCreatesD != nil {
			cfg.Caps.GlobalCreatesD = *c.GlobalCreatesD
		}
		if c.GlobalCommentsH != nil {
			cfg.Caps.GlobalCommentsH = *c.GlobalCommentsH
		}
		assign(&cfg.Caps.PerSigWindow, c.PerSigWindow)
		assign(&cfg.Caps.CommentMinInterval, c.CommentMinInterval)
	}
	// The driver sub-tables replace the matching default block; an unknown driver
	// name is added as-is and rejected by ValidateConfig (SPEC-12 §5 owns the
	// message, this package owns the set of names it can build).
	for name, dw := range iw.Drivers {
		base, ok := driverByName(cfg.Drivers, name)
		if !ok {
			base = types.IssueDriverConfig{Name: name}
			// Sensible neutral defaults so an unknown driver's block is decoded
			// and then refused by name, not by a zeroed struct.
			base.MaxAttempts = 5
			base.BaseBackoff = "1s"
			base.MaxBackoff = "60s"
			base.Jitter = 0.25
			base.Timeout = "15s"
		}
		d := applyDriverWire(base, dw)
		cfg.Drivers = upsertDriver(cfg.Drivers, d)
	}
	return cfg, ValidateConfig(cfg)
}

func applyDriverWire(base types.IssueDriverConfig, w driverWire) types.IssueDriverConfig {
	d := base
	if w.Enabled != nil {
		d.Enabled = *w.Enabled
	}
	if w.Primary != nil {
		d.Primary = *w.Primary
	}
	if w.Mirror != nil {
		d.Mirror = *w.Mirror
	}
	if w.DedupWindow != "" {
		d.DedupWindow = types.Duration(w.DedupWindow)
	}
	if w.MaxAttempts != nil {
		d.MaxAttempts = *w.MaxAttempts
	}
	if w.BaseBackoff != "" {
		d.BaseBackoff = types.Duration(w.BaseBackoff)
	}
	if w.MaxBackoff != "" {
		d.MaxBackoff = types.Duration(w.MaxBackoff)
	}
	if w.Jitter != nil {
		d.Jitter = *w.Jitter
	}
	if w.Timeout != "" {
		d.Timeout = types.Duration(w.Timeout)
	}
	if w.MinRemaining != nil {
		d.MinRemaining = *w.MinRemaining
	}
	if w.SearchMinInterval != "" {
		d.SearchMinInterval = types.Duration(w.SearchMinInterval)
	}
	if w.Owner != "" {
		d.Owner = w.Owner
	}
	if w.Repo != "" {
		d.Repo = w.Repo
	}
	if w.APIBase != "" {
		d.APIBase = w.APIBase
	}
	if w.TokenFile != "" {
		d.TokenFile = w.TokenFile
	}
	if w.TokenEnv != "" {
		d.TokenEnv = w.TokenEnv
	}
	if w.Labels != nil {
		d.Labels = w.Labels
	}
	if w.LabelsExtra != nil {
		d.LabelsExtra = w.LabelsExtra
	}
	if w.SeverityLabels != nil {
		d.SeverityLabels = w.SeverityLabels
	}
	if w.SourceLabels != nil {
		d.SourceLabels = *w.SourceLabels
	}
	if w.BaseURL != "" {
		d.BaseURL = w.BaseURL
	}
	if w.APIKeyFile != "" {
		d.APIKeyFile = w.APIKeyFile
	}
	if w.APIKeyEnv != "" {
		d.APIKeyEnv = w.APIKeyEnv
	}
	if w.APIKeyHeader != "" {
		d.APIKeyHeader = w.APIKeyHeader
	}
	if w.APIKeyScheme != "" {
		d.APIKeyScheme = w.APIKeyScheme
	}
	if w.NSPrefix != "" {
		d.NSPrefix = w.NSPrefix
	}
	return d
}

func driverByName(ds []types.IssueDriverConfig, name string) (types.IssueDriverConfig, bool) {
	for _, d := range ds {
		if d.Name == name {
			return d, true
		}
	}
	return types.IssueDriverConfig{}, false
}

func upsertDriver(ds []types.IssueDriverConfig, d types.IssueDriverConfig) []types.IssueDriverConfig {
	for i := range ds {
		if ds[i].Name == d.Name {
			ds[i] = d
			return ds
		}
	}
	return append(ds, d)
}

// ValidateConfig applies every §3.4/§3.8 clamp and every boot rejection this
// package owns: a driver whose name this build cannot build, a primary driver
// that is not enabled, a github block with no owner/repo, an out-of-range
// window, or a negative cap.
func ValidateConfig(cfg types.IssueDeskConfig) error {
	if !cfg.Enabled {
		return nil
	}
	dw := cfg.DedupWindow.Std()
	if dw == 0 {
		dw = 30 * time.Minute
	}
	if dw < time.Minute || dw > 24*time.Hour {
		return newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"dedup_window %q is outside the clamp 1m..24h", cfg.DedupWindow)
	}
	if cfg.Caps.PerSigComments < 0 || cfg.Caps.PerSigCreates < 0 ||
		cfg.Caps.PerProjectCreatesH < 0 || cfg.Caps.PerProjectCreatesD < 0 ||
		cfg.Caps.GlobalCreatesH < 0 || cfg.Caps.GlobalCreatesD < 0 {
		return newErr(types.CodeIssues003, ReasonConfig, 0, false, "caps may not be negative")
	}
	if len(cfg.Drivers) == 0 {
		return newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"no driver block is configured: declare [issues.drivers.<name>] enabled = true (this build knows: %s)",
			strings.Join(Registered(), ", "))
	}
	enabled := 0
	primaryOK := false
	for _, d := range cfg.Drivers {
		if _, ok := factoryFor(d.Name); !ok {
			return newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false,
				"driver %q is not registered in this build (known: %s)", d.Name, strings.Join(Registered(), ", "))
		}
		if !d.Enabled {
			continue
		}
		enabled++
		if d.Name == cfg.PrimaryDriver {
			primaryOK = true
		}
		if d.Name == "github" && (d.Owner == "" || d.Repo == "") {
			return newErr(types.CodeIssues003, ReasonConfig, 0, false,
				"driver github needs owner and repo (SPEC-09 §3.9.1)")
		}
		if dd := d.DedupWindow.Std(); d.DedupWindow != "" && (dd < time.Minute || dd > 24*time.Hour) {
			return newErr(types.CodeIssues003, ReasonConfig, 0, false,
				"driver %s dedup_window %q is outside the clamp 1m..24h", d.Name, d.DedupWindow)
		}
	}
	if enabled == 0 {
		return newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"no enabled driver: set enabled = true in one [issues.drivers.<name>] block (this build knows: %s)",
			strings.Join(Registered(), ", "))
	}
	if !primaryOK {
		return newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"primary_driver %q does not name an enabled driver", cfg.PrimaryDriver)
	}
	return nil
}

// TokenResolution is the outcome of the flag > env > file resolution order.
type TokenResolution struct {
	Value  string
	Source string // flag | env | file
	File   string
}

// ResolveToken resolves a credential in the §3.9.1 order. A token passed on argv
// is refused with TROUBLE-ISSUES-003 and never used: argv is world-readable.
func ResolveToken(flagValue string, cfg types.IssueDriverConfig, fileKey, envKey string) (TokenResolution, error) {
	if flagValue != "" {
		return TokenResolution{}, newErr(types.CodeIssues003, ReasonTokenArgv, 0, false,
			"a token must never be passed on argv (--issues-%s-token)", cfg.Name)
	}
	if v := os.Getenv(envKey); envKey != "" && v != "" {
		return TokenResolution{Value: v, Source: "env"}, nil
	}
	path := fileKey
	if path == "" {
		return TokenResolution{}, newErr(types.CodeIssues003, ReasonValidation, 0, false,
			"driver %s has no credential path configured", cfg.Name)
	}
	exp, err := ExpandPath(path)
	if err != nil {
		return TokenResolution{}, newErr(types.CodeIssues003, ReasonValidation, 0, false, "driver %s: %v", cfg.Name, err)
	}
	value, err := ReadSecretFile(exp)
	if err != nil {
		return TokenResolution{}, err
	}
	return TokenResolution{Value: value, Source: "file", File: exp}, nil
}

// ExpandPath expands a leading ~ (or ~user) and cleans the path.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "~") {
		rest := strings.TrimPrefix(p, "~")
		home := ""
		if rest == "" || strings.HasPrefix(rest, "/") {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			home = h
			rest = strings.TrimPrefix(rest, "/")
		} else {
			name := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				name, rest = rest[:i], rest[i+1:]
			} else {
				rest = ""
			}
			u, err := user.Lookup(name)
			if err != nil {
				return "", fmt.Errorf("unknown user in path %q: %w", p, err)
			}
			home = u.HomeDir
		}
		p = filepath.Join(home, rest)
	}
	return filepath.Clean(p), nil
}

// ReadSecretFile reads a credential file under the §3.9.1 rules: a regular file
// (no symlink), mode 0600, owned by the daemon uid. Anything else is
// TROUBLE-ISSUES-003 and the driver is marked failed with no outbound call.
func ReadSecretFile(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", newErr(types.CodeIssues003, ReasonValidation, 0, false, "credential file %s: %v", path, err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", newErr(types.CodeIssues003, ReasonTokenMode, 0, false,
			"credential file %s is a symlink; the mode of the target cannot be trusted", path)
	}
	if !st.Mode().IsRegular() {
		return "", newErr(types.CodeIssues003, ReasonTokenMode, 0, false,
			"credential file %s is not a regular file", path)
	}
	if st.Mode().Perm() != 0o600 {
		return "", newErr(types.CodeIssues003, ReasonTokenMode, 0, false,
			"credential file %s has mode %#o, want 0600", path, st.Mode().Perm())
	}
	if err := checkOwner(st); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", newErr(types.CodeIssues003, ReasonValidation, 0, false, "credential file %s: %v", path, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", newErr(types.CodeIssues003, ReasonUnauthorized, 0, false,
			"credential file %s is empty", path)
	}
	return v, nil
}

// checkOwner enforces "owned by the daemon uid" (§3.9.1). A file owned by another
// uid is refused: another user could swap its contents at any time.
func checkOwner(st os.FileInfo) error {
	uid := os.Getuid()
	owner, ok := platOwnerUID(st)
	if !ok {
		// NOT APPLICABLE on this platform: there is no POSIX uid, so the
		// daemon-uid ownership check cannot be expressed (Windows uses ACLs).
		// This is a KNOWN GAP, stated rather than hidden — see SPEC-12 §3.5a.
		return nil
	}
	if owner != uid {
		return newErr(types.CodeIssues003, ReasonTokenMode, 0, false,
			"credential file is owned by uid %d, not the daemon uid %d", owner, uid)
	}
	return nil
}

// ConfigLine is one `trouble config explain` row (SPEC-09 §3.4). SPEC-12 owns the
// CLI and its own ConfigValue; this is the desk's contribution to it, with the
// redaction flag the spec requires.
type ConfigLine struct {
	Key       string
	Value     string
	Source    string
	SourceRef string
	Redacted  bool
}

// Explain renders the desk's config surface. Credential paths are printed with
// Redacted=true, and a header is printed as its NAME only: no key value is ever
// printed, logged, or placed in the ledger (SPEC-02 mandatory rules).
func Explain(cfg types.IssueDeskConfig) []ConfigLine {
	out := []ConfigLine{
		{Key: "issues.enabled", Value: strconv.FormatBool(cfg.Enabled), Source: "default", SourceRef: "builtin"},
		{Key: "issues.primary_driver", Value: cfg.PrimaryDriver, Source: "default", SourceRef: "builtin"},
		{Key: "issues.dedup_window", Value: string(cfg.DedupWindow), Source: "default", SourceRef: "builtin"},
		{Key: "issues.quiet_close", Value: string(cfg.QuietClose), Source: "default", SourceRef: "builtin"},
		{Key: "issues.op_deadline", Value: string(cfg.OpDeadline), Source: "default", SourceRef: "builtin"},
		{Key: "issues.body_max_bytes", Value: strconv.Itoa(cfg.BodyMaxBytes), Source: "default", SourceRef: "builtin"},
	}
	for _, d := range cfg.Drivers {
		prefix := "issues.drivers." + d.Name + "."
		out = append(out, ConfigLine{Key: prefix + "enabled", Value: strconv.FormatBool(d.Enabled), Source: "default", SourceRef: "builtin"})
		if d.Name == "github" {
			out = append(out,
				ConfigLine{Key: prefix + "owner", Value: d.Owner, Source: "default", SourceRef: "builtin"},
				ConfigLine{Key: prefix + "repo", Value: d.Repo, Source: "default", SourceRef: "builtin"},
				ConfigLine{Key: prefix + "token_file", Value: d.TokenFile, Source: "default", SourceRef: "builtin", Redacted: true},
				ConfigLine{Key: prefix + "token_env", Value: d.TokenEnv, Source: "default", SourceRef: "builtin"},
			)
		}
		if d.Name == "duckbrain" {
			out = append(out,
				ConfigLine{Key: prefix + "base_url", Value: d.BaseURL, Source: "default", SourceRef: "builtin"},
				ConfigLine{Key: prefix + "api_key_file", Value: d.APIKeyFile, Source: "default", SourceRef: "builtin", Redacted: true},
				// The header NAME is configuration; its value never appears.
				ConfigLine{Key: prefix + "api_key_header", Value: d.APIKeyHeader, Source: "default", SourceRef: "builtin"},
				ConfigLine{Key: prefix + "ns_prefix", Value: d.NSPrefix, Source: "default", SourceRef: "builtin"},
			)
		}
	}
	return out
}
