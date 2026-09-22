package issues

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trouble-agent/trouble/internal/types"
)

// Evidence is the body-rendering input of §3.9.3 (the `bundle` of the §2.2
// signature). Each field of the template has exactly one source here, so a body
// can never silently pick a second one.
//
// Addition recorded in the run report: SPEC-09 §2.2 names the parameter type
// (`Evidence`) but the pinning table of §3.9.3 sources fields from the group
// projection, the incident's recorded state path and the scrubbed evidence
// lines, which `types.Evidence` (the verification tuple) does not carry.
type Evidence struct {
	// Summary is `Group.Title`.
	Summary string
	// Source is the reporting source label (`sentinel:payment-worker`,
	// `sensor:<rule>`, `collector:<name>`); it is the "from …" of the summary
	// line and the `source` of the origin row.
	Source string
	// Group carries occurrences, first/last seen and the release range.
	Group types.Group
	// Ladder is the incident's recorded state path, oldest first.
	Ladder []string
	// Lines are the already-scrubbed evidence lines (stack, journal tail,
	// config diff), in body order.
	Lines []string
	// Redactions is Σ Record.Redactions for the bundle. Counts, never values.
	Redactions int
	// RangeEvents is the event count inside the release range; 0 falls back to
	// the group's event counter.
	RangeEvents int
	// Recurrences are the fold lines already folded into this issue, oldest
	// first; they are capped by the caller.
	Recurrences []string
}

// SigMarker is the first mandatory marker line (§3.9.3): it is what makes a sig
// findable in issue bodies by search, and a driver must never strip it.
func SigMarker(sig string) string { return "<!-- trouble:sig=" + sig + " -->" }

// IncMarker is the second mandatory marker line.
func IncMarker(inc, driver string, ver int) string {
	return fmt.Sprintf("<!-- trouble:inc=%s driver=%s ver=%d -->", inc, driver, ver)
}

// IdemMarker is the machine-readable idempotency marker every comment ends with
// (§3.1), so an uncertain-outcome read-back is exact.
func IdemMarker(idemKey string) string { return "<!-- trouble:idem=" + idemKey + " -->" }

// MarkerVersion is the marker generation written into the inc marker line.
const MarkerVersion = 1

// TitleOf renders the §3.9.3 title, truncated to title_max_chars on a rune
// boundary. The sig is never truncated when it fits: the prefix and summary are
// shortened first.
func (d *Desk) TitleOf(inc types.Incident, ev Evidence) string {
	max := d.cfg.TitleMaxChars
	if max <= 0 {
		max = 256
	}
	title := fmt.Sprintf("[trouble] %s: %s (%s)", inc.Severity, ev.Summary, inc.Sig)
	if utf8.RuneCountInString(title) <= max {
		return title
	}
	suffix := fmt.Sprintf(" (%s)", inc.Sig)
	head := fmt.Sprintf("[trouble] %s: ", inc.Severity)
	room := max - utf8.RuneCountInString(head) - utf8.RuneCountInString(suffix)
	if room < 0 {
		return truncateRunes(title, max)
	}
	return head + truncateRunes(ev.Summary, room) + suffix
}

// BodyOf renders the §3.9.3 body template byte-for-byte. The two marker lines are
// mandatory and are emitted first, so truncation can never remove them: an over
// budget body is truncated inside the evidence section with an elision marker.
func (d *Desk) BodyOf(inc types.Incident, ev Evidence, driver string) string {
	var b strings.Builder
	b.WriteString(SigMarker(inc.Sig))
	b.WriteString("\n")
	b.WriteString(IncMarker(inc.ID, driver, MarkerVersion))
	b.WriteString("\n\n")

	source := ev.Source
	if source == "" {
		source = string(types.SrcUnknown)
	}
	fmt.Fprintf(&b, "**Summary** — %s — %s, from `%s`\n\n", ev.Summary, inc.Severity, source)

	b.WriteString("| field | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| sig | `%s` |\n", inc.Sig)
	fmt.Fprintf(&b, "| incident | `%s` (group `%s`) |\n", inc.ID, inc.GroupID)
	fmt.Fprintf(&b, "| first seen | %s |\n", blankDash(ev.Group.FirstSeenTS))
	fmt.Fprintf(&b, "| last seen | %s |\n", blankDash(ev.Group.LastSeenTS))
	fmt.Fprintf(&b, "| occurrences | %d |\n", ev.Group.Count)
	fmt.Fprintf(&b, "| release range | %s |\n", releaseRange(ev))
	fmt.Fprintf(&b, "| origin | host `%s`, source `%s` |\n", d.hostID(), source)
	fmt.Fprintf(&b, "| ladder | %s |\n", ladderPath(inc, ev))
	fmt.Fprintf(&b, "| redactions applied | %d |\n\n", ev.Redactions)

	// §3.13a (AC-31): with a bundle the body gains one section between the
	// field table and the evidence bundle; the fence holds json.Marshal of the
	// persisted bundle, byte-verbatim. Without a bundle: zero other deltas —
	// the §3.9.3 golden stays byte-identical.
	if inc.Codeplane != nil {
		if cpBytes, err := json.Marshal(inc.Codeplane); err == nil {
			b.WriteString("**Codeplane bundle**\n\n```json\n")
			b.Write(cpBytes)
			b.WriteString("\n```\n")
		}
	}

	b.WriteString("\n**Evidence bundle (scrubbed)**\n\n```\n")
	b.WriteString(strings.Join(ev.Lines, "\n"))
	b.WriteString("\n```\n\n")

	if len(ev.Recurrences) > 0 {
		b.WriteString("**Recurrences**\n\n")
		for _, r := range ev.Recurrences {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	size := int64(len(b.String()))
	idem := idemShort(inc.Sig, ev.Group.LastSeenTS)
	fmt.Fprintf(&b, "<sub>filed by trouble %s (%s) at %s · driver %s · idem %s</sub>\n",
		d.version(), d.gitSHA(), blankDash(ev.Group.LastSeenTS), driver, idem)

	out := b.String()
	return d.truncateBody(out, size)
}

// truncateBody applies the body_max_bytes budget to the evidence section only
// (§3.9.3). The sig marker, the inc marker and the metadata table are intact.
// §3.13a adds one truncation step, markers always last: an over-budget body
// loses the codeplane fence before it loses a marker or evidence bytes.
func (d *Desk) truncateBody(body string, _ int64) string {
	max := d.cfg.BodyMaxBytes
	if max <= 0 || len(body) <= max {
		return body
	}
	if start := strings.Index(body, "**Codeplane bundle**\n"); start >= 0 {
		rest := body[start:]
		if end := strings.Index(rest, "\n```\n\n"); end >= 0 {
			trimmed := body[:start] + body[start+end+len("\n```\n\n"):]
			if len(trimmed) <= max {
				return trimmed
			}
			body = trimmed
		}
	}
	open := strings.Index(body, "```\n")
	if open < 0 {
		return body // nothing safe to cut
	}
	head := body[:open+4]
	rest := body[open+4:]
	closeAt := strings.Index(rest, "\n```")
	if closeAt < 0 {
		return body
	}
	tail := rest[closeAt:]
	elided := len(head) + len(tail) + 64
	if elided >= max {
		return head + "\n[truncated by trouble: no room for evidence]\n" + tail
	}
	keep := max - len(head) - len(tail) - 64
	if keep < 0 {
		keep = 0
	}
	mid := rest[:closeAt]
	if keep < len(mid) {
		mid = truncateRunes(mid, keep)
	}
	elided = len(rest[:closeAt]) - len(mid)
	return head + mid + fmt.Sprintf("\n[truncated by trouble: %d bytes elided]", elided) + tail
}

// FoldComment is the one-line fold comment of §3.2 (recurrence inside the dedup
// window).
func (d *Desk) FoldComment(inc types.Incident, ev Evidence) string {
	return fmt.Sprintf("recurrence %s · %s · occurrences %d",
		blankDash(ev.Group.LastSeenTS), inc.ID, ev.Group.Count)
}

// RecurrenceBlock is the full block of §3.2 (recurrence outside the window):
// counters and release range since the previous comment.
func (d *Desk) RecurrenceBlock(inc types.Incident, ev Evidence) string {
	var b strings.Builder
	b.WriteString(d.FoldComment(inc, ev))
	b.WriteString("\n")
	fmt.Fprintf(&b, "counters since the previous comment: events +%d, suppressed +%d, dropped +%d\n",
		ev.Group.Counters.Events, ev.Group.Counters.Suppressed, ev.Group.Counters.Dropped)
	fmt.Fprintf(&b, "release range: %s", releaseRange(ev))
	return b.String()
}

func releaseRange(ev Evidence) string {
	rr := ev.Group.ReleaseRange
	if len(rr) == 0 {
		return "unknown"
	}
	first := rr[0]
	last := rr[len(rr)-1]
	n := ev.RangeEvents
	if n == 0 {
		n = int(ev.Group.Counters.Events)
	}
	return fmt.Sprintf("`%s` → `%s` (%d events)", first, last, n)
}

func ladderPath(inc types.Incident, ev Evidence) string {
	if len(ev.Ladder) > 0 {
		return strings.Join(ev.Ladder, " → ")
	}
	if inc.State != "" {
		return string(inc.State)
	}
	return "—"
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func blankDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// idemShort is the 8-hex display token of the §3.9.3 footer.
func idemShort(sig, ts string) string {
	sum := types.SigDigest([]byte(sig + "|" + ts))
	return types.DigestShort(sum)[:8]
}

func (d *Desk) version() string {
	if d.deps.Actor.Version != "" {
		return d.deps.Actor.Version
	}
	return "0.0.0-dev"
}

func (d *Desk) gitSHA() string {
	if d.deps.Actor.GitSHA != "" {
		return d.deps.Actor.GitSHA
	}
	return "unknown"
}

// CloseReason renders the §3.11 quiet-close reason comment. It carries the idem
// marker so a retried close is recognized at the driver.
func CloseReason(inc types.Incident, ev types.Evidence, quiet types.Duration, lastSeq uint64, idemKey string) string {
	return fmt.Sprintf(`Closed by trouble: sig quiet for %s after resolution.
- incident: %s
- evidence: events_observed=%d canary_seen=%t window=%s sources_alive=%d/%d result=%s
- resolved: %s   quiet period: %s
- ledger: last seq %d   %s`,
		quiet, inc.ID, ev.EventsObserved, ev.CanarySeen, windowText(ev.WindowS),
		len(ev.SourcesAlive), len(ev.SourcesExpected), ev.Result,
		blankDash(inc.ResolvedTS), quiet, lastSeq, IdemMarker(idemKey))
}

func windowText(seconds float64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds * float64(time.Second))
	return d.String()
}
