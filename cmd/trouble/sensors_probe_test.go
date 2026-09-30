package main

// Tests for `trouble sensors probe` (SPEC-03 §2 CLI verbs, §3.2 P9). The
// tests are hermetic: the probe outcome is host-dependent (an unprivileged
// run may report sampling-only), so assertions pin the RECORD CONTRACT —
// exactly one record, payload.kind = capability_probe, sig_keyed = false —
// never a specific PSI mode.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// sensorsProbeFixture isolates the resolution surface the verb reads: a
// scratch state root and a minimal config (the same shape the token verbs'
// fixtures use), so resolution succeeds and the probe reads the real host
// rather than a shared config file.
func sensorsProbeFixture(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	cfg := writeConfig(t, base, "")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
}

// TestSensorsProbeEmitsOneCapabilityProbeRecord: the probe on a synthetic,
// minimal config emits exactly one record carrying payload.kind =
// capability_probe with sig_keyed=false (SPEC-03 §3.2 P9; the record is not
// sig-keyed, the stable non-incident sig is the recorded DEVIATION in
// internal/sensors/sensors.go:300).
func TestSensorsProbeEmitsOneCapabilityProbeRecord(t *testing.T) {
	sensorsProbeFixture(t)
	stdout, stderr, code := runCaptured(t, "sensors", "probe", "--json")
	if code != 0 {
		t.Fatalf("sensors probe exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no JSON record on stdout")
	}
	var rec struct {
		Kind    string         `json:"kind"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("stdout is not one JSON record: %v\n%s", err, stdout)
	}
	if rec.Kind != "event" {
		t.Fatalf("record kind = %q, want event", rec.Kind)
	}
	if got := rec.Payload["kind"]; got != "capability_probe" {
		t.Fatalf("payload.kind = %v, want capability_probe", got)
	}
	if got, ok := rec.Payload["sig_keyed"].(bool); !ok || got {
		t.Fatalf("payload.sig_keyed = %v, want false", rec.Payload["sig_keyed"])
	}
	if _, ok := rec.Payload["psi_mode"]; !ok {
		t.Fatal("payload carries no psi_mode — the probe's whole point (P9)")
	}
	if _, ok := rec.Payload["probe_ts"]; !ok {
		t.Fatal("payload carries no probe_ts")
	}
}

// TestSensorsProbePrintsRecord: the human (non-JSON) output prints the
// record itself — the mode summary comes after it, never instead of it.
func TestSensorsProbePrintsRecord(t *testing.T) {
	sensorsProbeFixture(t)
	stdout, stderr, code := runCaptured(t, "sensors", "probe")
	if code != 0 {
		t.Fatalf("sensors probe exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "\"capability_probe\"") {
		t.Fatalf("pretty output does not print the capability_probe record:\n%s", stdout)
	}
	if !strings.Contains(stdout, "psi_mode=") {
		t.Fatalf("pretty output does not name the resulting psi_mode:\n%s", stdout)
	}
}

// TestSensorsUnknownSubcommandExits2: `trouble sensors <junk>` is a usage
// error, exit 2, usage named — same contract as the other subcommand verbs.
func TestSensorsUnknownSubcommandExits2(t *testing.T) {
	sensorsProbeFixture(t)
	_, stderr, code := runCaptured(t, "sensors", "bogus-verb")
	if code != 2 {
		t.Fatalf("sensors bogus-verb exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown sensors verb") || !strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr does not carry the usage refusal:\n%s", stderr)
	}
	// Bare `trouble sensors` is the same refusal.
	_, stderr, code = runCaptured(t, "sensors")
	if code != 2 {
		t.Fatalf("bare `sensors` exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("bare `sensors` stderr does not carry usage:\n%s", stderr)
	}
}

// TestUsageListsSensorsProbe: the top-level usage names the new verb
// (implementation requirement 4).
func TestUsageListsSensorsProbe(t *testing.T) {
	stdout, _, code := runCaptured(t, "--help")
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "trouble sensors probe") {
		t.Fatalf("--help does not list `trouble sensors probe`:\n%s", stdout)
	}
}
