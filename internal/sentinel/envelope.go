package sentinel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// maxHeaderLine is the pinned 8KB line cap for envelope and item header lines
// (§3.1). Body lines are capped at MaxLineBytes (64KB default).
const maxHeaderLine = 8 * 1024

// lookaheadLines bounds the `length`-absent heuristic: an item body ends at the
// next line that parses as an item header, searched over at most 64 lines.
const lookaheadLines = 64

// binaryItemTypes require an explicit `length` (§3.1).
var binaryItemTypes = map[string]bool{
	"attachment": true, "minidump": true, "profile": true, "replay": true,
}

// itemPolicy is what the envelope layer does with each item type (§3.2). An
// unsupported type never fails the envelope.
type itemPolicy int

const (
	policyEvent itemPolicy = iota
	policyClientReport
	policyDropPlane
	policyDropBinary
	policyUnknown
)

func policyFor(typ string) itemPolicy {
	switch typ {
	case "event":
		return policyEvent
	case "client_report":
		return policyClientReport
	case "session", "transaction", "profile", "replay":
		return policyDropPlane
	case "attachment", "minidump":
		return policyDropBinary
	default:
		return policyUnknown
	}
}

// envelopeItem is one parsed item.
type envelopeItem struct {
	Type           string
	ContentType    string
	Filename       string
	AttachmentType string
	Length         int64 // -1 when absent
	Body           []byte
	// Overrun marks a `length` that understated the body (§6.4).
	Overrun bool
}

// envelope is a parsed envelope: the header object plus its items in order.
type envelope struct {
	Header map[string]any
	Items  []envelopeItem
}

// headerString returns a string header value.
func (e *envelope) headerString(key string) string {
	if e.Header == nil {
		return ""
	}
	if v, ok := e.Header[key].(string); ok {
		return v
	}
	return ""
}

// itemTypes lists every type seen, in envelope order (duplicates collapse to
// the first sighting) — the §3.2 `SentryEvent.ItemTypes` source.
func (e *envelope) itemTypes() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(e.Items))
	for _, it := range e.Items {
		if seen[it.Type] {
			continue
		}
		seen[it.Type] = true
		out = append(out, it.Type)
	}
	return out
}

// pushbackReader is a line reader that can hand one line back, which is what
// the `length`-absent heuristic needs: the line that ends an item body is the
// next item's header.
type pushbackReader struct {
	br   *bufio.Reader
	back []byte
}

func newPushbackReader(b []byte) *pushbackReader {
	return &pushbackReader{br: bufio.NewReaderSize(bytes.NewReader(b), maxHeaderLine+1)}
}

func (p *pushbackReader) readLine(max int) ([]byte, error) {
	if p.back != nil {
		line := p.back
		p.back = nil
		return line, nil
	}
	return readCappedLine(p.br, max)
}

func (p *pushbackReader) unread(line []byte) { p.back = line }

// parseEnvelope decodes the §3.1 framing.
//
// Rules enforced here: the first line is a JSON object (001 otherwise); item
// headers are JSON objects with a `type` of at most 64 chars; `length` reads
// exactly that many bytes followed by exactly one LF; a binary item type
// without `length` is 001 cause length_required; a zero-item envelope is legal.
func (s *Server) parseEnvelope(b []byte) (*envelope, *Error) {
	pr := newPushbackReader(b)
	env := &envelope{Items: []envelopeItem{}}

	line, err := pr.readLine(maxHeaderLine)
	if err == io.EOF {
		return nil, errf(types.CodeSentinel001, "envelope is empty: the header line is mandatory", causeFraming)
	}
	if err != nil {
		return nil, framingErr(err)
	}
	hdr := map[string]any{}
	if jerr := json.Unmarshal(line, &hdr); jerr != nil || hdr == nil {
		return nil, errf(types.CodeSentinel001, "envelope header line is not a JSON object", causeFraming)
	}
	env.Header = hdr

	for {
		line, err := pr.readLine(maxHeaderLine)
		if err == io.EOF {
			return env, nil
		}
		if err != nil {
			return nil, framingErr(err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			// A trailing LF after the final item is legal (§3.1); a blank line
			// between items is not.
			rest, _ := io.ReadAll(io.LimitReader(pr.br, 1))
			if len(bytes.TrimSpace(rest)) == 0 {
				return env, nil
			}
			return nil, errf(types.CodeSentinel001, "unexpected blank line between items", causeFraming)
		}
		item, ierr := s.parseItemHeader(line)
		if ierr != nil {
			return nil, ierr
		}
		if item.Length >= 0 {
			body, overrun, berr := s.readLengthBody(pr, item.Length)
			if berr != nil {
				return nil, berr
			}
			item.Body = body
			item.Overrun = overrun
			env.Items = append(env.Items, item)
			continue
		}
		body, next, lerr := s.readUnlengthBody(pr)
		if lerr != nil {
			return nil, lerr
		}
		item.Body = body
		env.Items = append(env.Items, item)
		if next == nil {
			return env, nil
		}
		pr.unread(next)
	}
}

// readLengthBody reads exactly n bytes followed by exactly one LF (§3.1).
//
// A `length` that understates the body is the lying-length case of §6.4: the
// trailing bytes up to the next LF are discarded, the item parses, and the
// caller counts `item_overrun_total` — a lying length is data, not a rejection.
// A body shorter than its declared length is still TROUBLE-SENTINEL-001.
func (s *Server) readLengthBody(pr *pushbackReader, n int64) ([]byte, bool, *Error) {
	if n > s.cfg.MaxItemBytes {
		return nil, false, errf(types.CodeSentinel002, "item exceeds max_item_bytes", causeItemTooLarge)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(pr.br, body); err != nil {
		return nil, false, errf(types.CodeSentinel001, "item body is shorter than its declared length", causeFraming)
	}
	nl, err := pr.br.ReadByte()
	if err == io.EOF {
		// The envelope ended right after the body: a final LF is optional.
		return body, false, nil
	}
	if err != nil {
		return nil, false, errf(types.CodeSentinel001, "reading past the item body failed", causeFraming)
	}
	if nl == '\n' {
		return body, false, nil
	}
	// Lying length: discard the un-declared remainder of this line.
	if _, derr := pr.br.ReadBytes('\n'); derr != nil && derr != io.EOF {
		return nil, false, errf(types.CodeSentinel001, "discarding an overrunning item body failed", causeFraming)
	}
	return body, true, nil
}

// parseItemHeader decodes one item header line.
func (s *Server) parseItemHeader(line []byte) (envelopeItem, *Error) {
	item := envelopeItem{Length: -1}
	hdr := map[string]any{}
	if err := json.Unmarshal(line, &hdr); err != nil || hdr == nil {
		return item, errf(types.CodeSentinel001, "item header line is not a JSON object", causeFraming)
	}
	typ, _ := hdr["type"].(string)
	if typ == "" {
		return item, errf(types.CodeSentinel001, "item header has no type", causeFraming)
	}
	if len(typ) > 64 {
		return item, errf(types.CodeSentinel001, "item type is longer than 64 characters", causeFraming)
	}
	item.Type = typ
	if v, ok := hdr["content_type"].(string); ok {
		item.ContentType = v
	}
	if v, ok := hdr["filename"].(string); ok {
		item.Filename = v
	}
	if v, ok := hdr["attachment_type"].(string); ok {
		item.AttachmentType = v
	}
	if v, present := hdr["length"]; present {
		n, ok := jsonInt(v)
		if !ok || n < 0 {
			return item, errf(types.CodeSentinel001, "item length must be an integer >= 0", causeFraming)
		}
		item.Length = n
	} else if binaryItemTypes[typ] {
		return item, errf(types.CodeSentinel001, "binary item types require an explicit length", causeLengthRequired)
	}
	return item, nil
}

// readUnlengthBody reads an item body that carries no `length`. It returns the
// body and, when the lookahead found one, the item-header line that ends it
// (the caller hands it back to the reader).
func (s *Server) readUnlengthBody(pr *pushbackReader) ([]byte, []byte, *Error) {
	var body bytes.Buffer
	var held []byte
	for i := 0; i < lookaheadLines; i++ {
		line, err := pr.readLine(maxHeaderLine)
		if err == io.EOF {
			if held != nil {
				body.Write(held)
			}
			return body.Bytes(), nil, nil
		}
		if err != nil {
			return nil, nil, framingErr(err)
		}
		if looksLikeItemHeader(line) {
			return body.Bytes(), line, nil
		}
		if held != nil {
			body.Write(held)
		}
		held = line
		if int64(body.Len())+int64(len(held)) > s.cfg.MaxItemBytes {
			return nil, nil, errf(types.CodeSentinel002, "item exceeds max_item_bytes", causeItemTooLarge)
		}
	}
	// The lookahead window closed without an item header: everything left is
	// this item's body.
	if held != nil {
		body.Write(held)
	}
	rest, _ := io.ReadAll(pr.br)
	body.Write(rest)
	if int64(body.Len()) > s.cfg.MaxItemBytes {
		return nil, nil, errf(types.CodeSentinel002, "item exceeds max_item_bytes", causeItemTooLarge)
	}
	return body.Bytes(), nil, nil
}

// looksLikeItemHeader reports whether a line is a JSON object with a string
// `type` — the §3.1 heuristic for `length`-absent bodies.
func looksLikeItemHeader(line []byte) bool {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	var probe struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(t, &probe); err != nil {
		return false
	}
	return probe.Type != nil && *probe.Type != ""
}

// framingErr maps a read failure to the pinned framing code.
func framingErr(err error) *Error {
	if err == errLineTooLong {
		return errf(types.CodeSentinel001, "a header line exceeds the 8KB cap", causeFraming)
	}
	return errf(types.CodeSentinel001, "envelope framing is invalid", causeFraming)
}

var errLineTooLong = errLine("sentinel: header line exceeds the cap")

type errLine string

func (e errLine) Error() string { return string(e) }

// readCappedLine reads one LF-terminated line, refusing a line longer than max
// bytes with errLineTooLong. A final line without a trailing LF is accepted:
// SDKs omit it.
func readCappedLine(br *bufio.Reader, max int) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull || (err == nil && len(line) > max) {
		return nil, errLineTooLong
	}
	if err == io.EOF && len(line) == 0 {
		return nil, io.EOF
	}
	return line, nil
}

// jsonInt converts a JSON number into an int64.
func jsonInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}

// storeBody extracts the event JSON from a legacy /store/ body (§3.1): the body
// itself, or the `sentry_data` field of a form-encoded body.
func storeBody(contentType string, body []byte) ([]byte, *Error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "", "application/json", "text/plain", "application/octet-stream":
		return body, nil
	case "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, errf(types.CodeSentinel022, "form body is not parseable", causeMediaType)
		}
		data := vals.Get("sentry_data")
		if data == "" {
			return nil, errf(types.CodeSentinel019, "form body carries no sentry_data field", causeGenericJSON)
		}
		return []byte(data), nil
	default:
		return nil, errf(types.CodeSentinel022, "unsupported content type on the store route", causeMediaType)
	}
}
