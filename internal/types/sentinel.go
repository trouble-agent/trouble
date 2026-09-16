package types

// Types contributed by SPEC-04 (SPEC-TYPES §3.6) as consumed by SPEC-02: the
// scrub engine needs Project (shield source, per-project overrides) and
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
