package issues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dbDocument is the anchor document of §3.10: one document per sig, keyed by
// `<ns_prefix>.<project>.<sig16>`.
type dbDocument struct {
	Iss          string   `json:"iss"`
	Sig          string   `json:"sig"`
	Project      string   `json:"project"`
	Driver       string   `json:"driver"`
	State        string   `json:"state"`
	Title        string   `json:"title"`
	Severity     string   `json:"severity"`
	FirstSeenTS  string   `json:"first_seen_ts"`
	LastSeenTS   string   `json:"last_seen_ts"`
	Occurrences  int      `json:"occurrences"`
	ReleaseRange []string `json:"release_range"`
	Inc          string   `json:"inc"`
	TaskID       string   `json:"task_id"`
	ResearchID   string   `json:"research_id"`
	Comments     int      `json:"comments"`
	Manual       bool     `json:"manual"`
	Idem         string   `json:"idem"`
	UpdatedTS    string   `json:"updated_ts"`
	Redactions   int      `json:"redactions"`
}

// dbCommentDoc is one comment key's value: one key per comment, so two writers
// never clobber each other (§3.10).
type dbCommentDoc struct {
	ID   string `json:"id"`
	Iss  string `json:"iss"`
	Body string `json:"body"`
	TS   string `json:"ts"`
	Idem string `json:"idem"`
}

// dbIndexDoc is the fast-boot pointer document (§3.10).
type dbIndexDoc struct {
	Iss        string `json:"iss"`
	ExternalID string `json:"external_id"`
	UpdatedTS  string `json:"updated_ts"`
}

// dbKVDoc is the KV envelope this driver speaks. SPEC-09 §3.10 pins the key
// layout and the semantics (PUT then always GET and compare), not the envelope;
// the driver and the backend agree on this one shape.
type dbKVDoc struct {
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

type duckbrainDriver struct {
	cfg   types.IssueDriverConfig
	hc    *http.Client
	base  string
	key   string
	ns    string
	desk  *Desk
	mu    sync.Mutex
	cache map[string]dbDocument
}

func newDuckbrainDriver(cfg types.IssueDriverConfig, desk *Desk, hc *http.Client) (types.IssueDriver, error) {
	envKey := cfg.APIKeyEnv
	if envKey == "" {
		envKey = "TROUBLE_DUCKBRAIN_KEY"
	}
	key := ""
	// A local-first backend may legitimately need no credential: an unset key file
	// is fine, but a configured file that is unreadable or has a bad mode is
	// TROUBLE-ISSUES-003 (§3.10).
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		key = v
	} else if cfg.APIKeyFile != "" {
		expanded, err := ExpandPath(cfg.APIKeyFile)
		if err != nil {
			return nil, newErr(types.CodeIssues003, ReasonValidation, 0, false, "duckbrain key file: %v", err)
		}
		if _, statErr := os.Lstat(expanded); statErr == nil {
			v, rerr := ReadSecretFile(expanded)
			if rerr != nil {
				return nil, rerr
			}
			key = v
		}
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "http://127.0.0.1:7645"
	}
	ns := cfg.NSPrefix
	if ns == "" {
		ns = "trouble.issues"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &duckbrainDriver{cfg: cfg, hc: hc, base: base, key: key, ns: ns, desk: desk, cache: map[string]dbDocument{}}, nil
}

func (d *duckbrainDriver) Name() string { return "duckbrain" }

func (d *duckbrainDriver) timeout() time.Duration {
	if t := d.cfg.Timeout.Std(); t > 0 {
		return t
	}
	return 10 * time.Second
}

func (d *duckbrainDriver) sig16(sig string) string {
	sum := sha256.Sum256([]byte(sig))
	if s, err := types.ParseSig(sig); err == nil && s.Short != "" {
		return s.Short
	}
	return fmt.Sprintf("%x", sum)[:16]
}

// AnchorKey is the §3.10 anchor key layout.
func (d *duckbrainDriver) AnchorKey(sig, project string) string {
	return d.ns + "." + project + "." + d.sig16(sig)
}

// CommentKey derives the one-key-per-comment name. The spec fixes the shape
// (`<anchor>.c.<ULID>`) and requires it to be deterministic per idempotency
// marker, so the ULID body is a Crockford-base32 rendering of the marker's digest.
func (d *duckbrainDriver) CommentKey(sig, project, idem string) string {
	return d.AnchorKey(sig, project) + ".c." + stableULID(idem)
}

func (d *duckbrainDriver) IndexKey(sig, project string) string {
	return d.AnchorKey(sig, project) + ".i"
}

var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func stableULID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	enc := crockford.EncodeToString(sum[:16])
	if len(enc) > 26 {
		enc = enc[:26]
	}
	for len(enc) < 26 {
		enc += "0"
	}
	return enc
}

// Healthcheck is GET /health plus a header-only prefix probe (§3.10). It creates
// nothing.
func (d *duckbrainDriver) Healthcheck(ctx context.Context) (types.DriverHealth, error) {
	h := types.DriverHealth{Driver: "duckbrain", RateLimitRemaining: -1, CheckedTS: types.FormatUTC(time.Now())}
	status, err := d.do(ctx, http.MethodGet, "/health", nil, nil)
	if err != nil {
		return h, err
	}
	if status != http.StatusOK {
		return h, d.statusError(status, "health")
	}
	status, err = d.do(ctx, http.MethodGet, "/v1/kv?prefix="+url.QueryEscape(d.ns+".")+"&limit=1", nil, nil)
	if err != nil {
		return h, err
	}
	if status != http.StatusOK {
		return h, d.statusError(status, "prefix probe")
	}
	h.OK = true
	h.Detail = sanitizeDetail("kv " + d.ns + ". reachable")
	return h, nil
}

// EnsureBySig: the anchor key decides. Absent → create; present and open → fold
// (append a comment key); present and closed → report closed so the desk's reopen
// path owns the transition.
func (d *duckbrainDriver) EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error) {
	var resp types.EnsureBySigResponse
	project := d.projectFor(req.Sig)
	anchorKey := d.AnchorKey(req.Sig, project)
	doc, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return resp, err
	}
	if found {
		ref := d.refFromDoc(anchorKey, doc)
		resp.Ref = ref
		if doc.State == types.IssueClosed {
			return resp, nil
		}
		marker := "issue_comment|" + doc.Iss + "|adopt|" + req.Sig
		body := req.Body
		if !strings.Contains(body, "trouble:idem=") {
			body += "\n\n" + IdemMarker(marker)
		}
		out, err := d.Comment(ctx, ref, body)
		if err != nil {
			return resp, err
		}
		resp.Ref = out
		resp.Commented = true
		return resp, nil
	}
	now := types.FormatUTC(time.Now())
	iss := types.NewID(types.PIss)
	doc = dbDocument{
		Iss:         iss,
		Sig:         req.Sig,
		Project:     project,
		Driver:      "duckbrain",
		State:       types.IssueOpen,
		Title:       req.Title,
		Severity:    string(req.Severity),
		FirstSeenTS: now,
		LastSeenTS:  now,
		Occurrences: 1,
		Idem:        "issue_ensure|duckbrain|" + req.Sig + "|" + project,
		UpdatedTS:   now,
	}
	if err := d.putDoc(ctx, anchorKey, doc); err != nil {
		return resp, err
	}
	// The write response is never trusted: read the anchor back and compare.
	back, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return resp, err
	}
	if !found || back.Idem != doc.Idem || back.UpdatedTS != doc.UpdatedTS {
		return resp, newErr(types.CodeIssues006, ReasonReadback, 0, true,
			"anchor read-back mismatch after create (found=%t)", found)
	}
	if err := d.putIndex(ctx, anchorKey, back); err != nil {
		return resp, err
	}
	resp.Ref = d.refFromDoc(anchorKey, back)
	resp.Created = true
	return resp, nil
}

// Comment writes one comment key per idempotency marker, then re-reads the
// anchor and compares (§3.10).
func (d *duckbrainDriver) Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	project := d.projectFor(ref.Sig)
	anchorKey := ref.ExternalID
	if anchorKey == "" {
		anchorKey = d.AnchorKey(ref.Sig, project)
	}
	marker := idemFromBody(body)
	if marker == "" {
		marker = "issue_comment|" + ref.ID + "|" + stableULID(body)
		body += "\n\n" + IdemMarker(marker)
	}
	commentKey := anchorKey + ".c." + stableULID(marker)
	if _, found, err := d.getValue(ctx, commentKey); err != nil {
		return ref, err
	} else if found {
		// Repeat of the same trigger: a no-op that returns the unchanged ref.
		return ref, nil
	}
	doc, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return ref, err
	}
	if !found {
		return ref, newErr(types.CodeIssues007, ReasonAnchorLost, http.StatusNotFound, false,
			"anchor %s is gone", anchorKey)
	}
	now := types.FormatUTC(time.Now())
	cd := dbCommentDoc{ID: stableULID(marker), Iss: doc.Iss, Body: body, TS: now, Idem: marker}
	if err := d.putValue(ctx, commentKey, cd); err != nil {
		return ref, err
	}
	doc.Comments++
	doc.UpdatedTS = now
	doc.LastSeenTS = now
	doc.Occurrences++
	doc.Idem = marker
	if err := d.putDoc(ctx, anchorKey, doc); err != nil {
		return ref, err
	}
	back, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return ref, err
	}
	if !found || back.Idem != doc.Idem || back.Comments != doc.Comments {
		return ref, newErr(types.CodeIssues006, ReasonReadback, 0, true,
			"anchor read-back mismatch after comment (idem %q vs %q, comments %d vs %d)",
			short(back.Idem), short(doc.Idem), back.Comments, doc.Comments)
	}
	return d.refFromDoc(anchorKey, back), nil
}

// Close writes state=closed plus a reason comment; a second close is a no-op
// success.
func (d *duckbrainDriver) Close(ctx context.Context, ref types.IssueRef, reason string) (types.IssueRef, error) {
	return d.setState(ctx, ref, reason, types.IssueClosed)
}

// Reopen is the optional capability of §3.11.
func (d *duckbrainDriver) Reopen(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	return d.setState(ctx, ref, body, types.IssueOpen)
}

func (d *duckbrainDriver) setState(ctx context.Context, ref types.IssueRef, comment, state string) (types.IssueRef, error) {
	anchorKey := ref.ExternalID
	if anchorKey == "" {
		anchorKey = d.AnchorKey(ref.Sig, d.projectFor(ref.Sig))
	}
	doc, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return ref, err
	}
	if !found {
		return ref, newErr(types.CodeIssues007, ReasonAnchorLost, http.StatusNotFound, false,
			"anchor %s is gone", anchorKey)
	}
	if doc.State == state {
		return d.refFromDoc(anchorKey, doc), nil
	}
	if comment != "" {
		marker := idemFromBody(comment)
		if marker == "" {
			marker = "issue_close|" + doc.Iss + "|" + state
			comment += "\n\n" + IdemMarker(marker)
		}
		key := anchorKey + ".c." + stableULID(marker)
		cd := dbCommentDoc{ID: stableULID(marker), Iss: doc.Iss, Body: comment, TS: types.FormatUTC(time.Now()), Idem: marker}
		if err := d.putValue(ctx, key, cd); err != nil {
			return ref, err
		}
		doc.Comments++
	}
	doc.State = state
	doc.UpdatedTS = types.FormatUTC(time.Now())
	if err := d.putDoc(ctx, anchorKey, doc); err != nil {
		return ref, err
	}
	back, found, err := d.getDoc(ctx, anchorKey)
	if err != nil {
		return ref, err
	}
	if !found {
		return ref, newErr(types.CodeIssues006, ReasonReadback, 0, true, "anchor vanished after a %s write", state)
	}
	if back.State != state {
		return ref, newErr(types.CodeIssues008, ReasonCloseRefused, 0, false,
			"read-back reports state %q after a %s write", back.State, state)
	}
	return d.refFromDoc(anchorKey, back), nil
}

// Lookup returns the anchor document for a sig, the boot-rebuild read.
func (d *duckbrainDriver) Lookup(ctx context.Context, sig, project string) (types.IssueRef, bool, error) {
	doc, found, err := d.getDoc(ctx, d.AnchorKey(sig, project))
	if err != nil || !found {
		return types.IssueRef{}, false, err
	}
	return d.refFromDoc(d.AnchorKey(sig, project), doc), true, nil
}

func (d *duckbrainDriver) refFromDoc(anchorKey string, doc dbDocument) types.IssueRef {
	state := doc.State
	if state == "" {
		state = types.IssueOpen
	}
	return types.IssueRef{
		ID:         doc.Iss,
		Driver:     "duckbrain",
		Sig:        doc.Sig,
		ExternalID: anchorKey,
		URL:        "duckbrain://" + anchorKey,
		State:      state,
		CreatedTS:  doc.FirstSeenTS,
		UpdatedTS:  doc.UpdatedTS,
		Comments:   doc.Comments,
		TaskID:     doc.TaskID,
		ResearchID: doc.ResearchID,
	}
}

func (d *duckbrainDriver) getDoc(ctx context.Context, key string) (dbDocument, bool, error) {
	var doc dbDocument
	raw, found, err := d.getRaw(ctx, key)
	if err != nil || !found {
		return doc, false, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, false, newErr(types.CodeIssues006, ReasonReadback, 0, true, "anchor decode: %v", err)
	}
	d.mu.Lock()
	d.cache[key] = doc
	d.mu.Unlock()
	return doc, true, nil
}

func (d *duckbrainDriver) getValue(ctx context.Context, key string) (dbCommentDoc, bool, error) {
	var cd dbCommentDoc
	raw, found, err := d.getRaw(ctx, key)
	if err != nil || !found {
		return cd, false, err
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return cd, false, newErr(types.CodeIssues006, ReasonReadback, 0, true, "comment decode: %v", err)
	}
	return cd, true, nil
}

func (d *duckbrainDriver) getRaw(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var out dbKVDoc
	status, err := d.do(ctx, http.MethodGet, "/v1/kv/"+url.PathEscape(key), nil, &out)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if status != http.StatusOK {
		return nil, false, d.statusError(status, "get "+key)
	}
	return out.Value, true, nil
}

func (d *duckbrainDriver) putDoc(ctx context.Context, key string, doc dbDocument) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return newErr(types.CodeIssues001, ReasonValidation, 0, false, "anchor encode: %v", err)
	}
	return d.putRaw(ctx, key, raw)
}

func (d *duckbrainDriver) putValue(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return newErr(types.CodeIssues001, ReasonValidation, 0, false, "value encode: %v", err)
	}
	return d.putRaw(ctx, key, raw)
}

func (d *duckbrainDriver) putRaw(ctx context.Context, key string, raw json.RawMessage) error {
	body := dbKVDoc{Value: raw}
	status, err := d.do(ctx, http.MethodPut, "/v1/kv/"+url.PathEscape(key), body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		return d.statusError(status, "put "+key)
	}
	return nil
}

func (d *duckbrainDriver) putIndex(ctx context.Context, anchorKey string, doc dbDocument) error {
	idx := dbIndexDoc{Iss: doc.Iss, ExternalID: anchorKey, UpdatedTS: doc.UpdatedTS}
	return d.putValue(ctx, anchorKey+".i", idx)
}

func (d *duckbrainDriver) do(ctx context.Context, method, path string, payload any, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, newErr(types.CodeIssues001, ReasonValidation, 0, false, "encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, body)
	if err != nil {
		return 0, newErr(types.CodeIssues001, ReasonValidation, 0, false, "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "trouble/"+Version)
	// The header NAME is configuration; the value is resolved at boot and is
	// never logged, never placed in a record and never returned by the driver.
	header := d.cfg.APIKeyHeader
	if header == "" {
		header = "Authorization"
	}
	if d.key != "" {
		v := d.key
		if scheme := d.cfg.APIKeyScheme; scheme != "" {
			v = scheme + " " + d.key
		}
		req.Header.Set(header, v)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, newErr(types.CodeIssues001, ReasonTransient, 0, true, "%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		e := newErr(types.CodeIssues002, ReasonRateLimited, resp.StatusCode, true, "rate limited on %s %s", method, path)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				e.ResetTS = types.FormatUTC(time.Now().Add(time.Duration(secs) * time.Second))
			}
		}
		return resp.StatusCode, e
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, newErr(types.CodeIssues003, ReasonUnauthorized, resp.StatusCode,
			false, "auth failed on %s %s", method, path)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, newErr(types.CodeIssues001, ReasonTransient, resp.StatusCode, true,
			"%s %s returned %d", method, path, resp.StatusCode)
	}
	if out != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return resp.StatusCode, newErr(types.CodeIssues001, ReasonTransient, resp.StatusCode, true, "read body: %v", err)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.StatusCode, newErr(types.CodeIssues001, ReasonTransient, resp.StatusCode, true,
					"decode %s %s: %v", method, path, err)
			}
		}
	}
	return resp.StatusCode, nil
}

func (d *duckbrainDriver) statusError(status int, what string) error {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return newErr(types.CodeIssues007, ReasonAnchorLost, status, false, "%s: not found", what)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return newErr(types.CodeIssues003, ReasonUnauthorized, status, false, "%s: auth failed", what)
	case status == http.StatusTooManyRequests:
		return newErr(types.CodeIssues002, ReasonRateLimited, status, true, "%s: rate limited", what)
	case status >= 500:
		return newErr(types.CodeIssues001, ReasonTransient, status, true, "%s: server error %d", what, status)
	}
	return newErr(types.CodeIssues001, ReasonTransient, status, true, "%s: unexpected status %d", what, status)
}

// projectFor resolves the project a sig's filing belongs to: the desk's own
// resolver when the driver was built by a desk, else the `unknown` bucket so the
// key layout stays total.
func (d *duckbrainDriver) projectFor(sig string) string {
	if d.desk != nil {
		if p := d.desk.projectFor(sig); p != "" {
			return p
		}
	}
	return "unknown"
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

var (
	_ types.IssueDriver = (*duckbrainDriver)(nil)
	_ Reopener          = (*duckbrainDriver)(nil)
)
