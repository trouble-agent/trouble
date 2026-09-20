package lifecycle

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestForwardBatchBounds(t *testing.T) {
	cfg := defaults()
	cfg.Hub.ForwardBatchRecords = 2
	cfg.Hub.ForwardBatchBytes = 512
	recs := []types.Record{
		{Seq: 1, RecID: "ev_1", Payload: map[string]any{"x": "large payload body here"}},
		{Seq: 2, RecID: "ev_2", Payload: map[string]any{"x": "another large payload body"}},
		{Seq: 3, RecID: "ev_3", Payload: map[string]any{"x": "third"}},
	}
	batches := splitRecords(recs, cfg.Hub.ForwardBatchRecords, cfg.Hub.ForwardBatchBytes)
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Errorf("first batch size: got %d, want 2", len(batches[0]))
	}
	if len(batches[1]) != 1 {
		t.Errorf("second batch size: got %d, want 1", len(batches[1]))
	}
}

func TestPostBatchGzip(t *testing.T) {
	var got []types.Record
	var contentEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentEncoding = r.Header.Get("Content-Encoding")
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer gr.Close()
		if err := json.NewDecoder(gr).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Trouble-Ack", "10;3;h1")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ev_3"})
	}))
	defer srv.Close()

	cfg := defaults()
	cfg.Hub.URL = srv.URL
	cfg.Hub.ForwardProjectID = "proj"
	cfg.Origin.HostID = "h1"
	recs := []types.Record{{Seq: 1, RecID: "ev_1"}, {Seq: 2, RecID: "ev_2"}, {Seq: 3, RecID: "ev_3"}}
	ack, code, _, err := postBatch(context.Background(), srv.Client(), *cfg, recs, idemKey(recs, "h1"))
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK {
		t.Fatalf("status: got %d", code)
	}
	if contentEncoding != "gzip" {
		t.Errorf("encoding: got %q, want gzip", contentEncoding)
	}
	if !ack.HasAck || ack.LocalSeq != 3 {
		t.Errorf("ack: %+v", ack)
	}
	if len(got) != 3 {
		t.Errorf("hub received %d records", len(got))
	}
}

func TestRetryAfter429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg := defaults()
	cfg.Hub.URL = srv.URL
	cfg.Hub.ForwardProjectID = "proj"
	cfg.Hub.RetryBase = "2s"
	recs := []types.Record{{Seq: 1, RecID: "ev_1"}}
	_, code, retryAfter, _ := postBatch(context.Background(), srv.Client(), *cfg, recs, "k")
	if code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d", code)
	}
	if retryAfter != 3*time.Second {
		t.Errorf("retry-after: got %v", retryAfter)
	}
}

func splitRecords(recs []types.Record, maxRecs, maxBytes int) [][]types.Record {
	var batches [][]types.Record
	var cur []types.Record
	var curBytes int
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if len(cur) >= maxRecs || (len(cur) > 0 && curBytes+len(b) > maxBytes) {
			batches = append(batches, cur)
			cur = nil
			curBytes = 0
		}
		cur = append(cur, r)
		curBytes += len(b)
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}
