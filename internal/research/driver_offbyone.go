package research

// driver_offbyone.go — the Off-by-One lab driver (SPEC-07 §2.2, §2.3).
//
// This is the only driver that queues work, and every one of the lab's four
// surfaces is implemented against measured behaviour: discover is keyed on a
// class slug and never accepts a fingerprint; submit takes exactly four
// top-level fields; the queue is the only result channel; health lives at the
// root path. Every failure maps onto one SPEC-07 §5 code with the class the
// table pins, and nothing here retries: the service owns cooldown and the fuse.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// driverError is one lab failure, carrying the SPEC-07 §5 code, the pinned
// degrade reason and the lab's own message. The service turns it into an
// outcome, a ledger record and (for five conditions) a gap record.
type driverError struct {
	Code   types.ErrorCode
	Reason string
	Note   string
	Status int
	Msg    string
	Class  types.ErrorClass
}

func (e *driverError) Error() string {
	m := string(e.Code)
	if e.Note != "" {
		m += " (" + e.Note + ")"
	}
	if e.Status != 0 {
		m += fmt.Sprintf(" http=%d", e.Status)
	}
	if e.Msg != "" {
		m += ": " + e.Msg
	}
	return m
}

// errNotFound is the lab's 404: a discover miss ("problem class not found") or a
// queue id the lab no longer knows. It is a signal, not a failure.
var errNotFound = errors.New("research: not found")

// errDriverDisabled is what driver `none` returns from every method.
var errDriverDisabled = errors.New("research: driver disabled")

// maxErrBody bounds how much of a failure body is read (and later recorded).
const maxErrBody = 8 << 10

// offByOne is the live lab client. It is safe for concurrent use.
type offByOne struct {
	base     string // lab_url, no trailing slash
	hc       *http.Client
	reqTO    time.Duration
	submitTO time.Duration
	// webhook overrides the two paths when non-empty (the webhook driver).
	discoverPath string
	submitPath   string
	queuePath    string
	healthPath   string
	statsPath    string
	openAPIPath  string
	name         string
	webhook      bool // true = the relayed driver (query-param queue URL)
}

// newHTTPDriver builds the shared HTTP client and the path table. The connect
// timeout is a dialer timeout; the request timeouts are per-call deadlines.
func newHTTPDriver(cfg config) *offByOne {
	dial := &net.Dialer{Timeout: cfg.ConnectTimeout}
	d := &offByOne{
		hc: &http.Client{Transport: &http.Transport{
			DialContext:         dial.DialContext,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		}},
		reqTO: cfg.RequestTimeout, submitTO: cfg.SubmitTimeout,
		discoverPath: "/api/v1/problems/discover",
		submitPath:   "/api/v1/problems/submit",
		queuePath:    "/api/v1/queue/",
		healthPath:   "/health",
		statsPath:    "/api/v1/stats",
		openAPIPath:  "/openapi.json",
		name:         types.DriverOffByOne,
	}
	return d
}

// newOffByOne returns the off-by-one driver for a resolved config.
func newOffByOne(cfg config) *offByOne {
	d := newHTTPDriver(cfg)
	d.base = strings.TrimRight(cfg.LabURL, "/")
	return d
}

// Name reports the driver's config value.
func (d *offByOne) Name() string { return d.name }

// Discover is the cache lookup: D1 (broad) or D2 (narrowed). It never queues work.
func (d *offByOne) Discover(ctx context.Context, req types.DiscoverRequest) (types.DiscoverResponse, error) {
	narrow := req.Env != "" || req.Lang != "" || req.Version != ""
	body, err := encodeDiscover(req.ProblemClass, req.Env, req.Lang, req.Version, narrow)
	if err != nil {
		return types.DiscoverResponse{}, err
	}
	resp, raw, err := d.do(ctx, http.MethodPost, d.discoverPath, body, false)
	if err != nil {
		return types.DiscoverResponse{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return types.DiscoverResponse{}, errNotFound
	}
	if resp.StatusCode == http.StatusBadRequest {
		return types.DiscoverResponse{}, d.labError(types.CodeResearch002, types.ResReasonStrictDecoder, resp, raw)
	}
	if resp.StatusCode >= 300 {
		return types.DiscoverResponse{}, d.statusError(resp, raw)
	}
	if !isJSON(resp.Header) {
		return types.DiscoverResponse{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "non_json_200", Status: resp.StatusCode, Class: types.ErrClassTransient,
			Msg: "lab answered " + resp.Header.Get("Content-Type"),
		}
	}
	var w wireDiscoverResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return types.DiscoverResponse{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_json", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	if !w.Found || w.Answer == nil {
		// found:false is NOT absence (a verified answer whose signatures carry
		// empty env/lang/version is invisible to a narrowed probe); the service
		// runs the mandatory corpus grep next.
		return types.DiscoverResponse{Found: false}, nil
	}
	sol := answerMap(w.Answer)
	out := types.DiscoverResponse{Found: true, Solution: sol}
	if ts, ok := sol["created_at"].(string); ok && ts != "" {
		out.CachedTS = ts
	} else {
		out.CachedTS = types.NowUTC()
	}
	return out, nil
}

// Submit queues a miss. 409 is success (the lab already has the class queued).
func (d *offByOne) Submit(ctx context.Context, req types.SubmitRequest) (types.SubmitResponse, error) {
	body, err := encodeSubmit(req)
	if err != nil {
		return types.SubmitResponse{}, err
	}
	resp, raw, err := d.do(ctx, http.MethodPost, d.submitPath, body, true)
	if err != nil {
		return types.SubmitResponse{}, err
	}
	switch {
	case resp.StatusCode == http.StatusConflict:
		var w wireSubmitResponse
		_ = json.Unmarshal(raw, &w)
		return types.SubmitResponse{SubmissionID: w.SubmissionID, Duplicate: true}, nil
	case resp.StatusCode == http.StatusBadRequest:
		return types.SubmitResponse{}, d.labError(types.CodeResearch002, types.ResReasonStrictDecoder, resp, raw)
	case resp.StatusCode == http.StatusServiceUnavailable:
		return types.SubmitResponse{}, d.labError(types.CodeResearch004, types.ResReasonSolverUnavailable, resp, raw)
	case resp.StatusCode >= 300:
		return types.SubmitResponse{}, d.statusError(resp, raw)
	}
	var w wireSubmitResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return types.SubmitResponse{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_json", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	if w.SubmissionID == "" {
		return types.SubmitResponse{}, &driverError{
			Code: types.CodeResearch010, Reason: types.ResReasonSolverUnavailable,
			Status: resp.StatusCode, Class: types.ErrClassPermanent, Msg: "submit carried no submission_id",
		}
	}
	return types.SubmitResponse{SubmissionID: w.SubmissionID}, nil
}

// Poll is the only result channel.
func (d *offByOne) Poll(ctx context.Context, submissionID string) (types.QueueStatus, error) {
	q := d.queuePath + url.PathEscape(submissionID)
	if d.webhook {
		q = d.queuePath + "?submission_id=" + url.QueryEscape(submissionID)
	}
	resp, raw, err := d.do(ctx, http.MethodGet, q, nil, false)
	if err != nil {
		return types.QueueStatus{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return types.QueueStatus{}, &driverError{
			Code: types.CodeResearch004, Reason: types.ResReasonSolverUnavailable,
			Note: "queue_not_found", Status: resp.StatusCode, Class: types.ErrClassTransient,
			Msg: "submission " + submissionID + " is unknown to the lab",
		}
	}
	if resp.StatusCode >= 300 {
		return types.QueueStatus{}, d.statusError(resp, raw)
	}
	if !isJSON(resp.Header) {
		return types.QueueStatus{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "non_json_200", Status: resp.StatusCode, Class: types.ErrClassTransient,
		}
	}
	var w wireQueueStatus
	if err := json.Unmarshal(raw, &w); err != nil {
		return types.QueueStatus{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_json", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	st := types.QueueStatus{SubmissionID: submissionID}
	// stage wins when both status and stage are present (measured).
	switch strings.ToLower(w.Stage) {
	case "queued":
		st.State = types.QueueQueued
	case "solving":
		st.State = types.QueueSolving
	case "storing":
		st.State = types.QueueSolved
	case "failed":
		st.State = types.QueueFailed
	}
	if st.State == "" {
		switch strings.ToLower(w.Status) {
		case "pending", "queued":
			st.State = types.QueueQueued
		case "solving":
			st.State = types.QueueSolving
		case "solved":
			st.State = types.QueueSolved
		case "failed":
			st.State = types.QueueFailed
		default:
			st.State = "" // unknown: the service counts it and keeps polling
		}
	}
	if w.Answer != nil {
		st.State = types.QueueSolved
		st.Solution = answerMap(w.Answer)
	}
	return st, nil
}

// Health is GET /health at the ROOT path (/api/v1/health is 404 by design).
func (d *offByOne) Health(ctx context.Context) (types.LabHealth, error) {
	resp, raw, err := d.do(ctx, http.MethodGet, d.healthPath, nil, false)
	if err != nil {
		return types.LabHealth{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return types.LabHealth{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "health_404", Status: resp.StatusCode, Class: types.ErrClassTransient,
			Msg: "health is at the root path; check lab_url",
		}
	}
	if resp.StatusCode >= 300 {
		return types.LabHealth{}, d.statusError(resp, raw)
	}
	if !isJSON(resp.Header) {
		return types.LabHealth{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "non_json_200", Status: resp.StatusCode, Class: types.ErrClassTransient,
		}
	}
	var w wireHealth
	if err := json.Unmarshal(raw, &w); err != nil {
		return types.LabHealth{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_json", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	return types.LabHealth{Status: w.Status, Uptime: w.Uptime}, nil
}

// Stats is GET /api/v1/stats: the capability and depth surface.
func (d *offByOne) Stats(ctx context.Context) (types.LabHealth, error) {
	resp, raw, err := d.do(ctx, http.MethodGet, d.statsPath, nil, false)
	if err != nil {
		return types.LabHealth{}, err
	}
	if resp.StatusCode >= 300 {
		return types.LabHealth{}, d.statusError(resp, raw)
	}
	if !isJSON(resp.Header) {
		return types.LabHealth{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "non_json_200", Status: resp.StatusCode, Class: types.ErrClassTransient,
		}
	}
	var w wireStats
	if err := json.Unmarshal(raw, &w); err != nil {
		return types.LabHealth{}, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_json", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	return types.LabHealth{
		Status: "ok", TotalProblems: w.TotalProblems, TotalAnswers: w.TotalAnswers,
		VerifiedAnswers: w.VerifiedAnswers, QueueDepth: w.QueueDepth,
		HitRate: w.HitRate, Coverage: w.Coverage, SolverAvailable: w.SolverAvailable,
	}, nil
}

// OpenAPIPaths is the capability probe: the three routes trouble needs must be
// present, or the driver degrades to discover-only mode (§2.4).
func (d *offByOne) OpenAPIPaths(ctx context.Context) ([]string, error) {
	resp, raw, err := d.do(ctx, http.MethodGet, d.openAPIPath, nil, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, d.statusError(resp, raw)
	}
	var w wireOpenAPI
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Note: "bad_openapi", Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	out := make([]string, 0, len(w.Paths))
	for p := range w.Paths {
		out = append(out, p)
	}
	return out, nil
}

// do performs one request with the configured timeout. Transport failures are
// mapped onto TROUBLE-RESEARCH-001/transient; a request that outlived its
// deadline is the same code (the ladder never waits, the cooldown handles it).
func (d *offByOne) do(ctx context.Context, method, path string, body []byte, submit bool) (*http.Response, []byte, error) {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = d.base + path
	}
	to := d.reqTO
	if submit {
		to = d.submitTO
	}
	cctx := ctx
	cancel := func() {}
	if to > 0 {
		cctx, cancel = context.WithTimeout(ctx, to)
	}
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(cctx, method, u, rdr)
	if err != nil {
		return nil, nil, &driverError{Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable, Class: types.ErrClassTransient, Msg: err.Error()}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, nil, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	return resp, raw, nil
}

// labError decodes the lab's error body into a driverError.
func (d *offByOne) labError(code types.ErrorCode, reason string, resp *http.Response, raw []byte) *driverError {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > maxErrBody {
		msg = msg[:maxErrBody]
	}
	var w wireLabError
	if err := json.Unmarshal(raw, &w); err == nil {
		if w.Message != "" {
			msg = w.Message
		} else if w.Error != "" {
			msg = w.Error
		}
	}
	return &driverError{
		Code: code, Reason: reason, Status: resp.StatusCode,
		Class: types.CodeClass[code], Msg: msg,
	}
}

// statusError maps an unmapped status onto a code: 5xx and 429 are transient lab
// problems, every other 4xx is a permanent request problem.
func (d *offByOne) statusError(resp *http.Response, raw []byte) *driverError {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > maxErrBody {
		msg = msg[:maxErrBody]
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Status: resp.StatusCode, Class: types.ErrClassTransient, Msg: msg,
		}
	}
	return &driverError{
		Code: types.CodeResearch002, Reason: types.ResReasonStrictDecoder,
		Status: resp.StatusCode, Class: types.ErrClassPermanent, Msg: msg,
	}
}
