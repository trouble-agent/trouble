package ladder

import (
	"context"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// stabilize_test.go is the SPEC-05 §7 row for AC-2 and AC-3: the `for=`
// stabilization interval, the fallback for an invalid value, the four entry rungs
// and the play ceiling.
//
// Division of labour (§4.1): internal/sensors (SPEC-03) evaluates the rule and
// calls Admit only once the stabilization interval is satisfied. §3.2's T01 row
// carries the caller's verdict as data (`stabilize_degraded`) and names no
// stabilization guard, so the ladder records the verdict instead of re-deriving
// it. These tests pin both halves — what the ladder records, and that it does not
// second-guess the caller.

// probeEval is a RuleEvaluator that counts its own use; the harness's fakeEval
// answers "matched and stabilized" unconditionally.
type probeEval struct {
	matched, stabilized bool
	matchCalls          int
	stabilizedCalls     int
}

func (p *probeEval) Match(r types.Rule, ev Observation) bool { p.matchCalls++; return p.matched }

func (p *probeEval) Stabilized(r types.Rule, s StabilizationState, now time.Time) bool {
	p.stabilizedCalls++
	return p.stabilized
}

// ---- AC-2: the `for=` interval ---- //

// TestStabilizeForIntervalIsRecordedNotReEvaluated pins AC-2's `for=2m` half: the
// interval and its resets belong to the caller, and the ladder records the verdict
// on the admission record — including §5 -019's "the second reset admits with
// stabilize_degraded".
func TestStabilizeForIntervalIsRecordedNotReEvaluated(t *testing.T) {
	probe := &probeEval{matched: false, stabilized: false}
	h := newHarness(t, harnessOpts{
		cfg:   Config{StabilizeDefault: "2m"},
		rules: map[string]types.Rule{"rule-stab": {Name: "rule-stab", EntryRung: types.RungPlay, For: "2m"}},
	})
	h.l.deps.Eval = probe
	var a arrivals

	// `for=2m` parses and §4.3's replacement value is the same 2m.
	rule, ok := h.l.rule("rule-stab")
	if !ok {
		t.Fatal("the harness's rule lookup did not resolve rule-stab")
	}
	if rule.For != "2m" {
		t.Errorf("rule for = %q, want 2m", rule.For)
	}
	if h.l.cfg.StabilizeDefault != "2m" {
		t.Errorf("StabilizeDefault = %q, want 2m (the §4.3 replacement for an invalid for=)", h.l.cfg.StabilizeDefault)
	}

	now := h.clock.Now()
	vectors := []struct {
		key   string
		state StabilizationState
		want  bool
		what  string
	}{
		{
			key:   "held",
			state: StabilizationState{FirstQualifyingTS: types.FormatUTC(now.Add(-2 * time.Minute))},
			want:  false,
			what:  "for=2m held from the last qualifying event",
		},
		{
			key:   "reset-once",
			state: StabilizationState{FirstQualifyingTS: types.FormatUTC(now.Add(-10 * time.Second)), Resets: 1, Degraded: false},
			want:  false,
			what:  "a false sample at 1m50s reset the interval: the first reset admits undegraded",
		},
		{
			key:   "reset-twice",
			state: StabilizationState{FirstQualifyingTS: types.FormatUTC(now.Add(-5 * time.Second)), Resets: 2, Degraded: true},
			want:  true,
			what:  "the second reset admits with stabilize_degraded",
		},
	}
	for _, v := range vectors {
		obs := a.obs(sigFor("stab-"+v.key), "rule-stab", "journald")
		obs.Stabilization = v.state
		res := h.admit(t, obs)
		if !res.Created || res.Inc == "" {
			t.Fatalf("%s (%s): %+v, want a new incident", v.key, v.what, res)
		}
		rec := admissionRecord(h, obs.Sig.String())
		if rec == nil {
			t.Fatalf("%s (%s): no admission record was appended", v.key, v.what)
		}
		if got, ok := rec["stabilize_degraded"].(bool); !ok || got != v.want {
			t.Errorf("%s (%s): stabilize_degraded = %v, want %t", v.key, v.what, rec["stabilize_degraded"], v.want)
		}
		st := h.l.incStateFor(res.Inc)
		if st == nil {
			t.Fatalf("%s: incident %s is not in the index", v.key, res.Inc)
		}
		if st.Stabilization != v.state {
			t.Errorf("%s: the incident carries %+v, want the caller's %+v", v.key, st.Stabilization, v.state)
		}
		inc, err := h.l.Incident(context.Background(), res.Inc)
		if err != nil {
			t.Fatalf("%s: Incident: %v", v.key, err)
		}
		if inc.State != types.StRecorded {
			t.Errorf("%s: state %q, want recorded", v.key, string(inc.State))
		}
	}

	// The seam is one-way: an evaluator that answers "not matched and not
	// stabilized" does not stop an admission, because §4.1 has SPEC-03 make that
	// call before Admit. If this ever changes, the ladder owns the interval and
	// these vectors should assert a refusal here instead.
	if probe.matchCalls != 0 || probe.stabilizedCalls != 0 {
		t.Errorf("the ladder consulted the evaluator %d match / %d stabilized time(s) during admission: the condition language is SPEC-03's (§4.1), the ladder records its verdict",
			probe.matchCalls, probe.stabilizedCalls)
	}
	e, ok := FindEdge("T01")
	if !ok {
		t.Fatal("edge T01 is missing from the table")
	}
	if !contains(e.Emitted, "stabilize_degraded") {
		t.Errorf("T01 must emit stabilize_degraded as data (§3.2); it emits %v", e.Emitted)
	}
}

// ---- AC-2: an invalid `for=` ---- //

// TestStabilizeInvalidForIsNotLadderOwned records §7's invalid-`for=` row
// (`for=-1s` and `for=25h` → stabilize_default + TROUBLE-LADDER-019) as far as it
// is reachable: the replacement value resolves through the config surface and the
// ladder records no -019 of its own. Nothing in internal/ladder reads Rule.For,
// consults Deps.Eval during Admit or references types.CodeLadder019 — the
// fallback and its code belong to SPEC-03's governor, which parses and holds the
// interval before it calls Admit (reported; no path was invented here).
func TestStabilizeInvalidForIsNotLadderOwned(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg: Config{StabilizeDefault: "2m"},
		rules: map[string]types.Rule{
			"rule-neg-for":  {Name: "rule-neg-for", EntryRung: types.RungPlay, For: "-1s"},
			"rule-huge-for": {Name: "rule-huge-for", EntryRung: types.RungPlay, For: "25h"},
			"rule-zero-for": {Name: "rule-zero-for", EntryRung: types.RungPlay, For: "0s"},
		},
	})
	var a arrivals

	if h.l.cfg.StabilizeDefault != "2m" {
		t.Errorf("StabilizeDefault = %q, want 2m", h.l.cfg.StabilizeDefault)
	}
	cfg, cerr := FromValues([]types.ConfigValue{{Key: "ladder.stabilize_default", Value: "2m"}})
	if cerr != nil {
		t.Fatalf("FromValues(ladder.stabilize_default): %v", cerr)
	}
	if cfg.StabilizeDefault != "2m" {
		t.Errorf("FromValues StabilizeDefault = %q, want 2m", cfg.StabilizeDefault)
	}
	// An unparseable value keeps the documented default rather than a zero window.
	cfg, cerr = FromValues([]types.ConfigValue{{Key: "ladder.stabilize_default", Value: "not-a-duration"}})
	if cerr != nil {
		t.Fatalf("FromValues(ladder.stabilize_default) with a bad value: %v", cerr)
	}
	if cfg.StabilizeDefault != "2m" {
		t.Errorf("FromValues with an unparseable value gave %q, want the 2m default", cfg.StabilizeDefault)
	}

	for _, ruleName := range []string{"rule-neg-for", "rule-huge-for", "rule-zero-for"} {
		res := h.admit(t, a.obs(sigFor("stab-invalid-"+ruleName), ruleName, "journald"))
		if !res.Created || res.Inc == "" {
			t.Fatalf("%s: %+v, want a new incident", ruleName, res)
		}
		inc, err := h.l.Incident(context.Background(), res.Inc)
		if err != nil {
			t.Fatalf("%s: Incident: %v", ruleName, err)
		}
		if inc.State != types.StRecorded {
			t.Errorf("%s: state %q, want recorded", ruleName, string(inc.State))
		}
	}
	if n := h.ledger.countPayload("error_code", string(types.CodeLadder019)); n != 0 {
		t.Errorf("the ladder emitted %d TROUBLE-LADDER-019 record(s); the fallback for an invalid for= is SPEC-03's to record, so a ladder-side -019 means this test should assert the fallback instead", n)
	}
}

// ---- AC-3: entry rungs ---- //

// TestEntryRungsWalkTheirFirstRung pins AC-3's entry-rung row: a rule entering at
// `record`, `play`, `research` or `agent` walks that rung first (T06, T03, T04,
// T05), and a stage entry the entry rung puts out of reach is refused with
// TROUBLE-LADDER-002 without moving the incident.
func TestEntryRungsWalkTheirFirstRung(t *testing.T) {
	cases := []struct {
		name          string
		rule          types.Rule
		firstEdge     string
		wantState     types.LadderState
		wantRungAfter types.Rung
		outOfReach    string // a stage entry the entry rung excludes ("" = none)
		stillLegal    string // a stage entry the ceiling still allows ("" = none)
		stillState    types.LadderState
	}{
		{
			name:          "record",
			rule:          types.Rule{Name: "rule-record", EntryRung: types.RungRecord},
			firstEdge:     "T06",
			wantState:     types.StVerifying,
			wantRungAfter: types.RungOutlets,
			outOfReach:    "T03",
		},
		{
			name:          "play",
			rule:          types.Rule{Name: "rule-play", EntryRung: types.RungPlay},
			firstEdge:     "T03",
			wantState:     types.StPlayDrafted,
			wantRungAfter: types.RungPlay,
			outOfReach:    "T04",
		},
		{
			name:          "research",
			rule:          types.Rule{Name: "rule-research", EntryRung: types.RungResearch},
			firstEdge:     "T04",
			wantState:     types.StResRequested,
			wantRungAfter: types.RungResearch,
			outOfReach:    "T05",
		},
		{
			name:          "agent",
			rule:          types.Rule{Name: "rule-agent", EntryRung: types.RungAgent},
			firstEdge:     "T05",
			wantState:     types.StAgentRunning,
			wantRungAfter: types.RungAgent,
			stillLegal:    "T04",
			stillState:    types.StResRequested,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{research: true, rules: map[string]types.Rule{c.rule.Name: c.rule}})
			var a arrivals

			obs := a.obs(sigFor("entry-"+c.name), c.rule.Name, "journald")
			res := h.admit(t, obs)
			if !res.Created || res.Inc == "" {
				t.Fatalf("admission: %+v, want a new incident", res)
			}
			// The admission record names the entry rung (T01 carries entry_rung/rung).
			rec := admissionRecord(h, obs.Sig.String())
			if rec == nil {
				t.Fatalf("no T01 record for %s", obs.Sig.String())
			}
			if got := rec["transition"]; got != "T01" {
				t.Errorf("admission record transition = %v, want T01", got)
			}
			if got := rec["entry_rung"]; got != string(c.rule.EntryRung) {
				t.Errorf("admission record entry_rung = %v, want %q", got, c.rule.EntryRung)
			}
			if got := rec["rung"]; got != string(c.rule.EntryRung) {
				t.Errorf("admission record rung = %v, want %q", got, c.rule.EntryRung)
			}
			inc, err := h.l.Incident(context.Background(), res.Inc)
			if err != nil {
				t.Fatalf("Incident: %v", err)
			}
			if inc.State != types.StRecorded {
				t.Fatalf("state %q, want recorded", string(inc.State))
			}
			if inc.EntryRung != c.rule.EntryRung || inc.Rung != c.rule.EntryRung {
				t.Errorf("entry rung %q / rung %q, want %q for both", inc.EntryRung, inc.Rung, c.rule.EntryRung)
			}

			// The first edge the incident walks is the rule's entry rung.
			got, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: c.firstEdge})
			if err != nil {
				t.Fatalf("%s: %v", c.firstEdge, err)
			}
			if got.State != c.wantState {
				t.Errorf("%s reached %q, want %q", c.firstEdge, string(got.State), string(c.wantState))
			}
			if got.Rung != c.wantRungAfter {
				t.Errorf("%s left rung %q, want %q", c.firstEdge, got.Rung, c.wantRungAfter)
			}
			walk := recordWithTransition(h, res.Inc, c.firstEdge)
			if walk == nil {
				t.Fatalf("no %s record was appended", c.firstEdge)
			}
			if walk["rung"] != string(c.wantRungAfter) {
				t.Errorf("%s record rung = %v, want %q", c.firstEdge, walk["rung"], c.wantRungAfter)
			}

			// A rung the entry rung excludes is refused, and a refusal never
			// moves the incident (§3.3, INV-6).
			if c.outOfReach != "" {
				res2 := h.admit(t, a.obs(sigFor("entry-"+c.name+"-out-of-reach"), c.rule.Name, "journald"))
				if _, err := h.l.Advance(context.Background(), res2.Inc, Transition{Trigger: c.outOfReach}); CodeOf(err) != types.CodeLadder002 {
					t.Errorf("%s on a %s-entry incident: code %q, want TROUBLE-LADDER-002", c.outOfReach, c.name, CodeOf(err))
				}
				after, err := h.l.Incident(context.Background(), res2.Inc)
				if err != nil {
					t.Fatalf("Incident: %v", err)
				}
				if after.State != types.StRecorded || after.Rung != c.rule.EntryRung {
					t.Errorf("the refusal moved the incident to %q / rung %q, want recorded / %q",
						string(after.State), after.Rung, c.rule.EntryRung)
				}
			}
			if c.stillLegal != "" {
				res3 := h.admit(t, a.obs(sigFor("entry-"+c.name+"-still-legal"), c.rule.Name, "journald"))
				after, err := h.l.Advance(context.Background(), res3.Inc, Transition{Trigger: c.stillLegal})
				if err != nil {
					t.Fatalf("%s on a %s-entry incident: %v", c.stillLegal, c.name, err)
				}
				if after.State != c.stillState {
					t.Errorf("%s reached %q, want %q", c.stillLegal, string(after.State), string(c.stillState))
				}
			}
		})
	}
}

// ---- AC-3: the play ceiling ---- //

// TestPlayCeilingEscalatesInsteadOfEnteringResearch pins §7's last stabilize row
// and AC-3's ceiling rule: a rule entering at `play` may reach only the play rung,
// so a failed play ends at the outlets + escalate path (T18) instead of advancing
// to research — the research port is wired and is still never called.
func TestPlayCeilingEscalatesInsteadOfEnteringResearch(t *testing.T) {
	h := newHarness(t, harnessOpts{
		research: true,
		rules:    map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})

	inc, st := playFailed(t, h, "ceiling-play")
	st.RunSummary = &RunSummary{FailClass: "permanent", TasksFailed: 1}

	for _, edge := range []string{"T14", "T16"} {
		if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: edge}); CodeOf(err) != types.CodeLadder002 {
			t.Fatalf("%s on a play-ceiling incident: code %q, want TROUBLE-LADDER-002", edge, CodeOf(err))
		}
	}
	if h.research.requests != 0 {
		t.Fatalf("a refused rung advance called the research port %d time(s)", h.research.requests)
	}
	before, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if before.State != types.StPlayFailed || before.Rung != types.RungPlay {
		t.Errorf("the refusals moved the incident to %q / rung %q, want play:failed / play", string(before.State), before.Rung)
	}

	got, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T18"})
	if err != nil {
		t.Fatalf("T18: %v", err)
	}
	if got.State != types.StEscalated {
		t.Fatalf("state %q, want escalated", string(got.State))
	}
	rec := recordWithTransition(h, inc, "T18")
	if rec == nil {
		t.Fatalf("no T18 record was appended for %s", inc)
	}
	if v, _ := rec["pending_human"].(bool); !v {
		t.Errorf("T18 record pending_human = %v, want true", rec["pending_human"])
	}
	if v, _ := rec["error_code"].(string); v != string(types.CodeLadder002) {
		t.Errorf("T18 record error_code = %v, want %s", rec["error_code"], string(types.CodeLadder002))
	}
	if got.IssueID == "" || got.TaskID == "" {
		t.Errorf("the escalation filed issue %q / board row %q, want both (the outlets still fire, §3.7)", got.IssueID, got.TaskID)
	}
	issues, boards, comments := h.outlets.counts()
	if issues != 1 {
		t.Errorf("%d issue(s), want 1 for the escalation", issues)
	}
	if boards != 1 {
		t.Errorf("%d board row(s), want 1 for the escalation", boards)
	}
	if comments != 0 {
		t.Errorf("%d comment(s), want 0 (the escalation files one issue and one board row)", comments)
	}
	if h.research.requests != 0 {
		t.Errorf("the research port was called %d time(s) on a play-ceiling incident, want 0", h.research.requests)
	}
}
