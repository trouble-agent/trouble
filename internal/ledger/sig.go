package ledger

import (
	"regexp"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// MergeDomain is the pinned merge-key domain separator (SPEC-01 §3.3).
const MergeDomain = "trouble.merge.v1"

// MessageNormMaxLines and MessageNormBudgetBytes are the norm_version 1 pins.
const (
	MessageNormMaxLines    = 3
	MessageNormBudgetBytes = 512
	NormVersion            = types.NormVersionV1
)

var (
	hexRe     = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	uuidRe    = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	longHexRe = regexp.MustCompile(`[0-9a-fA-F]{8,}`)
	digitsRe  = regexp.MustCompile(`[0-9]+`)
	spaceRe   = regexp.MustCompile(`\s+`)
)

// MessageNorm applies the norm_version 1 message normalization (a→e, ordered,
// re-entrant) and truncates to 512 bytes at a rune boundary.
func MessageNorm(msg string) string {
	lines := strings.Split(msg, "\n")
	kept := make([]string, 0, MessageNormMaxLines)
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		kept = append(kept, ln)
		if len(kept) == MessageNormMaxLines {
			break
		}
	}
	for i, ln := range kept {
		ln = hexRe.ReplaceAllString(ln, "<hex>")
		ln = uuidRe.ReplaceAllString(ln, "<uuid>")
		ln = longHexRe.ReplaceAllString(ln, "<h>")
		ln = digitsRe.ReplaceAllString(ln, "#")
		ln = strings.TrimSpace(spaceRe.ReplaceAllString(ln, " "))
		kept[i] = ln
	}
	out := strings.Join(kept, "\n")
	return truncateRunes(out, MessageNormBudgetBytes)
}

func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// SigFor computes the v1 signature of a source's normalized field list.
func SigFor(source types.SigSource, fields ...string) types.Sig {
	return types.NewSigFromFields(source, fields...)
}

// MergeKey is the cross-source merge key (SPEC-01 §3.3):
// hex16(sha256("trouble.merge.v1\x1f" + class + "\x1f" + subject + "\x1e")).
func MergeKey(class, subject string) string {
	raw := MergeDomain + "\x1f" + cleanMergePart(class) + "\x1f" + cleanMergePart(subject) + "\x1e"
	return types.DigestShort(types.SigDigest([]byte(raw)))
}

func cleanMergePart(s string) string {
	s = strings.ReplaceAll(s, "\x00", " ")
	s = strings.ReplaceAll(s, "\x1f", " ")
	s = strings.ReplaceAll(s, "\x1e", " ")
	return strings.TrimSpace(s)
}

// ClassFor is the per-source `class` used by the merge key (SPEC-01 §3.3).
// state is the dbus SubState / job Result, or "" for sources without one.
func ClassFor(source types.SigSource, state string) string {
	switch source {
	case types.SrcPSI:
		return "resource_exhaustion"
	case types.SrcDisk:
		switch state {
		case "free_pct", "inode_pct":
			return "disk_full"
		case "io_error":
			return "io_stall"
		case "readonly":
			return "config_error"
		}
		return "disk_full"
	case types.SrcTimers:
		return "timer_missed"
	case types.SrcInotify:
		return "file_flap"
	case types.SrcDBus:
		switch state {
		case "failed", "auto-restart":
			return "crash_loop"
		}
		return "unit_degraded"
	case types.SrcCollector, types.SrcSentinel:
		return "crash_loop"
	case types.SrcJournald:
		return "crash_loop"
	}
	return "unknown"
}

// MergeSubject resolves the merge-key subject in the pinned order (first hit
// wins, never blocks): payload.subject → the [dedup.subjects] table → the
// source-scoped sig short (SPEC-01 §3.3).
func MergeSubject(payload map[string]any, cfg map[string]string, sig string) string {
	if payload != nil {
		if v, ok := payload["subject"].(string); ok && v != "" {
			return v
		}
	}
	if payload != nil && cfg != nil {
		for pattern, subject := range cfg {
			if v, ok := payload[pattern].(string); ok && v != "" {
				if subject != "" {
					return subject
				}
				return v
			}
		}
	}
	short := sig
	if s, err := types.ParseSig(sig); err == nil {
		short = s.Short
	}
	return short
}

// ---- published test vectors (SPEC-01 §3.3) ----

// SigVector is one row of the published sig vector table. Raw is the exact
// hashed byte string; Fields is the equivalent field list whose framing must
// reproduce Raw byte-for-byte.
type SigVector struct {
	Source types.SigSource
	Fields []string
	Raw    string
	Digest string
	Short  string
}

// SigVectors returns every published sig vector, in spec order.
func SigVectors() []SigVector {
	return []SigVector{
		{types.SrcPSI, []string{"cpu", "some", "b3"}, "psi\x1fcpu\x1fsome\x1fb3\x1e",
			"d8d1d8cc6f8c431d1b99197c3a52134a0a7d037e8ab9aa3bce221bbf5ddc7296", "d8d1d8cc6f8c431d"},
		{types.SrcJournald, []string{"payment-worker", "queue wedge: pool exhausted, retry # in # ms"},
			"journald\x1fpayment-worker\x1fqueue wedge: pool exhausted, retry # in # ms\x1e",
			"8721b2ea46c6cf26b09fe04f73f41b05fe3660e5c21ddefdbae6686708a13e4a", "8721b2ea46c6cf26"},
		{types.SrcDBus, []string{"user:1000", "payment-worker.service", "auto-restart"},
			"dbus\x1fuser:1000\x1fpayment-worker.service\x1fauto-restart\x1e",
			"2b180920160f1ea8a68a3cd63856212c1e78d01abc9e18572b5d0be76f7cea3e", "2b180920160f1ea8"},
		{types.SrcDisk, []string{"/", "free_pct"}, "disk\x1f/\x1ffree_pct\x1e",
			"ca5ae505ad8384448d929b5e4a8d7414f8d77db1973115723736c5ce25138936", "ca5ae505ad838444"},
		{types.SrcTimers, []string{"backup.timer", "missed"}, "timers\x1fbackup.timer\x1fmissed\x1e",
			"e529b7377e31655e90f5987023d0d8bb626616b41605939cd6b2e6429877dcc5", "e529b7377e31655e"},
		{types.SrcInotify, []string{"/etc/payment/config.toml", "modify"},
			"inotify\x1f/etc/payment/config.toml\x1fmodify\x1e",
			"7ebd19985bf29592ac9e990c0703028cb3ad0fa4f32a983cc53fb47c62939c2f", "7ebd19985bf29592"},
		{types.SrcSentinel, []string{"1", "worker.claim", "queue-wedge"}, "sentinel\x1f1\x1fworker.claim\x1fqueue-wedge\x1e",
			"18b12eea8a3929236822338d1b8d04055a2a670104b97782356e1559fc83f385", "18b12eea8a392923"},
		{types.SrcSentinel, []string{"1", "worker.claim", "b7e2d91c4a3f0e58"}, "sentinel\x1f1\x1fworker.claim\x1fb7e2d91c4a3f0e58\x1e",
			"3cd6cf21bc4259ea71157d42b218c8505134432d7384436a35ca0c907af22eff", "3cd6cf21bc4259ea"},
		{types.SrcCollector, []string{"go-panic", "panic", "panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=<hex> addr=<hex> pc=<hex>]"},
			"collector\x1fgo-panic\x1fpanic\x1fpanic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=<hex> addr=<hex> pc=<hex>]\x1e",
			"bce03e7abb085b0f963ca3b6fc4348112ebe7d93c316c4f7abd255dee97f498e", "bce03e7abb085b0f"},
		{types.SrcGeneric, []string{"1", "legacy-cron", "connection refused to db host #.#.#.#:#"},
			"generic\x1f1\x1flegacy-cron\x1fconnection refused to db host #.#.#.#:#\x1e",
			"f464cc58cfa080a66693c8cf2ae5898d76a74a1d9fbc65f62f685ba67fca616b", "f464cc58cfa080a6"},
		{types.SrcUnknown, []string{"17d04f75658547e6"}, "unknown\x1f17d04f75658547e6\x1e",
			"b52b9311988a6ef45221311ebebc52a0ed24016b9089bfeab674baae7a084b2e", "b52b9311988a6ef4"},
	}
}

// MergeVector is one row of the published merge-key vector table.
type MergeVector struct {
	Class   string
	Subject string
	Digest  string
	Short   string
}

// MergeVectors returns every published merge-key vector, in spec order.
func MergeVectors() []MergeVector {
	return []MergeVector{
		{"crash_loop", "payment-worker.service",
			"bcc48494f2190f4f5a4408fe3358445cd887856d386d72fec70ef73039f196f5", "bcc48494f2190f4f"},
		{"resource_exhaustion", "io",
			"a11a6c52eab58b3ae2fc3d2518da3c0ba990dd1352338e4502b7a58430751576", "a11a6c52eab58b3a"},
		{"disk_full", "/",
			"8b3c2119f8410f15eacfff1f4b4ef775ac69a91e896c3a183518b46e1a0a1980", "8b3c2119f8410f15"},
	}
}

// SourceFields documents the norm_version 1 field list per source (SPEC-01 §3.3).
var SourceFields = map[types.SigSource][]string{
	types.SrcSentinel:  {"project_slug", "culprit_norm", "fingerprint_or_stackhash"},
	types.SrcJournald:  {"unit_or_ident", "message_norm"},
	types.SrcDBus:      {"manager", "unit", "state"},
	types.SrcPSI:       {"scope", "signal", "bucket"},
	types.SrcDisk:      {"mount", "kind"},
	types.SrcTimers:    {"timer_unit", "kind"},
	types.SrcInotify:   {"path_norm", "class"},
	types.SrcCollector: {"collector_name", "parser_class", "message_norm"},
	types.SrcGeneric:   {"project_slug", "app_slug", "message_norm"},
	types.SrcUnknown:   {"raw_hash16"},
}

// PSIBucket is the norm_version 1 bucket function, evaluated on the sampled
// avg10 (sampling is the source of truth; triggers are only an accelerator).
func PSIBucket(avg10 float64) string {
	switch {
	case avg10 < 10.00:
		return "b1"
	case avg10 < 25.00:
		return "b2"
	case avg10 < 50.00:
		return "b3"
	default:
		return "b4"
	}
}
