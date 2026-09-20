package scrub

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §6 concurrency: Engine holds immutable compiled rules and an immutable
// shield table; only the counters mutate, through atomics. N ingestion
// goroutines share one engine with no lock on the hot path.
//
// Run with -race for the interesting version: `go test -race -run Concurrency`.
func TestConcurrencySharedEngine(t *testing.T) {
	e := newTestEngine(t, "")
	payloads := []string{
		"PASSWORD=hunter2swordfish",
		`{"token": "abc123xyz"}`,
		"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\n",
		"http://" + testPubKeyA + ":0123456789abcdef0123456789abcdef@hooks.example:7643/7",
		"no secret here at all, just a journal line",
		"~/projects/trouble/internal/scrub/engine.go",
	}
	want := make([]string, len(payloads))
	for i, p := range payloads {
		out, _ := scrubString(t, e, types.TgEventMsg, "1", p)
		want[i] = out
	}
	const goroutines = 16
	const rounds = 200
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < rounds; i++ {
				idx := (seed + i) % len(payloads)
				out, res, err := e.ScrubString(ctx, types.TgEventMsg, "1", payloads[idx])
				if err != nil {
					errs <- err.Error()
					return
				}
				if out != want[idx] {
					errs <- "output differs between goroutines: " + out
					return
				}
				if res.Redactions == 0 && strings.Contains(payloads[idx], "REDACTED") {
					errs <- "a redacted payload reported 0 redactions"
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	if got := e.Stats().Calls; got != uint64(goroutines*rounds+len(payloads)) {
		t.Errorf("calls = %d, want %d (the atomic counters must add up)", got, goroutines*rounds+len(payloads))
	}
}
