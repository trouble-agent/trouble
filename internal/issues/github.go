package issues

import (
	"bytes"
	"context"
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

// Version is the daemon version stamped into User-Agent and into ledger records
// (SPEC-12 §3.6 owns the ldflags stamp; the desk reads it from the actor and the
// drivers fall back to this).
var Version = "0.0.0-dev"

// Wire shapes of the GitHub REST API, limited to the fields this driver reads.
type ghIssueWire struct {
	Number    int    `json:"number"`
	State     string `json:"state"`
	StateReas string `json:"state_reason"`
	HTMLURL   string `json:"html_url"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Comments  int    `json:"comments"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Locked    bool   `json:"locked"`
	Message   string `json:"message"`
}

type ghCommentWire struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type ghSearchWire struct {
	TotalCount int           `json:"total_count"`
	Items      []ghIssueWire `json:"items"`
	Message    string        `json:"message"`
}

type ghRateLimitWire struct {
	Resources map[string]struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	} `json:"resources"`
}

// githubDriver speaks the GitHub REST API (SPEC-09 §3.9).
type githubDriver struct {
	cfg   types.IssueDriverConfig
	hc    *http.Client
	base  string
	token string

	mu         sync.Mutex
	pausedTill time.Time
	lastSearch time.Time
	searchHits int
	comments   map[string]map[string]bool // issue external id → idem marker → seen
	bootProbed bool
}

func newGitHubDriver(cfg types.IssueDriverConfig, d *Desk, hc *http.Client) (types.IssueDriver, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, newErr(types.CodeIssues003, ReasonConfig, 0, false,
			"github driver needs owner and repo")
	}
	envKey := cfg.TokenEnv
	if envKey == "" {
		envKey = "TROUBLE_GITHUB_TOKEN"
	}
	res, err := ResolveToken("", cfg, cfg.TokenFile, envKey)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(cfg.APIBase, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &githubDriver{
		cfg: cfg, hc: hc, base: base, token: res.Value,
		comments: map[string]map[string]bool{},
	}, nil
}

func (g *githubDriver) Name() string { return "github" }

func (g *githubDriver) repoPath() string {
	return "/repos/" + url.PathEscape(g.cfg.Owner) + "/" + url.PathEscape(g.cfg.Repo)
}

func (g *githubDriver) timeout() time.Duration {
	if t := g.cfg.Timeout.Std(); t > 0 {
		return t
	}
	return 15 * time.Second
}

// Healthcheck is the read-only probe (§2.1): GET /rate_limit and, once per
// process, the repo probe that confirms reachability and issues:write scope.
// OK=false with a nil error means "reachable but degraded" (rate headroom gone).
// probeCtxKey marks a read-only probe: the quota-headroom rule must not blind
// the probe that discovers the headroom.
type probeCtxKey struct{}

func (g *githubDriver) Healthcheck(ctx context.Context) (types.DriverHealth, error) {
	ctx = context.WithValue(ctx, probeCtxKey{}, true)
	h := types.DriverHealth{Driver: "github", RateLimitRemaining: -1, CheckedTS: types.FormatUTC(time.Now())}
	if remaining, reset, err := g.paused(); err != nil {
		return h, err
	} else if remaining > 0 {
		h.RateLimitRemaining = remaining
		h.RateLimitResetTS = reset
	}
	var rl ghRateLimitWire
	status, err := g.do(ctx, http.MethodGet, "/rate_limit", nil, &rl)
	if err != nil {
		return h, err
	}
	if status != http.StatusOK {
		return h, g.statusError(status, nil, "rate_limit")
	}
	core, ok := rl.Resources["core"]
	search := rl.Resources["search"]
	if ok {
		h.RateLimitRemaining = core.Remaining
		h.RateLimitResetTS = unixToRFC3339(core.Reset)
	}
	detail := fmt.Sprintf("core %d/%d, search %d/%d", core.Remaining, core.Limit, search.Remaining, search.Limit)
	h.Detail = sanitizeDetail(detail)

	if !g.bootProbed {
		st, err := g.do(ctx, http.MethodGet, g.repoPath(), nil, nil)
		if err != nil {
			return h, err
		}
		if st == http.StatusNotFound {
			return h, newErr(types.CodeIssues003, ReasonConfig, st, false, "repo %s/%s not found", g.cfg.Owner, g.cfg.Repo)
		}
		if st == http.StatusUnauthorized || st == http.StatusForbidden {
			return h, g.statusError(st, nil, "repo probe")
		}
		if st >= 300 {
			return h, g.statusError(st, nil, "repo probe")
		}
		g.mu.Lock()
		g.bootProbed = true
		g.mu.Unlock()
	}
	if g.cfg.MinRemaining > 0 && ok && core.Remaining <= g.cfg.MinRemaining {
		// Reachable but degraded: the desk pauses the driver below the headroom
		// floor, and the operation is spooled rather than retried inline (§3.5).
		g.setPause(core.Reset)
		h.OK = false
		h.Detail = sanitizeDetail(detail + " (below min_remaining)")
		return h, nil
	}
	h.OK = true
	return h, nil
}

func (g *githubDriver) setPause(resetUnix int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	till := time.Now().Add(15 * time.Minute)
	if resetUnix > 0 {
		t := time.Unix(resetUnix, 0)
		if t.Before(till) {
			till = t
		}
	}
	if till.After(g.pausedTill) {
		g.pausedTill = till
	}
}

func (g *githubDriver) paused() (int, string, error) {
	g.mu.Lock()
	till := g.pausedTill
	g.mu.Unlock()
	if till.IsZero() || time.Now().After(till) {
		return 0, "", nil
	}
	return g.cfg.MinRemaining, types.FormatUTC(till), nil
}

// EnsureBySig guarantees at most one issue per sig: it searches for the sig
// marker first (the read-back that makes a lost 201 harmless), adopts what it
// finds, and only creates when the search is authoritative and empty.
func (g *githubDriver) EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error) {
	var resp types.EnsureBySigResponse
	found, err := g.searchBySig(ctx, req.Sig, false)
	if err != nil {
		return resp, err
	}
	if found != nil {
		ref := g.refFromWire(*found, req.Sig)
		// An open issue is adopted; a closed one is reported as closed so the
		// desk's reopen path owns the transition (§3.11).
		resp.Ref = ref
		if ref.State == types.IssueClosed {
			return resp, nil
		}
		body := req.Body
		if !strings.Contains(body, "trouble:idem=") {
			body = body + "\n\n" + IdemMarker("issue_comment|"+ref.ID+"|adopt")
		}
		out, err := g.Comment(ctx, ref, body)
		if err != nil {
			return resp, err
		}
		resp.Ref = out
		resp.Created = false
		resp.Commented = true
		return resp, nil
	}
	payload := map[string]any{"title": req.Title, "body": req.Body, "labels": req.Labels}
	var out ghIssueWire
	status, err := g.do(ctx, http.MethodPost, g.repoPath()+"/issues", payload, &out)
	if err != nil {
		return resp, err
	}
	if status == http.StatusUnprocessableEntity {
		// 422: a validation refusal. The code is transient but the instance is not
		// retryable, so it must never enter the spool (§3.9.5).
		return resp, newErr(types.CodeIssues001, ReasonValidationFail, status, false,
			"create rejected: %s", firstLine(out.Message))
	}
	if status != http.StatusCreated {
		return resp, g.statusError(status, nil, "create issue")
	}
	// Read-back verification: a create is confirmed, never assumed.
	got, status, err := g.getIssue(ctx, out.Number)
	if err != nil {
		return resp, err
	}
	if status == http.StatusNotFound {
		return resp, newErr(types.CodeIssues001, ReasonReadback, status, true,
			"created issue %d is not readable back", out.Number)
	}
	if status != http.StatusOK {
		return resp, g.statusError(status, nil, "read-back")
	}
	ref := g.refFromWire(got, req.Sig)
	ref.State = types.IssueOpen
	resp.Ref = ref
	resp.Created = true
	return resp, nil
}

// Comment appends one comment per distinct idempotency marker (§3.1). The marker
// is read out of the body the desk built, and the issue's existing comment list is
// used as the read-back so a retried call cannot double-append.
func (g *githubDriver) Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	marker := idemFromBody(body)
	if marker != "" && g.markerSeen(ref.ExternalID, marker) {
		return ref, nil
	}
	if marker != "" && ref.ExternalID != "" {
		seen, err := g.readbackMarkers(ctx, ref.ExternalID)
		if err == nil && seen[marker] {
			g.rememberMarker(ref.ExternalID, marker)
			ref.Comments = len(seen)
			ref.UpdatedTS = types.FormatUTC(time.Now())
			return ref, nil
		}
	}
	payload := map[string]any{"body": body}
	status, err := g.do(ctx, http.MethodPost, g.issuePath(ref.ExternalID)+"/comments", payload, nil)
	if err != nil {
		return ref, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return ref, newErr(types.CodeIssues007, ReasonAnchorLost, status, false,
			"issue %s is gone at the driver", ref.ExternalID)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return ref, g.statusError(status, nil, "comment")
	}
	if marker != "" {
		g.rememberMarker(ref.ExternalID, marker)
	}
	got, status, err := g.getIssue(ctx, atoi(ref.ExternalID))
	if err == nil && status == http.StatusOK {
		// The driver owns the external view; the desk owns the local iss_ id and
		// the linkage fields, so they are carried across the read-back.
		localID, task, research := ref.ID, ref.TaskID, ref.ResearchID
		ref = g.refFromWire(got, ref.Sig)
		ref.ID = localID
		ref.TaskID = task
		ref.ResearchID = research
	} else {
		ref.Comments++
		ref.UpdatedTS = types.FormatUTC(time.Now())
	}
	return ref, nil
}

// Close comments the reason then closes, and reads the state back (§3.9.5).
// Closing an already-closed issue is success.
func (g *githubDriver) Close(ctx context.Context, ref types.IssueRef, reason string) (types.IssueRef, error) {
	return g.setState(ctx, ref, reason, "closed", "completed")
}

// Reopen is the optional capability of §3.11.
func (g *githubDriver) Reopen(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	return g.setState(ctx, ref, body, "open", "reopened")
}

func (g *githubDriver) setState(ctx context.Context, ref types.IssueRef, comment, state, reason string) (types.IssueRef, error) {
	if comment != "" {
		out, err := g.Comment(ctx, ref, comment)
		if err != nil {
			return ref, err
		}
		ref = out
	}
	if state == "closed" && ref.State == types.IssueClosed {
		return ref, nil // idempotent
	}
	payload := map[string]any{"state": state, "state_reason": reason}
	status, err := g.do(ctx, http.MethodPatch, g.issuePath(ref.ExternalID), payload, nil)
	if err != nil {
		return ref, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return ref, newErr(types.CodeIssues007, ReasonAnchorLost, status, false, "issue %s is gone", ref.ExternalID)
	}
	if status == http.StatusLocked || status == http.StatusForbidden {
		return ref, newErr(types.CodeIssues008, ReasonCloseRefused, status, false,
			"driver refused the %s transition for issue %s", state, ref.ExternalID)
	}
	if status == http.StatusUnprocessableEntity {
		return ref, newErr(types.CodeIssues008, ReasonCloseRefused, status, false,
			"driver rejected the %s transition (locked or already handled)", state)
	}
	if status != http.StatusOK {
		return ref, g.statusError(status, nil, state)
	}
	got, status, err := g.getIssue(ctx, atoi(ref.ExternalID))
	if err != nil {
		return ref, err
	}
	if status == http.StatusNotFound {
		return ref, newErr(types.CodeIssues007, ReasonAnchorLost, status, false, "issue %s is gone", ref.ExternalID)
	}
	if status != http.StatusOK {
		return ref, g.statusError(status, nil, "read-back")
	}
	out := g.refFromWire(got, ref.Sig)
	if out.State != state {
		// 2xx but the read-back disagrees: the state is not what the driver said.
		return ref, newErr(types.CodeIssues008, ReasonCloseRefused, status, false,
			"read-back reports state %q after a %s transition", out.State, state)
	}
	out.ID = ref.ID
	out.TaskID = ref.TaskID
	out.ResearchID = ref.ResearchID
	return out, nil
}

// searchBySig runs one of the three exact §3.9.4 queries under the search budget.
func (g *githubDriver) searchBySig(ctx context.Context, sig string, openOnly bool) (*ghIssueWire, error) {
	q := SearchQuery(g.cfg.Owner, g.cfg.Repo, sig, openOnly)
	if err := g.searchBudget(ctx); err != nil {
		return nil, err
	}
	var out ghSearchWire
	status, err := g.do(ctx, http.MethodGet, "/search/issues?"+q, nil, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnprocessableEntity || status == http.StatusForbidden && out.Message != "" {
		return nil, newErr(types.CodeIssues001, ReasonTransient, status, true, "search refused: %s", firstLine(out.Message))
	}
	if status != http.StatusOK {
		return nil, g.statusError(status, nil, "search")
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	item := out.Items[0]
	return &item, nil
}

// SearchQuery is the exact §3.9.4 query string, URL-encoded. It is exported so the
// suite can assert the golden query byte-for-byte.
func SearchQuery(owner, repo, sig string, openOnly bool) string {
	q := fmt.Sprintf("repo:%s/%s is:issue", owner, repo)
	if openOnly {
		q += " is:open"
	}
	q += fmt.Sprintf(" in:body \"trouble:sig=%s\"", sig)
	v := url.Values{}
	v.Set("q", q)
	if openOnly {
		v.Set("per_page", "1")
	} else {
		v.Set("sort", "created")
		v.Set("order", "asc")
		v.Set("per_page", "1")
	}
	return v.Encode()
}

func SearchQueryByIncident(owner, repo, inc string) string {
	v := url.Values{}
	v.Set("q", fmt.Sprintf("repo:%s/%s is:issue in:body \"trouble:inc=%s\"", owner, repo, inc))
	v.Set("per_page", "1")
	return v.Encode()
}

// searchBudget enforces ≤1 search call per search_min_interval (§3.9.4).
func (g *githubDriver) searchBudget(ctx context.Context) error {
	iv := g.cfg.SearchMinInterval.Std()
	if iv <= 0 {
		iv = 6 * time.Second
	}
	g.mu.Lock()
	wait := time.Until(g.lastSearch.Add(iv))
	if wait > 0 {
		g.lastSearch = g.lastSearch.Add(iv)
	} else {
		g.lastSearch = time.Now()
		wait = 0
	}
	g.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
		return newErr(types.CodeIssues002, ReasonRateLimited, 0, true,
			"search budget: next search allowed in %s", wait)
	}
	select {
	case <-ctx.Done():
		return wrapErr(types.CodeIssues001, ReasonTimeout, 0, true, ctx.Err())
	case <-time.After(wait):
		return nil
	}
}

func (g *githubDriver) readbackMarkers(ctx context.Context, externalID string) (map[string]bool, error) {
	var list []ghCommentWire
	status, err := g.do(ctx, http.MethodGet, g.issuePath(externalID)+"/comments?per_page=100", nil, &list)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, g.statusError(status, nil, "comment read-back")
	}
	seen := map[string]bool{}
	for _, c := range list {
		if m := idemFromBody(c.Body); m != "" {
			seen[m] = true
		}
	}
	g.mu.Lock()
	if g.comments[externalID] == nil {
		g.comments[externalID] = map[string]bool{}
	}
	for m := range seen {
		g.comments[externalID][m] = true
	}
	g.mu.Unlock()
	return seen, nil
}

func (g *githubDriver) markerSeen(externalID, marker string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.comments[externalID][marker]
}

func (g *githubDriver) rememberMarker(externalID, marker string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.comments[externalID] == nil {
		g.comments[externalID] = map[string]bool{}
	}
	g.comments[externalID][marker] = true
}

func (g *githubDriver) getIssue(ctx context.Context, n int) (ghIssueWire, int, error) {
	var out ghIssueWire
	status, err := g.do(ctx, http.MethodGet, g.issuePath(strconv.Itoa(n)), nil, &out)
	if err != nil {
		return out, 0, err
	}
	return out, status, nil
}

func (g *githubDriver) issuePath(externalID string) string {
	return g.repoPath() + "/issues/" + url.PathEscape(externalID)
}

func (g *githubDriver) refFromWire(w ghIssueWire, sig string) types.IssueRef {
	ref := types.IssueRef{
		Driver:     "github",
		Sig:        sig,
		ExternalID: strconv.Itoa(w.Number),
		URL:        w.HTMLURL,
		State:      w.State,
		CreatedTS:  w.CreatedAt,
		UpdatedTS:  w.UpdatedAt,
		Comments:   w.Comments,
	}
	if ref.State == "" {
		ref.State = types.IssueOpen
	}
	return ref
}

// do performs one request and classifies the response. Every response's
// X-RateLimit-Remaining / X-RateLimit-Reset is read, and a rate-limit condition
// becomes TROUBLE-ISSUES-002 with the reset carried on the error (§3.5).
func (g *githubDriver) do(ctx context.Context, method, path string, payload any, out any) (int, error) {
	if paused, reset, err := g.paused(); err != nil {
		return 0, err
	} else if paused > 0 {
		e := newErr(types.CodeIssues002, ReasonRateLimited, 0, true,
			"driver is paused until %s (below min_remaining %d)", reset, paused)
		e.ResetTS = reset
		return 0, e
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, newErr(types.CodeIssues001, ReasonValidation, 0, false, "encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return 0, newErr(types.CodeIssues001, ReasonValidation, 0, false, "build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "trouble/"+Version)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		// A transport failure after send is ambiguous: the write may have
		// committed. The reason is recorded so replay treats the entry as
		// unverified rather than creating blindly (edge case 1).
		return 0, newErr(types.CodeIssues001, ReasonTransient, 0, true, "%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	remaining, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	// The quota headroom rule pauses an operation; it must not blind the probe
	// that discovers the headroom in the first place, so /rate_limit is exempt.
	probe, _ := ctx.Value(probeCtxKey{}).(bool)
	lowHeadroom := !probe && g.cfg.MinRemaining > 0 &&
		resp.Header.Get("X-RateLimit-Remaining") != "" && remaining <= g.cfg.MinRemaining
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") != "") ||
		lowHeadroom {
		g.setPause(reset)
		e := newErr(types.CodeIssues002, ReasonRateLimited, resp.StatusCode, true,
			"rate limited on %s %s (remaining %d)", method, path, remaining)
		e.ResetTS = unixToRFC3339(reset)
		return resp.StatusCode, e
	}
	if resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "") {
		return resp.StatusCode, newErr(types.CodeIssues003, ReasonUnauthorized, resp.StatusCode, false,
			"auth failed on %s %s", method, path)
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

func (g *githubDriver) statusError(status int, body any, what string) error {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return newErr(types.CodeIssues007, ReasonAnchorLost, status, false, "%s: not found", what)
	case status == http.StatusLocked, status == http.StatusUnprocessableEntity:
		return newErr(types.CodeIssues008, ReasonCloseRefused, status, false, "%s refused", what)
	case status == http.StatusUnauthorized:
		return newErr(types.CodeIssues003, ReasonUnauthorized, status, false, "%s: auth failed", what)
	case status == http.StatusTooManyRequests:
		return newErr(types.CodeIssues002, ReasonRateLimited, status, true, "%s: rate limited", what)
	case status >= 500:
		return newErr(types.CodeIssues001, ReasonTransient, status, true, "%s: server error %d", what, status)
	}
	return newErr(types.CodeIssues001, ReasonTransient, status, true, "%s: unexpected status %d", what, status)
}

func unixToRFC3339(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return types.FormatUTC(time.Unix(unix, 0))
}

func idemFromBody(body string) string {
	const marker = "trouble:idem="
	i := strings.LastIndex(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	if j := strings.Index(rest, " -->"); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Ensure the driver satisfies the frozen contract and the optional reopen seam.
var (
	_ types.IssueDriver = (*githubDriver)(nil)
	_ Reopener          = (*githubDriver)(nil)
)

// ghKeepsEnv keeps os imported for the env-driven token path in tests.
var _ = os.Getenv
