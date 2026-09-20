package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// gate_test.go pins the SPEC-13 §4.1 step 1 profile matrix and the profile row
// of the topology table.
//
// The matrix is exercised through the REAL config resolution path
// (lifecycle.Resolve over argv/env, the way the daemon boots), not by poking a
// struct: the point of the profile being ordinary config is that its gate sees
// what resolution produced.

func resolveWith(t *testing.T, args []string) lifecycle.Config {
	t.Helper()
	res, err := lifecycle.Resolve(args, nil, "")
	if err != nil {
		t.Fatalf("lifecycle.Resolve(%v): %v", args, err)
	}
	return res.Config
}

func lightHubConfig(t *testing.T) lifecycle.Config {
	t.Helper()
	return resolveWith(t, []string{
		"--server-profile", "light-hub",
		"--server-redis-url", "redis://127.0.0.1:6379/0",
		"--server-duckbrain-namespace", "trouble/7f3a91c2d4e5b607",
	})
}

func TestGateProfileMatrix(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantErr  string
		wantCode types.ErrorCode
		enabled  bool
		reason   string
	}{
		{
			name:    "standalone default",
			args:    nil,
			enabled: false,
		},
		{
			name: "standalone explicit",
			args: []string{"--server-profile", "standalone"},
		},
		{
			name: "light-hub complete",
			args: []string{
				"--server-profile", "light-hub",
				"--server-redis-url", "redis://127.0.0.1:6379/0",
				"--server-duckbrain-namespace", "trouble/host1",
			},
			enabled: true,
		},
		{
			name:     "light-hub without redis url",
			args:     []string{"--server-profile", "light-hub", "--server-duckbrain-namespace", "trouble/host1"},
			wantErr:  "requires server.redis.url",
			wantCode: types.CodeHub001,
			reason:   "missing_redis_url",
		},
		{
			name:     "light-hub without namespace",
			args:     []string{"--server-profile", "light-hub", "--server-redis-url", "redis://127.0.0.1:6379/0"},
			wantErr:  "requires server.duckbrain.namespace",
			wantCode: types.CodeHub001,
			reason:   "missing_namespace",
		},
		{
			name:     "unknown profile",
			args:     []string{"--server-profile", "cluster"},
			wantErr:  "is not one of standalone|light-hub",
			wantCode: types.CodeHub001,
			reason:   "unknown_profile",
		},
		{
			name: "light-hub on a satellite",
			args: []string{
				"--server-profile", "light-hub",
				"--server-redis-url", "redis://127.0.0.1:6379/0",
				"--server-duckbrain-namespace", "trouble/host1",
				"--hub-mode", "satellite",
			},
			wantErr:  "hub.mode=satellite",
			wantCode: types.CodeHub001,
			reason:   "satellite_profile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolveWith(t, tc.args)
			gate, err := Gate(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Gate: unexpected error %v", err)
				}
				if gate.Enabled != tc.enabled {
					t.Fatalf("Enabled=%v want %v (profile %q)", gate.Enabled, tc.enabled, gate.Profile)
				}
				if !gate.Valid {
					t.Fatalf("standalone/light-hub complete must be Valid, reason=%q", gate.InvalidReason)
				}
				return
			}
			if err == nil {
				t.Fatalf("Gate: want refusal, got none")
			}
			if CodeOf(err) != tc.wantCode {
				t.Fatalf("code = %q want %q (%v)", CodeOf(err), tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %q", err.Error(), tc.wantErr)
			}
			if gate.InvalidReason != tc.reason {
				t.Fatalf("InvalidReason=%q want %q", gate.InvalidReason, tc.reason)
			}
			var he *Error
			if !errors.As(err, &he) {
				t.Fatalf("error is not a hub.Error")
			}
			if he.Class() != types.ErrClassPermanent {
				t.Fatalf("001 class = %q want permanent", he.Class())
			}
		})
	}
}

// TestGateLightHubProjectsRuntimeConfig checks that the resolved keys reach the
// runtime structures unchanged — a projection bug would make the hub run with
// defaults while `trouble config explain` showed the operator's values.
func TestGateLightHubProjectsRuntimeConfig(t *testing.T) {
	cfg := resolveWith(t, []string{
		"--server-profile", "light-hub",
		"--server-redis-url", "redis://127.0.0.1:6379/0",
		"--server-redis-stream", "trouble:custom",
		"--server-redis-group", "writers",
		"--server-redis-maxlen", "5000",
		"--server-redis-require_redis", "true",
		"--server-duckbrain-namespace", "trouble/host1",
		"--server-duckbrain-endpoint", "http://127.0.0.1:7645",
		"--server-duckbrain-keep_local_generations", "3",
		"--origin-host_id", "7f3a91c2d4e5b607",
	})
	gate, err := GateWithPaths(cfg, t.TempDir(), filepath.Join(t.TempDir(), "ledger"), "2026-09-16.jsonl")
	if err != nil {
		t.Fatalf("GateWithPaths: %v", err)
	}
	if gate.Redis.Stream != "trouble:custom" || gate.Redis.Group != "writers" {
		t.Fatalf("stream/group not projected: %+v", gate.Redis)
	}
	if gate.Redis.MaxLen != 5000 {
		t.Fatalf("maxlen = %d want 5000", gate.Redis.MaxLen)
	}
	if !gate.Redis.RequireRedis {
		t.Fatalf("require_redis not projected")
	}
	if gate.Archive.Namespace != "trouble/host1" || gate.Archive.Endpoint != "http://127.0.0.1:7645" {
		t.Fatalf("archive target not projected: %+v", gate.Archive)
	}
	if gate.Archive.KeepLocalGens != 3 {
		t.Fatalf("keep_local_generations = %d want 3", gate.Archive.KeepLocalGens)
	}
	if gate.Archive.LiveFile != "2026-09-16.jsonl" {
		t.Fatalf("live file not carried: %q", gate.Archive.LiveFile)
	}
	// The consumer resolves "" → origin.host_id (SPEC-13 §3.3: one consumer per
	// state root). The host identity is the composition root's to resolve, so the
	// gate only carries it through — and the test declares it the way a config
	// does.
	if got := gate.Consumer(); got != "7f3a91c2d4e5b607" {
		t.Fatalf("consumer = %q want the declared origin.host_id", got)
	}
}

func TestTopologyImpactCarriesTheProfileRow(t *testing.T) {
	cfg := lightHubConfig(t)
	gate, err := Gate(cfg)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	rows := TopologyImpact(gate)
	base := len(lifecycle.TopologyDecisions(cfg))
	if len(rows) != base+1 {
		t.Fatalf("rows = %d want %d (SPEC-12 §3.7 rows + the profile row)", len(rows), base+1)
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last.Decision, "light-hub") {
		t.Fatalf("profile row does not name the profile: %q", last.Decision)
	}
	if last.From != "" || last.To != "" {
		t.Fatalf("the profile row must not claim a rung transition (SPEC-13 §1: the profile is orthogonal): %+v", last)
	}
	found := false
	for _, k := range last.ConfigKeys {
		if k == "server.redis.url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile row does not list the queue keys: %v", last.ConfigKeys)
	}

	standalone, err := Gate(resolveWith(t, nil))
	if err != nil {
		t.Fatalf("Gate(standalone): %v", err)
	}
	rows = TopologyImpact(standalone)
	if !strings.Contains(rows[len(rows)-1].Decision, "standalone") {
		t.Fatalf("standalone row does not name the profile: %q", rows[len(rows)-1].Decision)
	}
	if len(rows[len(rows)-1].ConfigKeys) != 1 || rows[len(rows)-1].ConfigKeys[0] != "server.profile" {
		t.Fatalf("standalone row should own only server.profile: %v", rows[len(rows)-1].ConfigKeys)
	}
}

// TestProfileStateKeepsSinceWhileTheProfileIsUnchanged pins the TROUBLE-HUB-013
// pre-condition: a profile change is visible as a new `since`, which is what
// "refused live, effective after restart" has to mean on the health surface.
func TestProfileStateKeepsSinceWhileTheProfileIsUnchanged(t *testing.T) {
	root := t.TempDir()
	pc := types.ProfileConfig{Profile: ProfileLightHub, HubID: "h1", Valid: true}
	first, err := WriteProfileState(root, pc, mustTime(t, "2026-09-16T09:00:00.000Z"))
	if err != nil {
		t.Fatalf("WriteProfileState: %v", err)
	}
	second, err := WriteProfileState(root, pc, mustTime(t, "2026-09-17T09:00:00.000Z"))
	if err != nil {
		t.Fatalf("WriteProfileState: %v", err)
	}
	if second.Since != first.Since {
		t.Fatalf("since moved while the profile was unchanged: %q → %q", first.Since, second.Since)
	}
	if second.ConfigHash == "" || second.ConfigHash != first.ConfigHash {
		t.Fatalf("config hash not stable: %q / %q", first.ConfigHash, second.ConfigHash)
	}
	other := pc
	other.RedisURL = "redis://other:6379/0"
	third, err := WriteProfileState(root, other, mustTime(t, "2026-09-18T09:00:00.000Z"))
	if err != nil {
		t.Fatalf("WriteProfileState: %v", err)
	}
	if third.Since == second.Since {
		t.Fatalf("since did not move when the profile's config changed")
	}
	if third.ConfigHash == second.ConfigHash {
		t.Fatalf("config hash did not change with the config")
	}
	// The file is 0600 in a 0700 directory (SPEC-13 §3.1).
	fi, err := os.Stat(ProfilePath(root))
	if err != nil {
		t.Fatalf("stat profile.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("profile.json mode = %04o want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(StateDir(root))
	if err != nil {
		t.Fatalf("stat hub dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("hub dir mode = %04o want 0700", di.Mode().Perm())
	}
	if _, err := ReadProfileState(root); err != nil {
		t.Fatalf("ReadProfileState: %v", err)
	}
}

// TestStandaloneRuntimeTouchesNothing is the standalone invariant: Open on the
// default profile must not dial, must not create the hub state tree and must
// report Enabled=false (SPEC-13 §4.1 step 2).
func TestStandaloneRuntimeTouchesNothing(t *testing.T) {
	root := t.TempDir()
	gate, err := Gate(resolveWith(t, nil))
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	rt, err := Open(context.Background(), RuntimeConfig{
		Profile:   types.ProfileConfig{Profile: ProfileStandalone, Valid: true},
		Redis:     RedisConfig{URL: "redis://127.0.0.1:1/0"}, // would fail to dial if it were used
		StateRoot: root,
		HostID:    "h1",
	})
	if err != nil {
		t.Fatalf("Open(standalone): %v", err)
	}
	if rt.Enabled() {
		t.Fatalf("standalone runtime reports Enabled=true")
	}
	if rt.RuntimeMode() != ModeStandalone {
		t.Fatalf("mode = %q want standalone", rt.RuntimeMode())
	}
	if st := rt.Status(context.Background()); st.Enabled {
		t.Fatalf("standalone status must carry Enabled=false, got %+v", st)
	}
	if _, err := os.Stat(StateDir(root)); !os.IsNotExist(err) {
		t.Fatalf("standalone boot created the hub state tree (%v)", err)
	}
	if rt.Door() != nil || rt.Consumer() != nil || rt.Dedup() != nil {
		t.Fatalf("standalone runtime built queue machinery")
	}
	_ = gate
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := types.ParseUTC(s)
	if err != nil {
		t.Fatalf("ParseUTC(%q): %v", s, err)
	}
	return ts
}
