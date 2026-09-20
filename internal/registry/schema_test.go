package registry

// schema_test.go is the §7 row for the generated schema contract (SPEC-06 §3.2,
// §3.11, AC-23): generator determinism, the dialect and the closed keyword
// subset, boot-time regen equality against the committed artifacts, and the two
// "no shell" scans — TestNoExecInRegistry (a go/parser import scan, never a text
// grep) and TestNoArbitraryCommandModule.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/registry/validate"
	"github.com/trouble-agent/trouble/internal/types"
)

// shippedModuleCount is the v0.1 regression number: thirteen registered modules
// (SPEC-06 §3.8, §7).
const shippedModuleCount = 13

// TestSchemaDialectClosed asserts every generated schema declares
// "https://json-schema.org/draft/2020-12/schema" and contains no keyword outside
// the §3.2 subset (SPEC-06 §2.4).
func TestSchemaDialectClosed(t *testing.T) {
	descs := ShippedDescriptors()
	if len(descs) != shippedModuleCount {
		t.Fatalf("the v0.1 module set is %d modules, got %d", shippedModuleCount, len(descs))
	}
	for _, d := range descs {
		if got, _ := d.Schema["$schema"].(string); got != schemagen.Dialect {
			t.Errorf("%s: $schema is %q, want %q", d.Name, got, schemagen.Dialect)
		}
		want := fmt.Sprintf("%s%s@%d", schemagen.IDBase, d.Name, d.Version)
		if got, _ := d.Schema["$id"].(string); got != want {
			t.Errorf("%s: $id is %q, want %q", d.Name, got, want)
		}
		census := map[string]int{}
		walkSchemaKeywords(d.Schema, func(keyword string) { census[keyword]++ })
		if len(census) == 0 {
			t.Errorf("%s: schema carries no keywords at all", d.Name)
		}
		for keyword, uses := range census {
			if !validate.Keywords[keyword] {
				t.Errorf("%s: keyword %q (%d use(s)) is outside the closed dialect subset", d.Name, keyword, uses)
			}
		}
		if err := validate.DialectClosed(d.Schema); err != nil {
			t.Errorf("%s: the closed-subset walk refused the schema: %v", d.Name, err)
		}
		committed, err := committedSchema(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		if got, _ := committed["$schema"].(string); got != schemagen.Dialect {
			t.Errorf("%s: the committed schema declares %q, want %q", d.Name, got, schemagen.Dialect)
		}
	}
}

// TestSchemaGeneratorDeterminism runs the generator 100 times and asserts a
// byte-identical result (SPEC-06 §3.2: "a byte-deterministic output (verified by a
// 100-iteration golden test)"), and asserts the same for every shipped schema.
func TestSchemaGeneratorDeterminism(t *testing.T) {
	const iterations = 100

	type probeArgs struct {
		Zeta  int      `json:"zeta,omitempty" js:"min=1;max=9;default=3"`
		Alpha string   `json:"alpha" js:"pattern=^[a-z]+$"`
		List  []string `json:"list,omitempty" js:"items=string;minitems=0;maxitems=4"`
		Free  any      `json:"free,omitempty"`
	}
	var golden []byte
	for i := 0; i < iterations; i++ {
		got, err := schemagen.Marshal(schemagen.Generate("determinism.probe", 1, probeArgs{}))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i == 0 {
			golden = got
			continue
		}
		if !bytes.Equal(golden, got) {
			t.Fatalf("iteration %d of the generator is not byte-identical to iteration 0:\n%s\n%s", i, golden, got)
		}
	}
	if len(golden) == 0 {
		t.Fatal("the generator emitted nothing")
	}
	for _, d := range ShippedDescriptors() {
		first, err := schemagen.Marshal(d.Schema)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		for i := 0; i < iterations; i++ {
			got, err := json.Marshal(d.Schema)
			if err != nil {
				t.Fatalf("%s iteration %d: %v", d.Name, i, err)
			}
			if !bytes.Equal(first, got) {
				t.Fatalf("%s: marshalling iteration %d differs from iteration 0", d.Name, i)
			}
		}
	}
}

// TestSchemaCommittedRegenEquality is the boot-time regen equality of SPEC-06
// §3.2: the regenerated schema of every shipped module is byte-for-byte the
// committed artifact, so a descriptor edit without `make schema` fails the build
// (and the daemon would exit with TROUBLE-REGISTRY-014).
func TestSchemaCommittedRegenEquality(t *testing.T) {
	byName := map[string]types.Descriptor{}
	names := make([]string, 0, shippedModuleCount)
	for _, d := range ShippedDescriptors() {
		byName[d.Name] = d
		names = append(names, d.Name)
	}
	sort.Strings(names)
	if len(names) != shippedModuleCount {
		t.Fatalf("the v0.1 module set is %d modules, got %d", shippedModuleCount, len(names))
	}
	err := VerifyCommittedSchemas(names, func(name string) (types.Descriptor, bool) {
		d, ok := byName[name]
		return d, ok
	})
	if err != nil {
		t.Fatalf("a shipped descriptor differs from its committed schema (run `make schema`): %v", err)
	}

	want := make([]string, 0, len(names))
	for _, name := range names {
		want = append(want, SchemaFileName(name, byName[name].Version))
	}
	sort.Strings(want)
	got := CommittedSchemaNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("committed schema artifacts %v do not match the shipped modules %v", got, want)
	}
}

// TestNoExecInRegistry is the §3.11 "no shell" gate: internal/registry/**
// contains zero imports of os/exec and zero calls to syscall.Exec,
// syscall.ForkExec or os.StartProcess. The scan works on the parsed AST (imports
// and selector calls), never on source text, so a mention in a string or a
// comment cannot satisfy or defeat it.
func TestNoExecInRegistry(t *testing.T) {
	const root = "."
	fset := token.NewFileSet()
	var violations []string
	files := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		packages := map[string]string{}
		for _, imp := range file.Imports {
			pkgPath, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			name := pkgPath
			if i := strings.LastIndexByte(pkgPath, '/'); i >= 0 {
				name = pkgPath[i+1:]
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			packages[name] = pkgPath
			if pkgPath == "os/exec" {
				violations = append(violations, path+": imports os/exec")
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case packages[ident.Name] == "os" && sel.Sel.Name == "StartProcess":
				violations = append(violations, fmt.Sprintf("%s:%d: calls os.StartProcess", path, fset.Position(call.Pos()).Line))
			case packages[ident.Name] == "syscall" && (sel.Sel.Name == "Exec" || sel.Sel.Name == "ForkExec"):
				violations = append(violations, fmt.Sprintf("%s:%d: calls syscall.%s", path, fset.Position(call.Pos()).Line, sel.Sel.Name))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning internal/registry: %v", err)
	}
	// The walk must really cover the tree: the registry package, schemagen,
	// validate and testkit all contain Go files.
	if files < 10 {
		t.Fatalf("the import scan parsed only %d Go files under internal/registry: it did not walk the tree", files)
	}
	if len(violations) > 0 {
		t.Errorf("internal/registry has no shell: %s", strings.Join(violations, "; "))
	}
}

// TestNoArbitraryCommandModule is the second §3.11 gate: no registered module's
// schema declares a command, cmd, argv, shell or script args key (a schema test
// over every generated descriptor and its committed artifact).
func TestNoArbitraryCommandModule(t *testing.T) {
	banned := []string{"command", "cmd", "argv", "shell", "script"}
	descs := ShippedDescriptors()
	if len(descs) != shippedModuleCount {
		t.Fatalf("the v0.1 module set is %d modules, got %d", shippedModuleCount, len(descs))
	}
	for _, d := range descs {
		for _, key := range banned {
			if _, ok := propertyNames(d.Schema)[key]; ok {
				t.Errorf("%s: the generated schema accepts a %q args key; no module may accept an arbitrary command", d.Name, key)
			}
		}
		committed, err := committedSchema(d)
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		for _, key := range banned {
			if _, ok := propertyNames(committed)[key]; ok {
				t.Errorf("%s: the committed schema %s accepts a %q args key", d.Name, SchemaFileName(d.Name, d.Version), key)
			}
		}
		// A banned key could also hide under oneOf/items: scan every property
		// name anywhere in the committed document.
		for _, name := range allPropertyNames(committed) {
			for _, key := range banned {
				if name == key {
					t.Errorf("%s: the committed schema declares a %q property; no module may accept an arbitrary command", d.Name, key)
				}
			}
		}
	}
}

// allPropertyNames collects every property name anywhere in a schema document
// (top level, under oneOf alternatives and under nested items).
func allPropertyNames(schema map[string]any) []string {
	seen := map[string]bool{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		for key, value := range node {
			switch sub := value.(type) {
			case map[string]any:
				if key == "properties" {
					for name := range sub {
						seen[name] = true
					}
				}
				walk(sub)
			case []any:
				for _, item := range sub {
					if m, ok := item.(map[string]any); ok {
						walk(m)
					}
				}
			}
		}
	}
	walk(schema)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// committedSchema decodes one committed artifact.
func committedSchema(d types.Descriptor) (map[string]any, error) {
	raw, err := committedSchemaFS.ReadFile("schema/" + SchemaFileName(d.Name, d.Version))
	if err != nil {
		return nil, fmt.Errorf("committed schema %s is missing (run `make schema`): %w", SchemaFileName(d.Name, d.Version), err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("committed schema %s is not a JSON object: %w", SchemaFileName(d.Name, d.Version), err)
	}
	return out, nil
}

// propertyNames lists the property names of a schema (an empty set when the
// schema declares none).
func propertyNames(schema map[string]any) map[string]bool {
	props, _ := schema["properties"].(map[string]any)
	out := make(map[string]bool, len(props))
	for name := range props {
		out[name] = true
	}
	return out
}

// walkSchemaKeywords visits every keyword of a schema, recursively (properties,
// items and oneOf).
// walkSchemaKeywords visits every KEYWORD in a schema. Property names are data,
// not keywords, so the walk descends into `properties` values without visiting
// their keys (SPEC-06 §3.2's closed subset).
func walkSchemaKeywords(node map[string]any, visit func(keyword string)) {
	for key, value := range node {
		if key == "properties" {
			if props, ok := value.(map[string]any); ok {
				for _, sub := range props {
					if m, ok := sub.(map[string]any); ok {
						walkSchemaKeywords(m, visit)
					}
				}
			}
			continue
		}
		visit(key)
		switch sub := value.(type) {
		case map[string]any:
			walkSchemaKeywords(sub, visit)
		case []any:
			for _, item := range sub {
				if m, ok := item.(map[string]any); ok {
					walkSchemaKeywords(m, visit)
				}
			}
		}
	}
}
