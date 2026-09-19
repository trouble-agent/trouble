package flow

// router.go — the task-router driver and the durable queue (SPEC-08 §3.6).
//
// Two modes, one wire payload: HTTP POST to `{endpoint}{dispatch_path}` with a
// bearer token, or the router CLI with the identical JSON on stdin and stdout.
// Idempotency is the task id: the router treats a repeated task_id as accepted.
// 5xx/timeout/refused retry 1s/3s/9s and then spool; 4xx other than 429 is
// permanent and never retried.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dispatchPayload is the §3.6 wire body. The key set is pinned: the router
// dedups on `idem_key`, which is the task id.
type dispatchPayload struct {
	IdemKey       string   `json:"idem_key"`
	TaskID        string   `json:"task_id"`
	BoardPath     string   `json:"board_path"`
	Repo          string   `json:"repo"`
	Sig           string   `json:"sig"`
	Inc           string   `json:"inc"`
	Title         string   `json:"title"`
	Priority      string   `json:"priority"`
	Complexity    string   `json:"complexity"`
	CapabilityTag []string `json:"capability_tags"`
	Severity      string   `json:"severity"`
	HostID        string   `json:"host_id"`
	SubmittedTS   string   `json:"submitted_ts"`
}

// DispatchSpawn performs the §3.6 router dispatch for one hot-fix spawn
// request. It is the production implementation of the spawnRequester seam the
// composition root injects back: one wire format, produced once (the pinned
// dispatchPayload), with the router deduping on the idem key. The worktree is
// the router's to report; an ack without one returns "" and the spawn takes
// the §6.20 timeout path.
func (f *Flow) DispatchSpawn(ctx context.Context, req types.SpawnRequest) (string, string, error) {
	return f.dispatchSpawn(ctx, req, true)
}

// dispatchSpawn is DispatchSpawn with the durable-queue half made explicit. The
// replay loop calls it with spool=false: the entry it is draining IS the durable
// record, and re-spooling a failed replay would grow the queue on every attempt
// instead of counting the attempt against the entry (§3.9a).
func (f *Flow) dispatchSpawn(ctx context.Context, req types.SpawnRequest, spool bool) (string, string, error) {
	row := types.BoardRow{
		ID: req.TaskID, Sig: req.Sig, Inc: req.Inc, Repo: req.Repo,
		Title: f.titleFor(req), Priority: "P1",
	}
	if row.ID == "" {
		row.ID = types.NewID(types.PTsk)
	}
	ref, err := f.dispatchRow(ctx, row, types.SevHigh, spool)
	return ref, "", err
}

// dispatch marshals the payload and performs one dispatch attempt sequence.
func (f *Flow) dispatch(ctx context.Context, row types.BoardRow, severity types.Severity) (string, error) {
	return f.dispatchRow(ctx, row, severity, true)
}

// dispatchRow performs one dispatch attempt sequence (the three immediate
// retries) and, when the immediate path is exhausted, hands the payload to the
// durable queue unless the caller owns that decision already.
func (f *Flow) dispatchRow(ctx context.Context, row types.BoardRow, severity types.Severity, spool bool) (string, error) {
	p := dispatchPayload{
		IdemKey: row.ID, TaskID: row.ID, BoardPath: f.boardOfRow(row), Repo: row.Repo,
		Sig: row.Sig, Inc: row.Inc, Title: row.Title, Priority: row.Priority,
		Complexity: row.Complexity, CapabilityTag: defaultSlice(row.CapabilityTags),
		Severity: string(severity), HostID: f.deps.Actors.HostID,
		SubmittedTS: types.FormatUTC(f.clock().Now()),
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	retries := f.cfg.Router.Retries
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			// 1s / 3s / 9s.
			wait := time.Duration(1) * time.Second
			for i := 1; i < attempt; i++ {
				wait *= 3
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		}
		switch f.cfg.Router.Mode {
		case "cli":
			_, err = f.dispatchCLI(ctx, body)
		default:
			_, err = f.dispatchHTTP(ctx, body)
		}
		if err == nil {
			return p.TaskID, nil
		}
		last = err
		var fe *flowError
		if asFlowError(err, &fe) && !isRetryable(fe) {
			return "", err // a permanent 4xx: no retry, recorded with the body
		}
	}
	// The immediate path is exhausted: one SpoolEntry for replayed re-dispatch.
	if spool {
		f.enqueueDispatch(ctx, p)
	}
	return "", &flowError{Code: types.CodeFlow005, Msg: fmt.Sprintf("dispatch failed after %d retries: %v", retries, last)}
}

// dispatchHTTP posts the payload and classifies the status.
func (f *Flow) dispatchHTTP(ctx context.Context, body []byte) (string, error) {
	url := strings.TrimRight(f.cfg.Router.Endpoint, "/") + f.cfg.Router.DispatchPath
	to := f.cfg.Router.Timeout.Std()
	if to <= 0 {
		to = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", &flowError{Code: types.CodeFlow005, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := readToken(f.cfg.Router.TokenFile); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if env := os.Getenv(f.cfg.Router.TokenEnv); env != "" {
		req.Header.Set("Authorization", "Bearer "+env)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", &flowError{Code: types.CodeFlow005, Msg: err.Error()}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return string(rb), nil
	case resp.StatusCode == http.StatusConflict:
		// An idempotent replay after a spool flush is success.
		return string(rb), nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &flowError{Code: types.CodeFlow005, Msg: "429 rate limited", Retryable: true}
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500:
		return "", &flowError{Code: types.CodeFlow005, Msg: fmt.Sprintf("http %d", resp.StatusCode), Retryable: true}
	default:
		return "", &flowError{Code: types.CodeFlow005, Msg: strings.TrimSpace(string(rb))}
	}
}

// dispatchCLI runs the router CLI with the payload on stdin.
func (f *Flow) dispatchCLI(ctx context.Context, body []byte) (string, error) {
	if f.cfg.Router.CLIPath == "" {
		return "", &flowError{Code: types.CodeFlow005, Msg: "router.cli_path is unset"}
	}
	to := f.cfg.Router.Timeout.Std()
	if to <= 0 {
		to = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	cmd := exec.CommandContext(cctx, f.cfg.Router.CLIPath, "--json", "dispatch")
	cmd.Stdin = bytes.NewReader(body)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// A token in argv is forbidden; it is exported to the child instead.
		return "", &flowError{Code: types.CodeFlow005, Msg: err.Error(), Retryable: true}
	}
	return out.String(), nil
}

// enqueueDispatch puts a dispatch on the durable queue (SPEC-08 §3.9a).
//
// The three immediate retries above are the fast path; this is the durable one.
// It claims durability only when the entry landed on a queue the flow REPLAYS
// (§3.9a), and names the coupling when it did not — before §3.9a this recorded
// `dispatch_state="spooled"` for an entry the desk's driver-keyed replay never
// listed and could not decode.
func (f *Flow) enqueueDispatch(ctx context.Context, p dispatchPayload) {
	req := types.SpawnRequest{
		ID: types.NewID(types.PSpawn), TaskID: p.TaskID, Sig: p.Sig, Inc: p.Inc,
		Repo: p.Repo, PriorityClass: p.Priority, RequestedTS: p.SubmittedTS,
	}
	inc := types.Incident{ID: p.Inc, Sig: p.Sig}
	// spoolPut writes the record: the durable half claims `spooled` only when a
	// queue this subsystem replays accepted the entry (§3.9a).
	f.spoolPut(ctx, "dispatch", "spool_not_replayable", req, inc)
}

// enqueue puts a spawn request on the durable queue (§3.9). The in-memory queue
// is the bounded dashboard view: a pending spawn is never dropped, and QueueDepth
// is what the dashboard's first number reports. The durable half is the flow's
// own store (§3.9a), which is also what survives a restart.
func (f *Flow) enqueue(ctx context.Context, sp types.SpawnRequest) {
	f.mu.Lock()
	replaced := false
	for i := range f.queue {
		if f.queue[i].ID == sp.ID {
			f.queue[i] = sp
			replaced = true
			break
		}
	}
	if !replaced {
		f.queue = append(f.queue, sp)
	}
	over := len(f.queue) > 256
	if over {
		// Bounded, drop-oldest with a ledger note — never a silent loss.
		dropped := f.queue[0]
		f.queue = f.queue[1:]
		f.record(ctx, types.Incident{ID: dropped.Inc, Sig: dropped.Sig}, map[string]any{
			"stage": "gate", "decision": "failed", "reason": "queue_drop_oldest",
			"task_id": dropped.TaskID, "error_code": string(types.CodeLifecycle015),
		})
	}
	f.mu.Unlock()
	// The durable half. This is a spawn the state machine has just parked, so a
	// missing queue is recorded as a real loss of durability, not a hint.
	inc := types.Incident{ID: sp.Inc, Sig: sp.Sig}
	if !f.spoolPut(ctx, "spawn", "spool_not_replayable", sp, inc) {
		return
	}
	f.recordSpawn(ctx, sp, "spawn", sp.State, map[string]any{
		"reason": "spooled", "dispatch_state": "spooled", "coupling": spoolCoupling,
	})
}

// isRetryable reports whether a dispatch failure may be retried.
func isRetryable(fe *flowError) bool { return fe.Retryable }

// asFlowError is errors.As for *flowError.
func asFlowError(err error, out **flowError) bool {
	if fe, ok := err.(*flowError); ok {
		*out = fe
		return true
	}
	return false
}

// boardOfRow resolves a row's board directory from the configured project.
func (f *Flow) boardOfRow(row types.BoardRow) string {
	if p, ok := f.projectFor(row); ok && p.BoardPath != "" {
		return p.BoardPath
	}
	return boardOf(row.Repo, f.cfg.BoardPath)
}
