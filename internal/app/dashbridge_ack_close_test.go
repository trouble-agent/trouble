package app

// dashbridge_ack_close_test.go — TRBL-118: the user core loop
// ingest -> ack -> close, end to end through the production wiring.
//
// The dogfood tick of 2026-10-23 reproduced the blocker live: the ack route
// answered 200 while writing ONLY an audit record, so the incident stayed in a
// state with no edge into `resolved` and the dashboard's own close button could
// never succeed (400 no_close_edge, or 409 stale_view for any other CAS pair).
// These tests pin the fixed contract on the real route surface:
//
//	TestAckThenCloseSucceedsOnFreshIncident   — AC-1/AC-4: ack -> close is 200
//	                                            using the ack RESPONSE's own
//	                                            (data-seq, state) CAS pair.
//	TestAckIsIdempotentAndKeepsTheMachineClean — a second ack is a record, not an
//	                                            illegal transition.
//	TestCloseFromPreWorkStateStillRefused     — the refusal stays NAMED (409 +
//	                                            LADDER-012 + no_close_edge) and
//	                                            the page stops offering the click.

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/ladder"
	"github.com/trouble-agent/trouble/internal/types"
)

// coreLoop is the production wiring in miniature plus the handles the assertions
// need: real ledger, real ladder, real bridge (NewActions/NewLookup) and a real
// dashboard server on a scratch loopback listener with one write token.
type coreLoop struct {
	base  string
	token string
	inc   string
	sig   types.Sig
	l     *ladder.Ladder
	store *Store
}

func bootCoreLoop(t *testing.T) *coreLoop {
	t.Helper()
	s, lb := testStore(t)
	ctx := context.Background()

	rules := map[string]types.Rule{
		"io-pressure": {
			Name: "io-pressure", Enabled: true, Source: types.SigSource("psi"),
			EntryRung: types.RungPlay, Severity: types.SevHigh,
			Match: []types.Condition{{Field: "some_avg10", Op: ">=", Value: "50", ValueType: "number"}},
		},
	}
	lad, err := ladder.New(ladder.Deps{
		Ledger: s,
		Index:  s,
		Clock:  Clock{Start: time.Now()},
		Eval:   NewEvaluator(),
		Cfg:    ladder.DefaultConfig(),
		Rules:  func(name string) (types.Rule, bool) { r, ok := rules[name]; return r, ok },
	})
	if err != nil {
		t.Fatalf("ladder.New: %v", err)
	}

	obs := ladder.Observation{
		EventID: "ev_01JAAPACKCLOSELOOP0001",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("ack-close-loop-digest-0001")),
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 91.0},
	}
	res, err := lad.Admit(ctx, obs)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if res.Inc == "" {
		t.Fatalf("Admit returned no incident id (%+v)", res)
	}

	tokenPath := filepath.Join(t.TempDir(), "core-loop-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("dash-write@core-loop", []types.Scope{types.ScopeWrite}, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg := dashboard.DefaultConfig()
	cfg.TokenFile = tokenPath
	cfg.Bind = "127.0.0.1"

	dashDeps := dashboard.Deps{
		Config: cfg,
		Index:  lb.DashReader(),
		Lookup: NewLookup(lb),
		Story: func(inc string) dashboard.Story {
			if row, ok := NewLookup(lb).Incident(inc); ok {
				return NewLookup(lb).Story(*row)
			}
			return dashboard.Story{}
		},
		Health:    func(ctx context.Context) types.HealthResponse { return types.HealthResponse{} },
		Actions:   NewActions(lad, s),
		TokenFile: tokenPath,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srvCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- dashboard.ServeOn(srvCtx, cfg, dashDeps, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		_ = lb.Close(context.Background())
	})
	return &coreLoop{base: "http://" + ln.Addr().String(), token: plaintext, inc: string(res.Inc), sig: obs.Sig, l: lad, store: s}
}

func (c *coreLoop) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

func (c *coreLoop) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(out)
}

// pageCAS pulls the (expected_state, ledger_seq) pair the incident page rendered.
func pageCAS(t *testing.T, page string) (state string, seq uint64) {
	t.Helper()
	m := regexp.MustCompile(`name="expected_state" value="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("incident page carries no expected_state field: %.400s", page)
	}
	s := regexp.MustCompile(`name="ledger_seq" value="([0-9]+)"`).FindStringSubmatch(page)
	if s == nil {
		t.Fatalf("incident page carries no ledger_seq field: %.400s", page)
	}
	n, err := strconv.ParseUint(s[1], 10, 64)
	if err != nil {
		t.Fatalf("ledger_seq %q: %v", s[1], err)
	}
	return m[1], n
}

// fragmentCAS pulls the (state, data-seq) pair the ack RESPONSE fragment carries
// — the pair AC-1 requires the close to be driven with.
func fragmentCAS(t *testing.T, inc, frag string) (state string, seq uint64) {
	t.Helper()
	row := regexp.MustCompile(`href="/incidents/` + regexp.QuoteMeta(inc) + `">[^<]*</a></td><td>[^<]*</td><td>([^<]*)</td>`).
		FindStringSubmatch(frag)
	if row == nil {
		t.Fatalf("the ack response does not contain the incident's row: %.600s", frag)
	}
	s := regexp.MustCompile(`data-seq="([0-9]+)"`).FindStringSubmatch(frag)
	if s == nil {
		t.Fatalf("the ack response carries no data-seq: %.200s", frag)
	}
	n, err := strconv.ParseUint(s[1], 10, 64)
	if err != nil {
		t.Fatalf("data-seq %q: %v", s[1], err)
	}
	return row[1], n
}

func ladderState(t *testing.T, l *ladder.Ladder, inc string) types.LadderState {
	t.Helper()
	cur, err := l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident(%s): %v", inc, err)
	}
	return cur.State
}

// TestAckThenCloseSucceedsOnFreshIncident is AC-1 and AC-4: ingest -> ack ->
// close, with the close driven by the ack response's OWN (data-seq, state) pair,
// both writes answering 200 and the incident ending in `resolved`.
func TestAckThenCloseSucceedsOnFreshIncident(t *testing.T) {
	c := bootCoreLoop(t)

	// 1. The page before any ack: the ack control is offered, the close one is
	//    NOT (a pre-work state has no edge into resolved — AC-3).
	status, page := c.get(t, "/incidents/"+c.inc)
	if status != http.StatusOK {
		t.Fatalf("GET /incidents/%s = %d, want 200 (%.400s)", c.inc, status, page)
	}
	if !strings.Contains(page, `action="/api/incidents/`+c.inc+`/ack"`) {
		t.Fatalf("the page does not offer the ack action: %.600s", page)
	}
	if strings.Contains(page, `action="/api/incidents/`+c.inc+`/close"`) {
		t.Fatalf("the page still renders a close form for state %q: the control cannot succeed (TRBL-118)",
			ladderState(t, c.l, c.inc))
	}
	state, seq := pageCAS(t, page)

	// 2. The ack: 200, and the ladder really is in `acknowledged` afterwards.
	ackBody := `{"reason":"looking at it","until":"","expected_state":"` + state + `","ledger_seq":` + strconv.FormatUint(seq, 10) + `}`
	status, ackFrag := c.post(t, "/api/incidents/"+c.inc+"/ack", ackBody)
	if status != http.StatusOK {
		t.Fatalf("POST ack = %d, want 200 (body %.600s)", status, ackFrag)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StAcknowledged {
		t.Fatalf("ladder state after the ack is %q, want acknowledged (the ack must own the state, not only the record)", string(got))
	}
	// The index (the dashboard's own view) agrees with the ladder.
	if look, ok := NewLookup(c.store.L).Incident(c.inc); !ok || look.State != types.StAcknowledged {
		st := types.LadderState("<missing>")
		if ok {
			st = look.State
		}
		t.Fatalf("index state after the ack is %q, want acknowledged (live append == boot rebuild)", string(st))
	}

	// 3. The ack response is the fragment a phone reloads from: its own
	//    (data-seq, state) pair must drive a successful close.
	ackState, ackSeq := fragmentCAS(t, c.inc, ackFrag)
	if ackState != string(types.StAcknowledged) {
		t.Fatalf("the ack response renders state %q, want acknowledged", ackState)
	}
	if !strings.Contains(ackFrag, `data-seq="`+strconv.FormatUint(ackSeq, 10)+`"`) {
		t.Fatalf("the ack response's data-seq is not parseable: %.200s", ackFrag)
	}

	// 4. The refreshed page offers the close control now that the state has an
	//    edge into `resolved`.
	status, page = c.get(t, "/incidents/"+c.inc)
	if status != http.StatusOK {
		t.Fatalf("GET /incidents/%s after the ack = %d, want 200", c.inc, status)
	}
	if !strings.Contains(page, `action="/api/incidents/`+c.inc+`/close"`) {
		t.Fatalf("the page does not offer the close action for state %q: %.600s",
			ladderState(t, c.l, c.inc), page)
	}

	// 5. The close, with the ack response's pair: 200, and the incident resolves.
	closeBody := `{"reason":"fixed it","resolution":"fixed","expected_state":"` + ackState +
		`","ledger_seq":` + strconv.FormatUint(ackSeq, 10) + `}`
	status, closeFrag := c.post(t, "/api/incidents/"+c.inc+"/close", closeBody)
	if status != http.StatusOK {
		t.Fatalf("POST close after the ack = %d, want 200 — this is the TRBL-118 core-loop blocker (body %.600s)", status, closeFrag)
	}
	if !strings.Contains(closeFrag, `id="incident-rows"`) {
		t.Fatalf("the close did not answer with the refreshed incident-rows fragment: %.200s", closeFrag)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StResolved {
		t.Fatalf("ladder state after the close is %q, want resolved", string(got))
	}
	if look, ok := NewLookup(c.store.L).Incident(c.inc); !ok || look.State != types.StResolved {
		st := types.LadderState("<missing>")
		if ok {
			st = look.State
		}
		t.Fatalf("index state after the close is %q, want resolved", string(st))
	}

	// 6. The audit trail carries both human decisions with their state.
	bundle, err := c.store.L.Query().Evidence(c.inc, 200)
	if err != nil {
		t.Fatalf("Evidence(%s): %v", c.inc, err)
	}
	var ackRec, closeRec bool
	for _, rec := range bundle.Records {
		switch rec.Payload["transition"] {
		case "acknowledged":
			ackRec = true
			if got, _ := rec.Payload["state"].(string); got != string(types.StAcknowledged) {
				t.Errorf("the ack record's state payload is %q, want acknowledged", got)
			}
		case "closed":
			closeRec = true
			if got, _ := rec.Payload["state"].(string); got != string(types.StResolved) {
				t.Errorf("the close record's state payload is %q, want resolved", got)
			}
		}
	}
	if !ackRec || !closeRec {
		t.Fatalf("the ledger is missing the human decisions (ack=%t close=%t)", ackRec, closeRec)
	}

	// 7. Recurrence after the human close reopens the SAME incident: the ack/close
	//    path did not change the reopen behaviour (T42).
	res, err := c.l.Admit(context.Background(), ladder.Observation{
		EventID: "ev_01JAAPACKCLOSELOOP0002",
		TS:      types.FormatUTC(time.Now()),
		Sig:     c.sig,
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 93.0},
	})
	if err != nil {
		t.Fatalf("Admit (recurrence): %v", err)
	}
	if !res.Reopened || string(res.Inc) != c.inc {
		t.Fatalf("recurrence after a human close = %+v, want the same incident reopened", res)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StRecorded {
		t.Fatalf("state after the reopen is %q, want recorded", string(got))
	}
}

// TestAckIsIdempotentAndKeepsTheMachineClean pins the second half of the ack
// contract: acking an already-acknowledged incident is an AUDIT record (a
// re-stated hold), not a transition and not an illegal transition — three
// refusals would quarantine the incident (SPEC-05 §3.3/INV-6).
func TestAckIsIdempotentAndKeepsTheMachineClean(t *testing.T) {
	c := bootCoreLoop(t)

	status, body := c.post(t, "/api/incidents/"+c.inc+"/ack", `{"reason":"first","until":""}`)
	if status != http.StatusOK {
		t.Fatalf("first ack = %d, want 200 (%.400s)", status, body)
	}
	status, body = c.post(t, "/api/incidents/"+c.inc+"/ack", `{"reason":"second","until":""}`)
	if status != http.StatusOK {
		t.Fatalf("second ack = %d, want 200 — a re-ack is a record, not a refusal (%.400s)", status, body)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StAcknowledged {
		t.Fatalf("state after two acks is %q, want acknowledged", string(got))
	}
	// The close still works after the re-ack.
	ackState, ackSeq := fragmentCAS(t, c.inc, body)
	status, body = c.post(t, "/api/incidents/"+c.inc+"/close",
		`{"reason":"still fixed","resolution":"fixed","expected_state":"`+ackState+`","ledger_seq":`+strconv.FormatUint(ackSeq, 10)+`}`)
	if status != http.StatusOK {
		t.Fatalf("close after the re-ack = %d, want 200 (%.400s)", status, body)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StResolved {
		t.Fatalf("state after the close is %q, want resolved", string(got))
	}
}

// TestAckWithHoldThenCloseSucceeds pins the ack body the dashboard form actually
// sends: the `until` select defaults to `15m`, so the real click opens a
// suppression window for the sig (SPEC-05 §3.8) *after* the state moved to
// `acknowledged`. The hold must not stand between the operator and the close.
func TestAckWithHoldThenCloseSucceeds(t *testing.T) {
	c := bootCoreLoop(t)

	ack := `{"reason":"on it, hold the alerts","until":"15m"}`
	status, frag := c.post(t, "/api/incidents/"+c.inc+"/ack", ack)
	if status != http.StatusOK {
		t.Fatalf("ack with a 15m hold = %d, want 200 (%.400s)", status, frag)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StAcknowledged {
		t.Fatalf("state after the ack is %q, want acknowledged", string(got))
	}

	ackState, ackSeq := fragmentCAS(t, c.inc, frag)
	status, body := c.post(t, "/api/incidents/"+c.inc+"/close",
		`{"reason":"fixed","resolution":"fixed","expected_state":"`+ackState+`","ledger_seq":`+strconv.FormatUint(ackSeq, 10)+`}`)
	if status != http.StatusOK {
		t.Fatalf("close after an ack that carried a hold = %d, want 200 (%.400s)", status, body)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StResolved {
		t.Fatalf("state after the close is %q, want resolved", string(got))
	}
}

// TestCloseFromPreWorkStateStillRefused is AC-3's other half: the refusal a human
// gets for a state with no edge into `resolved` stays NAMED (409 +
// TROUBLE-LADDER-012 + no_close_edge), and the page no longer invites it.
func TestCloseFromPreWorkStateStillRefused(t *testing.T) {
	c := bootCoreLoop(t)

	status, page := c.get(t, "/incidents/"+c.inc)
	if status != http.StatusOK {
		t.Fatalf("GET /incidents/%s = %d, want 200", c.inc, status)
	}
	if strings.Contains(page, `action="/api/incidents/`+c.inc+`/close"`) {
		t.Fatal("the close control is rendered for a pre-work state")
	}
	// The server refuses the POST even when a client forges it.
	status, body := c.post(t, "/api/incidents/"+c.inc+"/close", `{"reason":"forced","resolution":"fixed"}`)
	if status != http.StatusConflict {
		t.Fatalf("close on a fresh incident = %d, want 409 (body %.400s)", status, body)
	}
	if !strings.Contains(body, `"no_close_edge"`) {
		t.Fatalf("the 409 does not name no_close_edge: %.400s", body)
	}
	if got := ladderState(t, c.l, c.inc); got != types.StRecorded {
		t.Fatalf("a refused close moved the state to %q", string(got))
	}
}
