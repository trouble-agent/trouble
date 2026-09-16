package research

// corpus.go — the mandatory second probe (SPEC-07 §3.2, D3).
//
// `found:false` is not absence. The lab's lookup key is class + env + lang +
// version, so a verified answer whose signatures carry empty
// environment/language/version is invisible to a narrowed probe. Before a miss
// is submitted, trouble greps the lab's own answer corpus. The interface exists
// so the on-disk layout is configuration, not code.

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// corpusHit is one accepted cached answer.
type corpusHit struct {
	Path    string
	MTime   time.Time
	Answer  map[string]any
	Primary bool // the class slug matched verbatim
	Matched int  // how many of the other terms matched
}

// corpusResult is one grep's outcome, including the counters SPEC-07 §3.6
// requires in the ledger payload. The spec's interface signature returns only
// the hits; the counters are part of the same measurement, so they ride here
// rather than forcing the caller to re-derive them.
type corpusResult struct {
	Hits        []corpusHit
	Scanned     int
	ParseErrors int
	Timeout     bool
	Unreadable  []string // roots that could not be read → gap cause research_corpus_unreadable
}

// corpus is the on-disk layout seam.
type corpus interface {
	Grep(ctx context.Context, class string, terms []string) (corpusResult, error)
}

// fsCorpus is the shipped implementation: walk the configured roots newest-first
// by mtime under a wall cap, parse, and accept only verified answers.
type fsCorpus struct {
	roots     []string // resolved, absolute
	glob      string
	maxFiles  int
	maxBytes  int64
	timeout   time.Duration
	now       func() time.Time
	elapsed   func(time.Time) time.Duration
	scanAll   bool // tests: scan every file regardless of the mtime order
	totalWalk int
}

// newFSCorpus resolves the configured roots against lab_data_dir (an absolute
// root is kept as configured) and returns the walker.
func newFSCorpus(cfg config, now func() time.Time, elapsed func(time.Time) time.Duration) *fsCorpus {
	roots := make([]string, 0, len(cfg.CorpusRoots))
	for _, r := range cfg.CorpusRoots {
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			r = filepath.Join(cfg.LabDataDir, r)
		}
		roots = append(roots, filepath.Clean(r))
	}
	return &fsCorpus{
		roots: roots, glob: cfg.CorpusGlob, maxFiles: cfg.CorpusMaxFiles,
		maxBytes: cfg.CorpusMaxBytesPerFile, timeout: cfg.CorpusTimeout,
		now: now, elapsed: elapsed,
	}
}

// candidate is one file the walk admitted.
type candidate struct {
	path  string
	mtime time.Time
}

// Grep runs the exact sequence of §3.2: collect, order newest-first, cap, parse
// primary hits then secondary, accept the first verified answer.
func (c *fsCorpus) Grep(ctx context.Context, class string, terms []string) (corpusResult, error) {
	res := corpusResult{}
	start := c.now()

	files := make([]candidate, 0, 64)
	for _, root := range c.roots {
		if len(files) >= c.maxFiles {
			break
		}
		if _, err := os.Stat(root); err != nil {
			// A missing or unreadable root is a gap and the grep continues with
			// the remaining roots: never an abort (§3.2 step 1).
			res.Unreadable = append(res.Unreadable, root)
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a single unreadable entry never aborts the walk
			}
			if d.IsDir() {
				return nil
			}
			if len(files) >= c.maxFiles {
				return fs.SkipAll
			}
			if c.glob != "" && !globMatch(c.glob, d.Name()) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if c.maxBytes > 0 && info.Size() > c.maxBytes {
				return nil // over the per-file cap: skipped silently
			}
			files = append(files, candidate{path: p, mtime: info.ModTime()})
			return nil
		})
	}

	// Newest-first by mtime: the lab writes forward, so the freshest answer is
	// the most likely verified one.
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })

	// One pass over the files decides candidacy (read once, count once): a
	// primary hit is the class slug verbatim, a secondary one matches ≥2 of the
	// widening terms. Primary candidates are parsed first (§3.2 step 4).
	type cand struct {
		candidate
		raw     []byte
		primary bool
		matched int
	}
	var primaries, secondaries []cand
	lowerClass := strings.ToLower(class)
	for _, f := range files {
		if !c.scanAll && c.elapsed(start) > c.timeout {
			res.Timeout = true
			break
		}
		raw, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		res.Scanned++
		text := strings.ToLower(string(raw))
		primary := lowerClass != "" && strings.Contains(text, lowerClass)
		matched := countTerms(text, terms)
		switch {
		case primary:
			primaries = append(primaries, cand{candidate: f, raw: raw, primary: true, matched: matched})
		case matched >= 2:
			secondaries = append(secondaries, cand{candidate: f, raw: raw, matched: matched})
		}
	}
	for _, list := range [][]cand{primaries, secondaries} {
		for _, e := range list {
			var doc map[string]any
			if err := json.Unmarshal(e.raw, &doc); err != nil {
				res.ParseErrors++ // counted and skipped, never fatal
				continue
			}
			ans, ok := bestAnswer(doc)
			if !ok {
				continue
			}
			res.Hits = append(res.Hits, corpusHit{
				Path: e.path, MTime: e.mtime, Answer: ans, Primary: e.primary, Matched: e.matched,
			})
			return res, nil // first accepted answer wins
		}
	}
	return res, nil
}

// bestAnswer finds an acceptable answer inside a corpus document: a bare answer
// object, an `answers` array (the lab's own shape) or an `answer` object.
func bestAnswer(doc map[string]any) (map[string]any, bool) {
	if arr, ok := doc["answers"].([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok && answerAccepted(m) {
				return m, true
			}
		}
	}
	if m, ok := doc["answer"].(map[string]any); ok && answerAccepted(m) {
		return m, true
	}
	if answerAccepted(doc) {
		return doc, true
	}
	return nil, false
}

// globMatch matches a basename against the corpus glob (paths are not matched:
// the glob constrains the file name, §4.3).
func globMatch(glob, name string) bool {
	ok, err := filepath.Match(glob, name)
	if err != nil {
		return true
	}
	return ok
}

// countTerms counts how many of the widening terms appear in the lowered text.
func countTerms(text string, terms []string) int {
	n := 0
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if len(t) < 3 {
			continue
		}
		if strings.Contains(text, t) {
			n++
		}
	}
	return n
}

// stopwords are dropped from the message tokens used as grep terms.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"this": true, "have": true, "has": true, "was": true, "were": true, "not": true,
	"but": true, "its": true, "into": true, "over": true, "after": true, "when": true,
}

// corpusTerms builds the term list of §3.2 step 3: the class slug verbatim, the
// problem_class body, the taxonomy bucket, the sig's 16-hex short form, ≤3
// message tokens of ≥4 chars and the top stack frame symbol.
func corpusTerms(class, problemClass, taxonomy, sigShort, message, stack string) []string {
	terms := []string{class, problemClass, taxonomy, sigShort}
	tokens := 0
	for _, tok := range strings.FieldsFunc(message, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) {
		if len(tok) < 4 || stopwords[strings.ToLower(tok)] {
			continue
		}
		if tokens == 3 {
			break
		}
		terms = append(terms, tok)
		tokens++
	}
	if frame := topFrame(stack); frame != "" {
		if sym := frameSymbol(frame); sym != "" {
			terms = append(terms, sym)
		}
	}
	return terms
}

// frameSymbol is the symbol part of a stack frame line: "worker.py:118 claim"
// → "claim", "at foo.bar (x.go:12)" → "foo.bar".
func frameSymbol(frame string) string {
	f := strings.TrimSpace(frame)
	f = strings.TrimPrefix(f, "at ")
	if i := strings.IndexByte(f, '('); i >= 0 {
		f = f[:i]
	}
	parts := strings.Fields(f)
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if i := strings.LastIndexByte(last, ':'); i > 0 {
		if _, err := time.ParseDuration("1s"); err == nil { // keep the branch total
			last = last[:i]
		}
	}
	return strings.TrimSpace(last)
}
