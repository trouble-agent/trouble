package sensors

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// reload_test.go covers SPEC-03 §7's reload row: swap atomicity under load,
// in-flight snapshot stability, timer carry-over, the invalid-file refusal that
// keeps the previous set firing, and the budgets/strike loop guard.

func ruleBody(name, op, value string, forD string) string {
	if forD == "" {
		forD = "0s"
	}
	return fmt.Sprintf(`
[[rule]]
name = "%s"
source = "psi"
for = "%s"
[[rule.match]]
field = "some_avg10"
op = "%s"
value = "%s"
value_type = "number"
`, name, forD, op, value)
}

// TestReloadSwapAtomicity drives evaluations concurrently with reloads and
// asserts no evaluation ever saw a mixed set: each observed generation is
// internally consistent (all rules of one generation).
func TestReloadSwapAtomicity(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("gen_rule", ">=", "1", "0s")+ruleBody("gen_rule2", ">=", "1", "0s"))
	h.mustReload()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var mixes int64
	var mu sync.Mutex
	gens := map[uint64]int{}

	// Evaluator: observes the installed generation and the rule names in it.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rs := h.s.rules.Load()
				if rs == nil {
					continue
				}
				names := map[string]bool{}
				for _, crs := range rs.bySource {
					for _, cr := range crs {
						names[cr.rule.Name] = true
					}
				}
				// Every generation writes both names, so a mixed set would show
				// exactly one of them.
				a, b := names["gen_rule"], names["gen_rule2"]
				mu.Lock()
				gens[rs.gen]++
				if a != b {
					mixes++
				}
				mu.Unlock()
				h.s.handleEvent(context.Background(), psiEvent("io", 5.0, false))
			}
		}()
	}

	for i := 0; i < 200; i++ {
		body := ruleBody("gen_rule", ">=", "1", "0s") + ruleBody("gen_rule2", ">=", "1", "0s")
		if i%2 == 1 {
			body = ruleBody("gen_rule", ">=", "2", "0s") + ruleBody("gen_rule2", ">=", "2", "0s")
		}
		h.writeRules("10.toml", body)
		h.mustReload()
	}
	close(stop)
	wg.Wait()
	if mixes != 0 {
		t.Fatalf("%d evaluations observed a mixed rule set", mixes)
	}
	mu.Lock()
	n := len(gens)
	mu.Unlock()
	if n < 2 {
		t.Fatalf("the evaluators observed only %d generations; the test proved nothing", n)
	}
	t.Logf("observed %d distinct generations across 200 reloads with 4 concurrent evaluators", n)
}

// TestReloadCarriesStabilizationTimer: an unchanged predicate keeps its elapsed
// window (±1ms); a changed one resets and is recorded.
func TestReloadCarriesStabilizationTimer(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "10s"))
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	h.advance(5 * time.Second)
	key := ruleSigKey{rule: "io_pressure", sig: psiEvent("io", 41.7, false).Sig.String()}
	held := h.s.stab.held(key, h.now())
	if held < 4*time.Second {
		t.Fatalf("held = %s, want ~5s", held)
	}
	// A reload with a byte-identical predicate carries the window over.
	h.mustReload()
	after := h.s.stab.held(key, h.now())
	if diff := after - held; diff > time.Millisecond || diff < -time.Millisecond {
		t.Fatalf("carry-over changed the elapsed window by %s", diff)
	}
	// A changed predicate resets it and the reset is recorded.
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "36", "10s"))
	h.mustReload()
	if got := h.s.stab.held(key, h.now()); got != 0 {
		t.Fatalf("a changed match must reset the timer, held = %s", got)
	}
	resets := h.recordsWhere(func(r types.Record) bool {
		v, ok := r.Payload["rule_state_reset"].(bool)
		return ok && v
	})
	if len(resets) != 1 {
		t.Fatalf("expected exactly one rule_state_reset record, got %d", len(resets))
	}
}

// TestReloadInvalidFileKeepsPreviousSet: SPEC-03 §3.7's core promise.
func TestReloadInvalidFileKeepsPreviousSet(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "0s"))
	h.mustReload()
	before := h.s.ruleCount()
	if before != 1 {
		t.Fatalf("setup: %d rules", before)
	}
	h.writeRules("20-broken.toml", "[[rule]]\nname = \"nope\"\nsource = \"psi\"\n[[rule.match]]\nfield = \"not_a_field\"\nop = \"==\"\nvalue = \"x\"\n")
	err := h.s.Reload(context.Background())
	if err == nil {
		t.Fatal("a broken rules.d must refuse the reload")
	}
	if got := h.s.ruleCount(); got != before {
		t.Fatalf("the previous set must stay active: %d -> %d rules", before, got)
	}
	// The previous set is still firing.
	h.s.handleEvent(context.Background(), psiEvent("io", 41.7, false))
	if len(h.fired()) == 0 {
		t.Fatal("the previous rule set must keep firing after a refused reload")
	}
	// Exactly one TROUBLE-SENSORS-019 record, naming the file.
	recs := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["kind"] == "rule_reload"
	})
	if len(recs) != 1 {
		t.Fatalf("expected one rule_reload record, got %d", len(recs))
	}
	if code, _ := recs[0].Payload["error_code"].(string); !strings.HasPrefix(code, "TROUBLE-SENSORS-0") {
		t.Fatalf("rule_reload record must carry an error_code, got %q", code)
	}
	if h.s.rt[types.SenPSI] == nil || h.s.rt[types.SenPSI].reason.Load() == nil {
		t.Fatal("the health reason must carry the reload failure")
	}
	if r := *h.s.rt[types.SenPSI].reason.Load(); !strings.Contains(r, "rule reload failed") {
		t.Fatalf("health reason = %q", r)
	}
}

// TestReloadDeletedRuleDropsTimerAndKeepsIncident: the timer goes, the incident
// is not retracted (it belongs to the ladder).
func TestReloadDeletedRuleDropsTimerAndKeepsIncident(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "30s"))
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	key := ruleSigKey{rule: "io_pressure", sig: psiEvent("io", 41.7, false).Sig.String()}
	h.advance(time.Second)
	if h.s.stab.held(key, h.now()) == 0 {
		t.Fatal("setup: no timer started")
	}
	h.writeRules("10.toml", ruleBody("other_rule", ">=", "1", "0s"))
	h.mustReload()
	if got := h.s.stab.held(key, h.now()); got != 0 {
		t.Fatalf("a removed rule must drop its timer, held = %s", got)
	}
	for _, r := range h.snapshot() {
		if v, ok := r.Payload["kind"].(string); ok && v == "incident_retracted" {
			t.Fatal("a reload must never retract an incident")
		}
	}
}

// TestReloadBudget256Rules asserts the §3.7 budget: 256 rules reload in ≤50ms
// on the reference box, and CI asserts <250ms on a shared host.
func TestReloadBudget256Rules(t *testing.T) {
	h := newHarness(t)
	var b strings.Builder
	for i := 0; i < 256; i++ {
		b.WriteString(ruleBody(fmt.Sprintf("rule_%03d", i), ">=", "35", "10s"))
	}
	h.writeRules("10.toml", b.String())
	start := time.Now()
	h.mustReload()
	d := time.Since(start)
	if d > 250*time.Millisecond {
		t.Fatalf("256-rule reload took %s, CI budget is 250ms (spec: ≤50ms on the reference box)", d)
	}
	if got := h.s.ruleCount(); got != 256 {
		t.Fatalf("loaded %d rules, want 256", got)
	}
	t.Logf("measured: 256-rule reload in %s", d)
}

// TestReloadTimeoutAbandonsAndKeepsPrevious exercises the >1s path.
func TestReloadTimeoutAbandonsAndKeepsPrevious(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "0s"))
	h.mustReload()
	before := h.s.ruleCount()
	// Shrink the deadline so the abandon path is exercised deterministically
	// instead of by building a 1-second workload.
	h.s.reloadDeadline = time.Nanosecond
	defer func() { h.s.reloadDeadline = 0 }()
	err := h.s.Reload(context.Background())
	if err == nil {
		t.Fatal("a reload past its deadline must be abandoned")
	}
	if got := h.s.ruleCount(); got != before {
		t.Fatalf("the previous set must be kept on a timeout: %d -> %d", before, got)
	}
	recs := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["reason"] == "reload_timeout"
	})
	if len(recs) != 1 {
		t.Fatalf("expected one reload_timeout record, got %d", len(recs))
	}
}

// TestThreeStrikesDisableInotifyTrigger: after 3 consecutive failures the
// inotify trigger stops retrying; SIGHUP and the mtime sweep keep going.
func TestThreeStrikesDisableInotifyTrigger(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "0s"))
	h.mustReload()
	h.writeRules("10.toml", "[[rule]]\nname = \"broken\"\nsource = \"nope\"\n")
	for i := 0; i < reloadStrikeLimit; i++ {
		if err := h.s.Reload(context.Background()); err == nil {
			t.Fatalf("strike %d: expected a refusal", i+1)
		}
	}
	if !h.s.inotifyOff.Load() {
		t.Fatal("three consecutive failed reloads must disable the inotify trigger")
	}
	if h.s.strikes.Load() < reloadStrikeLimit {
		t.Fatalf("strikes = %d", h.s.strikes.Load())
	}
}

// TestNoRetroactiveEvaluation: a new rule does not replay the ledger; it applies
// to events observed after the swap.
func TestNoRetroactiveEvaluation(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("first", ">=", "35", "0s"))
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	firedBefore := len(h.fired())
	h.writeRules("10.toml", ruleBody("second", ">=", "1", "0s"))
	h.mustReload()
	if got := len(h.fired()); got != firedBefore {
		t.Fatalf("the reload minted %d incidents from history", got-firedBefore)
	}
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	if len(h.fired()) != firedBefore+1 {
		t.Fatal("a new rule must apply to events observed after the swap")
	}
}

// TestRuleDirMTimesDetectsChange proves the 60s reload backstop has a real change
// detector rather than a hopeful one (SPEC-03 §3.7a).
//
// All four halves are deterministic on ANY filesystem: none of them relies on the
// clock advancing between two writes, so a coarse-granularity filesystem, a
// tar-synced tree or a restored tree cannot make the requirement unassertable.
func TestRuleDirMTimesDetectsChange(t *testing.T) {
	// (a) CONTENT-ONLY change, with size and mtime deliberately frozen. This is
	// the hostile condition — harsher than any real filesystem — and it is what
	// the pre-fix path/size/mtime fingerprint could not see.
	t.Run("content-only change inside a frozen timestamp tick", func(t *testing.T) {
		h := newHarness(t)
		rulesDir := filepath.Join(h.dir, "rules.d")
		first := ruleBody("aaa", ">=", "1", "0s")
		second := ruleBody("bbb", ">=", "1", "0s")
		if len(first) != len(second) {
			t.Fatalf("premise: the two bodies must be the same byte length, got %d and %d", len(first), len(second))
		}
		p := h.writeRules("10.toml", first)
		if err := os.Chtimes(p, time.Unix(1700000000, 0), time.Unix(1700000000, 0)); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		pinnedAt, pinnedSize := fi.ModTime(), fi.Size()

		before, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}

		h.writeRules("10.toml", second)
		if err := os.Chtimes(p, pinnedAt, pinnedAt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		after, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}

		// Assert the premise: the hostile condition really is in force, so a
		// failure below is the detector's, not the fixture's.
		fi2, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi2.Size() != pinnedSize || !fi2.ModTime().Equal(pinnedAt) {
			t.Fatalf("premise: size/mtime must be unchanged, got size %d (want %d) mtime %v (want %v)",
				fi2.Size(), pinnedSize, fi2.ModTime(), pinnedAt)
		}
		if before == after {
			t.Fatalf("content-only change missed: fingerprint stayed %s although the bytes changed while size (%d) and mtime (%v) did not",
				before, fi2.Size(), fi2.ModTime())
		}
	})

	// (b) The natural edit path: rewrite and POLL, so a filesystem whose
	// timestamps move only on the second still passes, and a detector that never
	// notices still fails.
	t.Run("natural edit is detected within a polled deadline", func(t *testing.T) {
		h := newHarness(t)
		rulesDir := filepath.Join(h.dir, "rules.d")
		h.writeRules("10.toml", ruleBody("a", ">=", "1", "0s"))
		before, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		h.writeRules("10.toml", ruleBody("b", ">=", "1", "0s"))
		deadline := time.Now().Add(3 * time.Second)
		after := before
		for {
			cur, err := rulesMTimes(rulesDir)
			if err != nil {
				t.Fatalf("rulesMTimes: %v", err)
			}
			if cur != before {
				after = cur
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a natural edit to 10.toml was not detected within 3s: the fingerprint is still %s", before)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if after == before {
			t.Fatalf("the fingerprint did not change after an edit")
		}
	})

	// (c) STABILITY: nothing changed ⇒ identical fingerprint, so the backstop
	// does not reload on every tick.
	t.Run("stable while nothing changes", func(t *testing.T) {
		h := newHarness(t)
		rulesDir := filepath.Join(h.dir, "rules.d")
		h.writeRules("10.toml", ruleBody("a", ">=", "1", "0s"))
		one, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		oneAgain, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		if one != oneAgain {
			t.Fatalf("one-file dir: consecutive fingerprints differ (%s vs %s) with nothing changed — that is a reload storm", one, oneAgain)
		}

		h.writeRules("20.toml", ruleBody("b", ">=", "2", "0s"))
		h.writeRules("30.toml", ruleBody("c", ">=", "3", "0s"))
		many, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		manyAgain, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		if many != manyAgain {
			t.Fatalf("three-file dir: consecutive fingerprints differ (%s vs %s) with nothing changed — that is a reload storm", many, manyAgain)
		}
	})

	// (d) FILE SET: add and remove are both changes.
	t.Run("file set changes are detected", func(t *testing.T) {
		h := newHarness(t)
		rulesDir := filepath.Join(h.dir, "rules.d")
		h.writeRules("10.toml", ruleBody("a", ">=", "1", "0s"))
		one, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		added := h.writeRules("20.toml", ruleBody("b", ">=", "2", "0s"))
		two, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		if one == two {
			t.Fatalf("adding rules.d/20.toml did not change the fingerprint (%s)", one)
		}
		if err := os.Remove(added); err != nil {
			t.Fatalf("remove: %v", err)
		}
		three, err := rulesMTimes(rulesDir)
		if err != nil {
			t.Fatalf("rulesMTimes: %v", err)
		}
		if three == two {
			t.Fatalf("removing rules.d/20.toml did not change the fingerprint (%s)", two)
		}
		if three != one {
			t.Fatalf("removing the added file did not restore the original fingerprint: %s (want %s)", three, one)
		}
	})

	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("a", ">=", "1", "0s"))
	if _, err := rulesDirFiles(filepath.Join(h.dir, "rules.d")); err != nil && err != fs.ErrNotExist {
		t.Fatalf("rulesDirFiles: %v", err)
	}
}
