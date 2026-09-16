package scrub

import (
	"context"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The persistence-boundary re-scan (SPEC-02 §3.4 point 3, SPEC-01 §4.2).
//
// This file keeps the narrow seam SPEC-01 was built against — MandatoryScan and
// PrefilterAllows — while the implementation behind it is now the real engine's
// mandatory set: the 13 mandatory rules of the compiled-in table, plus one
// boundary variant of the auth-header rule.
//
// The boundary check is "ONE coalesced RE2 alternation carrying the 13
// mandatory patterns" with the entropy rule deliberately excluded (§2, §3.9).
// Two things the alternation cannot express in RE2 are supplied around it:
//
//   - RE2 has no lookahead, so the alternation cannot exclude marker text.
//     A hit is therefore confirmed by the marker-aware scanners of the same
//     rules; a payload that carries only [REDACTED:…] markers is NOT refused.
//     Without that confirmation the boundary would refuse the ledger's own
//     scrubbed output, and §7's e2e step 4 ("re-run Verify over the finished
//     ledger: 0 hits") could not hold.
//   - rule 10 is line-anchored, and the serialized wire form of a header block
//     escapes its newlines. The boundary variant accepts a real newline, an
//     escaped newline (a backslash) or the start of the buffer before the
//     header name, so an unredacted Authorization/Cookie line inside a JSON
//     string is still refused.

// boundaryAuthPattern is the rule-10 boundary variant (see above).
const boundaryAuthPattern = `(?i)(?:^|[\\\n])[ \t]*(?:authorization|proxy-authorization|x-api-key|x-auth-token|x-sentry-auth|x-amz-security-token|api-key|cookie|set-cookie)[ \t]*:[ \t]*([^\s"\\]{6,})`

// boundaryAuthAnchors are the header names the boundary variant looks for.
var boundaryAuthAnchors = []string{"authorization", "proxy-authorization", "x-api-key",
	"x-auth-token", "x-sentry-auth", "x-amz-security-token", "api-key", "cookie", "set-cookie"}

var (
	boundaryRules, boundaryTrig = buildBoundaryRules()
)

// buildBoundaryRules compiles the boundary rule set — the 13 mandatory rules of
// the compiled-in table plus one boundary variant of the auth-header rule — and
// the coalesced prefilter that gates it.
func buildBoundaryRules() ([]tableRule, *trigWords) {
	t := newTrigWords()
	for i := range builtinTable {
		if builtinTable[i].mandatory {
			t.add(builtinTable[i].anchors)
		}
	}
	t.add(boundaryAuthAnchors)
	out := make([]tableRule, 0, len(mandatoryNames)+1)
	for i := range builtinTable {
		br := builtinTable[i]
		if !br.mandatory {
			continue
		}
		c, err := compileBuiltin(br, i)
		if err != nil {
			panic(fmt.Sprintf("scrub: mandatory rule %s does not compile: %v", br.name, err))
		}
		out = append(out, tableRule{compiledRule: c, gate: t.maskFor(br.anchors), signals: br.signals})
	}
	c, err := compileBuiltin(builtinRule{
		name: "auth_header_boundary", kind: kindRegex, pattern: boundaryAuthPattern,
		replace: "[REDACTED:auth_header]", mandatory: true, targets: allTargets(),
		anchors: boundaryAuthAnchors,
	}, len(out))
	if err != nil {
		panic(fmt.Sprintf("scrub: boundary auth rule does not compile: %v", err))
	}
	out = append(out, tableRule{compiledRule: c, gate: t.maskFor(boundaryAuthAnchors)})
	return out, t
}

// boundaryHitFor reports the first mandatory rule whose marker-aware scanner
// still finds a span in b. It never rewrites and never returns the bytes.
func boundaryHitFor(b []byte, sh *shield) (string, bool) {
	if len(b) == 0 || !boundaryFilterLike(b) {
		return "", false
	}
	notes := scrubNoteRanges(b)
	var matched [maxTrigMaskWords]uint64
	g := gateState{on: true, matched: matched[:boundaryTrig.words64()]}
	g.sigs = boundaryTrig.fire(b, g.matched)
	for _, r := range boundaryRules {
		if g.ruleGated(r.gate, r.signals) {
			continue
		}
		var spans []span
		switch r.kind {
		case kindRegex:
			spans = r.regexSpans(b, nil)
		case kindPrefix:
			spans = prefixSpans(b, r.begins)
		case kindDSNPart:
			if len(r.begins) > 0 && r.begins[0] == "secret" {
				spans = dsnSecretSpans(b, sh)
			} else {
				spans = dsnAnySpans(b, sh)
			}
		}
		for _, s := range spans {
			if overlapsAny(s, notes) {
				continue // the reserved payload["scrub"] note (§3.7)
			}
			return r.name, true
		}
	}
	return "", false
}

// boundaryFilterLike is the prefilter gate of the boundary rule set: it answers
// "could any mandatory rule match?" without running a rule scanner.
func boundaryFilterLike(b []byte) bool {
	if boundaryTrig.nbits == 0 {
		return true
	}
	var matched [maxTrigMaskWords]uint64
	sigs := boundaryTrig.fire(b, matched[:boundaryTrig.words64()])
	for _, r := range boundaryRules {
		g := gateState{on: true, matched: matched[:boundaryTrig.words64()], sigs: sigs}
		if !g.ruleGated(r.gate, r.signals) {
			return true
		}
	}
	return false
}

// scrubNoteRanges returns the byte ranges of every reserved payload["scrub"]
// object in a serialized record.
//
// The note's keys ARE rule names (§3.7 pins the shape:
// {"by_rule":{"env_assign":611,"dsn_secret":113},…}), so a name-driven pattern
// reads `"dsn_secret":113` as an assignment and would refuse the ledger's own
// scrubbed output — §7 step 4 ("re-run the read path over the finished ledger:
// 0 hits") and §3.7 cannot both hold without this exemption. Only the note is
// exempt: every other byte of the record is still checked.
func scrubNoteRanges(b []byte) []span {
	var out []span
	for i := 0; i+8 < len(b); i++ {
		if b[i] != '"' || (i > 0 && b[i-1] == '\\') {
			continue
		}
		if string(b[i:i+8]) != `"scrub":` {
			continue
		}
		j := i + 8
		for j < len(b) && b[j] == ' ' {
			j++
		}
		if j >= len(b) || b[j] != '{' {
			continue
		}
		depth := 0
		for k := j; k < len(b); k++ {
			switch b[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					out = append(out, span{start: j, end: k + 1})
					k = len(b)
				}
			}
		}
	}
	return out
}

func overlapsAny(s span, spans []span) bool {
	for _, o := range spans {
		if s.start < o.end && o.start < s.end {
			return true
		}
	}
	return false
}

// MandatoryScan re-scans bytes that are about to be persisted (SPEC-01 §4.2).
// A hit means the record is refused: it is not written, the ledger propagates
// this error unchanged, and no byte containing the value is stored anywhere.
// TROUBLE-SCRUB-008 is returned when a mandatory pattern matched; a payload that
// carries only markers is accepted (a fixed point, §6).
func MandatoryScan(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name, ok := boundaryHitFor(b, nil); ok {
		return boundaryHit(name)
	}
	return nil
}

// PrefilterAllows reports whether the persistence-boundary prefilter would let
// bytes through to the rules. It exists so a test can prove the prefilter never
// disables a rule; production code never calls it.
func PrefilterAllows(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	var matched [maxTrigMaskWords]uint64
	sigs := boundaryTrig.fire(b, matched[:boundaryTrig.words64()])
	for _, r := range boundaryRules {
		g := gateState{on: true, matched: matched[:boundaryTrig.words64()], sigs: sigs}
		if !g.ruleGated(r.gate, r.signals) {
			return true
		}
	}
	return false
}

// MandatoryRuleNames lists the mandatory rule names in evaluation order, plus
// the boundary auth variant.
func MandatoryRuleNames() []string {
	out := make([]string, 0, len(boundaryRules))
	for _, r := range boundaryRules {
		out = append(out, r.name)
	}
	return out
}

// BoundaryRefused reports whether err is the boundary refusal (TROUBLE-SCRUB-008).
func BoundaryRefused(err error) bool {
	se, ok := err.(*ScanError)
	return ok && se.Code == types.CodeScrub008
}
