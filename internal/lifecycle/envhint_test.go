package lifecycle

// envhint_test.go — DOC-27: the near-miss hint the docs promise.
//
// deploy/README.md and docs/operations.md both promised a boot-time WARN naming
// the close match when a plaintext secret is written into a TROUBLE_* name that
// only LOOKS like a real one. Resolve already computed the near-miss and parked
// it in Resolved.UnknownEnv; nothing consumed it, so the built binary printed
// nothing and recorded nothing. These tests pin the consumer (EnvHints) and the
// record it feeds — behaviour first, then the provenance of the assertion.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveKnownEnvNameIsNotUnknown(t *testing.T) {
	// Premise for the two tests below: the correction this diagnostic suggests
	// really is a registry key, so resolving it produces no unknown-env row and
	// can never produce a WARN.
	res, err := Resolve(nil, []string{"TROUBLE_INGEST_BIND=127.0.0.1:9"}, "")
	if err != nil {
		t.Fatalf("Resolve with the real variable name: %v", err)
	}
	if len(res.UnknownEnv) != 0 {
		t.Fatalf("premise: TROUBLE_INGEST_BIND is not a known env name (%+v)", res.UnknownEnv)
	}
	if hints := res.EnvHints(); len(hints) != 0 {
		t.Fatalf("a correct configuration produced %d env-hint rows: %+v", len(hints), hints)
	}
}

func TestEnvHintsNamesTheRealVariableForANearMiss(t *testing.T) {
	res, err := Resolve(nil, []string{"TROUBLE_INGEST_BINDS=127.0.0.1:9"}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	hints := res.EnvHints()
	if len(hints) != 1 {
		t.Fatalf("EnvHints = %+v, want exactly one row for TROUBLE_INGEST_BINDS", hints)
	}
	if hints[0].Name != "TROUBLE_INGEST_BINDS" {
		t.Errorf("hint names %q, want the name the operator actually set (TROUBLE_INGEST_BINDS)", hints[0].Name)
	}
	// The correction is offered as an ENV VARIABLE name, not the dotted key:
	// the operator typed a variable and has to be able to copy the fix.
	if hints[0].Hint != "TROUBLE_INGEST_BIND" {
		t.Errorf("hint suggests %q, want TROUBLE_INGEST_BIND (the real variable, rendered through envName)", hints[0].Hint)
	}
}

func TestEnvHintsStaysSilentForAnUnrelatedStrayName(t *testing.T) {
	res, err := Resolve(nil, []string{"TROUBLE_NONSENSE=hunter2"}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	hints := res.EnvHints()
	if len(hints) != 1 {
		t.Fatalf("EnvHints = %+v, want one row: the stray name is still recorded", hints)
	}
	if hints[0].Name != "TROUBLE_NONSENSE" {
		t.Errorf("hint names %q, want TROUBLE_NONSENSE", hints[0].Name)
	}
	if hints[0].Hint != "" {
		t.Errorf("hint suggests %q for TROUBLE_NONSENSE, want no suggestion: an unrelated name is not a typo", hints[0].Hint)
	}
}

// TestConfigRecordCarriesTheHintAndNeverTheValue pins SPEC-12 §3.1's "ignored
// but recorded, with a config record carrying payload.hint" — and the part the
// spec states by safety rather than by name: the stray VALUE is a plausibly
// typed secret (that is the whole failure mode this diagnostic exists for), so
// it must not reach the ledger.
func TestConfigRecordCarriesTheHintAndNeverTheValue(t *testing.T) {
	const secret = "hunter2-not-a-real-secret"
	res, err := Resolve(nil, []string{
		"TROUBLE_INGEST_BINDS=" + secret,
		"TROUBLE_NONSENSE=" + secret,
	}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	w := &watermarkWriter{}
	if err := WriteConfigRecord(w, res); err != nil {
		t.Fatalf("WriteConfigRecord: %v", err)
	}
	drafts := w.drafts()
	if len(drafts) != 1 {
		t.Fatalf("boot wrote %d config records, want 1", len(drafts))
	}
	payload := drafts[0].Payload

	rows, ok := payload["hint"].([]EnvHint)
	if !ok {
		t.Fatalf("config record payload.hint = %#v (%T), want []EnvHint", payload["hint"], payload["hint"])
	}
	if len(rows) != 2 {
		t.Fatalf("payload.hint carries %d rows, want 2 (both stray names are recorded): %+v", len(rows), rows)
	}
	if rows[0].Name != "TROUBLE_INGEST_BINDS" || rows[0].Hint != "TROUBLE_INGEST_BIND" {
		t.Errorf("payload.hint[0] = %+v, want {TROUBLE_INGEST_BINDS TROUBLE_INGEST_BIND}", rows[0])
	}
	if rows[1].Name != "TROUBLE_NONSENSE" || rows[1].Hint != "" {
		t.Errorf("payload.hint[1] = %+v, want {TROUBLE_NONSENSE \"\"}", rows[1])
	}

	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("the boot config record carries the stray VALUE, which is exactly the plaintext secret this diagnostic exists to catch:\n%s", b)
	}
}
