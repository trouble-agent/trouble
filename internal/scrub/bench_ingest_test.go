package scrub_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/loadfence"
	"github.com/trouble-agent/trouble/internal/scrub"
	"github.com/trouble-agent/trouble/internal/types"
)

// Harness wave parameters of SPEC-02 §7: the reference harness ran 128
// in-flight appends of the ~1 KiB envelope against a 100-record group-commit
// writer, and the §3.9 floors (≥5,000 req/s, ≤20% cost) were measured under
// exactly this shape.
const (
	harnessWorkers   = 128
	harnessPerWorker = 100
	// minHarnessWorkers is the floor of the auto-bounded wave: a crushed host
	// still runs the harness 32-wide, which keeps tens of appends in flight per
	// 5 ms group-commit window (see harnessWaveForLoad).
	minHarnessWorkers = 32
	// harnessWithRuns is the number of with-scrub attempts on a quiet host —
	// best of three — and, since TRBL-052, the same count for the no-scrub
	// baseline runs (best of best), so the loaded baseline comparison reads
	// the same-statistic on both sides; the v0.1 red baseline was measured
	// under the same best-of-three shape.
	harnessWithRuns = 3
)

// Operator pins (the TRBL-045 doctrine): an explicit positive integer wins
// over the auto-bounded default, so re-measuring the §7 shape on any host
// stays a one-env-var job.
const (
	EnvHarnessWorkers = "SCRUB_HARNESS_WORKERS"
	EnvHarnessRuns    = "SCRUB_HARNESS_RUNS"
)

// envPositiveInt reads an operator knob; the second return reports whether a
// valid pin was present (the auto-bounded default yields only to an explicit
// pin, never to a malformed one).
func envPositiveInt(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// harnessWaveForLoad bounds the append wave the harness sustains by default:
// 512/load workers, clamped to [32, 128]. A quiet host (< 4) keeps the spec
// shape (128-wide, the shape §3.9's reference numbers were measured under) and
// the shape is continuous at the fence (512/4 = 128), then decays with the
// host's own load: 102 at load 5, 64 at load 8, 42 at load 12, the 32 floor
// from load 16 on. 32 is the load TRBL-045 measured this test burning 37.1
// CPU-s over a 7.1 s wall at 518% (~6 cores of self-inflicted runnable
// threads) and the load at which a fleet wave tips the dispatch gate. The
// floor is not arbitrary: 32 workers × 100 appends keep multiple requests in
// flight per 5 ms group-commit window, so the batched-write claim the
// throughput floor stands on is measured at every wave — and because the
// loaded floor tracks the SAME-wave no-scrub baseline (min(4000/scale,
// 0.8x without)), the regression bar reads two waves of the same size and
// stays meaningful as the wave shrinks. Every assertion (the processed-equals-requested count, the
// quiet-host §3.9 floors and ≤20% cost, the loaded baseline floor and the
// re-denominated sanity bar) is untouched. SCRUB_HARNESS_WORKERS pins the wave
// explicitly for a spec-shape re-measurement.
//
// The attempt count is deliberately NOT load-scaled: the best-of-N defence
// exists because a single run's throughput on a shared host spans 2.4k-12.2k
// req/s for identical code, so the loaded 4000 floor is only sound with the
// full best-of-three (measured: best-of-1 at load 9 landed at 3,858 req/s and
// tripped the floor — cutting attempts under load would trade the floor's
// noise defence for CPU, which is a weakening, not a bound). The wave is the
// load lever; the attempt count is the spec shape, pinnable via
// SCRUB_HARNESS_RUNS.
func harnessWaveForLoad(load float64) int {
	if load < 4 {
		return harnessWorkers
	}
	w := int(512.0 / load)
	if w < minHarnessWorkers {
		w = minHarnessWorkers
	}
	if w > harnessWorkers {
		w = harnessWorkers
	}
	return w
}

// TestHarnessWaveScaling pins the load-derived bounds the same way
// TRBL-045's TestLoadGateScaling pins the sentinel's: the quiet-host spec
// shape is asserted directly, the contended-host curves decay with the
// observed load, the floors keep the harness honest (32 workers still fill
// group-commit batches; the loaded floor tracks the same-wave baseline), and
// the re-denominated sanity floor at the spec wave is exactly the historical
// 1500.
func TestHarnessWaveScaling(t *testing.T) {
	waveCases := []struct {
		load float64
		want int
	}{
		{0, 128},   // no /proc/loadavg → the spec shape (reachable below: TROUBLE_HOST_LOAD_OVERRIDE=0)
		{3.9, 128}, // quiet host: the §7 shape asserted directly
		{4, 128},   // 512/4 = 128: the shape is continuous at the fence
		{5, 102},   // 512/5
		{8, 64},    // 512/8
		{12, 42},   // 512/12
		{16, 32},   // 512/16 = 32: the floor boundary
		{33, 32},   // the load TRBL-045 measured 37.1 CPU-s / 518% at
		{200, 32},  // the clamp
	}
	for _, c := range waveCases {
		if got := harnessWaveForLoad(c.load); got != c.want {
			t.Errorf("harnessWaveForLoad(%.2f) = %d, want %d", c.load, got, c.want)
		}
	}
	// The no-loadavg host term is still reachable end-to-end (SPEC-01 §7a:
	// the spec shape stays assertable on a host with no /proc/loadavg):
	// TROUBLE_HOST_LOAD_OVERRIDE=0 drives loadfence.Measure's own load read
	// (the same path the gates consume) to the {0,128} row, with the
	// contention factor at the spec 1.0 alongside it. The load override pins
	// only the load half — the CPU pilot still runs and is irrelevant here —
	// so what this asserts is the shape, not the box.
	t.Setenv(loadfence.EnvLoadOverride, "0")
	quietProf := loadfence.Measure("")
	if quietProf.Load != 0 || quietProf.Contention() != 1 {
		t.Errorf("Measure under a zero load override: Load = %.2f, Contention = %.2f, want 0/1 (the no-loadavg shape)",
			quietProf.Load, quietProf.Contention())
	}
	if got := harnessWaveForLoad(quietProf.Load); got != 128 {
		t.Errorf("harnessWaveForLoad(measured Load %.2f) = %d, want 128 (the no-loadavg spec shape)", quietProf.Load, got)
	}
	// ...and the wave still decays off the MEASURED load: an override of 8
	// must produce the 512/8 = 64 shape through the same live path.
	t.Setenv(loadfence.EnvLoadOverride, "8")
	busyProf := loadfence.Measure("")
	if got := harnessWaveForLoad(busyProf.Load); got != 64 {
		t.Errorf("harnessWaveForLoad(overridden Load %.2f) = %d, want 64 (the 512/8 decay through Measure)", busyProf.Load, got)
	}
	// The attempt count is the spec shape and is NOT load-scaled: the loaded
	// 4000 floor only holds with the full best-of-three (best-of-1 measured
	// 3,858 req/s at load 9 and tripped it).
	if harnessWithRuns != 3 {
		t.Errorf("harnessWithRuns = %d, want 3 (the §7 best-of-three the loaded floor's noise defence depends on)", harnessWithRuns)
	}
	// The measurement bar: even the floored wave keeps 32 workers × 100
	// appends in flight — enough to fill group-commit batches — and the
	// re-denominated sanity floor equals the historical 1500 at the spec wave.
	if got := harnessWaveForLoad(200); got < minHarnessWorkers {
		t.Errorf("harnessWaveForLoad floors below %d workers", minHarnessWorkers)
	}
	if got := 1500 * harnessWaveForLoad(0) / harnessWorkers; got != 1500 {
		t.Errorf("sanity floor at the spec wave = %d, want 1500 (the historical bar, unchanged)", got)
	}
	// The env pins: a valid positive integer wins over the automatic bound; a
	// malformed or non-positive one must NOT be honoured (the default is
	// used), and the knob names are the documented ones.
	if n, ok := envPositiveInt("SCRUB_HARNESS_WORKERS"); ok || n != 0 {
		t.Errorf("envPositiveInt with no pin = (%d, %v), want (0, false)", n, ok)
	}
	t.Setenv(EnvHarnessWorkers, "64")
	if n, ok := envPositiveInt(EnvHarnessWorkers); !ok || n != 64 {
		t.Errorf("envPositiveInt(%s=64) = (%d, %v), want (64, true)", EnvHarnessWorkers, n, ok)
	}
	t.Setenv(EnvHarnessRuns, "-3")
	if _, ok := envPositiveInt(EnvHarnessRuns); ok {
		t.Errorf("a negative pin must not be honoured")
	}
}

// SPEC-02 §7 bench_ingest_test.go: re-runs the ingestion harness with the
// scrubber in the path.
//
// SPEC-04's sentinel (HTTP ingress, envelope decode, gzip, quota accounting)
// does not exist yet, so this harness reproduces the part of that path that
// exists: decode a 1,064-byte Sentry-envelope-shaped JSON body, scrub its four
// payload classes (message, stack, header, env) in one call, and append the
// resulting record through the real ledger writer — i.e. group commit, canonical
// JSON and the boundary re-scan. The measured baseline is the same harness with
// the scrubbing call removed.
func TestIngestHarnessThroughput(t *testing.T) {
	loadfence.SkipUnderCIIfLoadCalibrated(t, "ingestion floor 5,000 req/s quiet (SPEC-04 §3.9; runner measured 1,767 vs loaded floor 2,041)")
	if raceEnabled {
		// rule_timeout is a wall-clock budget (SPEC-02 §3.8): under -race with 128
		// in-flight requests a single rule evaluation can be descheduled past 250 ms
		// and the engine fails closed by design. The harness is a throughput gate,
		// so it runs on the plain build; `go test -race` is the correctness gate.
		t.Skip("throughput harness runs on the plain build; see race_test.go")
	}
	const body = `{"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f","message":"payment worker failed to flush the queue: pool exhausted after 30s of retries","stack":"Traceback (most recent call last):\n  File \"/srv/app/worker.py\", line 41, in flush\n    conn.execute(stmt)\n  File \"/srv/app/pool.py\", line 118, in execute\n    raise queue.Full(\"pool exhausted\")\n  File \"/usr/lib/python3.12/site-packages/sqlalchemy/engine/base.py\", line 1414, in execute\n    return meth(self, multiparams, params, _EMPTY_EXECUTION_OPTS)\n  File \"/usr/lib/python3.12/site-packages/sqlalchemy/engine/base.py\", line 171, in _execute_on_connection\n    return connection._execute_clauseelement(\nqueue.Full: pool exhausted","headers":"accept: application/json\nuser-agent: sentry.python/2.19.2 (cpython 3.12.4)\ncontent-type: application/json\ncontent-length: 1064","env":"PATH=/usr/local/bin:/usr/bin:/bin\nHOME=/home/appuser\nLANG=C.UTF-8\nPYTHONHASHSEED=0\nTZ=UTC\nHOSTNAME=payment-worker-3\nSHLVL=1\nPWD=/srv/app"}`
	targets := map[string]types.ScrubTarget{
		"message": types.TgEventMsg, "stack": types.TgStack,
		"headers": types.TgHeader, "env": types.TgEnv,
	}
	if len(body) < 900 || len(body) > 1200 {
		t.Fatalf("the harness body is %d bytes, want a representative ~1 KiB envelope", len(body))
	}

	// calibration (SPEC-01 §7a, QA-TROUBLE-5): the same Profile the §3.9 µs
	// gates consume, measured ONCE per test, BEFORE any harness run — the run
	// bodies below size their wave and the floors below grade both against
	// this one profile. The §3.9 floor numbers are reference-host figures; a
	// box the CPU pilot measures 1.4x slower gets a 1.4x looser floor
	// (5000/1.4 ≈ 3571), exactly the §3.6 budget/floor agreement ledger item
	// 1 encodes — a TIME scales up by the host factor, a RATE scales down by
	// it. load_avg still participates through the scale's contention half, so
	// a loaded host is no longer graded by load alone: the quiet-vs-busy
	// fence keys on the MEASURED scale (1.0 = the reference class), not on
	// raw load, and the TROUBLE_HOST_LOAD_OVERRIDE / TROUBLE_HOST_CALIB knobs
	// compose into it the same way they do for the ledger gates.
	prof := loadfence.Measure("")
	cpuScale := prof.CPUScale()
	load := prof.Load

	run := func(t *testing.T, withScrub bool) float64 {
		state := mustMkdirTemp(t, ".scrub-ingest-")
		eng := newTestEngine(t, "")
		actor := types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0"}
		origin := types.Origin{HostID: "7f3a91c2d4e5b607", Source: "sentinel:payment-worker"}
		// the reference harness measured "100-line group-commit" (§3.9), so the
		// writer is configured the same way: a 100-record batch and a 5 ms fsync
		// window, with enough in-flight appends to fill a batch.
		rot := ledger.DefaultRotationPolicy()
		rot.MaxBatchRecords = 100
		rot.FsyncWindowMS = 5
		l, err := ledger.Open(context.Background(), ledger.Options{
			Root:           filepath.Join(state, "ledger"),
			Rotation:       rot,
			Retention:      ledger.DefaultRetentionPolicy(),
			Index:          ledger.DefaultIndexOptions(),
			Writer:         actor,
			MaxSchema:      1,
			Now:            time.Now,
			HostID:         "7f3a91c2d4e5b607",
			Zone:           "loopback",
			AssertScrubbed: true,
			ScrubVerify:    eng.Verify,
		})
		if err != nil {
			t.Fatalf("ledger.Open: %v", err)
		}

		workers := harnessWaveForLoad(load) // the shared profile measured before any run
		const perWorker = harnessPerWorker
		var ok atomic.Int64
		ctx := context.Background()
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					var fields map[string]any
					if err := json.Unmarshal([]byte(body), &fields); err != nil {
						t.Errorf("decode: %v", err)
						return
					}
					payload := map[string]any{
						"event_id": fields["event_id"],
						"message":  fields["message"],
						"stack":    fields["stack"],
						"headers":  fields["headers"],
						"env":      fields["env"],
					}
					redactions := 0
					if withScrub {
						out, res, err := eng.ScrubFields(ctx, "1", map[string][]byte{
							"message": []byte(payload["message"].(string)),
							"stack":   []byte(payload["stack"].(string)),
							"headers": []byte(payload["headers"].(string)),
							"env":     []byte(payload["env"].(string)),
						}, targets)
						if err != nil {
							t.Errorf("scrub: %v", err)
							return
						}
						for k, v := range out {
							payload[k] = string(v)
						}
						redactions = res.Redactions
					}
					if _, err := l.Append(ctx, types.RecordDraft{
						Kind: types.KEvent, Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
						Origin: origin, Actor: actor, Redactions: redactions, Payload: payload,
					}); err != nil {
						t.Errorf("append: %v", err)
						return
					}
					ok.Add(1)
				}
			}(w)
		}
		wg.Wait()
		elapsed := time.Since(start)
		if err := l.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := ok.Load(); got != int64(workers*perWorker) {
			t.Fatalf("processed %d requests, want %d", got, workers*perWorker)
		}
		return float64(ok.Load()) / elapsed.Seconds()
	}

	// Throughput on a shared host is a capability, not an average: one hostile
	// scheduling window can halve a single run (measured 2.4k-12.2k req/s for
	// identical code at load_avg_1m 7-31). Two defences, mirroring the ledger
	// floors' load-awareness:
	//
	//   - the with-scrub number is best of three — the run the host disturbed
	//   least — at every load, because the loaded floor's noise defence rests
	//   on it (best-of-1 measured 3,858 req/s at load 9 and tripped the 4000
	//   floor); the no-scrub baseline is best of the SAME count (TRBL-052),
	//   so the loaded comparison is best-vs-best, never one draw against
	//   three (the old floor=without cap demanded a zero-cost scrubber
	//   whenever a crushed host dropped both sides under the bar — measured
	//   1,588 vs 1,667 req/s at 4.7% real cost, a noise flip, not a
	//   regression);
	//   - the loaded floor tracks the same-test no-scrub baseline: the whole
	//   host slows both paths together, so min(4000/scale, 0.8x without)
	//   stays a regression bar (the v0.1 red baseline measured 4.0-4.4k under exactly
	//   this condition, with `without` far above it) without flaking when the
	//   fleet crushes every core. 1500 is the sanity floor: below it the
	//   scrubber dominates even a crushed host, which is a real regression.
	//
	// The default wave is auto-bounded by the host's observed 1-minute load
	// (harnessWaveForLoad, the TRBL-045 pattern) so the harness stops being the
	// suite's biggest self-inflicted offender on a busy box; an explicit
	// SCRUB_HARNESS_WORKERS pin wins over the automatic bound, and a quiet host
	// keeps the exact §7 shape (128-wide, 12,800 appends per run). The attempt
	// count stays the §7 best-of-three at every load — it is the noise defence
	// the loaded 4000 floor rests on, not a load lever — and SCRUB_HARNESS_RUNS
	// pins it for a cheaper or fuller re-measurement.
	workers, workersPinned := envPositiveInt(EnvHarnessWorkers)
	if !workersPinned {
		workers = harnessWaveForLoad(load)
	}
	runs, runsPinned := envPositiveInt(EnvHarnessRuns)
	if runsPinned {
		if runs > harnessWithRuns {
			runs = harnessWithRuns
		}
	} else {
		runs = harnessWithRuns
	}
	// The sanity floor is per-run-wave: the harness body is identical per
	// worker, so a wave a quarter the spec size earns a quarter of the bar.
	// At the spec wave (128) this is exactly the historical 1500; on a quiet
	// host the wave is 128 and nothing moves.
	sanityFloor := 1500 * workers / harnessWorkers
	bestWith, without := 0.0, 0.0
	for attempt := 0; attempt < runs; attempt++ {
		with := run(t, true)
		if with > bestWith {
			bestWith = with
		}
		b := run(t, false)
		if b > without {
			without = b
		}
	}
	cost := (1 - bestWith/without) * 100
	pinNote := "auto-bounded by harnessWaveForLoad (attempts: the §7 best-of-three)"
	if workersPinned || runsPinned {
		pinNote = "pinned by " + EnvHarnessWorkers + "/" + EnvHarnessRuns
	}
	t.Logf("ingestion harness: %d workers, best of %d %.0f req/s with the scrubber, best of %d %.0f req/s without it "+
		"(cost %.1f%%, load_avg_1m %.2f, cpu-scale %.2f, %s; %s)",
		workers, runs, bestWith, runs, without, cost, load, cpuScale, prof, pinNote)
	if _, err := scrub.New(nil, testProjects()); err != nil {
		t.Fatal(err)
	}
	if scrubFence(cpuScale) && !workersPinned && !runsPinned {
		// quiet reference-class host (measured scale 1.0), spec shape: the
		// SPEC-04 §3.9 numbers are directly assertable
		if bestWith < 5000 {
			t.Errorf("ingestion floor: best of %d %.0f req/s with the scrubber in the path, want >= 5000 "+
				"(load_avg_1m=%.2f; SPEC-04 §3.9)", runs, bestWith, load)
		}
		if cost > 20 {
			t.Errorf("scrub cost %.1f%% of this harness on a quiet host, above the §3.9 ≤20%% target "+
				"(the harness omits the sentinel's HTTP/decode/gzip/quota work, so this bound is "+
				"conservative — a quiet-host failure still means the fast paths regressed)", cost)
		}
		return
	}
	// Shared host (or a pinned shape): the loaded floor's host term. The bar is
	// a RATE, so it DIVIDES by the measured scale — a box the pilot measures
	// 1.5x slower than the reference class deserves a two-thirds floor for
	// identical work, the same direction §7a item 1 gives the ledger's
	// scan-rate floor (60 / Scale()). clampedMin1 keeps a faster host honest
	// (a pilot-measured 0.7x does not tighten the 4000 bar) and keeps the
	// divisor away from 0 even if a pinned TROUBLE_HOST_CALIB of 0.01 slips
	// through. The baseline cap is 0.8 x without — the §3.9 ≤20% cost
	// contract applied to the baseline comparison, symmetrical with the
	// quiet branch: best-of-runs with-scrub only has to land within the
	// documented cost of best-of-runs without it. (The previous cap —
	// floor = without, a single baseline draw against the with-side's
	// best-of-three — demanded a zero-cost scrubber whenever a crushed host
	// pushed throughput under the 4000 bar; that gate flipped on sampling
	// noise, measured 1,588-with vs 1,667-without at 4.7% real cost on
	// 2026-09-19, and is the flake class this change exists to close.)
	floor := 4000.0 / clampedMin1(cpuScale) // shared host: still catches the v0.1 red 4.0-4.4k baseline
	if baselineCap := 0.8 * without; baselineCap < floor {
		floor = baselineCap
	}
	if floor < float64(sanityFloor) {
		floor = float64(sanityFloor)
	}
	if bestWith < floor {
		t.Errorf("ingestion floor: best of %d %.0f req/s with the scrubber in the path, want >= %.0f "+
			"(load_avg_1m=%.2f, no-scrub baseline best of %d %.0f req/s; the SPEC-04 §3.9 floor is 5,000 on a quiet host "+
			"and the v0.1 baseline before the per-field gates measured 4,000-4,400 under load)",
			runs, bestWith, floor, load, runs, without)
	} else if cost > 20 {
		t.Logf("NOTE: measured cost is %.1f%% of this harness at load_avg_1m %.2f; the ratio of two "+
			"loaded runs is noise-dominated and the §3.9 ≤20%% bound is asserted on quiet hosts", cost, load)
	}
}

// loadAvgExt was this file's host signal until SPEC-01 §7a (TRBL-052): the
// measured Profile (loadfence.Measure) replaced it — load_avg alone is the
// busy-vs-fast inversion §7a exists to close — and loadfence.LoadAvg1 remains
// the shared implementation of the same read, consumed through the profile's
// load half.
//
// The two helpers below are §7a's classification idioms, kept scrub-local
// (loadfence exports Scale/CPUScale/Contention; the classification lived in
// each consumer so far). They mirror the QA-TROUBLE-5 ledger gates so a
// reader can diff the two packages' fencing line for line.

// scrubFence reports whether a measured scale is the reference class — the
// condition under which the spec numbers are asserted directly. The fence
// sits at 1 because the multiples are best-of-N lower bounds measured on the
// same run they grade: a host even slightly slower than the reference class
// reads 1.0x and must take the scaled branch. It compares the SCALE (not the
// raw multiple) so the load-derived contention participates exactly as it
// does in the budgets themselves.
func scrubFence(scale float64) bool { return scale <= 1.0 }

// clampedMin1 bounds a scale used as a DIVISOR for the rate floors: a faster
// host (multiple below the reference) must not tighten the floor, so the
// divisor never drops below 1 — the same loosening-only rule loadfence's
// clamp1 encodes for multiplicative budgets, on the division side. The guard
// also keeps a pinned TROUBLE_HOST_CALIB of 0.01 from dividing by ~0.
func clampedMin1(scale float64) float64 {
	if scale < 1 || scale != scale { // NaN or below the reference: never tighten a floor
		return 1
	}
	return scale
}
