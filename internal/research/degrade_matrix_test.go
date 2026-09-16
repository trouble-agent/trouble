package research

// degrade_matrix_test.go — SPEC-07 §7 `degrade_matrix_test.go`: the seven-row
// matrix, the per-rung budget gates and the failure fence.
//
// Every row asserts state, reason, error_code, gap presence and the single
// non-negotiable: the rung returns so the ladder can proceed.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestDegradeMatrix(t *testing.T) {
	type row struct {
		name     string
		cfg      map[string]any
		lab      func(*labStub)
		wantStat string
		wantReas string
		wantCode types.ErrorCode
		wantGap  string // "" = no gap
		noHTTP   bool
	}
	rows := []row{
		{
			name: "driver none", cfg: map[string]any{"driver": types.DriverNone},
			wantStat: types.ResSkipped, wantReas: types.ResSkipDriverNone,
			wantCode: types.CodeResearch008, noHTTP: true,
		},
		{
			name: "lab unreachable", cfg: map[string]any{"request_timeout": "1ns"},
			lab:      func(l *labStub) {},
			wantStat: types.ResDegraded, wantReas: types.ResReasonLabUnreachable,
			wantCode: types.CodeResearch001, wantGap: "research_lab_unreachable",
		},
		{
			name: "strict decoder 400", cfg: nil,
			lab:      func(l *labStub) { l.discover400 = true },
			wantStat: types.ResDegraded, wantReas: types.ResReasonStrictDecoder,
			wantCode: types.CodeResearch002, wantGap: "research_strict_decoder_reject",
		},
		{
			name: "409 duplicate", cfg: nil,
			lab:      func(l *labStub) { l.submit409 = true },
			wantStat: types.ResRequested, wantReas: "",
			wantCode: types.CodeResearch003, wantGap: "",
		},
		{
			name: "503 solver unavailable", cfg: nil,
			lab:      func(l *labStub) { l.submit503 = true },
			wantStat: types.ResDegraded, wantReas: types.ResReasonSolverUnavailable,
			wantCode: types.CodeResearch004, wantGap: "research_solver_unavailable",
		},
		{
			name: "poll timeout", cfg: map[string]any{"poll_interval": "1ms", "poll_timeout": "30ms"},
			lab:      func(l *labStub) { l.queueStates = []string{"solving"} },
			wantStat: types.ResDegraded, wantReas: types.ResReasonPollTimeout,
			wantCode: types.CodeResearch006, wantGap: "research_poll_timeout",
		},
		{
			name: "brief invalid", cfg: nil,
			lab: func(l *labStub) {
				l.queueStates = []string{"solved"}
				l.queueAnswer = map[string]any{"status": "pending", "solution": "maybe later"}
			},
			wantStat: types.ResSkipped, wantReas: types.ResSkipBriefInvalid,
			wantCode: types.CodeResearch009, wantGap: "",
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			lab := newLabStub(t)
			if r.lab != nil {
				r.lab(lab)
			}
			s, deps := newTestService(t, lab, r.cfg)
			ctx := context.Background()
			if r.name == "driver none" {
				if err := s.Start(ctx); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			// Rows whose terminal state is produced by the poller (rather than by
			// the synchronous half of the rung) must await it.
			await := r.wantGap == "research_poll_timeout" || r.name == "brief invalid"
			out, err := s.Run(ctx, testIncident(), testSig(), bundleFor(map[string]any{"await": await}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// Every row must return an outcome the ladder can proceed past: a
			// nil error and a state from the frozen set.
			switch out.State {
			case types.ResRequested, types.ResReturned, types.ResDegraded, types.ResSkipped:
			default:
				t.Fatalf("state %q is outside the frozen set", out.State)
			}
			if r.wantStat == types.ResSkipped || r.wantStat == types.ResDegraded {
				if out.State != r.wantStat {
					t.Fatalf("state = %q, want %q (outcome %+v)", out.State, r.wantStat, out)
				}
				if out.DegradedReason != r.wantReas {
					t.Fatalf("reason = %q, want %q", out.DegradedReason, r.wantReas)
				}
			} else if out.State != types.ResRequested {
				t.Fatalf("state = %q, want requested", out.State)
			}
			if r.noHTTP && lab.total() != 0 {
				t.Fatalf("driver none made %d HTTP requests, want 0", lab.total())
			}
			if r.noHTTP {
				return
			}
			if r.wantGap == "" {
				if gaps := deps.payloads(types.KGap); len(gaps) != 0 {
					t.Fatalf("gaps = %v, want none for this row", gaps)
				}
			} else {
				gaps := deps.payloads(types.KGap)
				if len(gaps) == 0 || gaps[0]["cause"] != r.wantGap {
					t.Fatalf("gaps = %v, want %s", gaps, r.wantGap)
				}
				if gaps[0]["est_lost"] != -1 {
					t.Fatalf("est_lost = %v, want -1 (unknowable)", gaps[0]["est_lost"])
				}
				if gaps[0]["sensor"] != "research" {
					t.Fatalf("gap sensor = %v, want research", gaps[0]["sensor"])
				}
			}
			if r.wantCode != "" {
				found := false
				for _, p := range deps.payloads(types.KResearch) {
					if p["error_code"] == string(r.wantCode) {
						found = true
					}
				}
				if !found {
					t.Fatalf("no research record carried %s: %v", r.wantCode, deps.payloads(types.KResearch))
				}
			}
		})
	}
}

// TestWorstCaseRungBudget pins the §3.8 request ceiling: ≤54 requests for one
// rung and ≤2 discover requests + ≤3 submits + ≤48 polls.
func TestWorstCaseRungBudget(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, deps := newTestService(t, lab, map[string]any{
		"poll_interval": "1ms", "poll_timeout": "120ms", "poll_max_requests": 48,
	})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{
		"env": "prod", "lang": "go", "version": "2.4.1", "await": true,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded {
		t.Fatalf("outcome = %+v, want degraded", out)
	}
	if lab.total() > 54 {
		t.Fatalf("rung made %d requests, want ≤54", lab.total())
	}
	if n := lab.count("/api/v1/problems/discover"); n > 2 {
		t.Fatalf("discover requests = %d, want ≤2 (D1+D2)", n)
	}
	if n := lab.count("/api/v1/problems/submit"); n > 3 {
		t.Fatalf("submit requests = %d, want ≤3 (the cadence ladder)", n)
	}
	if n := lab.count("/api/v1/queue/"); n > 48 {
		t.Fatalf("poll requests = %d, want ≤48", n)
	}
	// The cost object is recorded with the request counts.
	p := deps.lastPayload(types.KResearch)
	cost, ok := p["cost"].(map[string]any)
	if !ok {
		t.Fatalf("payload.cost missing: %v", p)
	}
	for _, k := range []string{"requests", "discover_requests", "submit_requests", "poll_requests", "polls", "corpus_greps"} {
		if _, ok := cost[k]; !ok {
			t.Fatalf("cost is missing %q: %v", k, cost)
		}
	}
	if asInt64(cost["requests"], 0) < 1 {
		t.Fatalf("cost.requests = %v", cost["requests"])
	}
}

// TestDailyBudgetIsRebuiltFromTheLedger: the counter survives a restart because
// it is rebuilt by replaying today's records (§3.8).
func TestDailyBudgetIsRebuiltFromTheLedger(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, map[string]any{"requests_per_day": 3})
	for i := 0; i < 3; i++ {
		if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"message": "start request repeated too quickly " + string(rune('a'+i))})); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	// The 4th rung is skipped with the budget reason and no error code.
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResSkipped || out.DegradedReason != types.ResSkipBudgetExhausted {
		t.Fatalf("outcome = %+v, want skipped/budget_exhausted", out)
	}
	p := deps.lastPayload(types.KResearch)
	if p["error_code"] != "" {
		t.Fatalf("a budget decision is not a failure: error_code = %v", p["error_code"])
	}
	if p["skip_reason"] != types.ResSkipBudgetExhausted {
		t.Fatalf("skip_reason = %v", p["skip_reason"])
	}

	// A fresh Service replaying the same ledger starts with the counter spent.
	deps2 := newFakeDeps()
	s2, err := New(map[string]any{
		"lab_url": lab.url(), "requests_per_day": 3, "corpus_roots": []any{}, "lab_data_dir": t.TempDir(),
		"poll_interval": "1ms", "poll_timeout": "50ms",
	}, deps2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s2.Close()
	recs := make([]types.Record, 0, 8)
	for i, d := range deps.ofKind(types.KResearch) {
		recs = append(recs, types.Record{
			Seq: uint64(i + 1), Kind: types.KResearch, Sig: d.Sig, Inc: d.Inc,
			TS: types.NowUTC(), Payload: d.Payload,
		})
	}
	s2.Replay(recs)
	out2, err := s2.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run after replay: %v", err)
	}
	if out2.State != types.ResSkipped || out2.DegradedReason != types.ResSkipBudgetExhausted {
		t.Fatalf("a restart reset the daily budget: %+v", out2)
	}
}

// TestSubmitFuseOpensAfterThreeRejects: three consecutive strict-decoder
// rejects open the fence; discover and the corpus stay usable (§5.2).
func TestSubmitFuseOpensAfterThreeRejects(t *testing.T) {
	lab := newLabStub(t)
	lab.submit400 = "invalid JSON: json: unknown field \"whatever\""
	s, _ := newTestService(t, lab, nil)
	for i := 0; i < 3; i++ {
		if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"message": "start request repeated too quickly " + string(rune('a'+i))})); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if got := s.Snapshot()["fence"]; got != "submit_disabled" {
		t.Fatalf("fence = %v, want submit_disabled", got)
	}
	before := lab.count("/api/v1/problems/discover")
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonStrictDecoder {
		t.Fatalf("outcome = %+v, want the fuse's degraded/strict_decoder_reject", out)
	}
	if lab.count("/api/v1/problems/discover") <= before {
		t.Fatalf("discover must keep working while the fuse is open")
	}
}

// TestCooldownAfterConsecutiveFailures: three transport failures close the lab
// conversation for `cooldown`; the next rung degrades without any request.
func TestCooldownAfterConsecutiveFailures(t *testing.T) {
	lab := newLabStub(t)
	s, deps := newTestService(t, lab, map[string]any{"request_timeout": "1ns", "cooldown_failures": 3, "cooldown": "1h"})
	for i := 0; i < 3; i++ {
		if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"message": "start request repeated too quickly " + string(rune('a'+i))})); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	before := lab.total()
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonLabUnreachable {
		t.Fatalf("outcome = %+v", out)
	}
	if lab.total() != before {
		t.Fatalf("a cooldown must make no requests: %d → %d", before, lab.total())
	}
	if got := s.Snapshot()["cooldown_until"]; got == "" {
		t.Fatalf("snapshot must report the cooldown")
	}
	if p := deps.lastPayload(types.KResearch); p["error_code"] != string(types.CodeResearch001) {
		t.Fatalf("error_code = %v", p["error_code"])
	}
}

// TestMaxInflightDegradesInsteadOfBlocking: over the concurrency cap the rung
// degrades immediately (the ladder never waits on research).
func TestMaxInflightDegradesInsteadOfBlocking(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, _ := newTestService(t, lab, map[string]any{
		"max_inflight": 1, "poll_interval": "5ms", "poll_timeout": "2s",
	})
	first, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.State != types.ResRequested {
		t.Fatalf("first = %+v", first)
	}
	second, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"message": "another message entirely"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if second.State != types.ResDegraded {
		t.Fatalf("second = %+v, want an immediate degrade", second)
	}
}

// TestLedgerRecordKeysArePinned guards the §3.6 payload contract.
func TestLedgerRecordKeysArePinned(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, nil)
	if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p := deps.lastPayload(types.KResearch)
	for _, k := range []string{
		"res_id", "state", "driver", "slug", "slug_fallback", "source", "app_kind", "taxonomy",
		"idem_key", "submission_id", "cadence_attempted", "cadence_accepted", "http_status",
		"error_code", "degraded_reason", "skip_reason", "corpus_grep_hit", "corpus_path",
		"corpus_parse_errors", "corpus_timeout", "poll_count", "queue_depth_at_submit",
		"unknown_status", "cost",
	} {
		if _, ok := p[k]; !ok {
			t.Fatalf("research payload is missing the pinned key %q", k)
		}
	}
	if deps.lastPayload(types.KResearch)["res_id"] == "" {
		t.Fatalf("res_id must be allocated at rung entry")
	}
	// The record is the `research` kind and carries the sig + incident.
	d := deps.ofKind(types.KResearch)
	if len(d) == 0 || d[0].Sig == "" || d[0].Inc != testIncident().ID {
		t.Fatalf("research record must carry sig and inc: %+v", d)
	}
}

// TestCorpusStageRecordsParseErrorsAndTimeout pins the two corpus counters into
// the record.
func TestCorpusStageRecordsParseErrorsAndTimeout(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, nil)
	// A stub corpus reports a timeout and a parse error through its result.
	sc := &stubCorpus{res: corpusResult{Scanned: 3, ParseErrors: 2, Timeout: true}}
	s.corpus = sc
	if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sc.queries) != 1 || sc.queries[0] != "payment-worker-crash-loop" {
		t.Fatalf("corpus queries = %v", sc.queries)
	}
	p := deps.lastPayload(types.KResearch)
	raw, _ := json.Marshal(p)
	if !strings.Contains(string(raw), "corpus_grep_hit") {
		t.Fatalf("payload = %s", raw)
	}
	if asInt64(p["corpus_parse_errors"], -1) != 2 {
		t.Fatalf("corpus_parse_errors = %v, want 2 (from the grep's result)", p["corpus_parse_errors"])
	}
	if p["corpus_timeout"] != true {
		t.Fatalf("corpus_timeout = %v, want true", p["corpus_timeout"])
	}
	if p["corpus_path"] != "" {
		t.Fatalf("corpus_path = %v, want empty without a hit", p["corpus_path"])
	}
}
