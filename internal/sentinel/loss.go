package sentinel

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Dispositions recorded on every `event` record (`payload.disposition`, §3.9).
const (
	dispAdmitted  = "admitted"
	dispSampled   = "sampled"
	dispDropped   = "dropped_quota"
	dispSpooled   = "spooled"
	dispSpoolFull = "dropped_spool_full"
	// dispSuppressed is the reserved-suppression disposition (§3.8: the canary
	// is exempt, so this is used only by callers that suppress a sig).
	dispSuppressed = "suppressed"
)

// lossOutcome is what the request path must do with a refused item.
type lossOutcome struct {
	keep        bool
	disposition string
	status      int
	headers     map[string]string
	errorCode   types.ErrorCode
	sampleRate  float64
	// respCode is the code the HTTP response carries. It is 010 for a project
	// quota breach and 011 for the disk budget (§3.9's header table); the event
	// record carries `error_code` = 014 instead, because that is the loss
	// policy's own code (§5).
	respCode types.ErrorCode
}

// applyLoss implements the three loss policies of §3.9 for one event the
// quota or the disk budget refused.
func (s *Server) applyLoss(entry *projectEntry, ev *rawEvent, sig string, decision types.RateLimitDecision, now time.Time, used int) lossOutcome {
	policy := entry.proj.LossPolicy
	if policy == "" {
		policy = types.LossDropCounter
	}
	out := lossOutcome{status: 200, errorCode: types.CodeSentinel014}
	switch policy {
	case types.LossSample:
		n := 1
		if entry.proj.QuotaEPM > 0 {
			n = int(math.Ceil(float64(used+1) / float64(entry.proj.QuotaEPM)))
		}
		if n < 1 {
			n = 1
		}
		out.sampleRate = 1.0 / float64(n)
		out.disposition = dispSampled
		if sampleEvent(ev.ID, n) {
			out.keep = true
		}
		return out
	case types.LossSpoolIfLight:
		kept, droppedOldest := s.spoolEvent(entry, ev, decision.Reason)
		if !kept {
			out.disposition = dispSpoolFull
			out.errorCode = types.CodeSentinel015
			return out
		}
		out.disposition = dispSpooled
		s.counters.spooled.Add(1)
		entry.mu.Lock()
		entry.spooled++
		entry.mu.Unlock()
		if droppedOldest > 0 {
			// drop-oldest-with-ledger-note: the loss is accounted, never silent.
			_, _ = s.appendRecord(context.Background(), types.KGap, sig, "sentinel",
				gapRecordPayload("spool_drop_oldest", "spool:"+entry.proj.ID, droppedOldest, "", types.FormatUTC(now)), 0)
		}
		return out
	default: // drop-with-counter
		out.disposition = dispDropped
		out.status = 429
		out.respCode = types.CodeSentinel010
		headers := map[string]string{
			"Retry-After":          itoa(decision.RetryAfterS),
			"X-Sentry-Rate-Limits": decision.Header,
			"X-Sentry-Error":       string(types.CodeSentinel010),
		}
		if decision.Reason == causeDiskBudget {
			out.respCode = types.CodeSentinel011
			headers["X-Sentry-Error"] = string(types.CodeSentinel011)
			headers["X-Sentry-Rate-Limits"] = diskBudgetHeader
			headers["Retry-After"] = "300"
		}
		out.headers = headers
		entry.mu.Lock()
		entry.dropped++
		entry.mu.Unlock()
		s.counters.droppedQuota.Add(1)
		return out
	}
}

// sampleEvent is the deterministic 1/N sampler of §3.9: keep when
// sha256(event_id)[:4] mod N == 0.
func sampleEvent(eventID string, n int) bool {
	if n <= 1 {
		return true
	}
	sum := sha256.Sum256([]byte(eventID))
	v := binary.BigEndian.Uint32(sum[:4])
	return int(v%uint32(n)) == 0
}

// spoolEvent writes one event to the spool. It returns whether the event was
// kept and how many older entries drop-oldest evicted.
func (s *Server) spoolEvent(entry *projectEntry, ev *rawEvent, reason string) (kept bool, dropped int) {
	if s.spool == nil {
		return false, 0
	}
	raw := ev.Raw
	if len(raw) == 0 {
		obj := map[string]any{
			"event_id": ev.ID,
			"level":    ev.Level,
			"message":  ev.Message,
			"culprit":  ev.Culprit,
			"release":  ev.Release,
			"logger":   ev.Logger,
			"platform": ev.Platform,
		}
		if b, err := json.Marshal(obj); err == nil {
			raw = b
		}
	}
	se := SpoolEntry{
		Project:    entry.proj.ID,
		SourceKind: ev.SourceKind,
		AuthForm:   ev.AuthForm,
		TS:         ev.TS,
		ItemType:   "event",
		Reason:     reason,
		Raw:        raw,
	}
	dropped, err := s.spool.Append(se)
	if err != nil {
		if errors.Is(err, ErrSpoolFull) {
			// The spool budget could not be made to fit this entry: the event is
			// dropped with TROUBLE-SENTINEL-015 (§3.9).
			return false, dropped
		}
		// A failed spool write is TROUBLE-SENTINEL-020 and a loud log: the event
		// is counted dropped, never silently lost.
		s.counters.spoolWriteFail.Add(1)
		if s.logger != nil {
			s.logger.Printf("TROUBLE-SENTINEL-020 spool write failed for project %s: %v", entry.proj.ID, err)
		}
		return false, dropped
	}
	return true, 0
}
