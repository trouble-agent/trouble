package types

// SPEC-TYPES §3.15.12a — the agent-stage LLM outcome (SPEC-05 §3.7a, §3.12a).
//
// These four shapes are the whole of what crosses the LLM boundary: a buffered
// completion, the candidate that served it, the token accounting the budget was
// checked against, and the ordered attempt list that explains a failover. They
// are shared types because internal/llm produces them and internal/ladder records
// them, and neither package may import the other (SPEC-05 §4.2 keeps the ladder's
// dependency direction one-way).
//
// No field here can carry a credential: `Endpoint` is the credential-free base URL
// and a key is named by a `key_ref`, never by value (SPEC-05 §4.3a).

// LLMUsage is the token accounting of one completion. `Estimated` is true when a
// count came from the byte-based estimator rather than from the provider's own
// `usage` object — an estimate is never presented as a measurement.
type LLMUsage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated"`
}

// LLMAttempt is one candidate's try inside the ordered fallback chain. `Class` is
// the failure class ("" when the candidate served), `Status` the HTTP status (0
// when no response arrived) and `Reason` the stable token that says why.
type LLMAttempt struct {
	Candidate string `json:"candidate"`
	Model     string `json:"model"`
	Status    int    `json:"status"`
	Class     string `json:"class"`
	Reason    string `json:"reason"`
	LatencyMS int    `json:"latency_ms"`
}

// LLMCompaction is the accounting of one context-compaction pass. It is always
// present on the outcome of a run whose assembled context was over budget:
// `Applied` false with zero counts means the context fit, which is a fact worth
// recording rather than omitting.
type LLMCompaction struct {
	Applied   bool   `json:"applied"`
	Chunks    int    `json:"chunks"`
	InTokens  int    `json:"in_tokens"`
	OutTokens int    `json:"out_tokens"`
	MaxChunks int    `json:"max_chunks"`
	Candidate string `json:"candidate"`
	Model     string `json:"model"`
}

// AgentOutcome is the result of one buffered agent-stage completion.
type AgentOutcome struct {
	Text string `json:"text"`
	// Candidate is the name of the chain entry that served the request ("";
	// never when FailureClass is set).
	Candidate  string        `json:"serving_candidate"`
	Model      string        `json:"model"`
	Endpoint   string        `json:"endpoint"`
	Usage      LLMUsage      `json:"usage"`
	Compaction LLMCompaction `json:"compaction"`
	Attempts   []LLMAttempt  `json:"attempts"`
	// FailureClass is "" on success, else the class of the failure that ended the
	// chain (SPEC-05 §3.7a); the ladder mirrors it as the run's failure_class.
	FailureClass string `json:"failure_class"`
}
