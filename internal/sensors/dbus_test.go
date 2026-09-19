package sensors

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dbus_test.go covers SPEC-03 §7's dbus row: the path-escaping golden and its
// round-trip, the manager resolution and the degradation codes that must name
// the manager.

// sigForLabel is a small helper for the accounting test: it renders the sig of
// the incident the tracker would produce.
func (m *mergeTracker) sigForLabel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inc := range m.incidents {
		return m.sigFor(inc).String()
	}
	return ""
}

// TestEscapeUnitPathGolden pins the measured transform.
func TestEscapeUnitPathGolden(t *testing.T) {
	got := escapeUnitPath("coding-hermes-scheduler.service")
	want := "coding_2dhermes_2dscheduler_2eservice"
	if got != want {
		t.Fatalf("escapeUnitPath = %q, want %q", got, want)
	}
	if got := escapeUnitPath("plain_underscore.service"); got != "plain_5funderscore_2eservice" {
		t.Fatalf("underscore must escape as _5f so the transform is reversible, got %q", got)
	}
	if got := unitObjectPath("coding-hermes-scheduler.service"); got != "/org/freedesktop/systemd1/unit/coding_2dhermes_2dscheduler_2eservice" {
		t.Fatalf("object path = %q", got)
	}
}

// TestEscapeUnitPathRoundTrip generates 10k names and asserts the transform is
// invertible for every one of them.
func TestEscapeUnitPathRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_@.:+~!$%^&*()[]{}'\"\\/ \t")
	for i := 0; i < 10000; i++ {
		n := 1 + rng.Intn(40)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		name := string(b)
		esc := escapeUnitPath(name)
		if unesc := unescapeUnitPath(esc); unesc != name {
			t.Fatalf("round-trip failed: %q -> %q -> %q", name, esc, unesc)
		}
		// The escaped form must be a legal systemd object-path element.
		for k := 0; k < len(esc); k++ {
			c := esc[k]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				t.Fatalf("escaped form %q of %q contains an illegal byte %q", esc, name, string(c))
			}
		}
	}
}

// TestBase64LikeHashIsNotUnescapedTwice: escape/unescape must not mangle the
// common `x@1.2.3.service` and `dev-disk-by\x2duuid` shapes.
func TestEscapeRealWorldNames(t *testing.T) {
	cases := map[string]string{
		"troubled.service":                "troubled_2eservice",
		"user@1000.service":               "user_401000_2eservice",
		"dev-disk-by\\x2duuid-abc.device": "dev_2ddisk_2dby_5cx2duuid_2dabc_2edevice",
	}
	for name, want := range cases {
		if got := escapeUnitPath(name); got != want {
			t.Errorf("escapeUnitPath(%q) = %q, want %q", name, got, want)
		}
		if got := unescapeUnitPath(want); got != name {
			t.Errorf("unescapeUnitPath(%q) = %q, want %q", want, got, name)
		}
	}
}

// TestResolveManagers: "self" resolves to the daemon's own uid, uid forms are
// accepted, the system manager is always present, and the list is bounded.
func TestResolveManagers(t *testing.T) {
	// The fixture is derived from the RUNNING uid so the test holds on every
	// host: "self", user:<uid> and uid:<uid> are three spellings of one
	// identity, and that same-uid collapse must be exercised wherever the
	// process runs (root containers, service users, CI — not always 1000).
	self := os.Getuid()
	h := newHarness(t,
		types.ConfigValue{Key: "sensors.dbus.user_managers", Value: []string{
			"self", fmt.Sprintf("user:%d", self), fmt.Sprintf("uid:%d", self), "bogus",
		}},
	)
	got := h.s.resolveManagers()
	if got[0] != "system" {
		t.Fatalf("the system manager must always be watched first: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("managers = %v, want system + one deduped uid", got)
	}
	if !strings.HasPrefix(got[1], "user:") {
		t.Fatalf("second manager = %q", got[1])
	}
	if want := fmt.Sprintf("user:%d", self); got[1] != want {
		t.Fatalf("self must resolve to the daemon's own uid: managers = %v, want %q", got, want)
	}

	// The other half of the contract: a uid that is NOT self must survive dedup
	// even while the duplicate spellings of the self uid collapse, so the
	// assertion is meaningful on any host and not merely green.
	other := self + 1
	h2 := newHarness(t, types.ConfigValue{Key: "sensors.dbus.user_managers", Value: []string{
		fmt.Sprintf("user:%d", self), fmt.Sprintf("uid:%d", self),
		fmt.Sprintf("uid:%d", other), fmt.Sprintf("user:%d", other),
	}})
	got2 := h2.s.resolveManagers()
	if len(got2) != 3 {
		t.Fatalf("managers = %v, want system + self + one distinct other uid", got2)
	}
	seen := map[string]bool{}
	for _, m := range got2 {
		if seen[m] {
			t.Fatalf("managers = %v, duplicate entry %q survived dedup", got2, m)
		}
		seen[m] = true
	}
	if !seen[fmt.Sprintf("user:%d", self)] || !seen[fmt.Sprintf("user:%d", other)] {
		t.Fatalf("managers = %v, want both user:%d and user:%d", got2, self, other)
	}

	// Bounded at 16 uids.
	many := []string{}
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("uid:%d", 2000+i))
	}
	h3 := newHarness(t, types.ConfigValue{Key: "sensors.dbus.user_managers", Value: many})
	if n := len(h3.s.resolveManagers()); n > dbusMaxUserMgrs+1 {
		t.Fatalf("resolved %d managers, cap is %d uids + system", n, dbusMaxUserMgrs)
	}
}

// TestUnwatchedManagerIsNamed: a configured-but-unwatched manager must produce
// TROUBLE-SENSORS-013 with the manager identity, never a silent skip.
func TestUnwatchedManagerIsNamed(t *testing.T) {
	h := newHarness(t, types.ConfigValue{
		Key: "sensors.dbus.user_managers", Value: []string{"uid:4294967"}, // almost certainly nonexistent
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.s.startDBus(ctx)
	h.s.stopDBus()
	// Whether or not the system bus is reachable on this host, the unwatched
	// manager must appear in the health reason or in a record.
	reason := ""
	if rt := h.s.rt[types.SenDBus]; rt != nil && rt.reason.Load() != nil {
		reason = *rt.reason.Load()
	}
	found := strings.Contains(reason, "user:4294967")
	for _, r := range h.snapshot() {
		if code, _ := r.Payload["error_code"].(string); code == string(types.CodeSensors013) {
			if d, _ := r.Payload["detail"].(string); strings.Contains(d, "user:4294967") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("a configured-but-unwatched manager must be named; reason=%q err=%v", reason, err)
	}
}

// TestDBusPathEscapingForSignals: PropertiesChanged is addressed by object
// path, so the escaping must round-trip through the signal's path back to the
// visible unit name — which is what the dedup key uses.
func TestSignalPathToUnitName(t *testing.T) {
	unit := "coding-hermes-scheduler.service"
	path := unitObjectPath(unit)
	got := unescapeUnitPath(strings.TrimPrefix(string(path), "/org/freedesktop/systemd1/unit/"))
	if got != unit {
		t.Fatalf("signal path resolved to %q, want %q (dedup keys must use the visible name)", got, unit)
	}
}

// TestProbeDBusManagersRecordsEveryManager: the probe record lists the managers
// it intended to watch with connect/subscribe outcomes.
func TestProbeDBusManagersRecordsEveryManager(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries := h.s.probeDBusManagers(ctx)
	if len(entries) < 1 {
		t.Fatal("the probe must record at least the system manager")
	}
	for _, e := range entries {
		if _, ok := e["bus"]; !ok {
			t.Errorf("manager entry without an identity: %v", e)
		}
		if _, ok := e["connect"]; !ok {
			t.Errorf("manager entry without a connect outcome: %v", e)
		}
		if _, ok := e["subscribe"]; !ok {
			t.Errorf("manager entry without a subscribe outcome: %v", e)
		}
	}
	t.Logf("probe dbus managers: %v", entries)
}

// TestDBusOutcomeEmitsMergeAccounting: one merged failure becomes one event
// record carrying the accounting numbers.
func TestDBusOutcomeEmitsMergeAccounting(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "dbus_unit_failed"
source = "dbus"
[[rule.match]]
field = "unit_substate"
op = "=="
value = "failed"
value_type = "string"
`)
	h.mustReload()
	ctx := context.Background()
	h.s.dbState.merge = newMergeTracker(5*time.Second, h.s.now)
	res := h.s.dbState.merge.arrival("system", "payment-worker.service", arrivalProperties, "failed", "", "", h.now())
	h.s.emitDBusOutcome(ctx, res, "system", "failed", "failed", arrivalProperties)
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected one record, got %d", len(recs))
	}
	if recs[0].Origin.Source != string(types.SrcDBus) {
		t.Fatalf("origin.source = %q", recs[0].Origin.Source)
	}
	if recs[0].Sig == "" {
		t.Fatal("an event record needs a sig")
	}
	if recs[0].Payload["fire"] != true {
		t.Fatal("a matching unit failure must fire")
	}
	if recs[0].Payload["subject"] != "payment-worker.service" {
		t.Fatalf("subject = %v (the merge key rides on it)", recs[0].Payload["subject"])
	}
	detail, _ := recs[0].Payload["detail"].(map[string]any)
	if detail["crash_loop"] != false {
		t.Fatalf("crash_loop = %v", detail["crash_loop"])
	}
	if detail["unit_substate"] != "failed" {
		t.Fatalf("unit_substate = %v", detail["unit_substate"])
	}
}

// TestReconcilePicksUpPreExistingFailures: the startup ListUnits reconcile is
// what makes a failure that happened before this daemon started visible at all.
func TestReconcilePicksUpPreExistingFailures(t *testing.T) {
	mt := newMergeTracker(5*time.Second, time.Now)
	clock := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	res := mt.arrival("system", "old-worker.service", arrivalReconcile, "failed", "", "", clock)
	if !res.Opened {
		t.Fatal("a reconcile arrival for a failed unit must open an incident")
	}
	if res.Sig.Source != types.SrcDBus {
		t.Fatalf("sig source = %s", res.Sig.Source)
	}
}
