package sentinel

import (
	"regexp"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// canonicalHeader is the first line of every canonical byte string (§3.3). It
// carries the framing marker so a normalization change can never masquerade as
// the previous signature space.
const canonicalHeader = "sentinel/sha256v1"

// defaultMarker is the SDK's in-place default-frame placeholder.
const defaultMarker = "{{ default }}"

// unitSep is 0x1F, the pinned separator inside the fingerprint form.
const unitSep = "\x1f"

// maxFrameLines is the §3.3 frame collapse point: frames beyond the 8th become
// `frames_over=+N`.
const maxFrameLines = 8

// maxNormLine caps a normalized line at 512 bytes (masking rule 15).
const maxNormLine = 512

// The masking rule set (`norm_version = 1`), in the pinned order of §3.3.
var (
	reUUID       = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reTimestamp  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reFileLine   = regexp.MustCompile(`[\w./\-]+\.(?:go|py|js|ts|java|rb|rs|php|cs|c|cpp|h):\d+(?::\d+)?`)
	reAddr       = regexp.MustCompile(`0x[0-9a-fA-F]{4,}`)
	reGoroutine  = regexp.MustCompile(`goroutine \d+`)
	rePID        = regexp.MustCompile(`(?i)\bpid[=: ]\s*\d+`)
	reHexID      = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)
	reDuration   = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)\b`)
	reNumber     = regexp.MustCompile(`\b\d{3,}\b`)
	reTmpPath    = regexp.MustCompile(`/(?:tmp|var/tmp)/\S*\d+`)
	rePort       = regexp.MustCompile(`:(?:6[0-9]{4}|[1-9][0-9]{3,4})\b`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// maskLine applies masking rules 3..15 to one line.
func maskLine(line string) string {
	line = reUUID.ReplaceAllString(line, "UUID")
	line = reTimestamp.ReplaceAllString(line, "TS")
	line = reFileLine.ReplaceAllStringFunc(line, func(m string) string {
		colon := strings.Index(m, ":")
		if colon < 0 {
			return m
		}
		return baseName(m[:colon]) + ":LINE"
	})
	// Ports are masked immediately after file:LINE (documented deviation #3 in
	// docs/sentinel-compat.md §5): in the table's literal order the generic
	// `\b\d{3,}\b` rule shadows this one for every port, which cannot be the
	// intent of §3.3's "ports are masked after file:LINE" note.
	line = rePort.ReplaceAllString(line, ":PORT")
	line = reAddr.ReplaceAllString(line, "0xADDR")
	line = reGoroutine.ReplaceAllString(line, "goroutine N")
	line = rePID.ReplaceAllString(line, "pid=PID")
	line = reHexID.ReplaceAllString(line, "HEXID")
	line = reDuration.ReplaceAllString(line, "DUR")
	line = reNumber.ReplaceAllString(line, "N")
	line = reTmpPath.ReplaceAllString(line, "/TMP")
	// Rule 14: collapse whitespace runs (tabs and spaces alike) to one space.
	line = reWhitespace.ReplaceAllString(line, " ")
	line = strings.TrimRight(line, " \t\r\f\v")
	if len(line) > maxNormLine {
		line = line[:maxNormLine]
	}
	return line
}

// maskText applies the full mask to a possibly multi-line value: UTF-8 lossy
// replacement (1), CRLF→LF and per-line trimming (2), then rules 3..15 per line.
func maskText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		lines[i] = maskLine(l)
	}
	out := strings.Join(lines, "\n")
	return strings.TrimSpace(out)
}

// maskContext masks and normalizes a stack-frame context line: trimmed, run
// length collapsed to one line, whitespace runs collapsed to one space.
func maskContext(s string) string {
	if s == "" {
		return ""
	}
	// A context line is one line by definition; take the first.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = maskText(s)
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// baseName returns the last path element (masking rule 5's "file stem").
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// moduleOf renders a frame's module field: the SDK value when present, else the
// file stem (documented rule, §3.3's `module` column; the golden vectors pin it).
func moduleOf(f frame, maskedFile string) string {
	if f.Module != "" {
		return maskText(f.Module)
	}
	stem := baseName(maskedFile)
	if i := strings.LastIndexByte(stem, '.'); i > 0 {
		stem = stem[:i]
	}
	return stem
}

// frameNorm renders one frame as `in_app|module|function|file_base|LINE|context`.
func frameNorm(f frame) string {
	file := maskText(f.File)
	if i := strings.Index(file, ":LINE"); i >= 0 {
		file = file[:i]
	}
	fileBase := baseName(file)
	inApp := ""
	if f.InApp {
		inApp = "app"
	}
	return strings.Join([]string{
		inApp,
		moduleOf(f, fileBase),
		maskText(f.Function),
		fileBase,
		"LINE",
		maskContext(f.ContextLine),
	}, "|")
}

// normalizer computes canonical bytes and sigs for events.
type normalizer struct {
	normVersion int
}

func newNormalizer() *normalizer { return &normalizer{normVersion: types.NormVersionV1} }

// frameLines renders up to 8 frame lines plus the `frames_over` collapse.
func (n *normalizer) frameLines(ev *rawEvent) ([]string, int) {
	lines := make([]string, 0, len(ev.Frames))
	for _, f := range ev.Frames {
		lines = append(lines, frameNorm(f))
	}
	over := 0
	if len(lines) > maxFrameLines {
		over = len(lines) - maxFrameLines
		lines = lines[:maxFrameLines]
	}
	return lines, over
}

// canonicalDefault builds the canonical-stack-hash byte string.
func (n *normalizer) canonicalDefault(ev *rawEvent) string {
	var b strings.Builder
	b.WriteString(canonicalHeader)
	b.WriteByte('\n')
	b.WriteString("level=" + maskText(strings.ToLower(ev.Level)) + "\n")
	b.WriteString("logger=" + maskText(ev.Logger) + "\n")
	b.WriteString("culprit=" + maskText(ev.Culprit) + "\n")
	lines, over := n.frameLines(ev)
	for _, l := range lines {
		b.WriteString("frame=" + l + "\n")
	}
	if over > 0 {
		b.WriteString("frames_over=+" + itoa(over) + "\n")
	}
	return b.String()
}

// canonicalMessage builds the message-derived fallback byte string (§3.3 path 3).
func (n *normalizer) canonicalMessage(ev *rawEvent) string {
	var b strings.Builder
	b.WriteString(canonicalHeader)
	b.WriteByte('\n')
	b.WriteString("level=" + maskText(strings.ToLower(ev.Level)) + "\n")
	b.WriteString("logger=" + maskText(ev.Logger) + "\n")
	b.WriteString("culprit=" + maskText(ev.Culprit) + "\n")
	b.WriteString("frame=msg|" + maskText(ev.Message) + "\n")
	return b.String()
}

// canonicalOverride builds the SDK-fingerprint byte string. `{{ default }}`
// expands in place to the default frame list joined with 0x1F; repeated markers
// expand repeatedly; non-string and empty entries are already dropped by the
// parser (§3.3 path 1).
func (n *normalizer) canonicalOverride(ev *rawEvent) (string, bool) {
	lines, _ := n.frameLines(ev)
	expanded := strings.Join(lines, unitSep)
	entries := make([]string, 0, len(ev.Fingerprint))
	for _, raw := range ev.Fingerprint {
		masked := maskText(raw)
		if masked == "" {
			continue
		}
		masked = strings.ReplaceAll(masked, defaultMarker, expanded)
		if strings.TrimSpace(masked) == "" {
			continue
		}
		entries = append(entries, masked)
	}
	if len(entries) == 0 {
		return "", false
	}
	return canonicalHeader + "\nfp=" + strings.Join(entries, unitSep) + "\n", true
}

// canonicalFor resolves the §3.3 fingerprint order and returns the canonical
// bytes plus whether the message-derived path was used as a fallback.
func (n *normalizer) canonicalFor(ev *rawEvent) (canonical []byte, fallback bool) {
	if len(ev.Fingerprint) > 0 {
		if s, ok := n.canonicalOverride(ev); ok {
			return []byte(s), false
		}
	}
	if len(ev.Frames) > 0 {
		return []byte(n.canonicalDefault(ev)), false
	}
	return []byte(n.canonicalMessage(ev)), true
}

// sigFor computes the sentinel sig of a raw event.
//
// Failure of any path (a panic in the normalizer, an event that is not a JSON
// object after decode) emits TROUBLE-SENTINEL-016 and falls back to the
// message-derived sig, which is always computable.
func (s *Server) sigFor(ev *rawEvent) (sig types.Sig, fallback bool, err *Error) {
	defer func() {
		if r := recover(); r != nil {
			sig = s.norm.sigOfCanonical([]byte(s.norm.canonicalMessage(ev)))
			fallback = true
			err = errf(types.CodeSentinel016, "fingerprint computation failed; message-derived sig used", "normalizer_panic")
		}
	}()
	if ev == nil {
		return types.Sig{}, false, errf(types.CodeSentinel016, "event is not a JSON object after decode", causeFraming)
	}
	if len(ev.Fingerprint) == 0 && len(ev.Frames) == 0 && strings.TrimSpace(ev.Message) == "" {
		// No stack, no culprit and no message: the all-empty case collapses to
		// one stable sig per (level, logger, culprit) — the honest grouping
		// (§6.7).
		return s.norm.sigOfCanonical([]byte(s.norm.canonicalDefault(ev))), false, nil
	}
	canonical, fb := s.norm.canonicalFor(ev)
	return s.norm.sigOfCanonical(canonical), fb, nil
}

// sigOfCanonical hashes canonical bytes into a sentinel sig.
func (n *normalizer) sigOfCanonical(canonical []byte) types.Sig {
	return types.NewSig(types.SrcSentinel, types.SigAlgoSHA256, n.normVersion, types.SigDigest(canonical))
}

// SigOf is the exported signature surface of §2.2: it computes the sig of an
// event that already exists as a types.SentryEvent (the issue desk and the
// ladder re-derive identity through it, never through a second algorithm).
func (s *Server) SigOf(ev types.SentryEvent) (types.Sig, error) {
	raw := &rawEvent{
		Level:       ev.Level,
		Logger:      "",
		Culprit:     ev.Culprit,
		Message:     ev.Message,
		Fingerprint: ev.Fingerprint,
	}
	for _, line := range strings.Split(ev.Stack, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw.Frames = append(raw.Frames, frameFromStackLine(line))
	}
	sig, _, serr := s.sigFor(raw)
	if serr != nil && serr.Code == types.CodeSentinel016 {
		return sig, serr
	}
	return sig, nil
}

// frameFromStackLine parses the rendered `in_app|module|function|file|LINE|ctx`
// form back into a frame, so SigOf is stable across a serialize/parse round trip.
func frameFromStackLine(line string) frame {
	parts := strings.Split(line, "|")
	f := frame{}
	if len(parts) >= 6 {
		f.InApp = parts[0] == "app"
		f.Module = parts[1]
		f.Function = parts[2]
		f.File = parts[3]
		f.ContextLine = parts[5]
	} else {
		f.ContextLine = line
		f.Module = ""
		f.Function = ""
		f.File = ""
	}
	return f
}

// itoa is strconv.Itoa without the import in this file's hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
