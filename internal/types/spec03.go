package types

// Types contributed by SPEC-03 (SPEC-TYPES §3.5) plus the two shared types
// SPEC-03 consumes but does not own (SourceLiveness §3.12, ConfigValue §3.14).
// Field names, order and JSON tags are verbatim from SPEC-TYPES and are
// contractual.

// SensorKind names a sensor (SPEC-TYPES §3.5). The six values are the six
// rows of SPEC-03 §3.1; SigSource is the same vocabulary for the dedup core.
type SensorKind string

const (
	SenPSI      SensorKind = "psi"
	SenJournald SensorKind = "journald"
	SenDBus     SensorKind = "dbus"
	SenDisk     SensorKind = "disk"
	SenTimers   SensorKind = "timers"
	SenInotify  SensorKind = "inotify"
)

// SensorKinds lists every sensor in SPEC-03 §3.1 table order.
var SensorKinds = []SensorKind{SenPSI, SenJournald, SenDBus, SenDisk, SenTimers, SenInotify}

// Valid reports whether k is one of the six sensors.
func (k SensorKind) Valid() bool {
	for _, v := range SensorKinds {
		if v == k {
			return true
		}
	}
	return false
}

// SigSource maps a sensor onto its dedup-core source. The two vocabularies are
// identical by construction (SPEC-03 §3.1), so this is a cast with a guard.
func (k SensorKind) SigSource() SigSource {
	s := SigSource(k)
	if !s.Valid() {
		return SrcUnknown
	}
	return s
}

// SensorHealth is one /health.json sensors[] entry (SPEC-TYPES §3.5).
type SensorHealth struct {
	Sensor        SensorKind `json:"sensor"`
	Enabled       bool       `json:"enabled"`
	Degraded      bool       `json:"degraded"`
	Reason        string     `json:"reason"`          // "" when healthy; else human-readable cause
	LastSuccessTS string     `json:"last_success_ts"` // RFC3339 UTC
	LastEventTS   string     `json:"last_event_ts"`
	LastEventAgeS float64    `json:"last_event_age_s"`
	EventsTotal   uint64     `json:"events_total"`
	Gaps          int        `json:"gaps"`    // gap records emitted since boot
	Dropped       uint64     `json:"dropped"` // bounded-queue drops
}

// SensorEvent is the normalized observation a sensor emits (SPEC-TYPES §3.5).
type SensorEvent struct {
	ID     string         `json:"id"`     // ev_ + ULID
	TS     string         `json:"ts"`     //
	Sensor SensorKind     `json:"sensor"` //
	Scope  string         `json:"scope"`  // "cpu" | "memory" | "io" | unit name | mount | path | timer unit
	Sig    Sig            `json:"sig"`    //
	Value  float64        `json:"value"`  //
	Unit   string         `json:"unit"`   // "pct" | "bytes" | "count" | "seconds" | ""
	Detail map[string]any `json:"detail"` //
	Wake   bool           `json:"wake"`   // true when produced by a PSI trigger wake (accelerator), false when sampled
}

// Rule is the rule TOML schema (SPEC-TYPES §3.5, SPEC-03 §3.5). The toml tags
// are additive: SPEC-03 §3.5 requires the file to decode into []Rule, and the
// JSON tags are the contractual wire form used by /rules and SPEC-06.
type Rule struct {
	Name       string      `json:"name" toml:"name"`
	Enabled    bool        `json:"enabled" toml:"enabled"`
	Source     SigSource   `json:"source" toml:"source"`
	Match      []Condition `json:"match" toml:"match"`
	For        Duration    `json:"for" toml:"for"` // stabilization: condition must hold continuously
	EntryRung  Rung        `json:"entry_rung" toml:"entry_rung"`
	Severity   Severity    `json:"severity" toml:"severity"`
	Cooldown   Duration    `json:"cooldown" toml:"cooldown"`
	MaxRuns    int         `json:"max_runs" toml:"max_runs"` // play retries before next rung
	VerifyWin  Duration    `json:"verify_window" toml:"verify_window"`
	AutoGrants []string    `json:"auto_grants" toml:"auto_grants"` // module/scope names auto-granted (assisted mode)
	Hotfix     bool        `json:"hotfix" toml:"hotfix"`           // eligible for the hot-fix lane (SPEC-08)
}

// Condition is one rule match entry (SPEC-TYPES §3.5, SPEC-03 §3.5).
type Condition struct {
	Field     string `json:"field" toml:"field"` // e.g. some_avg10, full_avg60, value, count, substr, unit_substate, free_pct
	Op        string `json:"op" toml:"op"`       // == != >= <= > < ~ (regex) in
	Value     string `json:"value" toml:"value"`
	ValueType string `json:"value_type" toml:"value_type"` // number | string | bool
}

// BreakerState is the storm-breaker state (SPEC-03 §3.8).
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// Breaker is a storm breaker's observable state (SPEC-TYPES §3.5, SPEC-03 §3.8).
type Breaker struct {
	Scope     string       `json:"scope"` // rule:<name> | sig:<sig> | source:<kind> | global
	State     BreakerState `json:"state"` // closed | open | half_open
	OpenedTS  string       `json:"opened_ts"`
	OpenUntil string       `json:"open_until"`
	Trips     int          `json:"trips"`
	Reason    string       `json:"reason"`
}

// ConfigValue is a resolved configuration entry (SPEC-TYPES §3.14). SPEC-12
// owns precedence (flag > env > file > default); SPEC-03 consumes the resolved
// set in New (SPEC-03 §2).
type ConfigValue struct {
	Key       string `json:"key"`        // dotted path, e.g. sentinel.bind
	Value     any    `json:"value"`      //
	Source    string `json:"source"`     // flag | env | file | default
	SourceRef string `json:"source_ref"` // "--sentinel-bind" | "TROUBLE_SENTINEL_BIND" | "/etc/trouble/config.toml:12" | "builtin"
	Redacted  bool   `json:"redacted"`
}
