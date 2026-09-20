package lifecycle

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// health_test.go pins the §3.3a rule: the status string is degraded (or
// stalled, which is the more severe state) whenever a subsystem row reports
// that it was not built. The bug this closes (TRBL-007) was a stock boot whose
// ingest plane, issue desk and skill loop were all absent while /health.json
// said status="ok" — the project's own named anti-pattern ("never observe
// itself into a green lie").

func okInputs() types.HealthInputs {
	return types.HealthInputs{
		Version: "0.1.0",
		GitSHA:  "9c1f0ab",
		Sensors: []types.SensorHealth{{Sensor: types.SenPSI, Enabled: true}},
		Subsystems: []types.SubsystemHealth{
			{Name: "sentinel", Built: true},
			{Name: "issues", Built: true},
			{Name: "research", Built: true},
			{Name: "flow", Built: true},
			{Name: "skills", Built: true},
		},
	}
}

func TestHealthAllSubsystemsBuiltIsOk(t *testing.T) {
	h := Health(*defaults(), okInputs())
	if h.Status != "ok" {
		t.Fatalf("status = %q, want ok when every subsystem row reports built", h.Status)
	}
	if len(h.Subsystems) != 5 {
		t.Fatalf("subsystems block has %d rows, want the 5 rows it was given", len(h.Subsystems))
	}
	if _, present := h.Detail["subsystem_refused"]; present {
		t.Errorf("detail carries subsystem_refused on a healthy boot: %v", h.Detail)
	}
}

// TestHealthRefusedSubsystemNeverOk is AC1's unit half: a refused subsystem
// degrades the response and names its code.
func TestHealthRefusedSubsystemNeverOk(t *testing.T) {
	in := okInputs()
	in.Subsystems[0] = types.SubsystemHealth{
		Name: "sentinel", Refused: true,
		Code:   string(types.CodeLifecycle001),
		Reason: "TROUBLE-LIFECYCLE-001: no sentinel projects configured (SPEC-12 §3.1a)",
	}
	h := Health(*defaults(), in)
	if h.Status == "ok" {
		t.Fatalf("status = ok with a refused subsystem (%+v)", h.Subsystems[0])
	}
	if h.Status != "degraded" {
		t.Errorf("status = %q, want degraded", h.Status)
	}
	if got := h.Detail["subsystem_refused"]; got != string(types.CodeLifecycle001) {
		t.Errorf("detail.subsystem_refused = %v, want the refusal code %s", got, types.CodeLifecycle001)
	}
	if got := h.Detail["subsystem"]; got != "sentinel" {
		t.Errorf("detail.subsystem = %v, want sentinel", got)
	}
	if !h.Subsystems[0].Refused || h.Subsystems[0].Built {
		t.Errorf("the refused row was rewritten on the way out: %+v", h.Subsystems[0])
	}
}

// TestHealthRefusedWithoutCodeCarriesTheReason covers a refusal recorded as a
// plain reason (no TROUBLE-*-NNN prefix): the detail still names the cause.
func TestHealthRefusedWithoutCodeCarriesTheReason(t *testing.T) {
	in := okInputs()
	in.Subsystems[1] = types.SubsystemHealth{Name: "issues", Refused: true, Reason: "driver not wired"}
	h := Health(*defaults(), in)
	if h.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", h.Status)
	}
	if got := h.Detail["subsystem_refused"]; got != "driver not wired" {
		t.Errorf("detail.subsystem_refused = %v, want the refusal reason", got)
	}
}

// TestHealthUnbuiltSubsystemIsNotBuiltAndNotOk covers the third state: a row
// that is neither built nor refused (the health surface was served before the
// subsystems landed, or a build left no reason). It must not be reported as
// built, and it must not read ok either.
func TestHealthUnbuiltSubsystemIsNotBuiltAndNotOk(t *testing.T) {
	in := okInputs()
	in.Subsystems[4] = types.SubsystemHealth{Name: "skills", Reason: "subsystems not assembled yet"}
	h := Health(*defaults(), in)
	if h.Status != "degraded" {
		t.Fatalf("status = %q, want degraded for an unbuilt subsystem row", h.Status)
	}
	if got := h.Detail["subsystem_unbuilt"]; got != "skills" {
		t.Errorf("detail.subsystem_unbuilt = %v, want skills", got)
	}
	if _, present := h.Detail["subsystem_refused"]; present {
		t.Errorf("an unbuilt row must not be reported as a refusal: %v", h.Detail)
	}
}

// TestHealthStalledBeatsSubsystemDegrade keeps the §3.3 precedence: a stalled
// writer is the more severe, more actionable state.
func TestHealthStalledBeatsSubsystemDegrade(t *testing.T) {
	in := okInputs()
	in.Stalled = true
	in.Subsystems[0] = types.SubsystemHealth{Name: "sentinel", Refused: true, Code: string(types.CodeLifecycle001)}
	h := Health(*defaults(), in)
	if h.Status != "stalled" {
		t.Fatalf("status = %q, want stalled", h.Status)
	}
	if h.Detail["subsystem_refused"] != string(types.CodeLifecycle001) {
		t.Errorf("a stalled response still carries the refusal code in detail: %v", h.Detail)
	}
}

// TestHealthRefusalDetailIsDeterministic pins which refusal the single-valued
// detail key carries when several subsystems were refused: the last refused row
// in the block's own order (the block itself carries every row).
func TestHealthRefusalDetailIsDeterministic(t *testing.T) {
	in := okInputs()
	in.Subsystems[0] = types.SubsystemHealth{Name: "sentinel", Refused: true, Code: "TROUBLE-LIFECYCLE-001"}
	in.Subsystems[3] = types.SubsystemHealth{Name: "flow", Refused: true, Code: "TROUBLE-LIFECYCLE-001"}
	first := Health(*defaults(), in).Detail["subsystem"]
	for i := 0; i < 8; i++ {
		if got := Health(*defaults(), in).Detail["subsystem"]; got != first {
			t.Fatalf("detail.subsystem flipped between runs: %v then %v", first, got)
		}
	}
	if first != "flow" {
		t.Errorf("detail.subsystem = %v, want the last refused row (flow)", first)
	}
}

// TestHealthKeepsExistingDegradedReasons guards the pre-existing detail keys the
// stall checker and the runbook read.
func TestHealthKeepsExistingDegradedReasons(t *testing.T) {
	in := okInputs()
	in.GitSHA = "unknown"
	h := Health(*defaults(), in)
	if h.Status != "degraded" {
		t.Fatalf("status = %q, want degraded for an unstamped build", h.Status)
	}
	if h.Detail["reason"] != "unstamped_build" {
		t.Errorf("detail.reason = %v, want unstamped_build", h.Detail["reason"])
	}
}

// TestHealthOkIsReachableOnlyWhenEverySubsystemIsBuilt is AC-34's exhaustive
// arm (SPEC-12 §3.3a, TRBL-019): over all 32 built-combinations of the five
// rows, exactly one — the all-built set — may read ok. Every subset carrying
// at least one built=false row must read degraded. A future Health that
// spot-checks one row instead of sweeping the block fails here, whatever the
// block order.
func TestHealthOkIsReachableOnlyWhenEverySubsystemIsBuilt(t *testing.T) {
	n := len(okInputs().Subsystems)
	for mask := 0; mask < 1<<n; mask++ {
		in := okInputs()
		built := 0
		for i := range in.Subsystems {
			in.Subsystems[i].Built = mask&(1<<i) != 0
			if in.Subsystems[i].Built {
				built++
			}
		}
		h := Health(*defaults(), in)
		if built == n {
			if h.Status != "ok" {
				t.Fatalf("mask %#b: the all-built set reads %q, want ok (the only state §3.3a permits)", mask, h.Status)
			}
			continue
		}
		if h.Status == "ok" {
			t.Fatalf("mask %#b (%d of %d rows built) reads status=ok while a row is built=false: §3.3a violation", mask, built, n)
		}
		if h.Status != "degraded" {
			t.Errorf("mask %#b reads %q, want degraded for a not-built row", mask, h.Status)
		}
	}
}

// TestHealthEverySubsystemRefusalDegradesAndNamesItself is AC-34's per-row arm:
// the sweep above proves the boolean rule, this proves the naming — whichever
// of the five rows is not built, in EITHER shape, the status degrades and the
// detail names THAT row. The existing spot tests hold sentinel-with-code and
// issues-without-code; this table holds all five names in both shapes.
func TestHealthEverySubsystemRefusalDegradesAndNamesItself(t *testing.T) {
	for i, want := range okInputs().Subsystems {
		t.Run(want.Name+"/refused", func(t *testing.T) {
			in := okInputs()
			in.Subsystems[i] = types.SubsystemHealth{
				Name: want.Name, Refused: true,
				Code:   string(types.CodeLifecycle001),
				Reason: string(types.CodeLifecycle001) + ": refused for the AC-34 table (SPEC-12 §3.3a)",
			}
			h := Health(*defaults(), in)
			if h.Status != "degraded" {
				t.Fatalf("%s refused: status = %q, want degraded", want.Name, h.Status)
			}
			if got := h.Detail["subsystem_refused"]; got != string(types.CodeLifecycle001) {
				t.Errorf("%s refused: detail.subsystem_refused = %v, want the code", want.Name, got)
			}
			if got := h.Detail["subsystem"]; got != want.Name {
				t.Errorf("%s refused: detail.subsystem = %v, want %s", want.Name, got, want.Name)
			}
		})
		t.Run(want.Name+"/unbuilt", func(t *testing.T) {
			in := okInputs()
			in.Subsystems[i] = types.SubsystemHealth{Name: want.Name}
			h := Health(*defaults(), in)
			if h.Status != "degraded" {
				t.Fatalf("%s unbuilt: status = %q, want degraded", want.Name, h.Status)
			}
			if got := h.Detail["subsystem_unbuilt"]; got != want.Name {
				t.Errorf("%s unbuilt: detail.subsystem_unbuilt = %v, want %s", want.Name, got, want.Name)
			}
		})
	}
}
