package scrub

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/trouble-agent/trouble/internal/loadfence"
	"github.com/trouble-agent/trouble/internal/types"
)

// The rule-timeout fence (QA-TROUBLE-030).
//
// scrub.rule_timeout is defined as the CPU time ONE rule evaluation may take
// (SPEC-02 §3.2/§3.8). It used to be enforced against a raw wall-clock reading —
// `if d := time.Since(start); d > e.cfg.ruleTimeout` — which bills the time the
// OS took away from this goroutine to the rule, and fails the whole payload
// closed with TROUBLE-SCRUB-005. On an oversubscribed host that is not a product
// verdict: a rule that costs microseconds loses the race against a 250 ms budget
// as soon as the scheduler stops running it for a quarter of a second. It cost
// tier 1 of four separate GitReins runs on internal/scrub alone, every other
// package green. From run 04543947 at load_avg_1m 35.89:
//
//	TROUBLE-SCRUB-005 (rule_timeout): pii_email_ip: rule evaluation exceeded 250ms
//	TestPrefilterEquivalence input 959: pii_identity_kv: rule evaluation exceeded 250ms
//
// Because tier 1 gates every verdict, that one flake gates the whole queue.
//
// The fence charges elapsed-minus-runqueue-wait instead. loadfence.ThreadWait
// samples the running thread's cumulative schedstat wait either side of a rule
// evaluation; that difference is the part of the window the OS did not give to
// the rule, and what remains is what the budget is compared against. It is
// subtractive, never multiplicative: the configured number keeps its meaning
// ("CPU time for this rule") and is never widened, and the discount is bounded
// by the window itself, so a verdict can only move toward the one a quiet host
// would have produced. Unreadable schedstat (a kernel without it, a hidepid
// mount, a non-Linux host) yields a zero discount and exactly today's behaviour:
// a fence that cannot measure must never tighten.
//
// The runqueue wait is the right quantity rather than the thread's CPU time
// because a rule may legitimately block: the fail-closed fixtures prove SCRUB-005
// with a rule that SLEEPS 40 ms under a 5 ms budget, and sleep is not runqueue
// wait, so those fixtures keep failing closed (a CPU-time budget would silently
// pass them).
//
// WHY THE SAMPLES BRACKET A SECOND EVALUATION, NOT EVERY EVALUATION
//
// The obvious shape — two samples around each rule — cannot be afforded on the
// ingress path, and this was measured rather than assumed on the same 16-core box
// the flakes came from (load_avg ~30): a fresh schedstat read costs 6.4 µs
// (11.3 µs through os.ReadFile), two per rule over an 18-rule full pass is
// ~230 µs, against a §3.9 budget of 60 µs for the WHOLE full-set 1 KiB pass
// (measured ~130 µs here). The fence would cost more than the work it fences and
// would fail TestScrubBudget on a reference-class host. Caching a handle instead
// of re-opening it does not work either: /proc/thread-self resolves at OPEN time
// and keeps reporting that thread, so after a goroutine migration the handle
// reads stale counters (measured: the on-CPU field did not move across 20M loop
// iterations).
//
// So the samples are taken only when a rule's raw wall budget was ALREADY
// exceeded. The rule is evaluated once bare — free, exactly as before — and if
// that reading blows the budget it is evaluated once more with the clock and the
// schedstat samples around it, and THAT reading decides. The happy path pays one
// time.Since and nothing else (TestRuleTimeoutFenceCostsNothingWhenTheBudgetHolds
// pins that), and the deciding measurement is a properly bracketed one. Both
// evaluations are of the same deterministic rule over the same bytes, so the
// spans the fence returns are the spans the bare evaluation produced; they are
// taken through evalRuleSpans, the accounting-free entry point, so the repeat
// cannot double-count the values the rule's exemptions left unchanged.
//
// Measured on this 16-core box at load_avg_1m 40.4-47.4, with 32 pinned, niced
// CPU burners and a probe that drives the ingest harness's shape (a 1 KiB
// four-field envelope through ScrubFields, 64 goroutines): the PRE-fix tree
// (a12a86e) failed 77 of 128,000 calls closed with a spurious SCRUB-005, the
// fixed tree 0 of 128,000. The rescue in that window came from the repeat
// evaluation itself — the stalled window was 250-770 ms while the repeat ran in
// microseconds — and the runqueue-wait discount covers the case where the repeat
// is descheduled too.
//
// A rule that is genuinely slow stays red: its second window is slow for the same
// reason the first was, so the charged duration still exceeds the budget and
// SCRUB-005 fires with its documented code, reason, rule name, message shape and
// counter updates. The discount is logged — at Warn when it saves a payload from
// a fail-closed verdict (that is the widening, and it must never be silent) and
// at Debug when the evidence did not explain the overrun (the error is already
// the operator-facing signal).

// threadWaitSnapshot is loadfence.ThreadWaitSnapshot behind a package variable so
// the fence's arithmetic can be driven deterministically: a test that had to
// manufacture real host descheduling would be the very flake this fence removes.
var threadWaitSnapshot = loadfence.ThreadWaitSnapshot

// ruleBudget is the ONE place a rule's budget is decided, shared by both rule
// loops — the single-value pass and ScrubFields' per-field walk. They used to
// carry identical copies of the wall-clock check, and a fix applied to one copy
// leaves the other flaking: QA-TROUBLE-030's first cut patched only pass(), and
// the ingest harness (ScrubFields, four fields per record) kept failing closed
// with "TROUBLE-SCRUB-005 (rule_timeout): env_assign" at load_avg 42 while a
// 64-goroutine probe of the same shape reported 23 timeouts and 0 fence
// invocations. One decision point, or the class of bug survives the next fix.
//
// spans is the bare evaluation of r over buf and wall its raw reading. When the
// rule fits, they are returned unchanged and the fence costs nothing. When it
// does not, the rule is re-measured under the fence and spans is that
// evaluation's result (identical by determinism), or the SCRUB-005 error is
// returned after the documented counter updates.
func (e *Engine) ruleBudget(r *compiledRule, tbl *ruleTable, buf []byte, reserved, spans []span, wall time.Duration) ([]span, error) {
	if e.cfg.ruleTimeout <= 0 || wall <= e.cfg.ruleTimeout {
		return spans, nil
	}
	charged, fenced, err := e.evalRuleFenced(r, tbl, buf, reserved, wall)
	if err != nil {
		e.st.failClosed.Add(1)
		return nil, err
	}
	if charged > e.cfg.ruleTimeout {
		e.st.timeouts.Add(1)
		e.st.failClosed.Add(1)
		return nil, errf(types.CodeScrub005, ReasonRuleTimeout, r.name,
			fmt.Sprintf("rule evaluation exceeded %s", e.cfg.ruleTimeout), nil)
	}
	return fenced, nil
}

// evalRuleFenced re-evaluates one rule with loadfence.ThreadWait samples either
// side of it. It returns the spans of that evaluation and the duration the rule
// is charged: its wall time minus the runqueue wait this thread accumulated
// inside the window, in [0, wall]. wall is the first (bare) reading, reported so
// the log line carries both numbers.
func (e *Engine) evalRuleFenced(r *compiledRule, tbl *ruleTable, buf []byte, reserved []span, wall time.Duration) (time.Duration, []span, error) {
	before := threadWaitSnapshot()
	start := time.Now()
	spans, _, err := e.evalRuleSpans(r, tbl, buf, reserved)
	elapsed := time.Since(start)
	after := threadWaitSnapshot()
	descheduled := after.RunqueueWaitSince(before, elapsed)
	if err != nil {
		return elapsed, nil, err
	}
	charged := elapsed - descheduled // RunqueueWaitSince returns at most elapsed
	budget := e.cfg.ruleTimeout
	fields := []any{
		"rule", r.name,
		"budget", budget,
		"first_wall", wall,
		"fenced_wall", elapsed,
		"descheduled", descheduled,
		"charged", charged,
	}
	if charged > budget {
		slog.Debug("scrub: rule_timeout fence found no descheduling to discount", fields...)
		return charged, spans, nil
	}
	slog.Warn("scrub: rule_timeout fence discounted host descheduling", fields...)
	return charged, spans, nil
}
