package sentinel

import (
	"sort"

	"github.com/trouble-agent/trouble/internal/types"
)

// builtinParsers is the v0.1 parser table of §3.5 in configured order. Order is
// significant: parsers are tried in this order and the first match wins
// (guard 1), so a line that two parsers could start belongs to the earlier one
// and `parser_ambiguous_total` records the overlap.
func builtinParsers() []*parserDef {
	return []*parserDef{
		goPanicParser(),
		pyTracebackParser(),
		nodeRejectParser(),
	}
}

// sortLiveness orders the liveness view deterministically (dashboard-friendly).
func sortLiveness(list []types.SourceLiveness) {
	sort.Slice(list, func(i, j int) bool { return list[i].Source < list[j].Source })
}
