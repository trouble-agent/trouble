package sensors

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// expr_test.go covers SPEC-03 §7's expr row: the 14 published vectors in both
// directions plus short-circuit, `in` shapes, `not` precedence, the size/depth/
// variable bounds and the regex budget. Every refusal asserts
// TROUBLE-SENSORS-018.

func evalExpr(t *testing.T, expr string, source types.SigSource, scopes []string, vars map[string]any) (bool, error) {
	t.Helper()
	prog, err := compileExpression(expr, compileScope{source: source, scopes: scopes})
	if err != nil {
		return false, err
	}
	m := map[string]value{}
	for k, v := range vars {
		val, ok := toValue(v)
		if !ok {
			t.Fatalf("test var %s has an unsupported type %T", k, v)
		}
		m[k] = val
	}
	return prog.eval(newEvalScope(m)), nil
}

// TestVectorsPublished walks the §3.4 vector table exactly as written.
func TestVectorsPublished(t *testing.T) {
	type vec struct {
		name    string
		expr    string
		source  types.SigSource
		scopes  []string
		vars    map[string]any
		want    bool
		wantErr bool
	}
	vectors := []vec{
		{name: "psi avg10 high", expr: `some_avg10 >= 35`, source: types.SrcPSI, scopes: []string{"io"},
			vars: map[string]any{"some_avg10": 41.7}, want: true},
		{name: "psi avg10 + scope in", expr: `some_avg10 >= 35 and scope in ["io","memory"]`, source: types.SrcPSI,
			scopes: []string{"io", "memory"}, vars: map[string]any{"some_avg10": 41.7, "scope": "io"}, want: true},
		{name: "disk or", expr: `free_pct < 10 or inode_free_pct < 10`, source: types.SrcDisk,
			vars: map[string]any{"free_pct": 22.0, "inode_free_pct": 30.0}, want: false},
		{name: "dbus substate", expr: `unit_substate == "failed"`, source: types.SrcDBus,
			vars: map[string]any{"unit_substate": "failed"}, want: true},
		{name: "regex msg", expr: `msg ~ "(?i)panic|segfault"`, source: types.SrcPSI,
			vars: map[string]any{"msg": "kernel: SEGFAULT in worker"}, want: true},
		{name: "not group", expr: `not (value > 100)`, source: types.SrcPSI,
			vars: map[string]any{"value": 41.7}, want: true},
		{name: "count in list", expr: `count in [1,2,3]`, source: types.SrcPSI,
			vars: map[string]any{"count": 3}, want: true},
		{name: "bare bool", expr: `enabled`, source: types.SrcPSI,
			vars: map[string]any{"enabled": true}, want: true},
		{name: "full on cpu", expr: `full_avg60 >= 5`, source: types.SrcPSI, scopes: []string{"cpu"},
			vars: map[string]any{"full_avg60": 12.0}, wantErr: true},
		{name: "bare number", expr: `value`, source: types.SrcPSI,
			vars: map[string]any{"value": 41.7}, wantErr: true},
		{name: "string ordering", expr: `release > "1.0"`, source: types.SrcSentinel,
			vars: map[string]any{"release": "2.0"}, wantErr: true},
		{name: "no coercion", expr: `some_avg10 >= "35"`, source: types.SrcPSI,
			vars: map[string]any{"some_avg10": 41.7}, wantErr: true},
		{name: "trailing and", expr: `some_avg10 >= 35 and`, source: types.SrcPSI, wantErr: true},
		{name: "bad regex", expr: `msg ~ "([unclosed"`, source: types.SrcPSI, wantErr: true},
	}
	for _, v := range vectors {
		got, err := evalExpr(t, v.expr, v.source, v.scopes, v.vars)
		if v.wantErr {
			if err == nil {
				t.Errorf("%s: %q compiled and returned %v; expected a refusal", v.name, v.expr, got)
				continue
			}
			var ce *compileError
			if !asCompileError(err, &ce) {
				t.Errorf("%s: refusal is not a compile error: %v", v.name, err)
				continue
			}
			if ce.Code != types.CodeSensors018 {
				t.Errorf("%s: code = %s, want TROUBLE-SENSORS-018", v.name, ce.Code)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", v.name, err)
			continue
		}
		if got != v.want {
			t.Errorf("%s: %q = %v, want %v", v.name, v.expr, got, v.want)
		}
	}
}

func asCompileError(err error, out **compileError) bool {
	ce, ok := err.(*compileError)
	if !ok {
		return false
	}
	*out = ce
	return true
}

// TestVectorsReverseDirection asserts the false arm of the same table: a
// vector that returns true for one input must return false for its complement.
func TestVectorsReverseDirection(t *testing.T) {
	cases := []struct {
		expr   string
		src    types.SigSource
		sc     []string
		true_  map[string]any
		false_ map[string]any
	}{
		{`some_avg10 >= 35`, types.SrcPSI, []string{"io"}, map[string]any{"some_avg10": 41.7}, map[string]any{"some_avg10": 12.0}},
		{`unit_substate == "failed"`, types.SrcDBus, nil, map[string]any{"unit_substate": "failed"}, map[string]any{"unit_substate": "running"}},
		{`msg ~ "(?i)panic|segfault"`, types.SrcPSI, nil, map[string]any{"msg": "PANIC"}, map[string]any{"msg": "all good"}},
		{`not (value > 100)`, types.SrcPSI, nil, map[string]any{"value": 41.7}, map[string]any{"value": 101.0}},
		{`count in [1,2,3]`, types.SrcPSI, nil, map[string]any{"count": 3}, map[string]any{"count": 9}},
	}
	for _, c := range cases {
		got, err := evalExpr(t, c.expr, c.src, c.sc, c.true_)
		if err != nil || !got {
			t.Errorf("%s with %v: got %v err %v, want true", c.expr, c.true_, got, err)
		}
		got, err = evalExpr(t, c.expr, c.src, c.sc, c.false_)
		if err != nil || got {
			t.Errorf("%s with %v: got %v err %v, want false", c.expr, c.false_, got, err)
		}
	}
}

func TestTruthinessAndPrecedence(t *testing.T) {
	cases := []struct {
		name string
		expr string
		vars map[string]any
		want bool
	}{
		{"not binds tighter than and", `not wake and some_avg10 >= 10`, map[string]any{"wake": false, "some_avg10": 20.0}, true},
		{"and binds tighter than or", `some_avg10 >= 100 or some_avg10 >= 10 and wake`, map[string]any{"some_avg10": 20.0, "wake": true}, true},
		{"false and short-circuits", `wake and some_avg10 >= 10`, map[string]any{"wake": false, "some_avg10": 20.0}, false},
		{"in with one element", `scope in ["io"]`, map[string]any{"scope": "io"}, true},
		{"empty list is always false", `scope in []`, map[string]any{"scope": "io"}, false},
		{"float tolerance", `some_avg10 == 0.3`, map[string]any{"some_avg10": 0.1 + 0.2}, true},
		{"not equal", `scope != "cpu"`, map[string]any{"scope": "io"}, true},
		{"bool equality", `wake == true`, map[string]any{"wake": false}, false},
		{"greater than or equal boundary", `some_avg10 >= 35`, map[string]any{"some_avg10": 35.0}, true},
		{"strict less than boundary", `some_avg10 < 35`, map[string]any{"some_avg10": 35.0}, false},
	}
	for _, c := range cases {
		got, err := evalExpr(t, c.expr, types.SrcPSI, []string{"io", "cpu", "memory"}, c.vars)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %q = %v, want %v", c.name, c.expr, got, c.want)
		}
	}
}

// TestMissingFieldIsFalse: a statically valid but conditionally absent field
// evaluates false and is counted, never an error (the `full_*` on non-memory
// resources row).
func TestMissingFieldIsFalse(t *testing.T) {
	prog, err := compileExpression(`full_avg60 >= 5`, compileScope{source: types.SrcPSI})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	sc := newEvalScope(map[string]value{"some_avg10": numberValue(1)})
	if prog.eval(sc) {
		t.Fatal("an absent field must be false")
	}
	if sc.missing == 0 {
		t.Fatal("an absent field must be counted (rule.field_absent)")
	}
}

// TestUnconstrainedFullIsAllowedButConstrainedIsRefused pins the two arms of
// the scope-aware field rule.
func TestUnconstrainedFullIsAllowedButConstrainedIsRefused(t *testing.T) {
	if _, err := compileExpression(`full_avg60 >= 5`, compileScope{source: types.SrcPSI}); err != nil {
		t.Fatalf("unconstrained full_avg60 must compile: %v", err)
	}
	for _, scopes := range [][]string{{"cpu"}, {"cpu", "io"}} {
		_, err := compileExpression(`full_avg60 >= 5`, compileScope{source: types.SrcPSI, scopes: scopes})
		if err == nil {
			t.Fatalf("full_avg60 with scopes %v must be refused at load", scopes)
		}
	}
	if _, err := compileExpression(`full_avg60 >= 5`, compileScope{source: types.SrcPSI, scopes: []string{"memory"}}); err != nil {
		t.Fatalf("full_avg60 on memory must compile: %v", err)
	}
}

func TestBounds(t *testing.T) {
	// 4096-byte expression boundary: a filler comment-free expression of legal
	// shape just under the limit compiles; one over is refused.
	big := `some_avg10 >= 0` + strings.Repeat(" and some_avg10 >= 0", 180)
	if len(big) > maxExprBytes {
		t.Fatalf("test fixture is %d bytes; the boundary case must be under the limit", len(big))
	}
	if _, err := compileExpression(big, compileScope{source: types.SrcPSI}); err != nil {
		t.Fatalf("an expression under %d bytes must compile: %v", maxExprBytes, err)
	}
	huge := big + strings.Repeat(" ", maxExprBytes)
	if _, err := compileExpression(huge, compileScope{source: types.SrcPSI}); err == nil {
		t.Fatalf("an expression over %d bytes must be refused", maxExprBytes)
	}

	// Depth: 16 is allowed, 17 is refused.
	depth16 := strings.Repeat("(", maxASTDepth) + `some_avg10 >= 0` + strings.Repeat(")", maxASTDepth)
	if _, err := compileExpression(depth16, compileScope{source: types.SrcPSI}); err != nil {
		t.Fatalf("depth %d must compile: %v", maxASTDepth, err)
	}
	depth17 := strings.Repeat("(", maxASTDepth+1) + `some_avg10 >= 0` + strings.Repeat(")", maxASTDepth+1)
	if _, err := compileExpression(depth17, compileScope{source: types.SrcPSI}); err == nil {
		t.Fatalf("depth %d must be refused", maxASTDepth+1)
	}

	// Variable count: >64 distinct variables is refused. The namespace is
	// closed, so the bound is proven with the one variable kind that can repeat
	// legally: distinct play variables (SPEC-06's additive row).
	var pb strings.Builder
	for i := 0; i <= maxVars; i++ {
		if i > 0 {
			pb.WriteString(" and ")
		}
		fmt.Fprintf(&pb, "result.output.k%d == \"x\"", i)
	}
	if _, err := compileExpression(pb.String(), compileScope{playScope: true}); err == nil {
		t.Fatalf("more than %d variables must be refused", maxVars)
	}

	// Regex budget: >512 bytes is refused.
	long := strings.Repeat("a", maxRegexBytes+1)
	if _, err := compileExpression(`msg ~ "`+long+`"`, compileScope{source: types.SrcPSI}); err == nil {
		t.Fatalf("a regex over %d bytes must be refused", maxRegexBytes)
	}
}

// TestPlayScopeUnknownVarIsFalse is SPEC-06's row: an unknown variable in a
// play is false + field_absent, never an error.
func TestPlayScopeUnknownVarIsFalse(t *testing.T) {
	prog, err := compileExpression(`result.output.exit_code == 0`, compileScope{playScope: true})
	if err != nil {
		t.Fatalf("unknown play variable must compile: %v", err)
	}
	sc := newEvalScope(map[string]value{})
	if prog.eval(sc) {
		t.Fatal("an unknown play variable must be false")
	}
	if sc.missing == 0 {
		t.Fatal("an unknown play variable must be counted")
	}
}

// TestCompileConditionShapes covers the Condition → expression path including
// the JSON-array `in` encoding and value_type inference.
func TestCompileConditionShapes(t *testing.T) {
	cases := []struct {
		name    string
		cond    types.Condition
		src     types.SigSource
		scopes  []string
		vars    map[string]any
		want    bool
		wantErr bool
	}{
		{name: "number", cond: types.Condition{Field: "some_avg10", Op: ">=", Value: "35", ValueType: "number"},
			src: types.SrcPSI, vars: map[string]any{"some_avg10": 41.7}, want: true},
		{name: "string", cond: types.Condition{Field: "unit_substate", Op: "==", Value: "failed", ValueType: "string"},
			src: types.SrcDBus, vars: map[string]any{"unit_substate": "failed"}, want: true},
		{name: "bool", cond: types.Condition{Field: "crash_loop", Op: "==", Value: "true", ValueType: "bool"},
			src: types.SrcDBus, vars: map[string]any{"crash_loop": true}, want: true},
		{name: "in json array", cond: types.Condition{Field: "scope", Op: "in", Value: `["io","memory"]`, ValueType: "string"},
			src: types.SrcPSI, vars: map[string]any{"scope": "memory"}, want: true},
		{name: "inferred value_type", cond: types.Condition{Field: "unit_substate", Op: "==", Value: "failed"},
			src: types.SrcDBus, vars: map[string]any{"unit_substate": "failed"}, want: true},
		{name: "unknown field", cond: types.Condition{Field: "no_such_field", Op: "==", Value: "x", ValueType: "string"},
			src: types.SrcPSI, wantErr: true},
		{name: "bad op", cond: types.Condition{Field: "value", Op: "=~", Value: "x", ValueType: "string"},
			src: types.SrcPSI, wantErr: true},
		{name: "empty field", cond: types.Condition{Field: "", Op: "==", Value: "x"},
			src: types.SrcPSI, wantErr: true},
	}
	for _, c := range cases {
		prog, err := compileCondition(c.cond, compileScope{source: c.src, scopes: c.scopes})
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected a refusal", c.name)
			} else if ce, ok := err.(*compileError); !ok || ce.Code != types.CodeSensors018 {
				t.Errorf("%s: wrong error: %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		m := map[string]value{}
		for k, v := range c.vars {
			m[k], _ = toValue(v)
		}
		if got := prog.eval(newEvalScope(m)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestParseListLiteral(t *testing.T) {
	cases := map[string][]string{
		`["io"]`:           {"io"},
		`["io","memory"]`:  {"io", "memory"},
		`[]`:               nil,
		`["a","b","c"]`:    {"a", "b", "c"},
		`["io", "memory"]`: {"io", "memory"},
		`not a list`:       nil,
	}
	for in, want := range cases {
		got := parseListLiteral(in)
		if len(got) != len(want) {
			t.Errorf("parseListLiteral(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseListLiteral(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestNumbersEqualTolerance(t *testing.T) {
	if !numbersEqual(0.1+0.2, 0.3) {
		t.Error("0.1+0.2 must equal 0.3 within the relative tolerance")
	}
	if numbersEqual(1.0, 1.000001) {
		t.Error("1.0 and 1.000001 must not compare equal")
	}
	if math.Abs(0.1+0.2-0.3) > 1e-9 {
		t.Fatal("fixture assumption broken")
	}
}
