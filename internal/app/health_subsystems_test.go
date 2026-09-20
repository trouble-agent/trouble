package app

// health_subsystems_test.go is AC1 of TRBL-007, over a real boot: a stock
// configuration (no `[[projects]]`, the package default tables) must never
// report status="ok" while a subsystem was refused. Before the §3.3a rule the
// shipped default boot served status="ok" with all sensors healthy on an
// instance whose ingest plane, issue desk and skill loop were all absent — the
// project's own named anti-pattern.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/issues"
	"github.com/trouble-agent/trouble/internal/skills"
	"github.com/trouble-agent/trouble/internal/types"
)

// rowOf returns the health row for a subsystem name.
func rowOf(t *testing.T, health types.HealthResponse, name string) types.SubsystemHealth {
	t.Helper()
	for _, row := range health.Subsystems {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("health has no subsystems row for %q (block: %+v)", name, health.Subsystems)
	return types.SubsystemHealth{}
}

// allRecords reads the boot's ledger in order (the lifecycle records are the
// audit half of the same truth /health.json reports).
func allRecords(t *testing.T, h *harness) []types.Record {
	t.Helper()
	var out []types.Record
	if err := h.d.Ledger.Query().ScanFrom(1, func(r types.Record) bool {
		out = append(out, r)
		return true
	}); err != nil {
		t.Fatalf("ledger scan: %v", err)
	}
	return out
}

// TestStockBootHealthNeverReadsOKWithARefusedSubsystem is AC1 of TRBL-007, over a
// real boot: a boot with no `[[projects]]` must never report status="ok" while a
// subsystem was refused. Before the §3.3a rule the shipped default boot served
// status="ok" with all sensors healthy on an instance whose ingest plane, issue
// desk and skill loop were all absent — the project's own named anti-pattern.
//
// The two optional subsystems' refusals are CONSTRUCTED here, not inherited from
// the compiled defaults: since TRBL-016 (SPEC-09 §3.4a, SPEC-11 §2a) the shipped
// defaults are enabled=false, so a default boot BUILDS both and the only refusal
// left is the sentinel's. A desk and a loop that were ASKED for and cannot be
// built are exactly what this test must exercise, so it asks for them.
func TestStockBootHealthNeverReadsOKWithARefusedSubsystem(t *testing.T) {
	// An enabled desk over the §3.4 default driver set (no owner/repo) and an
	// enabled loop with neither source key: the operator asked for both and named
	// no credential, which is a refusal, not an accident.
	deskNoCreds := issues.DefaultConfig()
	deskNoCreds.Enabled = true
	loopNoSource := skills.DefaultConfig()
	loopNoSource.Enabled = true
	h := bootDaemonWith(t, SubsystemOptions{
		IssuesCfg: &deskNoCreds,
		SkillsCfg: &loopNoSource,
	}) // no project declared anywhere → the sentinel refuses too

	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}

	// The block is a real part of the payload, one row per subsystem.
	if len(health.Subsystems) != 5 {
		t.Fatalf("subsystems block has %d rows, want 5: %s", len(health.Subsystems), body)
	}
	if health.Status == "ok" {
		t.Fatalf("status = ok on a boot whose subsystems were refused (block: %s)", body)
	}
	if health.Status != "degraded" {
		t.Errorf("status = %q, want degraded", health.Status)
	}
	if got := health.Detail["subsystem_refused"]; got == nil {
		t.Errorf("detail carries no subsystem_refused code: %v", health.Detail)
	}

	// This boot refuses the sentinel (no project table), the issue desk (enabled,
	// and the default github driver carries no owner/repo) and the skill loop
	// (enabled, with neither source key); research and flow build on their
	// defaults.
	refused := map[string]string{
		"sentinel": string(types.CodeLifecycle001),
		"issues":   "TROUBLE-ISSUES-003",
		"skills":   "TROUBLE-SKILLS-001",
	}
	for name, wantCode := range refused {
		row := rowOf(t, health, name)
		if !row.Refused {
			t.Errorf("%s row is not refused: %+v", name, row)
		}
		if row.Built {
			t.Errorf("%s row claims built=true while refused: %+v", name, row)
		}
		if row.Code != wantCode {
			t.Errorf("%s code = %q, want %q (reason %q)", name, row.Code, wantCode, row.Reason)
		}
		if row.Reason == "" {
			t.Errorf("%s row carries no reason", name)
		}
	}
	for _, name := range []string{"research", "flow"} {
		row := rowOf(t, health, name)
		if !row.Built || row.Refused {
			t.Errorf("%s row = %+v, want built=true refused=false on the default tables", name, row)
		}
	}
	// The sentinel's refusal names the config surface that fixes it.
	if r := rowOf(t, health, "sentinel"); !strings.Contains(r.Reason, "no sentinel projects configured") {
		t.Errorf("sentinel refusal reason = %q, want it to name the missing project table", r.Reason)
	}

	// The rows and the lifecycle records are the same truth: one
	// subsystem_not_built record per refused row, no more and no fewer.
	recorded := map[string]bool{}
	for _, rec := range allRecords(t, h) {
		if rec.Kind != types.KLifecycle {
			continue
		}
		if stage, _ := rec.Payload["stage"].(string); stage != "subsystem_not_built" {
			continue
		}
		name, _ := rec.Payload["name"].(string)
		recorded[name] = true
	}
	for name := range refused {
		if !recorded[name] {
			t.Errorf("no subsystem_not_built record for %s (records: %v)", name, recorded)
		}
	}
	for name := range recorded {
		if _, ok := refused[name]; !ok {
			t.Errorf("a subsystem_not_built record exists for %s but /health.json does not report it as refused", name)
		}
	}
}

// TestStockDefaultsBuildBothOptionalSubsystems is TRBL-016 AC1's compiled-default
// half, over a real boot: with no [issues]/[skills] table anywhere
// (SubsystemOptions{} — what the shipped binary does), the issue desk and the
// skill loop are OFF but BUILT. Before route B the shipped defaults failed their
// own validators (TROUBLE-ISSUES-003 / TROUBLE-SKILLS-001), so every stock boot
// refused both of them forever and reported the boot degraded for two subsystems
// the operator never configured.
//
// The rows must be built=true refused=false with NO refusal code or reason, and
// the only refused row on this boot is the sentinel's — an absent project table
// is the operator's own statement about the ingest plane, not a defaulted
// failure.
func TestStockDefaultsBuildBothOptionalSubsystems(t *testing.T) {
	h := bootDaemon(t) // SubsystemOptions{} — the shipped compiled defaults

	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}

	for _, name := range []string{"issues", "skills"} {
		row := rowOf(t, health, name)
		if !row.Built || row.Refused {
			t.Errorf("%s row = %+v, want built=true refused=false on the compiled defaults (SPEC-09 §3.4a, SPEC-11 §2a)", name, row)
		}
		if row.Code != "" || row.Reason != "" {
			t.Errorf("%s row carries a refusal (%q / %q) on a stock boot, want the off-but-built posture", name, row.Code, row.Reason)
		}
	}
	if h.d.Subsystems == nil || h.d.Subsystems.Issues == nil || h.d.Subsystems.Skills == nil {
		t.Errorf("the compiled-default boot did not build the desk and the loop: %+v", h.d.Subsystems)
	}

	var refused []string
	for _, row := range health.Subsystems {
		if row.Refused {
			refused = append(refused, row.Name)
		}
	}
	if len(refused) != 1 || refused[0] != "sentinel" {
		t.Errorf("refused rows on a stock boot = %v, want exactly [sentinel] (no project declared)", refused)
	}
	if got := health.Detail["subsystem"]; got != "sentinel" {
		t.Errorf("detail.subsystem = %v, want sentinel: the only unnamed-config refusal left is the project table (%v)", got, health.Detail)
	}
}

// TestSubsystemsReportNeverClaimsBuilt covers the assembly-level rule the
// health surface reads: a set that was never assembled (or that holds no
// member) reports its subsystems as not built, and a set with refusals reports
// those rows verbatim.
func TestSubsystemsReportNeverClaimsBuilt(t *testing.T) {
	var nilSet *Subsystems
	rows := nilSet.Report()
	if len(rows) != len(subsystemNames) {
		t.Fatalf("nil set reported %d rows, want %d", len(rows), len(subsystemNames))
	}
	for _, row := range rows {
		if row.Built {
			t.Errorf("nil set reports %s as built: %+v", row.Name, row)
		}
		if row.Refused {
			t.Errorf("nil set reports %s as refused, but nothing refused it: %+v", row.Name, row)
		}
	}

	empty := &Subsystems{}
	empty.refusals = map[string]types.SubsystemHealth{
		"sentinel": {Name: "sentinel", Refused: true, Code: "TROUBLE-LIFECYCLE-001"},
	}
	rows = empty.Report()
	if len(rows) != len(subsystemNames) {
		t.Fatalf("report has %d rows, want %d", len(rows), len(subsystemNames))
	}
	seen := map[string]types.SubsystemHealth{}
	for _, row := range rows {
		seen[row.Name] = row
	}
	if got := seen["sentinel"]; !got.Refused || got.Built || got.Code != "TROUBLE-LIFECYCLE-001" {
		t.Errorf("sentinel row = %+v, want the recorded refusal", got)
	}
	for name, row := range seen {
		if name == "sentinel" {
			continue
		}
		if row.Built || row.Refused {
			t.Errorf("%s row = %+v, want not-built and not-refused (nothing built it, nothing refused it)", name, row)
		}
	}
}

// TestErrorCodeOf proves the code extraction the health row depends on.
func TestErrorCodeOf(t *testing.T) {
	cases := []struct {
		in   error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("TROUBLE-ISSUES-003: config_invalid: driver github needs owner and repo (SPEC-09 3.9.1)"), "TROUBLE-ISSUES-003"},
		{fmt.Errorf("TROUBLE-SKILLS-001: config_invalid: exactly one of source_path or source_url must be set"), "TROUBLE-SKILLS-001"},
		{fmt.Errorf("the scrub engine is unavailable"), ""},
		{fmt.Errorf("TROUBLE-is-prose"), ""},
	}
	for _, c := range cases {
		if got := errorCodeOf(c.in); got != c.want {
			t.Errorf("errorCodeOf(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
