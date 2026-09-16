package skills

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// DefaultConfig is the §2 default column: every default is safe, and
// `enabled=false` means the daemon pulls nothing and no skill is ever applied.
func DefaultConfig() types.SkillsConfig {
	return types.SkillsConfig{
		Enabled:             true,
		SourceRef:           "v*",
		RefMode:             "tag",
		PullInterval:        "15m",
		PullJitterPct:       10,
		PullOnBoot:          true,
		PullTimeout:         "2m",
		PullMaxBytes:        67108864,
		RequireSignature:    true,
		Approve:             "review",
		CanaryValidity:      "7d",
		MaxHold:             "30m",
		DemoteAfterFailures: 2,
		RetainVersions:      2,
		GitBinary:           "git",
		AutoAcceptModules:   []string{"proc.connections", "proc.top"},
	}
}

// durationOrDays parses a duration that may carry a day unit: `time.ParseDuration`
// has no "d", and SPEC-11's own example writes `canary_validity = "7d"`. A day is
// 24 h and nothing else is special-cased.
func durationOrDays(d types.Duration) time.Duration {
	s := strings.TrimSpace(string(d))
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return d.Std()
}

// DisabledConfig is the boot state when no [skills] table exists.
func DisabledConfig() types.SkillsConfig {
	c := DefaultConfig()
	c.Enabled = false
	return c
}

// wireDoc is the TOML document shape: the [skills] table plus the signer list.
// SPEC-11 §2's example writes `[[signers]]` at the document root; a nested
// `[[skills.signers]]` is accepted as well, because both readings are natural in
// TOML and the set is authoritative either way.
type wireDoc struct {
	Skills  *skillsWire         `toml:"skills"`
	Signers []types.SkillSigner `toml:"signers"`
}

type skillsWire struct {
	Enabled             *bool               `toml:"enabled"`
	SourcePath          string              `toml:"source_path"`
	SourceURL           string              `toml:"source_url"`
	SourceRef           string              `toml:"source_ref"`
	RefMode             string              `toml:"ref_mode"`
	PullInterval        string              `toml:"pull_interval"`
	PullJitterPct       *int                `toml:"pull_jitter_pct"`
	PullOnBoot          *bool               `toml:"pull_on_boot"`
	PullTimeout         string              `toml:"pull_timeout"`
	PullMaxBytes        int64               `toml:"pull_max_bytes"`
	RequireSignature    *bool               `toml:"require_signature"`
	Approve             string              `toml:"approve"`
	CanaryHostID        string              `toml:"canary_host_id"`
	CanaryValidity      string              `toml:"canary_validity"`
	CanaryOverride      *bool               `toml:"canary_override"`
	AutoAcceptEnabled   *bool               `toml:"auto_accept_enabled"`
	AutoAcceptThreshold *int                `toml:"auto_accept_threshold"`
	AutoAcceptWindow    string              `toml:"auto_accept_window"`
	AutoAcceptModules   []string            `toml:"auto_accept_modules"`
	MaxHold             string              `toml:"max_hold"`
	DemoteAfterFailures *int                `toml:"demote_after_failures"`
	RetainVersions      *int                `toml:"retain_versions"`
	StateDir            string              `toml:"state_dir"`
	GitBinary           string              `toml:"git_binary"`
	Signers             []types.SkillSigner `toml:"signers"`
}

// LoadConfig decodes a `[skills]` document over the defaults and validates it.
func LoadConfig(doc []byte) (types.SkillsConfig, error) {
	cfg := DefaultConfig()
	if len(doc) == 0 {
		return cfg, ValidateConfig(cfg)
	}
	var w wireDoc
	if err := decodeTOML(doc, &w); err != nil {
		return cfg, newErr(types.CodeSkills001, ReasonConfig, "skills config: %v", err)
	}
	if w.Skills == nil {
		return cfg, ValidateConfig(cfg)
	}
	sw := w.Skills
	if sw.Enabled != nil {
		cfg.Enabled = *sw.Enabled
	}
	if sw.SourcePath != "" {
		cfg.SourcePath = sw.SourcePath
	}
	if sw.SourceURL != "" {
		cfg.SourceURL = sw.SourceURL
	}
	if sw.SourceRef != "" {
		cfg.SourceRef = sw.SourceRef
	}
	if sw.RefMode != "" {
		cfg.RefMode = sw.RefMode
	}
	if sw.PullInterval != "" {
		cfg.PullInterval = types.Duration(sw.PullInterval)
	}
	if sw.PullJitterPct != nil {
		cfg.PullJitterPct = *sw.PullJitterPct
	}
	if sw.PullOnBoot != nil {
		cfg.PullOnBoot = *sw.PullOnBoot
	}
	if sw.PullTimeout != "" {
		cfg.PullTimeout = types.Duration(sw.PullTimeout)
	}
	if sw.PullMaxBytes > 0 {
		cfg.PullMaxBytes = sw.PullMaxBytes
	}
	if sw.RequireSignature != nil {
		cfg.RequireSignature = *sw.RequireSignature
	}
	if sw.Approve != "" {
		cfg.Approve = sw.Approve
	}
	cfg.CanaryHostID = sw.CanaryHostID
	if sw.CanaryValidity != "" {
		cfg.CanaryValidity = types.Duration(sw.CanaryValidity)
	}
	if sw.CanaryOverride != nil {
		cfg.CanaryOverride = *sw.CanaryOverride
	}
	if sw.AutoAcceptEnabled != nil {
		cfg.AutoAcceptEnabled = *sw.AutoAcceptEnabled
	}
	if sw.AutoAcceptThreshold != nil {
		cfg.AutoAcceptThreshold = *sw.AutoAcceptThreshold
	}
	if sw.AutoAcceptWindow != "" {
		cfg.AutoAcceptWindow = types.Duration(sw.AutoAcceptWindow)
	}
	if sw.AutoAcceptModules != nil {
		cfg.AutoAcceptModules = sw.AutoAcceptModules
	}
	if sw.MaxHold != "" {
		cfg.MaxHold = types.Duration(sw.MaxHold)
	}
	if sw.DemoteAfterFailures != nil {
		cfg.DemoteAfterFailures = *sw.DemoteAfterFailures
	}
	if sw.RetainVersions != nil {
		cfg.RetainVersions = *sw.RetainVersions
	}
	if sw.StateDir != "" {
		cfg.StateDir = sw.StateDir
	}
	if sw.GitBinary != "" {
		cfg.GitBinary = sw.GitBinary
	}
	switch {
	case len(sw.Signers) > 0:
		cfg.Signers = sw.Signers
	case len(w.Signers) > 0:
		cfg.Signers = w.Signers
	}
	return cfg, ValidateConfig(cfg)
}

// ValidateConfig is the §4.1 source discipline plus the enum rules. A credential
// embedded in a source URL is refused here, never dialled.
func ValidateConfig(cfg types.SkillsConfig) error {
	if !cfg.Enabled {
		return nil
	}
	switch {
	case cfg.SourcePath == "" && cfg.SourceURL == "":
		return newErr(types.CodeSkills001, ReasonConfig, "exactly one of source_path or source_url must be set")
	case cfg.SourcePath != "" && cfg.SourceURL != "":
		return newErr(types.CodeSkills001, ReasonConfig, "source_path and source_url are mutually exclusive")
	}
	if cfg.SourceURL != "" {
		u, err := url.Parse(cfg.SourceURL)
		if err != nil {
			return newErr(types.CodeSkills001, ReasonConfig, "source_url is unparsable: %v", err)
		}
		if u.User != nil {
			return newErr(types.CodeSkills001, ReasonConfig,
				"source_url carries embedded credentials; the channel is read with a credential-free URL")
		}
		if u.Scheme != "https" && u.Scheme != "file" {
			return newErr(types.CodeSkills001, ReasonConfig, "source_url scheme %q is not https", u.Scheme)
		}
	}
	switch cfg.RefMode {
	case "tag", "branch":
	default:
		return newErr(types.CodeSkills001, ReasonConfig, "ref_mode %q is not tag|branch", cfg.RefMode)
	}
	switch cfg.Approve {
	case "auto", "review", "never":
	default:
		return newErr(types.CodeSkills001, ReasonConfig, "approve %q is not auto|review|never", cfg.Approve)
	}
	if !cfg.RequireSignature && !(cfg.SourcePath != "" && cfg.Approve == "review") {
		return newErr(types.CodeSkills001, ReasonConfig,
			"require_signature=false is legal only with a local_path source and approve=review")
	}
	if cfg.PullMaxBytes <= 0 {
		return newErr(types.CodeSkills001, ReasonConfig, "pull_max_bytes must be > 0")
	}
	if cfg.AutoAcceptEnabled && cfg.AutoAcceptThreshold < 1 {
		return newErr(types.CodeSkills001, ReasonConfig, "auto_accept_threshold must be >= 1")
	}
	for _, s := range cfg.Signers {
		if !keyIDRE.MatchString(s.KeyID) {
			return newErr(types.CodeSkills001, ReasonConfig, "signer key_id %q is malformed", s.KeyID)
		}
		if _, err := decodePublicKey(s.PublicKey); err != nil {
			return newErr(types.CodeSkills001, ReasonConfig, "signer %q: %v", s.KeyID, err)
		}
		switch s.Trust {
		case types.TrustRelease, types.TrustLocal:
		default:
			return newErr(types.CodeSkills001, ReasonConfig,
				"signer %q has trust %q, want release|local", s.KeyID, s.Trust)
		}
	}
	return nil
}

// SourcePath resolves the effective source with credentials stripped and the
// path form recorded (`source_path` for a git dir, `source_url` for a remote).
type SourcePath struct {
	Path string
	Kind string // path | url
}

// EffectiveSource is the resolved §4.1 source: exactly one of the two keys, with
// userinfo stripped in the logged form.
func EffectiveSource(cfg types.SkillsConfig) (SourcePath, error) {
	switch {
	case cfg.SourcePath != "":
		return SourcePath{Path: cfg.SourcePath, Kind: "path"}, nil
	case cfg.SourceURL != "":
		u, err := url.Parse(cfg.SourceURL)
		if err != nil {
			return SourcePath{}, newErr(types.CodeSkills001, ReasonConfig, "source_url: %v", err)
		}
		u.User = nil
		return SourcePath{Path: u.String(), Kind: "url"}, nil
	}
	return SourcePath{}, newErr(types.CodeSkills001, ReasonUnknownSource, "no source configured")
}

// SignerList renders the `SkillsStatus.Signers` strings.
func SignerList(cfg types.SkillsConfig) []string {
	out := make([]string, 0, len(cfg.Signers))
	for _, s := range cfg.Signers {
		if s.Enabled {
			out = append(out, s.KeyID+":"+s.Trust)
		}
	}
	return out
}

// Explain renders the skills config surface for `trouble config explain`. A
// public key is a public value, so it is printed; no secret exists in this
// config (the channel is read with a credential-free URL by construction).
func Explain(cfg types.SkillsConfig) []string {
	lines := []string{
		fmt.Sprintf("skills.enabled = %t", cfg.Enabled),
		fmt.Sprintf("skills.source_path = %q", cfg.SourcePath),
		fmt.Sprintf("skills.source_url = %q", cfg.SourceURL),
		fmt.Sprintf("skills.source_ref = %q", cfg.SourceRef),
		fmt.Sprintf("skills.ref_mode = %q", cfg.RefMode),
		fmt.Sprintf("skills.approve = %q", cfg.Approve),
		fmt.Sprintf("skills.canary_host_id = %q", cfg.CanaryHostID),
		fmt.Sprintf("skills.signers = [%s]", strings.Join(SignerList(cfg), ", ")),
	}
	return lines
}
