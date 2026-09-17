package ladder

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dedup_test.go is the SPEC-05 §7 row for AC-22 (§3.9 dedup and the merge rule):
// admission is an idempotent upsert keyed by (sig, open incident) with a second
// key (inKey) for cross-plane correlation. One incident, one group, one issue and
// one board row per digest at any time (INV-1); a recurrence comments instead of
// duplicating; the same arrival replayed yields one record and one counter
// increment; a mismatched open incident is TROUBLE-LADDER-020, not a silent
// repair.
//
// Nothing here opens a socket, touches git or a real clock: every collaborator is
// the in-process fake from testutil_test.go and time moves only through
// fakeClock.

// arrivals mints distinct event ids for a test. Admission's fold idempotency is
// keyed on the arriving `ev_` id (§3.9), so a test that wants N distinct arrivals
// must hand N distinct ids: types.NewID repeats its value for every call inside
// one millisecond (its increment lands in the tail of the randomness that
// encodeULID does not read — reported to the owning workstream), and a repeated
// id would turn each later arrival into a replay instead of a fold.
//
// Each observation also carries its own unit subject, so two arrivals with
// different sigs stay two incidents unless a test says otherwise: §3.9 derives the
// inKey from (source_class, subject, taxonomy, app_kind), and two sigs agreeing on
// all four share one incident by design (asserted in
// TestCrossPlaneInKeyIsClassScoped).
type arrivals struct{ n int }

func (a *arrivals) obs(sig types.Sig, rule string, src SourcePath) Observation {
	a.n++
	o := obsFor(sig, rule, src)
	o.EventID = fmt.Sprintf("ev_dedup_%04d", a.n)
	unit := fmt.Sprintf("unit-%d.service", a.n)
	o.Subject = unit
	o.Detail = map[string]any{"unit": unit}
	return o
}

// ---- small readers ---- //

// arrivalPathCounts extracts the arrival_paths counters of a payload (the field
// is keyed by SourcePath, §3.9).
func arrivalPathCounts(p map[string]any) map[string]int {
	out := map[string]int{}
	switch v := p["arrival_paths"].(type) {
	case map[string]int:
		for k, n := range v {
			out[k] = n
		}
	case map[string]any:
		for k, raw := range v {
			switch n := raw.(type) {
			case int:
				out[k] = n
			case int64:
				out[k] = int(n)
			}
		}
	}
	return out
}

// sumArrivals is the number of arrivals folded into an incident.
func sumArrivals(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// latestArrivals returns the arrival_paths counters of the newest incident
// record (every incident record carries the incident's live map).
func latestArrivals(t *testing.T, h *harness) map[string]int {
	t.Helper()
	for i := len(h.ledger.records) - 1; i >= 0; i-- {
		r := h.ledger.records[i]
		if r.Kind != types.KIncident {
			continue
		}
		if _, ok := r.Payload["arrival_paths"]; ok {
			return arrivalPathCounts(r.Payload)
		}
	}
	t.Fatalf("no incident record carries arrival_paths")
	return nil
}

// admissionRecord returns the T01 record of a sig (nil when absent).
func admissionRecord(h *harness, sig string) map[string]any {
	for _, r := range h.ledger.records {
		if r.Kind != types.KIncident || r.Sig != sig {
			continue
		}
		if r.Payload["transition"] == "T01" {
			return r.Payload
		}
	}
	return nil
}

// recordWithTransition returns the newest incident record of an incident that
// carries the given transition id (nil when absent).
func recordWithTransition(h *harness, inc, edge string) map[string]any {
	var found map[string]any
	for _, r := range h.ledger.records {
		if r.Kind != types.KIncident || r.Inc != inc {
			continue
		}
		if r.Payload["transition"] == edge {
			found = r.Payload
		}
	}
	return found
}

// ---- AC-22: three arrival paths, one digest ---- //

// TestDedupThreeArrivalPathsFoldIntoOneIncident pins §3.9's D-Bus merge rule: the
// three arrival paths that carry one unit fault (journald follow,
// PropertiesChanged(SubState), JobRemoved) share one sig and therefore fold into
// one incident — never a second incident, issue or board row.
//
// arrival_paths is keyed by SourcePath (§2 lists the eight values), so the two
// D-Bus signals share the `dbus` counter and their identity rides in
// Observation.Detail; the second sub-case asserts the literal "three
// arrival_paths counters" reading with three distinct SourcePath values.
func TestDedupThreeArrivalPathsFoldIntoOneIncident(t *testing.T) {
	opts := harnessOpts{rules: map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay},
	}}
	var a arrivals

	t.Run("three D-Bus arrival paths", func(t *testing.T) {
		h := newHarness(t, opts)
		sig := sigFor("dedup-paths")
		follow := a.obs(sig, "rule-a", "journald")
		follow.Subject = "payment-worker.service"
		follow.Detail = map[string]any{"unit": "payment-worker.service", "path": "journald_follow"}
		props := a.obs(sig, "rule-a", "dbus")
		props.Subject = "payment-worker.service"
		props.Detail = map[string]any{"unit": "payment-worker.service", "path": "PropertiesChanged(SubState)"}
		job := a.obs(sig, "rule-a", "dbus")
		job.Subject = "payment-worker.service"
		job.Detail = map[string]any{"unit": "payment-worker.service", "path": "JobRemoved"}

		first := h.admit(t, follow)
		if !first.Created || first.Inc == "" {
			t.Fatalf("first arrival: %+v, want a new incident", first)
		}
		for _, o := range []Observation{props, job} {
			got := h.admit(t, o)
			if got.Inc != first.Inc || !got.Folded {
				t.Fatalf("arrival over %q: %+v, want a fold into %s", o.Source, got, first.Inc)
			}
		}

		counts := latestArrivals(t, h)
		if n := sumArrivals(counts); n != 3 {
			t.Errorf("arrival_paths %v sum to %d, want 3 arrivals", counts, n)
		}
		if counts["journald"] != 1 || counts["dbus"] != 2 {
			t.Errorf("arrival_paths = %v, want journald:1 and dbus:2 (both D-Bus signals travel the dbus path)", counts)
		}
		// One incident: exactly one admission record and one live incident.
		if n := h.ledger.countPayload("transition", "T01"); n != 1 {
			t.Errorf("%d T01 records, want 1 (one incident for the digest)", n)
		}
		if n := len(h.l.incs); n != 1 {
			t.Errorf("%d live incidents, want 1", n)
		}
		issues, boards, comments := h.outlets.counts()
		if issues != 0 || boards != 0 {
			t.Errorf("folds filed %d issue(s) and %d board row(s), want 0 extra of each", issues, boards)
		}
		// Cross-ref, never duplicate: the first recurrence comments (§3.9).
		if comments != 1 {
			t.Errorf("%d comment(s), want 1 (the first recurrence folds a comment)", comments)
		}
	})

	t.Run("three distinct source paths", func(t *testing.T) {
		h := newHarness(t, opts)
		sig := sigFor("dedup-paths-three")
		first := h.admit(t, a.obs(sig, "rule-a", "journald"))
		if !first.Created {
			t.Fatalf("first arrival: %+v", first)
		}
		for _, src := range []SourcePath{"dbus", "inotify"} {
			if got := h.admit(t, a.obs(sig, "rule-a", src)); got.Inc != first.Inc || !got.Folded {
				t.Fatalf("arrival over %q: %+v, want a fold into %s", src, got, first.Inc)
			}
		}
		counts := latestArrivals(t, h)
		if len(counts) != 3 {
			t.Fatalf("arrival_paths = %v, want 3 counters (journald, dbus, inotify)", counts)
		}
		for path, n := range counts {
			if n != 1 {
				t.Errorf("arrival_paths[%q] = %d, want 1", path, n)
			}
		}
		if n := h.ledger.countPayload("transition", "T01"); n != 1 {
			t.Errorf("%d T01 records, want 1", n)
		}
		issues, boards, _ := h.outlets.counts()
		if issues != 0 || boards != 0 {
			t.Errorf("folds filed %d issue(s) and %d board row(s), want 0 extra of each", issues, boards)
		}
	})
}

// ---- AC-22: the cross-plane (secondary key) merge ---- //

// TestDedupCrossPlaneMergesIntoSingleIncident pins §3.9's secondary key: two sigs
// with equal inKey share ONE incident, which keeps every sig and renders as one
// row keyed by IncidentID — one issue and one board row for three reporting
// planes, and one comment on the first recurrence.
//
// The correlation key is handed in through Observation.InKey (the field SPEC-04
// fills): §3.9's derived formula folds source_class into the hash, so a
// `unit`-class arrival (journald) and an `app`-class arrival (sentinel,
// collector) can never derive the same key — see
// TestCrossPlaneInKeyIsClassScoped and the run report.
func TestDedupCrossPlaneMergesIntoSingleIncident(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{CorrelationUnitAlias: map[string]string{"proj-payments": "payment-worker.service"}},
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	var a arrivals

	unitSig := sigFor("crossplane-unit")
	appSig := sigFor("crossplane-app")
	collectorSig := sigFor("crossplane-collector")

	unit := a.obs(unitSig, "rule-a", "journald")
	unit.Subject = "payment-worker.service"
	shared := h.l.InKeyFor(unit)
	if shared == "" {
		t.Fatal("the unit-class arrival derived an empty inKey")
	}
	unit.InKey = shared

	// sentinel(app): the project id aliases to the unit through
	// correlation.unit_alias, so the app-class half of the correlation is the
	// same unit as the journald one.
	app := a.obs(appSig, "rule-a", "sentinel")
	app.Subject = "proj-payments"
	app.InKey = shared

	// collector(app): the same project, reported from a second agent.
	collector := a.obs(collectorSig, "rule-a", "collector")
	collector.Subject = "proj-payments"
	collector.InKey = shared

	if got := h.l.subjectFor(unit); got != "payment-worker.service" {
		t.Errorf("unit-class subject = %q, want the unescaped unit name", got)
	}
	if got := h.l.subjectFor(app); got != "unit:payment-worker.service" {
		t.Errorf("app-class subject = %q, want unit:payment-worker.service (correlation.unit_alias)", got)
	}

	first := h.admit(t, unit)
	if !first.Created || first.Inc == "" {
		t.Fatalf("unit arrival: %+v, want a new incident", first)
	}
	for _, o := range []Observation{app, collector} {
		got := h.admit(t, o)
		if got.Inc != first.Inc || !got.Folded {
			t.Fatalf("arrival from %q: %+v, want a merge into %s", o.Source, got, first.Inc)
		}
		if got.Reason != "merged" {
			t.Errorf("arrival from %q: reason %q, want merged", o.Source, got.Reason)
		}
	}

	// One incident carrying all three sigs, with the first as the primary.
	st := h.l.incStateFor(first.Inc)
	if st == nil {
		t.Fatalf("incident %s is not in the index", first.Inc)
	}
	if len(st.Sigs) != 3 {
		t.Fatalf("sigs = %v, want 3", st.Sigs)
	}
	for _, want := range []string{unitSig.String(), appSig.String(), collectorSig.String()} {
		if !contains(st.Sigs, want) {
			t.Errorf("sigs = %v, want %s among them", st.Sigs, want)
		}
	}
	if st.Inc.Sig != unitSig.String() {
		t.Errorf("primary sig = %q, want %q", st.Inc.Sig, unitSig.String())
	}
	if st.Inc.GroupID == "" {
		t.Error("the merged incident has no group id: AC-22's one group is rendered as one row keyed by IncidentID")
	}
	merge := recordWithTransition(h, first.Inc, "merge")
	if merge == nil {
		t.Fatal("no merge record was appended")
	}
	if got, _ := merge["inKey"].(string); got != shared {
		t.Errorf("merge record inKey = %q, want %q", got, shared)
	}
	if sigs, _ := merge["sigs"].([]string); len(sigs) != 3 {
		t.Errorf("merge record sigs = %v, want all 3 sigs in one row", sigs)
	}
	if n := h.ledger.countPayload("transition", "T01"); n != 1 {
		t.Errorf("%d T01 records, want 1 (the merges are folds, not new incidents)", n)
	}
	if n := len(h.l.incs); n != 1 {
		t.Errorf("%d live incidents, want 1", n)
	}

	// The first recurrence arrives for the primary sig: it folds and comments.
	issuesBefore, boardsBefore, commentsBefore := h.outlets.counts()
	recurrence := h.admit(t, a.obs(unitSig, "rule-a", "journald"))
	if recurrence.Inc != first.Inc || !recurrence.Folded {
		t.Fatalf("recurrence: %+v, want a fold into %s", recurrence, first.Inc)
	}
	issuesAfter, boardsAfter, commentsAfter := h.outlets.counts()
	if issuesAfter != issuesBefore || boardsAfter != boardsBefore {
		t.Errorf("the recurrence filed issue/board row %d/%d, want %d/%d (cross-ref, never duplicate)",
			issuesAfter, boardsAfter, issuesBefore, boardsBefore)
	}
	if commentsAfter != commentsBefore+1 {
		t.Errorf("comments %d → %d, want exactly one added on the first recurrence", commentsBefore, commentsAfter)
	}

	// Drive the incident over the play rung to its ceiling: one issue and one
	// board row for the three reporting planes, still one comment.
	h.l.incStateFor(first.Inc).RunSummary = &RunSummary{FailClass: "permanent", TasksFailed: 1}
	for _, tr := range []Transition{{Trigger: "T03"}, {Trigger: "T08"}, {Trigger: "T11"}, {Trigger: "T18"}} {
		if _, err := h.l.Advance(context.Background(), first.Inc, tr); err != nil {
			t.Fatalf("%s: %v", tr.Trigger, err)
		}
	}
	issues, boards, comments := h.outlets.counts()
	if issues != 1 {
		t.Errorf("%d issue(s), want 1 for the merged incident", issues)
	}
	if boards != 1 {
		t.Errorf("%d board row(s), want 1 for the merged incident", boards)
	}
	if comments != 1 {
		t.Errorf("%d comment(s), want 1", comments)
	}
	got, err := h.l.Incident(context.Background(), first.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StEscalated {
		t.Errorf("state %q, want escalated", string(got.State))
	}
}

// TestCrossPlaneInKeyIsClassScoped documents §3.9's derivation boundary: inKey =
// ink_ + hex(sha256(source_class + US + subject + US + taxonomy + US +
// app_kind))[:16] folds source_class into the hash, so a `unit`-class arrival and
// an `app`-class arrival derive different keys however the project id is aliased
// to the unit. Within one class the derivation does correlate two reporting
// planes (sentinel and collector of the same project), which is why SPEC-05 §7's
// cross-plane vector is only reachable when the producer hands the shared key
// over in Observation.InKey. Recorded in the run report.
func TestCrossPlaneInKeyIsClassScoped(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{CorrelationUnitAlias: map[string]string{"proj-payments": "payment-worker.service"}},
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	var a arrivals

	unit := a.obs(sigFor("ink-unit"), "rule-a", "journald")
	unit.Subject = "payment-worker.service"
	app := a.obs(sigFor("ink-app"), "rule-a", "sentinel")
	app.Subject = "proj-payments"
	collector := a.obs(sigFor("ink-collector"), "rule-a", "collector")
	collector.Subject = "proj-payments"

	if h.l.inKeyFor(unit) == h.l.inKeyFor(app) {
		t.Errorf("a unit-class and an app-class arrival derived the same inKey (%q): the source_class arm of §3.9's formula is not being applied",
			h.l.inKeyFor(unit))
	}
	if h.l.inKeyFor(app) != h.l.inKeyFor(collector) {
		t.Errorf("two app-class arrivals for the same aliased project derived %q and %q, want the same key",
			h.l.inKeyFor(app), h.l.inKeyFor(collector))
	}

	// The positive control: the derived key does merge within a class — two
	// app-class sigs that agree on (source_class, subject, taxonomy, app_kind)
	// share one incident, which is §3.9's "two sigs with equal inKey share one
	// incident" expressed through the derivation alone.
	app.Subject = "proj-payments"
	collector.Subject = "proj-payments"
	first := h.admit(t, app)
	if !first.Created || first.Inc == "" {
		t.Fatalf("current-sentinel arrival: %+v, want a new incident", first)
	}
	merged := h.admit(t, collector)
	if merged.Inc != first.Inc || !merged.Folded || merged.Reason != "merged" {
		t.Errorf("current-collector arrival: %+v, want a merge into %s", merged, first.Inc)
	}
	if n := h.ledger.countPayload("transition", "T01"); n != 1 {
		t.Errorf("%d T01 records, want 1: equal derived inKeys share one incident", n)
	}
}

// ---- AC-22: idempotency of one arrival ---- //

// TestDedupReplayedArrivalIsIdempotentOnEventID pins §3.9's fold idempotency: the
// idempotency key inside the fold is the arriving ev_ event id, so replaying the
// same arrival yields one record and one counter increment.
func TestDedupReplayedArrivalIsIdempotentOnEventID(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay},
	}})

	o := obsFor(sigFor("dedup-replay"), "rule-a", "journald")
	o.EventID = "ev_replayed_0001"
	first := h.admit(t, o)
	if !first.Created || first.Inc == "" {
		t.Fatalf("first arrival: %+v", first)
	}
	recordsAfterFirst := len(h.ledger.records)

	for i := 0; i < 100; i++ {
		got := h.admit(t, o) // the same EventID
		if got.Inc != first.Inc || !got.Folded {
			t.Fatalf("replay %d: %+v, want a fold into %s", i+1, got, first.Inc)
		}
	}
	if n := len(h.ledger.records) - recordsAfterFirst; n != 0 {
		t.Errorf("100 replays appended %d record(s), want 0", n)
	}
	if n := sumArrivals(latestArrivals(t, h)); n != 1 {
		t.Errorf("arrival_paths total = %d, want 1 (one counter increment for the one arrival)", n)
	}
	if n := h.ledger.countPayload("folded", true); n != 0 {
		t.Errorf("%d fold record(s) were written for a replayed arrival, want 0", n)
	}
	if n := h.ledger.countPayload("transition", "T01"); n != 1 {
		t.Errorf("%d T01 records, want 1", n)
	}
	issues, boards, comments := h.outlets.counts()
	if issues != 0 || boards != 0 || comments != 0 {
		t.Errorf("a replayed arrival filed %d issue(s), %d board row(s) and %d comment(s), want 0 of each", issues, boards, comments)
	}
	got, err := h.l.Incident(context.Background(), first.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StRecorded {
		t.Errorf("state %q, want recorded", string(got.State))
	}
}

// ---- AC-22: volume ---- //

// TestDedupVolumeTenThousandAdmissionsOfOneSig pins §7's volume row: 10,000
// admissions of ONE sig produce 1 incident and no duplicate incident records, at
// ≥ 2,000 admissions/s. Admission is serialized under the ladder's mutex, so the
// loop is single-goroutine (the AC counts arrivals, not concurrency).
func TestDedupVolumeTenThousandAdmissionsOfOneSig(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay},
	}})
	sig := sigFor("dedup-volume")
	const n = 10000

	ids := map[string]bool{}
	start := time.Now()
	for i := 0; i < n; i++ {
		o := obsFor(sig, "rule-a", "journald")
		o.EventID = fmt.Sprintf("ev_volume_%05d", i)
		res := h.admit(t, o)
		if res.Inc == "" {
			t.Fatalf("admission %d created no incident", i+1)
		}
		ids[res.Inc] = true
	}
	elapsed := time.Since(start)
	rate := float64(n) / elapsed.Seconds()
	t.Logf("AC-22 volume: %d admissions in %s = %.0f admissions/s (spec floor 2000/s)", n, elapsed.Round(time.Microsecond), rate)

	if len(ids) != 1 {
		t.Errorf("%d distinct incident ids, want 1", len(ids))
	}
	if len(h.l.incs) != 1 {
		t.Errorf("%d live incidents, want 1", len(h.l.incs))
	}
	if got := h.ledger.countPayload("transition", "T01"); got != 1 {
		t.Errorf("%d admission records (T01), want 1: no duplicate incident was created", got)
	}
	if got := h.ledger.countPayload("from", "detected"); got != 1 {
		t.Errorf("%d records carry from=detected, want 1", got)
	}
	if got := sumArrivals(latestArrivals(t, h)); got != n {
		t.Errorf("arrival_paths total = %d, want %d folded arrivals", got, n)
	}
	issues, boards, comments := h.outlets.counts()
	if issues != 0 || boards != 0 {
		t.Errorf("%d issue(s) and %d board row(s), want 0 of each for one digest", issues, boards)
	}
	// Cross-ref cadence (§3.9): the first recurrence plus every 10th fold.
	if want := 1 + (n-1)/10; comments != want {
		t.Errorf("%d comment(s), want %d (first recurrence + every 10th fold)", comments, want)
	}
	if rate < 2000 {
		t.Errorf("admission throughput %.0f/s is below the spec floor of 2000/s", rate)
	}
	if elapsed > 5*time.Second {
		t.Errorf("10,000 admissions took %s, want well under 5s", elapsed)
	}
}

// ---- AC-22: reopen and the reopen conflict ---- //

// TestDedupReopenKeepsTheIncidentAndComments pins §3.9's reopen arm: a recurrence
// after `resolved` reopens the SAME incident (T42), increments reopen_count once,
// comments on the existing issue and creates no second issue or board row.
func TestDedupReopenKeepsTheIncidentAndComments(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay},
	}})
	const name = "dedup-reopen"
	inc, _ := resolved(t, h, name)

	issuesBefore, boardsBefore, commentsBefore := h.outlets.counts()
	recurrence := obsFor(sigFor(name), "rule-a", "journald")
	recurrence.EventID = "ev_reopen_recurrence_0001"
	again := h.admit(t, recurrence)
	if !again.Reopened || again.Created {
		t.Fatalf("recurrence: %+v, want a reopen of %s", again, inc)
	}
	if again.Inc != inc {
		t.Fatalf("recurrence re-entered incident %s, want the same incident %s", again.Inc, inc)
	}
	got, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StRecorded {
		t.Errorf("state %q, want recorded", string(got.State))
	}
	if got.ReopenCount != 1 {
		t.Errorf("reopen_count = %d, want 1", got.ReopenCount)
	}
	rec := recordWithTransition(h, inc, "reopen")
	if rec == nil {
		t.Fatalf("no reopen record was appended for %s", inc)
	}
	if v, _ := rec["reopen"].(bool); !v {
		t.Errorf("reopen record reopen = %v, want true", rec["reopen"])
	}
	if v := rec["reopen_count"]; v != 1 {
		t.Errorf("reopen record reopen_count = %v, want 1", v)
	}
	if v, _ := rec["from"].(string); v != string(types.StResolved) {
		t.Errorf("reopen record from = %v, want resolved", rec["from"])
	}
	if v, _ := rec["to"].(string); v != string(types.StRecorded) {
		t.Errorf("reopen record to = %v, want recorded", rec["to"])
	}
	if n := h.ledger.countPayload("transition", "T01"); n != 1 {
		t.Errorf("%d T01 records, want 1 (a reopen never opens a new incident)", n)
	}

	issuesAfter, boardsAfter, commentsAfter := h.outlets.counts()
	if issuesAfter != issuesBefore {
		t.Errorf("the reopen filed a new issue (%d → %d), want none", issuesBefore, issuesAfter)
	}
	if boardsAfter != boardsBefore {
		t.Errorf("the reopen filed a new board row (%d → %d), want none", boardsBefore, boardsAfter)
	}
	if commentsAfter != commentsBefore+1 {
		t.Errorf("comments %d → %d, want exactly one recurrence comment added", commentsBefore, commentsAfter)
	}

	open, found, err := h.l.OpenForKey(context.Background(), sigFor(name).String(), "")
	if err != nil {
		t.Fatalf("OpenForKey: %v", err)
	}
	if !found || open.ID != inc {
		t.Errorf("OpenForKey = %+v, %t, want the reopened incident %s", open, found, inc)
	}
}

// TestDedupReopenConflictQuarantinesMismatchedIncident pins §3.9's last arm and
// its §5 code: an open incident found for the arriving sig whose sig does not
// match is an index-integrity failure — TROUBLE-LADDER-020 is recorded, the
// mismatched incident is quarantined (T47), the correct incident is created, and
// both ids appear in one record.
//
// The mismatched incident is adopted with a controlled id (the boot-rebuild seam
// §3.9's failure describes) so the assertion cannot be confused by an id
// collision between two incidents minted in the same millisecond.
func TestDedupReopenConflictQuarantinesMismatchedIncident(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay},
	}})
	var a arrivals

	const staleID = "inc_zzd_stale_index_mismatch"
	staleSig := sigFor("dedup-conflict-seed")
	h.l.AdoptIncident(types.Incident{
		ID:       staleID,
		Sig:      staleSig.String(),
		State:    types.StRecorded,
		Rung:     types.RungPlay,
		Severity: types.SevHigh,
	}, []string{staleSig.String()}, "ink_zzd_stale_index_mismatch", "rule-a", types.RungPlay, types.RungPlay)

	// The index maps the arriving sig onto that incident, whose sig is a
	// different one: the integrity failure §3.9 describes.
	arrivingSig := sigFor("dedup-conflict-arriving")
	h.index.mu.Lock()
	h.index.openBySig[arrivingSig.String()] = types.Incident{
		ID:    staleID,
		Sig:   staleSig.String(),
		State: types.StRecorded,
	}
	h.index.mu.Unlock()

	// A different unit subject, so the arrival matches neither sig nor inKey of
	// the stale incident and the mismatch is the only explanation left.
	arrival := a.obs(arrivingSig, "rule-a", "dbus")
	arrival.Subject = "gateway.service"
	arrival.Detail = map[string]any{"unit": "gateway.service"}

	res := h.admit(t, arrival)
	if res.Reason != string(types.CodeLadder020) {
		t.Errorf("admission reason = %q, want %q", res.Reason, string(types.CodeLadder020))
	}
	if !res.Created || res.Inc == "" || res.Inc == staleID {
		t.Fatalf("admission = %+v, want a new incident distinct from %s", res, staleID)
	}

	mismatched, err := h.l.Incident(context.Background(), staleID)
	if err != nil {
		t.Fatalf("Incident(%s): %v", staleID, err)
	}
	if mismatched.State != types.StQuarantined {
		t.Errorf("the mismatched incident is %q, want quarantined", string(mismatched.State))
	}

	// Both ids in one record.
	rec := recordWithTransition(h, staleID, "quarantine")
	if rec == nil {
		t.Fatalf("no quarantine record was appended for %s", staleID)
	}
	if v, _ := rec["mismatched_with"].(string); v != res.Inc {
		t.Errorf("quarantine record mismatched_with = %v, want the new incident %s", rec["mismatched_with"], res.Inc)
	}
	if v, _ := rec["quarantine_reason"].(string); v != reasonIndexMismatch {
		t.Errorf("quarantine record quarantine_reason = %v, want %q", rec["quarantine_reason"], reasonIndexMismatch)
	}
	if sigs, _ := rec["sigs"].([]string); len(sigs) == 0 {
		t.Errorf("the quarantine record does not name the sigs it was keyed by: %v", rec["sigs"])
	}
	// §5 mirrors every returned code into payload.error_code (SPEC-INDEX §5.3).
	// This path records the ids but not the code today; if it ever does carry a
	// code it must be -020.
	if code, ok := rec["error_code"]; ok && code != string(types.CodeLadder020) {
		t.Errorf("quarantine record error_code = %v, want %s", code, string(types.CodeLadder020))
	}

	fresh, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident(%s): %v", res.Inc, err)
	}
	if fresh.State != types.StRecorded {
		t.Errorf("the correct incident is %q, want recorded", string(fresh.State))
	}
	if fresh.Sig != arrivingSig.String() {
		t.Errorf("the correct incident carries sig %q, want %q", fresh.Sig, arrivingSig.String())
	}
	open, found, err := h.l.OpenForKey(context.Background(), arrivingSig.String(), "")
	if err != nil {
		t.Fatalf("OpenForKey: %v", err)
	}
	if !found || open.ID != res.Inc {
		t.Errorf("OpenForKey = %+v, %t, want the correct incident %s", open, found, res.Inc)
	}
}
