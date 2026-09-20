package flow

// filing_test.go — SPEC-08 §7 `review_test.go`, `dedup_test.go` and
// `registration_test.go`: the review modes, one-row-per-(sig,board) dedup and the
// registration proof's zero-byte refusal.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/loadfence"
	"github.com/trouble-agent/trouble/internal/types"
)

// Dedup-fixture parameters of SPEC-08 §7. The reference shape is 1,000
// distinct sigs filed one at a time (exactly 1,000 rows, 0 duplicates), then
// three recurrences on one sig (3 comments, never a second row). The fixture
// size is host-calibrated per SPEC-01 §7a: on a quiet reference-class host —
// and wherever /proc/loadavg is unavailable — the shape is exactly the spec's
// 1,000 rows.
const (
	dedupRows    = 1000
	minDedupRows = 200
	// EnvDedupRows lets an operator pin the fixture size (the TRBL-045
	// doctrine): an explicit positive integer wins over the auto-bounded
	// default, so re-measuring the §7 shape on any host stays a one-env-var
	// job.
	EnvDedupRows = "DEDUP_ROWS"
)

// dedupRowsForHost bounds the dedup fixture by the host's own measured
// calibration (SPEC-01 §7a) instead of its load average alone: the
// I/O-bound scale from loadfence — the box's I/O-pilot multiple times its
// load contention, clamped so a host can only ever LOOSEN a budget — divides
// the spec shape: 1000/scale rows, clamped to [200, 1000]. A quiet
// reference-class host keeps the spec shape (1,000 rows — the size §7's
// table names), and so does a host with no /proc/loadavg (load_avg 0 →
// contention 1; a missing or failed pilot leaves the multiple at the
// reference): the no-/proc/loadavg fallback IS the spec shape. A contended
// or slower host gets a proportionally smaller fixture — 500 rows at load 16
// on reference I/O, the 200 floor from scale 5 on (5× the reference I/O
// latency, load 64 on reference I/O, or 2× I/O at load 48) — and the two
// factors compose multiplicatively, because descheduling and slow I/O both
// stretch the same wall the fixture has to fill.
//
// The floor is not arbitrary: dedup is a (sig, board) invariant, not a
// volume claim — every class of the §7 acceptance (exactly N rows for N
// distinct sigs, zero duplicates, the recurring sig comments without growing
// the board) is exercised identically at 200 rows, and the assertion
// `len(lines) != n` follows the requested n either way, so no assertion is
// deleted and no threshold is relaxed. What shrinks is the volume of
// durability work: each row write is an fsync'd append (writeRow →
// appendLine → Sync, the §3.2 two-file durability contract) and the test is
// I/O-bound, not CPU-bound (measured: 15.8s wall / ~11 CPU-s, 77% of one
// core — the wall is the fixture's 2,000+ fsync'd appends), which is why the
// gate is Profile.Scale(), the filesystem-bound factor: CPUScale would leave
// the fixture untouched on a box whose disk is the slow half. The measured
// values are logged either way, because the fixture size is the thing a
// reader needs to interpret them.
//
// This replaces the load_avg-only ladder (load ≥ 4 → 8000/load rows), which
// inverted the comparison: load_avg says how BUSY a box is, never how FAST.
// QA-TROUBLE-5 measured the same commit rebuilding the same fixture in
// 9,747 ms at load 3.99 — graded against the tight reference numbers — while
// a busier box at load 10.78 got a 3.3× looser bar for identical work. The
// spec shape stays the bar on a quiet reference-class host; every other host
// is graded against its own measured speed.
func dedupRowsForHost(prof loadfence.Profile) int {
	n := int(float64(dedupRows) / prof.Scale())
	if n < minDedupRows {
		n = minDedupRows
	}
	if n > dedupRows {
		n = dedupRows
	}
	return n
}

// dedupRowsForLoad is the load axis alone — the same arithmetic as
// dedupRowsForHost with the I/O multiple at the reference. It exists so
// TestDedupRowsScaling can pin the load ladder exactly as the pre-§7a bound
// stated it, now as a special case of the host model.
func dedupRowsForLoad(load float64) int {
	return dedupRowsForHost(loadfence.Profile{Load: load})
}

// dedupFixtureRows resolves the fixture size the test runs at: the
// DEDUP_ROWS pin when valid, otherwise the host-calibrated default.
func dedupFixtureRows(prof loadfence.Profile) (int, bool) {
	if v := os.Getenv(EnvDedupRows); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n, true
		}
	}
	return dedupRowsForHost(prof), false
}

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
	// The host profile is measured ONCE for the whole fixture (SPEC-01 §7a):
	// the pilots' dir is the fixture's own board filesystem class, so the
	// I/O multiple describes the disk the rows are actually fsync'd onto.
	prof := loadfence.Measure(fx.board)
	n, pinned := dedupFixtureRows(prof)
	pinNote := "auto-bounded by " + prof.String()
	if pinned {
		pinNote = "pinned by " + EnvDedupRows
	}
	t.Logf("dedup fixture: %d distinct sigs against one board (%s; i/o scale %.2f; §7 spec shape on a quiet reference-class host is 1,000)",
		n, pinNote, prof.Scale())
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

// TestDedupRowsScaling pins the host-calibrated fixture bound (the TRBL-045
// curve-pinning shape): the no-/proc/loadavg fallback and the unloaded
// reference host both assert the §7 spec shape (1,000 rows — the size the §7
// table names) directly, a faster-than-reference host is clamped to the
// reference (a fixture is only ever loosened by calibration, never tightened
// — the SPEC-01 §7a contract), the I/O and load axes decay the fixture
// exactly (int(1000/scale) asserted at exact points), the two compose
// multiplicatively, the 200-row floor keeps every §7 acceptance class
// (N distinct sigs → exactly N rows; a recurring sig comments without
// growing the board) exercised, the DEDUP_ROWS pin wins over the automatic
// bound, a malformed pin does not, and at the spec wave the floor arithmetic
// is exact.
func TestDedupRowsScaling(t *testing.T) {
	cases := []struct {
		io   float64
		load float64
		want int
	}{
		{1, 0, 1000},   // no /proc/loadavg (load 0) on reference I/O → the spec shape
		{1, 3.9, 804},   // int(1000/1.24375): mild contention decays continuously
		{1, 16, 500},   // 1000/2: load alone, continuous with the spec shape
		{0.5, 0, 1000}, // a FASTER-than-reference host is clamped to the reference — never tightened
		{2, 0, 500},    // 1000/2: a 2× slower disk halves the fixture at zero load
		{5, 0, 200},    // the floor boundary on the I/O axis
		{8, 0, 200},    // the I/O clamp
		{2, 16, 250},   // 1000/4: slow disk AND contention compose multiplicatively
		{3, 16, 200},   // composition crossing the floor: int(1000/6) < 200
		{8, 120, 200},  // slow and heavily contended still floors at 200
	}
	for _, c := range cases {
		prof := loadfence.Profile{IOMultiple: c.io, CPUMultiple: 1, Load: c.load}
		if got := dedupRowsForHost(prof); got != c.want {
			t.Errorf("dedupRowsForHost(io %.2f, load %.2f) = %d, want %d (scale %.4f)",
				c.io, c.load, got, c.want, prof.Scale())
		}
	}
	// The floor keeps every acceptance class exercised: at 200 rows the test
	// still files 200 distinct sigs (→ exactly 200 rows) plus a recurring sig
	// (→ comments, never a second row).
	if minDedupRows < 2 {
		t.Errorf("minDedupRows = %d: the fixture floor must leave room for the recurrence case", minDedupRows)
	}
	// dedupRowsForLoad is the load axis of the same model: identical
	// arithmetic to dedupRowsForHost with the I/O multiple at the reference —
	// asserted here so the helper cannot silently diverge from the host
	// model.
	for _, load := range []float64{0, 3.9, 16, 64, 120} {
		want := dedupRowsForHost(loadfence.Profile{IOMultiple: 1, CPUMultiple: 1, Load: load})
		if got := dedupRowsForLoad(load); got != want {
			t.Errorf("dedupRowsForLoad(%.2f) = %d, want %d (the load axis must track the host model exactly)", load, got, want)
		}
	}
	// The pin wins; a malformed pin does not.
	prof := loadfence.Profile{IOMultiple: 1, CPUMultiple: 1, Load: 0}
	t.Setenv(EnvDedupRows, "700")
	if n, pinned := dedupFixtureRows(prof); !pinned || n != 700 {
		t.Errorf("dedupFixtureRows with DEDUP_ROWS=700 = (%d, %v), want (700, true)", n, pinned)
	}
	t.Setenv(EnvDedupRows, "nope")
	if n, pinned := dedupFixtureRows(prof); pinned || n != dedupRows {
		t.Errorf("dedupFixtureRows with a malformed pin = (%d, %v), want the default (false)", n, pinned)
	}
	t.Setenv(EnvDedupRows, "")
	if n, pinned := dedupFixtureRows(prof); pinned || n != dedupRows {
		t.Errorf("dedupFixtureRows unset = (%d, %v), want (%d, false)", n, pinned, dedupRows)
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
