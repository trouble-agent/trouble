package research

// cost.go — per-rung cost accounting (SPEC-07 §3.8).
//
// Research spends zero agent tokens: the lab's solve burns the lab's compute.
// The rung records its own HTTP cost, and when a brief resolves the incident
// without an agent run it records the agent budget that went unspent — flagged
// as an estimate, because it is one.

import "github.com/trouble-agent/trouble/internal/types"

// researchCost is `payload.cost` of a `research` record.
type researchCost struct {
	Requests        int   `json:"requests"`
	DiscoverReqs    int   `json:"discover_requests"`
	SubmitReqs      int   `json:"submit_requests"`
	PollReqs        int   `json:"poll_requests"`
	ProbeReqs       int   `json:"probe_requests"`
	BytesIn         int64 `json:"bytes_in"`
	BytesOut        int64 `json:"bytes_out"`
	WallMS          int64 `json:"wall_ms"`
	CacheHits       int   `json:"cache_hits"`
	Submits         int   `json:"submits"`
	Polls           int   `json:"polls"`
	CorpusGreps     int   `json:"corpus_greps"`
	CacheHit        bool  `json:"cache_hit"`
	TokensSavedIn   int64 `json:"tokens_saved_in"`
	TokensSavedOut  int64 `json:"tokens_saved_out"`
	TokensEstimated bool  `json:"tokens_estimated"`
}

// asMap renders the accounting for the ledger payload. The key set is pinned by
// §3.8, so the record is comparable across rungs.
func (c *researchCost) asMap() map[string]any {
	return map[string]any{
		"requests": c.Requests, "discover_requests": c.DiscoverReqs,
		"submit_requests": c.SubmitReqs, "poll_requests": c.PollReqs,
		"probe_requests": c.ProbeReqs, "bytes_in": c.BytesIn, "bytes_out": c.BytesOut,
		"wall_ms": c.WallMS, "cache_hits": c.CacheHits, "submits": c.Submits,
		"polls": c.Polls, "corpus_greps": c.CorpusGreps, "cache_hit": c.CacheHit,
		"tokens_saved_in": c.TokensSavedIn, "tokens_saved_out": c.TokensSavedOut,
		"tokens_estimated": c.TokensEstimated,
	}
}

// charge records one HTTP request and its approximate body sizes. The sizes are
// the request/response bodies trouble sees; they are estimates for the ledger,
// never a wire measurement.
func (c *researchCost) charge(kind string, bytesIn, bytesOut int64) {
	c.Requests++
	c.BytesIn += bytesIn
	c.BytesOut += bytesOut
	switch kind {
	case "discover":
		c.DiscoverReqs++
	case "submit":
		c.SubmitReqs++
	case "poll":
		c.PollReqs++
	case "probe":
		c.ProbeReqs++
	}
}

// savedByBrief records the agent budget a brief made unnecessary (25000 /
// 8000 by default), flagged as an estimate: SPEC-05 owns that budget and
// research never debits it.
func (c *researchCost) savedByBrief(in, out int64) {
	c.TokensSavedIn = in
	c.TokensSavedOut = out
	c.TokensEstimated = true
}

// budgetOf is the research request budget: debited on submit only. discover,
// corpus grep and probes are free (§3.8).
func budgetOf(cfg config) int64 {
	if cfg.RequestsPerDay <= 0 {
		return 0
	}
	return cfg.RequestsPerDay
}

// costOfOutcome exposes the outcome's digest pair for the caller's own records
// (SPEC-05 stores both on the agent_run record).
func digestsOf(brief map[string]any, prompt string) (briefDigest, promptDigest string) {
	if brief != nil {
		briefDigest = Digest(canonicalJSON(brief))
	}
	if prompt != "" {
		promptDigest = Digest([]byte(prompt))
	}
	return briefDigest, promptDigest
}

// briefExtras is the outcome subset SPEC-08's ForemanBrief.ResearchBrief reads
// (SPEC-07 §3.6): {submission_id, slug, state, brief}.
func briefExtras(out types.ResearchOutcome) map[string]any {
	m := map[string]any{
		"submission_id": out.SubmissionID,
		"slug":          out.Slug.Slug,
		"state":         out.State,
	}
	if out.Brief != nil {
		m["brief"] = out.Brief
	}
	return m
}
