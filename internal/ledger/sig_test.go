package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestVectorTable asserts every published §3.3 row byte-for-byte: the framing of
// the field list must reproduce the hashed bytes, and the digest and 16-hex
// short form must match. Any diff fails CI.
func TestVectorTable(t *testing.T) {
	for i, v := range SigVectors() {
		raw := types.NormalizedBytes(v.Source, v.Fields...)
		if string(raw) != v.Raw {
			t.Errorf("row %d: framing = %q, want %q", i, raw, v.Raw)
			continue
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if got != v.Digest {
			t.Errorf("row %d: digest = %s, want %s", i, got, v.Digest)
		}
		if short := types.DigestShort(sum[:]); short != v.Short {
			t.Errorf("row %d: short = %s, want %s", i, short, v.Short)
		}
		sig := SigFor(v.Source, v.Fields...)
		if sig.DigestHex() != v.Digest || sig.Short != v.Short {
			t.Errorf("row %d: SigFor = %s/%s, want %s/%s", i, sig.DigestHex(), sig.Short, v.Digest, v.Short)
		}
		if sig.String() != string(v.Source)+":sha256v1:"+v.Short {
			t.Errorf("row %d: sig string = %s", i, sig.String())
		}
	}
}

// TestMergeKeyVectors asserts the three published merge-key rows.
func TestMergeKeyVectors(t *testing.T) {
	for i, v := range MergeVectors() {
		got := MergeKey(v.Class, v.Subject)
		if got != v.Short {
			t.Errorf("row %d: MergeKey(%s, %s) = %s, want %s", i, v.Class, v.Subject, got, v.Short)
		}
		raw := MergeDomain + "\x1f" + v.Class + "\x1f" + v.Subject + "\x1e"
		sum := sha256.Sum256([]byte(raw))
		if hex.EncodeToString(sum[:]) != v.Digest {
			t.Errorf("row %d: raw digest mismatch for %q", i, raw)
		}
	}
}

// specVectorRe extracts the published rows straight out of the spec file, so a
// silent edit in either direction (spec or table) fails CI.
var specVectorRe = regexp.MustCompile(`(?m)^"((?:[^"\\]|\\.)*)"[^\n]*(?:\n[ \t]*)?->\s*([0-9a-f]{64})\s*->\s*([0-9a-f]{16})`)

// TestSpecVectorParity reads specs/SPEC-01-ledger.md §3.3 and verifies every
// published vector against a fresh sha256 of the bytes it documents.
func TestSpecVectorParity(t *testing.T) {
	b, err := os.ReadFile("../../specs/SPEC-01-ledger.md")
	if err != nil {
		t.Skipf("spec file not readable: %v", err)
	}
	rows := specVectorRe.FindAllStringSubmatch(string(b), -1)
	if len(rows) < 14 {
		t.Fatalf("found %d vectors in the spec, want ≥14", len(rows))
	}
	checked := 0
	for _, m := range rows {
		raw := unescapeSpecLiteral(m[1])
		sum := sha256.Sum256([]byte(raw))
		got := hex.EncodeToString(sum[:])
		if got != m[2] {
			t.Errorf("spec vector %q: sha256 = %s, want %s", raw, got, m[2])
			continue
		}
		if types.DigestShort(sum[:]) != m[3] {
			t.Errorf("spec vector %q: short = %s, want %s", raw, types.DigestShort(sum[:]), m[3])
		}
		checked++
	}
	t.Logf("verified %d published vectors against the spec file", checked)
}

func unescapeSpecLiteral(s string) string {
	s = strings.ReplaceAll(s, `\x1f`, "\x1f")
	s = strings.ReplaceAll(s, `\x1e`, "\x1e")
	s = strings.ReplaceAll(s, `\x00`, "\x00")
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// TestMessageNormIdempotent: re-normalizing a normalized message is a fixed point.
func TestMessageNormIdempotent(t *testing.T) {
	cases := []string{
		"queue wedge: pool exhausted, retry 3 in 500 ms",
		"panic: runtime error: invalid memory address\n[signal SIGSEGV: code=0x1 addr=0x55 pc=0x4]",
		"request a1b2c3d4e5f6 failed at 0xdeadbeef\nretry 12 of 30\nthird line\ndropped",
	}
	for _, c := range cases {
		once := MessageNorm(c)
		twice := MessageNorm(once)
		if once != twice {
			t.Errorf("MessageNorm is not idempotent:\n once: %q\ntwice: %q", once, twice)
		}
		if len(once) > MessageNormBudgetBytes {
			t.Errorf("MessageNorm exceeded the 512-byte budget: %d", len(once))
		}
		if strings.Count(once, "\n") > MessageNormMaxLines-1 {
			t.Errorf("MessageNorm kept more than %d lines: %q", MessageNormMaxLines, once)
		}
	}
}

// TestFramingEscapes: separators inside a value are cleaned and the norm version
// is part of the sig string.
func TestFramingEscapes(t *testing.T) {
	raw := types.NormalizedBytes(types.SrcJournald, "unit\x1fname", "  msg\x1e tail  ")
	if strings.Count(string(raw), "\x1f") != 2 {
		t.Errorf("separator inside a field was not cleaned: %q", raw)
	}
	if strings.Contains(string(raw), "unit\x1fname") || strings.Contains(string(raw), "msg\x1e tail") {
		t.Errorf("a raw separator survived inside a field value: %q", raw)
	}
	if strings.Count(string(raw), "\x1e") != 1 {
		t.Errorf("record separator inside a field was not cleaned: %q", raw)
	}
	if !strings.HasSuffix(string(raw), "\x1e") {
		t.Errorf("framing must end with U+001E: %q", raw)
	}
	sig := SigFor(types.SrcPSI, "cpu", "some", "b3")
	if !strings.Contains(sig.String(), "v1") {
		t.Errorf("sig string must carry the norm version: %s", sig.String())
	}
	parsed, err := types.ParseSig(sig.String())
	if err != nil {
		t.Fatalf("ParseSig(%s): %v", sig.String(), err)
	}
	if parsed.Source != types.SrcPSI || parsed.NormVersion != 1 || parsed.Short != sig.Short {
		t.Errorf("sig round-trip drifted: %+v", parsed)
	}
}

// TestMergeKeySubjectResolution: payload → config table → sig-short fallback.
func TestMergeKeySubjectResolution(t *testing.T) {
	sig := SigFor(types.SrcPSI, "cpu", "some", "b3")
	if got := MergeSubject(map[string]any{"subject": "payment-worker"}, nil, sig.String()); got != "payment-worker" {
		t.Errorf("payload subject did not win: %q", got)
	}
	cfg := map[string]string{"project_slug": "payment-worker"}
	if got := MergeSubject(map[string]any{"project_slug": "7"}, cfg, sig.String()); got != "payment-worker" {
		t.Errorf("[dedup.subjects] mapping did not apply: %q", got)
	}
	if got := MergeSubject(nil, nil, sig.String()); got != sig.Short {
		t.Errorf("fallback must be the source-scoped sig short: %q", got)
	}
	if got := MergeSubject(nil, nil, "not-a-sig"); got != "not-a-sig" {
		t.Errorf("unparsable sig must pass through: %q", got)
	}
}

// TestNormVersionMismatchNeverMerges: two norm versions are two signature spaces.
func TestNormVersionMismatchNeverMerges(t *testing.T) {
	v1 := types.NewSig(types.SrcPSI, types.SigAlgoSHA256, 1, types.SigDigest([]byte("a")))
	v2 := types.NewSig(types.SrcPSI, types.SigAlgoSHA256, 2, types.SigDigest([]byte("a")))
	if v1.String() == v2.String() {
		t.Fatalf("norm versions produced the same sig string: %s", v1.String())
	}
	if v1.Short != v2.Short {
		t.Fatalf("same digest must share the short form")
	}
	a, err := types.ParseSig(v1.String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := types.ParseSig(v2.String())
	if err != nil {
		t.Fatal(err)
	}
	if a.NormVersion == b.NormVersion {
		t.Fatalf("parsed norm versions collapsed")
	}
}

// TestPSIBucketBoundaries pins the norm_version 1 bucket function.
func TestPSIBucketBoundaries(t *testing.T) {
	cases := []struct {
		avg  float64
		want string
	}{
		{0, "b1"}, {9.99, "b1"}, {10.00, "b2"}, {24.99, "b2"},
		{25.00, "b3"}, {49.99, "b3"}, {50.00, "b4"}, {99.0, "b4"},
	}
	for _, c := range cases {
		if got := PSIBucket(c.avg); got != c.want {
			t.Errorf("PSIBucket(%v) = %s, want %s", c.avg, got, c.want)
		}
	}
}
