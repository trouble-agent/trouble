package research

// corpus_test.go — SPEC-07 §7 `corpus_test.go`: the measured `found:false` trap,
// the acceptance rule, malformed files, a missing root, the wall cap and the
// per-file cap.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// verifiedAnswer is a corpus document holding one verified answer.
func verifiedAnswer(solution string) string {
	return `{"problem_class":"payment-worker-crash-loop","answers":[{"id":77,"problem_class":"payment-worker-crash-loop","status":"verified","solution":"` + solution + `","signatures":{"result":"passed"}}]}`
}

func TestFoundFalseCorpusHit(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.json"), []byte(verifiedAnswer("set MemoryMax=512M")), 0o600); err != nil {
		t.Fatal(err)
	}
	lab := newLabStub(t)
	lab.discoverFound = false // the measured trap: found:false is not absence
	s, deps := newTestService(t, lab, map[string]any{
		"lab_data_dir": dir, "corpus_roots": []any{"answers"},
	})
	// A deterministic corpus: the service's own fsCorpus is the code under test.
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResReturned || !out.CorpusGrepHit {
		t.Fatalf("outcome = %+v, want returned with corpus_grep_hit", out)
	}
	if out.Brief == nil || !strings.Contains(out.Brief["solution"].(string), "MemoryMax") {
		t.Fatalf("brief = %v", out.Brief)
	}
	if p := deps.lastPayload(types.KResearch); p["corpus_grep_hit"] != true {
		t.Fatalf("payload = %v", p)
	}
	if lab.count("/api/v1/problems/submit") != 0 {
		t.Fatalf("a corpus hit must not submit")
	}
}

func TestCorpusRejectsUnverified(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "pending.json"), []byte(`{"problem_class":"payment-worker-crash-loop","answers":[{"status":"pending","solution":"maybe"}]}`), 0o600)
	_ = os.WriteFile(filepath.Join(root, "failed.json"), []byte(`{"problem_class":"payment-worker-crash-loop","answers":[{"status":"verified","solution":"bad","signatures":{"result":"failed"}}]}`), 0o600)
	lab := newLabStub(t)
	lab.submit409 = true
	s, _ := newTestService(t, lab, map[string]any{"lab_data_dir": dir, "corpus_roots": []any{"answers"}})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.CorpusGrepHit || out.State == types.ResReturned {
		t.Fatalf("outcome = %+v, want a miss that reaches submit", out)
	}
	if lab.count("/api/v1/problems/submit") != 1 {
		t.Fatalf("submit count = %d, want 1", lab.count("/api/v1/problems/submit"))
	}
}

func TestCorpusMalformedFileCountedAndSkipped(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	_ = os.MkdirAll(root, 0o755)
	good := filepath.Join(root, "good.json")
	broken := filepath.Join(root, "broken.json")
	_ = os.WriteFile(good, []byte(verifiedAnswer("restart the unit with a higher limit")), 0o600)
	_ = os.WriteFile(broken, []byte(`{"problem_class":"payment-worker-crash-loop","answers":[{"status":"verified",`), 0o600)
	// The malformed file is the newest, so the newest-first walk parses it
	// first: the error is counted and the grep continues to the next file.
	now := time.Now()
	_ = os.Chtimes(good, now.Add(-time.Minute), now.Add(-time.Minute))
	_ = os.Chtimes(broken, now, now)

	lab := newLabStub(t)
	s, _ := newTestService(t, lab, map[string]any{"lab_data_dir": dir, "corpus_roots": []any{"answers"}})
	c := newFSCorpus(s.cfg, s.deps.Now, func(a time.Time) time.Duration { return s.deps.Now().Sub(a) })
	cr, err := c.Grep(context.Background(), "payment-worker-crash-loop", nil)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if cr.ParseErrors != 1 {
		t.Fatalf("parse errors = %d, want 1", cr.ParseErrors)
	}
	if len(cr.Hits) != 1 {
		t.Fatalf("hits = %d, want the good file", len(cr.Hits))
	}
	if cr.Scanned != 2 {
		t.Fatalf("scanned = %d, want 2 (one count per file)", cr.Scanned)
	}
	if cr.Hits[0].Primary != true {
		t.Fatalf("the hit must be primary (the class matched verbatim): %+v", cr.Hits[0])
	}
}

func TestCorpusMissingRootIsAGap(t *testing.T) {
	dir := t.TempDir()
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, map[string]any{
		"lab_data_dir": dir, "corpus_roots": []any{"does-not-exist"},
	})
	if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gaps := deps.payloads(types.KGap)
	if len(gaps) != 1 || gaps[0]["cause"] != "research_corpus_unreadable" {
		t.Fatalf("gaps = %v, want one research_corpus_unreadable", gaps)
	}
	// The grep continued: it still reached submit.
	if lab.count("/api/v1/problems/submit") != 1 {
		t.Fatalf("an unreadable root must not abort the rung")
	}
}

func TestCorpusPerFileCapAndGlob(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	_ = os.MkdirAll(root, 0o755)
	big := strings.Repeat("x", 4096)
	_ = os.WriteFile(filepath.Join(root, "big.json"), []byte(`{"answers":[{"status":"verified","solution":"`+big+`"}]}`), 0o600)
	_ = os.WriteFile(filepath.Join(root, "notes.txt"), []byte(verifiedAnswer("ignored by the glob")), 0o600)
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, map[string]any{
		"lab_data_dir": dir, "corpus_roots": []any{"answers"},
		"corpus_max_bytes_per_file": int64(64),
	})
	c := newFSCorpus(s.cfg, s.deps.Now, func(a time.Time) time.Duration { return s.deps.Now().Sub(a) })
	cr, err := c.Grep(context.Background(), "class", nil)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if cr.Scanned != 0 {
		t.Fatalf("scanned = %d: the oversized file must be skipped and the .txt filtered by the glob", cr.Scanned)
	}
}

func TestCorpusFileCap(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	_ = os.MkdirAll(root, 0o755)
	for i := 0; i < 12; i++ {
		_ = os.WriteFile(filepath.Join(root, "f"+string(rune('a'+i))+".json"), []byte(`{"answers":[]}`), 0o600)
	}
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, map[string]any{
		"lab_data_dir": dir, "corpus_roots": []any{"answers"}, "corpus_max_files": 5,
	})
	c := newFSCorpus(s.cfg, s.deps.Now, func(a time.Time) time.Duration { return s.deps.Now().Sub(a) })
	cr, err := c.Grep(context.Background(), "class", nil)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if cr.Scanned > 5 {
		t.Fatalf("scanned = %d, want ≤5 (corpus_max_files)", cr.Scanned)
	}
}

// TestCorpusTimeoutFlag: the wall cap sets corpus_timeout and treats the grep as
// a miss without a gap (§3.2 step 2).
func TestCorpusTimeoutFlag(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "answers")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "a.json"), []byte(`{"answers":[]}`), 0o600)
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, map[string]any{
		"lab_data_dir": dir, "corpus_roots": []any{"answers"}, "corpus_timeout": "1ns",
	})
	c := newFSCorpus(s.cfg, s.deps.Now, func(a time.Time) time.Duration { return time.Hour })
	cr, err := c.Grep(context.Background(), "class", nil)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if !cr.Timeout {
		t.Fatalf("corpus_timeout was not set when the wall cap fired")
	}
	if len(cr.Hits) != 0 {
		t.Fatalf("a timed-out grep is a miss")
	}
}

// TestCorpusTermsAreBounded pins the widening term set (§3.2 step 3).
func TestCorpusTermsAreBounded(t *testing.T) {
	terms := corpusTerms("payment-worker-crash-loop", "payment-worker-crash-loop",
		"crash_loop", "2ab4c6d8e0f1a3b5",
		"start request repeated too quickly for payment-worker", "worker.py:118 claim")
	if len(terms) > 8 {
		t.Fatalf("terms = %d, want a bounded list: %v", len(terms), terms)
	}
	var hasSlug, hasSig bool
	for _, x := range terms {
		if x == "payment-worker-crash-loop" {
			hasSlug = true
		}
		if x == "2ab4c6d8e0f1a3b5" {
			hasSig = true
		}
		if x == "the" || x == "and" {
			t.Fatalf("stopword leaked into the terms: %v", terms)
		}
	}
	if !hasSlug || !hasSig {
		t.Fatalf("terms must carry the slug verbatim and the sig short form: %v", terms)
	}
}
