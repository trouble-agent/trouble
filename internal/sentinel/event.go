package sentinel

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Event source kinds (the `payload.source_kind` vocabulary of §4.3's one
// signature space).
const (
	sourceEnvelope  = "envelope"
	sourceStore     = "store"
	sourceGeneric   = "generic_json"
	sourceCollector = "collector"
	sourceCanary    = "canary"
)

// frame is one normalized stack frame.
type frame struct {
	File        string
	Function    string
	Module      string
	Line        int
	InApp       bool
	ContextLine string
}

// rawEvent is the decoded, not-yet-persisted event: what the envelope parser,
// the store body, the generic JSON endpoint and the collector parsers all
// produce before the scrubber and the fingerprint see it.
type rawEvent struct {
	ID          string
	TS          string
	Level       string
	Logger      string
	Culprit     string
	Message     string
	Release     string
	Env         string
	Platform    string
	Fingerprint []string
	Tags        map[string]string
	Extra       map[string]any
	Frames      []frame
	ExcClass    string
	ExcValue    string

	ItemTypes    []string
	SourceKind   string
	AuthForm     string
	Project      string
	Zone         string
	ClientReport *types.ClientReport
	Partial      bool
	FlushReason  string
	Truncated    bool
	ClockSkewS   float64
	// Raw is the original JSON object the event was decoded from, kept so the
	// loss policy's spool can replay the exact bytes through the real pipeline.
	Raw json.RawMessage
	// LevelDegraded marks an unrecognized level that degraded to `error`
	// (`invalid_level_total`, §3.6): a counter, never a refusal.
	LevelDegraded   bool
	CollectorParser string
	CollectorSource string
}

// knownLevels are the §3.6 levels. An unrecognized level degrades to `error`
// and increments `invalid_level_total`.
var knownLevels = map[string]bool{"fatal": true, "error": true, "warning": true, "info": true, "debug": true}

// parseSentryEvent decodes a Sentry event object (envelope `event` item or the
// legacy /store/ body) into a rawEvent.
func parseSentryEvent(obj map[string]any) *rawEvent {
	ev := &rawEvent{
		Level:      "error",
		Platform:   "other",
		Tags:       map[string]string{},
		Extra:      map[string]any{},
		ItemTypes:  []string{"event"},
		SourceKind: sourceEnvelope,
	}
	if v, ok := obj["event_id"].(string); ok {
		ev.ID = strings.ToLower(strings.TrimSpace(v))
	}
	ev.TS = eventTimestamp(obj["timestamp"])
	if v, ok := obj["level"].(string); ok {
		ev.Level = normalizeLevel(v)
	}
	if v, ok := obj["logger"].(string); ok {
		ev.Logger = v
	}
	if v, ok := obj["culprit"].(string); ok {
		ev.Culprit = v
	}
	if v, ok := obj["release"].(string); ok {
		ev.Release = v
	}
	if v, ok := obj["environment"].(string); ok {
		ev.Env = v
	}
	if v, ok := obj["platform"].(string); ok && v != "" {
		ev.Platform = v
	}
	ev.Message = eventMessage(obj)
	ev.Fingerprint = stringList(obj["fingerprint"])
	ev.Tags = eventTags(obj["tags"])
	if x, ok := obj["extra"].(map[string]any); ok {
		ev.Extra = x
	}
	ev.Frames, ev.ExcClass, ev.ExcValue = eventFrames(obj)
	if ev.Message == "" {
		ev.Message = ev.ExcValue
	}
	return ev
}

// normalizeLevel lowercases a level and refuses anything outside §3.6's set.
func normalizeLevel(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if knownLevels[l] {
		return l
	}
	return "error"
}

// eventTimestamp accepts the SDK's epoch seconds (float) or an RFC3339 string.
func eventTimestamp(v any) string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return ""
		}
		if ts, err := types.ParseUTC(t); err == nil {
			return types.FormatUTC(ts)
		}
		return ""
	case float64:
		sec := int64(t)
		nsec := int64((t - float64(sec)) * 1e9)
		return types.FormatUTC(time.Unix(sec, nsec))
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return ""
		}
		return eventTimestamp(f)
	}
	return ""
}

// eventMessage extracts the message from the three shapes SDKs use.
func eventMessage(obj map[string]any) string {
	if m, ok := obj["message"].(string); ok && m != "" {
		return m
	}
	if m, ok := obj["message"].(map[string]any); ok {
		if f, ok := m["formatted"].(string); ok && f != "" {
			return f
		}
		if f, ok := m["message"].(string); ok && f != "" {
			return f
		}
	}
	if le, ok := obj["logentry"].(map[string]any); ok {
		if f, ok := le["formatted"].(string); ok && f != "" {
			return f
		}
		if f, ok := le["message"].(string); ok && f != "" {
			return f
		}
	}
	return ""
}

// eventFrames resolves the frames of the top (last) exception value, falling
// back to a top-level stacktrace.
func eventFrames(obj map[string]any) ([]frame, string, string) {
	if ex, ok := obj["exception"].(map[string]any); ok {
		if vals, ok := ex["values"].([]any); ok && len(vals) > 0 {
			top, _ := vals[len(vals)-1].(map[string]any)
			if top != nil {
				class, _ := top["type"].(string)
				value, _ := top["value"].(string)
				return stackFrames(top["stacktrace"]), class, value
			}
		}
		if class, ok := ex["type"].(string); ok {
			value, _ := ex["value"].(string)
			return stackFrames(ex["stacktrace"]), class, value
		}
	}
	return stackFrames(obj["stacktrace"]), "", ""
}

// stackFrames decodes {"frames":[…]}, or a bare array.
func stackFrames(v any) []frame {
	var raw []any
	switch t := v.(type) {
	case map[string]any:
		if arr, ok := t["frames"].([]any); ok {
			raw = arr
		}
	case []any:
		raw = t
	}
	out := make([]frame, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		f := frame{}
		if s, ok := m["filename"].(string); ok {
			f.File = s
		}
		if f.File == "" {
			if s, ok := m["abs_path"].(string); ok {
				f.File = s
			}
		}
		if s, ok := m["function"].(string); ok {
			f.Function = s
		}
		if s, ok := m["module"].(string); ok {
			f.Module = s
		}
		if n, ok := jsonInt(m["lineno"]); ok {
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
	return out
}

// eventTags accepts the map form and the legacy [{key,value}] array form.
func eventTags(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				out[k] = s
			} else if val != nil {
				out[k] = fmt.Sprint(val)
			}
		}
	case []any:
		for _, r := range t {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			k, _ := m["key"].(string)
			val, _ := m["value"].(string)
			if k != "" {
				out[k] = val
			}
		}
	}
	return out
}

// stringList coerces a JSON value into a list of strings, dropping non-strings
// and empties (the §3.3 fingerprint rule).
func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortedTags renders tags deterministically for stacking.
func sortedTags(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+tags[k])
	}
	return out
}

// validEventID reports whether s is 32 lowercase hex (the Sentry event_id shape).
func validEventID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// newEventID mints a 32-hex event id for an event that carries none.
func newEventID() string {
	id := types.NewID(types.PEv)
	sum := types.SigDigest([]byte(id))
	return hex.EncodeToString(sum)[:32]
}

// releaseFromHeader renders a release string for a collector-sourced event:
// collectors have no release, and §3.4 keeps such a group release-less.
func releaseFromHeader(release string) string {
	release = strings.TrimSpace(release)
	if len(release) > 200 {
		release = release[:200]
	}
	return release
}

// atoiSafe is a tiny helper for payload values.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
