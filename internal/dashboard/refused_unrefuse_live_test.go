package dashboard

// refused_unrefuse_live_test.go — TRBL-119: a token minted WHILE the dashboard
// boots refused must clear the refusal WITHOUT a restart.
//
// The in-process contract (TestTokenStoreRefusalClearsWhenTheFileAppears) mints
// through the SAME TokenStore object, which cannot reproduce the shipped failure:
// the daemon loads its store once and answers from refusedOnlyServer BEFORE the
// auth chain, so the per-request refresh that clears the signal never ran. With
// the CLI doing the mint in its own process, the daemon's memory is unchanged and
// every data route answered 503 (TROUBLE-DASHBOARD-013) forever.
//
// This test drives the real wrapper over a live HTTP server: refused boot ->
// 503-with-013 -> a SECOND store object writes the credential file (the CLI's
// process) -> the SAME server, no restart, answers the same token with 200, and a
// bogus token now reaches the auth chain and gets 401 rather than 503.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestRefusedWrapperServesAfterCliMintWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard-tokens.json") // must not exist

	cfg := DefaultConfig()
	cfg.TokenFile = path
	cfg.Bind = "127.0.0.1"

	s, err := newServer(cfg, refusedProbeDeps())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if !s.store.Refused() {
		t.Fatalf("premise: a missing token store must refuse at boot")
	}

	ts := httptest.NewServer(&refusedOnlyServer{server: s})
	t.Cleanup(ts.Close)

	get := func(token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/partials/health", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /partials/health: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(body)
	}

	// 1. The refused boot: the data plane answers 503 + 013/dashboard_refused.
	status, body := get("")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("data route on a refused boot = %d, want 503 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(types.CodeDashboard013)) || !strings.Contains(body, "dashboard_refused") {
		t.Fatalf("refusal does not name %s/dashboard_refused: %s", types.CodeDashboard013, body)
	}

	// 2. The CLI process mints into the SAME path: a separate TokenStore object,
	//    exactly what `trouble dashboard token create` does (Mint + Save in its own
	//    process). Nothing in this daemon's memory changes.
	cli, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore (cli): %v", err)
	}
	_, plain, err := cli.Mint("cli-mint@qa", []types.Scope{types.ScopeRead}, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := cli.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 3. NO restart: the very next request with the minted token must serve.
	status, body = get(plain)
	if status != http.StatusOK {
		t.Fatalf("minted-while-refused token = %d, want 200 without a restart (body: %s)", status, body)
	}

	// 4. The wrapper now routes into the auth chain, so a bad credential is a 401
	//    rather than the boot refusal — proof that the 200 above came from the
	//    ordinary route table and not from a still-open health-only surface.
	status, body = get("tdt_" + strings.Repeat("B", 43))
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown token after the un-refuse = %d, want 401 (body: %s)", status, body)
	}
}
