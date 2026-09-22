// Package types is the shared type source of truth for trouble (SPEC-TYPES).
//
// This file implements the subset of SPEC-TYPES §3 consumed by internal/ledger
// (SPEC-01) plus the §2 helper surface. Definitions are verbatim from SPEC-TYPES:
// field names, order and JSON tags are contractual and must not drift.
package types

import "time"

// ActorKind enumerates who acted (SPEC-TYPES §3.1).
type ActorKind string

const (
	ActorDaemon    ActorKind = "daemon"
	ActorAgent     ActorKind = "agent"
	ActorPlay      ActorKind = "play"
	ActorHuman     ActorKind = "human"
	ActorSatellite ActorKind = "satellite"
	ActorExternal  ActorKind = "external"
)

// Origin is the provenance of a record. host_id and source are required.
type Origin struct {
	HostID string `json:"host_id"`
	HubID  string `json:"hub_id"`
	Source string `json:"source"`
	// Route is how the event reached the system (SPEC-TYPES §3.1, SPEC-04
	// §3.10a): "A" direct (sensor → local daemon over loopback) or "B" proxied
	// (satellite forward path). It is empty for every non-sensor path, which
	// is why it is omitempty: a record that never touched the sensor transport
	// marshals byte-identically to a pre-AC-28 producer (the §7.6
	// no-schema-change guarantee), and §3.10a's A case is stamped explicitly —
	// "no relay" is as much a fact as a relay.
	Route string `json:"route,omitempty"`
}

// Actor identifies the process that wrote a record.
type Actor struct {
	Kind      ActorKind `json:"kind"`
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	GitSHA    string    `json:"git_sha"`
	BuildTime string    `json:"build_time"`
}

// Prefix enumerates the frozen id prefixes (SPEC-TYPES §3.1, §6.2).
type Prefix string

const (
	PInc   Prefix = "inc_"
	PGrp   Prefix = "grp_"
	PIss   Prefix = "iss_"
	PTsk   Prefix = "tsk_"
	PSk    Prefix = "sk_"
	PEv    Prefix = "ev_"
	PRes   Prefix = "res_"
	PSpawn Prefix = "sp_"
)

// RecordKind enumerates the 17 record kinds (SPEC-TYPES §3.2).
type RecordKind string

const (
	KEvent     RecordKind = "event"
	KGroup     RecordKind = "group"
	KIncident  RecordKind = "incident"
	KPlayRun   RecordKind = "play_run"
	KAgentRun  RecordKind = "agent_run"
	KToolCall  RecordKind = "tool_call"
	KResearch  RecordKind = "research"
	KVerify    RecordKind = "verify"
	KIssue     RecordKind = "issue"
	KFlow      RecordKind = "flow"
	KSpawn     RecordKind = "spawn"
	KSkill     RecordKind = "skill"
	KBreaker   RecordKind = "breaker"
	KGap       RecordKind = "gap"
	KCanary    RecordKind = "canary"
	KConfig    RecordKind = "config"
	KLifecycle RecordKind = "lifecycle"
)

// RecordKinds lists every valid kind in SPEC-TYPES order.
var RecordKinds = []RecordKind{
	KEvent, KGroup, KIncident, KPlayRun, KAgentRun, KToolCall, KResearch, KVerify,
	KIssue, KFlow, KSpawn, KSkill, KBreaker, KGap, KCanary, KConfig, KLifecycle,
}

// Valid reports whether k is one of the 17 frozen kinds.
func (k RecordKind) Valid() bool {
	for _, v := range RecordKinds {
		if v == k {
			return true
		}
	}
	return false
}

// SigRequired reports whether a kind may not carry an empty sig (SPEC-01 §3.1).
func (k RecordKind) SigRequired() bool {
	switch k {
	case KEvent, KIncident, KVerify, KPlayRun:
		return true
	}
	return false
}

// SpineKind reports whether the kind is an audit-spine kind: its payload may
// never be truncated or expired (SPEC-01 §3.1, §3.7 rule 2).
func (k RecordKind) SpineKind() bool {
	switch k {
	case KIncident, KGroup, KVerify, KPlayRun, KAgentRun, KToolCall, KResearch,
		KIssue, KFlow, KSpawn, KSkill, KBreaker, KConfig, KLifecycle, KGap, KCanary:
		return true
	}
	return false
}

// Record is the ledger wire shape (SPEC-TYPES §3.2).
type Record struct {
	Seq           uint64         `json:"seq"`
	RecID         string         `json:"rec_id"`
	TS            string         `json:"ts"`
	Kind          RecordKind     `json:"kind"`
	SchemaVersion int            `json:"schema_version"`
	Sig           string         `json:"sig"`
	Inc           string         `json:"inc,omitempty"`
	Origin        Origin         `json:"origin"`
	Actor         Actor          `json:"actor"`
	Redactions    int            `json:"redactions"`
	Payload       map[string]any `json:"payload"`
	// Codeplane is the in-memory cross-plane bundle the sentinel attaches to a
	// `group` record at its admission seam (SPEC-04 §3.9a): it rides the record
	// to the ladder bridge, which persists it as Incident.Codeplane (SPEC-05
	// §3.13a). It is not serialized on the ledger line — the incident record is
	// the bundle's copy of record.
	Codeplane *CodeplaneContext `json:"-"`
}

// RecordDraft is what a producer hands to Ledger.Append (SPEC-TYPES §3.15.1).
// Seq, RecID, TS and SchemaVersion are allocated by the ledger and therefore
// have no representation here.
type RecordDraft struct {
	Kind       RecordKind     `json:"kind"`
	Sig        string         `json:"sig"`
	Inc        string         `json:"inc"`
	Origin     Origin         `json:"origin"`
	Actor      Actor          `json:"actor"`
	Redactions int            `json:"redactions"`
	Payload    map[string]any `json:"payload"`
	// Codeplane is an in-memory ride-along for the cross-plane bundle
	// (SPEC-04 §3.9a → SPEC-05 §3.13a): the sentinel attaches it to a `group`
	// draft and the composition root's sink hands it to the ladder bridge. It
	// is never serialized on the ledger line — the incident record carries the
	// bundle's copy of record.
	Codeplane *CodeplaneContext `json:"-"`
}

// Duration is the canonical duration encoding: a Go duration string.
type Duration string

// Seconds parses the duration; an unparsable or empty value yields 0.
func (d Duration) Seconds() float64 {
	if d == "" {
		return 0
	}
	t, err := time.ParseDuration(string(d))
	if err != nil {
		return 0
	}
	return t.Seconds()
}

// Std parses the duration, returning zero on failure.
func (d Duration) Std() time.Duration {
	if d == "" {
		return 0
	}
	t, err := time.ParseDuration(string(d))
	if err != nil {
		return 0
	}
	return t
}

// GroupCounters are the never-dropped aggregates of a group (SPEC-TYPES §3.7).
type GroupCounters struct {
	Events     uint64  `json:"events"`
	Suppressed uint64  `json:"suppressed"`
	Redacted   uint64  `json:"redacted_values"`
	Dropped    uint64  `json:"dropped_events"`
	SampleRate float64 `json:"sample_rate"`
}

// Group is the dedup group projection (SPEC-TYPES §3.7).
type Group struct {
	ID           string        `json:"id"`
	Sig          string        `json:"sig"`
	Digest       string        `json:"digest"`
	Source       SigSource     `json:"source"`
	Title        string        `json:"title"`
	FirstSeenTS  string        `json:"first_seen_ts"`
	LastSeenTS   string        `json:"last_seen_ts"`
	Count        uint64        `json:"count"`
	Counters     GroupCounters `json:"counters"`
	ReleaseRange []string      `json:"release_range"`
	IncidentID   string        `json:"incident_id"`
	CompactedTS  string        `json:"compacted_ts"`
}

// Severity is the rule severity vocabulary (SPEC-TYPES §3.7).
type Severity string

const (
	SevCritical Severity = "critical"
	SevHigh     Severity = "high"
	SevMedium   Severity = "medium"
	SevLow      Severity = "low"
	SevInfo     Severity = "info"
)

// Rung is the ladder entry rung (SPEC-TYPES §3.7).
type Rung string

const (
	RungRecord   Rung = "record"
	RungPlay     Rung = "play"
	RungResearch Rung = "research"
	RungAgent    Rung = "agent"
	RungOutlets  Rung = "outlets"
)

// LadderState is the ladder state machine's state (SPEC-TYPES §3.7).
type LadderState string

const (
	StDetected       LadderState = "detected"
	StRecorded       LadderState = "recorded"
	StPlayDrafted    LadderState = "play:drafted"
	StPlayCheck      LadderState = "play:check_only"
	StPlayApplied    LadderState = "play:applied"
	StPlayFailed     LadderState = "play:failed"
	StResRequested   LadderState = "research:requested"
	StResReturned    LadderState = "research:returned"
	StResDegraded    LadderState = "research:degraded"
	StResSkipped     LadderState = "research:skipped"
	StAgentRunning   LadderState = "agent:running"
	StAgentDone      LadderState = "agent:done"
	StAgentFailed    LadderState = "agent:failed"
	StAgentSuspended LadderState = "agent:suspended"
	StVerifying      LadderState = "verifying"
	StResolved       LadderState = "resolved"
	StEscalated      LadderState = "escalated"
	StSuppressed     LadderState = "suppressed"
	StQuarantined    LadderState = "quarantined"
)

// VerifyResultKind is the evidence-tuple result (SPEC-TYPES §3.7).
type VerifyResultKind string

const (
	VerifyPassed  VerifyResultKind = "passed"
	VerifyFailed  VerifyResultKind = "failed"
	VerifyInvalid VerifyResultKind = "invalid"
)

// Incident is the incident projection stored by the ledger (SPEC-TYPES §3.7).
type Incident struct {
	ID          string            `json:"id"`
	Sig         string            `json:"sig"`
	GroupID     string            `json:"grp"`
	State       LadderState       `json:"state"`
	EntryRung   Rung              `json:"entry_rung"`
	Rung        Rung              `json:"rung"`
	Severity    Severity          `json:"severity"`
	OpenedTS    string            `json:"opened_ts"`
	UpdatedTS   string            `json:"updated_ts"`
	ResolvedTS  string            `json:"resolved_ts"`
	ReopenCount int               `json:"reopen_count"`
	PlayRuns    int               `json:"play_runs"`
	AgentRuns   int               `json:"agent_runs"`
	VerifyWin   Duration          `json:"verify_window"`
	LeaseID     string            `json:"lease_id"`
	IssueID     string            `json:"issue_id"`
	TaskID      string            `json:"task_id"`
	ResearchID  string            `json:"research_id"`
	Codeplane   *CodeplaneContext `json:"codeplane,omitempty"` // cross-plane bundle (SPEC-05 §3.13a)
	Evidence    *Evidence         `json:"evidence,omitempty"`
}

// CodeplaneContext is the sensor⇄sentinel cross-plane bundle (SPEC-TYPES
// §3.15.12, SPEC-05 §3.13a). The two planes write disjoint fields, so a bundle
// is never a merge decision: the sentinel half is assembled on a sentinel
// Admit (SPEC-04 §3.9a), the sensor half on a sensor-born one. The bundle is
// context, never evidence — it changes no rung, no gate and no verification
// input — and it is copied out verbatim to the research request (SPEC-07
// §3.10a, `context.codeplane`) and the issue body (SPEC-09 §3.13a).
type CodeplaneContext struct { // SPEC-05 §3.13a — the sensor⇄sentinel cross-plane bundle (§3.15.12)
	Side      string            `json:"side"`                // "sentinel" | "sensor" — which plane produced it
	Sig       string            `json:"sig,omitempty"`       // sentinel: the group signature
	GroupID   string            `json:"grp,omitempty"`       // sentinel: the group id (SPEC-04)
	Project   string            `json:"project,omitempty"`   // sentinel: project id
	Release   string            `json:"release,omitempty"`   // sentinel: the running release of the errored code
	Regressed bool              `json:"regressed,omitempty"` // sentinel: release regression currently open
	Recent    []SigCount        `json:"recent,omitempty"`    // sentinel: top-5 recent signatures, count-descending
	Sample    string            `json:"sample,omitempty"`    // sentinel: rec_id of the representative event
	RuleID    string            `json:"rule_id,omitempty"`   // sensor: the rule that fired
	Readings  map[string]string `json:"readings,omitempty"`  // sensor: rule id / metric → stabilized reading ("io.full.avg10":"3.11")
	TS        string            `json:"ts"`                  // bundle assembly time (RFC3339 ms UTC)
}

// SigCount is one recent-signature counter of the codeplane bundle's `Recent`
// slice (SPEC-TYPES §3.15.12).
type SigCount struct {
	Sig   string `json:"sig"`
	Count int64  `json:"count"`
	First string `json:"first_ts"`
	Last  string `json:"last_ts"`
}

// Evidence is the verification evidence tuple (never a boolean).
type Evidence struct {
	TSWindowStart   string           `json:"ts_window_start"`
	TSWindowEnd     string           `json:"ts_window_end"`
	WindowS         float64          `json:"window_s"`
	EventsObserved  int              `json:"events_observed"`
	CanarySeen      bool             `json:"canary_seen"`
	CanaryID        string           `json:"canary_id"`
	CounterDeltas   map[string]int64 `json:"counter_deltas"`
	SourcesExpected []string         `json:"sources_expected"`
	SourcesAlive    []string         `json:"sources_alive"`
	SourcesQuiet    []string         `json:"sources_quiet"`
	SourcesMissing  []string         `json:"sources_missing"`
	Zone            string           `json:"zone"`
	Result          VerifyResultKind `json:"result"`
}

// GapRecord is a documented dropout (SPEC-TYPES §3.7).
type GapRecord struct {
	ID      string `json:"id"`
	Sensor  string `json:"sensor"`
	Scope   string `json:"scope"`
	FromTS  string `json:"from_ts"`
	ToTS    string `json:"to_ts"`
	EstLost int    `json:"est_lost"`
	Cause   string `json:"cause"`
}
