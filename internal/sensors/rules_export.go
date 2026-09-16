package sensors

import "github.com/totalwindupflightsystems/trouble/internal/types"

// rules_export.go publishes the live rule set to the two consumers SPEC-INDEX
// §4.1 pins to the same dialect: internal/ladder (rung decisions read a rule's
// entry rung, cooldown, max runs and verify window) and the composition root.
//
// The compiled set stays private: a caller gets copies of the rules, never the
// evaluator's internals, so a second evaluation path cannot grow out of this
// accessor.

// Rules returns the live rule set, one copy per rule (empty when no set has been
// loaded yet — a daemon reads this after New/Reload, never before).
func (s *Sensors) Rules() []types.Rule {
	rs := s.rules.Load()
	if rs == nil || len(rs.rules) == 0 {
		return nil
	}
	out := make([]types.Rule, len(rs.rules))
	copy(out, rs.rules)
	return out
}

// RulesGeneration reports the generation counter of the live set; a caller that
// caches decisions beside the rules can invalidate on a change.
func (s *Sensors) RulesGeneration() uint64 {
	rs := s.rules.Load()
	if rs == nil {
		return 0
	}
	return rs.gen
}
