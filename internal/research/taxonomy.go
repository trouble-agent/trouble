package research

// taxonomy.go — the built-in classification rules of SPEC-07 §3.1.
//
// The taxonomy is an ordered, first-match-wins rule list over the event message,
// the sig and the top stack frame. It buckets an incident into one of the ten
// shipped vocabulary terms; anything unmatched is `error`. A table file may
// replace or extend the list (table.go), and the bucket is one third of the
// class-slug key the Off-by-One lab is keyed on.

import (
	"regexp"
	"strings"
)

// Taxonomy buckets (SPEC-07 §3.1). The set is closed: a table entry may add a
// slug or remap a bucket, but nothing outside this list is ever produced, so a
// downstream consumer can switch on it exhaustively.
const (
	TaxError              = "error"
	TaxResourceExhaustion = "resource_exhaustion"
	TaxCrashLoop          = "crash_loop"
	TaxPermissionDenied   = "permission_denied"
	TaxNetworkTimeout     = "network_timeout"
	TaxConfigError        = "config_error"
	TaxQueueWedge         = "queue_wedge"
	TaxDataCorruption     = "data_corruption"
	TaxDependencyFailure  = "dependency_failure"
)

// TaxonomyBuckets lists every bucket in the order the default rules produce them.
var TaxonomyBuckets = []string{
	TaxResourceExhaustion, TaxCrashLoop, TaxPermissionDenied, TaxNetworkTimeout,
	TaxConfigError, TaxQueueWedge, TaxDataCorruption, TaxDependencyFailure, TaxError,
}

// taxonomyRule is one ordered rule: a compiled alternation and its bucket.
type taxonomyRule struct {
	Bucket string
	Re     *regexp.Regexp
}

// builtinTaxonomy is the compiled default rule list. It is built once: a regex
// compiled per derivation would put regexp compilation on every rung's path
// (measured 83µs/call before this was hoisted; the §7 gate is a per-call bound).
var builtinTaxonomy = defaultTaxonomy()

// defaultTaxonomy is the SPEC-07 §3.1 rule list, in order. First match wins; the
// memory rule precedes the crash rule because an OOM-killed unit also logs a
// non-zero exit status and the cgroup exhaustion is the useful diagnosis.
//
// Two implementation notes, both recorded in the run report:
//   - the short tokens carry word boundaries ("\boom\b", not "oom"), because the
//     spec's literal alternation matches "boom", "room" and "zoom" and files them
//     as resource exhaustion;
//   - `pressure (full|some)` and `unhandled (exception|rejection)` are added so
//     trouble's own PSI events (SPEC-03 §3.2's pressure readings) and the
//     sentinel/collector exception messages reach the built-in entries SPEC-07
//     §3.1 ships for those sources; without them those two entries are
//     unreachable for the very sources they name.
func defaultTaxonomy() []taxonomyRule {
	return []taxonomyRule{
		{TaxResourceExhaustion, regexp.MustCompile(`(?i)\boom\b|out of memory|cannot allocate memory|memory cgroup|pressure (full|some)|no space left|disk full|quota exceeded|\binode\b`)},
		{TaxCrashLoop, regexp.MustCompile(`(?i)start request repeated too quickly|start-limit-hit|restart(ed|ing)? (loop|too quickly)|panic:|fatal:|segmentation fault|exit status (1|2|13[0-9])|unhandled (exception|rejection)|\w*Error:`)},
		{TaxPermissionDenied, regexp.MustCompile(`(?i)permission denied|eacces|eperm|auth_admin|polkit`)},
		{TaxNetworkTimeout, regexp.MustCompile(`(?i)connection refused|timed out|timeout|etimedout|dial tcp|no route to host`)},
		{TaxConfigError, regexp.MustCompile(`(?i)no such file|unknown field|invalid (config|yaml|json)|parse error|unmarshal`)},
		{TaxQueueWedge, regexp.MustCompile(`(?i)deadlock|queue (wedge|full)|pool exhausted|backpressure|blocked on`)},
		{TaxDataCorruption, regexp.MustCompile(`(?i)corrupt|checksum mismatch|bad magic|torn|truncated`)},
		{TaxDependencyFailure, regexp.MustCompile(`(?i)module not found|cannot find module|importerror|dependency`)},
	}
}

// classify returns the taxonomy bucket for a subject's text. The text is the
// event message, the sig and the top stack frame concatenated (SPEC-07 §3.1);
// an empty text classifies as TaxError.
func classify(rules []taxonomyRule, text string) string {
	for _, r := range rules {
		if r.Re.MatchString(text) {
			return r.Bucket
		}
	}
	return TaxError
}

// classifyFacts is the convenience wrapper the derivation uses: the message plus
// the top stack frame plus the raw sig, which is what the incident carries.
func classifyFacts(rules []taxonomyRule, f subjectFacts) string {
	var b strings.Builder
	b.WriteString(f.Message)
	if f.Stack != "" {
		b.WriteString("\n")
		b.WriteString(topFrame(f.Stack))
	}
	if f.Sig != "" {
		b.WriteString("\n")
		b.WriteString(f.Sig)
	}
	return classify(rules, b.String())
}

// topFrame returns the first non-empty, trimmed line of a stack: the canonical
// "top stack frame" the taxonomy rule list matches over.
func topFrame(stack string) string {
	for _, line := range strings.Split(stack, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
