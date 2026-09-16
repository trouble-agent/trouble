package sensors

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// This file is SPEC-03 §3.4: ONE expression dialect, defined once here and
// reused verbatim by SPEC-06 (`when:` in plays) and SPEC-05 (rule gates). No
// second dialect, no per-caller extensions.
//
// Pinned decisions encoded below (each has a test in expr_test.go):
//   - number|string|bool and homogeneous list literals; no null, no map;
//   - integer→float widening only — every other type mismatch is a
//     compile-time TROUBLE-SENSORS-018;
//   - `==`/`!=` are same-type and exact, numbers with a ±1e-9 relative
//     tolerance; string ordering (`>`,`<`,`>=`,`<=`) is a compile error;
//   - `~` is RE2, unanchored, ≤512-byte pattern, compiled once per rule;
//   - `in` requires a list literal right operand (encoded in a Condition.Value
//     string as a JSON array literal);
//   - a bare variable is legal only when its type is bool — no implicit
//     truthiness anywhere;
//   - precedence: not > and > or; parentheses always available;
//   - no arithmetic, no functions, no ternaries: a derived number must be a
//     field the sensor emits and can re-verify;
//   - the field namespace is closed per source/scope, so a typo can never
//     become a silently never-firing rule;
//   - bounds: ≤4096-byte expression, ≤32 match entries, depth ≤16, ≤64 vars.

// Expression bounds (SPEC-03 §3.4).
const (
	maxExprBytes    = 4096
	maxMatchEntries = 32
	maxASTDepth     = 16
	maxVars         = 64
	maxRegexBytes   = 512
)

// valueType is the expression value domain.
type valueType int

const (
	vtInvalid valueType = iota
	vtNumber
	vtString
	vtBool
	vtNumberList
	vtStringList
)

func (t valueType) String() string {
	switch t {
	case vtNumber:
		return "number"
	case vtString:
		return "string"
	case vtBool:
		return "bool"
	case vtNumberList:
		return "list[number]"
	case vtStringList:
		return "list[string]"
	}
	return "invalid"
}

// value is an evaluated operand. Unexported by SPEC-03 §3.10.
type value struct {
	typ   valueType
	num   float64
	str   string
	truth bool
	nums  []float64
	strs  []string
}

func numberValue(f float64) value { return value{typ: vtNumber, num: f} }
func stringValue(s string) value  { return value{typ: vtString, str: s} }
func boolValue(b bool) value      { return value{typ: vtBool, truth: b} }

type operandKind int

const (
	opndLiteral operandKind = iota
	opndVar
)

// operand is a compiled operand: a literal or a variable reference.
type operand struct {
	kind operandKind
	name string // variable path, "a.b.c"
	val  value  // literal value (or the var's declared type when kind==opndVar)
}

// conditionAST is the compiled expression tree (SPEC-03 §3.10).
type conditionAST struct {
	op       string // "" comparison | "and" | "or" | "not"
	left     operand
	right    operand
	re       *regexp.Regexp // compiled `~` pattern
	children []conditionAST
	pos      int
}

// exprProgram is one compiled expression plus its secret inventory (SPEC-03 §3.10).
type exprProgram struct {
	src    string
	ast    conditionAST
	fields []string
}

// compileError carries the pinned code and the source position so a refusal
// names the rule and the line (SPEC-03 §3.4, §5).
type compileError struct {
	Code types.ErrorCode
	Msg  string
	Pos  int
}

func (e *compileError) Error() string {
	if e.Pos > 0 {
		return fmt.Sprintf("%s: %s (at byte %d)", e.Code, e.Msg, e.Pos)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func condErr(format string, args ...any) *compileError {
	return &compileError{Code: types.CodeSensors018, Msg: fmt.Sprintf(format, args...)}
}

// ---- field namespace (closed, SPEC-03 §3.4) ----

// fieldTable is the closed variable namespace: source → field → type.
var fieldTable = map[types.SigSource]map[string]valueType{
	"": {
		// "any sensor event" row: resolvable for every sensor source.
		"value": vtNumber, "unit": vtString, "scope": vtString, "sensor": vtString,
		"source": vtString, "count": vtNumber, "window_s": vtNumber, "age_s": vtNumber,
		"severity": vtString, "substr": vtString, "msg": vtString,
		// `enabled` is the §3.4 vector table's bare-bool row: the gate variable
		// a rule or play is enabled under (SPEC-05/06 read it the same way).
		"enabled": vtBool,
	},
	types.SrcPSI: {
		"metric": vtString, "band": vtString,
		"some_avg10": vtNumber, "some_avg60": vtNumber, "some_avg300": vtNumber,
		"full_avg10": vtNumber, "full_avg60": vtNumber, "full_avg300": vtNumber,
		"some_total": vtNumber, "full_total": vtNumber,
		"stall_us": vtNumber, "wake": vtBool,
	},
	types.SrcJournald: {
		"unit": vtString, "comm": vtString, "priority": vtNumber,
		"priority_name": vtString, "non_utf8": vtBool, "truncated": vtBool,
	},
	types.SrcDBus: {
		"unit_key": vtString, "unit_active_state": vtString, "unit_substate": vtString,
		"unit_result": vtString, "exit_code": vtNumber, "crash_loop": vtBool,
		"arrival_path": vtString,
	},
	types.SrcDisk: {
		"mount": vtString, "fstype": vtString, "free_pct": vtNumber,
		"free_bytes": vtNumber, "total_bytes": vtNumber, "inode_free_pct": vtNumber,
		"read_only": vtBool,
	},
	types.SrcTimers: {
		"timer_unit": vtString, "missed_runs": vtNumber,
		"next_elapse_in_s": vtNumber, "last_run_age_s": vtNumber,
	},
	types.SrcInotify: {
		"path": vtString, "mask_class": vtString, "overflow": vtBool,
	},
	types.SrcSentinel: {
		"level": vtString, "release": vtString, "env": vtString,
		"culprit": vtString, "release_diff": vtNumber,
	},
}

// psiFullResources are the resources that have a `full` line (SPEC-03 §3.4
// vector: `full_avg60 >= 5` on a cpu event is a compile error).
var psiFullResources = map[string]bool{"memory": true, "io": true}

// compileScope is the closed-namespace context of one compile: the source, the
// scope set the rule constrains itself to (nil = unconstrained), and whether
// unknown names are a refusal (rules) or a legal false (play `when:`).
type compileScope struct {
	source      types.SigSource
	scopes      []string
	playScope   bool // SPEC-06: unknown result.output.<k> is false, never an error
	allowNoVars bool
}

func (s compileScope) typeOf(name string) (valueType, bool) {
	if s.playScope {
		if t, ok := playFields[name]; ok {
			return t, true
		}
		// SPEC-03 §3.4 last row (referenced by SPEC-06): an unknown
		// result.output.<k> / registered.<name> in a play is false +
		// rule.field_absent, never an error — so its type is "unknown" and the
		// runtime decides.
		if strings.HasPrefix(name, "result.output.") || strings.HasPrefix(name, "registered.") {
			return vtInvalid, true
		}
	}
	if t, ok := fieldTable[""][name]; ok {
		return t, true
	}
	t, ok := fieldTable[s.source][name]
	if !ok {
		return vtInvalid, false
	}
	// full_* exists only on resources with a `full` line. With the scope
	// constrained away from memory/io, referencing it is a load-time refusal
	// instead of a rule that can never fire (the inert-feature class).
	if strings.HasPrefix(name, "full_") && len(s.scopes) > 0 {
		for _, sc := range s.scopes {
			if !psiFullResources[sc] {
				return vtInvalid, false
			}
		}
	}
	return t, true
}

// playFields is SPEC-06's additive row of the namespace.
var playFields = map[string]valueType{
	"result.changed":   vtBool,
	"item":             vtString,
	"run.attempt":      vtNumber,
	"inc.severity":     vtString,
	"inc.state":        vtString,
	"autonomy.mode":    vtString,
	"result.output.ok": vtBool,
}

// ---- lexer ----

type tokKind int

const (
	tkEOF tokKind = iota
	tkIdent
	tkNumber
	tkString
	tkBool
	tkOp
	tkLParen
	tkRParen
	tkLBrack
	tkRBrack
	tkComma
	// keyword tokens
	tkAnd
	tkOr
	tkNot
	tkIn
)

type token struct {
	kind tokKind
	text string
	pos  int
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) errf(format string, args ...any) *compileError {
	e := condErr(format, args...)
	e.Pos = l.pos
	return e
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
		l.pos++
	}
	start := l.pos
	if l.pos >= len(l.src) {
		return token{kind: tkEOF, pos: start}, nil
	}
	c := l.src[l.pos]
	switch {
	case c == '(':
		l.pos++
		return token{kind: tkLParen, text: "(", pos: start}, nil
	case c == ')':
		l.pos++
		return token{kind: tkRParen, text: ")", pos: start}, nil
	case c == '[':
		l.pos++
		return token{kind: tkLBrack, text: "[", pos: start}, nil
	case c == ']':
		l.pos++
		return token{kind: tkRBrack, text: "]", pos: start}, nil
	case c == ',':
		l.pos++
		return token{kind: tkComma, text: ",", pos: start}, nil
	case c == '"' || c == '\'':
		return l.lexString(c)
	case c == '~':
		l.pos++
		return token{kind: tkOp, text: "~", pos: start}, nil
	case c == '=':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tkOp, text: "==", pos: start}, nil
		}
		return token{}, l.errf("expected == but found =")
	case c == '!':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tkOp, text: "!=", pos: start}, nil
		}
		return token{}, l.errf("expected != but found !")
	case c == '>':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tkOp, text: ">=", pos: start}, nil
		}
		l.pos++
		return token{kind: tkOp, text: ">", pos: start}, nil
	case c == '<':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return token{kind: tkOp, text: "<=", pos: start}, nil
		}
		l.pos++
		return token{kind: tkOp, text: "<", pos: start}, nil
	case isDigit(c) || ((c == '-' || c == '+') && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		return l.lexNumber()
	case isIdentStart(c):
		return l.lexIdent()
	}
	return token{}, l.errf("unexpected character %q", string(c))
}

func (l *lexer) lexString(quote byte) (token, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\\' && l.pos+1 < len(l.src) {
			l.pos += 2
			switch l.src[l.pos-1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\'':
				b.WriteByte('\'')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(l.src[l.pos-1])
			}
			continue
		}
		if c == quote {
			l.pos++
			return token{kind: tkString, text: b.String(), pos: start}, nil
		}
		b.WriteByte(c)
		l.pos++
	}
	return token{}, l.errf("unterminated string literal")
}

func (l *lexer) lexNumber() (token, error) {
	start := l.pos
	if l.src[l.pos] == '-' || l.src[l.pos] == '+' {
		l.pos++
	}
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		save := l.pos
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.pos++
			}
		} else {
			l.pos = save
		}
	}
	return token{kind: tkNumber, text: l.src[start:l.pos], pos: start}, nil
}

func (l *lexer) lexIdent() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	// Dotted paths are one token only if the next element is an identifier.
	for l.pos+1 < len(l.src) && l.src[l.pos] == '.' && isIdentStart(l.src[l.pos+1]) {
		l.pos++
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
	}
	word := l.src[start:l.pos]
	switch word {
	case "and":
		return token{kind: tkAnd, text: word, pos: start}, nil
	case "or":
		return token{kind: tkOr, text: word, pos: start}, nil
	case "not":
		return token{kind: tkNot, text: word, pos: start}, nil
	case "in":
		return token{kind: tkIn, text: word, pos: start}, nil
	case "true":
		return token{kind: tkBool, text: "true", pos: start}, nil
	case "false":
		return token{kind: tkBool, text: "false", pos: start}, nil
	}
	return token{kind: tkIdent, text: word, pos: start}, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// ---- parser ----

type parser struct {
	lex   *lexer
	cur   token
	scope compileScope
	vars  map[string]valueType
}

// Compile compiles a full expression against a compile scope (SPEC-03 §3.4).
func compileExpression(src string, scope compileScope) (exprProgram, error) {
	if len(src) > maxExprBytes {
		return exprProgram{}, condErr("expression is %d bytes, limit is %d", len(src), maxExprBytes)
	}
	if strings.TrimSpace(src) == "" {
		return exprProgram{}, condErr("expression is empty")
	}
	p := &parser{lex: &lexer{src: src}, scope: scope, vars: map[string]valueType{}}
	if err := p.advance(); err != nil {
		return exprProgram{}, err
	}
	ast, err := p.parseOr(0)
	if err != nil {
		return exprProgram{}, err
	}
	if p.cur.kind != tkEOF {
		return exprProgram{}, p.errf("unexpected trailing token %q", p.cur.text)
	}
	if len(p.vars) > maxVars {
		return exprProgram{}, condErr("expression references %d variables, limit is %d", len(p.vars), maxVars)
	}
	fields := make([]string, 0, len(p.vars))
	for name := range eachVarName(ast) {
		fields = append(fields, name)
	}
	sortStrings(fields)
	return exprProgram{src: src, ast: ast, fields: fields}, nil
}

func eachVarName(ast conditionAST) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(a conditionAST)
	walk = func(a conditionAST) {
		if a.left.kind == opndVar {
			out[a.left.name] = struct{}{}
		}
		if a.right.kind == opndVar {
			out[a.right.name] = struct{}{}
		}
		for _, c := range a.children {
			walk(c)
		}
	}
	walk(ast)
	return out
}

func (p *parser) errf(format string, args ...any) *compileError {
	e := condErr(format, args...)
	e.Pos = p.cur.pos
	return e
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.cur = t
	return nil
}

func (p *parser) parseOr(depth int) (conditionAST, error) {
	if depth > maxASTDepth {
		return conditionAST{}, condErr("expression nesting exceeds the depth limit of %d", maxASTDepth)
	}
	left, err := p.parseAnd(depth)
	if err != nil {
		return conditionAST{}, err
	}
	children := []conditionAST{left}
	for p.cur.kind == tkOr {
		if err := p.advance(); err != nil {
			return conditionAST{}, err
		}
		right, err := p.parseAnd(depth)
		if err != nil {
			return conditionAST{}, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return left, nil
	}
	return conditionAST{op: "or", children: children, pos: children[0].pos}, nil
}

func (p *parser) parseAnd(depth int) (conditionAST, error) {
	left, err := p.parseNot(depth)
	if err != nil {
		return conditionAST{}, err
	}
	children := []conditionAST{left}
	for p.cur.kind == tkAnd {
		if err := p.advance(); err != nil {
			return conditionAST{}, err
		}
		right, err := p.parseNot(depth)
		if err != nil {
			return conditionAST{}, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return left, nil
	}
	return conditionAST{op: "and", children: children, pos: children[0].pos}, nil
}

func (p *parser) parseNot(depth int) (conditionAST, error) {
	if depth > maxASTDepth {
		return conditionAST{}, condErr("expression nesting exceeds the depth limit of %d", maxASTDepth)
	}
	if p.cur.kind == tkNot {
		pos := p.cur.pos
		if err := p.advance(); err != nil {
			return conditionAST{}, err
		}
		inner, err := p.parseNot(depth + 1)
		if err != nil {
			return conditionAST{}, err
		}
		return conditionAST{op: "not", children: []conditionAST{inner}, pos: pos}, nil
	}
	return p.parsePrimary(depth)
}

func (p *parser) parsePrimary(depth int) (conditionAST, error) {
	if depth > maxASTDepth {
		return conditionAST{}, condErr("expression nesting exceeds the depth limit of %d", maxASTDepth)
	}
	switch p.cur.kind {
	case tkLParen:
		if err := p.advance(); err != nil {
			return conditionAST{}, err
		}
		inner, err := p.parseOr(depth + 1)
		if err != nil {
			return conditionAST{}, err
		}
		if p.cur.kind != tkRParen {
			return conditionAST{}, p.errf("expected ) to close the group")
		}
		if err := p.advance(); err != nil {
			return conditionAST{}, err
		}
		return inner, nil
	case tkIdent, tkNumber, tkString, tkBool, tkLBrack:
		return p.parseComparison()
	}
	return conditionAST{}, p.errf("expected a comparison but found %q", p.cur.text)
}

func (p *parser) parseComparison() (conditionAST, error) {
	pos := p.cur.pos
	left, err := p.parseOperand()
	if err != nil {
		return conditionAST{}, err
	}
	var op string
	switch p.cur.kind {
	case tkOp:
		op = p.cur.text
	case tkIn:
		op = "in"
	default:
		// A bare operand is legal only when it is a bool variable.
		if left.kind != opndVar {
			return conditionAST{}, condErr("a bare %s is not a condition (no implicit truthiness; SPEC-03 §3.4)", left.val.typ)
		}
		if left.val.typ != vtBool && left.val.typ != vtInvalid {
			return conditionAST{}, condErr("bare variable %q has type %s; only bool variables are legal on their own", left.name, left.val.typ)
		}
		return conditionAST{left: left, pos: pos}, nil
	}
	if err := p.advance(); err != nil {
		return conditionAST{}, err
	}
	right, err := p.parseOperand()
	if err != nil {
		return conditionAST{}, err
	}
	ast := conditionAST{op: op, left: left, right: right, pos: pos}
	if err := p.typecheck(&ast); err != nil {
		return conditionAST{}, err
	}
	return ast, nil
}

func (p *parser) parseOperand() (operand, error) {
	switch p.cur.kind {
	case tkNumber:
		f, err := strconv.ParseFloat(p.cur.text, 64)
		if err != nil {
			return operand{}, p.errf("malformed number %q", p.cur.text)
		}
		op := operand{kind: opndLiteral, val: numberValue(f)}
		if err := p.advance(); err != nil {
			return operand{}, err
		}
		return op, nil
	case tkString:
		op := operand{kind: opndLiteral, val: stringValue(p.cur.text)}
		if err := p.advance(); err != nil {
			return operand{}, err
		}
		return op, nil
	case tkBool:
		op := operand{kind: opndLiteral, val: boolValue(p.cur.text == "true")}
		if err := p.advance(); err != nil {
			return operand{}, err
		}
		return op, nil
	case tkIdent:
		name := p.cur.text
		t, known := p.scope.typeOf(name)
		if !known {
			if p.scope.playScope {
				// SPEC-03 §3.4 last row: an unknown variable in a play is
				// false + rule.field_absent, never an error.
				t = vtInvalid
			} else {
				return operand{}, p.errf("unknown field %q for source %q (the namespace is closed)", name, p.scope.source)
			}
		}
		if _, seen := p.vars[name]; !seen {
			if len(p.vars) >= maxVars {
				return operand{}, condErr("expression references more than %d variables", maxVars)
			}
			p.vars[name] = t
		}
		op := operand{kind: opndVar, name: name, val: value{typ: t}}
		if err := p.advance(); err != nil {
			return operand{}, err
		}
		return op, nil
	case tkLBrack:
		return p.parseList()
	}
	return operand{}, p.errf("expected an operand but found %q", p.cur.text)
}

func (p *parser) parseList() (operand, error) {
	if err := p.advance(); err != nil {
		return operand{}, err
	}
	var nums []float64
	var strs []string
	elem := vtInvalid
	for p.cur.kind != tkRBrack {
		switch p.cur.kind {
		case tkNumber:
			f, err := strconv.ParseFloat(p.cur.text, 64)
			if err != nil {
				return operand{}, p.errf("malformed number %q", p.cur.text)
			}
			if elem == vtString || elem == vtStringList {
				return operand{}, p.errf("list literals must be homogeneous")
			}
			elem = vtNumber
			nums = append(nums, f)
		case tkString:
			if elem == vtNumber {
				return operand{}, p.errf("list literals must be homogeneous")
			}
			elem = vtString
			strs = append(strs, p.cur.text)
		default:
			return operand{}, p.errf("list literals hold numbers or strings only, found %q", p.cur.text)
		}
		if err := p.advance(); err != nil {
			return operand{}, err
		}
		if p.cur.kind == tkComma {
			if err := p.advance(); err != nil {
				return operand{}, err
			}
			continue
		}
		break
	}
	if p.cur.kind != tkRBrack {
		return operand{}, p.errf("expected ] to close the list literal")
	}
	if err := p.advance(); err != nil {
		return operand{}, err
	}
	v := value{}
	switch elem {
	case vtNumber:
		v = value{typ: vtNumberList, nums: nums}
	case vtString:
		v = value{typ: vtStringList, strs: strs}
	default:
		// An empty list compiles and is always false (SPEC-03 §3.4 `in` row).
		v = value{typ: vtStringList}
	}
	return operand{kind: opndLiteral, val: v}, nil
}

// typecheck enforces the pinned typing rules on one comparison node.
func (p *parser) typecheck(ast *conditionAST) error {
	l, r := ast.left, ast.right
	// An unknown play variable has no compile-time type: the runtime answer is
	// false + field_absent, so typing is deferred rather than refused.
	if l.val.typ == vtInvalid || (r.kind == opndVar && r.val.typ == vtInvalid) {
		return nil
	}
	lNum, lStr, lBool := l.val.typ == vtNumber, l.val.typ == vtString, l.val.typ == vtBool
	switch ast.op {
	case "==", "!=":
		if lNum && r.val.typ == vtNumber || lStr && r.val.typ == vtString || lBool && r.val.typ == vtBool {
			return nil
		}
		// Numbers widen from integers only; nothing else coerces.
		return condErr("cannot compare %s %s %s (SPEC-03 §3.4: no coercion beyond integer→float)", l.val.typ, ast.op, r.val.typ)
	case ">", ">=", "<", "<=":
		if lStr || r.val.typ == vtString {
			return condErr("string ordering with %s is a compile error (SPEC-03 §3.4)", ast.op)
		}
		if !lNum || r.val.typ != vtNumber {
			return condErr("cannot order %s %s %s", l.val.typ, ast.op, r.val.typ)
		}
		return nil
	case "~":
		if !lStr || r.val.typ != vtString {
			return condErr("~ requires string operands, got %s ~ %s", l.val.typ, r.val.typ)
		}
		if len(r.val.str) > maxRegexBytes {
			return condErr("regex pattern is %d bytes, limit is %d", len(r.val.str), maxRegexBytes)
		}
		re, err := regexp.Compile(r.val.str)
		if err != nil {
			return condErr("invalid regex %q: %v", r.val.str, err)
		}
		ast.re = re
		return nil
	case "in":
		if r.kind != opndLiteral || (r.val.typ != vtStringList && r.val.typ != vtNumberList) {
			return condErr("`in` requires a list literal on the right")
		}
		if lNum && r.val.typ != vtNumberList {
			return condErr("number in %s is a type mismatch", r.val.typ)
		}
		if lStr && r.val.typ != vtStringList {
			return condErr("string in %s is a type mismatch", r.val.typ)
		}
		if !lNum && !lStr {
			return condErr("`in` needs a string or number on the left, got %s", l.val.typ)
		}
		return nil
	case "":
		return nil
	}
	return condErr("unknown operator %q", ast.op)
}

// ---- evaluation ----

// evalScope holds the variables of one evaluation (SPEC-03 §3.10).
type evalScope struct {
	vars    map[string]value
	missing uint64
}

func newEvalScope(vars map[string]value) *evalScope { return &evalScope{vars: vars} }

func (s *evalScope) lookup(name string) (value, bool) {
	v, ok := s.vars[name]
	if !ok {
		s.missing++
		return value{typ: vtInvalid}, false
	}
	return v, true
}

// eval evaluates a compiled program. A missing variable is false, never an
// error (SPEC-03 §3.4).
func (p exprProgram) eval(sc *evalScope) bool {
	return evalNode(p.ast, sc)
}

func evalNode(a conditionAST, sc *evalScope) bool {
	switch a.op {
	case "and":
		for _, c := range a.children {
			if !evalNode(c, sc) {
				return false
			}
		}
		return true
	case "or":
		for _, c := range a.children {
			if evalNode(c, sc) {
				return true
			}
		}
		return false
	case "not":
		return !evalNode(a.children[0], sc)
	}
	l, lok := operandValue(a.left, sc)
	if !lok {
		return false
	}
	if a.op == "" {
		return l.typ == vtBool && l.truth
	}
	r, rok := operandValue(a.right, sc)
	if !rok {
		return false
	}
	switch a.op {
	case "==":
		return valuesEqual(l, r)
	case "!=":
		return !valuesEqual(l, r)
	case ">":
		return l.num > r.num
	case ">=":
		return l.num >= r.num
	case "<":
		return l.num < r.num
	case "<=":
		return l.num <= r.num
	case "~":
		return a.re != nil && a.re.MatchString(l.str)
	case "in":
		if l.typ == vtNumber {
			for _, n := range r.nums {
				if numbersEqual(l.num, n) {
					return true
				}
			}
			return false
		}
		for _, s := range r.strs {
			if l.str == s {
				return true
			}
		}
		return false
	}
	return false
}

func operandValue(o operand, sc *evalScope) (value, bool) {
	if o.kind == opndLiteral {
		return o.val, true
	}
	return sc.lookup(o.name)
}

func valuesEqual(l, r value) bool {
	if l.typ != r.typ {
		if l.typ == vtNumber && r.typ == vtNumber {
			return numbersEqual(l.num, r.num)
		}
		if (l.typ == vtInvalid) != (r.typ == vtInvalid) {
			// A missing variable never equals a present one.
			return false
		}
		// Integer→float widening is a compile-time property, so reaching here
		// means a genuinely different type: not equal.
		if l.typ == vtNumber && r.typ == vtNumber {
			return numbersEqual(l.num, r.num)
		}
		return false
	}
	switch l.typ {
	case vtNumber:
		return numbersEqual(l.num, r.num)
	case vtString:
		return l.str == r.str
	case vtBool:
		return l.truth == r.truth
	}
	return false
}

// numbersEqual is the ±1e-9 relative tolerance of SPEC-03 §3.4.
func numbersEqual(a, b float64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	diff := math.Abs(a - b)
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= 1e-9*scale
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
