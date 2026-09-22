package ladder

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// codeplane_test.go is the SPEC-05 §7 row for AC-31 (§3.13a): sentinel-born
// admission persists the bundle byte-identically, a convergence hit fills BOTH
// planes on a sensor-born admission, the bundle changes no rung, a stale
// release never arrives (0 ladder-side gap records), and the copy-out joins
// carry the same bytes to the research request.

// fakeCodeplanes is the sentinel's convergence accessor double: one sig is
// linked, everything else misses (the stale-entry edge case).
type fakeCodeplanes struct {
	sigs map[string]types.CodeplaneContext
}

func (f *fakeCodeplanes) CodeplaneFor(sig types.Sig) (types.CodeplaneContext, bool) {
	b, ok := f.sigs[sig.String()]
	return b, ok
}

func stubSentinelBundle() types.CodeplaneContext {
	return types.CodeplaneContext{
		Side:      "sentinel",
		Sig:       "sentinel:sha256v1:af7e89fe750191f9",
		GroupID:   "grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH",
		Project:   "7",
		Release:   "payment-api@2.4.1",
		Regressed: true,
		Recent: []types.SigCount{{
			Sig:   "sentinel:sha256v1:af7e89fe750191f9",
			Count: 412,
			First: "2026-09-16T09:14:03.221Z",
			Last:  "2026-09-16T09:15:41.009Z",
		}},
		TS: "2026-09-16T09:15:41.009Z",
	}
}

func sensorSig() types.Sig {
	s, err := types.ParseSig("psi:sha256v1:2ab4c6d8e0f1a3b5")
	if err != nil {
		panic(err)
	}
	return s
}

// obsWithBundle builds the sentinel-born observation with a bundle attached.
func obsWithBundle() Observation {
	obs := obsFor(sensorSig(), "io-pressure", "psi")
	obs.Source = "sentinel"
	b := stubSentinelBundle()
	obs.Codeplane = &b
	return obs
}

// ladderCfgForResearch is the rule set that lets a bundle-carrying incident
// reach the research rung: the rule's entry rung is research.
func ladderCfgForResearch() harnessOpts {
	rule := types.Rule{Name: "io-pressure", EntryRung: types.RungResearch, Severity: types.SevHigh}
	return harnessOpts{research: true, rules: map[string]types.Rule{"io-pressure": rule}}
}

func TestAC31_SentinelBornAdmissionPersistsBundle(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	want := stubSentinelBundle()
	obs := obsWithBundle()
	res, err := h.l.Admit(context.Background(), obs)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if res.Inc == "" {
		t.Fatalf("no incident opened")
	}
	recs := h.ledger.byKind(types.KIncident)
	if len(recs) == 0 {
		t.Fatalf("no incident record written")
	}
	raw, ok := recs[len(recs)-1].Payload["codeplane"]
	if !ok {
		t.Fatalf("the T01 record must carry the codeplane key (§3.13a)")
	}
	enc, _ := json.Marshal(raw)
	var decoded types.CodeplaneContext
	if err := json.Unmarshal(enc, &decoded); err != nil {
		t.Fatalf("the persisted bundle does not decode as CodeplaneContext: %v", err)
	}
	reenc, _ := json.Marshal(&decoded)
	wantJSON, _ := json.Marshal(&want)
	if string(reenc) != string(wantJSON) {
		t.Fatalf("bundle drift on the incident record:\n got %s\nwant %s", reenc, wantJSON)
	}
}

func TestAC31_SensorBornAdmissionCarriesBothPlanes(t *testing.T) {
	bundle := stubSentinelBundle()
	h := newHarness(t, harnessOpts{})
	h.l.deps.Codeplanes = &fakeCodeplanes{sigs: map[string]types.CodeplaneContext{
		sensorSig().String(): bundle,
	}}
	obs := obsFor(sensorSig(), "io-pressure", "psi")
	obs.Rule = "io-pressure"
	obs.Detail = map[string]any{"value": 3.11, "unit": "io pressure", "full_avg10": 3.11}
	obs.Source = "psi"
	res, err := h.l.Admit(context.Background(), obs)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	st := h.l.incStateFor(res.Inc)
	if st == nil || st.Inc.Codeplane == nil {
		t.Fatalf("a convergence hit must fill the bundle")
	}
	b := st.Inc.Codeplane
	if b.Side != "sensor" {
		t.Fatalf("Side = %q, want sensor on the joined bundle", b.Side)
	}
	if b.RuleID != "io-pressure" {
		t.Fatalf("RuleID = %q, want io-pressure", b.RuleID)
	}
	if len(b.Readings) == 0 {
		t.Fatalf("Readings must carry the sensor half")
	}
	if b.GroupID != bundle.GroupID || b.Release != bundle.Release || !b.Regressed {
		t.Fatalf("the sentinel half was lost: %+v", b)
	}

	// With no hit nothing is carried: the rung runs unchanged (§3.13a).
	h2 := newHarness(t, harnessOpts{})
	res2, err := h2.l.Admit(context.Background(), obs)
	if err != nil {
		t.Fatalf("Admit no-hit: %v", err)
	}
	st2 := h2.l.incStateFor(res2.Inc)
	if st2 != nil && st2.Inc.Codeplane != nil {
		t.Fatalf("a sensor-born admission without a convergence hit must carry no bundle")
	}
}

func TestAC31_CopyOutJoinCarriesBundleBytes(t *testing.T) {
	want := stubSentinelBundle()

	// The §3.10a helper contract: nil → key absent, bundle → byte-equal.
	st := &incState{Inc: types.Incident{ID: "inc_x", Sig: sensorSig().String()}}
	sub := researchSubject(st)
	if _, present := sub.Context["codeplane"]; present {
		t.Fatalf("a nil bundle must leave the codeplane key absent")
	}
	st.Inc.Codeplane = &want
	sub = researchSubject(st)
	got, _ := json.Marshal(sub.Context["codeplane"])
	wantJSON, _ := json.Marshal(&want)
	if string(got) != string(wantJSON) {
		t.Fatalf("copy-out drift:\n got %s\nwant %s", got, wantJSON)
	}

	// And the state machine's research edge hands the persisted bundle over.
	h := newHarness(t, ladderCfgForResearch())
	res, err := h.l.Admit(context.Background(), obsWithBundle())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{
		From: types.StRecorded, To: types.StResRequested, Trigger: "T04",
	}); err != nil {
		t.Fatalf("Advance T04: %v", err)
	}
	if h.research.requests == 0 {
		t.Fatalf("the research rung never ran")
	}
	last := h.research.lastSubject()
	if last == nil {
		t.Fatalf("the fake recorded no subject")
	}
	gotJSON, _ := json.Marshal(last.Context["codeplane"])
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("copy-out drift through the rung:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestAC31_BundleChangesNoRung(t *testing.T) {
	// The same scripted arrival with and without a bundle produces the same
	// record kinds: context, never evidence.
	runWith := func(withBundle bool) []types.RecordKind {
		h := newHarness(t, harnessOpts{})
		var obs Observation
		if withBundle {
			obs = obsWithBundle()
		} else {
			obs = obsFor(sensorSig(), "io-pressure", "psi")
			obs.Source = "sentinel"
		}
		if _, err := h.l.Admit(context.Background(), obs); err != nil {
			t.Fatalf("Admit: %v", err)
		}
		var kinds []types.RecordKind
		for _, r := range h.ledger.records {
			kinds = append(kinds, r.Kind)
		}
		return kinds
	}
	a, b := runWith(false), runWith(true)
	if len(a) != len(b) {
		t.Fatalf("record-kind sequences diverge: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("record-kind sequences diverge at %d: %v vs %v", i, a, b)
		}
	}
}

// TestAC31_StaleReleaseNeverArrives names AC-31's discard contract from the
// ladder side: the accessor refuses a stale-release bundle at assembly
// (§3.9a), so the ladder receives no bundle at all and writes no gap of its
// own — the sentinel owns the discard.
func TestAC31_StaleReleaseNeverArrives(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	obs := obsFor(sensorSig(), "io-pressure", "psi")
	obs.Source = "sentinel"
	obs.Codeplane = nil
	if _, err := h.l.Admit(context.Background(), obs); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	for _, r := range h.ledger.byKind(types.KGap) {
		if c, _ := r.Payload["cause"].(string); c == "codeplane_release_mismatch" {
			t.Fatalf("the ladder must write no codeplane gap record (the sentinel owns the discard)")
		}
	}
}
