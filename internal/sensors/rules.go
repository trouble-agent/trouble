package sensors

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.5: the rule TOML schema. Each file decodes into []types.Rule —
// the shared type *is* the schema, field for field, so there is no second rule
// shape. Defaults are applied per key using the decoder's metadata, because a
// missing `enabled = false` and an absent `enabled` are different rules.

// Rule limits (SPEC-03 §3.5).
const (
	maxRulesPerSource = 256
	maxRulesTotal     = 1024
	maxCooldown       = 24 * time.Hour
	maxVerifyWindow   = time.Hour
)

var ruleNameRe = regexp.MustCompile(`^[a-z0-9_]{3,64}$`)

// ruleProblem is a rule-load refusal: TROUBLE-SENSORS-017 (schema) or
// TROUBLE-SENSORS-018 (expression/field), always naming file + rule + line.
type ruleProblem struct {
	Code types.ErrorCode
	File string
	Line int
	Rule string
	Msg  string
}

func (e *ruleProblem) Error() string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	if e.Rule != "" {
		return fmt.Sprintf("%s: %s (rule %q): %s", e.Code, loc, e.Rule, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, loc, e.Msg)
}

// ruleLoadErrors is the complete refusal set for one reload attempt: every
// problem is reported, not just the first (an editor gets one round trip).
type ruleLoadErrors struct {
	Problems []*ruleProblem
}

func (e *ruleLoadErrors) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0].Error()
	}
	return fmt.Sprintf("%d rule problems, first: %v", len(e.Problems), e.Problems[0])
}

// Code returns the code of the first problem — the one the health reason quotes.
func (e *ruleLoadErrors) Code() types.ErrorCode {
	if len(e.Problems) == 0 {
		return ""
	}
	return e.Problems[0].Code
}

// First returns the first problem (nil when there are none).
func (e *ruleLoadErrors) First() *ruleProblem {
	if len(e.Problems) == 0 {
		return nil
	}
	return e.Problems[0]
}

// ---- compiled rule set ----

// ruleSet is one immutable generation of the live rule set (SPEC-03 §3.10).
// It is installed with a single atomic pointer store, so no evaluation can
// observe a partially-visible set.
type ruleSet struct {
	gen      uint64
	bySource map[types.SigSource][]compiledRule
	rules    []types.Rule
	loadedTS time.Time
	// skipped records why the auto_grants existence check did not run, so a
	// skipped check is observable rather than a silent pass.
	skipped string
}

// compiledRule is a rule plus its compiled programs (SPEC-03 §3.10).
type compiledRule struct {
	rule     types.Rule
	prog     []exprProgram
	fields   []string
	matchKey string // fingerprint of Match+For, for reload carry-over
	scope    []string
}

func (c compiledRule) name() string { return c.rule.Name }

// ---- shipped defaults ----

// defaultRulesTOML is the shipped default set of SPEC-03 §3.5. The identical
// bytes are shipped as examples/rules/10-defaults.toml, and rules_test.go
// asserts they match — one source of truth, no drift between binary and repo.
const defaultRulesTOML = `# trouble shipped default rules (SPEC-03 §3.5) — examples only, no host-specific values.
# Installed to <config>/rules.d/10-defaults.toml by trouble install.

[[rule]]
name = "psi_cpu_some_avg10_high"
enabled = true
source = "psi"
for = "60s"
entry_rung = "play"
severity = "medium"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "scope"
op = "in"
value = "[\"cpu\"]"
value_type = "string"

[[rule.match]]
field = "some_avg10"
op = ">="
value = "25"
value_type = "number"

[[rule]]
name = "psi_io_some_avg10_high"
enabled = true
source = "psi"
for = "30s"
entry_rung = "play"
severity = "high"
cooldown = "10m"
max_runs = 2
verify_window = "10m"
auto_grants = ["proc.top", "proc.connections"]
hotfix = false

[[rule.match]]
field = "scope"
op = "in"
value = "[\"io\"]"
value_type = "string"

[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"

[[rule]]
name = "self_memory_pressure"
enabled = true
source = "psi"
for = "30s"
entry_rung = "play"
severity = "high"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = ["proc.top"]
hotfix = false

[[rule.match]]
field = "scope"
op = "in"
value = "[\"memory\"]"
value_type = "string"

[[rule.match]]
field = "some_avg10"
op = ">="
value = "8"
value_type = "number"

[[rule]]
name = "dbus_unit_failed"
enabled = true
source = "dbus"
for = "0s"
entry_rung = "play"
severity = "high"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = ["service.reload"]
hotfix = false

[[rule.match]]
field = "unit_substate"
op = "=="
value = "failed"
value_type = "string"

[[rule]]
name = "dbus_crash_loop"
enabled = true
source = "dbus"
for = "0s"
entry_rung = "research"
severity = "critical"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "crash_loop"
op = "=="
value = "true"
value_type = "bool"

[[rule]]
name = "disk_space_low"
enabled = true
source = "disk"
for = "5m"
entry_rung = "record"
severity = "high"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "free_pct"
op = "<"
value = "10"
value_type = "number"

[[rule]]
name = "disk_inode_low"
enabled = true
source = "disk"
for = "5m"
entry_rung = "record"
severity = "medium"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "inode_free_pct"
op = ">="
value = "0"
value_type = "number"

[[rule.match]]
field = "inode_free_pct"
op = "<"
value = "10"
value_type = "number"

[[rule]]
name = "timers_run_missed"
enabled = true
source = "timers"
for = "0s"
entry_rung = "record"
severity = "medium"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "missed_runs"
op = ">="
value = "2"
value_type = "number"

[[rule]]
name = "inotify_queue_overflow"
enabled = true
source = "inotify"
for = "0s"
entry_rung = "record"
severity = "low"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "overflow"
op = "=="
value = "true"
value_type = "bool"
`

// defaultRules decodes the shipped set; a failure here is a programming error
// and panics at first use (the set ships with the binary).
func defaultRules() []types.Rule {
	rs, err := decodeRulesTOML([]byte(defaultRulesTOML), "examples/rules/10-defaults.toml")
	if err != nil {
		panic("sensors: shipped default rules do not decode: " + err.Error())
	}
	return rs
}

// ---- decoding ----

// ruleFile is the TOML document wrapper. The document root is a table holding
// one `rule` array, so a wrapper is required to reach it; its element type is
// types.Rule itself, so there is still no second rule shape (SPEC-03 §3.5).
type ruleFile struct {
	Rule []types.Rule `toml:"rule"`
}

// decodeRulesTOML decodes one rule file into []types.Rule and applies the
// §3.5 defaults for every key the file does not define.
//
// Defaults need key presence, which the shared type cannot express (a missing
// `enabled = false` and an absent `enabled` are different rules), so the file
// is also decoded into its raw table form and presence is read from there. The
// decoded shape is still []types.Rule: there is no second rule struct.
func decodeRulesTOML(b []byte, name string) ([]types.Rule, error) {
	var file ruleFile
	md, err := toml.Decode(string(b), &file)
	if err != nil {
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors017, File: name, Line: lineOfDecodeError(err), Msg: err.Error(),
		}}}
	}
	for _, k := range md.Undecoded() {
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors017, File: name, Line: keyLine(b, k),
			Msg: fmt.Sprintf("unknown key %q (SPEC-03 §3.5: the shared Rule type is the schema; a typo must never become a silently ignored field)", k.String()),
		}}}
	}
	out := file.Rule
	var rawDoc map[string]any
	if _, err := toml.Decode(string(b), &rawDoc); err != nil {
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors017, File: name, Line: lineOfDecodeError(err), Msg: err.Error(),
		}}}
	}
	rawRules, _ := rawDoc["rule"].([]map[string]any)
	defined := func(i int, key string) bool {
		if i >= len(rawRules) || rawRules[i] == nil {
			return false
		}
		_, ok := rawRules[i][key]
		return ok
	}
	for i := range out {
		r := &out[i]
		if r.Match == nil {
			r.Match = []types.Condition{}
		}
		if !defined(i, "enabled") {
			r.Enabled = true
		}
		if !defined(i, "for") {
			r.For = "0s"
		}
		if !defined(i, "entry_rung") {
			r.EntryRung = types.RungRecord
		}
		if !defined(i, "severity") {
			r.Severity = types.SevMedium
		}
		if !defined(i, "cooldown") {
			r.Cooldown = "5m"
		}
		if !defined(i, "max_runs") {
			r.MaxRuns = 2
		}
		if !defined(i, "verify_window") {
			r.VerifyWin = "10m"
		}
		if r.AutoGrants == nil {
			r.AutoGrants = []string{}
		}
	}
	return out, nil
}

// loadRuleDir reads every *.toml in dir, sorted by filename (SPEC-03 §3.5).
func loadRuleDir(dir string) ([]types.Rule, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors017, File: dir, Msg: err.Error(),
		}}}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var all []types.Rule
	var problems []*ruleProblem
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, File: p, Msg: err.Error()})
			continue
		}
		rs, err := decodeRulesTOML(b, p)
		if err != nil {
			if le, ok := err.(*ruleLoadErrors); ok {
				problems = append(problems, le.Problems...)
				continue
			}
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, File: p, Msg: err.Error()})
			continue
		}
		all = append(all, rs...)
	}
	if len(problems) > 0 {
		return nil, &ruleLoadErrors{Problems: problems}
	}
	return all, nil
}

func lineOfDecodeError(err error) int {
	s := err.Error()
	i := strings.Index(s, " line ")
	if i < 0 {
		return 0
	}
	var n int
	fmt.Sscanf(s[i:], " line %d", &n)
	return n
}

// keyLine finds the source line of a key. BurntSushi's Key carries no
// position, so the line is recovered from the raw file text by locating the
// key's last path element — approximate by design, and only ever used to make
// a refusal human-locatable.
func keyLine(raw []byte, target toml.Key) int {
	if len(target) == 0 {
		return 0
	}
	leaf := target[len(target)-1]
	line := 0
	for i, ln := range strings.Split(string(raw), "\n") {
		if strings.Contains(ln, leaf) {
			line = i + 1
			break
		}
	}
	return line
}

// ---- compilation and validation ----

// compileRules validates the schema (017) and every condition expression (018)
// for a decoded rule list, against the configured module allowlist.
func compileRules(rules []types.Rule, knownModules []string, modulesKnown bool) (*ruleSet, *ruleLoadErrors) {
	var problems []*ruleProblem
	seen := map[string]string{}
	counts := map[types.SigSource]int{}

	for i := range rules {
		r := &rules[i]
		loc := fmt.Sprintf("rule[%d]", i)
		if r.Name != "" {
			loc = fmt.Sprintf("rule[%d] %q", i, r.Name)
		}
		if !ruleNameRe.MatchString(r.Name) {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("%s: name must match %s, got %q", loc, ruleNameRe, r.Name)})
			continue
		}
		if prev, dup := seen[r.Name]; dup {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("%s: duplicate rule name %q (already defined at %s); SPEC-03 §3.5 refuses the whole new set", loc, r.Name, prev)})
			continue
		}
		seen[r.Name] = loc
		if !r.Source.Valid() || r.Source == types.SrcUnknown || r.Source == types.SrcGeneric || r.Source == types.SrcCollector || r.Source == types.SrcSentinel {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("source %q is not one of psi|journald|dbus|disk|timers|inotify", r.Source)})
			continue
		}
		counts[r.Source]++
		if !validRung(r.EntryRung) {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("entry_rung %q is not record|play|research|agent (outlets is not a legal entry rung)", r.EntryRung)})
		}
		if !validSeverity(r.Severity) {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("severity %q is not critical|high|medium|low|info", r.Severity)})
		}
		if d, ok := parseDur(r.Cooldown); !ok || d < 0 || d > maxCooldown {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("cooldown %q must be a non-negative duration ≤ %s", r.Cooldown, maxCooldown)})
		}
		if d, ok := parseDur(r.For); !ok || d < 0 {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("for %q must be a non-negative duration", r.For)})
		}
		if d, ok := parseDur(r.VerifyWin); !ok || d <= 0 || d > maxVerifyWindow {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("verify_window %q must be >0 and ≤ %s", r.VerifyWin, maxVerifyWindow)})
		}
		if r.MaxRuns < 1 {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
				Msg: fmt.Sprintf("max_runs must be ≥1, got %d", r.MaxRuns)})
		}
		if len(r.Match) > maxMatchEntries {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors018, Rule: r.Name,
				Msg: fmt.Sprintf("%d match entries, limit is %d", len(r.Match), maxMatchEntries)})
		}
		if modulesKnown {
			known := map[string]bool{}
			for _, m := range knownModules {
				known[m] = true
			}
			for _, g := range r.AutoGrants {
				if !known[g] {
					problems = append(problems, &ruleProblem{Code: types.CodeSensors017, Rule: r.Name,
						Msg: fmt.Sprintf("auto_grants names %q, which is not in the registry module list", g)})
				}
			}
		}
	}

	for src, n := range counts {
		if n > maxRulesPerSource {
			problems = append(problems, &ruleProblem{Code: types.CodeSensors017,
				Msg: fmt.Sprintf("%d rules for source %s, limit is %d", n, src, maxRulesPerSource)})
		}
	}
	if len(rules) > maxRulesTotal {
		problems = append(problems, &ruleProblem{Code: types.CodeSensors017,
			Msg: fmt.Sprintf("%d rules total, limit is %d", len(rules), maxRulesTotal)})
	}
	if len(problems) > 0 {
		return nil, &ruleLoadErrors{Problems: problems}
	}

	set := &ruleSet{bySource: map[types.SigSource][]compiledRule{}, rules: rules}
	if !modulesKnown {
		set.skipped = "auto_grants/registry check skipped: registry.modules not resolved (SPEC-06 owns the module list)"
	}
	for i := range rules {
		r := rules[i]
		scope := compileScope{source: r.Source, scopes: inferScopes(r.Match)}
		cr := compiledRule{rule: r, scope: scope.scopes, matchKey: matchFingerprint(r)}
		for _, c := range r.Match {
			prog, err := compileCondition(c, scope)
			if err != nil {
				ce, ok := err.(*compileError)
				p := &ruleProblem{Code: types.CodeSensors018, Rule: r.Name}
				if ok {
					p.Code = ce.Code
					p.Line = ce.Pos
					p.Msg = fmt.Sprintf("%s: %s (field %q)", r.Name, ce.Msg, c.Field)
				} else {
					p.Msg = fmt.Sprintf("%s: %v", r.Name, err)
				}
				problems = append(problems, p)
				continue
			}
			cr.prog = append(cr.prog, prog)
			cr.fields = append(cr.fields, prog.fields...)
		}
		set.bySource[r.Source] = append(set.bySource[r.Source], cr)
	}
	if len(problems) > 0 {
		return nil, &ruleLoadErrors{Problems: problems}
	}
	return set, nil
}

// inferScopes reads the rule's own `scope` constraints so a `full_*` field on
// a resource without a full line is refused at load (SPEC-03 §3.4 vectors).
func inferScopes(match []types.Condition) []string {
	var scopes []string
	for _, c := range match {
		if c.Field != "scope" {
			continue
		}
		switch c.Op {
		case "==":
			scopes = append(scopes, c.Value)
		case "in":
			vals := parseListLiteral(c.Value)
			scopes = append(scopes, vals...)
		}
	}
	return scopes
}

// parseListLiteral parses the JSON-array literal encoding of `in` (SPEC-03
// §3.4) into its string elements; a parse failure yields no elements and is
// reported by compileCondition.
func parseListLiteral(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// compileCondition compiles one Condition into an exprProgram.
func compileCondition(c types.Condition, scope compileScope) (exprProgram, error) {
	if strings.TrimSpace(c.Field) == "" {
		return exprProgram{}, condErr("match entry without a field")
	}
	switch c.Op {
	case "==", "!=", ">", ">=", "<", "<=", "~", "in":
	default:
		return exprProgram{}, condErr("unknown operator %q (== != > >= < <= ~ in)", c.Op)
	}
	// `~` with no pattern is refused before the regex compiler sees it.
	if c.Op == "~" && c.Value == "" {
		return exprProgram{}, condErr("regex condition on %q has an empty pattern", c.Field)
	}
	src := c.Field + " " + c.Op + " " + literal(c)
	return compileExpression(src, scope)
}

// literal renders a Condition's value as the expression literal its
// value_type declares: number, bool, string, or (for `in`) a list literal.
func literal(c types.Condition) string {
	v := strings.TrimSpace(c.Value)
	switch c.ValueType {
	case "number":
		return v
	case "bool":
		return v
	case "string":
		if c.Op == "in" {
			// SPEC-03 §3.4: `in` is encoded as a JSON array literal, e.g.
			// value = "[\"io\",\"memory\"]" with value_type = "string".
			return strings.ReplaceAll(v, `"`, `"`)
		}
		return quoteExpr(v)
	}
	// Unset value_type: infer from the field's declared type so a plain
	// `value = "failed"` works, but a numeric literal stays numeric.
	if v == "true" || v == "false" {
		return v
	}
	if isNumericLiteral(v) {
		return v
	}
	return quoteExpr(v)
}

func quoteExpr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func isNumericLiteral(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i++
	}
	digits := false
	for ; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digits = true
			continue
		}
		if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			continue
		}
		return false
	}
	return digits
}

func parseDur(d types.Duration) (time.Duration, bool) {
	if d == "" {
		return 0, true
	}
	t, err := time.ParseDuration(string(d))
	if err != nil {
		return 0, false
	}
	return t, true
}

func validRung(r types.Rung) bool {
	switch r {
	case types.RungRecord, types.RungPlay, types.RungResearch, types.RungAgent:
		return true
	}
	return false
}

func validSeverity(s types.Severity) bool {
	switch s {
	case types.SevCritical, types.SevHigh, types.SevMedium, types.SevLow, types.SevInfo:
		return true
	}
	return false
}

// matchFingerprint is the identity of a rule's activation predicate. A reload
// carries a stabilization timer over only when this fingerprint is unchanged
// (SPEC-03 §3.7: "rule present with byte-identical match + for").
func matchFingerprint(r types.Rule) string {
	h := sha256.New()
	h.Write([]byte(string(r.Source)))
	h.Write([]byte{0})
	h.Write([]byte(r.For))
	for _, c := range r.Match {
		h.Write([]byte{0})
		h.Write([]byte(c.Field))
		h.Write([]byte{1})
		h.Write([]byte(c.Op))
		h.Write([]byte{1})
		h.Write([]byte(c.Value))
		h.Write([]byte{1})
		h.Write([]byte(c.ValueType))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---- live rule set accessors ----

func (s *Sensors) ruleSet() *ruleSet { return s.rules.Load() }

// Rules returns the resolved, post-validation rule set (SPEC-03 §2).
func (s *Sensors) Rules() []types.Rule {
	rs := s.rules.Load()
	if rs == nil {
		return nil
	}
	out := make([]types.Rule, len(rs.rules))
	copy(out, rs.rules)
	return out
}

// ruleGeneration returns the installed generation number (monotone).
func (s *Sensors) ruleGeneration() uint64 {
	if rs := s.rules.Load(); rs != nil {
		return rs.gen
	}
	return 0
}

// ruleCount is used by the health reason and the perf test.
func (s *Sensors) ruleCount() int {
	if rs := s.rules.Load(); rs != nil {
		return len(rs.rules)
	}
	return 0
}

// installRules swaps in a new set with a single atomic store (SPEC-03 §3.7).
func (s *Sensors) installRules(set *ruleSet) {
	set.gen = s.ruleGen.Add(1)
	if set.bySource == nil {
		set.bySource = map[types.SigSource][]compiledRule{}
	}
	s.rules.Store(set)
}

// compileFromRules validates + compiles a rule list without installing it.
func (s *Sensors) compileFromRules(rules []types.Rule) (*ruleSet, *ruleLoadErrors) {
	return compileRules(rules, s.cfg.knownModules, s.cfg.modulesKnownSet)
}

// loadAndCompileFromDir reads the configured rules directory and compiles it.
func (s *Sensors) loadAndCompileFromDir() (*ruleSet, *ruleLoadErrors) {
	rules, err := loadRuleDir(s.cfg.rulesDir)
	if err != nil {
		if le, ok := err.(*ruleLoadErrors); ok {
			return nil, le
		}
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{Code: types.CodeSensors017, File: s.cfg.rulesDir, Msg: err.Error()}}}
	}
	return s.compileFromRules(rules)
}

// rulesDirFiles lists the rule files currently present (for the mtime sweep).
func rulesDirFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".toml") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// rulesMTimes is the reload backstop's cheap change detector.
func rulesMTimes(dir string) (string, error) {
	files, err := rulesDirFiles(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s:%d:%d\n", f, fi.Size(), fi.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

var _ = atomic.Pointer[ruleSet]{}
