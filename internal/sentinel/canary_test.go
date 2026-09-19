package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestCanaryLandsThroughTheHTTPPath pins §3.8: the canary is injected as a real
// envelope over the listener, so routing → auth → envelope → scrub → sig →
// group → ledger are all exercised by the proof that ingestion works.
func TestCanaryLandsThroughTheHTTPPath(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "1" })
	defer ts.close()
	id, err := ts.s.InjectCanary("1")
	if err != nil {
		t.Fatalf("InjectCanary: %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("canary event id = %q, want 32 hex", id)
	}
	// One `canary` record per injection and one per observation.
	phases := map[string]int{}
	for _, rec := range ts.sink.ofKind(types.KCanary) {
		phases[fmt.Sprint(rec.Payload["phase"])]++
		if rec.Payload["sig"] != ts.s.CanarySig().String() {
			t.Errorf("canary record sig = %v, want the reserved canary sig", rec.Payload["sig"])
		}
	}
	if phases["injected"] != 1 || phases["observed"] != 1 {
		t.Fatalf("canary phases = %v, want one injected and one observed", phases)
	}
	project, lastTS, ok := ts.s.canaryState()
	if !ok || project != "1" || lastTS == "" {
		t.Fatalf("canary state = (%q,%q,%v), want a fresh observation", project, lastTS, ok)
	}
	rt, _ := ts.s.ProjectRuntime("1")
	if !rt.CanaryLastOK || rt.CanaryLastTS == "" {
		t.Fatalf("ProjectRuntime canary fields = (%v,%q), want ok", rt.CanaryLastOK, rt.CanaryLastTS)
	}
	// The canary also lands in the group index under the reserved sig.
	if _, ok := ts.s.GroupBySig(ts.s.CanarySig().String()); !ok {
		t.Fatal("the canary has no group: the injection did not go through the group path")
	}
}

// TestCanarySurvivesQuotaExhaustion pins §3.8's exemption: the canary must land
// during a flood, because that is exactly when evidence matters.
func TestCanarySurvivesQuotaExhaustion(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = "1"
		c.Projects[0].QuotaEPM = 1
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	defer ts.close()
	// Spend the quota and keep pushing: ordinary events are refused.
	_ = readBody(t, ts.postEvent(t, "1", fmt.Sprintf("%032x", 1)))
	for i := 0; i < 5; i++ {
		_ = readBody(t, ts.postEvent(t, "1", fmt.Sprintf("%032x", 100+i)))
	}
	if ts.s.counters.droppedQuota.Get() == 0 {
		t.Fatal("the quota was not exhausted; the test cannot prove the exemption")
	}
	before := ts.s.counters.canaryObserved.Get()
	if _, err := ts.s.InjectCanary("1"); err != nil {
		t.Fatalf("the canary must land during a quota breach: %v", err)
	}
	if ts.s.counters.canaryObserved.Get() != before+1 {
		t.Fatalf("canary observations = %d, want %d: the canary is exempt from quota (§3.8)",
			ts.s.counters.canaryObserved.Get(), before+1)
	}
}

// TestCanaryIsReserved pins §3.8's exclusion: the canary sig is reserved, so it
// never opens an incident and never reaches the ladder — and sentinel mints no
// incident record for it.
func TestCanaryIsReserved(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "1" })
	defer ts.close()
	if _, err := ts.s.InjectCanary("1"); err != nil {
		t.Fatalf("InjectCanary: %v", err)
	}
	sig := ts.s.CanarySig().String()
	if !ts.s.IsReservedSig(sig) {
		t.Fatalf("IsReservedSig(%q) = false, want true", sig)
	}
	if ts.s.IsReservedSig("sentinel:sha256v1:0000000000000000") {
		t.Fatal("an ordinary sig must not be reported as reserved")
	}
	for _, rec := range ts.sink.recs {
		if rec.Kind == types.KIncident {
			t.Fatalf("sentinel wrote an incident record: %v", rec.Payload)
		}
	}
}

// TestMissingCanaryEmitsGapAndMarksTheSourceDead pins §3.8: no observation within
// 2 × canary_interval is a `gap` plus a dead `sentinel` source and
// CanaryLastOK=false.
func TestMissingCanaryEmitsGapAndMarksTheSourceDead(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "1" })
	defer ts.close()
	cur := nowFunc()
	ts.s.now = func() time.Time { return cur }
	// No observation has landed yet.
	ts.s.checkCanaryAge()
	found := false
	for _, rec := range ts.sink.ofKind(types.KGap) {
		if rec.Payload["cause"] == "canary_missing" {
			found = true
		}
	}
	if !found {
		t.Fatal("no gap record with cause canary_missing")
	}
	if ts.s.counters.canaryMissing.Get() == 0 {
		t.Error("canary_missing_total did not move")
	}
	var sentinelSource *types.SourceLiveness
	ts.s.collectors.register("sentinel", nil)
	for i := range ts.s.Sources() {
		src := ts.s.Sources()[i]
		if src.Source == "sentinel" {
			s := src
			sentinelSource = &s
		}
	}
	if sentinelSource == nil {
		t.Fatal("Sources() has no sentinel entry")
	}
	if sentinelSource.Alive {
		t.Error("the sentinel source must be dead while the canary is missing")
	}
	if sentinelSource.MaxAgeS != 2*ts.s.cfg.CanaryInterval.Std().Seconds() {
		t.Errorf("max_age_s = %v, want 2 x canary_interval", sentinelSource.MaxAgeS)
	}
	rt, _ := ts.s.ProjectRuntime("1")
	if rt.CanaryLastOK {
		t.Error("CanaryLastOK must be false while the canary is missing")
	}
}

// TestCanaryRefreshClearsTheMiss pins the recovery side: a landed canary clears
// the missing state.
func TestCanaryRefreshClearsTheMiss(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "1" })
	defer ts.close()
	ts.s.checkCanaryAge()
	if _, _, ok := ts.s.canaryState(); ok {
		t.Fatal("canary state must be not-ok before the first observation")
	}
	if _, err := ts.s.InjectCanary("1"); err != nil {
		t.Fatalf("InjectCanary: %v", err)
	}
	ts.s.checkCanaryAge() // nothing to emit: the observation is fresh
	project, ts0, ok := ts.s.canaryState()
	if !ok || project != "1" || ts0 == "" {
		t.Fatalf("canary state = (%q,%q,%v), want a fresh observation", project, ts0, ok)
	}
	count := ts.s.counters.canaryMissing.Get()
	ts.s.checkCanaryAge()
	if ts.s.counters.canaryMissing.Get() != count {
		t.Fatal("a fresh canary observation must not emit another canary_missing gap")
	}
}

// TestCanaryLoopIsStartedByStart pins §4.2 step 5: Start returns only after a
// first canary observation (or one interval), so boot never claims readiness
// untested.
func TestCanaryLoopIsStartedByStart(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = "1"
		c.CanaryInterval = types.Duration("50ms")
	})
	defer ts.close()
	ts.s.cfg.Bind = strings.TrimPrefix(ts.ts.URL, "http://")
	if err := ts.s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, _, ok := ts.s.canaryState(); !ok {
		t.Fatal("Start returned without a canary observation")
	}
	if err := ts.s.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// TestWriteCursorIsSixtyHundred pins the state-file permissions of §3.5.
func TestWriteCursorIsSixtyHundred(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/collectors/journal-abc.cursor"
	if err := writeCursor(path, "s=1;i=2;b=3;m=4;t=5;x=6", "now"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("cursor permissions = %o, want 0600", st.Mode().Perm())
	}
	// The offset file has the same discipline.
	off := dir + "/collectors/py-traceback-abc.offset"
	if err := writeOffset(off, 123); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(off)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Mode().Perm() != 0o600 {
		t.Fatalf("offset permissions = %o, want 0600", st2.Mode().Perm())
	}
	if got := readOffset(off); got != 123 {
		t.Fatalf("offset round trip = %d, want 123", got)
	}
}

// TestDrainFlushesGroups pins §4.2 step 6: drain flushes every group counter.
func TestDrainFlushesGroups(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.GroupFlush = types.Duration("20ms")
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ev := groupEvent("aaaaaaaa11112222333344445555aaaa", "payment-api@2.4.1")
	ev.SourceKind = sourceGeneric
	if _, err := ts.s.admitEvent(context.Background(), entry, ev, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	ts.s.groups.markDropped(mustDigest(t, ev), 1, 0)
	if err := ts.s.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	flush := 0
	for _, rec := range ts.sink.ofKind(types.KGroup) {
		if rec.Payload["op"] == "flush" {
			flush++
		}
	}
	if flush == 0 {
		t.Fatal("Drain wrote no flush record: group counters would be lost")
	}
}

// mustDigest computes an event's digest hex (test helper).
func mustDigest(t *testing.T, ev *rawEvent) string {
	t.Helper()
	n := newNormalizer()
	canonical, _ := n.canonicalFor(ev)
	return n.sigOfCanonical(canonical).DigestHex()
}

// pinLedgerClock pins the e2e chain's ledger clock to a fixed instant for the
// duration of the test. Only the ledger's clock is pinned: the ledger is the
// side that stamps records (types.TsLayout, millisecond), while the sentinel's
// own process clock is not part of any window comparison and stays real so
// Start's first-observation wait keeps a wall-clock deadline.
func pinLedgerClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := e2eLedgerNow
	e2eLedgerNow = func() time.Time { return at }
	t.Cleanup(func() { e2eLedgerNow = prev })
}

// TestCanarySeenInTheLedgerSourceIndex pins §7's canary row pass threshold at
// the read the verification side is built on: after a canary observation, the
// ledger's per-source index reports the `sentinel` source — the origin every
// sentinel-written record carries — with canary_seen=true and a canary_last_ts
// inside canary_interval. `Evidence.CanarySeen` (SPEC-TYPES §4.1, the field
// SPEC-05's `CanarySeen == false ⇒ invalid` rule consumes) is derived from that
// read; the canary tests above assert the in-process projection
// (canaryState, ProjectRuntime.CanaryLastOK, Sources liveness) only, so this is
// the antecedent pin that was missing.
func TestCanarySeenInTheLedgerSourceIndex(t *testing.T) {
	// The interval is the bound the assertion is measured against, so a slow
	// first commit cannot false-fail it; the observation itself is expected in
	// the first interval, which is also what Start waits for (§4.2 step 5).
	const interval = 10 * time.Second
	// The ledger renders every record TS through types.TsLayout, which TRUNCATES
	// to the millisecond, and this assertion dates the observation from the boot
	// instant — so the two sides must share one clock. Reading boot from
	// time.Now (nanoseconds) against the ledger's millisecond reading made an
	// observation that landed inside boot's own millisecond compare as up to 1ms
	// BEFORE boot, i.e. a truncation artifact read as an early canary (~1 run in
	// 10 of the real-clock suite). The pinned instant sits 500µs past a
	// millisecond boundary, the phase the truncation crosses: every run now
	// exercises that boundary instead of depending on where the wall clock was.
	boot := time.Date(2026, 9, 19, 13, 25, 0, 500_000, time.UTC)
	pinLedgerClock(t, boot)
	e := newE2EChain(t, func(c *Config) {
		c.CanaryProject = "1"
		c.CanaryInterval = types.Duration(interval.String())
	})
	// Boot as the clock that stamps the observation saw it.
	t0 := e2eLedgerNow()
	e.open(t)

	canaryRecords := func() (injections, observations int) {
		if err := e.l.Query().ScanFrom(1, func(rec types.Record) bool {
			if rec.Kind != types.KCanary || rec.Origin.Source != "sentinel" {
				return true
			}
			switch rec.Payload["phase"] {
			case "injected":
				injections++
			case "observed":
				observations++
			}
			return true
		}); err != nil {
			t.Fatalf("ledger scan: %v", err)
		}
		return injections, observations
	}

	// The writer is group-commit: the record pair §3.8 pins (one per injection,
	// one per observation) and the index row derived from it become readable a
	// commit after the observation, so both reads are awaited under one deadline.
	// The deadline is harness wall time on purpose: it must keep bounding this
	// loop even if a future change pins the sentinel's process clock.
	var canary types.SourceAge
	var injections, observations int
	deadline := time.Now().Add(5 * time.Second)
	for {
		ages, _, err := e.l.Query().Sources()
		if err != nil {
			t.Fatalf("ledger Sources: %v", err)
		}
		found := false
		for i := range ages {
			if ages[i].Source == "sentinel" && ages[i].HostID == e.s.cfg.HostID {
				canary, found = ages[i], true
			}
		}
		injections, observations = canaryRecords()
		if found && canary.CanarySeen && injections == 1 && observations == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ledger never showed the canary once committed: sources = %+v, injected = %d, observed = %d",
				ages, injections, observations)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if canary.CanaryLastTS == "" {
		t.Fatal("canary_seen is true but canary_last_ts is empty: verification cannot date the observation")
	}
	obs, err := types.ParseUTC(canary.CanaryLastTS)
	if err != nil {
		t.Fatalf("canary_last_ts %q is not a UTC timestamp: %v", canary.CanaryLastTS, err)
	}
	// §7: "canary observation inside canary_interval". Both sides are read at
	// the ledger's resolution — the stored TS is truncated to the millisecond by
	// types.TsLayout and boot is floored the same way — so an observation in
	// boot's own millisecond cannot read as earlier than boot.
	if !canaryInsideInterval(obs, t0, interval) {
		t.Fatalf("canary observation at %s is %v after boot, want inside canary_interval (%v)",
			canary.CanaryLastTS, obs.Sub(t0.Truncate(time.Millisecond)), interval)
	}
	if canary.LastEventTS == "" {
		t.Error("the sentinel source has no last_event_ts")
	} else if canary.LastEventAgeS < 0 || canary.LastEventAgeS > interval.Seconds() {
		t.Errorf("sentinel last_event_age_s = %v, want inside canary_interval (%v)",
			canary.LastEventAgeS, interval.Seconds())
	}
	// The reading is recorded where the rest of the sentinel operating notes
	// live, and the record names this test the way the other §9 rows name theirs,
	// so the documented antecedent cannot drift away from the pinned one.
	doc := readOperationsDoc(t)
	for _, want := range []string{
		"TestCanarySeenInTheLedgerSourceIndex",
		"`canary_seen=true`",
		"`Evidence.CanarySeen`",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/operations.md §9 does not record the canary antecedent: missing %q", want)
		}
	}
}

// canaryInsideInterval reports whether a ledger-stamped observation timestamp
// falls inside [boot, boot+interval]. The ledger stamps every record TS through
// types.TsLayout, which truncates to the millisecond, so boot is floored to
// that same resolution before the comparison: an observation that landed in
// boot's own millisecond is inside the window, not up to 1ms before it. The
// window itself is NOT widened — an observation earlier than the floored boot
// instant, or later than boot+interval, is still outside.
func canaryInsideInterval(obs, boot time.Time, interval time.Duration) bool {
	boot = boot.Truncate(time.Millisecond)
	return !obs.Before(boot) && obs.Sub(boot) <= interval
}

// TestCanaryWindowStillRejectsEarlyAndLateObservations keeps the §7 window's
// teeth: reading both sides at the ledger's resolution must not have turned
// "canary observation inside canary_interval" into a check that cannot fail.
func TestCanaryWindowStillRejectsEarlyAndLateObservations(t *testing.T) {
	// The same instant the ledger-pinned canary test boots at: 500µs past a
	// millisecond boundary, so the truncation boundary is the case under test.
	boot := time.Date(2026, 9, 19, 13, 25, 0, 500_000, time.UTC)
	const interval = 10 * time.Second
	cases := []struct {
		name string
		obs  time.Time
		want bool
	}{
		// The truncation artifact this fix exists for: the ledger can only
		// store boot's own millisecond, and that reading is not "before boot".
		{"observation truncated into boot's own millisecond", boot.Truncate(time.Millisecond), true},
		{"observation at boot exactly", boot, true},
		{"observation exactly at the interval bound", boot.Truncate(time.Millisecond).Add(interval), true},
		{"observation one millisecond past the interval", boot.Truncate(time.Millisecond).Add(interval + time.Millisecond), false},
		{"observation one millisecond before boot", boot.Truncate(time.Millisecond).Add(-time.Millisecond), false},
		{"observation a second before boot", boot.Add(-time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canaryInsideInterval(tc.obs, boot, interval); got != tc.want {
				t.Errorf("canaryInsideInterval(%s, %s, %v) = %v, want %v",
					tc.obs.UTC().Format(types.TsLayout), boot.Format(types.TsLayout), interval, got, tc.want)
			}
		})
	}
}

// TestCanaryRecordShape pins the canary payload keys SPEC-05/SPEC-10 read.
func TestCanaryRecordShape(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "1" })
	defer ts.close()
	if _, err := ts.s.InjectCanary("1"); err != nil {
		t.Fatalf("InjectCanary: %v", err)
	}
	sawObserved := false
	for _, rec := range ts.sink.ofKind(types.KCanary) {
		switch rec.Payload["phase"] {
		case "injected":
			for _, key := range []string{"iteration", "event_id", "project", "sig"} {
				if _, ok := rec.Payload[key]; !ok {
					t.Errorf("injected canary record is missing %q", key)
				}
			}
		case "observed":
			sawObserved = true
			for _, key := range []string{"event_id", "project", "sig"} {
				if _, ok := rec.Payload[key]; !ok {
					t.Errorf("observed canary record is missing %q", key)
				}
			}
		}
	}
	if !sawObserved {
		t.Fatal("no observed canary record")
	}
	// The raw records must be JSON-serializable (the ledger stores JSON).
	for _, rec := range ts.sink.recs {
		if _, err := json.Marshal(rec.Payload); err != nil {
			t.Fatalf("payload is not JSON-serializable: %v", err)
		}
	}
}
