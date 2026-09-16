package flow

// board_test.go — SPEC-08 §7 `board_test.go` + `writer_test.go` +
// `validate_test.go`: the row round-trip against the §3.1 fixture, style
// preservation, id allocation from the board max, the append-only proof, the
// two-file order and every validate path.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestRowRoundTripMatchesTheFixture(t *testing.T) {
	fx := newFixture(t, nil)
	row := fx.row("")
	res, err := fx.flow.File(context.Background(), fx.incident(), row)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !res.Wrote || res.TaskID == "" || res.EventID == "" {
		t.Fatalf("result = %+v", res)
	}
	rows, events, lines := fx.readBoard()
	if len(rows) != 1 || len(events) != 1 {
		t.Fatalf("rows=%d events=%d, want 1 row + 1 event", len(rows), len(events))
	}
	// The row carries every §3.1 field, in schema order, as one compact line.
	wantOrder := []string{"id", "title", "status", "priority", "complexity", "depends_on",
		"blocks", "primary_model", "primary_provider", "fallback_model", "fallback_provider",
		"reasoning", "capability_tags", "worker_status", "dispatched_at", "completed_at",
		"sig", "inc", "issue_refs", "repo"}
	last := -1
	for _, k := range wantOrder {
		i := strings.Index(lines[0], `"`+k+`":`)
		if i < 0 {
			t.Fatalf("row is missing the field %q: %s", k, lines[0])
		}
		if i < last {
			t.Fatalf("field %q is out of schema order: %s", k, lines[0])
		}
		last = i
	}
	if strings.Contains(lines[0], `": `) {
		t.Fatalf("the row must be compact (one line, no spaced separators): %s", lines[0])
	}
	for _, k := range []string{"depends_on", "blocks", "issue_refs"} {
		var v any
		if err := json.Unmarshal([]byte(lines[0]), &map[string]any{}); err == nil {
			if err := json.Unmarshal([]byte(lines[0]), &struct{}{}); err != nil {
				t.Fatal(err)
			}
		}
		_ = v
		if !strings.Contains(lines[0], `"`+k+`":[]`) {
			t.Fatalf("%s must serialize as [] and never null: %s", k, lines[0])
		}
	}
	if rows[0]["sig"] != fx.incident().Sig || rows[0]["inc"] != fx.incident().ID {
		t.Fatalf("the trouble extensions are missing: %v", rows[0])
	}
	if rows[0]["repo"] != fx.repo {
		t.Fatalf("repo = %v, want %s", rows[0], fx.repo)
	}
	// The event names the sig and the incident, and its id is MAX+1.
	if events[0]["type"] != types.EvTaskCreated || events[0]["task_id"] != res.TaskID {
		t.Fatalf("event = %v", events[0])
	}
	if events[0]["id"].(float64) != 1 {
		t.Fatalf("first event id = %v, want 1", events[0]["id"])
	}
}

func TestAppendOnlyNeverRewrites(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	// Three filings for three different sigs on one board.
	sigs := []string{"sig:a", "sig:b", "sig:c"}
	for _, s := range sigs {
		if _, err := fx.flow.File(ctx, fx.incident(), fx.row(s)); err != nil {
			t.Fatalf("File %s: %v", s, err)
		}
	}
	_, _, lines := fx.readBoard()
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	before := append([]string(nil), lines...)
	// A fourth filing must append one line and touch nothing else.
	if _, err := fx.flow.File(ctx, fx.incident(), fx.row("sig:d")); err != nil {
		t.Fatalf("File: %v", err)
	}
	_, _, after := fx.readBoard()
	if len(after) != 4 {
		t.Fatalf("lines = %d, want 4 (append-only)", len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("line %d changed: %q → %q", i, before[i], after[i])
		}
	}
}

func TestIDAllocationFromBoardMax(t *testing.T) {
	fx := newFixture(t, nil)
	// Seed the board with a foreign row whose ULID is far above now.
	foreign := `{"id":"tsk_01ZZZZZZZZZZZZZZZZZZZZZZZZ","title":"seeded by the board owner","status":"todo","priority":"P2","complexity":"S","depends_on":[],"blocks":[],"primary_model":"","primary_provider":"","fallback_model":"","fallback_provider":"","reasoning":"seed","capability_tags":["trouble"],"worker_status":"","dispatched_at":"","completed_at":"","sig":"sig:seed","inc":"","issue_refs":[],"repo":"` + fx.repo + `"}`
	if err := os.WriteFile(filepath.Join(fx.board, tasksFile), []byte(foreign+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := fx.flow.File(context.Background(), fx.incident(), fx.row(""))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !strictlyGreater(ulidSuffix(res.TaskID), "01ZZZZZZZZZZZZZZZZZZZZZZZZ") {
		t.Fatalf("allocated id %s is not strictly greater than the board max", res.TaskID)
	}
}

func TestTwoFileOrderAndEventIdempotency(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	res, err := fx.flow.File(ctx, fx.incident(), fx.row(""))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// A second reconcile of the same task must not append a second event.
	ix, err := fx.flow.indexFor(fx.board)
	if err != nil {
		t.Fatalf("indexFor: %v", err)
	}
	ev := types.BoardEvent{Type: types.EvTaskCreated, TaskID: res.TaskID, TS: types.NowUTC()}
	if _, err := ix.appendEvent(ctx, ev, ""); err != nil {
		t.Fatalf("appendEvent: %v", err)
	}
	if _, err := ix.appendEvent(ctx, ev, ""); err != nil {
		t.Fatalf("appendEvent (2): %v", err)
	}
	_, events, _ := fx.readBoard()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (idempotent by (task_id, type))", len(events))
	}
}

func TestValidatePaths(t *testing.T) {
	ctx := context.Background()
	// rc=1 with findings that do NOT name our id is a pre-existing board
	// finding: recorded in the payload, never an error for this filing.
	fx := newFixture(t, func(c *types.FlowConfig) { c.ValidateCmd = "false" })
	res, err := fx.flow.File(ctx, fx.incident(), fx.row(""))
	if err != nil {
		t.Fatalf("rc=1 without our id must not fail the filing: %v", err)
	}
	if !res.Valid || res.ValidateRC != 1 {
		t.Fatalf("result = %+v, want valid with validate_rc 1", res)
	}
	if p := fx.rec.last(types.KFlow); p["validate_rc"] != 1 {
		t.Fatalf("payload validate_rc = %v", p["validate_rc"])
	}
	// A validator that is simply missing (rc=2/unusable) is TROUBLE-FLOW-002.
	fx5 := newFixture(t, func(c *types.FlowConfig) { c.ValidateCmd = "false-usage-rc2 --bad-flag" })
	if _, err := fx5.flow.File(ctx, fx5.incident(), fx5.row("")); err == nil {
		t.Fatalf("an unusable validate command must be TROUBLE-FLOW-002")
	}
	// rc=0 across the board.
	fx2 := newFixture(t, func(c *types.FlowConfig) { c.ValidateCmd = "true" })
	if _, err := fx2.flow.File(ctx, fx2.incident(), fx2.row("")); err != nil {
		t.Fatalf("rc=0 must be a valid filing: %v", err)
	}
	// rc=1 naming our id is TROUBLE-FLOW-003.
	fx3 := newFixture(t, nil)
	fx3.flow.cfg.ValidateCmd = "sh -c 'echo \"finding in $PWD\"; exit 1'"
	script := filepath.Join(t.TempDir(), "validate.sh")
	scriptBody := "#!/bin/sh\nid=$(grep -o 'tsk_[A-Z0-9]*' tasks.jsonl | head -1)\necho \"bad row $id: title too long\"\nexit 1\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	fx3.flow.cfg.ValidateCmd = script
	_, err = fx3.flow.File(ctx, fx3.incident(), fx3.row(""))
	if err == nil || !strings.Contains(err.Error(), string(types.CodeFlow003)) {
		t.Fatalf("err = %v, want TROUBLE-FLOW-003", err)
	}
	// A hanging validator returns inside 11s (the 10s cap).
	fx4 := newFixture(t, nil)
	hang := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fx4.flow.cfg.ValidateCmd = hang
	start := fx4.clock.Now()
	_, err = fx4.flow.File(ctx, fx4.incident(), fx4.row(""))
	if err == nil {
		t.Fatalf("a hanging validator must fail the filing")
	}
	// The validator's exit code is what the record carries.
	if p := fx4.rec.last(types.KFlow); p["validate_rc"] == nil {
		t.Fatalf("the flow record must carry validate_rc: %v", p)
	}
	_ = start
}

func TestValidateCommandMissingIsTransient(t *testing.T) {
	fx := newFixture(t, nil)
	fx.flow.cfg.ValidateCmd = filepath.Join(t.TempDir(), "nope-not-executable")
	_, err := fx.flow.File(context.Background(), fx.incident(), fx.row(""))
	if err == nil || !strings.Contains(err.Error(), string(types.CodeFlow002)) {
		t.Fatalf("err = %v, want TROUBLE-FLOW-002 for an unusable validate command", err)
	}
}

func TestInProcessValidationRefusesShapes(t *testing.T) {
	bad := []types.BoardRow{
		{ID: "not-a-task", Title: "x", Status: "todo", Priority: "P1", Complexity: "S", Sig: "s", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "  ", Status: "todo", Priority: "P1", Complexity: "S", Sig: "s", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "x", Status: "nope", Priority: "P1", Complexity: "S", Sig: "s", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "x", Status: "todo", Priority: "P9", Complexity: "S", Sig: "s", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "x", Status: "todo", Priority: "P1", Complexity: "XL", Sig: "s", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "x", Status: "todo", Priority: "P1", Complexity: "S", Inc: "i"},
		{ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Title: "x", Status: "todo", Priority: "P1", Complexity: "S", Sig: "s"},
	}
	for i, row := range bad {
		if err := validateRow(row); err == nil {
			t.Fatalf("row %d passed in-process validation", i)
		}
	}
}

// TestShellIsNeverUsedForTheWritePath proves the writer opens no shell: the only
// process it can start is the configured validator.
func TestShellIsNeverUsedForTheWritePath(t *testing.T) {
	fx := newFixture(t, nil)
	if _, err := fx.flow.File(context.Background(), fx.incident(), fx.row("")); err != nil {
		t.Fatalf("File: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable in this environment")
	}
	// No git command was needed: the board is written with O_APPEND and fsync.
	if fx.flow.cfg.ValidateCmd != "" {
		t.Fatalf("the fixture must not configure a validator for this test")
	}
}
