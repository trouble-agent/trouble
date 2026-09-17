package scrub

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// test-only rule kind: lets the limits suite inject a rule that exceeds
// rule_timeout or reports a counter overflow. It is never reachable from
// configuration (only the compiled-in table and the in-package tests can name
// it).
const kindTestScrub = types.RuleKind("test_only")

// ruleTable is one effective ordered rule set (§3.6 rule 1).
// tableRule is one rule as it appears in one table: the compiled rule plus the
// gate that is specific to that table (a project may configure its own home
// roots, so the path rule's keywords are per-table).
type tableRule struct {
	*compiledRule
	gate    []uint64
	signals signalMask
}

type ruleTable struct {
	projectID    string
	rules        []tableRule
	prefilter    bool // table-wide gate enabled (§3.6 rule 10)
	unionMask    []uint64
	unionSignals signalMask
	allowlist    []string
	homeRoots    []string
}

// maxTrigMaskWords bounds the prefilter keyword universe (512 keywords).
const maxTrigMaskWords = 8

// gateState carries one prefilter pass result to the rules of one call.
type gateState struct {
	on      bool
	matched []uint64
	sigs    signalMask
}

// ruleGated reports whether the prefilter proves the rule cannot match.
func (g gateState) ruleGated(gate []uint64, signals signalMask) bool {
	if !g.on {
		return false
	}
	if signals == 0 && (len(gate) == 0 || maskEmpty(gate)) {
		return false // no gate: always run (config-authored project rules)
	}
	return !intersects(g.matched, gate) && g.sigs&signals == 0
}

// Engine is immutable after New. Safe for concurrent use by any number of
// goroutines (SPEC-02 §2): the rule tables and the shield are read-only and
// only the counters mutate, through atomics.
type Engine struct {
	cfg        *resolved
	compiled   []*compiledRule
	tables     map[string]*ruleTable
	projectIDs []string
	sh         *shield
	st         *statSet
	trig       *trigWords
	trigWords  int
}

// New compiles the built-in rule table (18 rules, order = §3.3), strict-decodes
// the resolved [scrub] config subtree, resolves per-project redaction overrides
// and registers every project DSN public key in the shield table.
func New(cfgTOML []byte, projects []types.Project) (*Engine, error) {
	return newEngine(cfgTOML, projects, builtinTable, nil)
}

// newEngine is New with an injectable rule table and shield source (tests use
// it to prove the TROUBLE-SCRUB-006 refusal and to drive the limit paths).
func newEngine(cfgTOML []byte, projects []types.Project, table []builtinRule, shieldKeys []string) (*Engine, error) {
	raw, err := parseConfig(cfgTOML)
	if err != nil {
		return nil, err
	}
	cfg, err := resolveConfig(raw, projects)
	if err != nil {
		return nil, err
	}
	compiled := make([]*compiledRule, 0, len(table))
	for i, br := range table {
		c, err := compileBuiltin(br, i)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, c)
	}
	if name, ok := mandatorySetComplete(compiled); !ok {
		return nil, mandatoryMissing(name)
	}
	keys := shieldKeys
	if keys == nil {
		keys = AllowedPublicKeys(projects)
	}
	e := &Engine{cfg: cfg, compiled: compiled, sh: newShield(keys)}
	names := make([]string, 0, len(compiled)+RuleTableSize)
	for _, c := range compiled {
		names = append(names, c.name)
	}
	for _, id := range sortedProjectIDs(cfg.projects) {
		for _, c := range cfg.projects[id].rules {
			names = append(names, c.name)
		}
	}
	e.st = newStatSet(dedupStrings(names), cfg.rulesVersion)
	// One prefilter universe for every table: a rule's gate is a bitmask over
	// the union of every rule's keywords, so a single pass over a payload proves
	// which rules can possibly match (§3.6 rule 10).
	e.trig = newTrigWords()
	for _, c := range compiled {
		e.trig.add(c.anchors)
	}
	for _, id := range sortedProjectIDs(cfg.projects) {
		for _, c := range cfg.projects[id].rules {
			e.trig.add(c.anchors)
		}
	}
	// path_home_root gates on the configured home roots (per project), so those
	// literals belong to the universe too.
	e.trig.add(homeRootAnchors(e.cfg.homeRoots))
	for _, id := range sortedProjectIDs(cfg.projects) {
		if po := cfg.projects[id]; len(po.homeRoots) > 0 {
			e.trig.add(homeRootAnchors(po.homeRoots))
		}
	}
	e.trigWords = e.trig.words64()
	if e.trigWords > maxTrigMaskWords {
		return nil, configError("scrub", "prefilter keyword universe of %d words exceeds the %d-word bound",
			e.trig.nbits, maxTrigMaskWords*64)
	}
	for _, c := range compiled {
		c.gate = e.trig.maskFor(c.anchors)
	}
	for _, id := range sortedProjectIDs(cfg.projects) {
		for _, c := range cfg.projects[id].rules {
			c.gate = e.trig.maskFor(c.anchors)
		}
	}
	if err := e.buildTables(compiled); err != nil {
		return nil, err
	}
	return e, nil
}

// AllowedPublicKeys is the shield source: the DSN public key of every
// configured project. `enabled` does not gate it — a project disabled today can
// still appear in records captured while it was enabled, and the key is the one
// credential the system is allowed to persist (SPEC-02 §3.5).
func AllowedPublicKeys(projects []types.Project) []string {
	out := make([]string, 0, len(projects))
	seen := map[string]bool{}
	for _, p := range projects {
		if p.PublicKey == "" || seen[p.PublicKey] {
			continue
		}
		seen[p.PublicKey] = true
		out = append(out, p.PublicKey)
	}
	return out
}

// Rules returns the effective ordered rule set (global when projectID == "").
func (e *Engine) Rules(projectID string) []types.ScrubRule {
	t := e.table(projectID)
	if t == nil {
		return nil
	}
	out := make([]types.ScrubRule, 0, len(t.rules))
	for _, r := range t.rules {
		targets := make([]string, 0, len(types.ScrubTargets))
		for _, tg := range types.ScrubTargets {
			if r.targets.has(tg) {
				targets = append(targets, string(tg))
			}
		}
		pattern := r.pattern
		if pattern == "" {
			pattern = "builtin"
		}
		out = append(out, types.ScrubRule{
			Name: r.name, Kind: r.kind, Pattern: pattern,
			Replace: r.replaceS, Mandatory: r.mandatory, Targets: targets,
		})
	}
	return out
}

// RulesVersion reports the identity of the rule table that scrubbed a payload.
func (e *Engine) RulesVersion() int { return e.cfg.rulesVersion }

// Stats returns the cumulative process counters (SPEC-02 §3.7).
func (e *Engine) Stats() types.ScrubStats { return e.st.snapshot() }

// UnmappedFields reports how many payload fields ScrubFields was handed without
// a declared target (§6: an undeclared field is returned untouched and counted).
func (e *Engine) UnmappedFields() uint64 { return e.st.unmappedFields.Load() }

// ExemptValues reports how many candidate values were left unchanged by the
// loopback/RFC1918 exemption of rules 15/16 (§3.3).
func (e *Engine) ExemptValues() uint64 { return e.st.exemptValues.Load() }

// entry builds the per-table view of a compiled rule. Most rules gate on their
// static keyword mask; path_home_root gates on the table's configured home
// roots, which are literals the payload must contain for the rule to match.
func (e *Engine) entry(c *compiledRule, homeRoots []string) tableRule {
	gate := c.gate
	sig := c.signals
	if c.name == "path_home_root" {
		gate = e.trig.maskFor(homeRootAnchors(homeRoots))
		sig = 0
	}
	return tableRule{compiledRule: c, gate: gate, signals: sig}
}

// homeRootAnchors turns the configured home roots into the literals a matching
// payload must contain: "/home/*" → "/home/", "/root" → "/root".
func homeRootAnchors(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimRight(r, "/")
		if r == "" {
			continue
		}
		if strings.HasSuffix(r, "/*") {
			out = append(out, strings.TrimSuffix(r, "*"))
			continue
		}
		out = append(out, r)
	}
	return out
}

func (e *Engine) table(projectID string) *ruleTable {
	if t, ok := e.tables[projectID]; ok {
		return t
	}
	return e.tables[""]
}

// ScrubString scrubs one string value (SPEC-02 §2).
func (e *Engine) ScrubString(ctx context.Context, target types.ScrubTarget, projectID, s string) (string, types.ScrubResult, error) {
	b := []byte(s)
	out, res, err := e.ScrubBytes(ctx, target, projectID, b)
	if err != nil {
		return "", res, err
	}
	return string(out), res, nil
}

// ScrubBytes scrubs one byte value. ScrubBytes(nil) returns an empty non-nil
// slice so a caller can distinguish "scrubbed empty" from "failed" — failure is
// the error return and never a partial value (§6, invariant 5).
func (e *Engine) ScrubBytes(ctx context.Context, target types.ScrubTarget, projectID string, b []byte) ([]byte, types.ScrubResult, error) {
	res := types.ScrubResult{ByRule: map[string]int{}, BytesIn: len(b)}
	if !target.Valid() {
		return nil, res, errf(types.CodeScrub002, ReasonUnknownTarget, string(target),
			"unknown scrub target", nil)
	}
	if b == nil {
		return []byte{}, res, nil
	}
	return e.scrubOne(ctx, target, projectID, b)
}

func (e *Engine) budget(target types.ScrubTarget) targetBudget {
	if b, ok := e.cfg.budgets[target]; ok {
		return b
	}
	return targetBudget{maxBytes: e.cfg.maxBytesDefault, truncate: true}
}

// scrubOne is the single-value core: UTF-8 check, budget, rule pass, counters.
func (e *Engine) scrubOne(ctx context.Context, target types.ScrubTarget, projectID string, in []byte) ([]byte, types.ScrubResult, error) {
	res := types.ScrubResult{ByRule: map[string]int{}, BytesIn: len(in)}
	e.st.calls.Add(1)
	e.st.bytesIn.Add(uint64(len(in)))
	if err := ctx.Err(); err != nil {
		return nil, res, err
	}
	if !utf8.Valid(in) {
		e.st.invalidUTF8.Add(1)
		e.st.failClosed.Add(1)
		return nil, res, errf(types.CodeScrub007, ReasonInvalidUTF8, "",
			"input is not valid UTF-8 and no target accepts bytes the rule table cannot read", nil)
	}
	buf := in
	var tail []byte
	if b := e.budget(target); b.maxBytes > 0 && len(in) > b.maxBytes {
		if !b.truncate {
			e.st.refusedBytes.Add(uint64(len(in)))
			return nil, res, errf(types.CodeScrub003, ReasonTooLarge, string(target),
				fmt.Sprintf("input of %d bytes exceeds the %d-byte budget and this target refuses over-budget payloads",
					len(in), b.maxBytes), nil)
		}
		cut := lastNewlineWithin(in, b.maxBytes)
		tail = truncationMarker(len(in) - cut)
		buf = in[:cut]
		res.Truncated = true
		e.st.truncated.Add(1)
	}
	out, err := e.pass(ctx, e.table(projectID), target, buf, res.ByRule)
	if err != nil {
		return nil, res, err
	}
	if tail != nil {
		out = append(append([]byte{}, out...), tail...)
	}
	res.Value = out
	res.Redactions = countRules(res.ByRule)
	e.st.bytesOut.Add(uint64(len(out)))
	e.st.redactions.Add(uint64(res.Redactions))
	for n, c := range res.ByRule {
		e.st.addRule(n, uint64(c))
	}
	return out, res, nil
}

// pass walks the effective table once over the working buffer (SPEC-02 §3.6
// rule 9: a single pass, not multi-pass).
func (e *Engine) pass(ctx context.Context, tbl *ruleTable, target types.ScrubTarget, buf []byte, byRule map[string]int) ([]byte, error) {
	if tbl == nil || len(tbl.rules) == 0 {
		return buf, nil
	}
	var matched [maxTrigMaskWords]uint64
	var sigs signalMask
	g := gateState{matched: matched[:e.trigWords]}
	if tbl.prefilter {
		sigs = e.trig.fire(buf, g.matched)
		g.on = true
		g.sigs = sigs
		if sigs&tbl.unionSignals == 0 && !intersects(g.matched, tbl.unionMask) {
			e.st.prefMiss.Add(1)
			return buf, nil
		}
	}
	buf = e.sh.protect(buf)
	var flagSpans, headerSpans []span
	flagDone, headerDone := false, false
	for _, r := range tbl.rules {
		if !r.targets.has(target) || g.ruleGated(r.gate, r.signals) {
			continue
		}
		var reserved []span
		// §3.6 rules 5 and 9: the structured rules own their spans. env_assign
		// and kv_secret_assign yield a flag-form or header-form assignment to
		// cli_flag_secret / auth_header, which attribute it to the more
		// diagnostic rule name (and which the normative vectors P4 and P6
		// require).
		if r.name == "env_assign" || r.name == "kv_secret_assign" {
			if !flagDone {
				flagSpans = e.spansFor(tbl, "cli_flag_secret", target, buf, nil, g)
				flagDone = true
			}
			reserved = append(reserved, flagSpans...)
		}
		if r.name == "kv_secret_assign" || r.name == "pii_identity_kv" {
			if !headerDone {
				headerSpans = e.spansFor(tbl, "auth_header", target, buf, nil, g)
				headerDone = true
			}
			reserved = append(reserved, headerSpans...)
		}
		if r.rescan {
			continue // rescans run after rule 18 (§3.6 rule 9)
		}
		start := time.Now()
		spans, err := e.ruleSpans(r.compiledRule, tbl, buf, reserved)
		if err != nil {
			e.st.failClosed.Add(1)
			return nil, err
		}
		if d := time.Since(start); e.cfg.ruleTimeout > 0 && d > e.cfg.ruleTimeout {
			e.st.timeouts.Add(1)
			e.st.failClosed.Add(1)
			return nil, errf(types.CodeScrub005, ReasonRuleTimeout, r.name,
				fmt.Sprintf("rule evaluation exceeded %s", e.cfg.ruleTimeout), nil)
		}
		if len(spans) > 0 {
			buf = e.applyRuleSpans(buf, spans, r.replace)
			addRule(byRule, r.name, len(spans))
		}
	}
	// Project rules that asked for a bounded re-run (§3.6 rule 9): one extra
	// evaluation, at the end of the pass, over the marker-bearing buffer.
	for _, r := range tbl.rules {
		if !r.rescan || !r.targets.has(target) {
			continue
		}
		spans, err := e.ruleSpans(r.compiledRule, tbl, buf, nil)
		if err != nil {
			e.st.failClosed.Add(1)
			return nil, err
		}
		if len(spans) > 0 {
			buf = e.applyRuleSpans(buf, spans, r.replace)
			addRule(byRule, r.name, len(spans))
		}
	}
	buf = e.sh.restore(buf)
	return buf, nil
}

// applyRuleSpans replaces spans with the rule's marker, re-emitting any shield
// placeholder that fell inside a replaced span.
//
// Placeholders are inserted before rule 1 and restored after rule 18, so a
// placeholder lies wholly inside or wholly outside any span (NUL belongs to no
// rule's alphabet). Without this step a rule that consumes a whole value —
// env_assign on `dsn = "http://<pubkey>@host/1"`, or rule 1 on a PEM block whose
// body quotes the key — would destroy the one credential the system is allowed
// to persist, and §6's "the shield restores it and the block is still redacted
// around it" could not hold.
func (e *Engine) applyRuleSpans(buf []byte, spans []span, repl []byte) []byte {
	if e.sh.empty() {
		return applySpans(buf, spans, repl)
	}
	var carried [][]byte
	out := make([]byte, 0, len(buf)+len(spans)*(len(repl)+1))
	prev := 0
	for _, s := range spans {
		if s.start < prev || s.end > len(buf) || s.start >= s.end {
			continue
		}
		rep := repl
		if len(s.repl) > 0 {
			rep = s.repl
		}
		out = append(out, buf[prev:s.start]...)
		out = append(out, rep...)
		carried = carried[:0]
		for _, ph := range e.sh.ph {
			// a placeholder already carried by the replacement (rule 4 rebuilds
			// the URI in place, public key included) is not re-emitted
			if bytes.Contains(buf[s.start:s.end], ph) && !bytes.Contains(rep, ph) {
				carried = append(carried, ph)
			}
		}
		for _, ph := range carried {
			out = append(out, ph...)
		}
		prev = s.end
	}
	out = append(out, buf[prev:]...)
	return out
}

// spansFor computes the spans of a named rule of the table without counting
// them; used for the §3.6 rule 5/9 deferral and for the boundary confirmation.
func (e *Engine) spansFor(tbl *ruleTable, name string, target types.ScrubTarget, buf []byte, reserved []span, g gateState) []span {
	for _, r := range tbl.rules {
		if r.name != name || !r.targets.has(target) || g.ruleGated(r.gate, r.signals) {
			continue
		}
		s, err := e.ruleSpans(r.compiledRule, tbl, buf, reserved)
		if err != nil {
			return nil
		}
		return s
	}
	return nil
}

// ruleSpans dispatches one rule to its scanner (SPEC-02 §3.5).
func (e *Engine) ruleSpans(r *compiledRule, tbl *ruleTable, buf []byte, reserved []span) ([]span, error) {
	switch r.kind {
	case kindRegex:
		spans := r.regexSpans(buf, reserved)
		if r.exemptIP {
			var skipped int
			spans, skipped = filterExemptSpans(buf, spans, r.strictQuad)
			if skipped > 0 {
				e.st.exemptValues.Add(uint64(skipped))
			}
		}
		return spans, nil
	case kindPrefix:
		return prefixSpans(buf, r.begins), nil
	case kindEntropy:
		return entropySpans(buf, r.extend), nil
	case kindDSNPart:
		if len(r.begins) > 0 && r.begins[0] == "secret" {
			return dsnSecretSpans(buf, e.sh), nil
		}
		return dsnAnySpans(buf, e.sh), nil
	case kindPathAllowlist:
		mode := pathModeDisclosure
		if len(r.begins) > 0 && r.begins[0] == "home_root" {
			mode = pathModeHomeRoot
		}
		return pathSpans(buf, mode, tbl.allowlist, tbl.homeRoots), nil
	case kindTestScrub:
		if r.testSleep > 0 {
			time.Sleep(r.testSleep)
		}
		if r.testFail != nil {
			return nil, errf(types.CodeScrub004, ReasonRuleError, r.name, "rule failed", r.testFail)
		}
		if r.testOverflow {
			return nil, errf(types.CodeScrub004, ReasonReplacementLimit, r.name,
				"rule produced more than 2^31-1 replacements on one value", nil)
		}
		return nil, nil
	}
	return nil, errf(types.CodeScrub004, ReasonRuleError, r.name, "unknown rule kind", nil)
}

// ScrubFields scrubs a bundle (payload field name -> target class) in ONE pass
// over the rule table, sharing one budget and one counter set (SPEC-02 §2). The
// returned map is the persisted form.
func (e *Engine) ScrubFields(ctx context.Context, projectID string, fields map[string][]byte, targets map[string]types.ScrubTarget) (map[string][]byte, types.ScrubResult, error) {
	res := types.ScrubResult{ByRule: map[string]int{}}
	out := make(map[string][]byte, len(fields))
	e.st.calls.Add(1)
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)

	type field struct {
		name   string
		target types.ScrubTarget
		buf    []byte
		tail   []byte
		trunc  bool
	}
	declared := make([]*field, 0, len(fields))
	for _, k := range names {
		v := fields[k]
		t, ok := targets[k]
		if !ok {
			e.st.unmappedFields.Add(1)
			out[k] = v
			res.BytesIn += len(v)
			continue
		}
		if !t.Valid() {
			return nil, res, errf(types.CodeScrub002, ReasonUnknownTarget, string(t),
				"unknown scrub target", nil)
		}
		if err := ctx.Err(); err != nil {
			return nil, res, err
		}
		res.BytesIn += len(v)
		e.st.bytesIn.Add(uint64(len(v)))
		if !utf8.Valid(v) {
			e.st.invalidUTF8.Add(1)
			e.st.failClosed.Add(1)
			return nil, res, errf(types.CodeScrub007, ReasonInvalidUTF8, "",
				"field is not valid UTF-8", nil)
		}
		f := &field{name: k, target: t, buf: v}
		if b := e.budget(t); b.maxBytes > 0 && len(v) > b.maxBytes {
			if !b.truncate {
				e.st.refusedBytes.Add(uint64(len(v)))
				return nil, res, errf(types.CodeScrub003, ReasonTooLarge, string(t),
					fmt.Sprintf("field of %d bytes exceeds the %d-byte budget and this target refuses over-budget payloads",
						len(v), b.maxBytes), nil)
			}
			cut := lastNewlineWithin(v, b.maxBytes)
			f.tail = truncationMarker(len(v) - cut)
			f.buf = v[:cut]
			f.trunc = true
			res.Truncated = true
			e.st.truncated.Add(1)
		}
		declared = append(declared, f)
	}
	if len(declared) == 0 {
		res.Value = nil
		return out, res, nil
	}

	tbl := e.table(projectID)
	// The prefilter is per buffer: when it misses on every declared field, no
	// rule can match any of them and the pass is skipped entirely (§3.6 rule
	// 10). Each field gets its OWN gate: a rule is evaluated on a field only
	// when that field's own bytes contain one of the rule's anchors (or fire
	// one of its signals). A rule's match always lies within the bytes that
	// carry its anchor, so per-field gating is provably output-identical to
	// bundle-wide gating — and it keeps a `PWD=` in env from un-gating rules
	// on the message, stack and headers fields.
	scan := false
	fieldsGate := make([]gateState, len(declared))
	var fieldsGateBundle gateState
	if tbl.prefilter {
		var union []uint64
		var unionSigs signalMask
		for i, f := range declared {
			m := make([]uint64, e.trigWords)
			sigs := e.trig.fire(f.buf, m)
			fieldsGate[i] = gateState{on: true, matched: m, sigs: sigs}
			if !maskEmpty(m) || sigs != 0 {
				scan = true
			}
			// the rescan loop (§3.6 rule 9) keeps the bundle-wide gate: a
			// rescan rule is evaluated "over the marker-bearing buffer", so
			// anchor text a replacement injected mid-pass must still ungate it
			if union == nil {
				union = make([]uint64, e.trigWords)
			}
			for j, w := range m {
				union[j] |= w
			}
			unionSigs |= sigs
		}
		fieldsGateBundle = gateState{on: true, matched: union, sigs: unionSigs}
		if !scan {
			e.st.prefMiss.Add(1)
		}
	} else {
		scan = true
	}
	if scan {
		// shield in: no rule may see a configured public key (§3.5)
		for _, f := range declared {
			f.buf = e.sh.protect(f.buf)
		}
		for _, r := range tbl.rules {
			if r.rescan {
				continue
			}
			for i, f := range declared {
				if !r.targets.has(f.target) {
					continue
				}
				// when the table prefilter is off, fieldsGate[i] is the zero
				// gateState, which never gates (§3.6 rule 10's kill switch)
				if g := fieldsGate[i]; g.ruleGated(r.gate, r.signals) {
					continue
				}
				// the reservation is per field: headerSpans/flagSpans computed for
				// one field say nothing about another field's spans
				var reserved []span
				if r.name == "env_assign" || r.name == "kv_secret_assign" {
					reserved = append(reserved,
						e.spansFor(tbl, "cli_flag_secret", f.target, f.buf, nil, fieldsGate[i])...)
				}
				if r.name == "kv_secret_assign" || r.name == "pii_identity_kv" {
					reserved = append(reserved,
						e.spansFor(tbl, "auth_header", f.target, f.buf, nil, fieldsGate[i])...)
				}
				start := time.Now()
				spans, err := e.ruleSpans(r.compiledRule, tbl, f.buf, reserved)
				if err != nil {
					e.st.failClosed.Add(1)
					return nil, res, err
				}
				if d := time.Since(start); e.cfg.ruleTimeout > 0 && d > e.cfg.ruleTimeout {
					e.st.timeouts.Add(1)
					e.st.failClosed.Add(1)
					return nil, res, errf(types.CodeScrub005, ReasonRuleTimeout, r.name,
						fmt.Sprintf("rule evaluation exceeded %s", e.cfg.ruleTimeout), nil)
				}
				if len(spans) > 0 {
					f.buf = e.applyRuleSpans(f.buf, spans, r.replace)
					addRule(res.ByRule, r.name, len(spans))
				}
			}
		}
		for _, r := range tbl.rules {
			if !r.rescan {
				continue
			}
			for _, f := range declared {
				if !r.targets.has(f.target) {
					continue
				}
				// rescan keeps the bundle-wide gate: see the union comment above
				if fieldsGateBundle.ruleGated(r.gate, r.signals) {
					continue
				}
				spans, err := e.ruleSpans(r.compiledRule, tbl, f.buf, nil)
				if err != nil {
					e.st.failClosed.Add(1)
					return nil, res, err
				}
				if len(spans) > 0 {
					f.buf = e.applyRuleSpans(f.buf, spans, r.replace)
					addRule(res.ByRule, r.name, len(spans))
				}
			}
		}
	}
	for _, f := range declared {
		f.buf = e.sh.restore(f.buf)
		if f.tail != nil {
			f.buf = append(append([]byte{}, f.buf...), f.tail...)
		}
		out[f.name] = f.buf
	}
	res.Redactions = countRules(res.ByRule)
	for _, f := range declared {
		e.st.bytesOut.Add(uint64(len(out[f.name])))
	}
	e.st.redactions.Add(uint64(res.Redactions))
	for n, c := range res.ByRule {
		e.st.addRule(n, uint64(c))
	}
	return out, res, nil
}

// ScrubRecord rewrites the payload fields named in targets in place, sets
// r.Redactions to the call total and writes the reserved payload key "scrub"
// (§3.7). Seq, RecID, TS, Kind, Sig and Origin are never touched.
func (e *Engine) ScrubRecord(ctx context.Context, r *types.Record, targets map[string]types.ScrubTarget) (types.ScrubResult, error) {
	return e.ScrubRecordFor(ctx, "", r, targets)
}

// ScrubRecordFor is ScrubRecord with an explicit project scope: the record wire
// shape carries no project id, so a caller that knows the project (the sentinel
// ingestion path) passes it and gets that project's overrides.
func (e *Engine) ScrubRecordFor(ctx context.Context, projectID string, r *types.Record, targets map[string]types.ScrubTarget) (types.ScrubResult, error) {
	res := types.ScrubResult{ByRule: map[string]int{}}
	if r == nil {
		return res, errf(types.CodeScrub002, ReasonUnknownTarget, "", "record is nil", nil)
	}
	if r.Payload == nil {
		r.Payload = map[string]any{}
	}
	fields := make(map[string][]byte, len(targets))
	paths := make(map[string][]string, len(targets))
	for k := range targets {
		if v, ok, path := lookupPayload(r.Payload, k); ok {
			fields[k] = v
			paths[k] = path
		}
	}
	if len(fields) == 0 {
		r.Redactions = 0
		r.Payload[PayloadScrubKey] = e.scrubNote(res, 0, 0)
		return res, nil
	}
	out, res, err := e.ScrubFields(ctx, projectID, fields, targets)
	if err != nil {
		return res, err
	}
	total, bytesOut := 0, 0
	for k, v := range out {
		setPayload(r.Payload, paths[k], v)
		bytesOut += len(v)
	}
	for _, c := range res.ByRule {
		total += c
	}
	r.Redactions = total
	res.Redactions = total
	r.Payload[PayloadScrubKey] = e.scrubNote(res, bytesOut, total)
	return res, nil
}

// PayloadScrubKey is the reserved payload key written by ScrubRecord (§3.7).
const PayloadScrubKey = "scrub"

// scrubNote renders the reserved payload["scrub"] object. It is bounded to ≤64
// rule names by §3.2, so payload size and index cardinality stay bounded.
func (e *Engine) scrubNote(res types.ScrubResult, bytesOut, redactions int) map[string]any {
	byRule := make(map[string]any, len(res.ByRule))
	for k, v := range res.ByRule {
		byRule[k] = v
	}
	return map[string]any{
		"by_rule":       byRule,
		"bytes_in":      res.BytesIn,
		"bytes_out":     bytesOut,
		"truncated":     res.Truncated,
		"rules_version": e.cfg.rulesVersion,
		"engine":        "re2",
	}
}

// lookupPayload resolves a payload field, supporting dotted paths into nested
// maps (a header block arrives as payload["request"]["headers"]).
func lookupPayload(p map[string]any, key string) ([]byte, bool, []string) {
	parts := strings.Split(key, ".")
	var cur any = p
	for i, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[part]
		if !ok {
			return nil, false, nil
		}
		if i == len(parts)-1 {
			switch v := cur.(type) {
			case string:
				return []byte(v), true, parts
			case []byte:
				return v, true, parts
			default:
				return nil, false, nil
			}
		}
	}
	return nil, false, nil
}

func setPayload(p map[string]any, path []string, v []byte) {
	if len(path) == 0 {
		return
	}
	cur := p
	for i, part := range path {
		if i == len(path)-1 {
			switch cur[part].(type) {
			case string:
				cur[part] = string(v)
			case []byte:
				cur[part] = v
			default:
				cur[part] = string(v)
			}
			return
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
}

// ScrubEnvelope scrubs every Record in a ForwardEnvelope: the satellite send
// path and the hub receive path (§4).
func (e *Engine) ScrubEnvelope(ctx context.Context, env *types.ForwardEnvelope, targets map[string]types.ScrubTarget) (types.ScrubResult, error) {
	total := types.ScrubResult{ByRule: map[string]int{}}
	if env == nil {
		return total, errf(types.CodeScrub002, ReasonUnknownTarget, "", "envelope is nil", nil)
	}
	for i := range env.Records {
		res, err := e.ScrubRecordFor(ctx, env.Origin.Source, &env.Records[i], targets)
		if err != nil {
			return total, err
		}
		total.Redactions += res.Redactions
		total.BytesIn += res.BytesIn
		total.Truncated = total.Truncated || res.Truncated
		for k, v := range res.ByRule {
			total.ByRule[k] += v
		}
	}
	return total, nil
}

// Verify is the persistence-boundary re-scan (§3.4 point 3): a boolean check
// over the coalesced mandatory alternation, with the marker-aware confirmation
// that makes a scrubbed payload a fixed point at the boundary. It never
// rewrites and never attributes a rule to a counter.
func (e *Engine) Verify(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name, ok := boundaryHitFor(b, e.sh); ok {
		e.st.boundaryRefusals.Add(1)
		return boundaryHit(name)
	}
	return nil
}

// prefilterForTest exposes a table's prefilter state to the equivalence test.
func (e *Engine) prefilterForTest(projectID string) bool { return e.table(projectID).prefilter }

func sortedProjectIDs(m map[string]*projectOverride) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// orMaskInto unions src into dst, growing dst as needed.
func orMaskInto(dst []uint64, src []uint64) []uint64 {
	for len(dst) < len(src) {
		dst = append(dst, 0)
	}
	for i := range src {
		dst[i] |= src[i]
	}
	return dst
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func regexpCompile(p string) (*regexp.Regexp, error) { return regexp.Compile(p) }
