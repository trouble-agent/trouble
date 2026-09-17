package lifecycle

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ForwardLoop runs the satellite forward path (SPEC-12 §3.7).
func ForwardLoop(ctx context.Context, cfg Config) error {
	spool, err := OpenSpool(cfg)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	backoff := time.Duration(0)
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		batch, footer, err := spool.NextBatch(cfg.Hub.ForwardBatchRecords, cfg.Hub.ForwardBatchBytes)
		if err != nil {
			return err
		}
		if batch == nil {
			// Nothing to send; wait for more records.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.Hub.RetryBase.Std()):
				continue
			}
		}
		id := idemKey(batch, cfg.Origin.HostID)
		ack, code, retryAfter, err := postBatch(ctx, client, cfg, batch, id)
		if err != nil {
			// network/5xx
			attempts++
			backoff = nextBackoff(cfg, attempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		switch code {
		case http.StatusOK, http.StatusCreated:
			spool.UpdateAck(ack)
			spool.Trim(ack.LocalSeq)
			_ = spool.SaveState()
			attempts = 0
			backoff = 0
		case http.StatusTooManyRequests:
			attempts++
			backoff = maxDuration(cfg.Hub.RetryBase.Std(), retryAfter)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		case http.StatusBadRequest:
			return fmt.Errorf("%w: hub refused forward", types.CodeLifecycle014)
		default:
			attempts++
			backoff = nextBackoff(cfg, attempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		_ = footer
	}
}

// postBatch sends one batch to the hub and returns the ack.
func postBatch(ctx context.Context, client *http.Client, cfg Config, batch []types.Record, idem string) (AckHeader, int, time.Duration, error) {
	var ack AckHeader
	payload, err := json.Marshal(batch)
	if err != nil {
		return ack, 0, 0, err
	}
	gz, err := gzipData(payload)
	if err != nil {
		return ack, 0, 0, err
	}
	url := strings.TrimRight(cfg.Hub.URL, "/") + "/api/" + cfg.Hub.ForwardProjectID + "/envelope/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(gz))
	if err != nil {
		return ack, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("trouble-protocol-version", strconv.Itoa(cfg.Hub.ProtocolVersion))
	req.Header.Set("host_id", cfg.Origin.HostID)
	req.Header.Set("hub_id", cfg.Origin.HubID)
	req.Header.Set("idempotency_key", idem)
	req.Header.Set("ack", fmt.Sprintf("%d;%d;%s", 0, localSeqHigh(batch), cfg.Origin.HostID))
	if cfg.Hub.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Hub.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ack, 0, 0, err
	}
	defer resp.Body.Close()
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		ack = parseAckHeader(resp.Header.Get("X-Trouble-Ack"))
	}
	return ack, resp.StatusCode, retryAfter, nil
}

// AckHeader parses the hub ack.
type AckHeader struct {
	HubSeq   uint64
	LocalSeq uint64
	HostID   string
	HasAck   bool
}

func parseAckHeader(s string) AckHeader {
	parts := strings.Split(s, ";")
	if len(parts) != 3 {
		return AckHeader{}
	}
	hub, _ := strconv.ParseUint(parts[0], 10, 64)
	local, _ := strconv.ParseUint(parts[1], 10, 64)
	return AckHeader{HubSeq: hub, LocalSeq: local, HostID: parts[2], HasAck: true}
}

func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if i, err := strconv.Atoi(s); err == nil {
		return time.Duration(i) * time.Second
	}
	return 0
}

func nextBackoff(cfg Config, attempt int) time.Duration {
	base := cfg.Hub.RetryBase.Seconds()
	max := cfg.Hub.RetryMax.Seconds()
	if base <= 0 {
		base = 2
	}
	if max <= 0 {
		max = 300
	}
	d := base * math.Pow(2, float64(attempt))
	if d > max {
		d = max
	}
	// ±20% jitter
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(d*jitter) * time.Second
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func gzipData(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func localSeqHigh(batch []types.Record) uint64 {
	if len(batch) == 0 {
		return 0
	}
	return batch[len(batch)-1].Seq
}

// idemKey derives the record-scoped idempotency key (SPEC-12 §3.7).
func idemKey(batch []types.Record, hostID string) string {
	if len(batch) == 0 {
		return "rec:0-0|" + hostID
	}
	first := batch[0].Seq
	last := batch[len(batch)-1].Seq
	return fmt.Sprintf("rec:%d-%d|%s", first, last, hostID)
}

// NextBatch reads the next batch from sealed segments.
func (s *Spool) NextBatch(maxRecords, maxBytes int) ([]types.Record, spoolSegmentFooter, error) {
	batches, footers, err := s.ReadSealedSegments(maxRecords, maxBytes)
	if err != nil {
		return nil, spoolSegmentFooter{}, err
	}
	if len(batches) == 0 {
		return nil, spoolSegmentFooter{}, nil
	}
	var footer spoolSegmentFooter
	if len(footers) > 0 {
		footer = footers[0]
	}
	return batches[0], footer, nil
}

// UpdateAck updates the spool state from an ack header.
func (s *Spool) UpdateAck(ack AckHeader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack.HubSeq > s.state.AckHubSeq {
		s.state.AckHubSeq = ack.HubSeq
	}
	if ack.LocalSeq > s.state.AckLocalSeq {
		s.state.AckLocalSeq = ack.LocalSeq
	}
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
