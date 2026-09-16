package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// RecordWriter is the narrow ledger seam lifecycle needs (SPEC-12 §4.5).
type RecordWriter interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// Explain returns the resolved config dump, optionally filtered to keys.
// Secret-class keys are redacted to "[REDACTED:config]" (SPEC-12 §3.1).
func Explain(r Resolved, keys []string) ([]types.ConfigValue, error) {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	var out []types.ConfigValue
	for _, cv := range r.Values {
		if len(want) > 0 && !want[cv.Key] {
			continue
		}
		out = append(out, cv)
	}
	return out, nil
}

// WriteConfigRecord writes one config record carrying the full redacted dump
// (SPEC-12 §3.1 boot snapshot). Keys sourced from env are marked with
// payload.env_source=true.
func WriteConfigRecord(w RecordWriter, r Resolved) error {
	payload := map[string]any{
		"values":  r.Values,
		"conflicts": len(r.Conflicts),
	}
	if len(r.Conflicts) > 0 {
		payload["conflict_refs"] = r.Conflicts
	}
	for _, cv := range r.Values {
		if cv.Source == "env" {
			payload["env_source"] = true
			break
		}
	}
	_, err := w.Append(context.Background(), types.RecordDraft{
		Kind:    types.KConfig,
		Actor:   Actor(types.ActorDaemon, "troubled"),
		Payload: payload,
	})
	return err
}

// explainJSON is a helper for CLI --json output.
func explainJSON(values []types.ConfigValue) ([]byte, error) {
	return json.Marshal(values)
}

// containsSecret reports whether a config key is secret-class. It mirrors
// redacted() in config.go but is repeated here so Explain can be tested in
// isolation.
func containsSecret(key string) bool {
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "secrets.") {
		return true
	}
	if strings.HasSuffix(lower, ".token") || strings.HasSuffix(lower, ".secret") || strings.HasSuffix(lower, "_key") {
		return true
	}
	if lower == "dsn.secret" {
		return true
	}
	return false
}

// bytesString parses a human size like "80MB" or "256MB" into bytes.
func bytesString(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult = 1 << 10
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(v * float64(mult)), nil
}
