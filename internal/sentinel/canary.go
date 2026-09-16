package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// CanaryFingerprint is the reserved fingerprint of the canary (§3.8). The
// fingerprint form makes the canary sig stable across every injection, which is
// what lets verification look for exactly one sig.
const CanaryFingerprint = "trouble-canary"

// canaryMessage is the canary's message body (one ULID per injection).
const canaryMessage = "trouble canary"

// canaryClient is the sentry_client string the injection advertises.
const canaryClient = "trouble-canary/0.1.0"

// InjectCanary posts one synthetic envelope through the real HTTP path
// (§3.8): routing → auth → envelope → scrub → sig → group → ledger. It returns
// the event id the listener reported.
func (s *Server) InjectCanary(projectID string) (string, error) {
	if projectID == "" {
		projectID = s.canaryProj
	}
	entry, ok := s.projects.project(projectID)
	if !ok {
		return "", errf(types.CodeSentinel007, "canary project is not configured", causeProjectUnknown)
	}
	s.canaryMu.Lock()
	s.canaryIter++
	iteration := s.canaryIter
	s.canaryMu.Unlock()

	eventID := newEventID()
	ulid := types.NewID(types.PEv)
	body, err := json.Marshal(map[string]any{
		"event_id":    eventID,
		"level":       "info",
		"message":     fmt.Sprintf("%s %s", canaryMessage, ulid),
		"fingerprint": []string{CanaryFingerprint},
		"platform":    "other",
		"release":     "",
		"extra":       map[string]any{"iteration": iteration},
	})
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]any{
		"event_id":       eventID,
		"dsn":            s.canaryDSN(entry),
		"sentry_client":  canaryClient,
		"sentry_version": "7",
		"content_type":   "application/json",
	})
	if err != nil {
		return "", err
	}
	itemHeader, err := json.Marshal(map[string]any{
		"type":         "event",
		"length":       len(body),
		"content_type": "application/json",
	})
	if err != nil {
		return "", err
	}
	var env bytes.Buffer
	env.Write(header)
	env.WriteByte('\n')
	env.Write(itemHeader)
	env.WriteByte('\n')
	env.Write(body)
	env.WriteByte('\n')

	url := fmt.Sprintf("http://%s/api/%s/envelope/", loopbackAddr(s.cfg.Bind), projectID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(env.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	req.Header.Set("X-Sentry-Auth", fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_client=%s", entry.proj.PublicKey, canaryClient))
	// The canary is exempt from quota, loss policies and breakers: it MUST land
	// during a flood, because that is exactly when evidence matters (§3.8).
	if entry.proj.SecretKey != "" {
		req.Header.Set("X-Sentry-Auth", fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_secret=%s, sentry_client=%s",
			entry.proj.PublicKey, entry.proj.SecretKey, canaryClient))
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.noteCanaryMiss(projectID, "canary_transport")
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.noteCanaryMiss(projectID, "canary_rejected")
		return "", fmt.Errorf("sentinel: canary injection rejected with status %d", resp.StatusCode)
	}
	s.counters.canaryInjected.Add(1)
	// One `canary` record per injection (§3.8).
	_, _ = s.appendRecord(context.Background(), types.KCanary, s.CanarySig().String(), "sentinel", map[string]any{
		"phase":     "injected",
		"iteration": iteration,
		"event_id":  eventID,
		"project":   projectID,
		"sig":       s.CanarySig().String(),
	}, 0)
	return eventID, nil
}

// canaryDSN renders the DSN the canary envelope advertises.
func (s *Server) canaryDSN(entry *projectEntry) string {
	d := DSN{
		Scheme:    s.cfg.Scheme,
		PublicKey: entry.proj.PublicKey,
		Host:      s.cfg.AdvertisedHost,
		ProjectID: entry.proj.ID,
	}
	if p, ok := parseBindPort(s.cfg.Bind); ok && p != defaultPortFor(s.cfg.Scheme) {
		d.Port = p
	}
	return d.String()
}

// observeCanary records the canary observation (§3.8): one `canary` record per
// observation plus the runtime state verification reads.
func (s *Server) observeCanary(entry *projectEntry, ev *rawEvent, sig types.Sig) {
	ts := types.FormatUTC(s.now())
	s.counters.canaryObserved.Add(1)
	s.setCanaryState(entry.proj.ID, ts, true)
	s.releases.noteCanary(entry.proj.ID, ts)
	entry.mu.Lock()
	entry.canaryTS = ts
	entry.canaryOK = true
	entry.mu.Unlock()
	_, _ = s.appendRecord(context.Background(), types.KCanary, sig.String(), "sentinel", map[string]any{
		"phase":    "observed",
		"event_id": ev.ID,
		"project":  entry.proj.ID,
		"sig":      sig.String(),
	}, 0)
}

// noteCanaryMiss emits the §3.8 gap when the canary does not land.
func (s *Server) noteCanaryMiss(projectID, cause string) {
	s.counters.canaryMissing.Add(1)
	s.setCanaryState(projectID, "", false)
	entry, ok := s.projects.project(projectID)
	if ok {
		entry.mu.Lock()
		entry.canaryOK = false
		entry.mu.Unlock()
	}
	_, _ = s.appendRecord(context.Background(), types.KGap, s.CanarySig().String(), "sentinel",
		gapRecordPayload("canary_missing", "canary:"+projectID, 1, "", types.FormatUTC(s.now())), 0)
	_ = cause
}

// canaryLoop injects the canary every canary_interval and turns a miss into a
// gap plus a dead `sentinel` source (§3.8).
func (s *Server) canaryLoop() {
	defer s.wg.Done()
	iv := s.cfg.CanaryInterval.Std()
	if iv <= 0 {
		iv = 10 * time.Minute
	}
	// The first injection happens immediately: boot must prove the path works.
	if _, err := s.InjectCanary(s.canaryProj); err != nil {
		s.logger.Printf("sentinel: first canary injection failed: %v", err)
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if _, err := s.InjectCanary(s.canaryProj); err != nil {
				s.logger.Printf("sentinel: canary injection failed: %v", err)
			}
			s.checkCanaryAge()
		}
	}
}

// checkCanaryAge emits `canary_missing` when no observation arrived within
// 2 × canary_interval (§3.8).
func (s *Server) checkCanaryAge() {
	project, lastTS, ok := s.canaryState()
	if project == "" {
		// No observation has ever landed: the configured project is the one whose
		// canary is missing.
		project = s.canaryProj
	}
	if project == "" {
		return
	}
	maxAge := 2 * s.cfg.CanaryInterval.Std()
	if !ok || lastTS == "" {
		s.noteCanaryMiss(project, "canary_missing")
		return
	}
	ts, err := types.ParseUTC(lastTS)
	if err != nil {
		s.noteCanaryMiss(project, "canary_missing")
		return
	}
	if s.now().Sub(ts) > maxAge {
		s.noteCanaryMiss(project, "canary_missing")
	}
}

// setCanaryState updates the process-level canary watermark.
func (s *Server) setCanaryState(project, ts string, ok bool) {
	s.canaryMu.Lock()
	defer s.canaryMu.Unlock()
	if project != "" {
		s.canaryProjState = project
	}
	if ts != "" {
		s.canaryLastTS = ts
	}
	s.canaryLastOK = ok
}

// canaryState reads the canary watermark (project, last observation ts, ok).
func (s *Server) canaryState() (string, string, bool) {
	s.canaryMu.Lock()
	defer s.canaryMu.Unlock()
	return s.canaryProjState, s.canaryLastTS, s.canaryLastOK
}
