package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TRBL-106: `trouble ledger verify` reported deterministic
// payload_hash_mismatch findings on every record whose payload carries a value
// serialized from a typed Go struct.
//
// The write boundary stamps `payload_sha256` over the payload bytes its emitter
// built — a `map[string]any` whose nested values may be typed structs, so those
// bytes keep STRUCT FIELD order (`values: [{key,value,source,source_ref,
// redacted}]` for a lifecycle config snapshot). The verifier re-parsed the
// stored line into `map[string]any` and re-marshaled it, which re-orders every
// nested member alphabetically (`key,redacted,source,source_ref,value`), so the
// two hashes could never agree (SPEC-01 §4.4 Verify integrity, §5 code 011).
//
// These tests pin both halves: the stored bytes really are struct-ordered, and
// the verifier recomputes the hash over the bytes AS STORED (minus the reserved
// payload_sha256 member) so writer and verifier agree — while a genuinely
// mismatched hash is still reported.

// hashPad is the filler that pushes a payload past the 4096-byte
// payload_sha256 threshold (SPEC-01 §3.1).
const hashPad = "hashpad-0123456789abcdef-"

func padTo(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(hashPad)
	}
	return b.String()
}

// configValues builds the `values` shape internal/lifecycle.WriteConfigRecord
// emits for a boot snapshot: a []types.ConfigValue, whose members serialize in
// struct field order — never alphabetically.
func configValues(n int, value string) []types.ConfigValue {
	out := make([]types.ConfigValue, 0, n)
	for i := 0; i < n; i++ {
		v := value
		if v == "" {
			v = padTo(64)
		}
		out = append(out, types.ConfigValue{
			Key:       fmt.Sprintf("sentinel.routes.key_%03d", i),
			Value:     v,
			Source:    "default",
			SourceRef: "builtin",
			Redacted:  i%2 == 0,
		})
	}
	return out
}

// configSnapshotPayload is the config record payload WriteConfigRecord builds.
func configSnapshotPayload(n int, value string) map[string]any {
	return map[string]any{
		"values":    configValues(n, value),
		"conflicts": 0,
	}
}

// hashStructNested is a typed payload value whose json field order
// (op, from_seq, to_seq, note) is deliberately not its alphabetical order
// (from_seq, note, op, to_seq).
type hashStructNested struct {
	Op      string `json:"op"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	Note    string `json:"note"`
}

func hashStruct(value map[string]any) types.RecordDraft {
	return types.RecordDraft{
		Kind:    types.KLifecycle,
		Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "lifecycle"},
		Actor:   testActor(),
		Payload: value,
	}
}

// storedPayloadLine marshals a record the way the writer's line serializer does
// and returns the line without its trailing newline.
func storedPayloadLine(t *testing.T, payload map[string]any) []byte {
	return storedPayloadLineSeq(t, 1, payload)
}

func storedPayloadLineSeq(t *testing.T, seq uint64, payload map[string]any) []byte {
	t.Helper()
	rec := types.Record{
		Seq: seq, RecID: types.NewID(types.PEv), TS: types.FormatUTC(testNow()), Kind: types.KConfig,
		SchemaVersion: SchemaVersionV1,
		Origin:        types.Origin{HostID: "7f3a91c2d4e5b607", Source: "lifecycle"},
		Actor:         testActor(),
		Payload:       payload,
	}
	b, err := marshalRecordLine(&rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return bytes.TrimRight(b, "\n")
}

// rawPayloadOf extracts the stored payload object bytes of a line.
func rawPayloadOf(t *testing.T, line []byte) json.RawMessage {
	t.Helper()
	var probe struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return probe.Payload
}

// legacyPayloadHashFromLine is the pre-TRBL-106 verifier: decode the line into
// maps (nested structs become map[string]any), drop payload_sha256, re-marshal —
// Go sorts those map keys, so this is the form that never matched a
// struct-ordered payload.
func legacyPayloadHashFromLine(t *testing.T, line []byte) string {
	t.Helper()
	rec, err := decodeRecord(line, true)
	if err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	cp := map[string]any{}
	for k, v := range rec.Payload {
		if k == "payload_sha256" {
			continue
		}
		cp[k] = v
	}
	return payloadHash(cp)
}

// dayFileName is the authoritative part file the fake clock writes into.
func dayFileName() string { return testNow().Format(dayLayout) + ".jsonl" }

// recordLine returns the stored line whose seq matches, from the ledger's day
// file.
func recordLine(t *testing.T, l *Ledger, seq uint64) []byte {
	t.Helper()
	for _, ln := range readFileLines(t, l.Root(), dayFileName()) {
		var probe struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(ln, &probe); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		if probe.Seq == seq {
			return ln
		}
	}
	t.Fatalf("no stored line for seq %d in %s", seq, dayFileName())
	return nil
}

// TestVerifyPayloadHashStructOrderedPayload is the end-to-end regression: the
// writer stamps the hash over struct-ordered bytes, verify recomputes over the
// stored bytes, and the walk reports no findings (acceptance criterion 1).
func TestVerifyPayloadHashStructOrderedPayload(t *testing.T) {
	t.Run("config snapshot", func(t *testing.T) {
		clk := newFakeClock(testNow())
		l := testLedger(t, clk)
		payload := configSnapshotPayload(40, "")
		rec := mustAppend(t, l, types.RecordDraft{
			Kind:    types.KConfig,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "lifecycle"},
			Actor:   testActor(),
			Payload: payload,
		})
		stored, _ := rec.Payload["payload_sha256"].(string)
		if stored == "" {
			t.Fatalf("config payload %d bytes carries no payload_sha256", payloadBytes(rec.Payload))
		}
		line := recordLine(t, l, rec.Seq)

		// Premise 1: the stored bytes carry struct field order.
		structOrdered, err := json.Marshal(configValues(40, "")[0])
		if err != nil {
			t.Fatalf("marshal struct value: %v", err)
		}
		if !bytes.Contains(line, structOrdered) {
			t.Fatalf("stored line does not carry the struct-ordered value %s", structOrdered)
		}
		// Premise 2: the alphabetically re-marshaled form is NOT what is stored
		// — that is the byte stream the pre-fix verifier hashed.
		alphaOrdered, err := json.Marshal(map[string]any{
			"key": "sentinel.routes.key_000", "value": padTo(64), "source": "default",
			"source_ref": "builtin", "redacted": true,
		})
		if err != nil {
			t.Fatalf("marshal map value: %v", err)
		}
		if bytes.Contains(line, alphaOrdered) {
			t.Fatalf("stored line unexpectedly carries map-key order %s", alphaOrdered)
		}
		// Premise 3: the pre-fix verifier would have flagged exactly this record.
		if legacy := legacyPayloadHashFromLine(t, line); legacy == stored {
			t.Fatalf("legacy re-marshal hash %s equals the stored hash; the regression is not reproduced", legacy)
		}
		if got, err := verifyPayloadHashLine(line); err != nil || got != stored {
			t.Fatalf("verifyPayloadHashLine = %q, %v; want the stored %q", got, err, stored)
		}

		rep, err := l.Verify(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if rep.PayloadHashMismatches != 0 {
			t.Errorf("PayloadHashMismatches = %d (%v), want 0", rep.PayloadHashMismatches, rep.Errors)
		}
		if !rep.OK {
			t.Errorf("VerifyReport.OK = false, want true: %v", rep.Errors)
		}
	})

	t.Run("lifecycle record with nested struct", func(t *testing.T) {
		clk := newFakeClock(testNow())
		l := testLedger(t, clk)
		payload := map[string]any{
			"op":      "compaction",
			"holes":   []hashStructNested{{Op: "hole", FromSeq: 7, ToSeq: 9, Note: padTo(64)}},
			"retaint": map[string]any{"compacted": 41, "raw_keep": "168h", "note": padTo(3800)},
			"pad":     padTo(1200),
		}
		rec := mustAppend(t, l, hashStruct(payload))
		if h, _ := rec.Payload["payload_sha256"].(string); h == "" {
			t.Fatalf("lifecycle payload carries no payload_sha256")
		}
		line := recordLine(t, l, rec.Seq)
		if legacy := legacyPayloadHashFromLine(t, line); legacy == rec.Payload["payload_sha256"] {
			t.Fatalf("legacy re-marshal matched the stored hash; struct order not exercised")
		}
		rep, err := l.Verify(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if rep.PayloadHashMismatches != 0 || !rep.OK {
			t.Errorf("lifecycle record: mismatches=%d ok=%v errors=%v, want 0/true",
				rep.PayloadHashMismatches, rep.OK, rep.Errors)
		}
	})

	t.Run("map-only payload is unchanged", func(t *testing.T) {
		// A control: payloads whose nested values are already maps agreed under
		// the old verifier too, so the fix must leave them alone.
		clk := newFakeClock(testNow())
		l := testLedger(t, clk)
		rec := mustAppend(t, l, types.RecordDraft{
			Kind: types.KEvent, Sig: "psi:sha256v1:mapcontrol",
			Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"}, Actor: testActor(),
			Payload: map[string]any{"op": "sample", "pad": padTo(5000)},
		})
		stored, _ := rec.Payload["payload_sha256"].(string)
		if stored == "" {
			t.Fatalf("map payload carries no payload_sha256")
		}
		line := recordLine(t, l, rec.Seq)
		if legacy := legacyPayloadHashFromLine(t, line); legacy != stored {
			t.Fatalf("map-only payload: legacy hash %s != stored %s; the control is not a control", legacy, stored)
		}
		rep, err := l.Verify(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if rep.PayloadHashMismatches != 0 || !rep.OK {
			t.Errorf("map payload: mismatches=%d ok=%v errors=%v, want 0/true",
				rep.PayloadHashMismatches, rep.OK, rep.Errors)
		}
	})
}

// TestVerifyPayloadHashTamperedStillReported is the negative control: the fix
// must not make the hash check vacuous. A stored payload whose bytes changed
// under a stamped hash is still a finding (§5 code 011).
func TestVerifyPayloadHashTamperedStillReported(t *testing.T) {
	const marker = "TAMPER-TARGET-0001"
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	rec := mustAppend(t, l, types.RecordDraft{
		Kind:    types.KConfig,
		Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "lifecycle"},
		Actor:   testActor(),
		Payload: configSnapshotPayload(40, marker+padTo(64)),
	})
	path := filepath.Join(l.Root(), dayFileName())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the payload value in place (same length, whitespace-free line).
	after := bytes.Replace(before, []byte(marker), []byte("TAMPER-TARGET-0002"), 1)
	if bytes.Equal(before, after) {
		t.Fatalf("tamper marker absent from the stored line")
	}
	if err := os.WriteFile(path, after, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Verify(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.PayloadHashMismatches != 1 {
		t.Errorf("PayloadHashMismatches = %d (%v), want exactly 1", rep.PayloadHashMismatches, rep.Errors)
	}
	if rep.OK {
		t.Errorf("VerifyReport.OK = true after tampering, want false")
	}
	wantErr := fmt.Sprintf("seq %d: payload hash mismatch", rec.Seq)
	if !containsLine(rep.Errors, wantErr) {
		t.Errorf("errors = %v, want one naming %q", rep.Errors, wantErr)
	}
}

// TestVerifyPayloadHashMismatchDifferential runs two lines through one walk: a
// correct struct-ordered record and a copy whose stamped hash belongs to other
// bytes. Only the second is a finding — the check discriminates rather than
// failing (or passing) everything.
func TestVerifyPayloadHashMismatchDifferential(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	goodPayload := configSnapshotPayload(40, "DIFFERENTIAL-GOOD-0001")
	goodHash := payloadHash(goodPayload)
	if goodHash == "" {
		t.Fatalf("write boundary produced no hash for the good fixture")
	}
	goodPayload["payload_sha256"] = goodHash
	good := storedPayloadLineSeq(t, 1, goodPayload)

	badPayload := configSnapshotPayload(40, "DIFFERENTIAL-BAD-0002")
	badHash := payloadHash(badPayload)
	if badHash == "" || badHash == goodHash {
		t.Fatalf("bad fixture hash = %q (good %q); the differential proves nothing", badHash, goodHash)
	}
	badPayload["payload_sha256"] = badHash
	bad := storedPayloadLineSeq(t, 2, badPayload)
	if got, err := verifyPayloadHashLine(good); err != nil || got != goodHash {
		t.Fatalf("good fixture does not verify: %q, %v (want %q)", got, err, goodHash)
	}
	if got, err := verifyPayloadHashLine(bad); err != nil || got != badHash {
		t.Fatalf("bad fixture does not verify before tampering: %q, %v (want %q)", got, err, badHash)
	}
	// Give the second line the first line's hash: its payload no longer matches
	// its own stamp.
	const stamped = `"payload_sha256":"`
	idx := bytes.Index(bad, []byte(stamped))
	if idx < 0 {
		t.Fatalf("no payload_sha256 member in the stored line")
	}
	start := idx + len(stamped)
	end := start + len(badHash)
	if string(bad[start:end]) != badHash {
		t.Fatalf("stamped member is %q, want %q", bad[start:end], badHash)
	}
	tampered := append([]byte(nil), bad...)
	copy(tampered[start:end], goodHash)

	writeLines(t, root, dayFileName(), [][]byte{good, tampered})
	l := testLedgerAt(t, root, newFakeClock(day))
	rep, err := l.Verify(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.PayloadHashMismatches != 1 {
		t.Errorf("PayloadHashMismatches = %d (%v), want exactly 1 (line 1 is sound)", rep.PayloadHashMismatches, rep.Errors)
	}
	if rep.OK {
		t.Errorf("VerifyReport.OK = true, want false")
	}
	if !containsLine(rep.Errors, "seq 2: payload hash mismatch") {
		t.Errorf("errors = %v, want one naming the tampered seq 2", rep.Errors)
	}
	if rep.Lines < 2 {
		t.Errorf("Lines = %d, want at least the 2 fixtures", rep.Lines)
	}
}

// TestVerifyPayloadHashLineMatchesWriteBoundaryHash pins the contract the fix
// rests on: for every payload family, the hash recomputed from the STORED bytes
// (minus the reserved payload_sha256 member) is the hash the write boundary
// stamped — and the byte stream it hashes is byte-identical to the write
// boundary's canonical payload JSON. The sha member is exercised at the first,
// middle and last positions of the encoder's sorted key order.
func TestVerifyPayloadHashLineMatchesWriteBoundaryHash(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"struct-ordered config values", configSnapshotPayload(40, "")},
		{"struct-ordered lifecycle record", map[string]any{
			"op":    "retention",
			"holes": []hashStructNested{{Op: "hole", FromSeq: 1, ToSeq: 3, Note: padTo(64)}},
			"pad":   padTo(4100),
		}},
		{"map-only payload", map[string]any{"op": "sample", "pad": padTo(5000)}},
		{"nested member named payload_sha256 is preserved", map[string]any{
			"nested": map[string]any{"payload_sha256": "nested-not-reserved", "x": padTo(4200)},
			"pad":    padTo(64),
		}},
		{"sha member sorts first", map[string]any{"aaa.pad": padTo(4200)}},
		{"sha member sorts last", map[string]any{"zzz.pad": padTo(4200)}},
		{"empty-ish payload", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := payloadHash(tc.payload)
			if want == "" {
				t.Fatalf("write boundary produced no hash")
			}
			// What the writer stores: the payload plus the stamped hash.
			stored := map[string]any{}
			for k, v := range tc.payload {
				stored[k] = v
			}
			stored["payload_sha256"] = want
			line := storedPayloadLine(t, stored)

			// The byte stream must be the write boundary's canonical form.
			gotBody, err := payloadBytesWithoutHash(rawPayloadOf(t, line))
			if err != nil {
				t.Fatalf("payloadBytesWithoutHash: %v", err)
			}
			wantBody, err := canonicalPayloadJSON(tc.payload)
			if err != nil {
				t.Fatalf("canonicalPayloadJSON: %v", err)
			}
			if !bytes.Equal(gotBody, wantBody) {
				t.Fatalf("verifier bytes differ from the hashed bytes:\n got %s\nwant %s", gotBody, wantBody)
			}
			got, err := verifyPayloadHashLine(line)
			if err != nil {
				t.Fatalf("verifyPayloadHashLine: %v", err)
			}
			if got != want {
				t.Fatalf("verifyPayloadHashLine = %q, want the write-boundary hash %q", got, want)
			}
		})
	}
}

// TestVerifyPayloadHashEmptyPayloadNoMember: a record with no payload_sha256 is
// not a hash-check candidate at all — the walk must leave it alone (no
// fabricated empty-string comparison).
func TestVerifyPayloadHashEmptyPayloadNoMember(t *testing.T) {
	root := testRoot(t)
	rec := types.Record{
		Seq: 1, RecID: types.NewID(types.PEv), TS: types.FormatUTC(testNow()), Kind: types.KConfig,
		SchemaVersion: SchemaVersionV1,
		Origin:        types.Origin{HostID: "7f3a91c2d4e5b607", Source: "lifecycle"},
		Actor:         testActor(),
		Payload:       map[string]any{"op": "boot"},
	}
	line, err := marshalRecordLine(&rec)
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, root, dayFileName(), [][]byte{bytes.TrimRight(line, "\n")})
	l := testLedgerAt(t, root, newFakeClock(testNow()))
	rep, err := l.Verify(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.PayloadHashMismatches != 0 || !rep.OK {
		t.Errorf("mismatches=%d ok=%v errors=%v, want 0/true", rep.PayloadHashMismatches, rep.OK, rep.Errors)
	}
}

func containsLine(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
