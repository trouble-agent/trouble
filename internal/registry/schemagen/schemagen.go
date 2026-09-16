// Package schemagen generates the JSON Schema (draft 2020-12) of a module's args
// struct, so the schema in a Descriptor is generated and never hand-written
// (SPEC-06 §3.2). Stdlib reflect only: the allowed dependency set of
// SPECS-BRIEF §3 is untouched, and the output is byte-deterministic because
// encoding/json sorts map keys.
package schemagen

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Dialect and ID form (SPEC-06 §3.2).
const (
	Dialect = "https://json-schema.org/draft/2020-12/schema"
	IDBase  = "https://trouble.local/schema/registry/"
)

// Generate returns the draft 2020-12 schema for one module's args struct.
//
// Field rules:
//   - the name comes from the `json` tag; a field whose tag carries `omitempty`
//     is optional, every other field is required;
//   - the `js` tag adds closed-subset constraints, separated by `;` so a regex
//     may contain a comma: min/max, minlen/maxlen, pattern, enum (a|b|c),
//     default, items, minitems, maxitems and oneof=a|b (exactly one of the
//     named properties must be present).
//
// args is a struct, a pointer to a struct, or a zero struct value; anything else
// panics, because a descriptor that cannot be generated is a build failure.
func Generate(module string, version int, args any) map[string]any {
	t := reflect.TypeOf(args)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("schemagen: %s@%d: args must be a struct, got %v", module, version, reflect.TypeOf(args)))
	}
	schema := structSchema(t)
	schema["$schema"] = Dialect
	schema["$id"] = fmt.Sprintf("%s%s@%d", IDBase, module, version)
	return schema
}

func structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	required := []string{}
	oneOf := []any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name, opts := jsonName(f)
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
		props[name] = fieldSchema(f)
		if js := f.Tag.Get("js"); js != "" {
			for _, d := range strings.Split(js, ";") {
				if strings.HasPrefix(d, "oneof=") {
					for _, alt := range strings.Split(strings.TrimPrefix(d, "oneof="), "|") {
						if alt != "" {
							oneOf = append(oneOf, map[string]any{"required": []string{alt}})
						}
					}
				}
			}
		}
	}
	out := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
	}
	if len(required) > 0 {
		sortStrings(required)
		out["required"] = required
	}
	if len(oneOf) > 0 {
		out["oneOf"] = oneOf
	}
	return out
}

func fieldSchema(f reflect.StructField) map[string]any {
	js := parseJS(f.Tag.Get("js"))
	s := typeSchema(f.Type)
	for _, key := range js.order {
		switch key {
		case "min":
			applyBound(s, f.Type, "minimum", js.vals[key])
		case "max":
			applyBound(s, f.Type, "maximum", js.vals[key])
		case "minlen":
			s["minLength"] = mustInt(js.vals[key])
		case "maxlen":
			s["maxLength"] = mustInt(js.vals[key])
		case "pattern":
			s["pattern"] = js.vals[key]
		case "enum":
			vals := []any{}
			for _, v := range strings.Split(js.vals[key], "|") {
				vals = append(vals, v)
			}
			s["enum"] = vals
		case "const":
			s["const"] = js.vals[key]
		case "default":
			s["default"] = literal(js.vals[key])
		case "items":
			s["items"] = map[string]any{"type": js.vals[key]}
		case "minitems":
			s["minItems"] = mustInt(js.vals[key])
		case "maxitems":
			s["maxItems"] = mustInt(js.vals[key])
		case "oneof":
			// handled at the object level (structSchema)
		}
	}
	return s
}

func applyBound(s map[string]any, t reflect.Type, key, val string) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		k := "minLength"
		if key == "maximum" {
			k = "maxLength"
		}
		s[k] = mustInt(val)
	default:
		s[key] = literal(val)
	}
}

func typeSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object"}
	case reflect.Struct:
		return structSchema(t)
	case reflect.Interface:
		// `value` in config.set and similar free-form slots carry no type: the
		// closed dialect has no `any` keyword, so the property is untyped.
		return map[string]any{}
	}
	return map[string]any{}
}

func jsonName(f reflect.StructField) (string, string) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return "", ""
	}
	parts := strings.Split(tag, ",")
	return parts[0], strings.Join(parts[1:], ",")
}

type jsTag struct {
	order []string
	vals  map[string]string
}

func parseJS(tag string) jsTag {
	j := jsTag{vals: map[string]string{}}
	if tag == "" {
		return j
	}
	for _, d := range strings.Split(tag, ";") {
		if d == "" {
			continue
		}
		k, v, _ := strings.Cut(d, "=")
		if _, seen := j.vals[k]; !seen {
			j.order = append(j.order, k)
		}
		j.vals[k] = v
	}
	return j
}

// literal renders a tag value as the JSON value it denotes when it parses as
// JSON (number, bool, null, quoted string) and as a plain string otherwise.
func literal(s string) any {
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

func mustInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic("schemagen: bound is not an integer: " + s)
	}
	return n
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Marshal renders a schema deterministically (encoding/json sorts map keys).
func Marshal(schema map[string]any) ([]byte, error) { return json.Marshal(schema) }
