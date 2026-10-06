package dashboard

// tokens_missing_refusal_test.go is QA-TROUBLE-19's dashboard-package half:
// the missing token store is a REFUSAL SIGNAL (TokenStore.Refused), not a
// usable empty store. The dashboard package owns the detection and the
// fail-closed serve shape (health-only); internal/app owns the boot gate and
// the ledger audit (missing_token_store_refused_test.go).
//
// Before the refusal the missing store booted a fully serving dashboard
// (every route answering 401 + TROUBLE-DASHBOARD-002, loopback /health.json
// 200) behind a single WARN — the QA-TROUBLE-19 chaos-errorpath
// missing-config arm timed out against exactly that state.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestLoadTokenStoreMissingFileIsRefusalSignal pins the core semantic: a
// missing store loads as a valid empty set that CARRIES the refusal, and
// Refused() reports it. The store itself stays fail-closed (no token
// authenticates); the refusal signal is what lets the composition root gate
// the boot instead of serving an empty credential store.
func TestLoadTokenStoreMissingFileIsRefusalSignal(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "dashboard-tokens.json")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("premise: %s exists, so this load is not the missing-store case", missing)
	}

	store, err := LoadTokenStore(missing, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(missing): %v", err)
	}
	if !store.Refused() {
		t.Fatalf("a missing token store must carry the refusal signal: Refused() = false")
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("a refused store is still a VALID empty set (fail-closed, not invalid): got %v", err)
	}
	plain, _ := tokenFor(1)
	if _, ok := store.lookup(plain); ok {
		t.Fatalf("a token authenticated against the refused store — the store must stay empty")
	}

	// A store PRESENT with zero tokens is an operator mint that simply
	// produced nothing: usable, not a refusal (the mint path creates the
	// file before any boot needs it, so this state is reachable only by
	// hand-editing).
	empty := filepath.Join(dir, "empty.json")
	emptyStore, err := LoadTokenStore(empty, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(empty): %v", err)
	}
	if err := emptyStore.Save(); err != nil {
		t.Fatalf("materialize the present-but-empty store: %v", err)
	}
	emptyStore2, err := LoadTokenStore(empty, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(present-empty): %v", err)
	}
	if emptyStore2.Refused() {
		t.Fatalf("a present store with zero tokens must stay usable (empty, not refused)")
	}

	// An INVALID store (parse failure) keeps its own fail-closed path: it is
	// not the missing-store refusal either.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken store: %v", err)
	}
	brokenStore, err := LoadTokenStore(broken, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(broken): %v", err)
	}
	if brokenStore.Refused() {
		t.Fatalf("a parse-failed store must report invalid, not the missing-store refusal")
	}
	if brokenStore.invalid() == nil {
		t.Fatalf("a parse-failed store must stay invalid (fail-closed)")
	}
}

// TestTokenStoreRefusalClearsWhenTheFileAppears pins the no-op guarantee: the
// refusal signal describes THE BOOT-TIME STATE of the store. When a store
// appears at the path and the per-request refresh reads it, Refused() stops
// being true — the signal never wedges the store shut after the operator
// mints. (In the shipped daemon this matters only if a caller ignores the
// boot refusal; the signal must still be truthful.)
func TestTokenStoreRefusalClearsWhenTheFileAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard-tokens.json")

	store, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(missing): %v", err)
	}
	if !store.Refused() {
		t.Fatalf("premise: the missing store carries no refusal signal")
	}

	if _, _, err := store.Mint("late-mint@qa", []types.Scope{types.ScopeRead}, time.Now()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	store.refresh(time.Now())
	if store.Refused() {
		t.Fatalf("Refused() stayed true after the store file appeared: the signal must track the boot-time state only")
	}
}

// refusedProbeDeps is the smallest Deps the health-only serve needs.
func refusedProbeDeps() Deps {
	return Deps{
		Index:  newFakeIndex(),
		Lookup: newFakeLookup(),
		Health: func(ctx context.Context) types.HealthResponse {
			return types.HealthResponse{
				Status: "degraded",
				Detail: map[string]any{"subsystem_refused": string(types.CodeLifecycle001), "subsystem": "dashboard"},
				Subsystems: []types.SubsystemHealth{
					{Name: "dashboard", Refused: true,
						Code:   string(types.CodeLifecycle001),
						Reason: string(types.CodeLifecycle001) + ": dashboard token store missing: refused"},
				},
			}
		},
	}
}

// TestRefusedOnlyServesHealthAndRefusesEverythingElse is the QA-TROUBLE-19
// defeat, inverted: a boot with a missing token store must NOT answer the
// unauthenticated data plane. The health-only server answers GET
// /health.json (the one surface the external stall checker consumes, §6
// edge 8) and refuses every OTHER route with 503 + TROUBLE-DASHBOARD-013,
// before any credential evaluation.
func TestRefusedOnlyServesHealthAndRefusesEverythingElse(t *testing.T) {
	s, err := newServer(DefaultConfig(), refusedProbeDeps())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ro := &refusedOnlyServer{server: s}
	ts := httptest.NewServer(ro)
	t.Cleanup(ts.Close)

	// The health surface answers: this is what keeps the §3 watchdog's
	// TROUBLE-LIFECYCLE-008 arm (health.json unreachable) from misreading a
	// refused dashboard as a dead daemon.
	resp, err := http.Get(ts.URL + "/health.json")
	if err != nil {
		t.Fatalf("GET /health.json: %v", err)
	}
	hb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s, want 200 on the health-only server", resp.StatusCode, hb)
	}
	if !strings.Contains(string(hb), `"degraded"`) {
		t.Errorf("health-only serve status is not degraded: %s", hb)
	}

	// Every other route — page, partial — refuses before authentication:
	// the data plane stays closed even for a valid token, because the boot
	// refused the credential store that would have authorized it.
	for _, path := range []string{"/", "/incidents", "/partials/health", "/groups"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503 on the health-only server (body: %s)", path, resp.StatusCode, body)
			continue
		}
		eb := decodeError(t, string(body))
		if eb.Error.Code != string(types.CodeDashboard013) || eb.Detail != "dashboard_refused" {
			t.Errorf("GET %s refusal = %s/detail:%s, want %s/dashboard_refused", path, eb.Error.Code, eb.Detail, types.CodeDashboard013)
		}
	}
}

// TestRefusedOnlyRejectsTheDataPlaneWithAToken: even a PRESENTED token gets
// the 503, not a 401/200 — the refusal is about the boot, not about the
// caller. This is the literal inversion of the defeated state, where an
// unauthenticated dashboard was silently serving.
func TestRefusedOnlyRejectsTheDataPlaneWithAToken(t *testing.T) {
	s, err := newServer(DefaultConfig(), refusedProbeDeps())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ro := &refusedOnlyServer{server: s}
	ts := httptest.NewServer(ro)
	t.Cleanup(ts.Close)

	plain, _ := tokenFor(1)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/incidents", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /incidents with a token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /incidents with a valid token = %d %s, want 503: the data plane must stay closed after a refused boot", resp.StatusCode, body)
	}
	eb := decodeError(t, string(body))
	if eb.Error.Code != string(types.CodeDashboard013) || eb.Detail != "dashboard_refused" {
		t.Errorf("refusal = %s/detail:%s, want %s/dashboard_refused", eb.Error.Code, eb.Detail, types.CodeDashboard013)
	}
}

// TestTokenlessRefusedDashboardBootsToHealthOnly is the dashboard-package arm
// of the QA-TROUBLE-19 regression WITHOUT the composition root: a server
// built over a store whose file is missing at boot still serves /health.json
// and refuses the data plane. The composition-root arm (real boot, ledger
// audit) lives in internal/app.
func TestTokenlessRefusedDashboardBootsToHealthOnly(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "dashboard-tokens.json")

	deps := refusedProbeDeps()
	deps.TokenFile = missing
	s, err := newServer(DefaultConfig(), deps)
	if err != nil {
		t.Fatalf("newServer over a missing token store: %v", err)
	}
	if !s.store.Refused() {
		t.Fatalf("premise: the loaded store carries no refusal signal")
	}
	ro := &refusedOnlyServer{server: s}
	ts := httptest.NewServer(ro)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/health.json")
	if err != nil {
		t.Fatalf("GET /health.json: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health.json = %d, want 200 over the health-only serve of a refused boot", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/incidents")
	if err != nil {
		t.Fatalf("GET /incidents: %v", err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /incidents = %d %s, want 503", resp2.StatusCode, b2)
	}
}
