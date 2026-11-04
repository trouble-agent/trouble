package dashboard

// incident_subject_render_test.go — TRBL-126: the incident row names the unit a
// failure is about. The row renders the incident's subject under its title, and an
// incident without a subject renders none — the title/sig fallback the row
// already carries stays the only line, so an empty subject can never be mistaken
// for a named one.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// withSubject copies the seeded incident and stamps the subject on it, so the
// fixture's other fields (group title, story refs) stay exactly as the shared
// fixtures define them. The row reads the index and the page reads the lookup,
// so both are stamped: a surface disagreeing with the other is the bug class this
// row is about.
func withSubject(t *testing.T, env *testEnv, subject string) string {
	t.Helper()
	id := validIncidentID()
	ix := env.idx
	ix.mu.Lock()
	inc, ok := ix.incidents[id]
	if !ok {
		ix.mu.Unlock()
		t.Fatalf("the fixture index has no incident %s", id)
	}
	inc.Subject = subject
	ix.incidents[id] = inc
	ix.mu.Unlock()
	env.lookup.putIncident(inc)
	return id
}

func TestIncidentRowRendersTheSubject(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	const subject = "payment-worker.service: unit_substate=failed via properties_changed"
	inc := withSubject(t, env, subject)

	resp, body := env.get("/partials/incidents", env.readPlain)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /partials/incidents = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, subject) {
		t.Fatalf("the incident row does not render the subject %q: %s", subject, body)
	}
	if !strings.Contains(body, `<div class="subject">`) {
		t.Fatalf("the row renders no subject element: %s", body)
	}
	// The subject rides the title cell, so the row keeps its documented columns.
	if got, want := strings.Count(body, "<td>"), 6*strings.Count(body, "<tr>"); got != want {
		t.Fatalf("the incident row changed its column count: %d cells for %d rows (want %d): %s", got, strings.Count(body, "<tr>"), want, body)
	}

	// The incident page names it too (the operator's second click).
	resp, body = env.get("/incidents/"+inc, env.readPlain)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /incidents/%s = %d %s", inc, resp.StatusCode, body)
	}
	if !strings.Contains(body, subject) {
		t.Fatalf("the incident page does not render the subject: %s", body)
	}
}

func TestIncidentRowWithoutASubjectRendersNone(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	resp, body := env.get("/partials/incidents", env.readPlain)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /partials/incidents = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, `<div class="subject">`) {
		t.Fatalf("an incident with no subject rendered a subject element: %s", body)
	}
	// The fallback the row already had: the group title (or the sig) it resolves.
	if !strings.Contains(body, "payment worker queue wedge") {
		t.Fatalf("a subject-less incident lost its title fallback: %s", body)
	}
}

// TestSubjectIsTruncatedForTheRow keeps the row's layout contract: the subject is
// render-bounded at the same 200-rune limit as the title (SPEC-10 §6.7), so a
// pasted log line cannot push a row off the page.
func TestSubjectIsTruncatedForTheRow(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	long := "unit.service: " + strings.Repeat("x", 400)
	withSubject(t, env, long)

	row := env.s.incidentRow(func() types.Incident {
		inc := env.idx.incidents[validIncidentID()]
		return inc
	}(), env.s.now(), map[string]string{})
	if got := len([]rune(row.Subject)); got > maxTitleRunes+1 {
		t.Fatalf("subject is %d runes, want <= %d plus the truncation marker", got, maxTitleRunes)
	}
	if !strings.HasSuffix(row.Subject, "…") {
		t.Fatalf("a 400-rune subject was not truncated: %q", row.Subject)
	}
}
