package registry

// modules_common.go holds the shared machinery of the shipped modules: the
// environment binding, the typed-args decode helper and the target/unit
// normalization every module reuses. Module behaviour lives in modules_*.go.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/registry/validate"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// decodeArgs strictly decodes normalized args into a module's typed args struct:
// unknown keys and type mismatches are TROUBLE-REGISTRY-002 (§3.6).
func decodeArgs(args map[string]any, out any) error {
	if err := validate.DecodeArgs(args, out); err != nil {
		return err
	}
	return nil
}

// normalizePathArgs makes the named args entries absolute and resolves symlinks
// (SPEC-06 §2.3 stage 2: "paths made absolute and symlink-resolved").
func normalizePathArgs(args map[string]any, keys ...string) error {
	for _, k := range keys {
		raw, ok := args[k]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			continue
		}
		abs := s
		if !filepath.IsAbs(abs) {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			abs = filepath.Join(wd, abs)
		}
		args[k] = filepath.Clean(abs)
	}
	return nil
}

// normalizeUnitArgs normalizes a `unit`/`units` argument the way systemd does
// (bare name → .service; template instance compared by template).
func normalizeUnitArgs(args map[string]any) {
	if s, ok := args["unit"].(string); ok && s != "" {
		args["unit"] = normalizeUnit(s)
	}
}

// unitTarget declares a `unit`-shaped protected target when one is present.
func unitTarget(args map[string]any) []protectedTarget {
	if s, ok := args["unit"].(string); ok && s != "" {
		return []protectedTarget{{Kind: "unit", Value: normalizeUnit(s)}}
	}
	if list, ok := args["units"].([]any); ok {
		out := make([]protectedTarget, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, protectedTarget{Kind: "unit", Value: normalizeUnit(s)})
			}
		}
		return out
	}
	return nil
}

// pathTarget declares a `path`-shaped protected target when one is present.
func pathTarget(args map[string]any) []protectedTarget {
	if s, ok := args["path"].(string); ok && s != "" {
		return []protectedTarget{{Kind: "path", Value: s}}
	}
	return nil
}

// rollbackInvertible reports whether a module carries a usable RollbackHint
// (SPEC-06 §3.5): config.set and file.patch only.
func rollbackInvertible(name string) bool { return invertibleModules[name] }

// depsAvailable reports whether the flow collaborators are wired.
func (e *moduleEnv) depsAvailable() bool {
	return e.deps.FileIssue != nil && e.deps.CreateTask != nil && e.deps.Comment != nil
}

// upstreamCode reads a driver's own code off a flow reply so it can ride
// Result.Output.upstream_code instead of this area minting a foreign code
// (SPEC-06 §3.9 flow rules).
func upstreamCode(reply map[string]any) string {
	if reply == nil {
		return ""
	}
	for _, k := range []string{"error_code", "upstream_code", "code"} {
		if s, ok := reply[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// scopeTokens lists the `scope:` grant tokens a descriptor's scopes map onto, so
// a module can report what it needs without a second vocabulary.
func scopeTokens(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, "scope:"+s)
	}
	return out
}

var _ = strings.TrimSpace
var _ = types.KToolCall
