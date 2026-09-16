package testkit

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/registry"
	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/registry/validate"
)

// TestSchemaDialectClosed is the dialect half of SPEC-06 §2.4, asserted from the
// testkit package as well: the exact conformance command of §2.4 runs
//
//	go test ./internal/registry/testkit/... -run '...|TestSchemaDialectClosed'
//
// so the name has to exist here too. It asserts that every generated schema
// declares the draft 2020-12 dialect and contains no keyword outside
// validate.Keywords (the closed subset of §3.2), by walking every leaf itself
// rather than trusting one call.
func TestSchemaDialectClosed(t *testing.T) {
	const dialect = "https://json-schema.org/draft/2020-12/schema"
	descs := registry.ShippedDescriptors()
	if len(descs) == 0 {
		t.Fatal("no shipped descriptors: the dialect check would be vacuous")
	}
	for _, d := range descs {
		if got, _ := d.Schema["$schema"].(string); got != dialect {
			t.Errorf("%s: $schema is %q, want %q", d.Name, got, dialect)
		}
		census := map[string]int{}
		var walk func(node map[string]any, path string)
		walk = func(node map[string]any, path string) {
			for key, value := range node {
				if key == "properties" {
					// Property names are data, not keywords: descend without
					// counting them (SPEC-06 §3.2's closed subset).
					if props, ok := value.(map[string]any); ok {
						for _, sub := range props {
							if m, ok := sub.(map[string]any); ok {
								walk(m, path+".properties")
							}
						}
					}
					continue
				}
				census[key]++
				switch sub := value.(type) {
				case map[string]any:
					walk(sub, path+"."+key)
				case []any:
					for i, item := range sub {
						if m, ok := item.(map[string]any); ok {
							walk(m, fmt.Sprintf("%s.%s[%d]", path, key, i))
						}
					}
				}
			}
		}
		walk(d.Schema, "$")
		for keyword, uses := range census {
			if !validate.Keywords[keyword] {
				t.Errorf("%s: keyword %q (%d use(s)) is outside the closed dialect subset", d.Name, keyword, uses)
			}
		}
		if len(census) == 0 {
			t.Errorf("%s: schema carries no keywords at all", d.Name)
		}
		if err := validate.DialectClosed(d.Schema); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
		raw, err := json.Marshal(d.Schema)
		if err != nil {
			t.Fatalf("%s: schema is not serialisable: %v", d.Name, err)
		}
		var round map[string]any
		if err := json.Unmarshal(raw, &round); err != nil {
			t.Fatalf("%s: schema is not a JSON object: %v", d.Name, err)
		}
		if got, _ := round["$schema"].(string); got != schemagen.Dialect {
			t.Errorf("%s: the serialized schema declares %q, want %q", d.Name, got, schemagen.Dialect)
		}
	}
}
