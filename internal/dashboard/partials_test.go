package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// partials_test.go is the SPEC-10 §7 row for §2.1.2: the seven fragments, their
// exact root elements, the stale-render guard attributes every one of them
// carries, the ≤8 KB contract at 200 rows, and the no-inline-script rule.

// partialSpec is one §2.1.2 row: the request path, the exact root element id and
// the swap/trigger shape the fragment must carry.
type partialSpec struct {
	name    string
	path    string
	rootID  string
	swap    string
	trigger string
}

func partialSpecs(inc string) []partialSpec {
	return []partialSpec{
		{"health", "/partials/health", "health-strip", `hx-swap="outerHTML"`, `hx-trigger="every 1s"`},
		{"incidents", "/partials/incidents", "incident-rows", `hx-swap="innerHTML settle:0s"`, `hx-trigger="every 2s, troubleSeq from:body"`},
		{"timeline", "/partials/incidents/" + inc + "/timeline", "incident-timeline", `hx-swap="innerHTML settle:0s"`, `hx-trigger="every 2s, troubleSeq from:body"`},
		{"groups", "/partials/groups", "group-rows", `hx-swap="innerHTML settle:0s"`, `hx-trigger="every 2s, troubleSeq from:body"`},
		{"rules", "/partials/rules", "rule-rows", `hx-swap="innerHTML settle:0s"`, `hx-trigger="every 2s, troubleSeq from:body"`},
		{"breakers", "/partials/breakers", "breaker-rows", `hx-swap="innerHTML settle:0s"`, `hx-trigger="every 2s, troubleSeq from:body"`},
		{"budget", "/partials/budget", "budget-panel", `hx-swap="outerHTML"`, `hx-trigger="every 2s, troubleSeq from:body"`},
	}
}

func TestPartialsRootElements(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	specs := partialSpecs(validIncidentID())

	if len(specs) != 7 {
		t.Fatalf("§2.1.2 lists 7 polling partials, the table has %d", len(specs))
	}

	for _, spec := range specs {
		t.Run(spec.name, func(t *testing.T) {
			for _, hx := range []string{"", "true"} {
				req := env.req(http.MethodGet, spec.path, nil, func(r *http.Request) {
					bearer(env.readPlain)(r)
					if hx != "" {
						r.Header.Set("HX-Request", hx)
					}
				})
				resp, body := env.do(req)
				wantStatus(t, resp, body, http.StatusOK)

				// A GET partial is an ordinary GET: curl and the stall checker
				// consume the same fragment a browser polls (§2.1.2).
				if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
					t.Fatalf("HX-Request=%q: content type = %q", hx, ct)
				}
				if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
					t.Fatalf("HX-Request=%q: Cache-Control = %q", hx, cc)
				}

				// Exactly one root element, with the guard attributes.
				wantRoot := `<` + rootTag(spec.rootID) + ` id="` + spec.rootID + `"`
				if !strings.HasPrefix(body, wantRoot) {
					t.Fatalf("HX-Request=%q: fragment does not start with %q: %.160s", hx, wantRoot, body)
				}
				for _, attr := range []string{"data-seq=", "data-rendered-ts=", "data-stall-s="} {
					if !strings.Contains(body, attr) {
						t.Errorf("fragment is missing the stale-render guard attribute %s", attr)
					}
				}
				if !strings.Contains(body, spec.swap) {
					t.Errorf("fragment is missing %s", spec.swap)
				}
				if !strings.Contains(body, spec.trigger) {
					t.Errorf("fragment is missing %s", spec.trigger)
				}
				if !strings.Contains(body, `hx-sync="this:replace"`) {
					t.Error("fragment does not set hx-sync=this:replace (no request pile-up)")
				}

				// No document scaffold, no inline script.
				for _, bad := range []string{"<html", "<body", "<head", "<script"} {
					if strings.Contains(strings.ToLower(body), bad) {
						t.Errorf("fragment contains %q", bad)
					}
				}

				if !strings.HasPrefix(strings.TrimSpace(body), "<") || !strings.HasSuffix(strings.TrimSpace(body), ">") {
					t.Errorf("fragment is not a single element: %.120s", body)
				}
			}
		})
	}
}

// rootTag maps a root id onto the element that carries it (§2.1.2).
func rootTag(id string) string {
	switch id {
	case "incident-rows", "group-rows", "rule-rows", "breaker-rows":
		return "tbody"
	case "incident-timeline":
		return "ol"
	default:
		return "div"
	}
}

// TestPartialsSizeAt200Rows is the §2.1.2 ≤8 KB contract with 200 rows of
// fixtures in every table, and the same fragments on the wire.
func TestPartialsSizeAt200Rows(t *testing.T) {
	const rows = 200
	inc := validIncidentID()

	incidents := make([]types.Incident, 0, rows)
	groups := make([]types.GroupStat, 0, rows)
	rules := make([]types.Rule, 0, rows)
	breakers := make([]types.Breaker, 0, rows)

	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("inc_01J9F00000000000000000%02d", i%100)
		incidents = append(incidents, types.Incident{
			ID: id, Sig: fmt.Sprintf("sentinel:sha256v1:%032x", i), GroupID: fmt.Sprintf("grp_01J9F00000000000000000%02d", i%100),
			State: types.StVerifying, EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevHigh,
			OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:14:03.221Z",
		})
		groups = append(groups, types.GroupStat{
			GroupID: fmt.Sprintf("grp_01J9F00000000000000000%02d", i%100), Sig: fmt.Sprintf("sentinel:sha256v1:%032x", i),
			Source: "sentinel", Title: fmt.Sprintf("group %d of the fixture set", i), Count: uint64(100 + i),
			Rate1m: float64(i) / 10, LastSeenTS: "2026-09-16T09:14:03.221Z",
		})
		rules = append(rules, types.Rule{
			Name: fmt.Sprintf("rule-%03d", i), Enabled: true, Source: types.SrcPSI,
			EntryRung: types.RungPlay, Severity: types.SevLow,
		})
		breakers = append(breakers, types.Breaker{
			Scope: fmt.Sprintf("rule:rule-%03d", i), State: types.BreakerOpen,
			OpenUntil: "2026-09-16T10:00:00.000Z", Trips: i, Reason: "flapping",
		})
	}

	env := newEnv(t, envOptions{
		cfg: unlimitedRates,
		tokens: []types.Token{
			tokenEntryFor(labelRead, 1, types.ScopeRead),
		},
		deps: func(d *Deps) {
			ix := d.Index.(*fakeIndex)
			ix.mu.Lock()
			for _, i := range incidents {
				ix.incidents[i.ID] = i
				ix.order = append(ix.order, i.ID)
				ix.records[i.ID] = []types.Record{{
					Seq: ix.seq, RecID: "rec_x", TS: "2026-09-16T09:14:03.221Z", Kind: types.KIncident, Inc: i.ID,
					Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"}, Payload: map[string]any{"transition": "verifying", "reason": "play applied"},
				}}
			}
			for _, g := range groups {
				ix.groups = append(ix.groups, g)
			}
			ix.mu.Unlock()
			d.Rules = rules
			d.Breakers = func() []types.Breaker { return breakers }
		},
	})

	t.Run("incidents at 200 rows", func(t *testing.T) {
		resp, body := env.get("/partials/incidents", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		assertFragmentSize(t, body, rows)
	})

	t.Run("groups at 200 rows", func(t *testing.T) {
		resp, body := env.get("/partials/groups", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		assertFragmentSize(t, body, rows)
	})

	t.Run("rules at 200 rows", func(t *testing.T) {
		resp, body := env.get("/partials/rules", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		assertFragmentSize(t, body, rows)
	})

	t.Run("breakers at 200 rows", func(t *testing.T) {
		resp, body := env.get("/partials/breakers", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		assertFragmentSize(t, body, rows)
	})

	t.Run("timeline at 200 rows", func(t *testing.T) {
		// One incident with 200 ledger records.
		ix := env.idx
		ix.mu.Lock()
		recs := make([]types.Record, 0, rows)
		for i := 0; i < rows; i++ {
			recs = append(recs, types.Record{
				Seq: uint64(i + 1), RecID: fmt.Sprintf("rec_%03d", i), TS: "2026-09-16T09:14:03.221Z",
				Kind: types.KToolCall, Inc: inc, Actor: types.Actor{Kind: types.ActorHuman, ID: labelWrite},
				Payload: map[string]any{"module": "config.set", "check_mode": true},
			})
		}
		ix.records[inc] = recs
		ix.mu.Unlock()

		resp, body := env.get("/partials/incidents/"+inc+"/timeline", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		assertFragmentSize(t, body, rows)
	})
}

func assertFragmentSize(t *testing.T, body string, rows int) {
	t.Helper()
	// §2.1.2: ≤8 KB uncompressed, per fragment, at the fixture size.
	if n := len(body); n > maxPartialBytes {
		t.Fatalf("fragment is %d bytes at %d rows, the §2.1.2 cap is %d", n, rows, maxPartialBytes)
	}
	if !strings.Contains(body, "data-truncated") && !strings.Contains(body, "more rows") &&
		!strings.Contains(body, "more groups") && !strings.Contains(body, "more rules") &&
		!strings.Contains(body, "more breakers") && !strings.Contains(body, "more records") {
		return // everything fit: no marker, nothing to prove about the row count
	}
	// Over the cap: the fragment must carry the complete rows it kept plus a
	// single marker row naming the remainder (never a silent cut).
	switch {
	case strings.HasPrefix(body, "<tbody"):
		if got := strings.Count(body, "<tr"); got < 2 {
			t.Fatalf("truncated fragment rendered %d rows, want at least one plus the marker", got)
		}
	case strings.HasPrefix(body, "<ol"):
		if got := strings.Count(body, "<li"); got < 2 {
			t.Fatalf("truncated fragment rendered %d entries, want at least one plus the marker", got)
		}
	}
}

// TestPartialOverCapFitsTheCap proves the cap is enforced, not assumed: a
// fragment that would exceed 8 KB is re-rendered with as many complete rows as
// fit, marks the remainder (data-truncated + a marker row) and never ships an
// oversized swap.
func TestPartialOverCapFitsTheCap(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()

	// 200 records with ~200-rune summaries each: over the cap once rendered.
	ix := env.idx
	long := strings.Repeat("é", 200)
	ix.mu.Lock()
	recs := make([]types.Record, 0, 300)
	for i := 0; i < 300; i++ {
		recs = append(recs, types.Record{
			Seq: uint64(i + 1), RecID: fmt.Sprintf("rec_%03d", i), TS: "2026-09-16T09:14:03.221Z",
			Kind: types.KToolCall, Inc: inc, Actor: types.Actor{Kind: types.ActorHuman, ID: labelWrite},
			Payload: map[string]any{"transition": long},
		})
	}
	ix.records[inc] = recs
	ix.mu.Unlock()

	before := env.s.counters.renderErr.Load()
	resp, body := env.get("/partials/incidents/"+inc+"/timeline", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if len(body) > maxPartialBytes {
		t.Fatalf("fragment is %d bytes, the §2.1.2 cap is %d", len(body), maxPartialBytes)
	}
	if !strings.Contains(body, "data-truncated=\"") || strings.Contains(body, `data-truncated="0"`) {
		t.Fatalf("over-cap fragment does not report its truncation: %.200s", body)
	}
	if !strings.Contains(body, "more records") {
		t.Fatalf("over-cap fragment has no marker row: %.200s", body)
	}
	if env.s.counters.renderErr.Load() != before {
		t.Fatal("fitting a fragment to the cap must not count as a render error")
	}
}

// TestPartialRenderFailureIs007: a genuine template execute failure takes the §5
// 007 path (static error body, counter moves, the rest of the dashboard keeps
// serving).
func TestPartialRenderFailureIs007(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	// Corrupt one fragment: the entry points at a template name that is not in
	// the compiled set (the §2.1.2 "registered fragment absent from the
	// compiled set" case).
	entry := env.s.partials["breakers"]
	entry.name = "partial-does-not-exist"
	env.s.partials["breakers"] = entry

	before := env.s.counters.renderErr.Load()
	resp, body := env.get("/partials/breakers", env.readPlain)
	wantStatus(t, resp, body, http.StatusInternalServerError)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard007) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard007)
	}
	if env.s.counters.renderErr.Load() <= before {
		t.Fatal("dash_render_errors_total did not move")
	}
	// The rest keeps serving.
	resp, body = env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
}

// TestPartialWhitelist is row 20: only the seven §2.1.2 names exist; a
// multi-segment path is not a row at all (§2.1.3, 009).
func TestPartialPathShape(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})
	resp, body := env.get("/partials/incidents/", env.readPlain, func(r *http.Request) {
		r.Header.Set("HX-Request", "true")
	})
	wantStatus(t, resp, body, http.StatusNotFound)
	if eb := decodeError(t, body); eb.Error.Code != string(types.CodeDashboard009) {
		t.Fatalf("code = %s, want %s for a non-row path", eb.Error.Code, types.CodeDashboard009)
	}
}

// TestPartialWhitelist is row 20: only the seven §2.1.2 names exist.
func TestPartialWhitelist(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	known := []string{"health", "incidents", "groups", "rules", "breakers", "budget"}
	for _, name := range known {
		resp, body := env.get("/partials/"+name, env.readPlain)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/partials/%s: status = %d (body %.120s)", name, resp.StatusCode, body)
		}
	}
	for _, name := range []string{"secrets", "tokens", "health.json", "timeline"} {
		resp, body := env.get("/partials/"+name, env.readPlain, func(r *http.Request) {
			r.Header.Set("HX-Request", "true")
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/partials/%s: status = %d, want 404", name, resp.StatusCode)
			continue
		}
		eb := decodeError(t, body)
		if eb.Error.Code != string(types.CodeDashboard008) {
			t.Errorf("/partials/%s: code = %s, want %s", name, eb.Error.Code, types.CodeDashboard008)
		}
	}
}

// TestIncidentsSinceResync is edge case 11: a since past the newest seq returns
// the current top rows with the current seq — a full resync, not a gap.
func TestIncidentsSinceResync(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	resp, body := env.get("/partials/incidents?since=99999999", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, `<tbody id="incident-rows"`) {
		t.Fatalf("resync fragment wrong: %.160s", body)
	}
	if strings.Count(body, "<tr") == 0 {
		t.Fatal("a since beyond the newest seq returned an empty fragment instead of the current top rows")
	}
	if !strings.Contains(body, fmt.Sprintf(`data-seq="%d"`, env.idx.seqNow())) {
		t.Fatalf("resync fragment does not carry the current seq: %.200s", body)
	}
}

// TestTimelineForVanishedIncident is edge case 1: a poll racing a legitimate
// state change (close or compaction) gets 200 with an empty-state fragment —
// only a POST against an unknown id is a failure.
func TestTimelineForVanishedIncident(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	gone := "inc_01J9F0000000000000000000ZZ"

	resp, body := env.get("/partials/incidents/"+gone+"/timeline", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if !strings.HasPrefix(body, `<ol id="incident-timeline"`) {
		t.Fatalf("vanished-incident poll did not return the fragment: %.160s", body)
	}
	if strings.Contains(body, "<li") {
		t.Fatalf("vanished incident returned timeline rows: %.160s", body)
	}
}

// TestStripGuardAndCounters: the strip carries the denial counters and the
// banner flag the client's stale-render logic consumes.
func TestStripGuardAndCounters(t *testing.T) {
	env := newEnv(t, envOptions{noRefresh: true, cfg: unlimitedRates})

	// Provoke one 401 refusal and one CSRF refusal, then read the strip.
	env.get("/", "tdt_"+strings.Repeat("Z", 43))
	inc := validIncidentID()
	env.do(env.req(http.MethodPost, "/api/incidents/"+inc+"/ack", []byte(`{"reason":"x"}`), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		bearer(env.writePlain)(r)
		r.AddCookie(authCookie(env.writePlain, false))
	}))

	resp, body := env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	for _, want := range []string{`data-denied="1"`, `data-csrf="1"`, `data-rl="0"`, `data-banner="false"`} {
		if !strings.Contains(body, want) {
			t.Errorf("strip missing %s: %.240s", want, body)
		}
	}
	if !strings.Contains(body, `data-seq="`+fmt.Sprint(env.idx.seqNow())+`"`) {
		t.Errorf("strip data-seq is not the index seq: %.200s", body)
	}
}
