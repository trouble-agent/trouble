package ladder

import (
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Config is the resolved form of the SPEC-05 §4.3 table: one field per key, the
// field name being the key's last element (`verify_default` → VerifyDefault;
// `source_max_age.sentinel` → SourceMaxAge["sentinel"]).
//
// Every key has a default and no fleet value is compiled in.
type Config struct {
	VerifyDefault         types.Duration
	VerifyRetry           int
	VerifyReopens         int
	ResearchPlayExtra     int
	PlayMaxRunsDefault    int
	PlayMaxRunsCap        int
	StabilizeDefault      types.Duration
	AgentTimeout          types.Duration
	AgentStrikesWindow    types.Duration
	LeaseRecheck          types.Duration
	LeaseGrace            types.Duration
	LeaseRetry            types.Duration
	KillSwitch            bool
	AutonomyMode          types.AutonomyMode
	AgentRunsPerDay       int64
	PlayRunsPerDay        int64
	ResearchPerDay        int64
	SpawnsPerDay          int64
	CanaryInterval        types.Duration
	QuietClose            bool
	ReopenMax             int
	ReopenWindow          types.Duration
	MergeAllowGlobs       []string
	SkillAcceptMinSuccess int
	CorrelationUnitAlias  map[string]string
	SourceMaxAge          map[string]types.Duration
	MaxOpenIncidents      int64
	BreakerRulePerHour    int64
	BreakerRuleOpen       types.Duration
	BreakerSigPer24h      int64
	BreakerSigOpen        types.Duration
	BreakerSourcePerHour  int64
	BreakerSourceOpen     types.Duration
	BreakerGlobalOpen     types.Duration
	BreakerMaxOpen        types.Duration
	DrainTimeout          types.Duration
	ParkTTL               types.Duration
	OrphanTTL             types.Duration
	HostID                string
}

// DefaultConfig is the §4.3 default column.
func DefaultConfig() Config {
	return Config{
		VerifyDefault:         "10m",
		VerifyRetry:           1,
		VerifyReopens:         1,
		ResearchPlayExtra:     1,
		PlayMaxRunsDefault:    2,
		PlayMaxRunsCap:        5,
		StabilizeDefault:      "2m",
		AgentTimeout:          "15m",
		AgentStrikesWindow:    "24h",
		LeaseRecheck:          "250ms",
		LeaseGrace:            "90s",
		LeaseRetry:            "60s",
		AutonomyMode:          types.AutoShadow,
		AgentRunsPerDay:       20,
		PlayRunsPerDay:        50,
		ResearchPerDay:        30,
		SpawnsPerDay:          5,
		CanaryInterval:        "5m",
		QuietClose:            true,
		ReopenMax:             100,
		ReopenWindow:          "24h",
		MergeAllowGlobs:       []string{},
		SkillAcceptMinSuccess: 3,
		CorrelationUnitAlias:  map[string]string{},
		SourceMaxAge: map[string]types.Duration{
			"sentinel": "300s",
			"sensor":   "120s",
		},
		MaxOpenIncidents:     200,
		BreakerRulePerHour:   5,
		BreakerRuleOpen:      "30m",
		BreakerSigPer24h:     3,
		BreakerSigOpen:       "60m",
		BreakerSourcePerHour: 20,
		BreakerSourceOpen:    "30m",
		BreakerGlobalOpen:    "15m",
		BreakerMaxOpen:       "4h",
		DrainTimeout:         "5s",
		ParkTTL:              "24h",
		OrphanTTL:            "24h",
	}
}

// WithDefaults fills every unset field from the default column, so a caller can
// hand in only the keys it cares about.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	fillDur(&c.VerifyDefault, d.VerifyDefault)
	fillInt(&c.VerifyRetry, d.VerifyRetry)
	fillInt(&c.VerifyReopens, d.VerifyReopens)
	fillInt(&c.ResearchPlayExtra, d.ResearchPlayExtra)
	fillInt(&c.PlayMaxRunsDefault, d.PlayMaxRunsDefault)
	fillInt(&c.PlayMaxRunsCap, d.PlayMaxRunsCap)
	fillDur(&c.StabilizeDefault, d.StabilizeDefault)
	fillDur(&c.AgentTimeout, d.AgentTimeout)
	fillDur(&c.AgentStrikesWindow, d.AgentStrikesWindow)
	fillDur(&c.LeaseRecheck, d.LeaseRecheck)
	fillDur(&c.LeaseGrace, d.LeaseGrace)
	fillDur(&c.LeaseRetry, d.LeaseRetry)
	if c.AutonomyMode == "" {
		c.AutonomyMode = d.AutonomyMode
	}
	fillInt64(&c.AgentRunsPerDay, d.AgentRunsPerDay)
	fillInt64(&c.PlayRunsPerDay, d.PlayRunsPerDay)
	fillInt64(&c.ResearchPerDay, d.ResearchPerDay)
	fillInt64(&c.SpawnsPerDay, d.SpawnsPerDay)
	fillDur(&c.CanaryInterval, d.CanaryInterval)
	if !c.QuietClose {
		// quiet_close defaults to TRUE, so an unset value must not read as false:
		// the zero Config is the shadow default and this keeps §3.8's quiet-close
		// working unless a caller explicitly disables it through FromValues.
		c.QuietClose = d.QuietClose
	}
	fillInt(&c.ReopenMax, d.ReopenMax)
	fillDur(&c.ReopenWindow, d.ReopenWindow)
	if c.MergeAllowGlobs == nil {
		c.MergeAllowGlobs = d.MergeAllowGlobs
	}
	fillInt(&c.SkillAcceptMinSuccess, d.SkillAcceptMinSuccess)
	if c.CorrelationUnitAlias == nil {
		c.CorrelationUnitAlias = d.CorrelationUnitAlias
	}
	if c.SourceMaxAge == nil {
		c.SourceMaxAge = d.SourceMaxAge
	}
	fillInt64(&c.MaxOpenIncidents, d.MaxOpenIncidents)
	fillInt64(&c.BreakerRulePerHour, d.BreakerRulePerHour)
	fillDur(&c.BreakerRuleOpen, d.BreakerRuleOpen)
	fillInt64(&c.BreakerSigPer24h, d.BreakerSigPer24h)
	fillDur(&c.BreakerSigOpen, d.BreakerSigOpen)
	fillInt64(&c.BreakerSourcePerHour, d.BreakerSourcePerHour)
	fillDur(&c.BreakerSourceOpen, d.BreakerSourceOpen)
	fillDur(&c.BreakerGlobalOpen, d.BreakerGlobalOpen)
	fillDur(&c.BreakerMaxOpen, d.BreakerMaxOpen)
	fillDur(&c.DrainTimeout, d.DrainTimeout)
	fillDur(&c.ParkTTL, d.ParkTTL)
	fillDur(&c.OrphanTTL, d.OrphanTTL)
	return c
}

func fillDur(dst *types.Duration, def types.Duration) {
	if *dst == "" {
		*dst = def
	}
}

func fillInt(dst *int, def int) {
	if *dst == 0 {
		*dst = def
	}
}

func fillInt64(dst *int64, def int64) {
	if *dst == 0 {
		*dst = def
	}
}

// FromValues folds SPEC-12's resolved ConfigValues over the defaults. An unknown
// `ladder.*` key is a refusal: a config key that silently does nothing is the
// inert-feature class.
func FromValues(vals []types.ConfigValue) (Config, *Error) {
	c := DefaultConfig()
	seen := map[string]bool{}
	for _, v := range vals {
		key := v.Key
		if len(key) < 7 || key[:7] != "ladder." {
			continue
		}
		seen[key] = true
		switch key {
		case "ladder.verify_default":
			c.VerifyDefault = asDurationValue(v.Value, c.VerifyDefault)
		case "ladder.verify_retry":
			c.VerifyRetry = asIntValue(v.Value, c.VerifyRetry)
		case "ladder.verify_reopens":
			c.VerifyReopens = asIntValue(v.Value, c.VerifyReopens)
		case "ladder.research_play_extra":
			c.ResearchPlayExtra = asIntValue(v.Value, c.ResearchPlayExtra)
		case "ladder.play_max_runs_default":
			c.PlayMaxRunsDefault = asIntValue(v.Value, c.PlayMaxRunsDefault)
		case "ladder.play_max_runs_cap":
			c.PlayMaxRunsCap = asIntValue(v.Value, c.PlayMaxRunsCap)
		case "ladder.stabilize_default":
			c.StabilizeDefault = asDurationValue(v.Value, c.StabilizeDefault)
		case "ladder.agent_timeout":
			c.AgentTimeout = asDurationValue(v.Value, c.AgentTimeout)
		case "ladder.agent_strikes_window":
			c.AgentStrikesWindow = asDurationValue(v.Value, c.AgentStrikesWindow)
		case "ladder.lease_recheck":
			c.LeaseRecheck = asDurationValue(v.Value, c.LeaseRecheck)
		case "ladder.lease_grace":
			c.LeaseGrace = asDurationValue(v.Value, c.LeaseGrace)
		case "ladder.lease_retry":
			c.LeaseRetry = asDurationValue(v.Value, c.LeaseRetry)
		case "ladder.kill_switch":
			c.KillSwitch = asBoolValue(v.Value, c.KillSwitch)
		case "ladder.autonomy_mode":
			if s, ok := v.Value.(string); ok && types.AutonomyMode(s).Valid() {
				c.AutonomyMode = types.AutonomyMode(s)
			}
		case "ladder.agent_runs_per_day":
			c.AgentRunsPerDay = int64(asIntValue(v.Value, int(c.AgentRunsPerDay)))
		case "ladder.play_runs_per_day":
			c.PlayRunsPerDay = int64(asIntValue(v.Value, int(c.PlayRunsPerDay)))
		case "ladder.research_per_day":
			c.ResearchPerDay = int64(asIntValue(v.Value, int(c.ResearchPerDay)))
		case "ladder.spawns_per_day":
			c.SpawnsPerDay = int64(asIntValue(v.Value, int(c.SpawnsPerDay)))
		case "ladder.canary_interval":
			c.CanaryInterval = asDurationValue(v.Value, c.CanaryInterval)
		case "ladder.quiet_close":
			c.QuietClose = asBoolValue(v.Value, c.QuietClose)
		case "ladder.reopen_max":
			c.ReopenMax = asIntValue(v.Value, c.ReopenMax)
		case "ladder.reopen_window":
			c.ReopenWindow = asDurationValue(v.Value, c.ReopenWindow)
		case "ladder.drain_timeout":
			c.DrainTimeout = asDurationValue(v.Value, c.DrainTimeout)
		case "ladder.park_ttl":
			c.ParkTTL = asDurationValue(v.Value, c.ParkTTL)
		case "ladder.orphan_ttl":
			c.OrphanTTL = asDurationValue(v.Value, c.OrphanTTL)
		case "ladder.max_open_incidents":
			c.MaxOpenIncidents = int64(asIntValue(v.Value, int(c.MaxOpenIncidents)))
		case "ladder.merge_allow_globs":
			c.MergeAllowGlobs = asStringsValue(v.Value, c.MergeAllowGlobs)
		case "ladder.skill_accept_min_success":
			c.SkillAcceptMinSuccess = asIntValue(v.Value, c.SkillAcceptMinSuccess)
		case "ladder.correlation.unit_alias":
			if m, ok := v.Value.(map[string]string); ok {
				c.CorrelationUnitAlias = m
			} else if m, ok := v.Value.(map[string]any); ok {
				alias := map[string]string{}
				for k, raw := range m {
					if s, ok := raw.(string); ok {
						alias[k] = s
					}
				}
				c.CorrelationUnitAlias = alias
			}
		case "ladder.source_max_age.sentinel":
			c.SourceMaxAge["sentinel"] = asDurationValue(v.Value, c.SourceMaxAge["sentinel"])
		case "ladder.source_max_age.sensor":
			c.SourceMaxAge["sensor"] = asDurationValue(v.Value, c.SourceMaxAge["sensor"])
		default:
			return c, newErr(types.CodeLadder005, reasonStabilizeInvalid, "unknown ladder config key %q", key)
		}
	}
	return c, nil
}

func asDurationValue(v any, def types.Duration) types.Duration {
	switch t := v.(type) {
	case string:
		if _, err := time.ParseDuration(t); err == nil {
			return types.Duration(t)
		}
	case time.Duration:
		return types.Duration(t.String())
	case int:
		return types.Duration((time.Duration(t) * time.Second).String())
	case int64:
		return types.Duration((time.Duration(t) * time.Second).String())
	}
	return def
}

func asIntValue(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return def
}

func asBoolValue(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func asStringsValue(v any, def []string) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return def
}
