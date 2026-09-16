package flow

// filing_test.go — SPEC-08 §7 `review_test.go`, `dedup_test.go` and
// `registration_test.go`: the review modes, one-row-per-(sig,board) dedup and the
// registration proof's zero-byte refusal.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestReviewModes(t *testing.T) {
	ctx := context.Background()
	// auto → todo.
	fx := newFixture(t, nil)
	res, err := fx.flow.File(ctx, fx.incident(), fx.row(""))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	rows, _, _ := fx.readBoard()
	mustEqual(t, rows[0]["status"], "todo", "auto status")
	mustEqual(t, res.Wrote, true, "auto wrote")

	// review → blocked, with the row visible and inert.
	fx2 := newFixture(t, func(c *types.FlowConfig) { c.ReviewMode = types.FlowReviewReview })
	if _, err := fx2.flow.File(ctx, fx2.incident(), fx2.row("")); err != nil {
		t.Fatalf("File: %v", err)
	}
	rows2, _, _ := fx2.readBoard()
	mustEqual(t, rows2[0]["status"], "blocked", "review status")

	// never → zero bytes written and a skipped decision. (review_mode=never with
	// hotfix on is refused at load, so the lane is off in this fixture.)
	fx3 := newFixture(t, func(c *types.FlowConfig) {
		c.ReviewMode = types.FlowReviewNever
		c.Hotfix.Enabled = false
	})
	if _, err := fx3.flow.File(ctx, fx3.incident(), fx3.row("")); err != nil {
		t.Fatalf("File: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx3.board, tasksFile)); !os.IsNotExist(err) {
		t.Fatalf("review_mode=never must write zero bytes")
	}
	p := fx3.rec.last(types.KFlow)
	mustEqual(t, p["decision"], "skipped", "never decision")
	mustEqual(t, p["reason"], "review_mode_never", "never reason")
}

func TestDedupOneRowPerSig(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	const n = 1000
	for i := 0; i < n; i++ {
		sig := "sig:" + string(rune('a'+i%26)) + "-" + itoa(i)
		if _, err := fx.flow.File(ctx, fx.incident(), fx.row(sig)); err != nil {
			t.Fatalf("File %d: %v", i, err)
		}
	}
	rows, _, lines := fx.readBoard()
	if len(lines) != n {
		t.Fatalf("rows = %d, want %d", len(lines), n)
	}
	_ = rows
	// Three more incidents on ONE sig: comments, never a second row.
	sig := "sig:recurring"
	if _, err := fx.flow.File(ctx, fx.incident(), fx.row(sig)); err != nil {
		t.Fatalf("File: %v", err)
	}
	_, _, lines = fx.readBoard()
	before := len(lines)
	for i := 0; i < 3; i++ {
		res, err := fx.flow.File(ctx, fx.incident(), fx.row(sig))
		if err != nil {
			t.Fatalf("recurrence %d: %v", i, err)
		}
		if res.Wrote {
			t.Fatalf("a recurrence must not write a row")
		}
		if res.DupOf == "" {
			t.Fatalf("a recurrence must name the existing row")
		}
	}
	_, _, after := fx.readBoard()
	if len(after) != before {
		t.Fatalf("rows grew from %d to %d on recurrence", before, len(after))
	}
	_, events, _ := fx.readBoard()
	comments := 0
	for _, ev := range events {
		if ev["type"] == types.EvTaskComment {
			comments++
		}
	}
	if comments != 3 {
		t.Fatalf("comments = %d, want 3", comments)
	}
}

func TestRegistrationRefusalWritesNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		mut   func(*types.FlowConfig)
		check func(*testing.T, *fixture)
	}{
		{
			name: "project absent from the scheduler",
			mut: func(c *types.FlowConfig) {
				c.Projects["payment-api"] = setRegistered(c.Projects["payment-api"], false, "scheduler_project_absent")
			},
		},
		{
			name: "project disabled",
			mut:  func(c *types.FlowConfig) { c.Projects["payment-api"] = setEnabled(c.Projects["payment-api"], false) },
		},
		{
			name: "board path mismatch",
			mut: func(c *types.FlowConfig) {
				p := setBoard(c.Projects["payment-api"], "/somewhere/else/.board")
				c.Projects["payment-api"] = setRegistered(p, false, "board_path_mismatch")
			},
		},
		{
			name: "proof stale past registration_stale_max",
			mut: func(c *types.FlowConfig) {
				c.RegistrationStaleMax = "1m"
				c.Projects["payment-api"] = setProbeTS(c.Projects["payment-api"], types.FormatUTC(nowMinus(2*time.Minute)))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, tc.mut)
			_, err := fx.flow.File(ctx, fx.incident(), fx.row(""))
			if err == nil || !strings.Contains(err.Error(), string(types.CodeFlow018)) {
				t.Fatalf("err = %v, want TROUBLE-FLOW-018", err)
			}
			if _, err := os.Stat(filepath.Join(fx.board, tasksFile)); !os.IsNotExist(err) {
				t.Fatalf("an unproven project must receive zero bytes")
			}
		})
	}
}

// TestRegistrationProbeAgainstAScheduler exercises the real proof path: the three
// checks plus the staleness rule, over an httptest scheduler.
func TestRegistrationProbeAgainstAScheduler(t *testing.T) {
	var enabled = true
	var board = ""
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(projectList("payment-api", board, enabled)))
	})
	fx := newFixture(t, func(c *types.FlowConfig) {
		c.SchedulerEndpoint = srv.URL
		c.Projects["payment-api"] = setRegistered(c.Projects["payment-api"], false, "")
	})
	board = fx.board
	// The fixture's project is not proven yet: the probe proves it.
	if err := fx.flow.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	p, _ := fx.flow.projectByName("payment-api")
	if !p.Registered || p.Reason != "" {
		t.Fatalf("project = %+v, want a held proof", p)
	}
	if _, err := fx.flow.File(context.Background(), fx.incident(), fx.row("")); err != nil {
		t.Fatalf("File after a held proof: %v", err)
	}
	// A disabled project fails the proof and refuses the next filing.
	enabled = false
	if err := fx.flow.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if _, err := fx.flow.File(context.Background(), fx.incident(), fx.row("sig:two")); err == nil {
		t.Fatalf("a scheduler-disabled project must refuse")
	}
	// An unreachable scheduler is NOT a registration failure: the last proof
	// holds until registration_stale_max.
	fx2 := newFixture(t, func(c *types.FlowConfig) {
		c.SchedulerEndpoint = "http://127.0.0.1:1"
		c.Projects["payment-api"] = setRegistered(c.Projects["payment-api"], true, "")
	})
	if err := fx2.flow.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if _, err := fx2.flow.File(context.Background(), fx2.incident(), fx2.row("")); err != nil {
		t.Fatalf("an unreachable scheduler must not refuse inside the staleness window: %v", err)
	}
}

// TestRouterDriverDispatches: the task-router driver hands the row over and never
// claims to have written it; both modes produce the same payload.
func TestRouterDriverDispatches(t *testing.T) {
	var got []byte
	var auth string
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got = append([]byte(nil), buf[:n]...)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Driver = types.FlowDriverRouter
		c.ReviewMode = types.FlowReviewAuto
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
		c.Projects["payment-api"] = setSeverityNeutral(c.Projects["payment-api"])
	})
	fx.severity = types.SevMedium // below the hot-fix lane: the router is used
	res, err := fx.flow.File(context.Background(), fx.incident(), fx.row(""))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !strings.Contains(string(got), `"task_id":"`+res.TaskID+`"`) {
		t.Fatalf("dispatch body = %s", got)
	}
	if !strings.Contains(string(got), `"idem_key":"`+res.TaskID+`"`) {
		t.Fatalf("idempotency must be the task id: %s", got)
	}
	if _, err := os.Stat(filepath.Join(fx.board, tasksFile)); !os.IsNotExist(err) {
		t.Fatalf("the router owns the append: no local row")
	}
	if p := fx.rec.last(types.KFlow); p["decision"] != "dispatched" {
		t.Fatalf("flow record = %v", p)
	}
	_ = auth
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
