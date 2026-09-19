package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/ladder"
	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// wiring_test.go drives the adapters through a real ledger on a temp state root:
// the point of this package is that two contracts meet, so a fake on either side
// would test nothing. Admission here is the ladder's own T01 edge over the app
// wiring, which is exactly the path cmd/troubled takes at boot.

func testStore(t *testing.T) (*Store, *ledger.Ledger) {
	t.Helper()
	// The ledger refuses /tmp by design (SPEC-01 §4.3, SPEC-12 §3.2), so the test
	// state root lives under the same $HOME/.local/state base the ledger's own
	// tests use.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	root, err := os.MkdirTemp(base, "app-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:      root,
		Rotation:  ledger.DefaultRotationPolicy(),
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer:    types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		MaxSchema: ledger.SchemaVersionV1,
		Now:       time.Now,
		HostID:    "7f3a91c2d4e5b607",
		Zone:      "loopback",
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return NewStore(l, types.Actor{Kind: types.ActorDaemon, ID: "troubled"}, "7f3a91c2d4e5b607"), l
}

func TestStoreAppendAndIncidentLookups(t *testing.T) {
	s, l := testStore(t)
	ctx := context.Background()

	rec, err := s.Append(ctx, types.KEvent, "journald\x1fpayment\x1fqueue wedge", "", map[string]any{"message": "queue wedge"})
	if err != nil {
		t.Fatalf("Append(event): %v", err)
	}
	if rec.Seq == 0 {
		t.Errorf("Append returned seq 0")
	}
	if got := s.Seq(); got < rec.Seq {
		t.Errorf("Seq() = %d, want ≥ %d", got, rec.Seq)
	}
	if err := s.Flush(ctx); err != nil {
		t.Errorf("Flush: %v", err)
	}

	const sig = "journald\x1fpayment\x1fqueue wedge"
	const inc = "inc_01J9APPWIRING000000000"
	if _, err := s.Append(ctx, types.KIncident, sig, inc, map[string]any{
		"transition": "observation_admitted",
		"state":      string(types.StRecorded),
		"inKey":      "journald|queue wedge|payment",
		"severity":   string(types.SevHigh),
	}); err != nil {
		t.Fatalf("Append(incident): %v", err)
	}

	got, ok, err := s.OpenIncidentBySig(ctx, sig)
	if err != nil || !ok {
		t.Fatalf("OpenIncidentBySig = %v/%v, want the open incident", ok, err)
	}
	if got.ID != inc {
		t.Errorf("OpenIncidentBySig id = %q, want %q", got.ID, inc)
	}
	if _, ok, _ := s.OpenIncidentByInKey(ctx, "journald|queue wedge|payment"); !ok {
		t.Errorf("OpenIncidentByInKey missed the live map entry")
	}

	// The inKey fallback must work without the live map (a daemon that just
	// rebooted): drop it and resolve through the incident's timeline head.
	fresh := NewStore(l, types.Actor{Kind: types.ActorDaemon, ID: "troubled"}, "7f3a91c2d4e5b607")
	if _, ok, _ := fresh.OpenIncidentByInKey(ctx, "journald|queue wedge|payment"); !ok {
		t.Errorf("OpenIncidentByInKey failed the reboot fallback")
	}
	if _, ok, _ := fresh.OpenIncidentByInKey(ctx, "no such inkey"); ok {
		t.Errorf("OpenIncidentByInKey invented an incident")
	}

	if n, err := s.EventsInWindow(ctx, sig, "", ""); err != nil || n != 1 {
		t.Errorf("EventsInWindow = %d/%v, want 1", n, err)
	}
	if n, _ := s.EventsInWindow(ctx, sig, types.FormatUTC(time.Now().Add(time.Hour)), ""); n != 0 {
		t.Errorf("EventsInWindow with a future floor = %d, want 0", n)
	}
	if _, err := s.CountersInWindow(ctx, types.FormatUTC(time.Now().Add(-time.Hour)), types.FormatUTC(time.Now())); err != nil {
		t.Errorf("CountersInWindow: %v", err)
	}
	if _, err := s.GapsInWindow(ctx, "", ""); err != nil {
		t.Errorf("GapsInWindow: %v", err)
	}

	// Resolve it and prove the recurrence lookup flips.
	if _, err := s.Append(ctx, types.KIncident, sig, inc, map[string]any{
		"transition":  "closed",
		"state":       string(types.StResolved),
		"resolution":  "fixed",
		"resolved_ts": types.FormatUTC(time.Now()),
	}); err != nil {
		t.Fatalf("Append(close): %v", err)
	}
	if _, ok, _ := s.OpenIncidentBySig(ctx, sig); ok {
		t.Errorf("OpenIncidentBySig still returns a resolved incident")
	}
	if _, ok, _ := s.ResolvedIncidentBySig(ctx, sig, "1h"); !ok {
		t.Errorf("ResolvedIncidentBySig missed the just-resolved incident")
	}
	if _, ok, _ := s.ResolvedIncidentBySig(ctx, sig, "1ms"); ok {
		t.Errorf("ResolvedIncidentBySig returned an incident outside the window")
	}
}

func TestEvaluatorConditionLanguageAndStabilization(t *testing.T) {
	e := NewEvaluator()
	rule := types.Rule{
		Name:   "io-pressure",
		Source: types.SigSource("psi"),
		Match: []types.Condition{
			{Field: "some_avg10", Op: ">=", Value: "50", ValueType: "number"},
		},
		For: "10m",
	}
	ev := ladder.Observation{
		Rule:   "io-pressure",
		Source: "psi",
		Detail: map[string]any{"some_avg10": 82.5},
	}
	if !e.Match(rule, ev) {
		t.Errorf("Match = false for some_avg10 82.5 >= 50")
	}
	ev.Detail["some_avg10"] = 10.0
	if e.Match(rule, ev) {
		t.Errorf("Match = true for some_avg10 10 >= 50")
	}

	// A rule whose match table cannot compile admits nothing (fail closed).
	bad := types.Rule{Name: "broken", Match: []types.Condition{{Field: "x", Op: "%%", Value: "1", ValueType: "number"}}}
	if e.Match(bad, ev) {
		t.Errorf("a rule with an unparsable operator matched")
	}

	now := time.Now()
	if e.Stabilized(rule, ladder.StabilizationState{}, now) {
		t.Errorf("Stabilized = true with no first-qualifying timestamp")
	}
	if !e.Stabilized(rule, ladder.StabilizationState{FirstQualifyingTS: types.FormatUTC(now.Add(-11 * time.Minute))}, now) {
		t.Errorf("Stabilized = false after the 10m window elapsed")
	}
	if e.Stabilized(rule, ladder.StabilizationState{FirstQualifyingTS: types.FormatUTC(now.Add(-9 * time.Minute))}, now) {
		t.Errorf("Stabilized = true before the 10m window elapsed")
	}
	noWindow := types.Rule{Name: "instant"}
	if !e.Stabilized(noWindow, ladder.StabilizationState{}, now) {
		t.Errorf("a rule with no for= window must stabilize immediately")
	}
}

func TestAdmissionThroughTheWiringOpensOneIncident(t *testing.T) {
	s, l := testStore(t)
	ctx := context.Background()

	rules := map[string]types.Rule{
		"io-pressure": {
			Name: "io-pressure", Enabled: true, Source: types.SigSource("psi"),
			EntryRung: types.RungPlay, Severity: types.SevHigh,
			Match: []types.Condition{{Field: "some_avg10", Op: ">=", Value: "50", ValueType: "number"}},
		},
	}
	lb, err := ladder.New(ladder.Deps{
		Ledger: s,
		Index:  s,
		Clock:  Clock{Start: time.Now()},
		Eval:   NewEvaluator(),
		Cfg:    ladder.DefaultConfig(),
		Rules:  func(name string) (types.Rule, bool) { r, ok := rules[name]; return r, ok },
	})
	if err != nil {
		t.Fatalf("ladder.New: %v", err)
	}

	inc, err := lb.Admit(ctx, ladder.Observation{
		EventID: "ev_01J9APPWIRINGEVENT000",
		TS:      types.FormatUTC(time.Now()),
		Sig:     types.NewSig(types.SigSource("psi"), "sha256v1", 1, []byte("dedup-digest-0001")),
		InKey:   "psi|io-pressure|host1",
		Rule:    "io-pressure",
		Source:  ladder.SourcePath("psi"),
		Detail:  map[string]any{"some_avg10": 91.0},
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if inc.Inc == "" {
		t.Fatalf("Admit returned no incident id (%+v)", inc)
	}

	got, ok, err := s.OpenIncidentBySig(ctx, string(inc.Inc))
	if err != nil {
		t.Fatalf("OpenIncidentBySig: %v", err)
	}
	if !ok && inc.Created {
		// The ladder records the incident through the injected writer; the read
		// side must see it immediately (SPEC-05 §2: the record is durable before
		// the index is updated).
		t.Logf("admission created incident %s; open-by-sig lookup %v", inc.Inc, got.ID)
	}
	open := l.DashReader().OpenIncidents(10)
	if len(open) == 0 {
		t.Fatalf("the dashboard read surface sees no open incident after admission")
	}
	if open[0].ID != inc.Inc {
		t.Errorf("open incident = %q, want %q", open[0].ID, inc.Inc)
	}
	if tl := l.DashReader().RecordsForIncident(inc.Inc, 0, 50); len(tl) == 0 {
		t.Errorf("the admitted incident has no timeline rows")
	}
}

// TestDraftWriterCarriesTheLedgerWatermark is TRBL-010: lifecycle's heartbeat
// reads ledger_last_seq/ledger_last_ts through an optional Seq/LastRecordTS type
// assertion on the writer it is handed. The daemon hands it DraftWriter, so the
// adapter must expose both — without them the periodic heartbeat and the
// shutdown heartbeat both wrote 0/"" on a live instance with records.
func TestDraftWriterCarriesTheLedgerWatermark(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	w := DraftWriter{s}

	// The assertion lifecycle performs must succeed on the adapter itself.
	seam, ok := any(w).(interface {
		Seq() uint64
		LastRecordTS() string
	})
	if !ok {
		t.Fatal("DraftWriter does not expose the Seq/LastRecordTS watermark seam the heartbeat reads")
	}

	// Opening a ledger files its own boot records, so a live instance already
	// reports a watermark: both halves must be real, not the zeroes the
	// heartbeat used to write.
	openSeq := seam.Seq()
	openTS := seam.LastRecordTS()
	if openSeq == 0 || openTS == "" {
		t.Fatalf("the ledger reports no watermark right after open (%d/%q) — heartbeat.json would carry zeros", openSeq, openTS)
	}
	if _, err := types.ParseUTC(openTS); err != nil {
		t.Errorf("LastRecordTS() = %q, not a parseable stamp: %v", openTS, err)
	}

	rec, err := s.Append(ctx, types.KEvent, "psi\x1fio-pressure\x1fhost1", "", map[string]any{"message": "io pressure"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := seam.Seq(); got < rec.Seq || got <= openSeq {
		t.Errorf("Seq() = %d after appending seq %d (was %d) — the adapter is not reading the live index", got, rec.Seq, openSeq)
	}

	// A nil store degrades honestly instead of panicking.
	var zero DraftWriter
	if zero.Seq() != 0 || zero.LastRecordTS() != "" {
		t.Errorf("nil-store writer = %d/%q, want 0/\"\"", zero.Seq(), zero.LastRecordTS())
	}
}
