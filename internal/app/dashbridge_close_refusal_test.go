package app

// dashbridge_close_refusal_test.go — TRBL-REVIEW-CLOSE-ERR: the production
// close path, end to end. A human close of an incident the frozen state
// machine cannot resolve is a NAMED refusal: 409 + TROUBLE-LADDER-012 +
// detail no_close_edge + the reason — never the generic 500 action_failed
// (SPEC-10 §5/§7.6: `error.code` is the owning subsystem's code when a write
// seam exposes one). The test boots the real ladder over the real ledger and
// drives the real dashboard server, exactly the cmd/troubled wiring.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/ladder"
	"github.com/trouble-agent/trouble/internal/types"
)

// bootCloseDashboard is the production wiring in miniature: real store, real
// ladder, real bridge (NewActions/NewLookup), real dashboard server on a
// scratch loopback listener with one minted write token.
type closeDashBoot struct {
	base, token, incID string
	lb                 *ladder.Ladder
	s                  *Store
}

func bootLadder(t *testing.T, s *Store) *ladder.Ladder {
	t.Helper()
	rules := map[string]types.Rule{
		"io-pressure": {
			Name: "io-pressure", Enabled: true, Source: types.SigSource("psi"),
			EntryRung: types.RungPlay, Severity: types.SevHigh,
			Match: []types.Condition{{Field: "some_avg10", Op: ">=", Value: "50", ValueType: "number"}},
		},
	}
	lb, err := ladder.New(ladder.Deps{
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
	return lb
}

func bootCloseDashboard(t *testing.T) closeDashBoot {
	t.Helper()
	s, l := testStore(t)
	ctx := context.Background()

	lb := bootLadder(t, s)

	// Admit one incident through the ladder's own T01 edge: the incident sits
	// in `recorded`, a state with no human-close edge (T38/T45 are the only
	// edges into `resolved`).
	res, err := lb.Admit(ctx, ladder.Observation{
		EventID: "ev_01JAAPPWIRINGCLOSE0001",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("close-refusal-digest-0001")),
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 91.0},
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if res.Inc == "" {
		t.Fatalf("Admit returned no incident id (%+v)", res)
	}

	// One write token, minted the way the daemon mints them, in a scratch dir.
	tokenPath := filepath.Join(t.TempDir(), "close-dash-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("dash-write@e2e", []types.Scope{types.ScopeWrite}, time.Now())
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
		Index:  l.DashReader(),
		Lookup: NewLookup(l),
		Story: func(inc string) dashboard.Story {
			if row, ok := NewLookup(l).Incident(inc); ok {
				return NewLookup(l).Story(*row)
			}
			return dashboard.Story{}
		},
		Health:    func(ctx context.Context) types.HealthResponse { return types.HealthResponse{} },
		Actions:   NewActions(lb, s),
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
		_ = l.Close(context.Background())
	})
	return closeDashBoot{
		base:  "http://" + ln.Addr().String(),
		token: plaintext,
		incID: string(res.Inc),
		lb:    lb,
		s:     s,
	}
}

// TestCloseNonClosableIncidentSurfacesLadderRefusal is the wire contract: the
// refusal a human gets names the ladder code, the no_close_edge detail and the
// reason, with 409 Conflict — never the generic 500 action_failed.
func TestCloseNonClosableIncidentSurfacesLadderRefusal(t *testing.T) {
	b := bootCloseDashboard(t)
	base, token, inc := b.base, b.token, b.incID

	req, err := http.NewRequest(http.MethodPost, base+"/api/incidents/"+inc+"/close",
		strings.NewReader(`{"reason":"human close","resolution":"fixed"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST close: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("close on a non-closable incident = 500 with body %s: the named refusal was swallowed by the generic mapping", body)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", resp.StatusCode, body)
	}
	var eb struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("decode body %s: %v", body, err)
	}
	if eb.Error.Code != string(types.CodeLadder012) {
		t.Fatalf("error.code = %s, want %s", eb.Error.Code, types.CodeLadder012)
	}
	if eb.Detail != "no_close_edge" {
		t.Fatalf("detail = %q, want no_close_edge", eb.Detail)
	}
	if !strings.Contains(eb.Error.Message, "no legal edge to resolved") {
		t.Fatalf("message = %q, want the refusal reason", eb.Error.Message)
	}
}

// TestAdaptSeamGuardRefusal pins the adapter half of the same contract: a
// ladder guard refusal that rejected a write the human asked for (here a
// verifying close without a passing evidence window: T38 refuses with
// LADDER-006) maps to a refused transition — 409 + code + reason on the wire
// — instead of the generic 500.
func TestAdaptSeamGuardRefusal(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	rules := map[string]types.Rule{
		"io-pressure": {
			Name: "io-pressure", Enabled: true, Source: types.SigSource("psi"),
			EntryRung: types.RungPlay, Severity: types.SevHigh,
			Match: []types.Condition{{Field: "some_avg10", Op: ">=", Value: "50", ValueType: "number"}},
		},
	}
	lb, err := ladder.New(ladder.Deps{
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

	first, err := lb.Admit(ctx, ladder.Observation{
		EventID: "ev_01JAAPPWIRINGGUARD0001",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("guard-digest-0001")),
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 91.0},
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	_ = first
	sig := types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("guard-digest-0001")).String()

	// A human hold (the ack seam's Suppress call) opens the sig's suppression
	// window, so the NEXT admission of the same sig lands straight in
	// `suppressed` (T02) — the state whose human-close edge is T45.
	if _, err := lb.Suppress(ctx, sig, types.Duration("1h"), "human hold"); err != nil {
		t.Fatalf("Suppress: %v", err)
	}
	res2, err := lb.Admit(ctx, ladder.Observation{
		EventID: "ev_01JAAPPWIRINGGUARD0002",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("guard-digest-0002")),
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 92.0},
	})
	if err != nil {
		t.Fatalf("Admit (suppressed): %v", err)
	}
	if cur, err := lb.Incident(ctx, res2.Inc); err != nil || cur.State != types.StSuppressed {
		st := ""
		if err == nil {
			st = string(cur.State)
		}
		t.Fatalf("state = %q (err %v), want suppressed (T02)", st, err)
	}

	actions := NewActions(lb, s)
	actor := types.Actor{Kind: types.ActorHuman, ID: "dash-write@e2e"}

	// The human close now hits the T45 canary guard (no evidence tuple: quiet
	// is only accepted when it was observed). The bridge must surface the
	// ladder's refusal, not a 500.
	err = actions.Close(ctx, string(res2.Inc), actor, "human close", "fixed")
	if err == nil {
		t.Fatal("Close succeeded: T38 refuses a verifying close without a passing window")
	}
	var le *ladder.Error
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a ladder *Error", err)
	}
	if le.Code != types.CodeLadder006 {
		t.Fatalf("code = %s, want %s (no evidence tuple)", le.Code, types.CodeLadder006)
	}
	seam := adaptSeam(err)
	var ae interface {
		ErrorCode() types.ErrorCode
		ErrorDetail() string
		ErrorReason() string
		RefusedTransition() bool
	}
	if !errors.As(seam, &ae) {
		t.Fatalf("adaptSeam(%v) = %T, want a ladderSeamError carrying the seam shape", err, seam)
	}
	if ae.ErrorCode() != types.CodeLadder006 {
		t.Fatalf("code = %s, want %s", ae.ErrorCode(), types.CodeLadder006)
	}
	if !ae.RefusedTransition() {
		t.Fatal("RefusedTransition = false, want true (the guard rejected the write)")
	}
	if ae.ErrorReason() == "" {
		t.Fatal("reason empty: the refusal must carry the human cause")
	}
}

// TestCloseLedgerOnlyIncidentSurfacesTypedRefusal is the TRBL-144 dogfood
// reproduction, on the production wiring. The dogfood incident had been
// admitted in a PREVIOUS ladder lifetime (ladder.New replays nothing; the
// in-memory table is empty at boot), so the dashboard's ledger-backed lookup
// still displayed it — a close passed target validation, then the bridge's
// Ladder.Incident call refused with the ladder's own "not in the index"
// error, which reached the dashboard UN-adapted and collapsed into the
// generic 500 action_failed (TROUBLE-DASHBOARD-013) in 0.02s. After the fix
// every seam path adapts (and *ladder.Error carries the seam shape itself):
// close AND ack answer 409 + TROUBLE-LADDER-012 with the ladder's detail.
func TestCloseLedgerOnlyIncidentSurfacesTypedRefusal(t *testing.T) {
	b := bootCloseDashboard(t)

	// Admit a SECOND incident through a fresh ladder instance over the same
	// store: the ledger (and therefore the dashboard's lookup) holds the
	// incident, the serving ladder's in-memory table deliberately does not.
	lb2 := bootLadder(t, b.s)
	res, err := lb2.Admit(context.Background(), ladder.Observation{
		EventID: "ev_01JAAPPWIRINGLEDGER0001",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("ledger-digest-0001")),
		InKey:   "psi|io-pressure|host2",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 93.0},
	})
	if err != nil {
		t.Fatalf("Admit (second lifetime): %v", err)
	}
	if res.Inc == "" {
		t.Fatalf("Admit returned no incident id (%+v)", res)
	}
	ledgerInc := string(res.Inc)

	if _, ok := NewLookup(b.s.L).Incident(ledgerInc); !ok {
		t.Fatalf("fixture broken: the dashboard lookup cannot see %s in the ledger", ledgerInc)
	}

	// The close route: never the generic 500 — the refusal is the ladder's
	// own code.
	resp, body := postJSON(t, b, "/api/incidents/"+ledgerInc+"/close", `{"reason":"human close","resolution":"fixed"}`)
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("close on a ledger-only incident = the generic 500 (%.300s): the raw ladder error collapsed into action_failed", body)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %.300s)", resp.StatusCode, body)
	}
	assertLadder012Refusal(t, body, "ladder_012")

	// The ack route must carry the same typed code class.
	resp, body = postJSON(t, b, "/api/incidents/"+ledgerInc+"/ack", `{"reason":"operator check"}`)
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("ack on a ledger-only incident = the generic 500 (%.300s): same seam contract violation on the sibling route", body)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ack status = %d, want 409 (body %.300s)", resp.StatusCode, body)
	}
	assertLadder012Refusal(t, body, "ladder_012")
}

// postJSON POSTs one JSON body with the boot's write token.
func postJSON(t *testing.T, b closeDashBoot, path, payload string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

// assertLadder012Refusal checks the typed wire body: the ladder's code, its
// machine-readable detail and the human cause in the message.
func assertLadder012Refusal(t *testing.T, body, wantDetail string) {
	t.Helper()
	var eb struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("decode body %.300s: %v", body, err)
	}
	if eb.Error.Code != string(types.CodeLadder012) {
		t.Fatalf("error.code = %s, want %s (body %.300s)", eb.Error.Code, types.CodeLadder012, body)
	}
	if eb.Detail != wantDetail {
		t.Fatalf("detail = %q, want %q", eb.Detail, wantDetail)
	}
	if !strings.Contains(eb.Error.Message, "not in the index") {
		t.Fatalf("message = %q, want the ladder's human cause", eb.Error.Message)
	}
}
