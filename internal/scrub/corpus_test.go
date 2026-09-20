package scrub

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §7 corpus_test.go: a 10,000-line corpus of real-shaped journal lines
// that contains no rule-shaped span.
//
// The corpus (testdata/corpus/journal.txt) was verified independently with
// grep -E before it was committed; the verification is reproducible:
//
//	cd internal/scrub/testdata/corpus
//	grep -icE '(^|[^A-Za-z0-9])(PASSPHRASE|PASSWORD|PASSWD|PWD|SECRET|API[_-]?KEY|ACCESS[_-]?KEY|TOKEN|DSN)[[:space:]]*=' journal.txt   # 0
//	grep -icE '[a-z][a-z0-9+.-]{1,15}://[^:@/ ]{1,64}:[^@/ ]+@' journal.txt                                                              # 0
//	grep -icE 'eyJ[A-Za-z0-9_-]{8,}\.' journal.txt                                                                                       # 0
//	grep -icE '(AKIA|ASIA|ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|glpat-|xox[baprs]-|sk-|AIza|ya29\.|npm_|pypi-|dckr_pat_|SG\.|hf_|tvly-|shpat_)' journal.txt  # 0
//	grep -icE '([A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]{1,63}\.[A-Za-z]{2,24})' journal.txt                                                  # 0
//	grep -icE '(-----BEGIN|PuTTY-User-Key-File-)' journal.txt                                                                            # 0
//
// Every IPv4 in the corpus is loopback/RFC1918 or link-local (the exempt set of
// §3.3), and the only 40+ character run is a lower-case hex fingerprint, which
// the entropy rule rejects as pure hex.
// splitLines splits the corpus into lines, dropping the trailing newline.
func splitLines(s string) []string {
	out := make([]string, 0, 10000)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func TestCorpusMandatorySetIsSilent(t *testing.T) {
	data, err := readTestdata("corpus/journal.txt")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	lines := splitLines(data)
	if len(lines) != 10000 {
		t.Fatalf("corpus has %d lines, want 10000", len(lines))
	}
	// the mandatory set only: entropy off, pii kept, paths kept
	e := newTestEngine(t, "[scrub]\nentropy = false\npii_mode = \"keep\"\npath_mode = \"keep\"\n")
	total := 0
	for i, ln := range lines {
		out, res := scrubString(t, e, types.TgJournalTail, "1", ln)
		if res.Redactions != 0 {
			if total < 5 {
				t.Errorf("line %d: %d redactions (%s)\n  in  %q\n  out %q",
					i, res.Redactions, byRuleString(res.ByRule), ln, out)
			}
			total += res.Redactions
		}
	}
	if total != 0 {
		t.Errorf("the mandatory set touched %d spans across 10,000 clean lines", total)
	}
}

// TestCorpusEntropyBudget: with the full default set, the only rule that may
// touch a clean line is the one heuristic, and it must stay under 0.5% of lines.
func TestCorpusEntropyBudget(t *testing.T) {
	data, err := readTestdata("corpus/journal.txt")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	lines := splitLines(data)
	e := newTestEngine(t, "")
	touched := 0
	for _, ln := range lines {
		_, res := scrubString(t, e, types.TgJournalTail, "1", ln)
		if res.ByRule["entropy_token"] > 0 {
			touched++
		}
		if res.Redactions > 0 && res.ByRule["entropy_token"] == 0 {
			t.Errorf("a non-entropy rule touched a clean line: %s", byRuleString(res.ByRule))
		}
	}
	if pct := float64(touched) / float64(len(lines)) * 100; pct > 0.5 {
		t.Errorf("entropy_token touched %.2f%% of clean lines, the budget is 0.5%%", pct)
	}
}
