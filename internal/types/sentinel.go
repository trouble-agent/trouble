package types

// Types contributed by SPEC-04 (SPEC-TYPES §3.6, §3.12) as consumed by SPEC-02:
// the scrub engine needs Project (shield source, per-project overrides) and
// ForwardEnvelope (satellite send path and hub receive path). Field names,
// order and JSON tags are verbatim from SPEC-TYPES; the sentinel owner extends
// this file rather than redefining these.

// LossPolicy is a project's loss policy on overload (SPEC-TYPES §3.6).
type LossPolicy string

const (
	LossSample       LossPolicy = "sample"            // deterministic 1/N sampling, counted
	LossDropCounter  LossPolicy = "drop-with-counter" // reject + 429, counted
	LossSpoolIfLight LossPolicy = "spool-if-light"    // spool to disk when spool budget allows
)

// Valid reports whether p is one of the three pinned loss policies.
func (p LossPolicy) Valid() bool {
	switch p {
	case LossSample, LossDropCounter, LossSpoolIfLight:
		return true
	}
	return false
}

// Project is a sentinel project (SPEC-TYPES §3.6).
type Project struct {
	ID         string     `json:"id"` // numeric-as-string, 1..2^31-1; path element in the DSN
	Slug       string     `json:"slug"`
	PublicKey  string     `json:"public_key"` // 32 hex; the ONLY unredacted credential in the system
	SecretKey  string     `json:"secret_key"` // 32 hex; "" when unused for this project
	AuthForms  []string   `json:"auth_forms"` // x_sentry_auth | query_sentry_key | envelope_dsn
	QuotaEPM   int        `json:"quota_epm"`
	DiskBudget int64      `json:"disk_budget_bytes"`
	LossPolicy LossPolicy `json:"loss_policy"`
	Enabled    bool       `json:"enabled"`
	CreatedTS  string     `json:"created_ts"`
}

// ForwardEnvelope is the satellite→hub (or proxy→hub) wire format
// (SPEC-TYPES §3.7). It is the THE SENTINEL WIRE FORMAT.
type ForwardEnvelope struct {
	ProtocolVersion int      `json:"protocol_version"` // 1
	IdempotencyKey  string   `json:"idempotency_key"`  // sig + "|" + norm_version + "|" + host_id
	HostID          string   `json:"host_id"`
	HubID           string   `json:"hub_id"`
	Origin          Origin   `json:"origin"`
	Records         []Record `json:"records"` // complete ledger records, hub assigns canonical ids
	Ack             uint64   `json:"ack"`     // ordered cursor: highest hub seq the satellite may forget
}

// SentryEvent is a scrubbed, canonicalized SDK event as stored in an `event`
// record (SPEC-TYPES §3.6).
type SentryEvent struct {
	ID          string   `json:"id"` // SDK native event_id (32 hex), stored as native_id
	RecID       string   `json:"rec_id"`
	Project     string   `json:"project"`
	TS          string   `json:"ts"`
	Level       string   `json:"level"` // fatal|error|warning|info|debug
	Message     string   `json:"message"`
	Culprit     string   `json:"culprit"`
	Stack       string   `json:"stack"`       // canonicalized frames, already scrubbed
	Fingerprint []string `json:"fingerprint"` // SDK-supplied strings, may contain "{{ default }}"
	Release     string   `json:"release"`
	Env         string   `json:"env"`
	Sig         Sig      `json:"sig"`
	Redactions  int      `json:"redactions"`
	ItemTypes   []string `json:"item_types"` // every item type seen in the envelope, incl. dropped ones
}

// ClientReport is the SDK's own accounting of events it dropped before sending
// (SPEC-TYPES §3.6). It is data, not silence.
type ClientReport struct {
	Project   string         `json:"project"`
	TS        string         `json:"ts"`
	Discarded []DiscardCount `json:"discarded"`
}

// DiscardCount is one reason/category bucket of a client report.
type DiscardCount struct {
	Reason   string `json:"reason"`
	Category string `json:"category"` // error | transaction | session | attachment | other
	Quantity int    `json:"quantity"`
}

// RateLimitDecision is the sentinel's answer to "should this request be
// admitted", including the exact header string it emitted (SPEC-TYPES §3.6).
type RateLimitDecision struct {
	Allowed     bool     `json:"allowed"`
	Reason      string   `json:"reason"` // quota_epm | disk_budget | breaker | kill_switch | overloaded
	RetryAfterS int      `json:"retry_after_s"`
	Categories  []string `json:"categories"`
	Header      string   `json:"header"` // exact X-Sentry-Rate-Limits value emitted
}

// ProjectRuntime is per-project runtime state for SPEC-10 (quota usage, canary,
// loss) — SPEC-04 §3.10.
type ProjectRuntime struct {
	Project              string         `json:"project"`
	QuotaEPM             int            `json:"quota_epm"`
	WindowS              float64        `json:"window_s"`
	EventsWindow         int            `json:"events_window"`
	Remaining            int            `json:"remaining"`
	RejectedTotal        uint64         `json:"rejected_total"`
	DroppedTotal         uint64         `json:"dropped_total"`
	SpooledTotal         uint64         `json:"spooled_total"`
	SampledTotal         uint64         `json:"sampled_total"`
	LegacyStoreTotal     uint64         `json:"legacy_store_total"`
	UnknownItemsTotal    uint64         `json:"unknown_items_total"`
	ClientReportDiscards map[string]int `json:"client_report_discards"`
	AuthForms            []string       `json:"auth_forms"`
	LastEventTS          string         `json:"last_event_ts"`
	CanaryLastTS         string         `json:"canary_last_ts"`
	CanaryLastOK         bool           `json:"canary_last_ok"`
	DiskBytes            int64          `json:"disk_bytes"`
	DiskBudgetBytes      int64          `json:"disk_budget_bytes"`
}

// CollectorParser is the config + health surface for the log collectors
// (SPEC-04 §3.5, §3.10).
type CollectorParser struct {
	Name          string   `json:"name"` // "go-panic" | "py-traceback" | "node-reject"
	Enabled       bool     `json:"enabled"`
	Kind          string   `json:"kind"` // multiline
	StartPattern  string   `json:"start_pattern"`
	Continuation  []string `json:"continuation"` // RE2 list
	FlushTimeout  Duration `json:"flush_timeout"`
	MaxEventBytes int      `json:"max_event_bytes"`
	Level         string   `json:"level"`
	SigFields     []string `json:"sig_fields"`
	Sources       []string `json:"sources"` // "journal:<unit>" | "file:<path>"
}

// SourceLiveness is the per-source liveness expectation consumed by
// verification and the dashboard (SPEC-TYPES §3.12). It is declared here
// because SPEC-04 produces it (`Server.Sources`); SPEC-10 consumes it and the
// definition lives in exactly one place.
type SourceLiveness struct {
	HostID        string  `json:"host_id"`
	Source        string  `json:"source"`
	Zone          string  `json:"zone"`
	Expected      bool    `json:"expected"`
	Alive         bool    `json:"alive"`
	LastEventTS   string  `json:"last_event_ts"`
	LastEventAgeS float64 `json:"last_event_age_s"`
	MaxAgeS       float64 `json:"max_age_s"`
}
