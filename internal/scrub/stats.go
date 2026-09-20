package scrub

import (
	"sync/atomic"

	"github.com/trouble-agent/trouble/internal/types"
)

// statSet is the cumulative process counter set behind Engine.Stats()
// (SPEC-02 §3.7). Only these mutate; the rule table and the shield are
// immutable after New, so N ingestion goroutines share one engine with no lock
// on the hot path.
type statSet struct {
	calls            atomic.Uint64
	bytesIn          atomic.Uint64
	bytesOut         atomic.Uint64
	redactions       atomic.Uint64
	truncated        atomic.Uint64
	refusedBytes     atomic.Uint64
	invalidUTF8      atomic.Uint64
	timeouts         atomic.Uint64
	failClosed       atomic.Uint64
	boundaryRefusals atomic.Uint64
	unmappedFields   atomic.Uint64
	exemptValues     atomic.Uint64

	names    []string
	index    map[string]int
	perRule  []atomic.Uint64
	version  int
	prefMiss atomic.Uint64
}

func newStatSet(names []string, version int) *statSet {
	s := &statSet{names: append([]string(nil), names...), index: make(map[string]int, len(names)), version: version}
	s.perRule = make([]atomic.Uint64, len(names))
	for i, n := range names {
		s.index[n] = i
	}
	return s
}

// addRule records n replacements by the rule named name. A rule name outside
// the compiled set — impossible through the public API — is dropped rather than
// allowing unbounded by_rule cardinality (§3.2 bounds the set to 64 names).
func (s *statSet) addRule(name string, n uint64) {
	if n == 0 {
		return
	}
	if i, ok := s.index[name]; ok {
		s.perRule[i].Add(n)
	}
}

func (s *statSet) snapshot() types.ScrubStats {
	byRule := make(map[string]uint64)
	for i, n := range s.names {
		if v := s.perRule[i].Load(); v > 0 {
			byRule[n] = v
		}
	}
	return types.ScrubStats{
		Calls:            s.calls.Load(),
		BytesIn:          s.bytesIn.Load(),
		BytesOut:         s.bytesOut.Load(),
		Redactions:       s.redactions.Load(),
		ByRule:           byRule,
		Truncated:        s.truncated.Load(),
		RefusedBytes:     s.refusedBytes.Load(),
		InvalidUTF8:      s.invalidUTF8.Load(),
		Timeouts:         s.timeouts.Load(),
		FailClosed:       s.failClosed.Load(),
		BoundaryRefusals: s.boundaryRefusals.Load(),
		RulesVersion:     s.version,
	}
}

// addRule counts n replacements of a rule in one call's ByRule map.
func addRule(c map[string]int, name string, n int) {
	if n <= 0 {
		return
	}
	c[name] += n
}

// countRules sums a call's ByRule map (ScrubResult.Redactions = Σ ByRule).
func countRules(c map[string]int) int {
	t := 0
	for _, v := range c {
		t += v
	}
	return t
}
