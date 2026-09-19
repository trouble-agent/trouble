package lifecycle

// flow_bounds_test.go — TRBL-039: the SPEC-08 §3.9a dispatch queue's five bounds
// are SPEC-12 registry keys, not constants inside internal/flow.
//
// What this file proves, and what each level can prove that the others cannot:
//
//	defaults — the five keys resolve with no file, no env and no flag, and every
//	           one of them is the number §3.9a states (256 / 72h / 5 / 5s / 100);
//	file     — a `[flow]` table resolves, and a HALF-specified table leaves the
//	           other bounds at their defaults instead of zero;
//	precedence — flag > env > file > default, with the ordinary provenance on
//	           each row (the same rule every other scalar key follows);
//	explain  — `trouble config explain` lists all five, once each;
//	refusals — a zero, a negative and an unparsable bound are refused at
//	           resolution with TROUBLE-LIFECYCLE-001 naming the key, so a boot
//	           cannot come up with a queue that has no bound at all.
//
// The composition root's half (the resolved values reaching flow.Spool and the
// Flow itself) is internal/app/flow_bounds_wiring_test.go: this package must not
// import internal/flow (SPEC-12 §4.5/§8).

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// flowBoundsDefaults are the §3.9a numbers, spelled once so the assertions below
// read as the spec rather than as five loose literals.
var flowBoundsDefaults = []struct {
	key   string
	value string // the resolved value, as the explain row carries it
}{
	{"flow.spool_max_entries", "256"},
	{"flow.spool_ttl", "72h"},
	{"flow.spool_max_attempts", "5"},
	{"flow.replay_every", "5s"},
	{"flow.replay_batch", "100"},
}

// emptyConfigPath is a path with no file behind it: Resolve tolerates a missing
// file (os.ErrNotExist), and passing THIS one keeps the test away from the
// developer's real ~/.config/trouble/config.toml.
func emptyConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.toml")
}

func flowRow(t *testing.T, r Resolved, key string) types.ConfigValue {
	t.Helper()
	var found []types.ConfigValue
	for _, cv := range r.Values {
		if cv.Key == key {
			found = append(found, cv)
		}
	}
	if len(found) != 1 {
		t.Fatalf("explain dump carries %d rows for %s, want exactly 1", len(found), key)
	}
	return found[0]
}

// TestFlowBoundsDefaultsAreTheSection39aNumbers is the first acceptance
// criterion's default half: with nothing set anywhere, the five keys resolve to
// the numbers SPEC-08 §3.9a states, each with builtin provenance.
func TestFlowBoundsDefaultsAreTheSection39aNumbers(t *testing.T) {
	r, err := Resolve(nil, nil, emptyConfigPath(t))
	if err != nil {
		t.Fatalf("Resolve with no file, no env and no flags: %v", err)
	}
	c := r.Config
	if c.Flow.SpoolMaxEntries != 256 {
		t.Errorf("flow.spool_max_entries = %d, want 256 (§3.9a's in-memory bound)", c.Flow.SpoolMaxEntries)
	}
	if c.Flow.SpoolTTL != "72h" {
		t.Errorf("flow.spool_ttl = %q, want 72h", c.Flow.SpoolTTL)
	}
	if c.Flow.SpoolMaxAttempts != 5 {
		t.Errorf("flow.spool_max_attempts = %d, want 5", c.Flow.SpoolMaxAttempts)
	}
	if c.Flow.ReplayEvery != "5s" {
		t.Errorf("flow.replay_every = %q, want 5s", c.Flow.ReplayEvery)
	}
	if c.Flow.ReplayBatch != 100 {
		t.Errorf("flow.replay_batch = %d, want 100", c.Flow.ReplayBatch)
	}
	// And the resolved set really is a bounded one: every duration parses and
	// every count is positive, which is what the composition root projects onto
	// flow.SpoolBounds.
	if d := c.Flow.SpoolTTL.Std(); d <= 0 {
		t.Errorf("flow.spool_ttl.Std() = %v: an unparsable TTL would make the bound unbounded", d)
	}
	if d := c.Flow.ReplayEvery.Std(); d <= 0 {
		t.Errorf("flow.replay_every.Std() = %v: an unparsable cadence would make the drain spin", d)
	}

	for _, w := range flowBoundsDefaults {
		cv := flowRow(t, r, w.key)
		if cv.Source != "default" || cv.SourceRef != "builtin" {
			t.Errorf("%s provenance = (%q, %q), want (\"default\", \"builtin\")", w.key, cv.Source, cv.SourceRef)
		}
		// The value's Go type follows its source (an int default, an int64 from
		// the file reader, a types.Duration string); what the operator reads out
		// of the explain dump is the rendered value, and that is what is pinned.
		if got := fmt.Sprintf("%v", cv.Value); got != w.value {
			t.Errorf("%s explain value = %q, want %q", w.key, got, w.value)
		}
	}
}

// TestFlowBoundsFromTheConfigFileAndAHalfSpecifiedTable is the resolution half:
// a `[flow]` table resolves as ordinary file config, and leaving a bound out
// leaves it at its default rather than at zero — the "defaults fill, never zero"
// rule the queue depends on.
func TestFlowBoundsFromTheConfigFileAndAHalfSpecifiedTable(t *testing.T) {
	path := writeConfigFile(t, `[flow]
spool_max_entries = 8
spool_ttl = "6h"
`)
	r, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v\n"+
			"the [flow] table must be a registered config surface (SPEC-12 §3.1d), not an unknown file key", path, err)
	}
	c := r.Config
	if c.Flow.SpoolMaxEntries != 8 || c.Flow.SpoolTTL != "6h" {
		t.Errorf("declared bounds = (%d, %q), want (8, 6h)", c.Flow.SpoolMaxEntries, c.Flow.SpoolTTL)
	}
	// The three the table never mentions keep the documented defaults.
	if c.Flow.SpoolMaxAttempts != 5 || c.Flow.ReplayEvery != "5s" || c.Flow.ReplayBatch != 100 {
		t.Errorf("undeclared bounds = (%d, %q, %d), want the §3.9a defaults (5, 5s, 100): a half-specified [flow] table still has to bound the queue",
			c.Flow.SpoolMaxAttempts, c.Flow.ReplayEvery, c.Flow.ReplayBatch)
	}

	declared := map[string]bool{"flow.spool_max_entries": true, "flow.spool_ttl": true}
	for _, w := range flowBoundsDefaults {
		cv := flowRow(t, r, w.key)
		if declared[w.key] {
			if cv.Source != "file" || cv.SourceRef != path {
				t.Errorf("%s provenance = (%q, %q), want (\"file\", %q)", w.key, cv.Source, cv.SourceRef, path)
			}
			continue
		}
		if cv.Source != "default" || cv.SourceRef != "builtin" {
			t.Errorf("%s provenance = (%q, %q), want (\"default\", \"builtin\"): the table does not declare it",
				w.key, cv.Source, cv.SourceRef)
		}
	}
}

// TestFlowBoundsFollowTheOrdinaryPrecedence is the third half: the five keys are
// ordinary keys, so flag > env > file > default and the provenance names the
// source that won (plus the non-fatal TROUBLE-LIFECYCLE-002 conflict row, the
// same record every other key produces).
func TestFlowBoundsFollowTheOrdinaryPrecedence(t *testing.T) {
	path := writeConfigFile(t, `[flow]
spool_max_entries = 8
replay_batch = 7
`)
	env := []string{"TROUBLE_FLOW_SPOOL_MAX_ENTRIES=5"}
	args := []string{"--flow-spool_max_entries", "3"}

	r, err := Resolve(args, env, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if got := r.Config.Flow.SpoolMaxEntries; got != 3 {
		t.Errorf("flow.spool_max_entries = %d, want 3 (the flag wins over the file and the environment)", got)
	}
	cv := flowRow(t, r, "flow.spool_max_entries")
	if cv.Source != "flag" || cv.SourceRef != "--flow-spool_max_entries" {
		t.Errorf("flow.spool_max_entries provenance = (%q, %q), want (\"flag\", \"--flow-spool_max_entries\")", cv.Source, cv.SourceRef)
	}

	// The environment is the middle rung: with the flag removed, the env value
	// wins over the file.
	r2, err := Resolve(nil, env, path)
	if err != nil {
		t.Fatalf("Resolve (env only) = %v", err)
	}
	if got := r2.Config.Flow.SpoolMaxEntries; got != 5 {
		t.Errorf("flow.spool_max_entries = %d, want 5 (the environment wins over the file)", got)
	}
	if cv := flowRow(t, r2, "flow.spool_max_entries"); cv.Source != "env" || cv.SourceRef != "TROUBLE_FLOW_SPOOL_MAX_ENTRIES" {
		t.Errorf("flow.spool_max_entries provenance = (%q, %q), want (\"env\", \"TROUBLE_FLOW_SPOOL_MAX_ENTRIES\")", cv.Source, cv.SourceRef)
	}
	// A key only the file declares still resolves from the file.
	if got := r2.Config.Flow.ReplayBatch; got != 7 {
		t.Errorf("flow.replay_batch = %d, want 7 (the file's value)", got)
	}
	if cv := flowRow(t, r2, "flow.replay_batch"); cv.Source != "file" || cv.SourceRef != path {
		t.Errorf("flow.replay_batch provenance = (%q, %q), want (\"file\", %q)", cv.Source, cv.SourceRef, path)
	}
}

// TestFlowBoundsAppearInTheExplainDump is the visibility half of the first
// acceptance criterion: `trouble config explain` lists all five keys, once each,
// which is what `Explain` renders from.
func TestFlowBoundsAppearInTheExplainDump(t *testing.T) {
	r, err := Resolve([]string{"--flow-replay_batch", "4"}, nil, emptyConfigPath(t))
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	out, err := Explain(r, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	seen := map[string]types.ConfigValue{}
	for _, cv := range out {
		seen[cv.Key] = cv
	}
	for _, w := range flowBoundsDefaults {
		cv, ok := seen[w.key]
		if !ok {
			t.Errorf("config explain does not list %s: an operator cannot read the queue's bound out", w.key)
			continue
		}
		if cv.Source == "" || cv.SourceRef == "" {
			t.Errorf("%s explain row carries no provenance: (%q, %q)", w.key, cv.Source, cv.SourceRef)
		}
	}
	// The key that was overridden reports the flag; the filter form of explain
	// reaches the same key by name.
	only, err := Explain(r, []string{"flow.replay_batch"})
	if err != nil {
		t.Fatalf("Explain(filtered): %v", err)
	}
	if len(only) != 1 || only[0].Key != "flow.replay_batch" || only[0].Source != "flag" {
		t.Errorf("Explain([flow.replay_batch]) = %+v, want exactly the flag-sourced row", only)
	}
}

// TestFlowBoundsRefuseNonPositiveAndUnparsableValues is the refusal half: a bound
// that is not strictly positive is a boot refusal naming the key, never a resolved
// zero (which the flow would read as "unbounded"). The table drives both classes
// through all three sources an operator has.
func TestFlowBoundsRefuseNonPositiveAndUnparsableValues(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		env     []string
		args    []string
		wantKey string
	}{
		{"file: zero entries", "[flow]\nspool_max_entries = 0\n", nil, nil, "flow.spool_max_entries"},
		{"file: negative entries", "[flow]\nspool_max_entries = -4\n", nil, nil, "flow.spool_max_entries"},
		{"file: unparsable ttl", "[flow]\nspool_ttl = \"banana\"\n", nil, nil, "flow.spool_ttl"},
		{"file: zero ttl", "[flow]\nspool_ttl = \"0s\"\n", nil, nil, "flow.spool_ttl"},
		{"file: negative ttl", "[flow]\nspool_ttl = \"-1h\"\n", nil, nil, "flow.spool_ttl"},
		{"file: empty ttl", "[flow]\nspool_ttl = \"\"\n", nil, nil, "flow.spool_ttl"},
		{"file: zero attempts", "[flow]\nspool_max_attempts = 0\n", nil, nil, "flow.spool_max_attempts"},
		{"file: negative attempts", "[flow]\nspool_max_attempts = -1\n", nil, nil, "flow.spool_max_attempts"},
		{"file: unparsable cadence", "[flow]\nreplay_every = \"every-so-often\"\n", nil, nil, "flow.replay_every"},
		{"file: zero cadence", "[flow]\nreplay_every = \"0s\"\n", nil, nil, "flow.replay_every"},
		{"file: negative batch", "[flow]\nreplay_batch = -2\n", nil, nil, "flow.replay_batch"},
		{"file: zero batch", "[flow]\nreplay_batch = 0\n", nil, nil, "flow.replay_batch"},
		{"flag: zero entries", "", nil, []string{"--flow-spool_max_entries", "0"}, "flow.spool_max_entries"},
		{"flag: unparsable ttl", "", nil, []string{"--flow-spool_ttl", "soon"}, "flow.spool_ttl"},
		{"env: negative batch", "", []string{"TROUBLE_FLOW_REPLAY_BATCH=-5"}, nil, "flow.replay_batch"},
		{"env: zero attempts", "", []string{"TROUBLE_FLOW_SPOOL_MAX_ATTEMPTS=0"}, nil, "flow.spool_max_attempts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := emptyConfigPath(t)
			if tc.file != "" {
				path = writeConfigFile(t, tc.file)
			}
			_, err := Resolve(tc.args, tc.env, path)
			if err == nil {
				t.Fatalf("Resolve accepted a non-positive/unparsable bound: the queue would run with no bound at all")
			}
			if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-001") {
				t.Errorf("refusal code: got %v, want the TROUBLE-LIFECYCLE-001 class a neighbouring key's bad value carries", err)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("refusal does not name the key: got %v, want %s in the message", err, tc.wantKey)
			}
		})
	}
}
