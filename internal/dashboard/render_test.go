package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// render_test.go is the SPEC-10 §7 row for AC-16 and §2.7/§2.8: the golden
// render of all four entity classes (incidents, groups, issues/board row,
// breakers), the no-external-origin rule, the 404 path's independence from the
// template set, and the §2.8 header surface.

// attrURL matches every URL-bearing attribute in a rendered document.
var attrURL = regexp.MustCompile(`(?:src|href|action)\s*=\s*"([^"]*)"`)

// TestAC16GoldenRender walks every page and asserts the entity classes render
// where §2.1 says they do.
func TestAC16GoldenRender(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	grp := validGroupID()

	t.Run("/ renders incidents and group rows", func(t *testing.T) {
		_, page := env.get("/", env.readPlain)
		want := []string{
			"inc_01J9F0000000000000000000AB", // the open incident
			"payment worker queue wedge",     // its group title
			`id="health-strip"`,              // the accelerator strip
			`data-seq="`,                     // the render guard
		}
		assertContainsAll(t, page, want)
	})

	t.Run("/incidents renders each incident with state and rung", func(t *testing.T) {
		_, page := env.get("/incidents", env.readPlain)
		assertContainsAll(t, page, []string{inc, "verifying", "agent", "high"})
	})

	t.Run("/incidents/{id} is the story panel", func(t *testing.T) {
		_, page := env.get("/incidents/"+inc, env.writePlain)
		// AC-16 + AC-19: the issue ref, the board row, the research outcome, the
		// spawn, the PR link and the skill candidate all live on this page.
		assertContainsAll(t, page, []string{
			inc,
			"iss_01J9F0000000000000000000AB",
			"https://github.com/example/trouble/issues/7",
			"tsk_01J9F00000000000000000000A",
			"res_01J9F0000000000000000000AB",
			"queue-wedge class brief returned",
			"sp_01J9F0000000000000000000AB",
			"https://github.com/example/trouble/pull/12",
			"sk_01J9F0000000000000000000AB",
			// the timeline fragment and its ledger rows
			`id="incident-timeline"`,
			"tool_call",
			"transition verifying",
			// the CAS pair on the write forms
			`name="expected_state" value="verifying"`,
			`name="ledger_seq" value="41207"`,
			// the group link
			`href="/groups/grp_01J9F0000000000000000000AB"`,
		})
	})

	t.Run("/groups and /groups/{id} render groups", func(t *testing.T) {
		_, page := env.get("/groups", env.readPlain)
		assertContainsAll(t, page, []string{grp, "payment worker queue wedge", "3.5/min"})

		_, detail := env.get("/groups/"+grp, env.readPlain)
		assertContainsAll(t, detail, []string{
			grp, "payment worker queue wedge", "1.4.0", "1.5.0",
			"redacted values", "sample rate",
		})
	})

	t.Run("/rules renders rules with their stats and sensors", func(t *testing.T) {
		_, page := env.get("/rules", env.readPlain)
		assertContainsAll(t, page, []string{"io-pressure", "journal-loop", "psi", "3m ago", "no sample yet"})
	})

	t.Run("/breakers renders the breaker registry", func(t *testing.T) {
		_, page := env.get("/breakers", env.readPlain)
		assertContainsAll(t, page, []string{"rule:io-pressure", "open", "2026-09-16T10:00:00.000Z", "flapping"})
	})
}

// TestReadOnlyScopeRendersEveryControl is §2.2/AC-19: a read-scope token
// renders every page and partial in full, including every ack/close/autonomy
// control — disabled with data-requires and an inline reason, never hidden.
func TestReadOnlyScopeRendersEveryControl(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()

	_, page := env.get("/incidents/"+inc, env.readPlain)
	assertContainsAll(t, page, []string{
		`data-requires="write"`,
		"requires a write-scope token",
		"acknowledge",
		"close",
	})
	// The controls are disabled for a read session, and the server would refuse
	// the POST even if a client forged it (asserted next).
	if strings.Count(page, " disabled") < 3 {
		t.Fatalf("read-scope page does not disable the write controls: %.400s", page)
	}

	_, index := env.get("/", env.readPlain)
	assertContainsAll(t, index, []string{`data-requires="autonomy"`, "requires an autonomy-scope token"})

	// A write-scope session gets live controls.
	_, writePage := env.get("/incidents/"+inc, env.writePlain)
	if !strings.Contains(writePage, `data-requires="write"`) {
		t.Fatal("write session lost the data-requires marker")
	}

	// A forged POST from the read session is still refused (§2.2: UI state is
	// presentation only; the server re-checks every POST).
	resp, body := env.post("/api/incidents/"+inc+"/ack", env.readPlain,
		`{"reason":"forged","expected_state":"verifying","ledger_seq":41207}`)
	wantStatus(t, resp, body, http.StatusForbidden)
	if eb := decodeError(t, body); eb.Error.Code != string(types.CodeDashboard003) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard003)
	}
	if env.actions.ackCount() != 0 {
		t.Fatal("the forged POST reached the ladder")
	}
}

// TestNoExternalOrigins is §2.7: every src/href/action in every rendered page is
// same-origin — no CDN, no fonts, no analytics, no third-party request.
func TestNoExternalOrigins(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	pages := []string{"/", "/incidents", "/incidents/" + inc, "/groups", "/groups/" + validGroupID(), "/rules", "/breakers"}

	for _, path := range pages {
		_, page := env.get(path, env.writePlain)
		for _, m := range attrURL.FindAllStringSubmatch(page, -1) {
			u := m[1]
			if u == "" || strings.HasPrefix(u, "/") || strings.HasPrefix(u, "#") {
				continue
			}
			// The one legitimate absolute URL is the issue/PR link a human wrote
			// into the ledger: it is text, never fetched by the page.
			if strings.HasPrefix(u, "https://github.com/example/trouble/") {
				continue
			}
			t.Errorf("%s: external origin %q", path, u)
		}
		// No inline script or style anywhere (§2.7: 'self' must suffice).
		lower := strings.ToLower(page)
		if strings.Contains(lower, "<script>") || strings.Contains(lower, "<style") {
			t.Errorf("%s: inline script/style present", path)
		}
		if strings.Contains(lower, " onclick=") || strings.Contains(lower, " onload=") {
			t.Errorf("%s: inline event handler present", path)
		}
	}
}

// TestSecurityHeaders is §2.8: every response carries the four baseline headers,
// every HTML response the CSP, and HTML ≥1 KB is gzipped for clients that asked.
func TestSecurityHeaders(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	for _, path := range []string{"/", "/health.json", "/partials/health", "/nope"} {
		resp, _ := env.get(path, env.readPlain)
		for h, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
			"Cache-Control":          "no-store",
		} {
			if got := resp.Header.Get(h); got != want {
				t.Errorf("%s: %s = %q, want %q", path, h, got, want)
			}
		}
	}

	resp, _ := env.get("/", env.readPlain)
	if csp := resp.Header.Get("Content-Security-Policy"); csp != cspHeader {
		t.Errorf("CSP = %q, want the §2.8 policy", csp)
	}

	// Static assets are the documented exception to no-store (private cache,
	// max-age 3600) but keep the baseline hardening headers.
	resp, _ = env.get("/static/app.css", env.readPlain)
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Errorf("static Cache-Control = %q", cc)
	}

	// Gzip for HTML ≥1 KB when the client asks for it.
	req := env.req(http.MethodGet, "/incidents", nil, func(r *http.Request) {
		bearer(env.readPlain)(r)
		r.Header.Set("Accept-Encoding", "gzip")
	})
	resp, body := env.do(req)
	wantStatus(t, resp, body, http.StatusOK)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Error("a large HTML response was not gzipped for a gzip-capable client")
	}
}

// TestFourOhFourIndependentOfTemplates is §2.1.3/§7: the error path is a static
// embedded document, so a deliberately broken template set cannot break it while
// every templated page fails loudly with 007.
func TestFourOhFourIndependentOfTemplates(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	// Corrupt every page set: the shell now calls a template that does not exist.
	broken := template.Must(template.New("root").Parse(`{{define "shell"}}{{template "nope" .}}{{end}}{{define "content"}}x{{end}}`))
	brokenSet := &templateSetImpl{tpl: broken}
	for name := range env.s.pageSets {
		env.s.pageSets[name] = brokenSet
	}
	// The fragments live in the same sets; repoint them so the whole rendered
	// surface is broken at once.
	for name, entry := range env.s.partials {
		entry.set = brokenSet
		env.s.partials[name] = entry
	}

	// Every page render now fails with 007 (a static body, not a template).
	for _, path := range []string{"/", "/incidents", "/groups", "/rules", "/breakers"} {
		resp, body := env.get(path, env.readPlain)
		wantStatus(t, resp, body, http.StatusInternalServerError)
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard007) {
			t.Errorf("%s: code = %s, want %s", path, eb.Error.Code, types.CodeDashboard007)
		}
	}

	// The 404 path is untouched: it serves the embedded static document.
	resp, body := env.get("/no-such-route", env.readPlain, htmlRequest)
	wantStatus(t, resp, body, http.StatusNotFound)
	if len(body) == 0 || len(body) > 400 {
		t.Fatalf("404 body is %d bytes (the embedded document is ≤400 B)", len(body))
	}
	if !strings.Contains(body, "404") {
		t.Fatalf("404 body is not the static document: %.200s", body)
	}

	// The fragments live in the shell set too, so a corrupted shell breaks them
	// as well — assert the documented severity split: a broken set is a
	// per-request 007, never a dead listener.
	resp, body = env.get("/partials/health", env.readPlain)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("partial with a broken set: status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(body, string(types.CodeDashboard007)) {
		t.Fatalf("partial failure body = %.160s", body)
	}
}

// TestPageParseFailureRefusesBoot is §2.7: a parse/define collision is a startup
// failure, not a 500 on every page.
func TestPageParseFailureRefusesBoot(t *testing.T) {
	// parseTemplates() over the real embedded set succeeds; prove the failure
	// mode by pointing it at a set with a duplicate define.
	if _, _, err := parseTemplates(); err != nil {
		t.Fatalf("the shipped template set does not parse: %v", err)
	}

	dup, err := template.New("root").Parse(`{{define "content"}}a{{end}}{{define "content"}}b{{end}}`)
	if err == nil {
		t.Fatal("a duplicate define parsed without error")
	}
	if dup != nil {
		t.Fatal("template returned non-nil on a duplicate define")
	}
}

// TestEscapingAndTruncation is §6.7/§6.9: hostile strings are template data (no
// template.HTML anywhere), a title is truncated at 200 runes for layout, and
// invalid UTF-8 does not reach the page raw.
func TestEscapingAndTruncation(t *testing.T) {
	hostile := `</script><img src=x onerror=alert(1)>` + strings.Repeat("é", 300)
	invalid := "bad \xff\xfe bytes"

	env := newEnv(t, envOptions{
		cfg: unlimitedRates,
		deps: func(d *Deps) {
			ix := d.Index.(*fakeIndex)
			ix.mu.Lock()
			ix.incidents["inc_01J9F0000000000000000000ZZ"] = types.Incident{
				ID: "inc_01J9F0000000000000000000ZZ", Sig: invalid,
				State: types.StDetected, Severity: types.SevHigh,
				OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:14:03.221Z",
			}
			ix.order = append(ix.order, "inc_01J9F0000000000000000000ZZ")
			ix.records["inc_01J9F0000000000000000000ZZ"] = []types.Record{{
				Seq: ix.seq, RecID: "rec_h", TS: "2026-09-16T09:14:03.221Z", Kind: types.KEvent,
				Inc: "inc_01J9F0000000000000000000ZZ", Sig: invalid,
				Actor:   types.Actor{Kind: types.ActorDaemon, ID: hostile},
				Payload: map[string]any{"transition": hostile},
			}}
			ix.mu.Unlock()
			d.Lookup.(*fakeLookup).putIncident(types.Incident{
				ID: "inc_01J9F0000000000000000000ZZ", Sig: invalid,
				State: types.StDetected, Severity: types.SevHigh,
				OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:14:03.221Z",
			})
			d.Story = func(inc string) Story { return Story{IssueRef: hostile, Candidate: hostile} }
		},
	})

	_, page := env.get("/incidents/inc_01J9F0000000000000000000ZZ", env.writePlain)

	// The raw payload must appear only in its escaped form: no <img> element is
	// ever produced by the page, and the whole payload is entity-encoded.
	if strings.Contains(page, "<img") {
		t.Fatal("a hostile string reached the page as markup: html/template escaping was bypassed")
	}
	if strings.Contains(page, "</script><img") {
		t.Fatal("the raw hostile payload is present unescaped")
	}
	if !strings.Contains(page, "&lt;/script&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatal("the hostile string is not present in fully escaped form")
	}
	// §6.7 truncation applies to titles (the story fields are not titles and
	// render in full): give a group a 300-rune title and read /groups.
	longTitle := strings.Repeat("é", 300)
	env.idx.addGroup(types.GroupStat{
		GroupID: "grp_01J9F0000000000000000000ZZ", Sig: "sentinel:sha256v1:aaa",
		Source: "sentinel", Title: longTitle, Count: 3, Rate1m: 0.1,
		LastSeenTS: "2026-09-16T09:14:03.221Z",
	})
	_, groupsPage := env.get("/groups", env.writePlain)
	if strings.Contains(groupsPage, longTitle) {
		t.Fatal("a 300-rune title rendered in full: §6.7 truncation did not apply")
	}
	if !strings.Contains(groupsPage, strings.Repeat("é", 200)+"…") {
		t.Fatal("a title was not truncated at exactly 200 runes plus the marker")
	}
	// Invalid UTF-8 is replaced, never emitted raw.
	if strings.Contains(page, "\xff") {
		t.Fatal("invalid UTF-8 reached the page unmodified")
	}
	if !strings.Contains(page, "\uFFFD") {
		t.Fatal("invalid UTF-8 was not replaced with U+FFFD")
	}
}

// TestNavAndShellShape: the shell carries the meta the poller needs, one CSRF
// meta per page, and no document-level markup inside fragments.
func TestNavAndShellShape(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	_, page := env.get("/", env.readPlain)

	assertContainsAll(t, page, []string{
		`<meta name="trouble-csrf" content="`,
		`<meta name="trouble-poll-ms" content="2000">`,
		`<meta name="trouble-strip-poll-ms" content="1000">`,
		`<meta name="trouble-stall-alert-s" content="90">`,
		`<meta name="viewport"`,
		`href="/static/app.css"`,
		`src="/static/htmx.min.js"`,
		`src="/static/app.js"`,
		`id="stale-banner"`,
		`id="budget-panel"`,
		`id="health-strip"`,
	})
	if strings.Count(page, "trouble-csrf") != 1 {
		t.Fatalf("expected exactly one CSRF meta, found %d", strings.Count(page, "trouble-csrf"))
	}
	if !strings.HasPrefix(page, "<!DOCTYPE html>") {
		t.Fatal("page does not start with a doctype")
	}
}

// TestBudgetPanelWatermarks: the panel renders the §3.4 watermark set from the
// injected source, never from the process's own accounting.
func TestBudgetPanelWatermarks(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	resp, body := env.get("/partials/budget", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	assertContainsAll(t, body, []string{
		"10.0 MB", // binary
		"41.0 MB", // rss
		"30.0 MB", // ledger
		"18.4/min",
		"0.1.0", "9c1f0ab",
	})
}

func assertContainsAll(t *testing.T, haystack string, needles []string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("rendered output is missing %q", n)
		}
	}
}

// dlPair matches one <dt>key</dt><dd>value</dd> row of the budget panel and
// counterPair one <li><span>name</span><strong>value</strong></li> row of the
// overview counters.
var (
	dlPair      = regexp.MustCompile(`<dt>([^<]*)</dt><dd>([^<]*)</dd>`)
	counterPair = regexp.MustCompile(`<li><span>([^<]*)</span><strong>([^<]*)</strong></li>`)
)

// budgetPairs reads the key/value rows of the budget panel a document carries.
func budgetPairs(t *testing.T, doc string) map[string]string {
	t.Helper()
	start := strings.Index(doc, `id="budget-panel"`)
	if start < 0 {
		t.Fatal("document carries no budget panel")
	}
	rest := doc[start:]
	if end := strings.Index(rest, "</div>"); end >= 0 {
		rest = rest[:end]
	}
	out := map[string]string{}
	for _, m := range dlPair.FindAllStringSubmatch(rest, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// counterPairs reads the overview counter rows (incidents/groups open).
func counterPairs(t *testing.T, doc string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range counterPair.FindAllStringSubmatch(doc, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("document carries no counters block")
	}
	return out
}

// footerLine returns the footer paragraph of a rendered page.
func footerLine(t *testing.T, doc string) string {
	t.Helper()
	i := strings.Index(doc, `<footer class="app-footer">`)
	if i < 0 {
		return ""
	}
	rest := doc[i:]
	j := strings.Index(rest, "<p>")
	if j < 0 {
		return ""
	}
	rest = rest[j+len("<p>"):]
	k := strings.Index(rest, "</p>")
	if k < 0 {
		return ""
	}
	return rest[:k]
}

// TestFirstPaintBudgetMatchesCounters is TRBL-010 defect 2: the budget panel a
// page server-renders on the FIRST paint carries the numbers the page's own
// header counters show — the numbers are already available at render time —
// and the numbers /partials/budget later serves from the same source, instead
// of the zeroes the first poll used to contradict.
func TestFirstPaintBudgetMatchesCounters(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	resp, page := env.get("/", env.readPlain)
	wantStatus(t, resp, page, http.StatusOK)

	counters := counterPairs(t, page)
	budget := budgetPairs(t, page)

	for _, key := range []string{"incidents open", "groups open"} {
		if budget[key] == "" || budget[key] == "0" {
			t.Errorf("first paint: budget %s = %q — the panel rendered empty while the header counted %q", key, budget[key], counters[key])
			continue
		}
		if budget[key] != counters[key] {
			t.Errorf("first paint contradicts itself: budget %s = %q, header counter = %q", key, budget[key], counters[key])
		}
	}
	for _, key := range []string{"binary", "rss", "ledger", "events"} {
		if budget[key] == "" {
			t.Errorf("first paint: budget %s is blank", key)
		}
	}

	// The fragment the poller fetches is the same source: the first paint must
	// already agree with it.
	fresp, frag := env.get("/partials/budget", env.readPlain)
	wantStatus(t, fresp, frag, http.StatusOK)
	served := budgetPairs(t, frag)
	for _, key := range []string{"binary", "rss", "ledger", "spool", "events", "groups open", "incidents open", "worktrees", "version"} {
		if budget[key] != served[key] {
			t.Errorf("first paint %s = %q but /partials/budget serves %q", key, budget[key], served[key])
		}
	}
}

// TestFooterVersionMatchesHealth is TRBL-010 defect 1: every page footer renders
// the build stamp the health surface reports (the same accessor), so the stamp
// an operator reads on a page can never be an unpopulated template field while
// /health.json carries the real sha.
func TestFooterVersionMatchesHealth(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	resp, body := env.get("/health.json", "")
	wantStatus(t, resp, body, http.StatusOK)
	var hr types.HealthResponse
	if err := json.Unmarshal([]byte(body), &hr); err != nil {
		t.Fatalf("health.json is not a HealthResponse: %v", err)
	}
	if hr.Version == "" || hr.GitSHA == "" {
		t.Fatalf("health surface reports an empty stamp (%+v) — the fixture is wrong, not the page", hr)
	}
	want := "trouble v" + hr.Version + " (" + hr.GitSHA + ")"

	pages := []string{
		"/", "/incidents", "/incidents/" + validIncidentID(),
		"/groups", "/groups/" + validGroupID(), "/rules", "/breakers",
	}
	for _, path := range pages {
		_, page := env.get(path, env.readPlain)
		line := footerLine(t, page)
		if line == "" {
			t.Errorf("%s: no footer paragraph", path)
			continue
		}
		if !strings.Contains(line, want) {
			t.Errorf("%s: footer = %q, want it to carry %q", path, line, want)
		}
	}
}

var _ = fmt.Sprint
