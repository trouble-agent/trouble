package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// csrf_test.go is the SPEC-10 §7 row for §2.3: the four checks in order, both
// legal POST shapes, and the cookie attributes that implement check 3
// (SameSite) — asserted by header inspection, because SameSite is enforced by
// the browser only if the attribute is there.

// browserPost builds the canonical browser POST: the auth cookie, the CSRF
// cookie, the double-submit header and an Origin equal to the request host.
func (e *testEnv) browserPost(path, body string, mutate ...func(*http.Request)) (*http.Response, string) {
	e.t.Helper()
	return e.do(e.req(http.MethodPost, path, []byte(body), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(authCookie(e.writePlain, false))
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: e.csrfFor(labelWrite)})
		r.Header.Set(csrfHeaderName, e.csrfFor(labelWrite))
		r.Header.Set("Origin", "http://"+strings.TrimPrefix(e.srv.URL, "http://"))
		for _, m := range mutate {
			m(r)
		}
	}))
}

func TestCSRFMatrix(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	payload := `{"reason":"csrf probe","until":"15m","expected_state":"verifying","ledger_seq":41207}`
	ackPath := "/api/incidents/" + inc + "/ack"

	// The exact code+detail the refusal must carry (§2.3: the first failing
	// check returns 403 + 004 and no state change).
	wantRefusal := func(t *testing.T, resp *http.Response, body string) {
		t.Helper()
		wantStatus(t, resp, body, http.StatusForbidden)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard004) {
			t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard004)
		}
		if eb.Detail == "" {
			t.Fatal("004 must name the failed check in detail")
		}
	}

	// Case 6 (Origin mismatch) is its own subtest because it is the only one
	// that needs a different Origin value.
	t.Run("1 missing header", func(t *testing.T) {
		before := env.actions.ackCount()
		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(authCookie(env.writePlain, false))
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: env.csrfFor(labelWrite)})
			r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
		}))
		wantRefusal(t, resp, body)
		if got := decodeError(t, body).Detail; got != "missing_header" {
			t.Fatalf("detail = %q, want missing_header", got)
		}
		if env.actions.ackCount() != before {
			t.Fatal("a rejected POST reached the ladder")
		}
	})

	t.Run("2 wrong header value", func(t *testing.T) {
		resp, body := env.browserPost(ackPath, payload, func(r *http.Request) {
			r.Header.Set(csrfHeaderName, "not-the-derived-value")
		})
		wantRefusal(t, resp, body)
	})

	t.Run("3 cookie differs from header", func(t *testing.T) {
		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(authCookie(env.writePlain, false))
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "different-cookie-value"})
			r.Header.Set(csrfHeaderName, env.csrfFor(labelWrite))
			r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
		}))
		wantRefusal(t, resp, body)
		if got := decodeError(t, body).Detail; got != "cookie_mismatch" {
			t.Fatalf("detail = %q, want cookie_mismatch", got)
		}
	})

	t.Run("4 value from three hours ago", func(t *testing.T) {
		old := env.s.csrf.value(labelRead, env.clock.Now().Add(-3*time.Hour)).Value
		resp, body := env.browserPost(ackPath, payload, func(r *http.Request) {
			r.Header.Set(csrfHeaderName, old)
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: old})
		})
		wantRefusal(t, resp, body)
	})

	t.Run("5 value bound to a different token ID", func(t *testing.T) {
		other := env.s.csrf.value(labelAuto, env.clock.Now()).Value
		resp, body := env.browserPost(ackPath, payload, func(r *http.Request) {
			r.Header.Set(csrfHeaderName, other)
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: other})
		})
		wantRefusal(t, resp, body)
	})

	t.Run("6 Origin mismatch", func(t *testing.T) {
		resp, body := env.browserPost(ackPath, payload, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
		})
		wantRefusal(t, resp, body)
		if got := decodeError(t, body).Detail; got != "origin_mismatch" {
			t.Fatalf("detail = %q, want origin_mismatch", got)
		}
	})

	t.Run("6b browser POST without Origin or Referer", func(t *testing.T) {
		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(authCookie(env.writePlain, false))
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: env.csrfFor(labelWrite)})
			r.Header.Set(csrfHeaderName, env.csrfFor(labelWrite))
		}))
		wantRefusal(t, resp, body)
		if got := decodeError(t, body).Detail; got != "missing_origin" {
			t.Fatalf("detail = %q, want missing_origin", got)
		}
	})

	t.Run("7 Bearer plus cookie mixture", func(t *testing.T) {
		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			bearer(env.writePlain)(r)
			r.AddCookie(authCookie(env.writePlain, false))
			r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
		}))
		wantRefusal(t, resp, body)
		if got := decodeError(t, body).Detail; got != "mixed_credentials" {
			t.Fatalf("detail = %q, want mixed_credentials", got)
		}
	})

	t.Run("8 correct browser POST", func(t *testing.T) {
		// Fetch the page first: that is where the CSRF cookie is set and the
		// meta value comes from. Then replay exactly what a browser would.
		resp, page := env.get("/incidents/"+inc, "", withCookies(authCookie(env.writePlain, false)))
		wantStatus(t, resp, page, http.StatusOK)

		var csrfCookieVal string
		for _, c := range resp.Cookies() {
			if c.Name == csrfCookieName {
				csrfCookieVal = c.Value
			}
		}
		if csrfCookieVal == "" {
			t.Fatal("the page did not set the trouble_csrf cookie")
		}
		metaIdx := strings.Index(page, `name="trouble-csrf" content="`)
		if metaIdx < 0 {
			t.Fatal("the page did not render the trouble-csrf meta")
		}
		rest := page[metaIdx+len(`name="trouble-csrf" content="`):]
		metaVal := rest[:strings.Index(rest, `"`)]
		if metaVal != csrfCookieVal {
			t.Fatalf("meta (%q) and cookie (%q) disagree", metaVal, csrfCookieVal)
		}

		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(authCookie(env.writePlain, false))
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookieVal})
			r.Header.Set(csrfHeaderName, metaVal)
			r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
		}))
		wantStatus(t, resp, body, http.StatusOK)
		if !strings.Contains(body, `id="incident-rows"`) {
			t.Fatalf("the browser POST did not return the refreshed fragment: %.200s", body)
		}
		if env.actions.ackCount() == 0 {
			t.Fatal("the browser POST never reached the ladder")
		}
	})

	t.Run("8b previous-hour value is accepted", func(t *testing.T) {
		prev := env.s.csrf.value(labelWrite, env.clock.Now().Add(-1*time.Hour)).Value
		resp, body := env.do(env.req(http.MethodPost, ackPath, []byte(payload), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(authCookie(env.writePlain, false))
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: prev})
			r.Header.Set(csrfHeaderName, prev)
			r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
		}))
		wantStatus(t, resp, body, http.StatusOK)
	})

	t.Run("9 correct curl Bearer POST", func(t *testing.T) {
		// No cookies at all: checks 1–2 hold, the double-submit is vacuous.
		resp, body := env.post(ackPath, env.writePlain, payload)
		wantStatus(t, resp, body, http.StatusOK)
	})
}

// TestCSRFCookieAttributes implements check 3: SameSite=Lax on both cookies,
// HttpOnly on the auth cookie and not on the CSRF cookie (the template must
// read it), Max-Age values, no Domain, and Secure only when it must be.
func TestCSRFCookieAttributes(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	resp, page := env.get("/", env.readPlain, withCookies(authCookie(env.readPlain, false)))
	wantStatus(t, resp, page, http.StatusOK)

	var csrf *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == csrfCookieName {
			csrf = c
		}
	}
	if csrf == nil {
		t.Fatal("no trouble_csrf cookie on the page response")
	}
	if csrf.SameSite != http.SameSiteLaxMode {
		t.Errorf("csrf cookie SameSite = %v, want Lax", csrf.SameSite)
	}
	if csrf.HttpOnly {
		t.Error("csrf cookie must not be HttpOnly: the template reads it")
	}
	if csrf.Path != "/" {
		t.Errorf("csrf cookie Path = %q, want /", csrf.Path)
	}
	if csrf.MaxAge != 3600 {
		t.Errorf("csrf cookie Max-Age = %d, want 3600", csrf.MaxAge)
	}
	if csrf.Domain != "" {
		t.Errorf("csrf cookie carries a Domain attribute: %q", csrf.Domain)
	}
	if csrf.Secure {
		t.Error("csrf cookie is Secure on a loopback bind")
	}

	// The auth cookie's attributes (§2.2): constructed by the dashboard for a
	// client to hold; asserted directly because nothing in v0.1 sets it.
	ac := authCookie("x", false)
	if ac.SameSite != http.SameSiteLaxMode || !ac.HttpOnly || ac.MaxAge != 43200 || ac.Path != "/" || ac.Domain != "" {
		t.Fatalf("auth cookie attributes wrong: %+v", ac)
	}
	if authCookie("x", true).Secure != true {
		t.Fatal("the auth cookie is not Secure when it must be")
	}

	// On a non-loopback bind every cookie is Secure.
	env2 := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
		unlimitedRates(c)
		c.Bind = "192.0.2.5"
		c.Mandate = "tailnet"
		c.PublicOrigin = "https://trouble.example"
	}})
	req := env2.req(http.MethodGet, "/", nil, bearer(env2.readPlain))
	rec := httptest.NewRecorder()
	env2.s.ServeHTTP(rec, req)
	foundSecure := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName && c.Secure {
			foundSecure = true
		}
	}
	if !foundSecure {
		t.Error("non-loopback bind must force Secure cookies")
	}
}

// TestCSRFBindingIsTokenScoped: the same derived value is refused for a
// different token ID even when the cookie/header pair agrees (case 5 restated
// against the derivation, not the pair).
func TestCSRFBindingIsTokenScoped(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	payload := `{"reason":"x","until":"15m","expected_state":"verifying","ledger_seq":41207}`

	// Cookie-only (browser) POST authenticated as labelWrite, carrying a value
	// derived for labelRead: the pair agrees, the binding does not.
	valForRead := env.s.csrf.value(labelRead, env.clock.Now()).Value
	resp, body := env.do(env.req(http.MethodPost, "/api/incidents/"+inc+"/ack", []byte(payload), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(authCookie(env.writePlain, false))
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: valForRead})
		r.Header.Set(csrfHeaderName, valForRead)
		r.Header.Set("Origin", "http://"+strings.TrimPrefix(env.srv.URL, "http://"))
	}))
	wantStatus(t, resp, body, http.StatusForbidden)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard004) || eb.Detail != "token_binding" {
		t.Fatalf("code/detail = %s/%q, want %s/token_binding", eb.Error.Code, eb.Detail, types.CodeDashboard004)
	}
}

// TestCSRFSecretIsPerProcess: a restart regenerates k_csrf, so a captured value
// stops working and there is no persisted CSRF key file to steal.
func TestCSRFSecretIsPerProcess(t *testing.T) {
	a := newEnv(t, envOptions{noRefresh: true})
	b := newEnv(t, envOptions{noRefresh: true})

	va := a.s.csrf.value(labelRead, a.clock.Now()).Value
	vb := b.s.csrf.value(labelRead, b.clock.Now()).Value
	if va == vb {
		t.Fatal("two processes derived the same CSRF value: k_csrf is not random per process")
	}
	if len(a.s.csrf.key) != 32 {
		t.Fatalf("k_csrf is %d bytes, want 32", len(a.s.csrf.key))
	}
}
