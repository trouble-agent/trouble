package sensors

// ladderport.go exports the shared condition language (SPEC-03 §3.4) to the two
// consumers SPEC-INDEX §4.1 pins: internal/ladder's default RuleEvaluator
// (SPEC-05 §2, §4.2) and internal/registry's play `when:` evaluator (SPEC-06
// §3.7 — "the same package and the same covered operators", no second dialect).
//
// The engine stays private: callers get an opaque predicate value, so neither
// consumer can grow a second parser.

import (
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// RulePredicate is a compiled rule match list: true when every match entry holds
// for the variable map (SPEC-03 §3.4, conjunction of the `match` table).
type RulePredicate func(vars map[string]any) bool

// PlayPredicate is a compiled play `when:` expression (SPEC-06 §3.7).
type PlayPredicate func(vars map[string]any) bool

// CompileRuleMatch compiles a rule's match list under its source namespace.
// An empty match list is a predicate that is always true (the rule's own
// admission gate is the `for=` window, not the match list).
func CompileRuleMatch(r types.Rule) (RulePredicate, error) {
	scope := compileScope{source: r.Source, scopes: inferScopes(r.Match)}
	progs := make([]exprProgram, 0, len(r.Match))
	for _, c := range r.Match {
		prog, err := compileCondition(c, scope)
		if err != nil {
			return nil, err
		}
		progs = append(progs, prog)
	}
	return func(vars map[string]any) bool {
		if len(progs) == 0 {
			return true
		}
		sc := newEvalScope(toVars(vars))
		for _, p := range progs {
			if !p.eval(sc) {
				return false
			}
		}
		return true
	}, nil
}

// CompilePlayWhen compiles a play `when:` expression (SPEC-06 §3.7) under the
// play namespace, with the play's own register names resolvable as dynamic
// values. An empty expression compiles to a predicate that is always true.
func CompilePlayWhen(src string, registerNames []string) (PlayPredicate, error) {
	if strings.TrimSpace(src) == "" {
		return func(map[string]any) bool { return true }, nil
	}
	scope := compileScope{playScope: true, playRegisters: map[string]bool{}}
	for _, n := range registerNames {
		if n != "" {
			scope.playRegisters[n] = true
		}
	}
	prog, err := compileExpression(src, scope)
	if err != nil {
		return nil, err
	}
	return func(vars map[string]any) bool {
		return prog.eval(newEvalScope(toVars(vars)))
	}, nil
}

// toVars converts a caller's variable map into the engine's internal value form.
// Values the engine cannot type are dropped (a missing variable is false, never
// an error — SPEC-03 §3.4).
func toVars(in map[string]any) map[string]value {
	out := make(map[string]value, len(in))
	for k, raw := range in {
		if v, ok := toValue(raw); ok {
			out[k] = v
		}
	}
	return out
}
