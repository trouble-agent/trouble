package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// routes_test.go is the SPEC-10 §7 row for the §2.1 route table: completeness,
// reachability through a real ServeMux, the duplicate-registration panic, and
// the §2.2 rule that no route is reachable with a URL-borne token.

// routeTableRows is the §2.1 table size: 20 registrable (method,path) rows
// plus row 21, the catch-all the router wrapper implements in ServeHTTP.
const routeTableRows = 21

func TestRouteTableCompleteness(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})
	rows := env.s.routeTable()

	if got, want := len(rows)+1, routeTableRows; got != want {
		t.Fatalf("table has %d registrable rows + the catch-all = %d, want %d", len(rows), got, want)
	}

	seen := map[string]bool{}
	for i, row := range rows {
		key := row.Method + " " + row.Path
		if seen[key] {
			t.Errorf("row %d: duplicate (method,path) %q", i, key)
		}
		seen[key] = true
		if row.Handler == nil {
			t.Errorf("row %d (%s): nil handler", i, key)
		}
		// Every row carries a scope except the documented health exemption,
		// which is a runtime decision on the same read scope.
		if row.Scope == "" {
			t.Errorf("row %d (%s): no scope and not the documented exemption", i, key)
		}
		if row.Scope != types.ScopeRead && row.Scope != types.ScopeWrite && row.Scope != types.ScopeAutonomy {
			t.Errorf("row %d (%s): unknown scope %q", i, key, row.Scope)
		}
		if !strings.HasPrefix(row.Path, "/") {
			t.Errorf("row %d: path %q is not absolute", i, row.Path)
		}
	}

	// The §2.1 table's rows, verbatim: method, path, scope.
	want := []route{
		{"GET", "/", types.ScopeRead, nil},
		{"GET", "/incidents", types.ScopeRead, nil},
		{"GET", "/incidents/{id}", types.ScopeRead, nil},
		{"GET", "/groups", types.ScopeRead, nil},
		{"GET", "/groups/{id}", types.ScopeRead, nil},
		{"GET", "/rules", types.ScopeRead, nil},
		{"GET", "/breakers", types.ScopeRead, nil},
		{"GET", "/health.json", types.ScopeRead, nil},
		{"POST", "/api/incidents/{id}/ack", types.ScopeWrite, nil},
		{"POST", "/api/incidents/{id}/close", types.ScopeWrite, nil},
		{"POST", "/api/autonomy", types.ScopeAutonomy, nil},
		{"GET", "/partials/health", types.ScopeRead, nil},
		{"GET", "/partials/incidents", types.ScopeRead, nil},
		{"GET", "/partials/incidents/{id}/timeline", types.ScopeRead, nil},
		{"GET", "/partials/groups", types.ScopeRead, nil},
		{"GET", "/partials/rules", types.ScopeRead, nil},
		{"GET", "/partials/breakers", types.ScopeRead, nil},
		{"GET", "/partials/budget", types.ScopeRead, nil},
		{"GET", "/static/{asset}", types.ScopeRead, nil},
		{"GET", "/partials/{name}", types.ScopeRead, nil},
	}
	if len(rows) != len(want) {
		t.Fatalf("route count = %d, want %d", len(rows), len(want))
	}
	for i := range want {
		if rows[i].Method != want[i].Method || rows[i].Path != want[i].Path || rows[i].Scope != want[i].Scope {
			t.Errorf("row %d = %s %s (%s), want %s %s (%s)", i,
				rows[i].Method, rows[i].Path, rows[i].Scope,
				want[i].Method, want[i].Path, want[i].Scope)
		}
	}
}

// TestRoutesRegisterAndAreReachable proves every row registers into a real
// http.ServeMux and is reachable through it with the row's own scope: 0
// unregistered rows.
func TestRoutesRegisterAndAreReachable(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	for _, row := range env.s.routeTable() {
		path := strings.ReplaceAll(row.Path, "{id}", validIncidentID())
		path = strings.ReplaceAll(path, "{name}", "health")
		path = strings.ReplaceAll(path, "{asset}", "app.css")
		if row.Path == "/partials/incidents/{id}/timeline" {
			path = "/partials/incidents/" + validIncidentID() + "/timeline"
		}
		if row.Path == "/groups/{id}" {
			path = "/groups/" + validGroupID()
		}

		token := env.readPlain
		switch row.Scope {
		case types.ScopeWrite:
			token = env.writePlain
		case types.ScopeAutonomy:
			token = env.autoPlain
		}

		var resp *http.Response
		var body string
		if row.Method == http.MethodPost {
			resp, body = env.post(path, token, `{"reason":"reachability probe","until":"15m","resolution":"fixed","mode":"shadow","kill_switch":true}`)
		} else {
			resp, body = env.get(path, token)
		}
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("row %s %s: 404 through the mux (body %.120s)", row.Method, row.Path, body)
		}
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("row %s %s: 405 — the §2.1.3 rule allows no 405", row.Method, row.Path)
		}
	}
}

// TestDuplicateRegistrationPanics: a duplicate (method,path) is a startup panic
// by construction — the mux is the only registration path (§4.1 step 4).
func TestDuplicateRegistrationPanics(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})
	mux := http.NewServeMux()
	env.s.registerRoutes(mux) // first registration is fine

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("registering the table twice did not panic")
		}
	}()
	env.s.registerRoutes(mux)
}

// TestNoRouteAcceptsURLToken is the §2.2/§7 assertion: on every row — with a
// token that would otherwise authenticate — a token in the query string or in
// a path segment is refused before authentication (400 + TROUBLE-DASHBOARD-005,
// never the route's own response).
func TestNoRouteAcceptsURLToken(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	paramNames := []string{"token", "access_token", "auth", "apikey", "api_key", "key", "trouble_token", "tdt", "TOKEN"}

	for _, row := range env.s.routeTable() {
		path := strings.ReplaceAll(row.Path, "{id}", validIncidentID())
		path = strings.ReplaceAll(path, "{name}", "health")
		path = strings.ReplaceAll(path, "{asset}", "app.css")
		if row.Path == "/groups/{id}" {
			path = "/groups/" + validGroupID()
		}
		if row.Path == "/partials/incidents/{id}/timeline" {
			path = "/partials/incidents/" + validIncidentID() + "/timeline"
		}
		token := env.readPlain
		switch row.Scope {
		case types.ScopeWrite:
			token = env.writePlain
		case types.ScopeAutonomy:
			token = env.autoPlain
		}

		for _, p := range paramNames {
			q := path + "?" + p + "=" + env.readPlain
			resp, body := env.get(q, token)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %s?%s=…: status = %d, want 400 (body %.120s)", row.Method, path, p, resp.StatusCode, body)
				continue
			}
			if eb := decodeError(t, body); eb.Error.Code != string(types.CodeDashboard005) {
				t.Errorf("%s %s?%s=…: code = %s, want %s", row.Method, path, p, eb.Error.Code, types.CodeDashboard005)
			}
			if strings.Contains(body, env.readPlain) {
				t.Errorf("%s %s?%s=…: response echoed the token", row.Method, path, p)
			}
		}

		// A token as a path segment is refused too — including on rows whose
		// {id} wildcard would otherwise match it.
		segPath := path + "/" + env.readPlain
		resp, body := env.get(segPath, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400 (body %.120s)", row.Method, segPath, resp.StatusCode, body)
		}
	}
}

// TestURLTokenRefusedBeforeAuth: the refusal happens even when nothing
// authenticates, and even when the parameter name is not in the list but the
// value matches the token grammar (a renamed leak is still a leak).
func TestURLTokenRefusedBeforeAuth(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})

	cases := []struct {
		name string
		url  string
	}{
		{"listed param, no auth", "/?token=" + env.readPlain},
		{"listed param, wrong case", "/?API_KEY=" + env.readPlain},
		{"unlisted param name, token value", "/?sdt=" + env.readPlain},
		{"token grammar in a path segment", "/incidents/" + env.readPlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := env.get(tc.url, "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %.160s)", resp.StatusCode, body)
			}
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard005) {
				t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard005)
			}
		})
	}
}

// TestNotFoundAndWrongMethod is §2.1.3: an unmatched (method,path) is 404 (never
// 405), a known path with a wrong method is also 404 with an Allow header, the
// browser gets the static 404.html, the API gets JSON, and the path is never
// echoed.
func TestNotFoundAndWrongMethod(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	t.Run("unknown path, browser", func(t *testing.T) {
		resp, body := env.get("/nope", env.readPlain, htmlRequest)
		wantStatus(t, resp, body, http.StatusNotFound)
		if resp.Header.Get("Allow") != "" {
			t.Errorf("Allow header set for an unknown path: %q", resp.Header.Get("Allow"))
		}
		if !strings.Contains(body, "404") || strings.Contains(body, "/nope") {
			t.Errorf("static 404 body wrong or echoes the path: %.200s", body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("browser 404 content type = %q", ct)
		}
	})

	t.Run("unknown path, api", func(t *testing.T) {
		resp, body := env.get("/nope", env.readPlain, func(r *http.Request) {
			r.Header.Set("Accept", "application/json")
		})
		wantStatus(t, resp, body, http.StatusNotFound)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard009) {
			t.Errorf("code = %s, want %s", eb.Error.Code, types.CodeDashboard009)
		}
		if strings.Contains(body, "/nope") {
			t.Errorf("JSON 404 echoes the path: %.200s", body)
		}
	})

	t.Run("wrong method on a known path is 404 with Allow", func(t *testing.T) {
		resp, body := env.post("/incidents", env.readPlain, `{}`, func(r *http.Request) {
			r.Header.Set("Accept", "application/json")
		})
		wantStatus(t, resp, body, http.StatusNotFound)
		if got := resp.Header.Get("Allow"); !strings.Contains(got, "GET") {
			t.Errorf("Allow = %q, want it to name GET", got)
		}
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard009) {
			t.Errorf("code = %s, want %s", eb.Error.Code, types.CodeDashboard009)
		}
	})

	t.Run("HEAD on a GET row is not a row", func(t *testing.T) {
		resp, body := env.do(env.req(http.MethodHead, "/incidents", nil, bearer(env.readPlain)))
		wantStatus(t, resp, body, http.StatusNotFound)
		if got := resp.Header.Get("Allow"); !strings.Contains(got, "GET") {
			t.Errorf("Allow = %q, want GET", got)
		}
	})

	t.Run("htmx requests get JSON error bodies", func(t *testing.T) {
		resp, body := env.get("/nope", env.readPlain, func(r *http.Request) {
			r.Header.Set("HX-Request", "true")
		})
		wantStatus(t, resp, body, http.StatusNotFound)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard009) {
			t.Errorf("code = %s, want %s", eb.Error.Code, types.CodeDashboard009)
		}
	})
}

// TestUnknownPartialIs008 covers row 20: a partial name outside the §2.1.2
// whitelist is 404 + TROUBLE-DASHBOARD-008.
func TestUnknownPartialIs008(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})
	resp, body := env.get("/partials/secrets", env.readPlain, func(r *http.Request) {
		r.Header.Set("HX-Request", "true")
	})
	wantStatus(t, resp, body, http.StatusNotFound)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard008) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard008)
	}
}

// TestMalformedIDsAreNotRouteMatches: {id} is an exact <prefix>_<ULID> match, so
// a malformed id is not a row (§2.1), and an unknown-but-well-formed incident on
// a POST carries the ladder's code (§6.1).
func TestMalformedIDsAreNotRouteMatches(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	for _, id := range []string{"inc_notaulid", "grp_01J9F0000000000000000000AB", "inc_01J9F0000000000000000000aB"} {
		resp, body := env.get("/incidents/"+id, env.readPlain, func(r *http.Request) {
			r.Header.Set("Accept", "application/json")
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/incidents/%s: status = %d, want 404 (body %.120s)", id, resp.StatusCode, body)
		}
	}

	resp, body := env.post("/api/incidents/inc_01J9F0000000000000000000ZZ/ack", env.writePlain,
		`{"reason":"x","expected_state":"verifying","ledger_seq":41207}`)
	wantStatus(t, resp, body, http.StatusNotFound)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeLadder012) {
		t.Fatalf("code = %s, want the ladder's %s (§6.1 case 1)", eb.Error.Code, types.CodeLadder012)
	}
}

// TestStaticAssets: row 19 serves the embedded assets with an ETag and a
// private cache, and an unknown asset is not a route.
func TestStaticAssets(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	for _, name := range []string{"app.css", "app.js", "htmx.min.js"} {
		resp, body := env.get("/static/"+name, env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		if resp.Header.Get("ETag") == "" {
			t.Errorf("/static/%s: no ETag", name)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
			t.Errorf("/static/%s: Cache-Control = %q", name, cc)
		}
		if len(body) == 0 {
			t.Errorf("/static/%s: empty body", name)
		}
	}

	resp, _ := env.get("/static/../go.mod", env.readPlain)
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("path traversal asset: status = %d, want 404 (or a redirect that cannot leave /static)", resp.StatusCode)
	}
}

// TestServeHTTPPanicNet: a panicking handler never takes the listener down; the
// request answers 500 and the panic counter moves ("no panics on request paths"
// is the contract, this is the net behind it).
func TestServeHTTPPanicNet(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true})
	before := env.s.counters.panics.Load()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	env.s.mux = mux

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped ServeHTTP: %v", r)
			}
		}()
		env.s.ServeHTTP(rec, req)
	}()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if env.s.counters.panics.Load() <= before {
		t.Error("panic did not increment the panic counter")
	}
}
