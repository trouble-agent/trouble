// Parser file: the §3.6 generic JSON dialect — trouble's own on-ramp, one curl
// from any runtime. It is deliberately NOT a Sentry route, and it is never
// written into a DSN.
package sentinel

import (
	"encoding/json"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// genericJSONLimits are the §3.6 field caps.
const (
	genericMaxMessage  = 16 * 1024
	genericMaxExtra    = 8 * 1024
	genericMaxText     = 256
	genericMaxRelease  = 200
	genericMaxPairs    = 32
	genericMaxKeyLen   = 128
	genericMaxFingerpt = 32
)

// parseGenericEvent decodes the §3.6 generic JSON dialect. A body that is not an
// object, exceeds a limit, or carries neither `message` nor `exception` is
// 400 + TROUBLE-SENTINEL-019 with `causes` naming each offending field.
func parseGenericEvent(obj map[string]any) (*rawEvent, *Error) {
	if obj == nil {
		return nil, errf(types.CodeSentinel019, "body is not a JSON object", causeGenericJSON)
	}
	ev := &rawEvent{
		Level:      "error",
		Platform:   "generic",
		Tags:       map[string]string{},
		Extra:      map[string]any{},
		ItemTypes:  []string{"event"},
		SourceKind: sourceGeneric,
	}
	var causes []string
	if v, ok := obj["event_id"].(string); ok {
		ev.ID = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := obj["message"].(string); ok {
		if len(v) > genericMaxMessage {
			causes = append(causes, "message_too_long")
		}
		ev.Message = v
	} else if v, present := obj["message"]; present && v != nil {
		causes = append(causes, "message_not_string")
	}
	if v, ok := obj["exception"].(map[string]any); ok {
		ev.Frames, ev.ExcClass, ev.ExcValue = genericException(v, &causes)
		if ev.Message == "" {
			if s, ok := v["value"].(string); ok {
				ev.Message = s
			}
		}
	} else if v, present := obj["exception"]; present && v != nil {
		causes = append(causes, "exception_not_object")
	}
	if ev.Message == "" && ev.ExcValue == "" {
		causes = append(causes, "message_or_exception_required")
	}
	if v, ok := obj["level"].(string); ok {
		lv := strings.ToLower(strings.TrimSpace(v))
		if lv == "" {
			ev.Level = "error"
		} else if knownLevels[lv] {
			ev.Level = lv
		} else {
			ev.Level = "error"
			ev.LevelDegraded = true
		}
	}
	if v, ok := obj["culprit"].(string); ok {
		if len(v) > genericMaxText {
			causes = append(causes, "culprit_too_long")
		}
		ev.Culprit = v
	}
	if v, ok := obj["logger"].(string); ok {
		if len(v) > genericMaxText {
			causes = append(causes, "logger_too_long")
		}
		ev.Logger = v
	}
	if v, ok := obj["platform"].(string); ok && v != "" {
		if len(v) > genericMaxText {
			causes = append(causes, "platform_too_long")
		}
		ev.Platform = v
	}
	if v, ok := obj["release"].(string); ok {
		if len(v) > genericMaxRelease {
			causes = append(causes, "release_too_long")
		}
		ev.Release = v
	}
	if v, ok := obj["env"].(string); ok {
		if len(v) > genericMaxRelease {
			causes = append(causes, "env_too_long")
		}
		ev.Env = v
	}
	if v, ok := obj["timestamp"].(string); ok {
		if ts, err := types.ParseUTC(v); err == nil {
			ev.TS = types.FormatUTC(ts)
		} else {
			causes = append(causes, "timestamp_invalid")
		}
	} else if v, present := obj["timestamp"]; present && v != nil {
		causes = append(causes, "timestamp_not_string")
	}
	if fp, ok := obj["fingerprint"].([]any); ok {
		if len(fp) > genericMaxFingerpt {
			causes = append(causes, "fingerprint_too_long")
		}
		ev.Fingerprint = stringList(obj["fingerprint"])
	}
	if t, ok := obj["tags"].(map[string]any); ok {
		if len(t) > genericMaxPairs {
			causes = append(causes, "tags_too_many")
		}
		for k, val := range t {
			s, _ := val.(string)
			if len(k) > genericMaxKeyLen || len(s) > genericMaxKeyLen {
				causes = append(causes, "tags_entry_too_long")
				continue
			}
			ev.Tags[k] = s
		}
	} else if _, present := obj["tags"]; present {
		causes = append(causes, "tags_not_object")
	}
	if x, ok := obj["extra"].(map[string]any); ok {
		// extra.fingerprint is ignored: the top-level field wins (§3.6).
		delete(x, "fingerprint")
		if b, err := json.Marshal(x); err == nil && len(b) > genericMaxExtra {
			causes = append(causes, "extra_too_large")
		}
		ev.Extra = x
	} else if _, present := obj["extra"]; present {
		causes = append(causes, "extra_not_object")
	}
	if len(causes) > 0 {
		return nil, errf(types.CodeSentinel019, "generic JSON body invalid", sortedCauses(causes)...)
	}
	return ev, nil
}

// genericException decodes the §3.6 exception shape
// `{type, value, stack:[]frame}` with `frame = {file, function, line, in_app,
// context_line}`.
func genericException(ex map[string]any, causes *[]string) ([]frame, string, string) {
	class, _ := ex["type"].(string)
	value, _ := ex["value"].(string)
	if len(class) > genericMaxText || len(value) > genericMaxMessage {
		*causes = append(*causes, "exception_field_too_long")
	}
	arr, ok := ex["stack"].([]any)
	if !ok {
		return nil, class, value
	}
	out := make([]frame, 0, len(arr))
	for _, r := range arr {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		f := frame{}
		if s, ok := m["file"].(string); ok {
			f.File = s
		}
		if s, ok := m["function"].(string); ok {
			f.Function = s
		}
		if n, ok := jsonInt(m["line"]); ok {
			f.Line = int(n)
		}
		if b, ok := m["in_app"].(bool); ok {
			f.InApp = b
		}
		if s, ok := m["context_line"].(string); ok {
			f.ContextLine = s
		}
		out = append(out, f)
	}
	return out, class, value
}
