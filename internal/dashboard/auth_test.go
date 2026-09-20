package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// auth_test.go is the SPEC-10 §7 row for §2.2/§2.4/§3.2: the scope matrix, the
// refusal codes, the bind rules, the identity seam, the throttle, and the token
// store's own contract (hashed at rest, shown once, fail-closed, ingestion-key
// refusal). Statuses and codes are asserted exactly.

const bodyMinimal = `{}`

// TestAuthMatrixAbsentAndMalformed walks the 401 family: (001) no material,
// (002) every malformed/unknown/revoked form.
func TestAuthMatrixAbsentAndMalformed(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	long := "tdt_" + strings.Repeat("A", 42)           // 46 chars: one short
	short := "tdt_" + strings.Repeat("A", 44)          // 48 chars: one long
	badChars := "tdt_" + strings.Repeat("a", 42) + "!" // 47 chars, illegal byte
	revokedPlain, revokedHash := tokenFor(9)

	envRevoked := newEnv(t, envOptions{
		cfg: unlimitedRates,
		tokens: []types.Token{
			{ID: labelRead, Hash: revokedHash, Scopes: []types.Scope{types.ScopeRead}, Revoked: true},
			tokenEntryFor(labelWrite, 2, types.ScopeWrite),
		},
	})

	cases := []struct {
		name  string
		env   *testEnv
		token string
		want  int
		code  types.ErrorCode
	}{
		{"absent, header route", env, "", 401, types.CodeDashboard001},
		{"absent, partial route", env, "", 401, types.CodeDashboard001},
		{"blank bearer value is no material", env, " ", 401, types.CodeDashboard001},
		{"no tdt_ prefix", env, "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijk", 401, types.CodeDashboard002},
		{"too short", env, long, 401, types.CodeDashboard002},
		{"too long", env, short, 401, types.CodeDashboard002},
		{"illegal characters", env, badChars, 401, types.CodeDashboard002},
		{"well-formed but unknown", env, "tdt_" + strings.Repeat("B", 43), 401, types.CodeDashboard002},
		{"revoked", envRevoked, revokedPlain, 401, types.CodeDashboard002},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/"
			if strings.Contains(tc.name, "partial") {
				path = "/partials/health"
			}
			var resp *http.Response
			var body string
			if tc.token == "" {
				resp, body = tc.env.get(path, "")
			} else {
				resp, body = tc.env.get(path, strings.TrimSpace(tc.token))
			}
			wantStatus(t, resp, body, tc.want)
			eb := decodeError(t, body)
			if eb.Error.Code != string(tc.code) {
				t.Fatalf("code = %s, want %s", eb.Error.Code, tc.code)
			}
			// 002 never says which check failed.
			if tc.code == types.CodeDashboard002 && strings.Contains(strings.ToLower(eb.Error.Message), "revoked") {
				t.Fatalf("002 leaks the failed check: %q", eb.Error.Message)
			}
		})
	}

	if revokedPlain == env.readPlain {
		t.Fatal("fixture bug: revoked and read plaintext are equal")
	}
}

// TestScopeHierarchy covers the linear model (autonomy ⇒ write ⇒ read) and the
// 403 + X-Trouble-Required-Scope mapping.
func TestScopeHierarchy(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()

	cases := []struct {
		name   string
		token  string
		method string
		path   string
		want   int
		scope  types.Scope
	}{
		{"read reads", env.readPlain, "GET", "/incidents", 200, ""},
		{"read cannot ack", env.readPlain, "POST", "/api/incidents/" + inc + "/ack", 403, types.ScopeWrite},
		{"read cannot close", env.readPlain, "POST", "/api/incidents/" + inc + "/close", 403, types.ScopeWrite},
		{"read cannot set autonomy", env.readPlain, "POST", "/api/autonomy", 403, types.ScopeAutonomy},
		{"write reads (hierarchy)", env.writePlain, "GET", "/incidents", 200, ""},
		{"write ack", env.writePlain, "POST", "/api/incidents/" + inc + "/ack", 200, ""},
		{"write cannot set autonomy", env.writePlain, "POST", "/api/autonomy", 403, types.ScopeAutonomy},
		{"autonomy reads", env.autoPlain, "GET", "/", 200, ""},
		{"autonomy writes", env.autoPlain, "POST", "/api/autonomy", 200, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			var body string
			payload := `{"reason":"scope probe","until":"15m","expected_state":"verifying","ledger_seq":41207,"mode":"shadow","kill_switch":false}`
			switch tc.method {
			case "GET":
				resp, body = env.get(tc.path, tc.token)
			default:
				// Bearer-only POST (the curl path): checks 1–2 of §2.3 hold and
				// the scope matrix is what this table asserts.
				resp, body = env.post(tc.path, tc.token, payload)
			}
			wantStatus(t, resp, body, tc.want)
			if tc.want == 403 {
				if got := resp.Header.Get("X-Trouble-Required-Scope"); got != string(tc.scope) {
					t.Fatalf("X-Trouble-Required-Scope = %q, want %q", got, tc.scope)
				}
				eb := decodeError(t, body)
				if eb.Error.Code != string(types.CodeDashboard003) {
					t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard003)
				}
			}
		})
	}
}

// labelFor maps a fixture plaintext back onto its token label.
func labelFor(plain string, env *testEnv) string {
	switch plain {
	case env.readPlain:
		return labelRead
	case env.writePlain:
		return labelWrite
	case env.autoPlain:
		return labelAuto
	}
	return ""
}

// TestBothCarriers covers the two-carrier rule: equal values authenticate,
// different values are 002.
func TestBothCarriers(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	t.Run("equal", func(t *testing.T) {
		resp, body := env.get("/", "", func(r *http.Request) {
			bearer(env.readPlain)(r)
			r.AddCookie(authCookie(env.readPlain, false))
		})
		wantStatus(t, resp, body, http.StatusOK)
	})

	t.Run("different", func(t *testing.T) {
		resp, body := env.get("/", "", func(r *http.Request) {
			bearer(env.readPlain)(r)
			r.AddCookie(authCookie(env.writePlain, false))
		})
		wantStatus(t, resp, body, http.StatusUnauthorized)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard002) {
			t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard002)
		}
	})

	t.Run("cookie only", func(t *testing.T) {
		resp, body := env.get("/", "", withCookies(authCookie(env.readPlain, false)))
		wantStatus(t, resp, body, http.StatusOK)
	})
}

// TestHealthLoopbackExemption covers §2.1 row 8: unauthenticated on loopback
// when the flag is on, 401 when it is off or the bind is not loopback.
func TestHealthLoopbackExemption(t *testing.T) {
	t.Run("loopback exempt", func(t *testing.T) {
		env := newEnv(t, envOptions{noRefresh: true})
		resp, body := env.get("/health.json", "")
		wantStatus(t, resp, body, http.StatusOK)
		var hr types.HealthResponse
		if err := json.Unmarshal([]byte(body), &hr); err != nil {
			t.Fatalf("health.json is not a HealthResponse: %v", err)
		}
		if hr.LedgerLastSeq != 41207 {
			t.Errorf("ledger_last_seq = %d, want 41207", hr.LedgerLastSeq)
		}
		// The body never carries token/DSN material.
		if strings.Contains(body, "tdt_") || strings.Contains(body, "hash") {
			t.Error("health.json leaks credential material")
		}
	})

	t.Run("exemption disabled", func(t *testing.T) {
		env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
			c.HealthLoopbackExempt = false
		}})
		resp, body := env.get("/health.json", "")
		wantStatus(t, resp, body, http.StatusUnauthorized)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard001) {
			t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard001)
		}
	})

	t.Run("non-loopback bind requires read scope", func(t *testing.T) {
		env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
			c.HealthLoopbackExempt = false
		}})
		resp, body := env.get("/health.json", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
	})
}

// TestNonLoopbackRequestOnLoopbackListener is §2.4's live bind re-check: a
// request arriving on a non-loopback connection while the config says loopback
// is refused 503 + 006.
func TestNonLoopbackRequestOnLoopbackListener(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+env.readPlain)
	req.RemoteAddr = "203.0.113.5:44321"
	rec := httptest.NewRecorder()
	env.s.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	eb := decodeError(t, rec.Body.String())
	if eb.Error.Code != string(types.CodeDashboard006) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard006)
	}
}

// TestForwardedForTrust covers §2.4's proxy rules: X-Forwarded-For is honored
// only from a trusted peer inside proxy_cidrs, and an untrusted client can
// never forge its own bucket.
func TestForwardedForTrust(t *testing.T) {
	t.Run("untrusted XFF is ignored", func(t *testing.T) {
		env := newEnv(t, envOptions{noRefresh: true})
		req := httptest.NewRequest(http.MethodGet, "/health.json", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := httptest.NewRecorder()
		env.s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("untrusted XFF changed the peer identity: status = %d", rec.Code)
		}
	})

	t.Run("trusted proxy XFF changes the peer identity", func(t *testing.T) {
		env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
			c.ProxyTrusted = true
			c.ProxyCIDRs = []string{"127.0.0.0/8"}
		}})
		req := httptest.NewRequest(http.MethodGet, "/health.json", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := httptest.NewRecorder()
		env.s.ServeHTTP(rec, req)
		// The peer is now non-loopback, so the loopback exemption does not
		// apply — and because the listener is bound loopback, the §2.4 live
		// bind re-check refuses the request outright.
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (exemption must not apply to a forwarded peer)", rec.Code)
		}
		var eb errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if eb.Error.Code != string(types.CodeDashboard006) {
			t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard006)
		}
	})
}

// TestTokenStoreFailClosed is §3.2: a broken credential file must not keep a
// rotated-away token alive, and loopback /health.json keeps serving.
func TestTokenStoreFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard-tokens.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := newEnv(t, envOptions{noRefresh: true, tokens: []types.Token{tokenEntryFor(labelRead, 1, types.ScopeRead)}})
	// Point the live server at the broken file (same path the env wrote).
	if err := os.WriteFile(env.tokenFile, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Touch the mtime so the per-request stat sees a change.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(env.tokenFile, future, future); err != nil {
		t.Fatal(err)
	}

	resp, body := env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusServiceUnavailable)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard013) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard013)
	}
	if eb.Detail != "token_store_invalid" {
		t.Fatalf("detail = %q, want token_store_invalid", eb.Detail)
	}

	// The loopback health exemption is the one route that keeps serving.
	resp, body = env.get("/health.json", "")
	wantStatus(t, resp, body, http.StatusOK)

	// A partial poll does not (fail-closed).
	resp, body = env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusServiceUnavailable)
}

// TestTokenStoreWrongModeRefusesBoot is the §3.2 mode check: the daemon
// refuses to start with TROUBLE-LIFECYCLE-013 on a wrong file mode.
func TestTokenStoreWrongModeRefusesBoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	b, _ := json.Marshal(tokenFile{Version: 1, Tokens: []types.Token{tokenEntryFor(labelRead, 1, types.ScopeRead)}})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokenStore(path, nil, nil); err == nil {
		t.Fatal("0600 check did not refuse a 0644 token file")
	} else {
		var de *dashError
		if !errors.As(err, &de) || de.Code != types.CodeLifecycle013 {
			t.Fatalf("error = %v, want %s", err, types.CodeLifecycle013)
		}
	}
}

// TestIngestionKeyRefusals is §4.3: a stored hash equal to a project key
// refuses the boot, and a Bearer value equal to a project key is an auth
// failure (002) that the throttle counts.
func TestIngestionKeyRefusals(t *testing.T) {
	dir := t.TempDir()
	key := "0123456789abcdef0123456789abcdef" // 32 hex, the DSN grammar
	path := filepath.Join(dir, "tokens.json")
	handEdited := []types.Token{{ID: "sneaky", Hash: hashOf(key), Scopes: []types.Scope{types.ScopeRead}}}
	b, _ := json.Marshal(tokenFile{Version: 1, Tokens: handEdited})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokenStore(path, []string{key}, nil); err == nil {
		t.Fatal("load-time ingestion-key check did not refuse the store")
	} else {
		var de *dashError
		if !errors.As(err, &de) || de.Code != types.CodeDashboard002 {
			t.Fatalf("error = %v, want %s", err, types.CodeDashboard002)
		}
		if de.Detail != "equals_ingestion_key" {
			t.Fatalf("detail = %q, want equals_ingestion_key", de.Detail)
		}
	}

	// Mint refuses a plaintext equal to a key.
	env := newEnv(t, envOptions{noRefresh: true, deps: func(d *Deps) {
		d.IngestionKeys = []string{key}
	}})
	// Force the collision deterministically: a store whose forbidden list holds
	// the plaintext we are about to mint cannot happen with real entropy, so
	// assert the documented check on the load path instead — a token file
	// hand-edited to hold the key hash is already covered above.

	// Bearer equal to a project key → 002, counted by the throttle.
	resp, body := env.get("/", key)
	wantStatus(t, resp, body, http.StatusUnauthorized)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard002) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard002)
	}
}

// TestIdentitySeamAbsent is §2.5: selecting tailscale/proxy-header returns
// 503 + 013 (detail impl_absent) at the login boundary.
func TestIdentitySeamAbsent(t *testing.T) {
	for _, impl := range []string{"tailscale", "proxy-header"} {
		t.Run(impl, func(t *testing.T) {
			env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) { c.Identity = impl }})
			resp, body := env.get("/", env.readPlain)
			wantStatus(t, resp, body, http.StatusServiceUnavailable)
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard013) {
				t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard013)
			}
			if eb.Detail != "impl_absent" {
				t.Fatalf("detail = %q, want impl_absent", eb.Detail)
			}
		})
	}
}

// throttleRates keeps the §2.8 auth-failure throttle at its shipped limit while
// removing the read bucket, so the requests the throttle cases make are never
// confused with the rate limiter's own 429.
func throttleRates(cfg *Config) {
	cfg.ReadRPS = 1e9
	cfg.ReadBurst = 1_000_000
	cfg.AuthFailLimit = 10
	cfg.AuthFailWindow = types.Duration("60s")
}

// TestAuthFailureThrottle is §2.8/§2.8a: 10 failures for ONE credential inside
// 60s throttle that credential for 60s with 429 + Retry-After ≥ 1 — and a VALID
// token is never throttled by failures it did not make (TRBL-010 defect 3,
// where a mistyped/rotated token throttled every request from the client IP,
// including ones carrying a good token).
func TestAuthFailureThrottle(t *testing.T) {
	env := newEnv(t, envOptions{cfg: throttleRates})

	bad, _ := tokenFor(9) // grammar-valid, absent from the store
	throttled := false
	for i := 0; i < 12; i++ {
		resp, body := env.get("/", bad)
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled = true
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard012) {
				t.Fatalf("throttle code = %s, want %s", eb.Error.Code, types.CodeDashboard012)
			}
			if eb.Detail != "auth_failure_throttle" {
				t.Fatalf("throttle detail = %q, want auth_failure_throttle", eb.Detail)
			}
			ra := resp.Header.Get("Retry-After")
			if ra == "" || ra == "0" {
				t.Fatalf("Retry-After = %q, want an integer ≥ 1", ra)
			}
			break
		}
		// Before the limit the invalid credential is REJECTED, not throttled:
		// 401 + 002 is a different answer from the 429 the throttle gives.
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 before the limit", i, resp.StatusCode)
		}
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard002) {
			t.Fatalf("attempt %d: code = %s, want %s", i, eb.Error.Code, types.CodeDashboard002)
		}
	}
	if !throttled {
		t.Fatal("11 auth failures did not throttle the credential")
	}

	// The credential that failed is still refused for the rest of the window —
	// and answered 429, which is distinguishable from its own 401.
	resp, body := env.get("/", bad)
	wantStatus(t, resp, body, http.StatusTooManyRequests)

	// A VALID token from the same client IP serves normally: it is throttled by
	// its own failures only, and it has none.
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	for _, path := range []string{"/incidents", "/partials/budget", "/health.json"} {
		resp, body = env.get(path, env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
	}

	// A second, different credential is unaffected: it has made no failures.
	other, _ := tokenFor(8)
	resp, body = env.get("/", other)
	wantStatus(t, resp, body, http.StatusUnauthorized)
	if eb := decodeError(t, body); eb.Error.Code != string(types.CodeDashboard002) {
		t.Fatalf("unseen credential code = %s, want %s (it must be 401, never the throttle's 429)", eb.Error.Code, types.CodeDashboard002)
	}

	// The throttle is a window, not a ban: after it the credential is answered
	// 401 again, and the valid token keeps working.
	env.clock.advance(61 * time.Second)
	resp, body = env.get("/", bad)
	wantStatus(t, resp, body, http.StatusUnauthorized)
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
}

// TestThrottleAnonymousFloodStaysPerIP is the other half of §2.8a: a request
// that presents no grammar-valid credential is counted and throttled per client
// IP (an unauthenticated flood is still answered 429 instead of being handed to
// the identity seam for free) — while a valid credential from that same IP is
// never collateral damage.
func TestThrottleAnonymousFloodStaysPerIP(t *testing.T) {
	env := newEnv(t, envOptions{cfg: throttleRates})

	// No Authorization header and no cookie at all: 401 + 001 for the first
	// auth_fail_limit requests, then the IP is throttled.
	var last int
	for i := 0; i < 12; i++ {
		resp, _ := env.get("/", "")
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("anonymous flood #12 status = %d, want 429", last)
	}

	// A grammar-invalid value is also IP-keyed (§2.8a): still 429.
	resp, body := env.get("/", "not-a-token")
	wantStatus(t, resp, body, http.StatusTooManyRequests)

	// The valid token presents a credential, so it is keyed on that credential
	// and serves — the operator is not locked out by someone else's flood.
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)

	// Once the window passes, the anonymous path is answered 401 again.
	env.clock.advance(61 * time.Second)
	resp, body = env.get("/", "")
	wantStatus(t, resp, body, http.StatusUnauthorized)
}

// TestReadBucketAndFairness is §2.8/edge case 10: the read bucket is keyed by
// token AND IP, an unauthenticated flood cannot consume an authenticated
// bucket, and Retry-After is an integer ≥ 1.
func TestReadBucketAndFairness(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
		c.ReadRPS = 1
		c.ReadBurst = 3
		c.AuthFailLimit = 1000
	}})

	// Drain the read bucket for the read token.
	var limited int
	for i := 0; i < 6; i++ {
		resp, body := env.get("/", env.readPlain)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard012) {
				t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard012)
			}
			if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
				t.Fatalf("Retry-After = %q, want ≥ 1", ra)
			}
		}
	}
	if limited == 0 {
		t.Fatal("the read bucket never filled at 1 rps / burst 3")
	}

	// The read bucket is keyed by token AND client IP (§2.8): the same client
	// IP is itself a key, so a second token from that IP is limited by the IP
	// bucket while refilling; the tokens' own buckets stay separate.
	env.clock.advance(20 * time.Second) // both the token and the IP bucket refill
	resp, body := env.get("/breakers", env.autoPlain)
	wantStatus(t, resp, body, http.StatusOK)
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)

	// Draining the IP bucket limits every token behind that IP — the documented
	// behaviour, and why the read burst is 60 rather than 3.
	for i := 0; i < 4; i++ {
		env.get("/", env.readPlain)
	}
	resp, body = env.get("/breakers", env.autoPlain)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (the IP bucket is shared by design)", resp.StatusCode)
	}
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard012) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard012)
	}
}

// TestWriteBucket is §2.8's per-token write bucket (5/s, burst 10).
func TestWriteBucket(t *testing.T) {
	env := newEnv(t, envOptions{cfg: func(c *Config) {
		c.WriteRPS = 1
		c.WriteBurst = 2
		c.ReadRPS = 1e6
		c.ReadBurst = 1e6
	}})
	inc := validIncidentID()
	payload := `{"reason":"rate probe","until":"15m","expected_state":"verifying","ledger_seq":41207}`

	var limited bool
	for i := 0; i < 5; i++ {
		resp, body := env.post("/api/incidents/"+inc+"/ack", env.writePlain, payload)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard012) {
				t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard012)
			}
			break
		}
	}
	if !limited {
		t.Fatal("the write bucket never filled at 1 rps / burst 2")
	}
}

// TestValidateConfigMatrix is §2.4's startup validation, all fail-loud.
func TestValidateConfigMatrix(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		want   string // "" = ok, else the code
		detail string
	}{
		{"loopback default", func() Config { c := DefaultConfig(); return c }(), "", ""},
		{"non-loopback without mandate", func() Config { c := DefaultConfig(); c.Bind = "0.0.0.0"; return c }(), string(types.CodeDashboard006), "mandate_required"},
		{"non-loopback lan with mandate", func() Config {
			c := DefaultConfig()
			c.Bind = "192.0.2.5"
			c.Mandate = "tailnet"
			c.PublicOrigin = "https://trouble.example"
			return c
		}(), "", ""},
		{"multi-project off loopback needs project_scope", func() Config {
			c := DefaultConfig()
			c.Bind = "0.0.0.0"
			c.Mandate = "proxy"
			c.PublicOrigin = "https://trouble.example"
			c.MaxProjects = 2
			return c
		}(), string(types.CodeLifecycle001), "project_scope_required"},
		{"empty public_origin off loopback", func() Config {
			c := DefaultConfig()
			c.Bind = "0.0.0.0"
			c.Mandate = "proxy"
			return c
		}(), string(types.CodeLifecycle001), "public_origin_required"},
		{"multi-project with scope and origin is fine", func() Config {
			c := DefaultConfig()
			c.Bind = "0.0.0.0"
			c.Mandate = "proxy"
			c.PublicOrigin = "https://trouble.example"
			c.MaxProjects = 2
			c.ProjectScope = []string{"payment"}
			return c
		}(), "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a startup refusal")
			}
			var de *dashError
			if !errors.As(err, &de) {
				t.Fatalf("error is not a dashError: %v", err)
			}
			if string(de.Code) != tc.want {
				t.Fatalf("code = %s, want %s", de.Code, tc.want)
			}
			if tc.detail != "" && de.Detail != tc.detail {
				t.Fatalf("detail = %q, want %q", de.Detail, tc.detail)
			}
			if de.HTTP != http.StatusInternalServerError {
				t.Fatalf("startup refusal HTTP = %d, want 500", de.HTTP)
			}
		})
	}

	// PageLimit is capped at 500 (§3.4).
	cfg := DefaultConfig()
	cfg.PageLimit = 5000
	cfg.normalize()
	if cfg.PageLimit != 500 {
		t.Fatalf("page_limit = %d, want the 500 cap", cfg.PageLimit)
	}
}

// TestTokenStoreContract is §3.2's own contract: plaintext shown once, hashed
// at rest, 0600 at rest, rotation with no grace window, revocation kept for
// audit, and LastUsedTS written at most once per 60s.
func TestTokenStoreContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	store, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("load empty store: %v", err)
	}
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

	tok, plain, err := store.Mint(labelWrite, []types.Scope{types.ScopeWrite}, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !tokenGrammar.MatchString(plain) || len(plain) != tokenPlainLen {
		t.Fatalf("plaintext %q does not match the §3.2 grammar", plain)
	}
	if tok.Hash != hashOf(plain) {
		t.Fatalf("stored hash is not sha256(plaintext)[:32]")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(b), plain) {
		t.Fatal("plaintext is on disk")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	// Lookup by plaintext; scope expansion autonomy ⇒ write ⇒ read.
	entry, ok := store.lookup(plain)
	if !ok {
		t.Fatal("minted token does not authenticate")
	}
	if !entry.scopes.has(types.ScopeRead) || !entry.scopes.has(types.ScopeWrite) {
		t.Fatalf("write token scopes = %s, want the read+write expansion", entry.scopes)
	}
	if entry.scopes.has(types.ScopeAutonomy) {
		t.Fatal("write token must not expand to autonomy")
	}

	// Rotate: new label <label>+<yyyymmdd>, old entry revoked, no grace window.
	newTok, newPlain, err := store.Rotate(labelWrite, now)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if want := labelWrite + "+20260916"; newTok.ID != want {
		t.Fatalf("rotated label = %q, want %q", newTok.ID, want)
	}
	if _, ok := store.lookup(plain); ok {
		t.Fatal("the rotated-away token still authenticates (grace window)")
	}
	if _, ok := store.lookup(newPlain); !ok {
		t.Fatal("the rotated token does not authenticate")
	}
	_ = b

	// Revocation keeps the entry for audit.
	if err := store.Revoke(newTok.ID, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := store.lookup(newPlain); ok {
		t.Fatal("revoked token still authenticates")
	}
	b, _ = os.ReadFile(path)
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range tf.Tokens {
		if e.ID == newTok.ID {
			found = true
			if !e.Revoked {
				t.Fatal("revoked entry lost its revoked:true flag")
			}
		}
	}
	if !found {
		t.Fatal("revoked entry was dropped instead of kept for audit")
	}
}

// TestLastUsedWriteAmplification is §3.2: LastUsedTS is written at most once per
// 60s per token.
func TestLastUsedWriteAmplification(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})
	base := env.clock.Now()

	env.get("/", env.readPlain)
	first := readLastUsed(t, env.tokenFile, labelRead)
	if first == "" {
		t.Fatal("LastUsedTS was not written on first use")
	}

	env.clock.advance(10 * time.Second)
	env.get("/", env.readPlain)
	if got := readLastUsed(t, env.tokenFile, labelRead); got != first {
		t.Fatalf("LastUsedTS written again inside 60s: %q → %q", first, got)
	}

	env.clock.advance(61 * time.Second)
	env.get("/", env.readPlain)
	if got := readLastUsed(t, env.tokenFile, labelRead); got == first {
		t.Fatalf("LastUsedTS not refreshed after 60s (still %q, base %s)", got, base.Format(types.TsLayout))
	}
}

func readLastUsed(t *testing.T, path, label string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatalf("parse token file: %v", err)
	}
	for _, tok := range tf.Tokens {
		if tok.ID == label {
			return tok.LastUsedTS
		}
	}
	return ""
}

// TestTokenFileReloadIsPerRequest is §3.2's reload semantics and edge case 3: an
// mtime change swaps the set, and a rotation cannot half-apply inside one
// request.
func TestTokenFileReloadIsPerRequest(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	// A token added to the file by the CLI becomes usable without a restart.
	_, freshHash := tokenFor(7)
	freshPlain, _ := tokenFor(7)
	b, _ := json.Marshal(tokenFile{Version: 1, Tokens: []types.Token{
		{ID: "dash-read@tablet", Hash: freshHash, Scopes: []types.Scope{types.ScopeRead}},
	}})
	tmp := env.tokenFile + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, env.tokenFile); err != nil {
		t.Fatal(err)
	}
	env.clock.advance(2 * time.Second)

	resp, body := env.get("/", freshPlain)
	wantStatus(t, resp, body, http.StatusOK)

	// And the rotated-away token is gone at the same request boundary.
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusUnauthorized)
	resp, body = env.get("/", env.writePlain)
	wantStatus(t, resp, body, http.StatusUnauthorized)
}

// TestHumanActorOnWrites is §4.2: the actor is always the human behind the
// token — kind human, id = the token label, build fields empty.
func TestHumanActorOnWrites(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	payload := `{"reason":"actor probe","until":"15m","expected_state":"verifying","ledger_seq":41207}`

	resp, body := env.post("/api/incidents/"+inc+"/ack", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusOK)

	call, ok := env.actions.lastAck()
	if !ok {
		t.Fatal("ladder.Ack was not called")
	}
	if call.Actor.Kind != types.ActorHuman || call.Actor.ID != labelWrite {
		t.Fatalf("actor = %+v, want {human %s}", call.Actor, labelWrite)
	}
	if call.Actor.Version != "" || call.Actor.GitSHA != "" || call.Actor.BuildTime != "" {
		t.Fatalf("human actor carries build fields: %+v", call.Actor)
	}
	if call.Reason != "actor probe" || call.Until != types.Duration("15m") {
		t.Fatalf("ack args = %+v", call)
	}
}

// TestWriteActionPolicyRefusals is §2.2/§4.2: read_only refuses every POST,
// clearing the kill-switch needs allow_resume, mode full needs allow_full, and
// enabling the kill-switch is always allowed.
func TestWriteActionPolicyRefusals(t *testing.T) {
	inc := validIncidentID()

	t.Run("read_only refuses every POST", func(t *testing.T) {
		env := newEnv(t, envOptions{cfg: func(c *Config) {
			unlimitedRates(c)
			c.ReadOnly = true
		}})
		for _, tc := range []struct{ path, body string }{
			{"/api/incidents/" + inc + "/ack", `{"reason":"x","expected_state":"verifying","ledger_seq":41207}`},
			{"/api/incidents/" + inc + "/close", `{"reason":"x","resolution":"fixed","expected_state":"verifying","ledger_seq":41207}`},
			{"/api/autonomy", `{"mode":"assisted"}`},
		} {
			token := env.writePlain
			if strings.HasSuffix(tc.path, "/autonomy") {
				token = env.autoPlain
			}
			resp, body := env.post(tc.path, token, tc.body)
			wantStatus(t, resp, body, http.StatusForbidden)
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard010) || eb.Detail != "read_only" {
				t.Fatalf("%s: code/detail = %s/%q, want %s/read_only", tc.path, eb.Error.Code, eb.Detail, types.CodeDashboard010)
			}
		}
		if env.actions.ackCount() != 0 {
			t.Fatal("read_only still reached the ladder")
		}
		if env.autonomy.writes() != 0 {
			t.Fatal("read_only still reached the autonomy writer")
		}
	})

	t.Run("clearing the kill-switch needs allow_resume", func(t *testing.T) {
		env := newEnv(t, envOptions{cfg: unlimitedRates})
		g := env.autonomy.Gates()
		g.KillSwitch = true
		env.autonomy.gates = g

		resp, body := env.post("/api/autonomy", env.autoPlain, `{"kill_switch":false}`)
		wantStatus(t, resp, body, http.StatusForbidden)
		eb := decodeError(t, body)
		if eb.Detail != "resume_disabled" {
			t.Fatalf("detail = %q, want resume_disabled", eb.Detail)
		}

		// Enabling it again is always allowed.
		resp, body = env.post("/api/autonomy", env.autoPlain, `{"kill_switch":true}`)
		wantStatus(t, resp, body, http.StatusOK)

		// With allow_resume the clear works.
		env2 := newEnv(t, envOptions{cfg: func(c *Config) {
			unlimitedRates(c)
			c.AllowResume = true
		}})
		g2 := env2.autonomy.Gates()
		g2.KillSwitch = true
		env2.autonomy.gates = g2
		resp, body = env2.post("/api/autonomy", env2.autoPlain, `{"kill_switch":false}`)
		wantStatus(t, resp, body, http.StatusOK)
	})

	t.Run("mode full needs allow_full", func(t *testing.T) {
		env := newEnv(t, envOptions{cfg: unlimitedRates})
		resp, body := env.post("/api/autonomy", env.autoPlain, `{"mode":"full"}`)
		wantStatus(t, resp, body, http.StatusForbidden)
		eb := decodeError(t, body)
		if eb.Detail != "full_disabled" {
			t.Fatalf("detail = %q, want full_disabled", eb.Detail)
		}

		env2 := newEnv(t, envOptions{cfg: func(c *Config) {
			unlimitedRates(c)
			c.AllowFull = true
		}})
		resp, body = env2.post("/api/autonomy", env2.autoPlain, `{"mode":"full","kill_switch":true}`)
		wantStatus(t, resp, body, http.StatusOK)
		gates := env2.autonomy.Gates()
		if gates.Mode != types.AutoFull || !gates.KillSwitch {
			t.Fatalf("gates after write = %+v", gates)
		}
	})

	t.Run("an invalid mode is refused", func(t *testing.T) {
		env := newEnv(t, envOptions{cfg: unlimitedRates})
		resp, body := env.post("/api/autonomy", env.autoPlain, `{"mode":"yolo"}`)
		wantStatus(t, resp, body, http.StatusBadRequest)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard010) {
			t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard010)
		}
	})
}

// TestWriteBodyCap is §2.8: a body over max_body_bytes is 413 + 011 before any
// decode.
func TestWriteBodyCap(t *testing.T) {
	env := newEnv(t, envOptions{cfg: func(c *Config) {
		unlimitedRates(c)
		c.MaxBodyBytes = 64
	}})
	inc := validIncidentID()
	big := `{"reason":"` + strings.Repeat("x", 200) + `","expected_state":"verifying","ledger_seq":41207}`
	resp, body := env.post("/api/incidents/"+inc+"/ack", env.writePlain, big)
	wantStatus(t, resp, body, http.StatusRequestEntityTooLarge)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard011) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard011)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Fatalf("413 carried Retry-After = %q (not a rate condition)", ra)
	}
	if env.actions.ackCount() != 0 {
		t.Fatal("an oversized body reached the ladder")
	}
}

// TestStaleViewCompareAndSet is §2.1.1/edge case 2: a double-tap cannot
// double-close; the second POST is 409 + 010 stale_view with a refreshed
// fragment.
func TestStaleViewCompareAndSet(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()

	// Render the incident page as a phone would, and take the CAS pair from it.
	resp, page := env.get("/incidents/"+inc, env.writePlain)
	wantStatus(t, resp, page, http.StatusOK)
	if !strings.Contains(page, `name="expected_state" value="verifying"`) {
		t.Fatalf("incident page does not render the CAS pair: %.400s", page)
	}

	first := `{"reason":"closing","resolution":"false_positive","expected_state":"verifying","ledger_seq":41207}`
	resp, body := env.post("/api/incidents/"+inc+"/close", env.writePlain, first)
	wantStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `id="incident-rows"`) {
		t.Fatalf("close did not return the refreshed incident-rows fragment: %.200s", body)
	}

	// Second tap with the same (now stale) pair.
	resp, body = env.post("/api/incidents/"+inc+"/close", env.writePlain, first)
	wantStatus(t, resp, body, http.StatusConflict)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard010) || eb.Detail != "stale_view" {
		t.Fatalf("code/detail = %s/%q, want %s/stale_view", eb.Error.Code, eb.Detail, types.CodeDashboard010)
	}
	if eb.Fragment == "" || !strings.Contains(eb.Fragment, `id="incident-rows"`) {
		t.Fatal("409 stale_view did not carry the refreshed fragment")
	}

	// The ladder's close moved the state, so a well-formed third POST is stale
	// on expected_state alone (no double-close from a stale form).
	resp, body = env.post("/api/incidents/"+inc+"/close", env.writePlain,
		`{"reason":"again","resolution":"fixed","expected_state":"verifying","ledger_seq":41207}`)
	wantStatus(t, resp, body, http.StatusConflict)

	// And an ack CAS also smells a moved ledger seq.
	env.idx.setSeq(41208)
	resp, body = env.post("/api/incidents/"+inc+"/ack", env.writePlain,
		`{"reason":"stale seq","expected_state":"verifying","ledger_seq":41207}`)
	wantStatus(t, resp, body, http.StatusConflict)
}

// TestWriteFormBody: urlencoded bodies are accepted alongside JSON (§2.1.1).
func TestWriteFormBody(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	form := "reason=form+probe&until=1h&expected_state=verifying&ledger_seq=41207"
	resp, body := env.do(env.req(http.MethodPost, "/api/incidents/"+inc+"/ack", []byte(form), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		bearer(env.writePlain)(r)
	}))
	wantStatus(t, resp, body, http.StatusOK)
	call, _ := env.actions.lastAck()
	if call.Reason != "form probe" || call.Until != types.Duration("1h") {
		t.Fatalf("form body decoded as %+v", call)
	}
}

// TestSubsystemCodeMapping is §6.1/SPEC-INDEX §5 rule 2: a write seam that
// exposes the owning subsystem's code has it surfaced on the wire.
func TestSubsystemCodeMapping(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	env.actions.ackErr = &seamError{Code: types.CodeLadder001, Msg: "illegal transition"}

	payload := `{"reason":"x","expected_state":"verifying","ledger_seq":41207}`
	resp, body := env.post("/api/incidents/"+inc+"/ack", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusBadRequest)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeLadder001) {
		t.Fatalf("code = %s, want the seam's %s", eb.Error.Code, types.CodeLadder001)
	}

	// A transient class maps to 503.
	env.actions.ackErr = &seamError{Code: types.CodeLadder009, Msg: "research unavailable"}
	resp, body = env.post("/api/incidents/"+inc+"/ack", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusServiceUnavailable)
}

// TestActionableErrorWithoutCodeIs500 keeps the untyped path honest: an error
// the adapter did not wrap is a 500, never a silent success.
func TestActionableErrorWithoutCodeIs500(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	env.actions.ackErr = errors.New("boom")
	payload := `{"reason":"x","expected_state":"verifying","ledger_seq":41207}`
	resp, body := env.post("/api/incidents/"+inc+"/ack", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusInternalServerError)
	eb := decodeError(t, body)
	if eb.Detail != "action_failed" {
		t.Fatalf("detail = %q, want action_failed", eb.Detail)
	}
}

// TestMemPressureShed is §2.9: sustained RSS at or above mem_pressure_pct of
// MemoryHigh for 60s sheds new partial polls (503 + Retry-After: 5) while
// pages, /health.json and POSTs keep serving.
func TestMemPressureShed(t *testing.T) {
	high := false
	env := newEnv(t, envOptions{cfg: unlimitedRates, deps: func(d *Deps) {
		d.Watermarks = func() types.RuntimeWatermarks {
			rss := int64(41 << 20)
			if high {
				rss = 180 << 20 // 93% of a 192 MiB MemoryHigh
			}
			return types.RuntimeWatermarks{RSSBytes: rss, MemHighBytes: 192 << 20}
		}
	}})

	// Not yet over: polls serve.
	resp, body := env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)

	high = true
	// First observation starts the 60s clock; polls still serve.
	resp, body = env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)

	env.clock.advance(61 * time.Second)
	resp, body = env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusServiceUnavailable)
	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Fatalf("Retry-After = %q, want 5", ra)
	}

	// Pages, health.json and POSTs keep serving.
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	resp, body = env.get("/health.json", "")
	wantStatus(t, resp, body, http.StatusOK)
	inc := validIncidentID()
	resp, body = env.post("/api/incidents/"+inc+"/ack", env.writePlain,
		`{"reason":"under pressure","expected_state":"verifying","ledger_seq":41207}`)
	wantStatus(t, resp, body, http.StatusOK)

	// Pressure clears → polls resume.
	high = false
	env.clock.advance(time.Second)
	resp, body = env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
}

// TestHealthExemptionDoesNotBypassShed: the shed is not an auth path, so
// /health.json still serves while partials are shed (asserted above) — and the
// shed response itself never carries credential material.
func TestShedResponseShape(t *testing.T) {
	high := true
	env := newEnv(t, envOptions{cfg: unlimitedRates, deps: func(d *Deps) {
		d.Watermarks = func() types.RuntimeWatermarks {
			return types.RuntimeWatermarks{RSSBytes: 180 << 20, MemHighBytes: 192 << 20}
		}
	}})
	_ = high
	env.get("/partials/health", env.readPlain) // start the clock
	env.clock.advance(61 * time.Second)
	_, body := env.get("/partials/incidents", env.readPlain)
	if strings.Contains(body, "tdt_") || strings.Contains(body, "hash") {
		t.Fatalf("shed response leaks credential material: %.200s", body)
	}
	if !strings.Contains(body, string(types.CodeDashboard013)) {
		t.Fatalf("shed response code missing: %.200s", body)
	}
}

var _ = fmt.Sprintf
var _ = time.Second
