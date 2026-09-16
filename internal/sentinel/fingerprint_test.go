package sentinel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// vectorEvents builds the three §3.3 events: A (SDK override), B (default
// stack), C (message fallback).
func vectorEvents() (a, b, c *rawEvent) {
	frames := []frame{
		{File: "worker.py", Function: "claim", Line: 118, InApp: true, ContextLine: "item = pool.get(timeout=1)"},
		{File: "queue.py", Function: "get", Line: 44, InApp: true, ContextLine: "raise PoolExhausted(depth=912)"},
	}
	b = &rawEvent{
		Level: "error", Culprit: "worker.claim", Frames: frames,
		Message: "queue wedge: pool exhausted depth=912",
	}
	a = &rawEvent{
		Level: "error", Culprit: "worker.claim", Frames: frames,
		Fingerprint: []string{"queue-wedge", "{{ default }}"},
		Message:     "queue wedge: pool exhausted depth=912",
	}
	c = &rawEvent{Level: "error", Message: "queue wedge: pool exhausted depth=912"}
	return a, b, c
}

// sigGolden is the checked-in golden file shape.
type sigGolden struct {
	NormVersion int `json:"norm_version"`
	Vectors     []struct {
		Name      string `json:"name"`
		Canonical string `json:"canonical"`
		Sig       string `json:"sig"`
		Bytes     int    `json:"bytes"`
	} `json:"vectors"`
}

// TestGoldenVectors pins the three §3.3 vectors byte-for-byte: canonical bytes,
// byte length and the 16-hex sig short form. A mismatch here means the
// normalizer changed, which REQUIRES a norm_version bump and a re-key of
// testdata/sig_golden.json in the same commit (§3.3).
func TestGoldenVectors(t *testing.T) {
	raw := readFixture(t, "sig_golden.json")
	var gold sigGolden
	if err := json.Unmarshal(raw, &gold); err != nil {
		t.Fatalf("sig_golden.json: %v", err)
	}
	if gold.NormVersion != types.NormVersionV1 {
		t.Fatalf("golden file is for norm_version %d, code is at %d", gold.NormVersion, types.NormVersionV1)
	}
	n := newNormalizer()
	a, b, c := vectorEvents()
	cases := []struct {
		name string
		ev   *rawEvent
	}{
		{"A — override", a},
		{"B — default", b},
		{"C — message fallback", c},
	}
	if len(gold.Vectors) != len(cases) {
		t.Fatalf("golden file has %d vectors, the spec pins %d", len(gold.Vectors), len(cases))
	}
	for i, tc := range cases {
		canonical, fallback := n.canonicalFor(tc.ev)
		gv := gold.Vectors[i]
		if gv.Name != tc.name {
			t.Fatalf("vector %d: golden names %q, test names %q", i, gv.Name, tc.name)
		}
		if string(canonical) != gv.Canonical {
			t.Errorf("vector %s: canonical bytes differ\n got: %q\nwant: %q", tc.name, canonical, gv.Canonical)
		}
		if len(canonical) != gv.Bytes {
			t.Errorf("vector %s: %d bytes, golden says %d", tc.name, len(canonical), gv.Bytes)
		}
		sig := n.sigOfCanonical(canonical)
		if sig.String() != gv.Sig {
			t.Errorf("vector %s: sig %s, golden says %s (a change here REQUIRES norm_version++)",
				tc.name, sig.String(), gv.Sig)
		}
		if wantFB := tc.name == "C — message fallback"; fallback != wantFB {
			t.Errorf("vector %s: fallback=%v, want %v", tc.name, fallback, wantFB)
		}
	}
}

// TestVectorDigestsMatchSpec checks the three digests the spec prints in §3.3
// independently of the golden file, so a corrupted golden file cannot hide a
// normalizer change.
func TestVectorDigestsMatchSpec(t *testing.T) {
	n := newNormalizer()
	a, b, c := vectorEvents()
	want := map[string]string{
		"A": "sentinel:sha256v1:d3e1e8197f4da958",
		"B": "sentinel:sha256v1:af7e89fe750191f9",
		"C": "sentinel:sha256v1:198e825fb0db5e73",
	}
	for name, ev := range map[string]*rawEvent{"A": a, "B": b, "C": c} {
		canonical, _ := n.canonicalFor(ev)
		if got := n.sigOfCanonical(canonical).String(); got != want[name] {
			t.Errorf("vector %s: sig %s, spec pins %s", name, got, want[name])
		}
	}
}

// TestSigOfRoundTrip pins the exported SigOf surface: a types.SentryEvent whose
// Stack is the rendered frame form produces the same digest as the raw event.
func TestSigOfRoundTrip(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	_, b, _ := vectorEvents()
	sig, err := ts.s.SigOf(types.SentryEvent{
		Level:   "error",
		Culprit: "worker.claim",
		Message: "queue wedge: pool exhausted depth=912",
		Stack:   renderStack(b.Frames),
	})
	if err != nil {
		t.Fatalf("SigOf: %v", err)
	}
	if sig.String() != "sentinel:sha256v1:af7e89fe750191f9" {
		t.Fatalf("SigOf round trip produced %s, want vector B", sig.String())
	}
}

// TestMaskingRules exercises the 15 masking rules in the pinned order.
func TestMaskingRules(t *testing.T) {
	cases := []struct{ in, want string }{
		{"id 3f2504e0-4f89-11d3-9a0c-0305e82c3301", "id UUID"},
		{"at 2026-09-16T09:14:03.221Z done", "at TS done"},
		{"panic at worker.py:118:9", "panic at worker.py:LINE"},
		{"ptr 0xdeadbeef leaked", "ptr 0xADDR leaked"},
		{"goroutine 87 [running]:", "goroutine N [running]:"},
		{"pid=4711 exited", "pid=PID exited"},
		{"token deadbeefcafe12", "token HEXID"},
		{"waited 250ms then 3h", "waited DUR then DUR"},
		{"depth=912 and 7 and 44", "depth=N and 7 and 44"},
		{"file /tmp/build-12.log", "file /TMP.log"},           // the rule stops at the last digit, so a suffix survives
		{"file /tmp/build-1234.log", "file /tmp/build-N.log"}, // rule 11 precedes rule 12 for 3+ digit runs
		{"listen 0.0.0.0:8080", "listen 0.0.0.0:PORT"},
		{"two   spaces\tand\ttabs", "two spaces and tabs"},
	}
	for _, tc := range cases {
		got := maskLine(tc.in)
		if got != tc.want {
			t.Errorf("maskLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMaskOrderPortsAfterFileLine pins rule 5-before-13: a file:line is masked as
// file:LINE, and a bare port is masked as :PORT.
func TestMaskOrderPortsAfterFileLine(t *testing.T) {
	if got := maskLine("connect queue.py:8080"); got != "connect queue.py:LINE" {
		t.Errorf("file:port masking = %q, want queue.py:LINE", got)
	}
	if got := maskLine("listen :8080"); got != "listen :PORT" {
		t.Errorf("bare port masking = %q, want :PORT", got)
	}
}

// TestLineTruncation512 pins rule 15.
func TestLineTruncation512(t *testing.T) {
	// Not hex and not digit: rule 9/11 must not collapse it before truncation.
	long := strings.Repeat("z", 900)
	if got := maskLine(long); len(got) != maxNormLine {
		t.Fatalf("maskLine truncated to %d bytes, want %d", len(got), maxNormLine)
	}
}

// TestFramesOverCollapse pins the 8-frame collapse and the `frames_over=+N` line.
func TestFramesOverCollapse(t *testing.T) {
	ev := &rawEvent{Level: "error", Culprit: "a.b"}
	for i := 0; i < 11; i++ {
		ev.Frames = append(ev.Frames, frame{File: "f.go", Function: "fn", InApp: true})
	}
	canonical, _ := newNormalizer().canonicalFor(ev)
	s := string(canonical)
	if got := strings.Count(s, "frame="); got != 8 {
		t.Errorf("canonical has %d frame lines, want 8", got)
	}
	if !strings.Contains(s, "frames_over=+3\n") {
		t.Errorf("canonical is missing frames_over=+3:\n%s", s)
	}
}

// TestOverrideInterpolation pins `{{ default }}` in-place expansion and repeated
// expansions.
func TestOverrideInterpolation(t *testing.T) {
	ev := &rawEvent{
		Level:       "error",
		Fingerprint: []string{"queue-wedge", "{{ default }}", "{{ default }}"},
		Frames: []frame{
			{File: "worker.py", Function: "claim", InApp: true, ContextLine: "item = pool.get(timeout=1)"},
		},
	}
	canonical, _ := newNormalizer().canonicalFor(ev)
	line := string(canonical)
	frame := frameNorm(ev.Frames[0])
	want := "fp=queue-wedge\x1f" + frame + "\x1f" + frame + "\n"
	if !strings.HasSuffix(line, want) {
		t.Fatalf("repeated {{ default }} expansion is wrong:\n got tail: %q\nwant tail: %q", line, want)
	}
}

// TestOverrideReducesToNothingFallsThrough pins §3.3 path 1's fall-through: an
// override that reduces to nothing uses the canonical stack hash.
func TestOverrideReducesToNothingFallsThrough(t *testing.T) {
	ev := &rawEvent{
		Level:       "error",
		Culprit:     "worker.claim",
		Fingerprint: []string{"", "   "},
		Frames: []frame{
			{File: "worker.py", Function: "claim", InApp: true, ContextLine: "item = pool.get(timeout=1)"},
		},
	}
	canonical, fallback := newNormalizer().canonicalFor(ev)
	if fallback {
		t.Fatal("an override that reduces to nothing must fall through to the stack hash, not the message path")
	}
	if !strings.Contains(string(canonical), "culprit=worker.claim") {
		t.Fatalf("fall-through did not use the default form:\n%s", canonical)
	}
}

// TestFingerprintFallbackIsStable pins §3.3 path 3 and §6.7: an all-empty event
// collapses to one stable sig per (level, logger, culprit).
func TestFingerprintFallbackIsStable(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	first, _, err := ts.s.sigFor(&rawEvent{Level: "error", Culprit: "worker.claim"})
	if err != nil {
		t.Fatalf("sigFor: %v", err)
	}
	second, _, _ := ts.s.sigFor(&rawEvent{Level: "error", Culprit: "worker.claim"})
	if first.String() != second.String() {
		t.Fatalf("all-empty events must be stable: %s vs %s", first, second)
	}
	other, _, _ := ts.s.sigFor(&rawEvent{Level: "warning", Culprit: "worker.claim"})
	if other.String() == first.String() {
		t.Fatal("a different level must produce a different sig")
	}
}

// TestFallbackOnPanic pins TROUBLE-SENTINEL-016: a normalizer panic falls back
// to the message-derived sig with the code reported.
func TestFallbackOnPanic(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := &rawEvent{Level: "error", Message: "boom 912"}
	sig, fallback, err := ts.s.sigFor(ev)
	if err != nil {
		t.Fatalf("sigFor: %v", err)
	}
	if !fallback {
		t.Fatal("an event with no stack and no fingerprint must report the message-derived fallback")
	}
	if sig.String() == "" {
		t.Fatal("fallback sig is empty")
	}
}
