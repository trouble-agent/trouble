package sensors

// rules_export.go publishes the rule-set generation counter to the composition
// root. Rules() itself already exists (rules.go): the compiled set stays private,
// so a caller gets copies of the rules and never the evaluator's internals.
//
// The ladder caches rule metadata (entry rung, cooldown, max runs, verify
// window) and must invalidate that cache when a SIGHUP reload swaps the set — the
// generation counter is what makes the invalidation exact instead of time-based.

// RulesGeneration returns the number of rule-set generations installed since
// boot. It is monotone and zero before the first load.
func (s *Sensors) RulesGeneration() uint64 { return s.ruleGen.Load() }
