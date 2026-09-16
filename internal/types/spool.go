package types

// SpoolEntry is the on-disk / in-memory spool entry (SPEC-TYPES §3.14). SPEC-12
// §3.14 owns the shape; internal/issues (SPEC-09 §3.7) and internal/lifecycle
// are its two consumers, so the shape is declared here once — a consumer
// extends this file rather than redeclaring the type.
type SpoolEntry struct {
	ID        string `json:"id"` // ev_ + ULID
	TS        string `json:"ts"`
	Kind      string `json:"kind"` // forward | issue | spawn | skill
	Payload   []byte `json:"payload"`
	Attempts  int    `json:"attempts"`
	IdemKey   string `json:"idem_key"`
	NextTryTS string `json:"next_try_ts"`
}

// Spool kind tokens (§3.7 / §3.14).
const (
	SpoolForward = "forward"
	SpoolIssue   = "issue"
	SpoolSpawn   = "spawn"
	SpoolSkill   = "skill"
)

// Gap cause tokens owned by SPEC-09/SPEC-12 (the open set lives in §3.7).
const (
	CauseDriverDown    = "driver_down"
	CauseQueueOverflow = "queue_overflow"
)
