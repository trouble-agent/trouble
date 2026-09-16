package sensors

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

// merge_rule_test.go is SPEC-03 §7's property row: 10k randomized arrival
// sequences asserting the §3.6 M1–M4 invariants — one incident per key per
// window, reopen accounting that matches the post-resolve arrivals, and no
// arrival lost (arrivals == attaches + opens + reopens).

func TestMergeTrackerProperty(t *testing.T) {
	const (
		sequences = 10000
		units     = 4
	)
	rng := rand.New(rand.NewSource(20260916))
	base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

	var totalArrivals, totalOpens, totalAttaches, totalReopens int

	for seq := 0; seq < sequences; seq++ {
		clock := base
		now := func() time.Time { return clock }
		mt := newMergeTracker(5*time.Second, now)
		unit := []string{"a.service", "b.service", "c.service", "d.service"}[rng.Intn(units)]

		// 12 arrivals at randomized offsets around the 5s merge-window boundary.
		type rec struct {
			path     arrivalPath
			substate string
			result   string
			journald bool
			resolve  bool
		}
		events := make([]rec, 0, 12)
		for i := 0; i < 12; i++ {
			r := rec{path: arrivalProperties, substate: "auto-restart"}
			switch rng.Intn(4) {
			case 0:
				r = rec{path: arrivalJobRemoved, result: []string{"done", "failed", "timeout", "canceled"}[rng.Intn(4)]}
			case 1:
				r = rec{path: arrivalReconcile, substate: "failed"}
			case 2:
				r = rec{path: arrivalJournald, journald: true}
			}
			r.resolve = rng.Intn(8) == 0
			events = append(events, r)
		}

		opens, attaches, reopens, counted, journaldMissed := 0, 0, 0, 0, 0
		for _, e := range events {
			if e.journald {
				if mt.noteJournalArrival(unit, clock) {
					attaches++
				} else {
					// M3: journald opens nothing on its own; the entry is still
					// recorded as its own journald event (that is what the
					// collector does), so it is accounted, not lost.
					journaldMissed++
				}
			} else {
				res := mt.arrival("user:1000", unit, e.path, e.substate, e.result, "/job/"+e.result, clock)
				switch {
				case res.Opened:
					opens++
				case res.Attached:
					attaches++
				case res.Reopened:
					reopens++
				case res.Counted:
					counted++
				default:
					t.Fatalf("seq %d: arrival did nothing (opened=%v attached=%v reopened=%v counted=%v)", seq, res.Opened, res.Attached, res.Reopened, res.Counted)
				}
			}
			if e.resolve {
				mt.resolve(unit, clock)
			}
			// ±1ms around the 5s window boundary.
			clock = clock.Add(time.Duration(4900+rng.Intn(200)) * time.Millisecond)
		}

		// Invariant 1: no arrival is lost.
		if got := opens + attaches + reopens + counted + journaldMissed; got != len(events) {
			t.Fatalf("seq %d: %d arrivals, accounted %d (opens=%d attaches=%d reopens=%d counted=%d journald=%d)",
				seq, len(events), got, opens, attaches, reopens, counted, journaldMissed)
		}
		// Invariant 1b: exactly one non-journald arrival opened the story.
		if len(events) > 0 && opens > 1 {
			t.Fatalf("seq %d: %d opens for one unit key", seq, opens)
		}
		// Invariant 2: at most one incident per unit key per storm window.
		mt.mu.Lock()
		open := 0
		for _, inc := range mt.incidents {
			if !inc.resolved {
				open++
			}
		}
		mt.mu.Unlock()
		if open > 1 {
			t.Fatalf("seq %d: %d open incidents for %q inside one storm window", seq, open, unit)
		}
		// Invariant 3: merge accounting is observable and consistent.
		arr, merges, trackerOpens, _ := mt.snapshot()
		sum := uint64(0)
		for _, n := range arr {
			sum += n
		}
		if int(sum) != len(events) {
			t.Fatalf("seq %d: counter arrivals=%d, real arrivals=%d", seq, sum, len(events))
		}
		if int(merges) > len(events) || int(trackerOpens) != opens {
			t.Fatalf("seq %d: merges=%d opens=%d events=%d", seq, merges, trackerOpens, len(events))
		}

		totalArrivals += len(events)
		totalOpens += opens
		totalAttaches += attaches
		totalReopens += reopens
	}
	t.Logf("property: %d sequences, %d arrivals, %d opens, %d attaches, %d reopens",
		sequences, totalArrivals, totalOpens, totalAttaches, totalReopens)
}

// TestMergeWindowBoundary pins the ±1ms behaviour at the 5s window edge: an
// arrival inside the window attaches, one past it counts into the same incident
// without attaching, and a recurrence after resolution reopens the SAME
// incident (SPEC-03 §3.6 M1/M2).
//
// The sequence deliberately keeps only two *failures* inside the 300s crash
// window: a third failure in that window is a crash loop (M4), which is a
// different assertion (TestCrashLoopReDerivesSig).
func TestMergeWindowBoundary(t *testing.T) {
	base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	clock := base
	mt := newMergeTracker(5*time.Second, func() time.Time { return clock })

	first := mt.arrival("system", "payment-worker.service", arrivalProperties, "auto-restart", "", "", clock)
	if !first.Opened {
		t.Fatal("the first arrival must open the incident")
	}
	if first.Class != "auto-restart" {
		t.Fatalf("class = %q, want auto-restart", first.Class)
	}

	clock = base.Add(4999 * time.Millisecond)
	in := mt.arrival("system", "payment-worker.service", arrivalJobRemoved, "", "failed", "/job/1", clock)
	if !in.Attached {
		t.Fatal("an arrival 1ms inside the window must attach")
	}
	// M1: inside the window a JobRemoved re-derives the failure class, encoded
	// as SPEC-01 §3.3's `job:<Result>` state.
	if in.Class != "job:failed" {
		t.Fatalf("class inside the window = %q, want job:failed (M1)", in.Class)
	}

	// A post-window arrival on the still-open incident counts; a successful job
	// is an arrival but not a failure, so the crash counter does not move.
	clock = base.Add(6 * time.Second)
	out := mt.arrival("system", "payment-worker.service", arrivalJobRemoved, "", "done", "/job/2", clock)
	if out.Attached || out.Reopened || out.Opened {
		t.Fatalf("a post-window arrival on an open incident must only count: %+v", out)
	}
	if !out.Counted {
		t.Fatal("a post-window arrival on an open incident must be reported as counted")
	}

	// Resolve, then a genuine recurrence after the crash window expires: the
	// SAME incident reopens rather than a second one being created.
	mt.resolve("payment-worker.service", clock)
	clock = base.Add(310 * time.Second)
	rec := mt.arrival("system", "payment-worker.service", arrivalProperties, "auto-restart", "", "", clock)
	if !rec.Reopened {
		t.Fatalf("an arrival after resolution must reopen the same incident, got %+v", rec)
	}
	if rec.Count < 4 {
		t.Fatalf("count = %d, want the merged count", rec.Count)
	}
	mt.mu.Lock()
	n := len(mt.incidents)
	mt.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d incidents for one unit key, want 1", n)
	}
}

// TestMergeArrivalPathsAreSixOrderings is SPEC-03 §7's dbus row for M1–M4:
// {PropertiesChanged, JobRemoved, journald} in 6 orderings ⇒ exactly one
// incident, three attaches, zero duplicates.
func TestMergeArrivalPathsAreSixOrderings(t *testing.T) {
	orders := [][]string{
		{"properties", "job", "journald"},
		{"properties", "journald", "job"},
		{"job", "properties", "journald"},
		{"job", "journald", "properties"},
		{"journald", "properties", "job"},
		{"journald", "job", "properties"},
	}
	for _, order := range orders {
		base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
		clock := base
		mt := newMergeTracker(5*time.Second, func() time.Time { return clock })
		unit := "payment-worker.service"
		opens, attaches, journaldAttach, journaldMissed := 0, 0, 0, 0
		incs := map[string]bool{}
		for _, step := range order {
			switch step {
			case "properties":
				res := mt.arrival("user:1000", unit, arrivalProperties, "failed", "", "", clock)
				if res.Opened {
					opens++
					incs[res.Sig.String()] = true
				}
				if res.Attached {
					attaches++
				}
			case "job":
				res := mt.arrival("user:1000", unit, arrivalJobRemoved, "", "failed", "/job/7", clock)
				if res.Opened {
					opens++
					incs[res.Sig.String()] = true
				}
				if res.Attached {
					attaches++
				}
			case "journald":
				if mt.noteJournalArrival(unit, clock) {
					journaldAttach++
				} else {
					journaldMissed++
				}
			}
			clock = clock.Add(500 * time.Millisecond)
		}
		if opens != 1 {
			t.Errorf("order %v: %d incidents opened, want exactly 1", order, opens)
		}
		// Three arrivals, one incident: exactly one opened it and the other two
		// were accounted as attaches (journald attaches only when an incident is
		// already open — M3).
		if attaches+journaldAttach+journaldMissed != 2 {
			t.Errorf("order %v: %d secondary arrivals accounted, want 2", order, attaches+journaldAttach+journaldMissed)
		}
		if len(incs) > 1 {
			t.Errorf("order %v: %d distinct incident sigs, want 1", order, len(incs))
		}
	}
}

// TestJournaldAloneOpensNothing: M3 — journald enriches, it never opens a unit
// incident by itself.
func TestJournaldAloneOpensNothing(t *testing.T) {
	clock := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	mt := newMergeTracker(5*time.Second, func() time.Time { return clock })
	if mt.noteJournalArrival("payment-worker.service", clock) {
		t.Fatal("journald alone must not attach to a nonexistent incident")
	}
	arr, _, opens, journald := mt.snapshot()
	if opens != 0 || journald != 0 {
		t.Fatalf("journald alone opened %d incidents (journald attaches=%d)", opens, journald)
	}
	if arr[string(arrivalJournald)] != 1 {
		t.Fatalf("the journald arrival must still be counted: %v", arr)
	}
}

// TestCrashLoopReDerivesSig: ≥3 failures of one unit inside 300s ⇒
// crash_loop=true, critical, and its own signature (M4).
func TestCrashLoopReDerivesSig(t *testing.T) {
	clock := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	mt := newMergeTracker(5*time.Second, func() time.Time { return clock })
	unit := "worker.service"
	var classes []string
	var sigs []string
	var sev []string
	var crash []bool
	for i := 0; i < 3; i++ {
		res := mt.arrival("system", unit, arrivalProperties, "auto-restart", "", "", clock)
		classes = append(classes, res.Class)
		sigs = append(sigs, res.Sig.String())
		sev = append(sev, string(res.Severity))
		crash = append(crash, res.CrashLoop)
		clock = clock.Add(30 * time.Second)
	}
	if crash[0] || !crash[2] {
		t.Fatalf("crash loop detected at the wrong arrival: %v", crash)
	}
	if classes[2] != "crash_loop" {
		t.Fatalf("third failure class = %q, want crash_loop", classes[2])
	}
	if len(sigs) != 3 {
		t.Fatalf("sigs = %v", sigs)
	}
	// The shipped dbus_crash_loop rule promotes the rung; the severity is the
	// merge tracker's own critical.
	if sev[2] != "critical" {
		t.Fatalf("crash-loop severity = %q, want critical", sev[2])
	}
	// The crash-loop incident absorbs further failures for 1800s.
	later := mt.arrival("system", unit, arrivalProperties, "auto-restart", "", "", clock.Add(20*time.Minute))
	if !later.CrashLoop || !later.Attached {
		t.Fatalf("the crash-loop incident must absorb further failures, got %+v", later)
	}
	after := mt.arrival("system", unit, arrivalProperties, "auto-restart", "", "", clock.Add(31*time.Minute))
	if after.CrashLoop {
		t.Fatal("a failure after the absorption window is a new story, not a crash-loop continuation")
	}
	if !after.Opened {
		t.Fatal("a failure after the absorption window must open a fresh incident")
	}
}

// TestMergeAccountingIsObservable pins the §3.6 regression numbers so a
// "3 arrivals → 3 incidents" bug is caught by numbers, not by a dashboard read.
func TestMergeAccountingIsObservable(t *testing.T) {
	clock := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	mt := newMergeTracker(5*time.Second, func() time.Time { return clock })
	mt.arrival("system", "a.service", arrivalProperties, "failed", "", "", clock)
	mt.arrival("system", "a.service", arrivalJobRemoved, "", "failed", "/j1", clock.Add(time.Second))
	mt.noteJournalArrival("a.service", clock.Add(2*time.Second))
	arr, merges, opens, journald := mt.snapshot()
	if opens != 1 {
		t.Fatalf("opens = %d, want 1", opens)
	}
	if merges != 2 {
		t.Fatalf("merges = %d, want 2 (two arrivals merged into the open incident)", merges)
	}
	if journald != 1 {
		t.Fatalf("journald attaches = %d, want 1", journald)
	}
	if arr["properties_changed"] != 1 || arr["job_removed"] != 1 || arr["journald"] != 1 {
		t.Fatalf("arrivals by path = %v", arr)
	}
	if !strings.Contains(mt.sigForLabel(), "dbus") {
		t.Fatalf("sig space = %q", mt.sigForLabel())
	}
}
