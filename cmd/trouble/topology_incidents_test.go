package main

// topology_incidents_test.go — TRBL-126: `trouble topology` is where an operator
// asks what this host is; the incident tail answers "what is broken right now"
// and names the unit, so the failing unit is visible without opening the ledger.
//
// The ledger here is a real one written by the real writer, then read back by the
// verb's read-only path — no fixture ever hands the verb the answer.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

func TestTopologyNamesTheFailingUnitOfAnOpenIncident(t *testing.T) {
	// The state-root check refuses anything under a scratch root (/tmp, /var/tmp),
	// so the fixture lives under the real home like an installed state root does.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	base, err := os.MkdirTemp(home, ".trouble-topology-test-")
	if err != nil {
		t.Fatalf("temp base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	stateRoot := filepath.Join(base, "state")
	cfg := filepath.Join(base, "config.toml")
	if err := os.WriteFile(cfg, []byte("state_root = \""+stateRoot+"\"\n"), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	const unit = "payment-worker.service"
	const subject = unit + ": unit_substate=failed via properties_changed"
	led, err := ledger.Open(context.Background(), ledger.Options{
		Root:           filepath.Join(stateRoot, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         lifecycle.Actor(types.ActorDaemon, "trouble-topology-test"),
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         "7f3a91c2d4e5b607",
		Zone:           "loopback",
		AssertScrubbed: true,
	})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if _, err := led.Append(context.Background(), types.RecordDraft{
		Kind:   types.KIncident,
		Sig:    "dbus:sha256v1:1a2b3c4d5e6f7a8b",
		Inc:    types.NewID(types.PInc),
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "dbus"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"transition": "T01", "from": "detected", "to": "recorded",
			"rung": "play", "entry_rung": "play",
			"grp":     types.NewID(types.PGrp),
			"subject": subject,
		},
	}); err != nil {
		t.Fatalf("append incident: %v", err)
	}
	if err := led.Close(context.Background()); err != nil {
		t.Fatalf("close ledger: %v", err)
	}

	stdout, stderr, code := runCaptured(t, "topology", "--config", cfg)
	if code != 0 {
		t.Fatalf("trouble topology exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "incidents open (1") {
		t.Fatalf("the topology output renders no incident tail: %s", stdout)
	}
	if !strings.Contains(stdout, subject) {
		t.Fatalf("the topology output does not name the unit and its message (%q): %s", subject, stdout)
	}

	// The machine contract is untouched: --json stays []TopologyDecision.
	jsonOut, stderr, code := runCaptured(t, "topology", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("trouble topology --json exit = %d (stderr %q)", code, stderr)
	}
	if strings.Contains(jsonOut, unit) || strings.Contains(jsonOut, "incidents open") {
		t.Fatalf("--json grew an incident shape: %s", jsonOut)
	}
	if !strings.Contains(jsonOut, "\"from\"") || !strings.Contains(jsonOut, "\"decision\"") {
		t.Fatalf("--json no longer emits TopologyDecision rows: %s", jsonOut)
	}
}
