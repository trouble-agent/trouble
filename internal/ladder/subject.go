package ladder

// subject.go — the incident's human-readable subject (TRBL-126).
//
// An incident row that carries only a signature id forces the operator to open
// the ledger to answer the first question of any outage: WHICH unit failed. The
// unit identifier and the message that produced the signal are both available at
// admission time (the observation carries the unit as its subject and the source
// text in its detail), so the incident records them in ONE field — a second
// parallel type would only give two surfaces two different stories.

import (
	"fmt"
	"strings"
)

// incidentSubjectText composes the incident subject: "<identifier>: <message>",
// where the identifier is the merge subject of §3.9 (the unescaped unit name for
// a unit-scoped signal, project:<id>/unit:<name> for an app-class one, the
// sensor|scope pair for a resource one) and the message is originatingMessage.
//
// The fallbacks are deliberate and named: an identifier with no message renders
// the identifier alone, a message with no identifier renders the message alone,
// and a signal with neither (an anonymous event) renders empty — every surface
// then falls back to the group title / sig id it already carries, so nothing
// renders a placeholder that reads like a fact.
func incidentSubjectText(identifier string, detail map[string]any) string {
	identifier = strings.TrimSpace(identifier)
	msg := originatingMessage(detail)
	switch {
	case identifier != "" && msg != "":
		return identifier + ": " + msg
	case identifier != "":
		return identifier
	default:
		return msg
	}
}

// originatingMessage is the message that caused the signal: the source's own
// text when it carries one (journald's masked log line, psi's raw counter text),
// else the state transition a unit-scoped D-Bus arrival reported. A failed unit
// publishes no log line of its own — its originating message IS the
// PropertiesChanged(SubState)/JobRemoved signal, and the substate it reported is
// that message's content.
func originatingMessage(detail map[string]any) string {
	if msg := strings.TrimSpace(detailString(detail, "msg")); msg != "" {
		return msg
	}
	substate := strings.TrimSpace(detailString(detail, "unit_substate"))
	path := strings.TrimSpace(detailString(detail, "arrival_path"))
	switch {
	case substate != "" && path != "":
		return fmt.Sprintf("unit_substate=%s via %s", substate, path)
	case substate != "":
		return "unit_substate=" + substate
	default:
		return ""
	}
}

// detailString reads one detail field. A sensor event carries its rule variables
// under payload.detail (the ladder's observation is the whole record payload,
// SPEC-03 §3.1), while an event built in-process may carry them flat; both are
// the same fact, so both shapes are read and neither lies about the other.
func detailString(detail map[string]any, key string) string {
	if detail == nil {
		return ""
	}
	if s, ok := detail[key].(string); ok {
		return s
	}
	if nested, ok := detail["detail"].(map[string]any); ok {
		if s, ok := nested[key].(string); ok {
			return s
		}
	}
	return ""
}
