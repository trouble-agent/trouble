package lifecycle

// envhint.go — the unknown-environment hint (SPEC-12 §3.1: "Unknown key from
// the environment ⇒ ignored but recorded, with a `config` record carrying
// `payload.hint` when the name is within edit distance 2 of a known key").
//
// Resolution already computes the near-miss: parseEnv parses a stray
// TROUBLE_* name into its dotted key and parks it in Resolved.UnknownEnv with
// the nearest registered key attached (nearestKey, threshold 2). Until this
// file nothing consumed that field — it was computed and discarded, so the
// boot log carried no hint and the config record carried no payload.hint. The
// consequence was exactly the case the docs describe: a plaintext secret
// written into a name that only LOOKS like a real one (the classic shape: a
// secret variable spelled with an extra plural or a dropped segment) — vanished
// without a diagnostic. EnvHints is that consumer.
//
// Two rules this file exists to enforce:
//
//   - The hint is rendered as an ENVIRONMENT VARIABLE name, through envName,
//     not as the dotted config key: the operator typed a variable name and has
//     to be able to copy the correction out of the log.
//   - The stray VALUE never leaves this type. The name a plaintext secret is
//     written into is exactly the shape of this diagnostic, so a row is
//     {name, hint} only — no value, and no partly-masked stand-in for one.

// EnvHint is one unknown TROUBLE_* variable observed while resolving the
// configuration. Hint is empty when the name is not within edit distance 2 of
// a registered key: an unrelated stray name is recorded but has no correction
// to offer.
type EnvHint struct {
	Name string `json:"name"`
	Hint string `json:"hint,omitempty"`
}

// EnvHints returns one row per unknown TROUBLE_* variable, in resolution
// order. The zero case returns nil so a caller can range without a check.
func (r Resolved) EnvHints() []EnvHint {
	if len(r.UnknownEnv) == 0 {
		return nil
	}
	out := make([]EnvHint, 0, len(r.UnknownEnv))
	for _, cv := range r.UnknownEnv {
		h := EnvHint{Name: cv.SourceRef}
		if h.Name == "" {
			// Defensive: parseEnv always sets SourceRef to the name it read.
			h.Name = envName(cv.Key)
		}
		if m, ok := cv.Value.(map[string]any); ok {
			if near, ok := m["hint"].(string); ok && near != "" {
				h.Hint = envName(near)
			}
		}
		out = append(out, h)
	}
	return out
}
