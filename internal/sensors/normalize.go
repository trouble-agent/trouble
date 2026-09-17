package sensors

import (
	"regexp"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// This file is the sensors-side implementation of the norm_version 1 signature
// recipes. SPEC-03 does not import internal/ledger (SPEC-INDEX §4.1 keeps the
// dependency graph acyclic), so the framing and the per-source field lists are
// implemented here and cross-checked against SPEC-01 §3.3's published vectors
// in normalize_test.go — a byte-for-byte interop proof rather than a comment.
//
// FIELD-LIST RESOLUTION (spec conflict, recorded in the card metadata):
// SPEC-03 §3.1 publishes per-source "normalized fields" that contradict
// SPEC-01 §3.3's norm_version 1 table for psi (scope,metric,band,rule vs
// scope,signal,bucket), dbus (unit_key,failure_class,crash_loop vs
// manager,unit,state) and journald (unit/comm,priority,masked_message vs
// unit_or_ident,message_norm). SPEC-01 §3.3 ships digests that CI asserts
// byte-for-byte and internal/ledger already implements, and the dedup core is
// SPEC-01's, so this package produces SPEC-01 §3.3's fields. SPEC-03 §3.1's
// table remains the authoring source for the *scope vocabulary* and the
// SensorEvent.Detail vocabulary (which is what rules read, per §3.1).

// MessageNormMaxLines and MessageNormBudgetBytes are the norm_version 1 pins
// (SPEC-01 §3.3).
const (
	MessageNormMaxLines    = 3
	MessageNormBudgetBytes = 512
)

// MergeDomain is the pinned merge-key domain separator (SPEC-01 §3.3).
const MergeDomain = "trouble.merge.v1"

var (
	reHex     = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	reUUID    = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reLongHex = regexp.MustCompile(`[0-9a-fA-F]{8,}`)
	reDigits  = regexp.MustCompile(`[0-9]+`)
	reSpace   = regexp.MustCompile(`\s+`)

	// SPEC-03 §3.1 mask set, applied to a journald MESSAGE before hashing.
	reRFC3339 = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reDigit6  = regexp.MustCompile(`\b\d{6,}\b`)
	rePID     = regexp.MustCompile(`(?i)\b(?:pid|tid|thread)[ =:]+(\d+)\b`)
	rePort    = regexp.MustCompile(`(?i)\b(?:port|listen)[ =:]+(\d+)\b`)
	reHome    = regexp.MustCompile(`/home/[A-Za-z0-9_.-]+`)
	rePath    = regexp.MustCompile(`/[A-Za-z0-9._/-]+`)
)

// messageNorm applies SPEC-01 §3.3's norm_version 1 steps a→e, ordered and
// re-entrant, then truncates to the 512-byte budget at a rune boundary.
func messageNorm(msg string) string {
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
		ln = reHex.ReplaceAllString(ln, "<hex>")
		ln = reUUID.ReplaceAllString(ln, "<uuid>")
		ln = reLongHex.ReplaceAllString(ln, "<h>")
		ln = reDigits.ReplaceAllString(ln, "#")
		ln = strings.TrimSpace(reSpace.ReplaceAllString(ln, " "))
		kept[i] = ln
	}
	return truncateRunes(strings.Join(kept, "\n"), MessageNormBudgetBytes)
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

// maskMessage applies SPEC-03 §3.1's mask set to a journald message before it
// is hashed into a sig. The masks are deliberately order-dependent: the
// RFC3339 timestamp goes first, then long digit runs, then PIDs/ports (whose
// digits have already been collapsed only if they were ≥6 long), then
// `/home/<user>` prefixes, then remaining absolute paths.
//
// This is the *display-safe* projection recorded in SensorEvent.Detail; the
// hashed sig uses messageNorm on the already-scrubbed text (SPEC-01 §3.3), and
// maskedField is applied on top of it for the `masked_message` detail value.
func maskMessage(msg string) string {
	s := reRFC3339.ReplaceAllString(msg, "<ts>")
	s = reDigit6.ReplaceAllString(s, "<n>")
	s = rePID.ReplaceAllString(s, "pid=<n>")
	s = rePort.ReplaceAllString(s, "port=<n>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reLongHex.ReplaceAllString(s, "<h>")
	// Basename-resolve absolute paths FIRST: keep the last element only, so a
	// path with a random temp component is one signature, not thousands. Doing
	// this before the home-prefix pass is what keeps `/home/<user>/x.log` from
	// being spliced around its own mask.
	s = rePath.ReplaceAllStringFunc(s, func(p string) string {
		if p == "/" {
			return p
		}
		parts := strings.Split(strings.TrimRight(p, "/"), "/")
		last := parts[len(parts)-1]
		if last == "" || strings.Contains(last, "<") {
			return last
		}
		return last
	})
	s = reHome.ReplaceAllString(s, "/home/<user>")
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

// psiBucket is the norm_version 1 bucket function, evaluated on the *sampled*
// avg10: sampling is the source of truth, triggers are only an accelerator
// (SPEC-01 §3.3, SPEC-03 §3.2 P3).
func psiBucket(avg10 float64) string {
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

// classFor is the per-source `class` used by the merge key (SPEC-01 §3.3).
// state is the dbus SubState / job Result, or the disk/inotify/timer kind.
func classFor(source types.SigSource, state string) string {
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
	case types.SrcCollector, types.SrcSentinel, types.SrcJournald:
		return "crash_loop"
	}
	return "unknown"
}

// mergeKey is hex16(sha256(domain ␟ class ␟ subject ␞)) (SPEC-01 §3.3).
func mergeKey(class, subject string) string {
	raw := MergeDomain + "\x1f" + cleanMergePart(class) + "\x1f" + cleanMergePart(subject) + "\x1e"
	return types.DigestShort(types.SigDigest([]byte(raw)))
}

func cleanMergePart(s string) string {
	s = strings.ReplaceAll(s, "\x00", " ")
	s = strings.ReplaceAll(s, "\x1f", " ")
	s = strings.ReplaceAll(s, "\x1e", " ")
	return strings.TrimSpace(s)
}

// sigFor computes a source's v1 signature over its normalized field list.
func sigFor(source types.SigSource, fields ...string) types.Sig {
	return types.NewSigFromFields(source, fields...)
}
