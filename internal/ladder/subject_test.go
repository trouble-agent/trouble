package ladder

// subject_test.go — TRBL-126: an incident carries its human-readable subject
// (the unit/scope identifier plus the originating message), so `trouble
// topology` and the dashboard incident row name the failing unit without an
// operator opening the ledger.
//
// The admission vectors drive the ladder's own Admit (the production path the
// daemon's sensors→ladder bridge calls), and the composition vectors pin the
// fallbacks for signals that are not unit-scoped.

import (
	"context"
	"strings"
	"testing"
)

// TestIncidentSubjectText pins the composition and every fallback. The two
// detail shapes are both covered on purpose: a sensor event nests its rule
// variables under payload.detail, while an in-process event may carry them flat,
// and the subject must read the same fact from either.
func TestIncidentSubjectText(t *testing.T) {
	cases := []struct {
		name       string
		identifier string
		detail     map[string]any
		want       string
	}{
		{
			name:       "journald unit: the unit and its log line",
			identifier: "payment-worker.service",
			detail: map[string]any{
				"detail": map[string]any{"msg": "open /srv/payment/queue.db failed: exit 1"},
			},
			want: "payment-worker.service: open /srv/payment/queue.db failed: exit 1",
		},
		{
			name:       "dbus unit: the unit and the signal content",
			identifier: "payment-worker.service",
			detail: map[string]any{
				"detail": map[string]any{
					"unit_substate": "failed",
					"arrival_path":  "properties_changed",
					"msg":           "",
				},
			},
			want: "payment-worker.service: unit_substate=failed via properties_changed",
		},
		{
			name:       "dbus unit, reconcile arrival, no message text",
			identifier: "batch-worker@3.service",
			detail:     map[string]any{"unit_substate": "failed"},
			want:       "batch-worker@3.service: unit_substate=failed",
		},
		{
			name:       "unit only: no message is invented",
			identifier: "quiet-worker.service",
			detail:     map[string]any{},
			want:       "quiet-worker.service",
		},
		{
			name:       "message only: a non-unit-scoped signal keeps the text",
			identifier: "",
			detail:     map[string]any{"detail": map[string]any{"msg": "some 12.4 avg10"}},
			want:       "some 12.4 avg10",
		},
		{
			name:       "neither: empty, so every surface falls back to its own title",
			identifier: "  ",
			detail:     nil,
			want:       "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := incidentSubjectText(c.identifier, c.detail); got != c.want {
				t.Fatalf("incidentSubjectText(%q, %v) = %q, want %q", c.identifier, c.detail, got, c.want)
			}
		})
	}
}

// TestAdmissionRecordsTheUnitAndMessage is the acceptance test for TRBL-126: a
// unit-failure signal admitted through the ladder names the unit AND its
// originating message on the incident record. Dropping the payload key or the
// projection field makes this fail.
func TestAdmissionRecordsTheUnitAndMessage(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	obs := obsFor(sigFor("trbl-126-unit-failure"), "dbus_unit_failed", "dbus")
	obs.Subject = "payment-worker.service"
	obs.Detail = map[string]any{
		"detail": map[string]any{
			"unit_key":      "payment-worker.service",
			"unit_substate": "failed",
			"arrival_path":  "properties_changed",
			"msg":           "",
		},
	}
	res := h.admit(t, obs)
	if res.Inc == "" {
		t.Fatal("the unit-failure signal opened no incident")
	}

	const want = "payment-worker.service: unit_substate=failed via properties_changed"

	// The record the ledger keeps: the T01 admission record names the subject.
	rec := incidentRecord(t, h, res.Inc, "T01")
	got, _ := rec.Payload["subject"].(string)
	if got != want {
		t.Fatalf("T01 payload subject = %q, want %q (the admission record must name the unit and its message)", got, want)
	}

	// The projection every reader sees (dashboard, outlets, CLI).
	inc, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident(%s): %v", res.Inc, err)
	}
	if inc.Subject != want {
		t.Fatalf("incident projection subject = %q, want %q", inc.Subject, want)
	}
	if inc.Sig == "" {
		t.Fatal("the incident lost its raw signature id")
	}
	if !strings.Contains(inc.Subject, "payment-worker.service") {
		t.Fatalf("subject %q does not name the unit", inc.Subject)
	}
}

// TestAdmissionSubjectNamesAJournaldUnitMessage is the same acceptance on the
// other unit-scoped source: journald's masked log line IS the originating
// message, and it must ride the incident next to the unit.
func TestAdmissionSubjectNamesAJournaldUnitMessage(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	obs := obsFor(sigFor("trbl-126-journald-unit"), "journald_crash_loop", "journald")
	obs.Subject = "indexer.service"
	obs.Detail = map[string]any{
		"detail": map[string]any{"msg": "panic: runtime error: invalid memory address"},
	}
	res := h.admit(t, obs)
	rec := incidentRecord(t, h, res.Inc, "T01")
	got, _ := rec.Payload["subject"].(string)
	const want = "indexer.service: panic: runtime error: invalid memory address"
	if got != want {
		t.Fatalf("T01 payload subject = %q, want %q", got, want)
	}
}

// TestAdmissionSubjectFallsBackForNonUnitSignals: a resource-class signal has no
// unit, so its subject is the message it does carry — never a fabricated unit
// name and never the empty string when a message exists.
func TestAdmissionSubjectFallsBackForNonUnitSignals(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	obs := obsFor(sigFor("trbl-126-psi"), "psi_cpu_some_avg10_high", "psi")
	obs.Subject = "cpu"
	obs.Detail = map[string]any{"detail": map[string]any{"msg": "some avg10=41.02"}}
	res := h.admit(t, obs)
	rec := incidentRecord(t, h, res.Inc, "T01")
	got, _ := rec.Payload["subject"].(string)
	const want = "cpu: some avg10=41.02"
	if got != want {
		t.Fatalf("T01 payload subject = %q, want %q", got, want)
	}

	// A signal with no subject and no message records none: the surfaces fall
	// back to the group title / sig rather than a placeholder that reads like a
	// fact.
	h2 := newHarness(t, harnessOpts{})
	bare := obsFor(sigFor("trbl-126-bare"), "rule-a", "sentinel")
	bare.Subject = ""
	bare.Detail = map[string]any{}
	res2 := h2.admit(t, bare)
	rec2 := incidentRecord(t, h2, res2.Inc, "T01")
	if got, _ := rec2.Payload["subject"].(string); got != "" {
		t.Fatalf("a subject-less signal recorded subject %q, want empty", got)
	}
}
