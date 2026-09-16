package sensors

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// perf_test.go covers SPEC-03 §7's perf row: 256 rules across 6 sensors,
// ≤2ms/event rule evaluation (p99), ≥5000 events/min sustained through
// normalize→scrub→dedup→ledger and a bounded RSS contribution.

// TestRuleEvaluationBudget256Rules measures one event's full evaluation against
// a 256-rule set (all six sources) and asserts the p99.
func TestRuleEvaluationBudget256Rules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 256-rule evaluation budget in -short")
	}
	h := newHarness(t)
	sources := []string{"psi", "journald", "dbus", "disk", "timers", "inotify"}
	fields := map[string]string{
		"psi": "some_avg10", "journald": "priority", "dbus": "exit_code",
		"disk": "free_pct", "timers": "missed_runs", "inotify": "overflow",
	}
	ops := map[string]string{"inotify": "==", "journald": ">=", "dbus": ">=", "disk": ">=", "timers": ">=", "psi": ">="}
	types_ := map[string]string{"inotify": "bool"}
	var b strings.Builder
	for i := 0; i < 256; i++ {
		src := sources[i%len(sources)]
		vt := types_[src]
		val := "1"
		if vt == "" {
			vt = "number"
		}
		if src == "inotify" {
			val = "false"
		}
		fmt.Fprintf(&b, "[[rule]]\nname = \"%s_%04d\"\nsource = \"%s\"\nfor = \"10s\"\n[[rule.match]]\nfield = \"%s\"\nop = \"%s\"\nvalue = \"%s\"\nvalue_type = \"%s\"\n",
			src, i, src, fields[src], ops[src], val, vt)
	}
	h.writeRules("10.toml", b.String())
	h.mustReload()
	if got := h.s.ruleCount(); got != 256 {
		t.Fatalf("loaded %d rules, want 256", got)
	}
	const n = 2000
	lat := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(ev.Sensor.SigSource(), "io", "some", fmt.Sprintf("perf-%d", i))
		start := time.Now()
		h.s.handleEvent(context.Background(), ev)
		lat = append(lat, time.Since(start))
	}
	sortDurations(lat)
	p50 := lat[len(lat)/2]
	p99 := lat[(len(lat)*99)/100]
	if p99 > 2*time.Millisecond {
		t.Fatalf("p99 rule evaluation = %s, budget is 2ms (256 rules)", p99)
	}
	t.Logf("measured: rule evaluation p50 %s, p99 %s over %d events with 256 rules", p50, p99, n)
}

// TestSustainedEventThroughput asserts ≥5000 events/min through the pipeline.
func TestSustainedEventThroughput(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "throughput"
source = "psi"
for = "0s"
cooldown = "0s"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
`)
	h.mustReload()
	ctx := context.Background()
	const target = 5000 // events per minute
	start := time.Now()
	for i := 0; i < target; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(ev.Sensor.SigSource(), "io", "some", fmt.Sprintf("tput-%d", i))
		h.s.handleEvent(ctx, ev)
	}
	elapsed := time.Since(start)
	perMin := float64(target) / elapsed.Minutes()
	if perMin < target {
		t.Fatalf("sustained throughput = %.0f events/min, floor is %d events/min", perMin, target)
	}
	if got := len(h.snapshot()); got != target {
		t.Fatalf("%d records emitted for %d events", got, target)
	}
	t.Logf("measured: %.0f events/min sustained through normalize→rules→emit (%d events in %s)",
		perMin, target, elapsed.Round(time.Millisecond))
}

// TestRSSContributionIsBounded measures the sensor subsystem's own memory
// contribution, not the test binary's.
func TestRSSContributionIsBounded(t *testing.T) {
	h := newHarness(t)
	// The harness must not retain what it measures.
	h.discard = true
	var b strings.Builder
	for i := 0; i < 256; i++ {
		b.WriteString(ruleBody(fmt.Sprintf("rss_%04d", i), ">=", "35", "10s"))
	}
	h.writeRules("10.toml", b.String())
	h.mustReload()
	runtime.GC()
	before := rssBytes()
	if before == 0 {
		t.Skip("/proc/self/statm unavailable")
	}
	ctx := context.Background()
	for i := 0; i < 20000; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(ev.Sensor.SigSource(), "io", "some", fmt.Sprintf("rss-%d", i))
		h.s.handleEvent(ctx, ev)
	}
	runtime.GC()
	after := rssBytes()
	delta := after - before
	const budget = 12 << 20
	if delta > budget {
		t.Fatalf("sensors contributed %d bytes of RSS (%d -> %d), budget is %d", delta, before, after, budget)
	}
	t.Logf("measured: sensors RSS contribution %d bytes after 20k events with 256 rules (budget %d)", delta, budget)
}

func rssBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
