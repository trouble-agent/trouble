package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestAC6AppendOnlyLedger: seq has no holes, every line parses, every record has
// non-empty origin.host_id/origin.source and a rec_id, and no byte of a written
// file changes.
func TestAC6AppendOnlyLedger(t *testing.T) {
	n := 1000000
	if testing.Short() {
		n = 20000
	}
	clk := newFakeClock(testNow())
	root := testRoot(t)
	l := testLedgerAt(t, root, clk, func(o *Options) {
		o.Rotation.MaxBatchRecords = DefaultMaxBatchRecords
		o.Rotation.FsyncWindowMS = DefaultFsyncWindowMS
	})
	el := runAppends(t, l, n, 2*DefaultMaxBatchRecords)
	st := l.Status()
	if st.LastSeq != uint64(n)+1 { // + the boot lifecycle record
		t.Errorf("LastSeq = %d, want %d (no holes, no re-use)", st.LastSeq, n+1)
	}
	t.Logf("AC-6: %d appends in %s (%.0f rec/s), last_seq=%d", n, el, float64(n)/el.Seconds(), st.LastSeq)
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	var seqs []uint64
	records := 0
	for _, e := range ents {
		if e.IsDir() || fileRE(e.Name()) == "" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(root, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		before = append(before, b...)
		for _, ln := range splitLines(b) {
			if len(bytes.TrimSpace(ln)) == 0 {
				continue
			}
			rec, derr := decodeRecord(ln, true)
			if derr != nil || rec == nil {
				t.Fatalf("%s: unparsable line", e.Name())
			}
			records++
			if rec.RecID == "" {
				t.Fatalf("seq %d has no rec_id", rec.Seq)
			}
			if rec.Origin.HostID == "" || rec.Origin.Source == "" {
				t.Fatalf("seq %d has an empty origin: %+v", rec.Seq, rec.Origin)
			}
			if rec.Actor.ID == "" || rec.Actor.Kind == "" {
				t.Fatalf("seq %d has an empty actor: %+v", rec.Seq, rec.Actor)
			}
			seqs = append(seqs, rec.Seq)
		}
	}
	if records != n+1 {
		t.Errorf("parsed %d records, want %d", records, n+1)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seq hole between %d and %d", seqs[i-1], seqs[i])
		}
	}
	if len(seqs) > 0 && seqs[0] != 1 {
		t.Errorf("first seq = %d, want 1", seqs[0])
	}
	beforeHash := sha256.Sum256(before)

	// reopen: recovery + index rebuild must not rewrite a single byte
	l2 := testLedgerAt(t, root, clk)
	_ = l2
	if err := l2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after []byte
	ents2, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents2 {
		if e.IsDir() || fileRE(e.Name()) == "" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(root, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		after = append(after, b...)
	}
	if len(after) < len(before) {
		t.Fatalf("the ledger shrank across a reopen: %d → %d bytes", len(before), len(after))
	}
	if !bytes.Equal(after[:len(before)], before) {
		t.Errorf("a written byte changed across a reopen: %s vs %s",
			hex.EncodeToString(beforeHash[:8]), hex.EncodeToString(hashOf(after[:len(before)])[:8]))
	}
}

func hashOf(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// fileRE reports the matched ledger file name, or "" when it is not one.
func fileRE(name string) string {
	if fileRe.MatchString(name) {
		return name
	}
	return ""
}

// TestAC22DedupTotality: three arrival paths (journald, sentinel, collector)
// sharing one merge key and three different sigs collapse to one group and one
// incident; two distinct merge keys stay two.
func TestAC22DedupTotality(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)

	mk := MergeKey("crash_loop", "payment-worker.service")
	inc := types.NewID(types.PInc)
	grp := types.NewID(types.PGrp)
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: "journald:sha256v1:a1", Inc: inc,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
		Actor:  testActor(),
		Payload: map[string]any{
			"state": "detected", "grp": grp, "entry_rung": "play", "merge_key": mk,
			"opened_ts": types.FormatUTC(testNow()),
		},
	})

	sigs := []struct {
		sig    string
		source string
	}{
		{"journald:sha256v1:a1", "journald:payment-worker"},
		{"sentinel:sha256v1:b2", "sentinel:payment-worker"},
		{"collector:sha256v1:c3", "collector:go-panic"},
	}
	for _, s := range sigs {
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KEvent, Sig: s.sig, Inc: inc,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: s.source},
			Actor:   testActor(),
			Payload: map[string]any{"merge_key": mk, "subject": "payment-worker.service", "op": "sample"},
		})
	}
	// a second, distinct bug class must stay separate
	mk2 := MergeKey("disk_full", "/")
	mustAppend(t, l, eventDraft("disk", "disk:sha256v1:d4", "d4", 0))
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KEvent, Sig: "disk:sha256v1:d4",
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "disk"},
		Actor:  testActor(), Payload: map[string]any{"merge_key": mk2, "subject": "/"},
	})

	q := l.Query()
	// one incident, reachable from any of the three sigs
	for _, s := range sigs {
		in, info, err := q.IncidentBySig(s.sig)
		if err != nil {
			t.Fatalf("IncidentBySig(%s): %v", s.sig, err)
		}
		if in == nil || in.ID != inc {
			t.Fatalf("IncidentBySig(%s) = %+v, want %s (one incident per sig space)", s.sig, in, inc)
		}
		if !info.Indexed {
			t.Errorf("IncidentBySig(%s) answered from the cold path", s.sig)
		}
	}
	// one group: every sig resolves to the same digest
	var digest string
	for _, s := range sigs {
		d, ok := l.idx.digestForSig(s.sig)
		if !ok {
			t.Fatalf("sig %s is not in sigToDigest", s.sig)
		}
		if digest == "" {
			digest = d
		} else if d != digest {
			t.Errorf("sig %s maps to digest %s, want %s (one group)", s.sig, d, digest)
		}
	}
	if d, ok := l.idx.digestForMergeKey(mk); !ok || d != digest {
		t.Errorf("merge key %s resolves to %q (ok=%v), want %s", mk, d, ok, digest)
	}
	// two distinct merge keys stay two groups
	if d2, ok := l.idx.digestForMergeKey(mk2); !ok || d2 == digest {
		t.Errorf("distinct merge keys collapsed: %s vs %s", d2, digest)
	}
	// and the incident's evidence chain carries all three arrival paths
	bundle, err := q.Evidence(inc, 64)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range bundle.Records {
		seen[r.Sig] = true
	}
	for _, s := range sigs {
		if !seen[s.sig] {
			t.Errorf("evidence chain is missing arrival path %s", s.sig)
		}
	}
	for i := 1; i < len(bundle.Records); i++ {
		if bundle.Records[i].Seq < bundle.Records[i-1].Seq {
			t.Errorf("evidence chain is not in seq order")
		}
	}
}

// TestAC26EvidenceChain: a 20-step ladder fixture plus a kill-switch flip yields
// ≥1 evidence record per ladder state, in seq order, with no gap between the
// incident's first and last seq, and the kill-switch checkpoint is present.
func TestAC26EvidenceChain(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	inc := types.NewID(types.PInc)
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	steps := []types.LadderState{
		types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck,
		types.StPlayApplied, types.StResRequested, types.StResReturned, types.StAgentRunning,
		types.StAgentDone, types.StVerifying, types.StPlayFailed, types.StAgentFailed,
		types.StAgentSuspended, types.StResDegraded, types.StResSkipped, types.StEscalated,
		types.StResolved, types.StSuppressed, types.StQuarantined, types.StVerifying,
	}
	for i, s := range steps {
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KIncident, Sig: sig, Inc: inc,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "sentinel:payment-worker"},
			Actor:   testActor(),
			Payload: map[string]any{"state": string(s), "step": i, "incident": map[string]any{"id": inc, "sig": sig, "state": string(s)}},
		})
	}
	// the kill-switch flip inside the loop
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KLifecycle, Inc: inc,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "ledger"},
		Actor:  testActor(),
		Payload: map[string]any{
			"op": "kill_switch", "kill_switch": true, "mode": "full",
			"checkpoint": "stage", "error_code": "TROUBLE-LADDER-010",
		},
	})
	for i := 0; i < 3; i++ {
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KVerify, Sig: sig, Inc: inc,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "sentinel:payment-worker"},
			Actor:   testActor(),
			Payload: map[string]any{"result": "passed", "events_observed": 0, "canary_seen": true},
		})
	}

	bundle, err := l.Query().Evidence(inc, 512)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if len(bundle.Records) != 24 {
		t.Errorf("evidence chain = %d records, want 24", len(bundle.Records))
	}
	states := map[types.LadderState]int{}
	killSwitch := false
	for _, r := range bundle.Records {
		if s, ok := r.Payload["state"].(string); ok {
			states[types.LadderState(s)]++
		}
		if v, ok := r.Payload["kill_switch"].(bool); ok && v {
			killSwitch = true
		}
	}
	for _, s := range steps {
		if states[s] == 0 {
			t.Errorf("no evidence record for ladder state %s", s)
		}
	}
	if !killSwitch {
		t.Errorf("the kill-switch checkpoint is missing from the evidence chain")
	}
	for i := 1; i < len(bundle.Records); i++ {
		if bundle.Records[i].Seq <= bundle.Records[i-1].Seq {
			t.Errorf("evidence chain is not strictly ordered by seq at %d", i)
		}
	}
	// no gap between the incident's first and last seq
	files := map[string]bool{}
	for _, r := range bundle.Records {
		name, _, rerr := l.Resolve(r.Seq)
		if rerr != nil {
			t.Fatalf("Resolve(%d): %v", r.Seq, rerr)
		}
		files[name] = true
	}
	for seq := bundle.FirstSeq; seq <= bundle.LastSeq; seq++ {
		if _, ok, rerr := l.recordAt(seq); rerr != nil {
			t.Fatalf("recordAt(%d): %v", seq, rerr)
		} else if !ok {
			t.Errorf("hole in the incident's seq range at %d", seq)
		}
	}
	if bundle.KindCounts["incident"] < 20 {
		t.Errorf("KindCounts[incident] = %d, want >= 20", bundle.KindCounts["incident"])
	}
	if bundle.Partial {
		t.Errorf("the bundle reported Partial for a fully indexed incident")
	}
	_ = fmt.Sprint(files)
}
