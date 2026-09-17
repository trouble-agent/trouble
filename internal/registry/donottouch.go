package registry

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// stateRootPlaceholder is the token the compiled-in do-not-touch floor carries;
// it is resolved to the live state root at boot (SPEC-06 §3.6).
const stateRootPlaceholder = "<state_root>"

// floorPaths, floorUnits and floorScopes are the compiled-in do-not-touch floor
// (SPEC-06 §3.6). The floor is ALWAYS applied: configuration can widen the deny
// set, never narrow it.
var (
	floorPaths = []string{
		"/etc/shadow", "/etc/gshadow", "/etc/passwd", "/etc/sudoers", "/etc/sudoers.d/**",
		"/etc/ssh/**", "/etc/polkit-1/**", "/etc/trouble/do-not-touch.toml", "/etc/fstab",
		"/boot/**", "/usr/**", "/lib/**", "/lib64/**", "/sbin/**", "/bin/**",
		"/var/lib/trouble/**", stateRootPlaceholder + "/**", stateRootPlaceholder + "/backups/**",
	}
	floorUnits = []string{
		"init.scope", "systemd-journald.service", "systemd-logind.service", "dbus.service",
		"polkit.service", "ssh.service", "sshd.service", "trouble.service", "trouble-escalate@.service",
		"systemd-oomd.service",
	}
	floorScopes = []string{"service:stop", "config:delete", "file:exec"}
)

// FloorDoNotTouch returns the compiled-in floor with the state root resolved.
func FloorDoNotTouch(stateRoot string) types.DoNotTouch {
	resolve := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			out = append(out, strings.ReplaceAll(p, stateRootPlaceholder, stateRoot))
		}
		return out
	}
	return types.DoNotTouch{
		Paths:  resolve(floorPaths),
		Units:  append([]string(nil), floorUnits...),
		Scopes: append([]string(nil), floorScopes...),
	}
}

// protectedTarget is one target a module declares (SPEC-06 §2.1).
type protectedTarget struct {
	Kind  string // path | unit | scope
	Value string // canonical string
}

// targetScopeKeywords are the args keys a unit- or scope-shaped target may hide
// under; they are used both by ProtectedTargets implementations and by the
// fallback key scan.
const targetScopeUnits = "unit"

// targetDeclarer is the package-private extension every shipped module
// implements; a module that omits it falls back to the args key scan so it
// cannot silently opt out of do-not-touch by omission (SPEC-06 §2.1).
type targetDeclarer interface {
	ProtectedTargets(args map[string]any) []protectedTarget
}

// fallbackTargetKeys is the key scan for a module without ProtectedTargets.
var fallbackTargetKeys = map[string]string{
	"path": "path", "paths": "path", "file": "path", "files": "path",
	"source": "path", "target": "path", "dest": "path",
	"unit": "unit", "units": "unit",
}

func targetsOf(m types.Module, args map[string]any) []protectedTarget {
	if td, ok := m.(targetDeclarer); ok {
		return td.ProtectedTargets(args)
	}
	var out []protectedTarget
	for key, kind := range fallbackTargetKeys {
		raw, ok := args[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case string:
			out = append(out, protectedTarget{Kind: kind, Value: v})
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					out = append(out, protectedTarget{Kind: kind, Value: s})
				}
			}
		case []string:
			for _, s := range v {
				out = append(out, protectedTarget{Kind: kind, Value: s})
			}
		}
	}
	return out
}

// canonicalPath makes a candidate absolute, clean, and symlink-resolved; a
// resolution failure falls back to the cleaned form so a dangling symlink cannot
// dodge the floor (SPEC-06 §3.6, edge case 7).
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	clean := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
}

// matchPath reports whether the candidate is denied. Patterns use Go path.Match
// semantics with the single extension that a trailing "/**" matches the prefix
// itself and everything under it.
func matchPath(pattern, candidate string) bool {
	if pattern == candidate {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return candidate == prefix || strings.HasPrefix(candidate, prefix+"/")
	}
	ok, err := filepath.Match(pattern, candidate)
	if err != nil {
		return false
	}
	return ok
}

// normalizeUnit applies systemd unit normalization: a bare name gains
// ".service"; a template instance is compared by its template name (SPEC-06 §3.6).
func normalizeUnit(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, ".") {
		u += ".service"
	}
	if at := strings.IndexByte(u, '@'); at >= 0 {
		rest := u[at+1:]
		suffix := ""
		if i := strings.IndexByte(rest, '.'); i >= 0 {
			suffix = rest[i:]
		}
		u = u[:at+1] + suffix
	}
	return u
}

// checkProtected enforces step a3 of authorize: before validate, before Check,
// before any target is touched. A hit is TROUBLE-REGISTRY-007, class
// policy_refused (SPEC-06 §3.6).
func (r *Registry) checkProtected(m types.Module, args map[string]any) *Error {
	for _, t := range targetsOf(m, args) {
		switch t.Kind {
		case "path":
			cand := canonicalPath(t.Value)
			if cand == "" {
				continue
			}
			for _, p := range r.dnt.Paths {
				if matchPath(p, cand) {
					return newErr(types.CodeRegistry007, types.StageAuthorize, reasonDoNotTouchPath,
						"%s target %s is protected by %s", m.Descriptor().Name, cand, p)
				}
			}
		case "unit":
			u := normalizeUnit(t.Value)
			for _, want := range r.dnt.Units {
				if normalizeUnit(want) == u {
					return newErr(types.CodeRegistry007, types.StageAuthorize, reasonDoNotTouchUnit,
						"%s target unit %s is protected", m.Descriptor().Name, u)
				}
			}
		case "scope":
			for _, s := range r.dnt.Scopes {
				if s == t.Value {
					return newErr(types.CodeRegistry007, types.StageAuthorize, reasonDoNotTouchScope,
						"%s target scope %s is refused outright", m.Descriptor().Name, t.Value)
				}
			}
		}
	}
	return nil
}

// mergeDoNotTouch applies the additive merge: the floor stays, the file's values
// are appended, and weakening keys are rejected with a recorded refusal
// (SPEC-06 §3.6 rule 2, edge case 6).
func mergeDoNotTouch(floor, file types.DoNotTouch, pathsExtra, unitsExtra, scopesExtra []string) types.DoNotTouch {
	out := types.DoNotTouch{}
	out.Paths = append(out.Paths, floor.Paths...)
	out.Paths = append(out.Paths, file.Paths...)
	out.Paths = append(out.Paths, pathsExtra...)
	out.Units = append(out.Units, floor.Units...)
	out.Units = append(out.Units, file.Units...)
	out.Units = append(out.Units, unitsExtra...)
	out.Scopes = append(out.Scopes, floor.Scopes...)
	out.Scopes = append(out.Scopes, file.Scopes...)
	out.Scopes = append(out.Scopes, scopesExtra...)
	return out
}

// defaultStateRoot resolves <state_root> for this host: /var/lib/trouble when
// running as root, otherwise the per-user state directory.
func defaultStateRoot() string {
	if os.Geteuid() == 0 {
		return "/var/lib/trouble"
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "trouble")
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".local", "state", "trouble")
	}
	return "/var/lib/trouble"
}

func defaultDoNotTouchFile(stateRoot string) string {
	if os.Geteuid() == 0 {
		return "/etc/trouble/do-not-touch.toml"
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "trouble", "do-not-touch.toml")
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".config", "trouble", "do-not-touch.toml")
	}
	_ = stateRoot
	return "/etc/trouble/do-not-touch.toml"
}

// ---- resolved configuration (SPEC-06 §4.3) ----

// config holds the resolved registry.*/file.*/proc.* keys. Every key has a
// default and no fleet value is compiled in.
type config struct {
	StateRoot        string
	PlaysDir         string
	SkillsDir        string
	DoNotTouchFile   string
	DNTPathsExtra    []string
	DNTUnitsExtra    []string
	DNTScopesExtra   []string
	ServiceUnits     []string
	DbusAddress      string
	SystemdScopeUser string
	ProbeTimeout     time.Duration
	RestartProbeS    int
	MaxTimeoutS      int
	RetryMax         int
	IdemWindow       int
	FileAllowRoots   []string
	FileBackupDir    string
	FileKeepBackups  int
	ProcRoot         string
	ProcTopN         int
	ProcFDScanMax    int
}

func defaultConfig() config {
	root := defaultStateRoot()
	return config{
		StateRoot:       root,
		PlaysDir:        filepath.Join(root, "plays"),
		SkillsDir:       filepath.Join(root, "skills-local"),
		DoNotTouchFile:  defaultDoNotTouchFile(root),
		ServiceUnits:    []string{},
		ProbeTimeout:    2 * time.Second,
		RestartProbeS:   10,
		MaxTimeoutS:     60,
		RetryMax:        3,
		IdemWindow:      4096,
		FileAllowRoots:  []string{},
		FileBackupDir:   filepath.Join(root, "backups"),
		FileKeepBackups: 20,
		ProcRoot:        "/proc",
		ProcTopN:        15,
		ProcFDScanMax:   20000,
	}
}

// decodeConfig folds resolved ConfigValues (SPEC-12 owns precedence) over the
// defaults; an unknown registry.*/file.*/proc.* key fails loudly.
func decodeConfig(vals []types.ConfigValue) (config, error) {
	cfg := defaultConfig()
	for _, v := range vals {
		switch v.Key {
		case "registry.state_root":
			cfg.StateRoot = asString(v.Value, cfg.StateRoot)
			cfg.PlaysDir = filepath.Join(cfg.StateRoot, "plays")
			cfg.SkillsDir = filepath.Join(cfg.StateRoot, "skills-local")
			cfg.FileBackupDir = filepath.Join(cfg.StateRoot, "backups")
		case "registry.plays_dir":
			cfg.PlaysDir = asString(v.Value, cfg.PlaysDir)
		case "registry.skills_dir":
			cfg.SkillsDir = asString(v.Value, cfg.SkillsDir)
		case "registry.do_not_touch_file":
			cfg.DoNotTouchFile = asString(v.Value, cfg.DoNotTouchFile)
		case "registry.do_not_touch_paths_extra":
			cfg.DNTPathsExtra = asStrings(v.Value, cfg.DNTPathsExtra)
		case "registry.do_not_touch_units_extra":
			cfg.DNTUnitsExtra = asStrings(v.Value, cfg.DNTUnitsExtra)
		case "registry.do_not_touch_scopes_extra":
			cfg.DNTScopesExtra = asStrings(v.Value, cfg.DNTScopesExtra)
		case "registry.service_units":
			cfg.ServiceUnits = asStrings(v.Value, cfg.ServiceUnits)
		case "registry.systemd_scope_user":
			cfg.SystemdScopeUser = asString(v.Value, cfg.SystemdScopeUser)
		case "registry.probe_timeout":
			cfg.ProbeTimeout = asDuration(v.Value, cfg.ProbeTimeout)
		case "registry.restart_probe_s":
			cfg.RestartProbeS = asInt(v.Value, cfg.RestartProbeS)
		case "registry.max_timeout_s":
			cfg.MaxTimeoutS = asInt(v.Value, cfg.MaxTimeoutS)
		case "registry.retry_max":
			cfg.RetryMax = asInt(v.Value, cfg.RetryMax)
		case "registry.idem_window":
			cfg.IdemWindow = asInt(v.Value, cfg.IdemWindow)
		case "file.allow_roots":
			cfg.FileAllowRoots = asStrings(v.Value, cfg.FileAllowRoots)
		case "file.backup_dir":
			cfg.FileBackupDir = asString(v.Value, cfg.FileBackupDir)
		case "file.keep_backups":
			cfg.FileKeepBackups = asInt(v.Value, cfg.FileKeepBackups)
		case "proc.root":
			cfg.ProcRoot = asString(v.Value, cfg.ProcRoot)
		case "proc.top_n":
			cfg.ProcTopN = asInt(v.Value, cfg.ProcTopN)
		case "proc.fd_scan_max":
			cfg.ProcFDScanMax = asInt(v.Value, cfg.ProcFDScanMax)
		default:
			if isRegistryKey(v.Key) {
				return cfg, newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid,
					"unknown registry config key %q", v.Key)
			}
		}
	}
	return cfg, nil
}

func isRegistryKey(k string) bool {
	return strings.HasPrefix(k, "registry.") || strings.HasPrefix(k, "file.") || strings.HasPrefix(k, "proc.")
}

func asString(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func asStrings(v any, def []string) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t == "" {
			return def
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return def
}

func asInt(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return def
}

func asDuration(v any, def time.Duration) time.Duration {
	switch t := v.(type) {
	case time.Duration:
		return t
	case string:
		if d, err := time.ParseDuration(t); err == nil {
			return d
		}
	case int:
		return time.Duration(t) * time.Second
	case int64:
		return time.Duration(t) * time.Second
	}
	return def
}
