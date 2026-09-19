package sensors

// config_test.go pins the two halves of the sensor config contract that
// TRBL-036 turned into a registry surface (SPEC-12 §3.1e):
//
//  1. the COMPILED DEFAULTS of the detection plane are the numbers SPEC-03 §4
//     (and §3.8a) state. internal/lifecycle's registry now carries the same
//     numbers as the resolvable defaults, so a host with no `[sensors]` table
//     runs this posture — and a drift on either side fails a test on that side
//     instead of silently changing what a stock boot does.
//
//  2. every reader this decoder uses accepts BOTH forms a resolved
//     `types.ConfigValue` can carry: the TYPED form the registry emits for a
//     default row (`types.Duration`, `bool`, `int`, `[]string`, the array of
//     tables) and the raw/string forms a file, an environment variable or a
//     flag produces (`string`, `int64`, `[]any`, `[]map[string]any`). Before
//     TRBL-036 only the fold window ever arrived from the registry, so only
//     that one path was exercised; the array-of-tables form of
//     `sensors.inotify.paths` in particular used to be an unmatched type.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// decodeFor folds values over the compiled defaults exactly as New does, with
// the one required identity key supplied.
func decodeFor(t *testing.T, vals ...types.ConfigValue) sensorConfig {
	t.Helper()
	base := []types.ConfigValue{{Key: "origin.host_id", Value: "testhost"}}
	cfg, err := decodeConfig(append(base, vals...))
	if err != nil {
		t.Fatalf("decodeConfig(%v) = %v", vals, err)
	}
	return cfg
}

// TestSensorConfigDefaultsAreTheSpecNumbers is the defaults pin: the compiled
// posture the registry mirrors, field by field. Every value here is also
// asserted against internal/lifecycle's registry default
// (internal/lifecycle/sensors_registry_test.go), so the two sides cannot drift
// apart without one of the two tests failing.
func TestSensorConfigDefaultsAreTheSpecNumbers(t *testing.T) {
	cfg := defaultConfig()

	if cfg.psi.enabled != true || cfg.psi.sampleInterval != 2*time.Second || cfg.psi.window != 2*time.Second {
		t.Errorf("psi defaults = (%v, %v, %v), want (true, 2s, 2s)", cfg.psi.enabled, cfg.psi.sampleInterval, cfg.psi.window)
	}
	if cfg.journald.enabled != true || cfg.journald.followAll != false ||
		cfg.journald.followAllMaxEntries != 2000000 || cfg.journald.queue != 8192 ||
		cfg.journald.queueBytes != 32<<20 || cfg.journald.maxEntry != 65536 ||
		cfg.journald.probeInterval != 30*time.Second || len(cfg.journald.units) != 0 {
		t.Errorf("journald defaults = %+v, want (true, [], false, 2000000, 8192, %d, 65536, 30s)", cfg.journald, 32<<20)
	}
	if cfg.dbus.enabled != true || len(cfg.dbus.userManagers) != 1 || cfg.dbus.userManagers[0] != "self" ||
		cfg.dbus.pingInterval != 30*time.Second || cfg.dbus.reconcileInterval != 300*time.Second ||
		cfg.dbus.oomdProbeInterval != 10*time.Minute {
		t.Errorf("dbus defaults = %+v, want (true, [self], 30s, 300s, 10m)", cfg.dbus)
	}
	if cfg.disk.enabled != true || len(cfg.disk.mounts) != 1 || cfg.disk.mounts[0] != "/" || cfg.disk.interval != 60*time.Second {
		t.Errorf("disk defaults = %+v, want (true, [/], 60s)", cfg.disk)
	}
	if cfg.timers.enabled != true || cfg.timers.interval != 60*time.Second {
		t.Errorf("timers defaults = %+v, want (true, 60s)", cfg.timers)
	}
	if cfg.inotify.enabled != true || len(cfg.inotify.paths) != 0 ||
		cfg.inotify.recheckInterval != 900*time.Second || cfg.inotify.maxDepth != 8 {
		t.Errorf("inotify defaults = %+v, want (true, [], 900s, 8)", cfg.inotify)
	}
	wantRulesDir := filepath.Join(filepath.Dir(defaultStateRoot()), "config", "rules.d")
	if cfg.rulesDir != wantRulesDir || cfg.reloadDebounce != 500*time.Millisecond {
		t.Errorf("rules defaults = (%q, %v), want (%q, 500ms)", cfg.rulesDir, cfg.reloadDebounce, wantRulesDir)
	}
	if cfg.mergeWindow != 5*time.Second || cfg.sampleFoldWindow != 5*time.Minute {
		t.Errorf("window defaults = (%v, %v), want (5s, 5m)", cfg.mergeWindow, cfg.sampleFoldWindow)
	}
	if cfg.limits.rulePerMin != 120 || cfg.limits.sourcePerMin != 600 ||
		cfg.limits.globalPerMin != 1200 || cfg.limits.incidentsPer5m != 25 {
		t.Errorf("limits defaults = %+v, want (120, 600, 1200, 25)", cfg.limits)
	}
}

// TestSensorConfigAcceptsTheResolvedForms: every reader, driven with the TYPED
// form a registry default row carries and with the raw form a file, an
// environment variable or a flag carries. The typed half is what TRBL-009 paid
// for once (a `types.Duration` default row was an unmatched type); the
// array-of-tables half is what TRBL-036 paid for (a `[]map[string]any` row from
// `[[sensors.inotify.paths]]` was refused outright).
func TestSensorConfigAcceptsTheResolvedForms(t *testing.T) {
	t.Run("bool: bool (default row) and string (flag/env/file)", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.psi.enabled", Value: false}); got.psi.enabled {
			t.Error("bool form: a false default row left psi.enabled true")
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.journald.follow_all", Value: "true"}); !got.journald.followAll {
			t.Error("string form: \"true\" did not enable journald.follow_all")
		}
	})

	t.Run("duration: types.Duration (default row), string, and nanosecond ints", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: types.Duration("90s")}); got.sampleFoldWindow != 90*time.Second {
			t.Errorf("typed form: fold window = %v, want 90s", got.sampleFoldWindow)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.psi.sample_interval", Value: "3s"}); got.psi.sampleInterval != 3*time.Second {
			t.Errorf("string form: psi sample interval = %v, want 3s", got.psi.sampleInterval)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.dbus.ping_interval", Value: int64(45 * time.Second)}); got.dbus.pingInterval != 45*time.Second {
			t.Errorf("nanosecond form: dbus ping interval = %v, want 45s", got.dbus.pingInterval)
		}
	})

	t.Run("int: int (default row), int64 (file) and string (flag/env)", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.journald.queue", Value: 4096}); got.journald.queue != 4096 {
			t.Errorf("typed form: queue = %d, want 4096", got.journald.queue)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.journald.queue_bytes", Value: int64(1 << 20)}); got.journald.queueBytes != 1<<20 {
			t.Errorf("int64 form: queue_bytes = %d, want %d", got.journald.queueBytes, 1<<20)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.inotify.max_depth", Value: "3"}); got.inotify.maxDepth != 3 {
			t.Errorf("string form: max_depth = %d, want 3", got.inotify.maxDepth)
		}
	})

	t.Run("string: plain and typed", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.rules.dir", Value: "/srv/rules.d"}); got.rulesDir != "/srv/rules.d" {
			t.Errorf("rules dir = %q, want /srv/rules.d", got.rulesDir)
		}
	})

	t.Run("string slice: []string (default/env-coerced), []any (file) and string (flag)", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.disk.mounts", Value: []string{"/srv", "/var"}}); len(got.disk.mounts) != 2 || got.disk.mounts[1] != "/var" {
			t.Errorf("[]string form: mounts = %v, want [/srv /var]", got.disk.mounts)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.journald.units", Value: []any{"a.service", "b.service"}}); len(got.journald.units) != 2 {
			t.Errorf("[]any form: units = %v, want two entries", got.journald.units)
		}
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.dbus.user_managers", Value: "self,1000"}); len(got.dbus.userManagers) != 2 {
			t.Errorf("string form: user_managers = %v, want two entries", got.dbus.userManagers)
		}
	})

	t.Run("inotify paths: the array-of-tables form the file reader produces", func(t *testing.T) {
		rows := []map[string]any{
			{"path": "/etc", "recursive": true, "max_depth": int64(3)},
			{"path": "/srv", "mask": "IN_MODIFY", "rule": "svc"},
		}
		got := decodeFor(t, types.ConfigValue{Key: "sensors.inotify.paths", Value: rows})
		if len(got.inotify.paths) != 2 {
			t.Fatalf("paths = %+v, want the two declared rows", got.inotify.paths)
		}
		if got.inotify.paths[0].path != "/etc" || !got.inotify.paths[0].recursive || got.inotify.paths[0].maxDepth != 3 {
			t.Errorf("first row = %+v, want (/etc, recursive, depth 3)", got.inotify.paths[0])
		}
		// The fields a row omits keep the collector's defaults, exactly as a
		// table that spelled them out would.
		if got.inotify.paths[1].mask == "" || got.inotify.paths[1].rule != "svc" {
			t.Errorf("second row = %+v, want the declared mask and rule", got.inotify.paths[1])
		}
	})

	t.Run("inotify paths: untyped carrier and the empty forms", func(t *testing.T) {
		if got := decodeFor(t, types.ConfigValue{Key: "sensors.inotify.paths", Value: []any{map[string]any{"path": "/x"}}}); len(got.inotify.paths) != 1 || got.inotify.paths[0].path != "/x" {
			t.Errorf("[]any form: paths = %+v, want one row", got.inotify.paths)
		}
		for name, v := range map[string]any{
			"nil":             nil,
			"empty []map":     []map[string]any{},
			"empty []any":     []any{},
			"empty string":    "",
			"nil typed []map": []map[string]any(nil),
			"whitespace":      "  ",
		} {
			got := decodeFor(t, types.ConfigValue{Key: "sensors.inotify.paths", Value: v})
			if len(got.inotify.paths) != 0 {
				t.Errorf("%s: paths = %+v, want the empty set (the compiled default)", name, got.inotify.paths)
			}
		}
	})

	t.Run("inotify paths: a table without a path, and a scalar, are refused by name", func(t *testing.T) {
		_, err := decodeConfig([]types.ConfigValue{
			{Key: "origin.host_id", Value: "testhost"},
			{Key: "sensors.inotify.paths", Value: []map[string]any{{"mask": "IN_MODIFY"}}},
		})
		if err == nil || !contains(err.Error(), "sensors.inotify.paths") || !contains(err.Error(), "without a path") {
			t.Errorf("a path-less table = %v, want the by-name refusal", err)
		}
		_, err = decodeConfig([]types.ConfigValue{
			{Key: "origin.host_id", Value: "testhost"},
			{Key: "sensors.inotify.paths", Value: "/etc"},
		})
		if err == nil {
			t.Error("a scalar value for the list-of-tables key was accepted (the file owns this key; a flag or an env var cannot express it)")
		}
	})
}
