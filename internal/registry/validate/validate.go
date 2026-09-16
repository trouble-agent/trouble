// Package validate implements the closed JSON Schema keyword subset the
// generator can emit (SPEC-06 §3.2): a committed schema may use no keyword
// outside this set, so the subset can never silently ignore a constraint, and
// the registry's validate stage is exactly this package.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Keywords is the closed subset (SPEC-06 §3.2), plus the two metadata keys the
// dialect declaration requires.
var Keywords = map[string]bool{
	"type": true, "required": true, "properties": true, "additionalProperties": true,
	"enum": true, "const": true, "minimum": true, "maximum": true,
	"minLength": true, "maxLength": true, "pattern": true,
	"items": true, "minItems": true, "maxItems": true, "oneOf": true, "default": true,
	"$schema": true, "$id": true,
}

// Error is a refusal carrying the registry's validate-stage code.
type Error struct {
	Code   types.ErrorCode
	Path   string
	Reason string
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s at %s", e.Code, e.Reason, e.Path)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Reason)
}

func errf(path, format string, args ...any) *Error {
	return &Error{Code: types.CodeRegistry002, Path: path, Reason: fmt.Sprintf(format, args...)}
}

// DialectClosed walks a schema and refuses any keyword outside the subset.
func DialectClosed(schema map[string]any) error {
	return dialectWalk(schema, "$")
}

func dialectWalk(node map[string]any, path string) error {
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !Keywords[k] {
			return &Error{Code: types.CodeRegistry002, Path: path + "." + k,
				Reason: fmt.Sprintf("keyword %q is outside the closed dialect subset", k)}
		}
		switch k {
		case "properties":
			props, ok := node[k].(map[string]any)
			if !ok {
				return errf(path+".properties", "properties must be an object")
			}
			for name, sub := range props {
				if m, ok := sub.(map[string]any); ok {
					if err := dialectWalk(m, path+".properties."+name); err != nil {
						return err
					}
				}
			}
		case "items":
			if m, ok := node[k].(map[string]any); ok {
				if err := dialectWalk(m, path+".items"); err != nil {
					return err
				}
			}
		case "oneOf":
			list, ok := node[k].([]any)
			if !ok {
				return errf(path+".oneOf", "oneOf must be an array")
			}
			for i, sub := range list {
				if m, ok := sub.(map[string]any); ok {
					if err := dialectWalk(m, fmt.Sprintf("%s.oneOf[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Args validates raw args against a generated schema. Every failure is a
// TROUBLE-REGISTRY-002 schema violation.
func Args(schema map[string]any, args map[string]any) error {
	return validateObject(schema, args, "$")
}

func validateObject(schema map[string]any, args map[string]any, path string) error {
	// oneOf first: it constrains presence, before the properties are walked.
	if raw, ok := schema["oneOf"].([]any); ok && len(raw) > 0 {
		matched := 0
		for i, sub := range raw {
			m, ok := sub.(map[string]any)
			if !ok {
				return errf(path+".oneOf", "alternative %d is not a schema object", i)
			}
			if validateObject(m, args, path) == nil {
				matched++
			}
		}
		if matched != 1 {
			return errf(path, "exactly one of the oneOf alternatives must be satisfied (matched %d)", matched)
		}
	}
	if len(schema) == 0 {
		return nil
	}
	if t, ok := schema["type"].(string); ok {
		if t != "object" {
			return errf(path, "schema type %q cannot describe an args object", t)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for _, name := range requiredOf(schema) {
		if _, ok := args[name]; !ok {
			return errf(path, "required property %q is missing", name)
		}
	}
	additional, hasAdditional := schema["additionalProperties"]
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		val := args[name]
		sub, known := props[name]
		if !known {
			if hasAdditional {
				if b, ok := additional.(bool); ok && !b {
					return errf(path, "unknown property %q (additionalProperties is false)", name)
				}
			}
			continue
		}
		m, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		if err := validateValue(m, val, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func validateValue(schema map[string]any, val any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if want, ok := schema["type"].(string); ok {
		switch want {
		case "string":
			if _, ok := val.(string); !ok {
				return errf(path, "expected string, got %s", jsonKind(val))
			}
		case "boolean":
			if _, ok := val.(bool); !ok {
				return errf(path, "expected boolean, got %s", jsonKind(val))
			}
		case "integer":
			n, ok := val.(json.Number)
			if !ok {
				return errf(path, "expected integer, got %s", jsonKind(val))
			}
			if _, err := strconv.ParseInt(n.String(), 10, 64); err != nil {
				return errf(path, "expected an integer literal, got %s", n.String())
			}
		case "number":
			if _, ok := val.(json.Number); !ok {
				return errf(path, "expected number, got %s", jsonKind(val))
			}
		case "array":
			list, ok := val.([]any)
			if !ok {
				return errf(path, "expected array, got %s", jsonKind(val))
			}
			if err := boundsInt(schema, "minItems", len(list), path, "at least"); err != nil {
				return err
			}
			if err := boundsInt(schema, "maxItems", len(list), path, "at most"); err != nil {
				return err
			}
			if itemSchema, ok := schema["items"].(map[string]any); ok {
				for i, item := range list {
					if err := validateValue(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
			return nil
		case "object":
			obj, ok := val.(map[string]any)
			if !ok {
				return errf(path, "expected object, got %s", jsonKind(val))
			}
			return validateObject(schema, obj, path)
		}
	}
	if s, ok := val.(string); ok {
		if err := boundsInt(schema, "minLength", utf8.RuneCountInString(s), path, "at least"); err != nil {
			return err
		}
		if err := boundsInt(schema, "maxLength", utf8.RuneCountInString(s), path, "at most"); err != nil {
			return err
		}
		if pat, ok := schema["pattern"].(string); ok && pat != "" {
			re, err := regexp.Compile(pat)
			if err != nil {
				return errf(path, "schema pattern %q does not compile: %v", pat, err)
			}
			if !re.MatchString(s) {
				return errf(path, "value %q does not match %s", s, pat)
			}
		}
	}
	if n, ok := val.(json.Number); ok {
		f, err := n.Float64()
		if err != nil {
			return errf(path, "value %s is not a number", n.String())
		}
		if min, ok := schema["minimum"]; ok {
			mf, err := toFloat(min)
			if err == nil && f < mf {
				return errf(path, "value %s is below the minimum %v", n.String(), min)
			}
		}
		if max, ok := schema["maximum"]; ok {
			mf, err := toFloat(max)
			if err == nil && f > mf {
				return errf(path, "value %s is above the maximum %v", n.String(), max)
			}
		}
	}
	if raw, ok := schema["enum"].([]any); ok && len(raw) > 0 {
		found := false
		for _, e := range raw {
			if sameJSON(e, val) {
				found = true
				break
			}
		}
		if !found {
			return errf(path, "value %s is not one of the enum members", describe(val))
		}
	}
	if c, ok := schema["const"]; ok && !sameJSON(c, val) {
		return errf(path, "value %s is not the const %v", describe(val), c)
	}
	return nil
}

func boundsInt(schema map[string]any, key string, got int, path, word string) error {
	raw, ok := schema[key]
	if !ok {
		return nil
	}
	want, err := toFloat(raw)
	if err != nil {
		return errf(path, "%s is not a number", key)
	}
	if (key == "minLength" || key == "minItems") && float64(got) < want {
		return errf(path, "length %d is below the %s %v", got, word, raw)
	}
	if (key == "maxLength" || key == "maxItems") && float64(got) > want {
		return errf(path, "length %d is above the %v %v", got, word, raw)
	}
	return nil
}

func requiredOf(schema map[string]any) []string {
	raw, ok := schema["required"].([]any)
	if ok {
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out
	}
	// A schema decoded from committed JSON carries []any; one built by the
	// generator carries []string.
	if list, ok := schema["required"].([]string); ok {
		return list
	}
	return nil
}

func toFloat(v any) (float64, error) {
	switch t := v.(type) {
	case json.Number:
		return t.Float64()
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func describe(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 64 {
		b = append(b[:61], '.', '.', '.')
	}
	return string(b)
}

// Normalize round-trips raw args through encoding/json with UseNumber so every
// number is a JSON literal: an integer is an integer, a float is refused by the
// schema, and a value that cannot be represented in JSON is a decode failure
// (TROUBLE-REGISTRY-018).
func Normalize(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, &Error{Code: types.CodeRegistry018, Reason: fmt.Sprintf("args are not JSON-representable: %v", err)}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, &Error{Code: types.CodeRegistry018, Reason: fmt.Sprintf("args failed JSON decoding: %v", err)}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// DecodeArgs strictly decodes normalized args into a module's typed args struct:
// unknown keys are refused (the schema already did, this is the second gate) and
// a type mismatch is a schema violation.
func DecodeArgs(args map[string]any, out any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return &Error{Code: types.CodeRegistry018, Reason: err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown field") {
			return errf("$", "unknown property: %s", strings.TrimPrefix(msg, "json: "))
		}
		return errf("$", "args do not decode into the module's typed args: %s", msg)
	}
	return nil
}
