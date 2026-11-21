package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/ladder"
	"github.com/trouble-agent/trouble/internal/types"
)

// TestClosePlayFailedThroughDashboard exercises the real ladder, bridge and HTTP listener.
func TestClosePlayFailedThroughDashboard(t *testing.T) {
	c := bootCoreLoop(t)
	for _, trigger := range []string{"T03", "T09"} {
		if _, err := c.l.Advance(context.Background(), c.inc, ladder.Transition{Trigger: trigger}); err != nil {
			t.Fatalf("advance %s: %v", trigger, err)
		}
	}
	if got := ladderState(t, c.l, c.inc); got != types.StPlayFailed {
		t.Fatalf("state = %q, want play:failed", got)
	}
	status, page := c.get(t, "/incidents/"+c.inc)
	if status != http.StatusOK || !strings.Contains(page, `action="/api/incidents/`+c.inc+`/close"`) {
		t.Fatalf("failed incident close control missing: status %d, page %s", status, page)
	}
	status, body := c.post(t, "/api/incidents/"+c.inc+"/close", `{"reason":"operator reviewed failure","resolution":"wontfix"}`)
	if status != http.StatusOK {
		t.Fatalf("POST close = %d, want 200: %s", status, body)
	}
	inc, err := c.l.Incident(context.Background(), c.inc)
	if err != nil || inc.State != types.StResolved || inc.ResolvedTS == "" {
		t.Fatalf("incident after close = %+v, err %v; want resolved with timestamp", inc, err)
	}
}
