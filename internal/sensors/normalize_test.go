package sensors

import (
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// normalize_test.go is the interop proof between this package and the dedup
// core: the sig bytes SPEC-03 produces must equal SPEC-01 §3.3's published
// vectors exactly, full 32-byte digest and 16-hex short. SPEC-03 does not
// import internal/ledger (SPEC-INDEX §4.1), so the field lists are implemented
// here — and this table is what keeps the two implementations from drifting
// into two signature spaces.

func TestSigVectorsPublishedBySPEC01(t *testing.T) {
	vectors := []struct {
		source types.SigSource
		fields []string
		digest string
		short  string
	}{
		{types.SrcPSI, []string{"cpu", "some", "b3"},
			"d8d1d8cc6f8c431d1b99197c3a52134a0a7d037e8ab9aa3bce221bbf5ddc7296", "d8d1d8cc6f8c431d"},
		{types.SrcJournald, []string{"payment-worker", "queue wedge: pool exhausted, retry # in # ms"},
			"8721b2ea46c6cf26b09fe04f73f41b05fe3660e5c21ddefdbae6686708a13e4a", "8721b2ea46c6cf26"},
		{types.SrcDBus, []string{"user:1000", "payment-worker.service", "auto-restart"},
			"2b180920160f1ea8a68a3cd63856212c1e78d01abc9e18572b5d0be76f7cea3e", "2b180920160f1ea8"},
		{types.SrcDisk, []string{"/", "free_pct"},
			"ca5ae505ad8384448d929b5e4a8d7414f8d77db1973115723736c5ce25138936", "ca5ae505ad838444"},
		{types.SrcTimers, []string{"backup.timer", "missed"},
			"e529b7377e31655e90f5987023d0d8bb626616b41605939cd6b2e6429877dcc5", "e529b7377e31655e"},
		{types.SrcInotify, []string{"/etc/payment/config.toml", "modify"},
			"7ebd19985bf29592ac9e990c0703028cb3ad0fa4f32a983cc53fb47c62939c2f", "7ebd19985bf29592"},
		{types.SrcSentinel, []string{"1", "worker.claim", "queue-wedge"},
			"18b12eea8a3929236822338d1b8d04055a2a670104b97782356e1559fc83f385", "18b12eea8a392923"},
	}
	for _, v := range vectors {
		got := sigFor(v.source, v.fields...)
		if got.DigestHex() != v.digest {
			t.Errorf("%s %v: digest = %s, want %s", v.source, v.fields, got.DigestHex(), v.digest)
		}
		if got.Short != v.short {
			t.Errorf("%s %v: short = %s, want %s", v.source, v.fields, got.Short, v.short)
		}
		if !strings.HasPrefix(got.String(), string(v.source)+":sha256v1:") {
			t.Errorf("%s: canonical string = %s", v.source, got.String())
		}
	}
}

func TestMergeKeyVectorsPublishedBySPEC01(t *testing.T) {
	vectors := []struct {
		class, subject string
		short          string
	}{
		{"crash_loop", "payment-worker.service", "bcc48494f2190f4f"},
		{"resource_exhaustion", "io", "a11a6c52eab58b3a"},
		{"disk_full", "/", "8b3c2119f8410f15"},
	}
	for _, v := range vectors {
		if got := mergeKey(v.class, v.subject); got != v.short {
			t.Errorf("mergeKey(%q,%q) = %s, want %s", v.class, v.subject, got, v.short)
		}
	}
}

func TestClassForTable(t *testing.T) {
	cases := []struct {
		source types.SigSource
		state  string
		want   string
	}{
		{types.SrcPSI, "", "resource_exhaustion"},
		{types.SrcDisk, "free_pct", "disk_full"},
		{types.SrcDisk, "inode_pct", "disk_full"},
		{types.SrcDisk, "io_error", "io_stall"},
		{types.SrcDisk, "readonly", "config_error"},
		{types.SrcTimers, "missed", "timer_missed"},
		{types.SrcInotify, "modify", "file_flap"},
		{types.SrcDBus, "failed", "crash_loop"},
		{types.SrcDBus, "auto-restart", "crash_loop"},
		{types.SrcDBus, "dead", "unit_degraded"},
		{types.SrcJournald, "", "crash_loop"},
	}
	for _, c := range cases {
		if got := classFor(c.source, c.state); got != c.want {
			t.Errorf("classFor(%s,%q) = %q, want %q", c.source, c.state, got, c.want)
		}
	}
}

func TestPSIBucketBoundaries(t *testing.T) {
	cases := []struct {
		avg10 float64
		want  string
	}{
		{0, "b1"}, {9.99, "b1"}, {10.0, "b2"}, {24.99, "b2"},
		{25.0, "b3"}, {49.99, "b3"}, {50.0, "b4"}, {1e6, "b4"},
	}
	for _, c := range cases {
		if got := psiBucket(c.avg10); got != c.want {
			t.Errorf("psiBucket(%v) = %s, want %s", c.avg10, got, c.want)
		}
	}
}

func TestMessageNormIsAFixedPoint(t *testing.T) {
	in := "queue wedge: pool exhausted, retry 8812 in 30 ms\n0xDEADBEEF at 12:00:00\nuuid 3f2504e0-4f89-11d3-9a0c-0305e82c3301\nfourth line dropped"
	once := messageNorm(in)
	twice := messageNorm(once)
	if once != twice {
		t.Fatalf("messageNorm is not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
	if strings.Count(once, "\n") > MessageNormMaxLines-1 {
		t.Fatalf("messageNorm kept too many lines: %q", once)
	}
	if strings.Contains(once, "0xDEADBEEF") {
		t.Fatalf("hex was not masked: %q", once)
	}
	if strings.Contains(once, "3f2504e0-4f89-11d3-9a0c-0305e82c3301") {
		t.Fatalf("uuid was not masked: %q", once)
	}
	long := messageNorm(strings.Repeat("abcdefghij ", 200))
	if len(long) > MessageNormBudgetBytes {
		t.Fatalf("messageNorm produced %d bytes, budget is %d", len(long), MessageNormBudgetBytes)
	}
	if strings.HasSuffix(long, "\uFFFD") {
		t.Fatal("truncation must land on a rune boundary")
	}
}

// TestMasksRemovedTheDocumentedSubstrings covers the SPEC-03 §3.1 mask set.
func TestMasksRemovedTheDocumentedSubstrings(t *testing.T) {
	in := "2026-09-16T09:00:01.004Z pid=4242 port=8080 id 1234567890 hash deadbeefcafe file ~/x.log"
	out := maskMessage(in)
	for _, forbidden := range []string{"2026-09-16T09:00:01.004Z", "4242", "8080", "1234567890", "deadbeefcafe", "~", "opuser"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("mask set left %q in %q", forbidden, out)
		}
	}
	if !strings.Contains(out, "x.log") {
		t.Errorf("the basename must survive masking: %q", out)
	}
	// Basename-resolved paths: a random temp component must not become a new sig.
	two := []string{maskMessage("error at /tmp/abc123/x.log"), maskMessage("error at /tmp/zzz999/x.log")}
	if two[0] != two[1] {
		t.Fatalf("identical-shaped paths must mask to one value: %q vs %q", two[0], two[1])
	}
}

func TestUnitIdentFallback(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{scope: "u", ring: make([]string, journalCursorRing), q: newJournalQueue(8192, 0)}
	// _SYSTEMD_UNIT wins; SYSLOG_IDENTIFIER is next.
	line := `{"__CURSOR":"c=1","MESSAGE":"boom","SYSLOG_IDENTIFIER":"app","_COMM":"comm","PRIORITY":"3","__REALTIME_TIMESTAMP":"1758012345000000"}`
	e, err := h.s.parseJournalEntry(f, []byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h.s.drainJournal(t.Context(), f, e)
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	wantSig := sigFor(types.SrcJournald, "app", messageNorm("boom")).String()
	if recs[0].Sig != wantSig {
		t.Fatalf("sig = %s, want %s (SYSLOG_IDENTIFIER fallback)", recs[0].Sig, wantSig)
	}
}
