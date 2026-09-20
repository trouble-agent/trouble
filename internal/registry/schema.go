package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"embed"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

// schemagenDialect is the only schema dialect a committed schema may declare
// (SPEC-06 §3.2).
const schemagenDialect = schemagen.Dialect

//go:embed schema
var committedSchemaFS embed.FS

// SchemaFileName is the committed artifact name of one module's schema.
func SchemaFileName(name string, version int) string {
	return fmt.Sprintf("%s@%d.json", name, version)
}

// WriteSchemas regenerates internal/registry/schema/*.json from the shipped
// descriptors. `make schema` runs it and `git diff --exit-code` makes a
// descriptor edit without a regenerated schema a build failure (SPEC-06 §3.2).
func WriteSchemas(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, d := range ShippedDescriptors() {
		b, err := json.Marshal(d.Schema)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, SchemaFileName(d.Name, d.Version))
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// CommittedSchemaNames lists the committed artifacts (diagnostics and tests).
func CommittedSchemaNames() []string {
	entries, err := committedSchemaFS.ReadDir("schema")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		// Only the generated artifacts are schema names; the directory README
		// keeps the embed non-empty before the first `make schema` run.
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// VerifyCommittedSchemas compares every registered module's in-memory schema with
// its committed artifact byte for byte. A mismatch (a tampered or stale schema)
// is TROUBLE-REGISTRY-014 and the daemon exits, so a stale schema never reaches a
// call (SPEC-06 §3.2).
func VerifyCommittedSchemas(names []string, desc func(string) (types.Descriptor, bool)) error {
	for _, name := range names {
		d, ok := desc(name)
		if !ok {
			continue
		}
		want, err := json.Marshal(d.Schema)
		if err != nil {
			return newErr(types.CodeRegistry014, "boot", reasonSchemaDrift, "%s: schema is not serialisable: %v", name, err)
		}
		file := SchemaFileName(d.Name, d.Version)
		got, err := committedSchemaFS.ReadFile("schema/" + file)
		if err != nil {
			return newErr(types.CodeRegistry014, "boot", reasonSchemaDrift,
				"%s: committed schema %s is missing (run `make schema`)", name, file)
		}
		got = trimTrailingNewline(got)
		if string(got) != string(want) {
			return newErr(types.CodeRegistry014, "boot", reasonSchemaDrift,
				"%s: the committed schema %s differs from the generated one (descriptor edit without `make schema`)", name, file)
		}
	}
	return nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
