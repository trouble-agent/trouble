package ledger

// TRBL-092: the 3-day fault soak (4306 samples, 95 faults, all recovered; RSS
// 37.2 MB → 44.5 MB, max 58.6 MB; ledger 256 → 158,998 records) grew while the
// record count grew. The index is where a linear-in-records structure hurts, so
// these tests pin the bounds §3.6 requires on the three structures whose key
// spaces scale with the number of DISTINCT groups ever seen rather than with
// the live working set:
//
//   - `cold` — the retention shadow of rung 4. evictIfNeededLocked parks the
//     victim there; without a second eviction pass the combined live+cold
//     footprint (and the group entries behind it) grew one entry per eviction
//     forever.
//   - the parallel rank arrays `order`/`counts`/`rates`/`trends`/`retired` —
//     one slot per group ever created, never released.
//   - `mergeToDigest` — a last-writer-wins reverse index. A group that re-derives
//     its merge key per arrival left one stale entry per distinct key ever seen.
//
// `sigToDigest` is deliberately NOT bounded to one entry per group: it is the
// many-to-one alias table `incidentForSig` uses to resolve any arrival path's sig
// to the group/incident (AC-22 — three arrival paths sharing one merge key reach
// ONE incident). Its bound is the live+cold group count, reached by evicting the
// groups themselves (which is what TestSigAliasesSurviveWithinEvictionBound
// proves: aliases survive an eviction, and the map stops growing once the group
// churn is the only input).

import (
	"fmt"
	"testing"
	"time"
)

// churnDistinctGroups appends n events, each a distinct group (digest, sig AND
// merge key all differ): the high-cardinality shape a per-request, per-unit or
// per-mount signature space produces under sustained load. The fake clock
// advances one minute per arrival so the eviction's least-recently-seen
// ordering is deterministic.
func churnDistinctGroups(t *testing.T, l *Ledger, clk *fakeClock, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d := eventDraft("psi",
			fmt.Sprintf("psi:sha256v1:%04d", i),
			fmt.Sprintf("%064d", i), 0)
		// the fixture's merge key is one shared value; the leak shape needs a
		// distinct key per group, so override it with a real MergeKey() product
		d.Payload["merge_key"] = MergeKey("resource_exhaustion", fmt.Sprintf("subject-%04d", i))
		mustAppend(t, l, d)
		clk.Advance(time.Minute)
	}
}

// TestColdGroupsBounded: after 4×MaxGroups distinct groups churn through the
// index, the cold retention shadow must stay at its own bound and live+cold at
// 2× that bound. RED on the pre-TRBL-092 code: cold grew one entry per eviction
// forever (32 measured after 64 arrivals, unbounded in the arrival count).
func TestColdGroupsBounded(t *testing.T) {
	clk := newFakeClock(testNow())
	const max = 16
	l := testLedger(t, clk, func(o *Options) { o.Index.MaxGroups = max })
	churnDistinctGroups(t, l, clk, max*4)
	l.idx.mu.RLock()
	cold := len(l.idx.cold)
	live := len(l.idx.groups)
	l.idx.mu.RUnlock()
	if cold > max {
		t.Errorf("cold groups = %d, want <= %d (the retention shadow must not grow with the arrival count)", cold, max)
	}
	if live+cold > 2*max {
		t.Errorf("live+cold groups = %d, want <= %d (neither map may grow without bound)", live+cold, 2*max)
	}
}

// TestSlotArraysBounded: the parallel rank arrays must not keep one slot per
// distinct group ever seen — a dropped cold group's slot is reusable.
func TestSlotArraysBounded(t *testing.T) {
	clk := newFakeClock(testNow())
	const max = 16
	l := testLedger(t, clk, func(o *Options) { o.Index.MaxGroups = max })
	churnDistinctGroups(t, l, clk, max*8)
	l.idx.mu.RLock()
	slots := len(l.idx.order)
	counts := len(l.idx.counts)
	retired := len(l.idx.retired)
	l.idx.mu.RUnlock()
	for name, n := range map[string]int{"order": slots, "counts": counts, "retired": retired} {
		if n > 2*max {
			t.Errorf("%s = %d slots after %d distinct groups, want <= %d (slots of dropped groups must be reused)", name, n, max*8, 2*max)
		}
	}
}

// TestMergeToDigestBounded: a group that rotates its merge key every arrival
// must retain exactly ONE entry, not one per distinct key ever seen.
func TestMergeToDigestBounded(t *testing.T) {
	clk := newFakeClock(testNow())
	const rotations = 1000
	l := testLedger(t, clk, func(o *Options) { o.Index.MaxGroups = 16 })
	clk.Advance(time.Minute)
	for i := 0; i < rotations; i++ {
		d := eventDraft("psi", "psi:seed", "shared-digest", 0)
		d.Payload["merge_key"] = MergeKey("resource_exhaustion", fmt.Sprintf("subject-%04d", i))
		mustAppend(t, l, d)
		clk.Advance(time.Minute)
	}
	l.idx.mu.RLock()
	n := len(l.idx.mergeToDigest)
	l.idx.mu.RUnlock()
	if n > 1 {
		t.Errorf("mergeToDigest = %d entries after %d merge-key rotations, want 1", n, rotations)
	}
}

// TestSigAliasesSurviveWithinEvictionBound: sig aliases of LIVE and COLD groups
// must survive (AC-22 needs them to reach one incident), while the group churn
// alone must not push the alias map past one entry per live+cold group. RED on
// the pre-TRBL-092 code, where evictions left stale aliases behind without
// bound; this also fails if a future "fix" prunes aliases on re-key (which
// breaks AC-22 — TestAC22DedupTotality is the guard for that half).
func TestSigAliasesSurviveWithinEvictionBound(t *testing.T) {
	clk := newFakeClock(testNow())
	const max = 16
	l := testLedger(t, clk, func(o *Options) { o.Index.MaxGroups = max })
	churnDistinctGroups(t, l, clk, max*4)
	l.idx.mu.RLock()
	sigs := len(l.idx.sigToDigest)
	groups := len(l.idx.groups) + len(l.idx.cold)
	l.idx.mu.RUnlock()
	if groups == 0 {
		t.Fatalf("no groups survived the churn")
	}
	if sigs > groups {
		t.Errorf("sigToDigest = %d entries for %d live+cold groups, want <= groups (aliases of dropped groups must be pruned)", sigs, groups)
	}
}
