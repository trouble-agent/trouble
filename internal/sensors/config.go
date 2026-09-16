package sensors

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// sensorConfig is the decoded, resolved configuration of the detection plane
// (SPEC-03 §2, §4). It is package-private: no spec may depend on its shape.
type sensorConfig struct {
	stateRoot string
	hostID    string
	hubID     string
	actor     types.Actor

	psi      psiConf
	journald journalConf
	dbus     dbusConf
	disk     diskConf
	timers   timerConf
	inotify  inotifyConf

	rulesDir        string
	reloadDebounce  time.Duration
	rulesMTMSSweep  time.Duration
	mergeWindow     time.Duration
	limits          limitsConf
	knownModules    []string
	modulesKnownSet bool
}

type psiConf struct {
	enabled        bool
	sampleInterval time.Duration
	window         time.Duration
}

type journalConf struct {
	enabled             bool
	units               []string
	followAll           bool
	followAllMaxEntries int
	queue               int
	queueBytes          int
	maxEntry            int
	probeInterval       time.Duration
}

type dbusConf struct {
	enabled           bool
	userManagers      []string
	pingInterval      time.Duration
	reconcileInterval time.Duration
	oomdProbeInterval time.Duration
}

type diskConf struct {
	enabled  bool
	mounts   []string
	interval time.Duration
}

type timerConf struct {
	enabled  bool
	interval time.Duration
}

type inotifyConf struct {
	enabled         bool
	paths           []inotifyPathConf
	recheckInterval time.Duration
	maxDepth        int
}

type inotifyPathConf struct {
	path      string
	mask      string
	recursive bool
	maxDepth  int
	rule      string
}

type limitsConf struct {
	rulePerMin      int
	sourcePerMin    int
	globalPerMin    int
	incidentsPer5m  int
	ruleIncidents5m int
}

// defaultStateRoot is the SPEC-12 §3.1 default: `${XDG_STATE_HOME:-~/.local/state}/trouble`.
func defaultStateRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return filepath.Join("/var/lib", "trouble")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "trouble")
}

func defaultConfig() sensorConfig {
	sr := defaultStateRoot()
	return sensorConfig{
		stateRoot: sr,
		actor: types.Actor{
			Kind: types.ActorDaemon,
			ID:   "troubled",
		},
		psi: psiConf{enabled: true, sampleInterval: 2 * time.Second, window: 2 * time.Second},
		journald: journalConf{
			enabled:             true,
			units:               []string{},
			followAll:           false,
			followAllMaxEntries: 2000000,
			queue:               8192,
			queueBytes:          32 << 20,
			maxEntry:            65536,
			probeInterval:       30 * time.Second,
		},
		dbus: dbusConf{
			enabled:           true,
			userManagers:      []string{"self"},
			pingInterval:      30 * time.Second,
			reconcileInterval: 300 * time.Second,
			oomdProbeInterval: 10 * time.Minute,
		},
		disk:   diskConf{enabled: true, mounts: []string{"/"}, interval: 60 * time.Second},
		timers: timerConf{enabled: true, interval: 60 * time.Second},
		inotify: inotifyConf{
			enabled:         true,
			paths:           []inotifyPathConf{},
			recheckInterval: 900 * time.Second,
			maxDepth:        8,
		},
		rulesDir:       filepath.Join(filepath.Dir(sr), "config", "rules.d"),
		reloadDebounce: 500 * time.Millisecond,
		rulesMTMSSweep: 60 * time.Second,
		mergeWindow:    5 * time.Second,
		limits: limitsConf{
			// SPEC-03 §3.8 table numbers; the rule scope's 20-incidents/300s
			// arm is not a config key in §4, so it carries the table value.
			rulePerMin:      120,
			sourcePerMin:    600,
			globalPerMin:    1200,
			incidentsPer5m:  25,
			ruleIncidents5m: 20,
		},
	}
}

// decodeConfig folds resolved ConfigValues into sensorConfig (SPEC-03 §4).
//
// Precedence is SPEC-12's job: this function takes the already-resolved set and
// applies each value in order (later wins), so a caller that passes
// default→file→env→flag ordered values gets the pinned precedence for free.
// An unknown `sensors.*` key is a loud failure, never a silent ignore: a
// configured sensor that does not exist is exactly the inert-feature class.
func decodeConfig(vals []types.ConfigValue) (sensorConfig, error) {
	cfg := defaultConfig()
	for _, v := range vals {
		if err := cfg.apply(v); err != nil {
			return cfg, err
		}
	}
	if cfg.hostID == "" {
		return cfg, fmt.Errorf("sensors: origin.host_id is required (SPEC-03 §2; a record without a host identity cannot be deduped cross-host)")
	}
	return cfg, nil
}

func (c *sensorConfig) apply(v types.ConfigValue) error {
	key := strings.TrimSpace(v.Key)
	switch key {
	// ---- identity (SPEC-12 §3.14) ----
	case "state_root":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		if s != "" {
			c.stateRoot = s
			c.rulesDir = filepath.Join(filepath.Dir(s), "config", "rules.d")
		}
		return nil
	case "origin.host_id":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.hostID = s
		return nil
	case "origin.hub_id":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.hubID = s
		return nil
	case "actor.kind":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.actor.Kind = types.ActorKind(s)
		return nil
	case "actor.id":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.actor.ID = s
		return nil
	case "actor.version", "daemon.version":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.actor.Version = s
		return nil
	case "daemon.git_sha":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.actor.GitSHA = s
		return nil
	case "daemon.build_time":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.actor.BuildTime = s
		return nil
	// ---- the auto_grants existence check input (SPEC-03 §3.5) ----
	case "registry.modules":
		ss, err := asStringSlice(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.knownModules = ss
		c.modulesKnownSet = true
		return nil

	// ---- psi ----
	case "sensors.psi.enabled":
		return setBool(key, v, &c.psi.enabled)
	case "sensors.psi.sample_interval":
		return setDur(key, v, &c.psi.sampleInterval)
	case "sensors.psi.window":
		return setDur(key, v, &c.psi.window)

	// ---- journald ----
	case "sensors.journald.enabled":
		return setBool(key, v, &c.journald.enabled)
	case "sensors.journald.units":
		ss, err := asStringSlice(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.journald.units = ss
		return nil
	case "sensors.journald.follow_all":
		return setBool(key, v, &c.journald.followAll)
	case "sensors.journald.follow_all_max_entries":
		return setInt(key, v, &c.journald.followAllMaxEntries)
	case "sensors.journald.queue":
		return setInt(key, v, &c.journald.queue)
	case "sensors.journald.queue_bytes":
		return setInt(key, v, &c.journald.queueBytes)
	case "sensors.journald.max_entry":
		return setInt(key, v, &c.journald.maxEntry)
	case "sensors.journald.probe_interval":
		return setDur(key, v, &c.journald.probeInterval)

	// ---- dbus ----
	case "sensors.dbus.enabled":
		return setBool(key, v, &c.dbus.enabled)
	case "sensors.dbus.user_managers":
		ss, err := asStringSlice(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.dbus.userManagers = ss
		return nil
	case "sensors.dbus.ping_interval":
		return setDur(key, v, &c.dbus.pingInterval)
	case "sensors.dbus.reconcile_interval":
		return setDur(key, v, &c.dbus.reconcileInterval)
	case "sensors.dbus.oomd_probe_interval":
		return setDur(key, v, &c.dbus.oomdProbeInterval)

	// ---- disk ----
	case "sensors.disk.enabled":
		return setBool(key, v, &c.disk.enabled)
	case "sensors.disk.mounts":
		ss, err := asStringSlice(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.disk.mounts = ss
		return nil
	case "sensors.disk.interval":
		return setDur(key, v, &c.disk.interval)

	// ---- timers ----
	case "sensors.timers.enabled":
		return setBool(key, v, &c.timers.enabled)
	case "sensors.timers.interval":
		return setDur(key, v, &c.timers.interval)

	// ---- inotify ----
	case "sensors.inotify.enabled":
		return setBool(key, v, &c.inotify.enabled)
	case "sensors.inotify.paths":
		paths, err := asInotifyPaths(v)
		if err != nil {
			return keyErr(key, err)
		}
		c.inotify.paths = paths
		return nil
	case "sensors.inotify.recheck_interval":
		return setDur(key, v, &c.inotify.recheckInterval)
	case "sensors.inotify.max_depth":
		return setInt(key, v, &c.inotify.maxDepth)

	// ---- rules ----
	case "sensors.rules.dir":
		s, err := asString(v)
		if err != nil {
			return keyErr(key, err)
		}
		if s != "" {
			c.rulesDir = s
		}
		return nil
	case "sensors.rules.reload_debounce":
		return setDur(key, v, &c.reloadDebounce)

	// ---- merge window (SPEC-03 §3.6) ----
	case "sensors.merge_window":
		return setDur(key, v, &c.mergeWindow)

	// ---- limits (SPEC-03 §3.8); the table's own numbers are the defaults ----
	case "sensors.limits.rule_per_min":
		var n int
		if err := setInt(key, v, &n); err != nil {
			return err
		}
		c.limits.rulePerMin = n
		return nil
	case "sensors.limits.source_per_min":
		var n int
		if err := setInt(key, v, &n); err != nil {
			return err
		}
		c.limits.sourcePerMin = n
		return nil
	case "sensors.limits.global_per_min":
		var n int
		if err := setInt(key, v, &n); err != nil {
			return err
		}
		c.limits.globalPerMin = n
		return nil
	case "sensors.limits.incidents_per_5m":
		var n int
		if err := setInt(key, v, &n); err != nil {
			return err
		}
		c.limits.incidentsPer5m = n
		return nil
	}
	if strings.HasPrefix(key, "sensors.") {
		return fmt.Errorf("sensors: unknown configuration key %q (SPEC-03 §4: an unknown sensors.* key is a loud failure, never a silent ignore)", key)
	}
	// Keys outside the sensors.* namespace belong to other subsystems; the
	// composition root passes one resolved set, so they are not ours to judge.
	return nil
}

func keyErr(key string, err error) error { return fmt.Errorf("sensors: %s: %w", key, err) }

// ---- typed readers (ConfigValue.Value is `any`: flags/env arrive as strings,
// file values as their natural Go types) ----

func asString(v types.ConfigValue) (string, error) {
	switch t := v.Value.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(t), nil
	case fmt.Stringer:
		return t.String(), nil
	default:
		return "", fmt.Errorf("expected a string, got %T", v.Value)
	}
}

func asBool(v types.ConfigValue) (bool, error) {
	switch t := v.Value.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("expected a bool, got %q", t)
		}
		return b, nil
	default:
		return false, fmt.Errorf("expected a bool, got %T", v.Value)
	}
}

func asInt(v types.ConfigValue) (int, error) {
	switch t := v.Value.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("expected an integer, got %q", t)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v.Value)
	}
}

func asDur(v types.ConfigValue) (time.Duration, error) {
	switch t := v.Value.(type) {
	case string:
		d, err := time.ParseDuration(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("expected a duration string, got %q", t)
		}
		return d, nil
	case int:
		return time.Duration(t), nil
	case int64:
		return time.Duration(t), nil
	case float64:
		return time.Duration(t), nil
	default:
		return 0, fmt.Errorf("expected a duration, got %T", v.Value)
	}
}

func asStringSlice(v types.ConfigValue) ([]string, error) {
	switch t := v.Value.(type) {
	case nil:
		return nil, nil
	case []string:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return []string{}, nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings, got an element of type %T", e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected a list of strings, got %T", v.Value)
	}
}

func asInotifyPaths(v types.ConfigValue) ([]inotifyPathConf, error) {
	list, ok := v.Value.([]any)
	if !ok {
		if s, ok := v.Value.(string); ok && strings.TrimSpace(s) == "" {
			return []inotifyPathConf{}, nil
		}
		return nil, fmt.Errorf("expected a list of tables, got %T", v.Value)
	}
	out := make([]inotifyPathConf, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected a table, got %T", e)
		}
		p := inotifyPathConf{mask: defaultInotifyMask, maxDepth: 8}
		for k, raw := range m {
			switch k {
			case "path":
				p.path, _ = raw.(string)
			case "mask":
				p.mask, _ = raw.(string)
			case "recursive":
				switch b := raw.(type) {
				case bool:
					p.recursive = b
				case string:
					p.recursive, _ = strconv.ParseBool(b)
				}
			case "max_depth":
				switch n := raw.(type) {
				case int:
					p.maxDepth = n
				case int64:
					p.maxDepth = int(n)
				case float64:
					p.maxDepth = int(n)
				case string:
					p.maxDepth, _ = strconv.Atoi(n)
				}
			case "rule":
				p.rule, _ = raw.(string)
			}
		}
		if p.path == "" {
			return nil, fmt.Errorf("sensors.inotify.paths entry without a path")
		}
		out = append(out, p)
	}
	return out, nil
}

func setBool(key string, v types.ConfigValue, dst *bool) error {
	b, err := asBool(v)
	if err != nil {
		return keyErr(key, err)
	}
	*dst = b
	return nil
}

func setInt(key string, v types.ConfigValue, dst *int) error {
	n, err := asInt(v)
	if err != nil {
		return keyErr(key, err)
	}
	*dst = n
	return nil
}

func setDur(key string, v types.ConfigValue, dst *time.Duration) error {
	d, err := asDur(v)
	if err != nil {
		return keyErr(key, err)
	}
	*dst = d
	return nil
}
