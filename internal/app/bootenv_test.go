package app

// bootenv_test.go — QA-TROUBLE-14: the two environment-dependent inputs a
// first-run boot test cannot control through its own fixtures, neutralized the
// same way readybudget_test.go neutralizes host speed: by removing the
// environment's ability to decide the outcome, never by weakening an
// assertion.
//
// The two sources of environment dependence:
//
//   - lifecycle.self_rss_warn (default and shipped-example value 80MB): the
//     health assembly degrades the whole boot when the test process's own RSS
//     exceeds it (internal/lifecycle/health.go). RSS is a property of the HOST
//     and of the test binary's heap, not of the boot: on a cold JIT box a Go
//     test process that ran the whole package before this boot measures
//     ~135MB and an absent-rules boot that is healthy by SPEC-03 §3.7b serves
//     status=degraded with detail.rss_over_budget=true. The number is
//     unassertable as a posture gate from inside a process whose footprint the
//     test does not control, so first-run boot tests raise the budget over the
//     process footprint via the ordinary config ladder (env outranks the
//     shipped file) instead. The RSS gate itself stays tested at its real
//     boundary by internal/lifecycle's own tests.
//
//   - sensors.dbus.enabled: on a headless host the boot reconcile's first
//     ListUnits sights pre-existing failed USER units. The TRBL-094 boot
//     stabilizer holds and paces them (§3.8a), but the §3.8 breaker window is
//     wall-clock: under host load a paced admission can still land 21+
//     incidents inside the boot test's READY window and trip
//     rule:dbus_unit_failed. D-Bus watching is not what any of these tests
//     assert, so the sensor is disabled by configuration for the boot — the
//     designed, non-degrading posture internal/sensors/dbus.go serves for
//     enabled=false ("disabled by configuration") — and the DBus boot
//     inventory/storm behaviour stays tested by internal/sensors' own
//     dbus_boot tests with synthetic unit sets.
//
// Both overrides arrive through BootOptions.Env (the test's own controlled
// environ — the tests already pass Env: []string{}), so they win the ordinary
// flag > env > file > default precedence without touching the shipped file's
// bytes and without any production change. Neither deletes or loosens an
// assertion: status/sensor/subsystem assertions stay exactly as they were; the
// overrides only remove inputs the tests never meant to grade.

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/lifecycle"
)

// The neutralizer rows first-run boot tests append to their BootOptions.Env.
const (
	// bootTestRSSWarnEnv raises lifecycle.self_rss_warn far above any Go test
	// process footprint, so rss_over_budget can only fire if the boot itself
	// leaks catastrophically — the posture the tests actually pin.
	bootTestRSSWarnEnv = "TROUBLE_LIFECYCLE_SELF_RSS_WARN=4GB"
	// bootTestDBusOffEnv disables the dbus sensor: a disabled sensor is
	// enabled=false, degraded=false ("disabled by configuration") and never
	// contributes a degraded sensor or sensor_degraded detail.
	bootTestDBusOffEnv = "TROUBLE_SENSORS_DBUS_ENABLED=false"
	// bootTestTimersOffEnv disables the timers sensor for the same reason: it
	// depends on dbus ("dependency dbus degraded", internal/sensors/disk.go),
	// so with dbus watching off a live timers sensor would carry the
	// degradation forward. Disabled = enabled=false, degraded=false.
	bootTestTimersOffEnv = "TROUBLE_SENSORS_TIMERS_ENABLED=false"
)

// bootEnvWithNeutralizers returns env with the two neutralizer rows appended.
// The resolver applies precedence per key, so an appended row outranks the
// shipped file's own value of the same key.
func bootEnvWithNeutralizers(env []string) []string {
	out := make([]string, 0, len(env)+3)
	out = append(out, env...)
	return append(out, bootTestRSSWarnEnv, bootTestDBusOffEnv, bootTestTimersOffEnv)
}

// TestBootNeutralizerEnvResolves pins the neutralizer rows against the real
// resolver: both names must be KNOWN registry keys resolving with source=env
// to the intended values — and, against the shipped example as the config
// file, must win over the file's own lifecycle.self_rss_warn = "80MB". A
// registry rename that orphans one of the names fails here (unknown env) or
// on the value assertion below, instead of silently re-exposing the first-run
// boot tests to host RSS and host D-Bus state.
func TestBootNeutralizerEnvResolves(t *testing.T) {
	for _, cfgPath := range []string{"", shippedExampleConfigPath(t)} {
		res, err := lifecycle.Resolve(nil, bootEnvWithNeutralizers(nil), cfgPath)
		if err != nil {
			t.Fatalf("Resolve(nil, neutralizer env, %q): %v", cfgPath, err)
		}
		if len(res.UnknownEnv) != 0 {
			t.Errorf("config %q: the neutralizer rows produced %d unknown-env rows (%+v): a TROUBLE_* name below no longer addresses a registry key, so first-run boot tests would silently re-inherit the host noise", cfgPath, len(res.UnknownEnv), res.UnknownEnv)
		}
		var rss, dbus, timers bool
		for _, cv := range res.Values {
			switch cv.Key {
			case "lifecycle.self_rss_warn":
				rss = true
				if cv.Source != "env" || cv.Value != "4GB" {
					t.Errorf("config %q: lifecycle.self_rss_warn resolved from %s = %#v, want source=env value 4GB (the env row must beat the file's 80MB)", cfgPath, cv.Source, cv.Value)
				}
			case "sensors.dbus.enabled":
				dbus = true
				if cv.Source != "env" || cv.Value != false {
					t.Errorf("config %q: sensors.dbus.enabled resolved from %s = %#v, want source=env value false", cfgPath, cv.Source, cv.Value)
				}
			case "sensors.timers.enabled":
				timers = true
				if cv.Source != "env" || cv.Value != false {
					t.Errorf("config %q: sensors.timers.enabled resolved from %s = %#v, want source=env value false", cfgPath, cv.Source, cv.Value)
				}
			}
		}
		if !rss {
			t.Errorf("config %q: resolved values carry no lifecycle.self_rss_warn row", cfgPath)
		}
		if !dbus {
			t.Errorf("config %q: resolved values carry no sensors.dbus.enabled row", cfgPath)
		}
		if !timers {
			t.Errorf("config %q: resolved values carry no sensors.timers.enabled row", cfgPath)
		}
	}
}
