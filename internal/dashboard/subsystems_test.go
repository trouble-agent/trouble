package dashboard

// subsystems_test.go is AC4 of TRBL-007: the dashboard shows which subsystems a
// boot refused, with the code and the reason, so a degraded status has a visible
// cause instead of leaving an operator to infer it from a missing route. It also
// pins the §3.3a status rule on the dashboard-side aggregator, which must agree
// with lifecycle.Health.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func refusedBlock() []types.SubsystemHealth {
	return []types.SubsystemHealth{
		{Name: "sentinel", Built: true},
		{Name: "issues", Refused: true, Code: "TROUBLE-ISSUES-003",
			Reason: "TROUBLE-ISSUES-003: config_invalid: driver github needs owner and repo (SPEC-09 3.9.1)"},
		{Name: "research", Built: true},
		{Name: "flow", Built: true},
		{Name: "skills", Refused: true, Code: "TROUBLE-SKILLS-001",
			Reason: "TROUBLE-SKILLS-001: config_invalid: exactly one of source_path or source_url must be set"},
	}
}

func TestOverviewRendersRefusedSubsystems(t *testing.T) {
	env := newEnv(t, envOptions{deps: func(d *Deps) {
		d.Health = func(ctx context.Context) types.HealthResponse {
			return types.HealthResponse{
				Status: "degraded", Version: "0.1.0", GitSHA: "9c1f0ab",
				BuildTime: "2026-09-16T09:00:00.000Z", UptimeS: 3881.4,
				LedgerLastSeq: 41207, LedgerLastTS: "2026-09-16T09:14:03.221Z", LedgerStallS: 1.2,
				Autonomy:   types.DefaultGates(),
				Subsystems: refusedBlock(),
			}
		}
	}})

	resp, body := env.get("/", env.readPlain, htmlRequest)
	wantStatus(t, resp, body, http.StatusOK)

	// The refused subsystems are on the page with the code AND the reason, not
	// just a count: the operator has to be able to act on it from here.
	for _, want := range []string{
		"<h2>subsystems</h2>",
		"TROUBLE-ISSUES-003",
		"driver github needs owner and repo",
		"TROUBLE-SKILLS-001",
		"exactly one of source_path or source_url must be set",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview page does not carry %q", want)
		}
	}
	// One row per subsystem, refused rows marked, built rows not.
	for _, want := range []string{"<td>sentinel</td>", "<td>issues</td>", "<td>skills</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview page has no row cell %q", want)
		}
	}
	if n := strings.Count(body, `<tr class="refused">`); n != 2 {
		t.Errorf("refused rows = %d, want 2", n)
	}
	if n := strings.Count(body, `<tr class="built">`); n != 3 {
		t.Errorf("built rows = %d, want 3", n)
	}
	// The strip carries the count so the polled fragment says it too.
	if !strings.Contains(body, `data-refused="2"`) || !strings.Contains(body, "· refused 2") {
		t.Errorf("the health strip does not carry the refused count: %.600s", body)
	}
}

// TestHealthStripPartialCarriesRefusedCount proves the polled fragment (row 12)
// carries the same count, not just the full page.
func TestHealthStripPartialCarriesRefusedCount(t *testing.T) {
	env := newEnv(t, envOptions{deps: func(d *Deps) {
		d.Health = func(ctx context.Context) types.HealthResponse {
			return types.HealthResponse{
				Status: "degraded", LedgerLastSeq: 41207, LedgerStallS: 1.2,
				Autonomy:   types.DefaultGates(),
				Subsystems: []types.SubsystemHealth{{Name: "sentinel", Refused: true, Code: "TROUBLE-LIFECYCLE-001"}},
			}
		}
	}})
	resp, body := env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `data-refused="1"`) || !strings.Contains(body, "refused 1") {
		t.Errorf("the health fragment does not carry the refused count: %s", body)
	}
}

// TestOverviewShowsEverySubsystemBuiltWithoutRefusals guards the other
// direction: a healthy boot must not sprout refusal rows.
func TestOverviewShowsEverySubsystemBuiltWithoutRefusals(t *testing.T) {
	env := newEnv(t, envOptions{deps: func(d *Deps) {
		d.Health = func(ctx context.Context) types.HealthResponse {
			return types.HealthResponse{
				Status: "ok", Version: "0.1.0", GitSHA: "9c1f0ab", LedgerLastSeq: 41207, LedgerStallS: 1.2,
				Autonomy: types.DefaultGates(),
				Subsystems: []types.SubsystemHealth{
					{Name: "sentinel", Built: true}, {Name: "issues", Built: true},
					{Name: "research", Built: true}, {Name: "flow", Built: true}, {Name: "skills", Built: true},
				},
			}
		}
	}})
	resp, body := env.get("/", env.readPlain, htmlRequest)
	wantStatus(t, resp, body, http.StatusOK)
	if strings.Contains(body, `<tr class="refused">`) {
		t.Errorf("a healthy boot rendered a refused subsystem row: %.600s", body)
	}
	if !strings.Contains(body, `data-refused="0"`) {
		t.Errorf("the strip does not carry a zero refused count: %.600s", body)
	}
}

// TestBuildHealthDegradesOnAnUnbuiltSubsystem pins the §3.3a rule on the
// dashboard-side assembler: it must agree with lifecycle.Health, which is the
// authoritative assembly the composition root wires.
func TestBuildHealthDegradesOnAnUnbuiltSubsystem(t *testing.T) {
	base := HealthDeps{
		Clock:     func() time.Time { return time.Date(2026, 9, 16, 9, 14, 3, 0, time.UTC) },
		StartTime: time.Date(2026, 9, 16, 9, 14, 3, 0, time.UTC),
		Version:   func() (string, string, string, bool) { return "0.1.0", "9c1f0ab", "2026-09-16T09:00:00.000Z", false },
		Sensors:   func() []types.SensorHealth { return nil },
	}

	t.Run("all built is ok", func(t *testing.T) {
		d := base
		d.Subsystems = func() []types.SubsystemHealth {
			return []types.SubsystemHealth{{Name: "sentinel", Built: true}}
		}
		got, err := BuildHealth(context.Background(), d)
		if err != nil {
			t.Fatalf("BuildHealth: %v", err)
		}
		if got.Status != StatusOK {
			t.Errorf("status = %q, want ok", got.Status)
		}
	})

	t.Run("refused degrades and names the subsystem", func(t *testing.T) {
		d := base
		d.Subsystems = func() []types.SubsystemHealth {
			return refusedBlock()
		}
		got, err := BuildHealth(context.Background(), d)
		if err != nil {
			t.Fatalf("BuildHealth: %v", err)
		}
		if got.Status != StatusDegraded {
			t.Errorf("status = %q, want degraded", got.Status)
		}
		if got.Detail["subsystem"] != "issues" {
			t.Errorf("detail.subsystem = %v, want the first refused row (issues)", got.Detail["subsystem"])
		}
		if len(got.Subsystems) != 5 {
			t.Errorf("the response carries %d subsystem rows, want 5", len(got.Subsystems))
		}
	})

	t.Run("unbuilt without refusal still degrades", func(t *testing.T) {
		d := base
		d.Subsystems = func() []types.SubsystemHealth {
			return []types.SubsystemHealth{{Name: "sentinel", Built: true}, {Name: "skills"}}
		}
		got, err := BuildHealth(context.Background(), d)
		if err != nil {
			t.Fatalf("BuildHealth: %v", err)
		}
		if got.Status != StatusDegraded {
			t.Errorf("status = %q, want degraded for a row that is not built", got.Status)
		}
		if _, present := got.Detail["subsystem"]; present {
			t.Errorf("an unbuilt-and-unrefused row must not be reported as a refusal: %v", got.Detail)
		}
	})

	t.Run("no subsystem view leaves the status to the other producers", func(t *testing.T) {
		d := base
		got, err := BuildHealth(context.Background(), d)
		if err != nil {
			t.Fatalf("BuildHealth: %v", err)
		}
		if got.Status != StatusOK {
			t.Errorf("status = %q, want ok when no subsystem view was supplied", got.Status)
		}
		if got.Subsystems != nil {
			t.Errorf("no subsystem view must not invent rows: %v", got.Subsystems)
		}
	})
}
