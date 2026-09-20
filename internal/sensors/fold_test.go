package sensors

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// sampledEvent is the event shape the PSI sampler itself produces
// (internal/sensors/psi.go sampleOnce): one observation per
// `sensors.psi.sample_interval`. The `psiEvent` helper in testutil_test.go is a
// rule test's shape and carries no `sample_backed` marker, so it is deliberately
// NOT foldable — that is how the rule/breaker suites keep asserting the raw
// per-event contract.
func sampledEvent(scope string, avg10 float64) types.SensorEvent {
	ev := psiEvent(scope, avg10, false)
	ev.Detail["sample_backed"] = true
	return ev
}

// recordsForSig returns the emitted records carrying one signature — the
// identity the ladder keys an incident by (SPEC-01 §3.3, SPEC-05).
func recordsForSig(h *harness, sig string) []types.Record {
	return h.recordsWhere(func(r types.Record) bool { return r.Sig == sig })
}

// foldCount reads a payload's `count` whichever numeric shape it survived as:
// the in-memory harness keeps the Go value, a ledger round trip yields float64.
func foldCount(t *testing.T, p map[string]any) uint64 {
	t.Helper()
	switch v := p["count"].(type) {
	case uint64:
		return v
	case int:
		return uint64(v)
	case int64:
		return uint64(v)
	case float64:
		return uint64(v)
	}
	t.Fatalf("payload.count = %v (%T), want a number", p["count"], p["count"])
	return 0
}

// TestFoldAbsorbsRepeatedSampledObservations is the core count-preserving claim
// (SPEC-03 §3.8a): N identical sampled observations inside one window become ONE
// record whose count is N, and the record carries the window it stands for.
func TestFoldAbsorbsRepeatedSampledObservations(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()
	ev := sampledEvent("cpu", 12.5)
	sig := ev.Sig.String()
	start := h.now()

	// One window at the shipped 2s sampling interval: 150 observations.
	const n = 150
	for i := 0; i < n; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	if got := len(recordsForSig(h, sig)); got != 0 {
		t.Fatalf("a fold wrote %d records inside its own window, want 0", got)
	}
	if got := h.s.fold.openCount(); got != 1 {
		t.Fatalf("open folds = %d, want 1 for one signature", got)
	}

	// The observation that starts the NEXT window closes the fold it replaces.
	h.advance(2 * time.Second)
	h.s.handleEvent(ctx, ev)

	recs := recordsForSig(h, sig)
	if len(recs) != 1 {
		t.Fatalf("%d records for %d identical observations in one window, want exactly 1", len(recs), n)
	}
	p := recs[0].Payload
	if got := foldCount(t, p); got != n {
		t.Fatalf("payload.count = %d, want %d (the count IS the drop-proof)", got, n)
	}
	if p["fold"] != true {
		t.Fatalf("payload.fold = %v, want true: the disposition must be visible", p["fold"])
	}
	if got, want := p["first_ts"], types.FormatUTC(start); got != want {
		t.Fatalf("payload.first_ts = %v, want %v", got, want)
	}
	if got, want := p["last_ts"], types.FormatUTC(start.Add((n-1)*2*time.Second)); got != want {
		t.Fatalf("payload.last_ts = %v, want %v", got, want)
	}
	if got := p["fold_window_s"]; got != 300.0 {
		t.Fatalf("payload.fold_window_s = %v, want 300", got)
	}
	// The folded record is still the SAME observation, under the SAME signature:
	// the ladder's sig+inKey identity and the index see one story, not a rewrite.
	// The payload's own fields survive; `detail` stays the FIRST observation's,
	// including the `sample_backed` marker that made it foldable.
	for _, k := range []string{"value", "unit", "scope", "sensor", "subject", "detail", "id"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("the folded record lost the observation field %q: %v", k, p)
		}
	}
	detail, ok := p["detail"].(map[string]any)
	if !ok {
		t.Fatalf("payload.detail = %T, want the folded observation's detail map", p["detail"])
	}
	if detail["sample_backed"] != true || detail["scope"] != "cpu" || p["scope"] != "cpu" {
		t.Fatalf("the folded record does not carry the observation it stands for: %v", p)
	}
	if got := h.s.fold.observations.Load(); got != n+1 {
		t.Fatalf("folded observations = %d, want %d", got, n+1)
	}
	if got := h.s.fold.records.Load(); got != 1 {
		t.Fatalf("fold records written = %d, want 1", got)
	}
}

// TestFoldWindowBoundaryClosesAndRestarts pins the window semantics: a fold is
// closed by the first observation at or after first_ts+window, that observation
// opens the next fold, and a signature that stops repeating still gets its
// record written by the next unrelated arrival.
func TestFoldWindowBoundaryClosesAndRestarts(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "60s"})
	ctx := context.Background()
	ev := sampledEvent("io", 7.25)
	sig := ev.Sig.String()
	start := h.now()

	// Window one: 30 observations at 2s (t=0 … t=58s), closed by t=60s.
	for i := 0; i < 30; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	h.advance(2 * time.Second) // t=60s: exactly the boundary
	h.s.handleEvent(ctx, ev)
	if open := h.s.fold.openCount(); open != 1 {
		t.Fatalf("after the boundary the next fold must be open: %d", open)
	}

	// Window two: 29 more observations, then the t=120s boundary observation.
	for i := 0; i < 29; i++ {
		h.advance(2 * time.Second)
		h.s.handleEvent(ctx, ev)
	}
	h.advance(2 * time.Second) // t=120s: the second boundary
	h.s.handleEvent(ctx, ev)

	recs := recordsForSig(h, sig)
	if len(recs) != 2 {
		t.Fatalf("%d records across two windows, want 2 (one per window)", len(recs))
	}
	if got, want := foldCount(t, recs[0].Payload), uint64(30); got != want {
		t.Fatalf("window one count = %d, want %d", got, want)
	}
	if got, want := foldCount(t, recs[1].Payload), uint64(30); got != want {
		t.Fatalf("window two count = %d, want %d", got, want)
	}
	if got, want := recs[0].Payload["first_ts"], types.FormatUTC(start); got != want {
		t.Fatalf("window one first_ts = %v, want %v", got, want)
	}
	if got, want := recs[1].Payload["first_ts"], types.FormatUTC(start.Add(60*time.Second)); got != want {
		t.Fatalf("window two first_ts = %v, want %v", got, want)
	}

	// A signature that stops repeating is not held open: an unrelated arrival
	// after the window writes it.
	h2 := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "60s"})
	quiet := sampledEvent("io", 3.5)
	h2.s.handleEvent(ctx, quiet)
	h2.s.handleEvent(ctx, quiet)
	if got := len(recordsForSig(h2, quiet.Sig.String())); got != 0 {
		t.Fatalf("the first two observations wrote %d records, want 0", got)
	}
	h2.advance(61 * time.Second)
	other := sampledEvent("memory", 1.0) // a different signature, unrelated
	h2.s.handleEvent(ctx, other)
	recs = recordsForSig(h2, quiet.Sig.String())
	if len(recs) != 1 {
		t.Fatalf("a quiet signature holds %d records after its window passed, want 1 written by the next arrival", len(recs))
	}
	if got := foldCount(t, recs[0].Payload); got != 2 {
		t.Fatalf("the quiet fold's count = %d, want 2", got)
	}
	if got := h2.s.fold.openCount(); got != 1 {
		t.Fatalf("open folds after the sweep = %d, want 1 (the new signature)", got)
	}
}

// TestFoldOffReproducesOneRecordPerCycle is the `0 = off` half of the config
// gate: with the fold disabled the subsystem persists exactly what it persisted
// before the fold existed — one record per sampled observation, no fold fields.
func TestFoldOffReproducesOneRecordPerCycle(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "0"})
	ctx := context.Background()
	ev := sampledEvent("cpu", 4.0)
	sig := ev.Sig.String()
	const observations = 25
	for i := 0; i < observations; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	recs := recordsForSig(h, sig)
	if len(recs) != observations {
		t.Fatalf("fold off: %d records for %d observations, want one per cycle", len(recs), observations)
	}
	for _, r := range recs {
		for _, k := range []string{"fold", "fold_window_s", "first_ts", "last_ts", "count"} {
			if _, ok := r.Payload[k]; ok {
				t.Fatalf("fold off still stamped %q on a record: %v", k, r.Payload)
			}
		}
		// Unchanged from before the fold existed: the observation's own count
		// rides in `detail`, exactly as the sampler wrote it.
		detail, ok := r.Payload["detail"].(map[string]any)
		if !ok {
			t.Fatalf("payload.detail = %T, want the observation's detail map", r.Payload["detail"])
		}
		if got := foldCount(t, detail); got != 1 {
			t.Fatalf("fold off: detail.count = %d, want the observation's own 1", got)
		}
	}
	if got := h.s.fold.openCount(); got != 0 {
		t.Fatalf("fold off holds %d folds, want 0", got)
	}
}

// TestFoldLeavesFiringAndWakeEventsImmediate is the boundary of what may be
// folded: anything the ladder acts on, and anything that is not a sampler tick,
// is written when it happens — and the fold it ends is written first, so the
// ledger keeps the chronology.
//
// The two observations sit in the SAME bucket (b2: 10 <= avg10 < 25) so they
// share a signature while the rule's threshold splits their outcome: that is the
// only shape in which a firing observation can end a fold for its own identity.
func TestFoldLeavesFiringAndWakeEventsImmediate(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	h.writeRules("10.toml", `
[[rule]]
name = "fold_io_high"
source = "psi"
for = "0s"
cooldown = "5m"
severity = "high"
entry_rung = "play"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "15"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()

	below := sampledEvent("io", 12.0) // b2, under the threshold: does not match
	above := sampledEvent("io", 20.0) // b2, over the threshold: fires
	if below.Sig.String() != above.Sig.String() {
		t.Fatalf("the two observations must share a signature (got %s and %s)", below.Sig, above.Sig)
	}
	sig := below.Sig.String()
	h.s.handleEvent(ctx, below)
	h.s.handleEvent(ctx, below)
	if got := len(h.snapshot()); got != 0 {
		t.Fatalf("the matching-but-quiet observations wrote %d records, want 0 (folded)", got)
	}

	// A fired observation is the ladder's input: written NOW, and the fold for
	// its own signature is written first.
	h.s.handleEvent(ctx, above)
	recs := h.snapshot()
	if len(recs) != 2 {
		t.Fatalf("%d records after the fire, want 2 (the closed fold + the fire)", len(recs))
	}
	if recs[0].Sig != sig || recs[1].Sig != sig {
		t.Fatalf("both records must carry the fold's signature: %s then %s", recs[0].Sig, recs[1].Sig)
	}
	if got := foldCount(t, recs[0].Payload); got != 2 {
		t.Fatalf("the closed fold's count = %d, want 2", got)
	}
	if fire, _ := recs[1].Payload["fire"].(bool); !fire {
		t.Fatalf("the fired observation was not emitted as a fire: %v", recs[1].Payload)
	}
	if _, folded := recs[1].Payload["fold"]; folded {
		t.Fatal("a firing record was folded: the ladder would never be told")
	}
	if got := recs[1].Payload["rule"]; got != "fold_io_high" {
		t.Fatalf("the fired record carries rule %v", got)
	}
	if got := h.s.fold.openCount(); got != 0 {
		t.Fatalf("open folds after the fire = %d, want 0", got)
	}

	// A trigger wake is an edge the sampler did not produce: never folded.
	wake := sampledEvent("io", 12.0)
	wake.Wake = true
	before := len(h.snapshot())
	h.s.handleEvent(ctx, wake)
	if got := len(h.snapshot()); got != before+1 {
		t.Fatalf("a wake observation wrote %d records, want 1 immediately", got-before)
	}
	if wakeRec := h.snapshot()[len(h.snapshot())-1]; wakeRec.Payload["fold"] == true {
		t.Fatal("a wake observation was folded")
	}
}

// TestFoldFlushesOnStop pins the shutdown path: an observation that already
// happened is written by Stop, never lost to a stop.
func TestFoldFlushesOnStop(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()
	ev := sampledEvent("cpu", 55.0)
	sig := ev.Sig.String()
	for i := 0; i < 5; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	if got := len(recordsForSig(h, sig)); got != 0 {
		t.Fatalf("%d records before the stop, want 0 (all folded)", got)
	}
	if err := h.s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	recs := recordsForSig(h, sig)
	if len(recs) != 1 {
		t.Fatalf("Stop wrote %d records for the open fold, want 1", len(recs))
	}
	if got := foldCount(t, recs[0].Payload); got != 5 {
		t.Fatalf("the flushed fold's count = %d, want 5", got)
	}
	if got := h.s.fold.openCount(); got != 0 {
		t.Fatalf("Stop left %d folds open", got)
	}
}

// TestFoldIsBoundedAtFoldMaxOpen pins the memory bound: the fold table never
// grows past foldMaxOpen, and reaching the bound closes a fold (writing it)
// rather than discarding one.
func TestFoldIsBoundedAtFoldMaxOpen(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()
	base := sampledEvent("io", 9.0)
	for i := 0; i <= foldMaxOpen; i++ {
		ev := base
		ev.Sig = sigFor(types.SrcPSI, "io", "some", fmt.Sprintf("bound-%d", i))
		h.s.handleEvent(ctx, ev)
	}
	if got := h.s.fold.openCount(); got > foldMaxOpen {
		t.Fatalf("open folds = %d, want <= %d", got, foldMaxOpen)
	}
	if got := h.s.fold.overflow.Load(); got != 1 {
		t.Fatalf("overflow closes = %d, want exactly 1", got)
	}
	if got := h.s.fold.records.Load(); got != 1 {
		t.Fatalf("the fold closed at the bound wrote %d records, want 1", got)
	}
	if got := len(h.snapshot()); got != 1 {
		t.Fatalf("records emitted = %d, want 1 (only the fold that had to close)", got)
	}
}

// TestFoldRateBudgetIsDerivedFromTheWindow turns the fold into the numbers the
// task asks for: an explicit records/day budget derived from the shipped
// defaults, checked against the amplification TRBL-009 measured.
func TestFoldRateBudgetIsDerivedFromTheWindow(t *testing.T) {
	cfg := defaultConfig()
	iv, w := cfg.psi.sampleInterval, cfg.sampleFoldWindow
	if iv != 2*time.Second {
		t.Fatalf("sampling interval default = %s, want the spec's 2s", iv)
	}
	if w != 5*time.Minute {
		t.Fatalf("fold window default = %s, want 5m: the budget below is derived from it", w)
	}

	// Derived from the fold: a signature that keeps repeating is persisted once
	// per window, whatever the sampling rate is.
	observationsPerDay := int64((24 * time.Hour) / iv)
	observationsPerRecord := int64(w / iv)
	recordsPerSigPerDay := observationsPerDay / observationsPerRecord
	if observationsPerRecord != 150 || recordsPerSigPerDay != 288 {
		t.Fatalf("derived: %d observations/record, %d records/sig/day, want 150 and 288", observationsPerRecord, recordsPerSigPerDay)
	}

	// The measured idle-host shape (foreman probe at HEAD 31d6b76, tick
	// trouble-2026-09-19-08-24-56): 503 event records in 259s -> 175,583/day,
	// across 78 distinct event signatures.
	const measuredSigs = 78
	const measuredRecordsPerDay = 175583
	budget := int64(measuredRecordsPerDay) / 7 // the fold must cut the rate by >=7x
	if got := recordsPerSigPerDay * measuredSigs; got > budget {
		t.Fatalf("folded idle-host rate = %d records/day, budget (measured/7) = %d", got, budget)
	}
	t.Logf("budget: %d records/day for %d signatures at the 5m default (measured baseline %d/day) = %.1fx fewer",
		recordsPerSigPerDay*measuredSigs, measuredSigs, measuredRecordsPerDay,
		float64(measuredRecordsPerDay)/float64(recordsPerSigPerDay*measuredSigs))

	// ...and the arithmetic is not the evidence: run one simulated day through
	// the real handleEvent path and count what the ledger would receive.
	h := newHarness(t)
	ctx := context.Background()
	ev := sampledEvent("cpu", 6.0)
	sig := ev.Sig.String()
	const dayObservations = 24 * 3600 / 2 // one observation every 2s
	for i := 0; i < dayObservations; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	h.s.fold.flush(ctx, h.s.emit)
	recs := recordsForSig(h, sig)
	if len(recs) != 288 {
		t.Fatalf("one simulated day of a repeating signature wrote %d records, want %d", len(recs), 288)
	}
	var written uint64
	for _, r := range recs {
		written += foldCount(t, r.Payload)
	}
	if written != dayObservations {
		t.Fatalf("the folded records account for %d of %d observations: the count must be the drop-proof", written, dayObservations)
	}
	t.Logf("measured through handleEvent: %d records for %d observations in one simulated day (%d observations/record)",
		len(recs), dayObservations, dayObservations/len(recs))
}

// TestFoldWindowAcceptsTheCanonicalDurationEncoding pins the cross-package seam
// the SPEC-12 registry actually uses: a resolved key's value reaches this decoder
// as types.Duration, the canonical encoding (SPEC-TYPES), NOT as a bare string. A
// named string type does not match `case string` in a type switch, so a decoder
// that only knew `string` refused every registered sensors duration key at boot
// (found by internal/app's shipped-example boot: "sensors.sample_fold_window:
// expected a duration, got types.Duration").
func TestFoldWindowAcceptsTheCanonicalDurationEncoding(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: types.Duration("90s")})
	if got := h.s.cfg.sampleFoldWindow; got != 90*time.Second {
		t.Fatalf("fold window = %s, want 90s from the canonical types.Duration encoding", got)
	}
	// And it is the live window, not just a stored value: 45 observations at 2s
	// fill a 90s window exactly, and the 46th closes it.
	ctx := context.Background()
	ev := sampledEvent("cpu", 8.0)
	for i := 0; i < 45; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	if got := len(recordsForSig(h, ev.Sig.String())); got != 0 {
		t.Fatalf("%d records inside a 90s window, want 0", got)
	}
	h.advance(2 * time.Second)
	h.s.handleEvent(ctx, ev)
	recs := recordsForSig(h, ev.Sig.String())
	if len(recs) != 1 || foldCount(t, recs[0].Payload) != 45 {
		t.Fatalf("records = %d, want one carrying count 45", len(recs))
	}
}

// TestFoldCountsARefusedRecordAsDropped: a fold whose record the emit path
// refuses is not silent — it is counted on the producing sensor, exactly as
// §3.8a claims.
func TestFoldCountsARefusedRecordAsDropped(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()
	ev := sampledEvent("cpu", 3.0)
	h.s.handleEvent(ctx, ev)
	h.s.handleEvent(ctx, ev)

	h.mu.Lock()
	h.emitErr = errors.New("ledger refused the write")
	h.mu.Unlock()

	if err := h.s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.s.fold.emitFailures.Load(); got != 1 {
		t.Fatalf("fold emit failures = %d, want 1", got)
	}
	rt := h.s.rt[types.SenPSI]
	if rt == nil {
		t.Fatal("the psi runtime is missing")
	}
	if got := rt.drops.Load(); got != 1 {
		t.Fatalf("sensor Dropped = %d, want 1: a refused fold record must not be silent", got)
	}
	if got := len(recordsForSig(h, ev.Sig.String())); got != 0 {
		t.Fatalf("%d records survived a refusing emit path", got)
	}
}

// TestFoldRejectsANegativeWindow: `0` is the documented OFF, so a negative value
// is a configuration error rather than a second spelling of off.
func TestFoldRejectsANegativeWindow(t *testing.T) {
	dir := t.TempDir()
	_, err := New([]types.ConfigValue{
		{Key: "origin.host_id", Value: "h"},
		{Key: "state_root", Value: dir},
		{Key: "sensors.rules.dir", Value: dir + "/rules"},
		{Key: "sensors.sample_fold_window", Value: "-1m"},
	}, func(context.Context, types.RecordDraft) (types.Record, error) { return types.Record{}, nil }, nil, nil)
	if err == nil {
		t.Fatal("a negative fold window was accepted; 0 is the documented OFF")
	}
	if got := fmt.Sprint(err); !contains(got, "negative fold window") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestFoldDoesNotWeakenTheBreakerOrDropObservations is the no-regression half
// (AC-4, §3.8): with the fold ON, a sustained over-cap sampled burst still trips
// the rule breaker, the ladder cap still gates firing, and every observation is
// still accounted for — the folded records' counts add back up to the burst.
func TestFoldDoesNotWeakenTheBreakerOrDropObservations(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	h.writeRules("10.toml", `
[[rule]]
name = "fold_burst"
source = "psi"
for = "0s"
cooldown = "0s"
severity = "low"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "1"
value_type = "number"
`)
	h.mustReload()
	h.s.limitsForTest(120)
	ctx := context.Background()

	// 600 sampled observations of ONE signature inside 60 simulated seconds:
	// over the 120/min rule cap, every one of them foldable.
	const events = 600
	ev := sampledEvent("io", 41.7)
	sig := ev.Sig.String()
	for i := 0; i < events; i++ {
		h.s.handleEvent(ctx, ev)
		h.advance(100 * time.Millisecond)
	}

	fired := len(firedFor(h, "fold_burst"))
	if fired > 120 {
		t.Fatalf("the ladder gate was invoked %d times in one minute, cap is 120", fired)
	}
	if got := h.s.suppressed.Load(); got != uint64(events-fired) {
		t.Fatalf("Suppressed = %d, want %d (the cap counted what it refused)", got, events-fired)
	}

	// The breaker still opens on the sustained rate — the fold is downstream of
	// it and cannot hide events from it.
	h.advance(3 * time.Minute)
	for i := 0; i < 500; i++ {
		h.s.handleEvent(ctx, ev)
	}
	open := false
	for _, b := range h.s.Breakers() {
		if b.Scope == "rule:fold_burst" && b.State == types.BreakerOpen {
			open = true
			if b.Trips < 1 {
				t.Fatalf("an open breaker must carry its trip count: %+v", b)
			}
		}
	}
	if !open {
		t.Fatalf("the rule breaker did not open under the sustained burst; breakers = %+v", h.s.Breakers())
	}

	// Nothing was dropped by the cap, the breaker or the fold: the counts add
	// back up to exactly what the host observed. A fired record stands for the
	// one observation that fired it; a folded record stands for its count.
	h.s.fold.flush(ctx, h.s.emit)
	var written uint64
	for _, r := range recordsForSig(h, sig) {
		if r.Payload["fold"] == true {
			written += foldCount(t, r.Payload)
			continue
		}
		if fire, _ := r.Payload["fire"].(bool); !fire {
			t.Fatalf("a non-folded record that did not fire: %v", r.Payload)
		}
		written++
	}
	const total = events + 500
	if written != total {
		t.Fatalf("the records account for %d of %d observations; the count must be the drop-proof", written, total)
	}
	if got := len(recordsForSig(h, sig)); got >= total {
		t.Fatalf("%d records for %d observations: the fold did not fold", got, total)
	}
	if got := h.s.fold.observations.Load(); got != uint64(total-len(firedFor(h, "fold_burst"))) {
		t.Fatalf("folded observations = %d, want %d (everything that did not fire)", got, total-len(firedFor(h, "fold_burst")))
	}
}
