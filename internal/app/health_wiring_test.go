package app

// health_wiring_test.go is AC-34's composition-root arm (SPEC-12 §3.3a,
// TRBL-019): the unit sweep in internal/lifecycle proves Health degrades on
// any built=false row; this proves the wire — that the block the daemon's
// dashdeps actually reads is `Subsystems.Report()` and that a PARTIAL set
// (some members built, one nil) degrades the composed response. The
// full-package suite (health_subsystems_test.go) boots real daemons and
// covers the refused and nil-set ends; no test held the middle: a set whose
// members are live but whose block is not complete.

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/flow"
	"github.com/trouble-agent/trouble/internal/issues"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/research"
	"github.com/trouble-agent/trouble/internal/sentinel"
	"github.com/trouble-agent/trouble/internal/skills"
	"github.com/trouble-agent/trouble/internal/types"
)

// partialSet returns a set whose every member is live (no refusals recorded,
// nothing nil) — the blocks Report() hands a healthy boot.
func partialSet() *Subsystems {
	return &Subsystems{
		Sentinel: &sentinel.Server{},
		Research: &research.Service{},
		Flow:     &flow.Flow{},
		Issues:   &issues.Desk{},
		Skills:   &skills.Skills{},
	}
}

// TestReportPartialSetIsHonestRowByRow walks the five names: a complete set
// reports every row built=true refused=false; then ONE member is dropped nil
// at a time and that row — and only that row — must lose built.
func TestReportPartialSetIsHonestRowByRow(t *testing.T) {
	full := partialSet().Report()
	if len(full) != len(subsystemNames) {
		t.Fatalf("full set reported %d rows, want %d", len(full), len(subsystemNames))
	}
	for _, row := range full {
		if !row.Built || row.Refused {
			t.Errorf("full set reports %s = %+v, want built=true refused=false", row.Name, row)
		}
	}

	for _, victim := range subsystemNames {
		t.Run(victim, func(t *testing.T) {
			s := partialSet()
			switch victim {
			case "sentinel":
				s.Sentinel = nil
			case "issues":
				s.Issues = nil
			case "research":
				s.Research = nil
			case "flow":
				s.Flow = nil
			case "skills":
				s.Skills = nil
			}
			rows := s.Report()
			if len(rows) != len(subsystemNames) {
				t.Fatalf("partial set reported %d rows, want all %d (§3.3a: the block is never omitted)", len(rows), len(subsystemNames))
			}
			for _, row := range rows {
				wantBuilt := row.Name != victim
				if row.Built != wantBuilt || row.Refused {
					t.Errorf("partial set (nil %s): %s row = %+v, want built=%v refused=false", victim, row.Name, row, wantBuilt)
				}
			}
			// The names arrive in build order — §3.3a's ordering rule.
			for i, row := range rows {
				if row.Name != subsystemNames[i] {
					t.Errorf("row %d = %s, want %s (block order is the build order)", i, row.Name, subsystemNames[i])
				}
			}
		})
	}
}

// TestComposedHealthDegradesOnAPartialSubsystemSet closes the seam: the same
// call dashdeps.go makes — Report() feeding lifecycle.Health — degrades when
// one member of an otherwise-live set is missing, and reads ok only on the
// complete set. The unit tests cannot prove this: they never hold a
// Subsystems value.
func TestComposedHealthDegradesOnAPartialSubsystemSet(t *testing.T) {
	h := lifecycle.Health(lifecycle.Config{}, types.HealthInputs{
		Version:    "0.1.0",
		GitSHA:     "9c1f0ab",
		Subsystems: partialSet().Report(),
	})
	if h.Status != "ok" {
		t.Fatalf("the complete set composes status %q, want ok — the AC-34 sweep proves ok is reachable only here", h.Status)
	}

	for _, victim := range subsystemNames {
		s := partialSet()
		switch victim {
		case "sentinel":
			s.Sentinel = nil
		case "issues":
			s.Issues = nil
		case "research":
			s.Research = nil
		case "flow":
			s.Flow = nil
		case "skills":
			s.Skills = nil
		}
		in := types.HealthInputs{
			Version:    "0.1.0",
			GitSHA:     "9c1f0ab",
			Subsystems: s.Report(),
		}
		got := lifecycle.Health(lifecycle.Config{}, in)
		if got.Status != "degraded" {
			t.Fatalf("nil %s: composed status = %q, want degraded at the Report→Health seam (§3.3a)", victim, got.Status)
		}
		if got.Detail["subsystem_unbuilt"] != victim {
			t.Errorf("nil %s: detail.subsystem_unbuilt = %v, want the missing row named", victim, got.Detail["subsystem_unbuilt"])
		}
	}
}
