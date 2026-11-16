package types

// errorcodes_doc_test.go — DOC-25: the generated error-code reference must not
// drift from the declarations it is generated from.
//
// docs/reference/error-codes.md is produced by specs/tools/generate_error_codes.py
// from the `CodeXxxNNN ErrorCode = "TROUBLE-..."` declarations in this package.
// A generator without a gate is a snapshot with extra steps: a new code would
// land in the binary and in the CLI's output while the page a reader consults
// silently lacks it, and a retired code would keep advertising a refusal that
// no longer exists. This test reads the declarations with go/ast (the same
// text the compiler sees, not a regex over a hand-maintained list), reads the
// page, and fails on either direction of drift — plus a class column that
// disagrees with CodeClass, the map the runtime actually consults.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	errorCodeReferencePath = "../../docs/reference/error-codes.md"
	// codeShape is the `TROUBLE-<FAMILY>-<NNN>` grammar the catalog's numbering
	// rules are written in.
	codeShape = `TROUBLE-[A-Z][A-Z0-9]*-[0-9]{3}`
)

// sentinelNotACode is named in the reference on purpose: it is the minimum
// length the subsystem parser accepts before treating a token as a code
// (internal/app/subsystems.go:737), never an emitted refusal. It is the only
// token the page may carry without a declaration behind it.
var sentinelNotACode = []string{"TROUBLE-A-000"}

var (
	shapedCodeRE = regexp.MustCompile(`\b` + codeShape + `\b`)
	// tableRowRE matches one generated table row: | `CODE` | `class` | ...
	tableRowRE = regexp.MustCompile("^\\| `(TROUBLE-[A-Z0-9-]+)` \\| `([a-z_]+)` \\|")
	// measuredCountRE matches the page's measured-count line.
	measuredCountRE = regexp.MustCompile(`Measured code count at HEAD \(generator run on this tree\):\*\* ([0-9]+)`)
)

// declaredErrorCodes parses this package's non-test files and returns every
// constant declared with the ErrorCode type, code value → declaring file.
func declaredErrorCodes(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	declared := make(map[string]string)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "ErrorCode" {
					continue
				}
				if len(value.Values) != 1 {
					continue
				}
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				code, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", name, lit.Value, err)
				}
				declared[code] = name
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("parsed zero ErrorCode declarations — the parser's assumptions are stale")
	}
	return declared
}

func readReference(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Clean(errorCodeReferencePath))
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with `python3 specs/tools/generate_error_codes.py`)",
			errorCodeReferencePath, err)
	}
	return string(body)
}

// documentedClasses returns code → class for every generated table row.
func documentedClasses(doc string) map[string]string {
	classes := make(map[string]string)
	for _, line := range strings.Split(doc, "\n") {
		match := tableRowRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		classes[match[1]] = match[2]
	}
	return classes
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestErrorCodeReference is the DOC-25 drift gate. It fails when a declared
// code is absent from the reference, when the reference carries a code that is
// no longer declared, and when a documented class contradicts CodeClass.
func TestErrorCodeReference(t *testing.T) {
	declared := declaredErrorCodes(t)
	doc := readReference(t)
	documented := documentedClasses(doc)

	exempt := make(map[string]bool, len(sentinelNotACode))
	for _, code := range sentinelNotACode {
		exempt[code] = true
	}

	// Direction 1 — every declared code is documented. A reader who is handed a
	// code in a log line must be able to find it; this is the direction that a
	// forgotten regeneration breaks.
	missing := make([]string, 0, len(declared))
	for _, code := range sortedKeys(declared) {
		if _, ok := documented[code]; !ok {
			missing = append(missing, code+" (declared in "+declared[code]+")")
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d declared error code(s) have no entry in %s:\n  %s\n"+
			"regenerate: python3 specs/tools/generate_error_codes.py",
			len(missing), errorCodeReferencePath, strings.Join(missing, "\n  "))
	}

	// Direction 2 — every shaped code on the page is still declared. A retired
	// code left behind advertises a refusal the binary cannot produce.
	extra := make([]string, 0)
	for _, token := range shapedCodeRE.FindAllString(doc, -1) {
		if _, ok := declared[token]; ok || exempt[token] {
			continue
		}
		extra = append(extra, token)
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("%d code(s) appear in %s but are not declared in internal/types "+
			"(and are not the %s sentinel):\n  %s",
			len(extra), errorCodeReferencePath, strings.Join(sentinelNotACode, " / "),
			strings.Join(unique(extra), "\n  "))
	}

	// Direction 3 — the class column is the declaration's class. CodeClass is
	// what the daemon's retry/refusal decisions read, so a page that says
	// "transient" where the map says "permanent" sends an operator to retry a
	// condition that cannot clear.
	classDrift := make([]string, 0)
	for _, code := range sortedKeys(documented) {
		declaredClass, ok := CodeClass[ErrorCode(code)]
		if !ok {
			continue
		}
		if documented[code] != string(declaredClass) {
			classDrift = append(classDrift, code+": page says "+documented[code]+
				", CodeClass says "+string(declaredClass))
		}
	}
	if len(classDrift) > 0 {
		t.Errorf("%d class column(s) disagree with CodeClass:\n  %s",
			len(classDrift), strings.Join(classDrift, "\n  "))
	}

	// The measured count is the page's own claim about this tree; a hand edit
	// that changes the tables but not the number leaves a lie in the header.
	match := measuredCountRE.FindStringSubmatch(doc)
	if match == nil {
		t.Fatalf("%s has no 'Measured code count at HEAD' line — regenerate it",
			errorCodeReferencePath)
	}
	if match[1] != strconv.Itoa(len(declared)) {
		t.Errorf("%s claims %s codes at HEAD; internal/types declares %d",
			errorCodeReferencePath, match[1], len(declared))
	}
}

func unique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
