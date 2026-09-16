package types

// Types contributed by SPEC-12 (SPEC-TYPES §3.14) that other areas consume before
// that area's package lands: the durable spool entry. SPEC-12 owns the rest of
// the lifecycle contract; declaring the entry once keeps a single source of truth.

// SpoolEntry is one durable-queue row (SPEC-TYPES §3.14): the forward/issue/
// spawn/skill payload that must survive a restart, bounded drop-oldest with a
// ledger note.
type SpoolEntry struct {
	ID        string `json:"id"` // ev_ + ULID
	TS        string `json:"ts"`
	Kind      string `json:"kind"` // forward | issue | spawn | skill
	Payload   []byte `json:"payload"`
	Attempts  int    `json:"attempts"`
	IdemKey   string `json:"idem_key"`
	NextTryTS string `json:"next_try_ts"`
}

// Runtime watermark helper types SPEC-12 also owns are declared with that area;
// this file is only the cross-area seam.
