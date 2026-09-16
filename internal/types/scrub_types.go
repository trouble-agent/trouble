package types

// ScrubResult is the scrubbing outcome (SPEC-TYPES §3.3). SPEC-02 owns this
// type; it is declared here because SPEC-03's §2 seam
//
//	redact func([]byte, string) (types.ScrubResult, error)
//
// is the injected scrub boundary and internal/sensors must compile against the
// shared type rather than invent a second shape (SPEC-INDEX §4.2).
//
// MERGE NOTE: when SPEC-02 (internal/scrub, which owns ScrubRule/ScrubResult)
// lands, delete this file and keep SPEC-02's declaration — it is a separate
// file precisely so the two branches do not conflict inside one file.

// ScrubResult is the outcome of one scrub call (SPEC-TYPES §3.3).
type ScrubResult struct {
	Value      []byte         `json:"value"`      // scrubbed bytes (never logged raw)
	Redactions int            `json:"redactions"` //
	ByRule     map[string]int `json:"by_rule"`    // rule name → count (counts only, never values)
	Truncated  bool           `json:"truncated"`  //
	BytesIn    int            `json:"bytes_in"`   //
}
