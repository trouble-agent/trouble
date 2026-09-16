package types

// Types contributed by SPEC-02 (SPEC-TYPES §3.3 + §3.15.2). Field names, order
// and JSON tags are verbatim and contractual.

// RuleKind enumerates the rule implementations (SPEC-TYPES §3.3).
type RuleKind string

const (
	KindRegex         RuleKind = "regex"
	KindPrefix        RuleKind = "prefix"
	KindPathAllowlist RuleKind = "path_allowlist"
	KindDSNPart       RuleKind = "dsn_part"
	KindEntropy       RuleKind = "entropy"
)

// RuleKinds lists every valid kind.
var RuleKinds = []RuleKind{KindRegex, KindPrefix, KindPathAllowlist, KindDSNPart, KindEntropy}

// Valid reports whether k is one of the five rule kinds.
func (k RuleKind) Valid() bool {
	for _, v := range RuleKinds {
		if v == k {
			return true
		}
	}
	return false
}

// ScrubRule is one entry of the ordered rule table (SPEC-TYPES §3.3).
type ScrubRule struct {
	Name      string   `json:"name"`      // e.g. bearer_token, env_assign, private_key_block
	Kind      RuleKind `json:"kind"`      // regex | prefix | path_allowlist | dsn_part | entropy
	Pattern   string   `json:"pattern"`   // "builtin" for the kind-specific scanners
	Replace   string   `json:"replace"`   // always a fixed marker: "[REDACTED:<name>]"
	Mandatory bool     `json:"mandatory"` // mandatory rules cannot be disabled by config
	Targets   []string `json:"targets"`   // the ScrubTarget values the rule applies to
}

// ScrubResult reports one scrub call (SPEC-TYPES §3.3). Counts, never values.
type ScrubResult struct {
	Value      []byte         `json:"value"`      // scrubbed bytes (never logged raw)
	Redactions int            `json:"redactions"` // Σ ByRule
	ByRule     map[string]int `json:"by_rule"`    // rule name → count (counts only, never values)
	Truncated  bool           `json:"truncated"`
	BytesIn    int            `json:"bytes_in"`
}

// ScrubTarget is a content class, not a component (SPEC-02 §3.1).
type ScrubTarget string

const (
	TgEventMsg       ScrubTarget = "event_msg"
	TgStack          ScrubTarget = "stack"
	TgHeader         ScrubTarget = "header"
	TgEnv            ScrubTarget = "env"
	TgJournalTail    ScrubTarget = "journal_tail"
	TgConfigSnapshot ScrubTarget = "config_snapshot"
	TgSkill          ScrubTarget = "skill"
	TgIssue          ScrubTarget = "issue"
	TgBoard          ScrubTarget = "board"
	TgDSN            ScrubTarget = "dsn"
	TgSpool          ScrubTarget = "spool"
)

// ScrubTargets lists the 11 targets in SPEC-02 §3.1 table order.
var ScrubTargets = []ScrubTarget{
	TgEventMsg, TgStack, TgHeader, TgEnv, TgJournalTail, TgConfigSnapshot,
	TgSkill, TgIssue, TgBoard, TgDSN, TgSpool,
}

// Valid reports whether t is one of the 11 frozen targets.
func (t ScrubTarget) Valid() bool {
	for _, v := range ScrubTargets {
		if v == t {
			return true
		}
	}
	return false
}

// ScrubStats is the cumulative process counter set (SPEC-02 §3.7).
type ScrubStats struct {
	Calls            uint64            `json:"calls"`
	BytesIn          uint64            `json:"bytes_in"`
	BytesOut         uint64            `json:"bytes_out"`
	Redactions       uint64            `json:"redactions"`
	ByRule           map[string]uint64 `json:"by_rule"`
	Truncated        uint64            `json:"truncated"`
	RefusedBytes     uint64            `json:"refused_bytes"`
	InvalidUTF8      uint64            `json:"invalid_utf8"`
	Timeouts         uint64            `json:"timeouts"`
	FailClosed       uint64            `json:"fail_closed"`
	BoundaryRefusals uint64            `json:"boundary_refusals"`
	RulesVersion     int               `json:"rules_version"`
}
