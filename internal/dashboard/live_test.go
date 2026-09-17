package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// live_test.go is the SPEC-10 §7 row for AC-19: a real trigger must appear in a
// polled fragment inside the budget, with the accelerator running (p100 ≤2000 ms,
// p50 ≤1100 ms) and with it stopped (p95 ≤2000 ms). It also asserts the
// stale-render banner when the writer pauses past stall_alert_s.
//
// The trigger is a real mutation of the index the handler reads (a new incident
// record plus a seq bump) — not a stub that returns a canned fragment. The
// client side is modelled on §2.6: the strip polls at strip_poll_ms and the
// content partials at poll_ms; the accelerator is the strip's seq change.

// stripSeqOf extracts data-seq from a rendered health-strip fragment.
func stripSeqOf(body string) string {
	const marker = `data-seq="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// stripPoller is the browser's accelerator loop: it polls the strip at the
// configured interval and publishes the newest seq it has seen.
type stripPoller struct {
	client *http.Client
	url    string
	token  string

	seq   atomic.Value // string
	stop  chan struct{}
	done  chan struct{}
	errCh chan error
}

func startStripPoller(env *testEnv, interval time.Duration) *stripPoller {
	p := &stripPoller{
		client: env.srv.Client(),
		url:    env.srv.URL + "/partials/health",
		token:  env.readPlain,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		errCh:  make(chan error, 4),
	}
	p.seq.Store("")
	go func() {
		defer close(p.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				seq, err := p.poll()
				if err != nil {
					select {
					case p.errCh <- err:
					default:
					}
					continue
				}
				if seq != "" {
					p.seq.Store(seq)
				}
			}
		}
	}()
	return p
}

func (p *stripPoller) poll() (string, error) {
	req, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("strip poll: status %d", resp.StatusCode)
	}
	return stripSeqOf(string(b)), nil
}

func (p *stripPoller) current() string {
	v, _ := p.seq.Load().(string)
	return v
}

// waitChange blocks until the poller has observed a seq different from from.
func (p *stripPoller) waitChange(from string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cur := p.current(); cur != "" && cur != from {
			return cur, nil
		}
		select {
		case err := <-p.errCh:
			return "", err
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", fmt.Errorf("the accelerator never observed a seq change from %s", from)
}

// waitFirst blocks until the poller has taken its first sample.
func (p *stripPoller) waitFirst(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cur := p.current(); cur != "" {
			return cur, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", fmt.Errorf("the strip poller never produced a seq")
}

func (p *stripPoller) close() {
	close(p.stop)
	<-p.done
}

// percentile returns the p-th percentile of the sample set.
func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(d))
	copy(cp, d)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp))*p/100.0 + 0.999999)
	if idx < 1 {
		idx = 1
	}
	if idx > len(cp) {
		idx = len(cp)
	}
	return cp[idx-1]
}

// triggerIncident appends a real incident record to the fixture index and
// returns its id.
func triggerIncident(env *testEnv, i int) string {
	id := fmt.Sprintf("inc_01J9LIVE000000000000%04d", i)
	env.idx.trigger(types.Incident{
		ID: id, Sig: fmt.Sprintf("sentinel:sha256v1:live%04d", i),
		GroupID: "grp_01J9F0000000000000000000AB", State: types.StDetected,
		EntryRung: types.RungRecord, Rung: types.RungRecord, Severity: types.SevHigh,
		OpenedTS: "2026-09-16T09:14:03.221Z", UpdatedTS: "2026-09-16T09:14:03.221Z",
	}, types.Record{
		RecID: fmt.Sprintf("rec_live_%04d", i), TS: "2026-09-16T09:14:03.221Z",
		Kind: types.KIncident, Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		Payload: map[string]any{"transition": "detected", "via": "sensors"},
	})
	return id
}

// TestAC19LiveIncidentWithinBudget is the AC-19 timing assertion with the
// accelerator running: 20 iterations, p100 ≤2000 ms and p50 ≤1100 ms.
func TestAC19LiveIncidentWithinBudget(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	poller := startStripPoller(env, time.Duration(env.cfg.StripPollMS)*time.Millisecond)
	defer poller.close()
	// Let the poller take its first sample so a change is detectable.
	baseline, err := poller.waitFirst(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 20
	lat := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		// The trigger is not synchronized with the poll schedule: spread the
		// phase across the interval so the measurement is not an artifact of
		// the test loop's own cadence.
		time.Sleep(time.Duration((i*137)%900) * time.Millisecond)

		preSeq := poller.current()
		if preSeq == "" {
			preSeq = baseline
		}
		preSeqInt := env.idx.seqNow()
		t0 := time.Now()
		incID := triggerIncident(env, i)

		// The accelerator: the strip reports the new seq, and the client reacts
		// by refetching the content partial immediately (troubleSeq).
		if _, err := poller.waitChange(preSeq, 4*time.Second); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		resp, body := env.get(fmt.Sprintf("/partials/incidents?since=%d", preSeqInt), env.readPlain)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d: content partial status = %d", i, resp.StatusCode)
		}
		if !strings.Contains(body, incID) {
			t.Fatalf("iteration %d: the first fragment after the trigger does not contain %s", i, incID)
		}
		lat = append(lat, time.Since(t0))
	}

	p50 := percentile(lat, 50)
	p100 := percentile(lat, 100)
	t.Logf("AC-19 accelerator on: n=%d p50=%v p100=%v (budget p50 ≤1100ms, p100 ≤2000ms)", len(lat), p50, p100)
	if p100 > 2000*time.Millisecond {
		t.Errorf("p100 = %v, AC-19 budget is ≤2000ms", p100)
	}
	if p50 > 1100*time.Millisecond {
		t.Errorf("p50 = %v, the §7 threshold is ≤1100ms", p50)
	}
}

// TestAC19AcceleratorOff is the §7 accelerator-off variant: content partials poll
// at the mandated 2 s interval and p95 must still be ≤2000 ms.
func TestAC19AcceleratorOff(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	interval := time.Duration(env.cfg.PollMS) * time.Millisecond
	const iterations = 20
	lat := make([]time.Duration, 0, iterations)

	// The client polls on its own fixed cadence; the trigger lands at an
	// unsynchronized phase, which is what makes the measurement honest.
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for i := 0; i < iterations; i++ {
		phase := time.Duration(i*100) * time.Millisecond
		time.Sleep(phase)

		preSeq := env.idx.seqNow()
		t0 := time.Now()
		incID := triggerIncident(env, 100+i)

		// No strip: the next scheduled content poll is the first chance to see
		// the row.
		<-tick.C
		resp, body := env.get(fmt.Sprintf("/partials/incidents?since=%d", preSeq), env.readPlain)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d: status = %d", i, resp.StatusCode)
		}
		if !strings.Contains(body, incID) {
			t.Fatalf("iteration %d: the scheduled content poll did not carry the incident", i)
		}
		// Re-align the cadence for the next phase (the ticker keeps running;
		// the sleep above absorbs the offset).
		lat = append(lat, time.Since(t0))
	}

	p95 := percentile(lat, 95)
	p100 := percentile(lat, 100)
	t.Logf("AC-19 accelerator off: n=%d p95=%v p100=%v (budget p95 ≤2000ms)", len(lat), p95, p100)
	if p95 > 2000*time.Millisecond {
		t.Errorf("p95 = %v without the accelerator, §7 requires ≤2000ms", p95)
	}
}

// floatBox is a mutex-guarded float the health producer and the test share.
type floatBox struct {
	mu sync.Mutex
	v  float64
}

func (f *floatBox) get() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

func (f *floatBox) set(v float64) {
	f.mu.Lock()
	f.v = v
	f.mu.Unlock()
}

// TestStaleBannerOnPausedWriter is §2.6: the banner turns on when the writer is
// paused past stall_alert_s — identical seqs alone are not enough, the growing
// stall counter is the signal.
func TestStaleBannerOnPausedWriter(t *testing.T) {
	stall := &floatBox{}
	env := newEnv(t, envOptions{
		cfg: func(c *Config) {
			unlimitedRates(c)
			c.StallAlertS = 2
		},
		deps: func(d *Deps) {
			autonomy := d.Autonomy
			d.Health = func(ctx context.Context) types.HealthResponse {
				return types.HealthResponse{
					Status: "degraded", LedgerStallS: stall.get(), Autonomy: autonomy.Gates(),
				}
			}
		},
	})

	// A quiet host below the alert threshold: identical seqs, no banner.
	stall.set(0.4)
	for i := 0; i < 5; i++ {
		resp, body := env.get("/partials/health", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		if strings.Contains(body, `data-banner="true"`) {
			t.Fatalf("banner on while the stall counter is below the alert threshold: %.200s", body)
		}
	}

	// The writer pauses: the seq freezes and the stall counter climbs past
	// stall_alert_s (2s in this configuration). Three consecutive identical seqs
	// plus the growing counter is the documented signal.
	stall.set(3.5)
	var last string
	for i := 0; i < 4; i++ {
		resp, body := env.get("/partials/health", env.readPlain)
		wantStatus(t, resp, body, http.StatusOK)
		last = body
	}
	if !strings.Contains(last, `data-banner="true"`) {
		t.Fatalf("banner did not appear after the writer stalled past stall_alert_s: %.240s", last)
	}

	// The page carries the visible banner element in the stalled state.
	_, page := env.get("/", env.readPlain)
	if !strings.Contains(page, `id="stale-banner"`) || !strings.Contains(page, `data-stalled="true"`) {
		t.Fatalf("page does not render the stalled banner: %.300s", page)
	}

	// The writer resumes: the seq advances and the banner clears.
	triggerIncident(env, 9999)
	stall.set(0.1)
	resp, body := env.get("/partials/health", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if strings.Contains(body, `data-banner="true"`) {
		t.Fatalf("banner did not clear after the writer resumed: %.240s", body)
	}
}

// TestFragmentSwapShape drives the exact client exchange §2.6 describes: the
// strip's seq change is what the content partials act on, and the fragment they
// receive carries the guard attributes the client needs to decide freshness.
func TestFragmentSwapShape(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})

	preSeq := env.idx.seqNow()
	resp, empty := env.get(fmt.Sprintf("/partials/incidents?since=%d", preSeq), env.readPlain)
	wantStatus(t, resp, empty, http.StatusOK)
	if strings.Contains(empty, "inc_01J9LIVE") {
		t.Fatal("fixture bug: the live incident already exists")
	}

	incID := triggerIncident(env, 7)
	resp, body := env.get(fmt.Sprintf("/partials/incidents?since=%d", preSeq), env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)
	if !strings.Contains(body, incID) {
		t.Fatalf("the since=<seq> fragment does not carry the new incident")
	}
	if !strings.Contains(body, fmt.Sprintf(`data-seq="%d"`, env.idx.seqNow())) {
		t.Fatalf("the fragment does not carry the seq it was rendered from: %.200s", body)
	}
	if !strings.Contains(body, "data-rendered-ts=") || !strings.Contains(body, "data-stall-s=") {
		t.Fatal("the fragment is missing the stale-render guard")
	}
	if !strings.Contains(body, `hx-sync="this:replace"`) {
		t.Fatal("the fragment does not set hx-sync")
	}
}
