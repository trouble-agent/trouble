package lifecycle

// profile_test.go pins the SPEC-13 §2.1 `[server]` surface as a RESOLVED CONFIG
// SURFACE (SPEC-12 §3.7a rule 1 and SPEC-12 §7's `internal/lifecycle/profile_test.go`
// row). It is the regression for TRBL-027: before this file, the light-hub
// profile was unreachable because `[server]` was not in the registry, so a
// config file that declared it — including the shipped
// deploy/container/config.light-hub.toml — was refused as an unknown file key
// (TROUBLE-LIFECYCLE-001) and the profile could not be selected at all.
//
// Four halves, asserted separately because each can regress on its own:
//
//  1. REGISTRATION + DEFAULTS — every documented key resolves, one ConfigValue
//     per leaf, defaults exactly as SPEC-13 §2.1 pins them, and the standalone
//     default keeps working (the profile is additive, never a replacement).
//  2. PRECEDENCE + PROVENANCE — flag > env > file > default with the winning
//     source named, like every other key: the profile is not a second config
//     system.
//  3. THE BOOT GATE — light-hub without server.redis.url or
//     server.duckbrain.namespace (and light-hub on a satellite) is refused with
//     TROUBLE-HUB-001 and the SPEC-TYPES §3.15.11 reason, while resolution
//     itself still succeeds (the gate, not the file reader, is the refusal).
//  4. CLOSEDNESS — an unknown key inside `[server]` stays fatal by name (the
//     registry is the only whitelist), and the redaction rules still hold.
//
// Two items of the same spec row are NOT covered here, because the machinery
// they assert does not exist in this tree: the SIGHUP profile-switch refusal
// (TROUBLE-HUB-013) needs a reload path that re-runs Resolve while serving, and
// the `trouble topology` profile row needs a TopologyDecision shape that can
// express a non-rung decision (its From/To are rung-typed, and SPEC-12 §3.7a
// states the profile is orthogonal to the T-levels). Both are pinned as gaps by
// TestServerProfileBoundariesAreUnimplemented, so they cannot be forgotten and
// cannot be "fixed" by inventing a rung.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// serverDefaults is the SPEC-13 §2.1 / §2.1.1 default for every key of the
// surface, as the resolved value string (the registry's own default, never a
// fabricated one). server.redis.url and server.duckbrain.namespace are "" on
// purpose: they are REQUIRED for light-hub, so a non-empty default would make
// the TROUBLE-HUB-001 refusal unreachable.
var serverDefaults = map[string]string{
	"server.profile":                          "standalone",
	"server.hub_id":                           "",
	"server.redis.url":                        "",
	"server.redis.password_env":               "TROUBLE_REDIS_PASSWORD",
	"server.redis.stream":                     "trouble:ingest",
	"server.redis.group":                      "ledger-writers",
	"server.redis.consumer":                   "",
	"server.redis.maxlen":                     "1000000",
	"server.redis.dedup_prefix":               "trouble:dedup:",
	"server.redis.dedup_ttl":                  "24h",
	"server.redis.batch_records":              "256",
	"server.redis.batch_bytes":                "524288",
	"server.redis.block_ms":                   "1000",
	"server.redis.claim_min_idle":             "60s",
	"server.redis.dial_timeout":               "2s",
	"server.redis.read_timeout":               "5s",
	"server.redis.write_timeout":              "2s",
	"server.redis.require_redis":              "false",
	"server.redis.require_persistence":        "true",
	"server.redis.check_policy":               "true",
	"server.redis.failover_grace":             "30s",
	"server.duckbrain.enabled":                "false",
	"server.duckbrain.namespace":              "",
	"server.duckbrain.endpoint":               "",
	"server.duckbrain.archive_interval":       "1h",
	"server.duckbrain.archive_batch_files":    "8",
	"server.duckbrain.keep_local_generations": "2",
	"server.duckbrain.verify_after_write":     "true",
	"server.duckbrain.gzip":                   "true",
}

func serverRow(t *testing.T, r Resolved, key string) types.ConfigValue {
	t.Helper()
	for _, cv := range r.Values {
		if cv.Key == key {
			return cv
		}
	}
	t.Fatalf("the resolved config carries no %q row: the key is not registered in the SPEC-13 §2.1 surface", key)
	return types.ConfigValue{}
}

func serverValue(t *testing.T, r Resolved, key string) string {
	t.Helper()
	cv := serverRow(t, r, key)
	s, err := asString(cv.Value)
	if err != nil {
		t.Fatalf("%s: the resolved value is not a scalar: %v", key, err)
	}
	return s
}

// TestServerProfileDefaultsAreStandalone is half 1: an untouched config (no file
// at all) resolves the whole `[server]` surface to the SPEC-13 defaults, and the
// profile verdict is valid. This is the "standalone is the default and stays
// standalone" regression: registering the surface must not change a single
// default of the shipped standalone daemon.
func TestServerProfileDefaultsAreStandalone(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.toml")
	r, err := Resolve(nil, nil, absent)
	if err != nil {
		t.Fatalf("Resolve with no config file = %v, want a default resolve", err)
	}

	for key, want := range serverDefaults {
		cv := serverRow(t, r, key)
		if got := cv.Source; got != "default" {
			t.Errorf("%s source = %q, want %q", key, got, "default")
		}
		if got := cv.SourceRef; got != "builtin" {
			t.Errorf("%s source_ref = %q, want %q", key, got, "builtin")
		}
		if got, _ := asString(cv.Value); got != want {
			t.Errorf("%s value = %q, want %q", key, got, want)
		}
	}

	// The typed Config carries the same values (the projection below reads it).
	c := r.Config
	if c.Server.Profile != ProfileStandalone {
		t.Errorf("cfg.Server.Profile = %q, want %q", c.Server.Profile, ProfileStandalone)
	}
	if c.Server.Redis.RequirePersistence != true || c.Server.Redis.CheckPolicy != true {
		t.Errorf("server.redis preflight defaults = (require_persistence=%v check_policy=%v), want (true true) (SPEC-13 §2.1.1)",
			c.Server.Redis.RequirePersistence, c.Server.Redis.CheckPolicy)
	}
	if c.Server.DuckBrain.VerifyAfterWrite != true || c.Server.DuckBrain.Gzip != true {
		t.Errorf("server.duckbrain defaults = (verify_after_write=%v gzip=%v), want (true true)", c.Server.DuckBrain.VerifyAfterWrite, c.Server.DuckBrain.Gzip)
	}

	p := c.ServerProfile()
	if !p.Valid || p.InvalidReason != "" {
		t.Errorf("standalone verdict = (valid=%v reason=%q), want (true \"\")", p.Valid, p.InvalidReason)
	}
	if p.Profile != ProfileStandalone || p.RedisURL != "" || p.DBNamespace != "" {
		t.Errorf("standalone profile = %+v, want profile=standalone with no Redis URL and no namespace", p)
	}
	if err := c.CheckServerProfile(); err != nil {
		t.Errorf("CheckServerProfile on the default config = %v, want nil (standalone needs no dependencies)", err)
	}
	if got := p.DedupTTL.Seconds(); got != 86400 {
		t.Errorf("standalone dedup_ttl = %v seconds, want 86400 (\"24h\")", got)
	}
}

// TestServerProfilePrecedence is half 2: the profile resolves through the same
// four sources every other key uses, and the row names the source that won.
func TestServerProfilePrecedence(t *testing.T) {
	// The file declares a COMPLETE light-hub profile: the cases below move only
	// the profile key, so a case that selects light-hub through env or argv is
	// still complete, and the gate passes for the reason under test.
	file := `[server]
profile = "standalone"

[server.redis]
url = "redis://127.0.0.1:6379/0"

[server.duckbrain]
namespace = "trouble/host"
`
	cases := []struct {
		name       string
		args       []string
		env        []string
		want       string
		wantSource string
		wantRef    string
	}{
		{
			name:       "flag beats env, file and default",
			args:       []string{"--server-profile", "light-hub"},
			env:        []string{"TROUBLE_SERVER_PROFILE=standalone"},
			want:       ProfileLightHub,
			wantSource: "flag",
			wantRef:    "--server-profile",
		},
		{
			name:       "env beats file and default",
			env:        []string{"TROUBLE_SERVER_PROFILE=light-hub"},
			want:       ProfileLightHub,
			wantSource: "env",
			wantRef:    "TROUBLE_SERVER_PROFILE",
		},
		{
			name:       "file beats default",
			want:       ProfileStandalone,
			wantSource: "file",
			wantRef:    "", // the config path, asserted below
		},
	}
	path := writeConfigFile(t, file)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Resolve(c.args, c.env, path)
			if err != nil {
				t.Fatalf("Resolve = %v, want the profile to resolve like any other key", err)
			}
			cv := serverRow(t, r, "server.profile")
			if got, _ := asString(cv.Value); got != c.want {
				t.Errorf("server.profile = %q, want %q", got, c.want)
			}
			if cv.Source != c.wantSource {
				t.Errorf("server.profile source = %q, want %q", cv.Source, c.wantSource)
			}
			wantRef := c.wantRef
			if wantRef == "" {
				wantRef = path
			}
			if cv.SourceRef != wantRef {
				t.Errorf("server.profile source_ref = %q, want %q", cv.SourceRef, wantRef)
			}
			// Whatever won, the dependency keys still come from the file: the
			// profile is one key among many, not a switch that re-reads config.
			if got := serverValue(t, r, "server.redis.url"); got != "redis://127.0.0.1:6379/0" {
				t.Errorf("server.redis.url = %q, want the file's value", got)
			}
			if err := r.Config.CheckServerProfile(); err != nil {
				t.Errorf("CheckServerProfile = %v, want nil for this complete profile", err)
			}
		})
	}
}

// TestServerProfileLightHubAccepted is half 3's positive direction: the exact
// spec shape ([server] profile=light-hub + [server.redis].url +
// [server.duckbrain].namespace) resolves, is valid, projects onto
// types.ProfileConfig field for field, and appears in the explain dump with file
// provenance. This is the state TRBL-027 could not reach at all.
func TestServerProfileLightHubAccepted(t *testing.T) {
	file := `[server]
profile = "light-hub"
hub_id = "hub-1"

[server.redis]
url = "redis://127.0.0.1:6379/0"
maxlen = 500000
require_redis = true
dedup_ttl = "12h"
stream = "trouble:ingest:test"
group = "ledger-writers-test"

[server.duckbrain]
namespace = "trouble/host-1"
endpoint = "http://127.0.0.1:3000"
archive_interval = "30m"
keep_local_generations = 4
`
	path := writeConfigFile(t, file)
	r, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v\n"+
			"the documented SPEC-13 §2.1 light-hub profile must be a registered config surface, not an unknown file key (TRBL-027)", file, err)
	}

	// Every leaf is ONE registered key: the ones the file declares take the file
	// as their source, the rest keep the builtin default (the profile is not a
	// switch that re-reads config, and there is no per-sub-key absorption).
	declared := []string{
		"server.profile", "server.hub_id",
		"server.redis.url", "server.redis.maxlen", "server.redis.require_redis",
		"server.redis.dedup_ttl", "server.redis.stream", "server.redis.group",
		"server.duckbrain.namespace", "server.duckbrain.endpoint",
		"server.duckbrain.archive_interval", "server.duckbrain.keep_local_generations",
	}
	inFile := map[string]bool{}
	for _, k := range declared {
		inFile[k] = true
	}
	for key := range serverDefaults {
		cv := serverRow(t, r, key)
		if !inFile[key] {
			if cv.Source != "default" || cv.SourceRef != "builtin" {
				t.Errorf("%s source = (%q, %q), want (\"default\", \"builtin\"): the file does not declare it", key, cv.Source, cv.SourceRef)
			}
			continue
		}
		if cv.Source != "file" {
			t.Errorf("%s source = %q, want \"file\" (the declaration is the operator's statement)", key, cv.Source)
		}
		if cv.SourceRef != path {
			t.Errorf("%s source_ref = %q, want the config path %q", key, cv.SourceRef, path)
		}
	}

	c := r.Config
	if c.Server.Profile != ProfileLightHub {
		t.Fatalf("cfg.Server.Profile = %q, want %q", c.Server.Profile, ProfileLightHub)
	}
	if c.Server.Redis.MaxLen != 500000 || c.Server.Redis.RequireRedis != true {
		t.Errorf("cfg.Server.Redis = (%d, %v), want maxlen 500000 and require_redis true", c.Server.Redis.MaxLen, c.Server.Redis.RequireRedis)
	}
	if c.Server.DuckBrain.KeepLocalGens != 4 || c.Server.DuckBrain.ArchiveInterval != "30m" {
		t.Errorf("cfg.Server.DuckBrain = (%d, %q), want keep_local_generations 4 and archive_interval 30m",
			c.Server.DuckBrain.KeepLocalGens, c.Server.DuckBrain.ArchiveInterval)
	}

	p := c.ServerProfile()
	if !p.Valid || p.InvalidReason != "" {
		t.Fatalf("light-hub verdict = (valid=%v reason=%q), want (true \"\")", p.Valid, p.InvalidReason)
	}
	wantProjection := types.ProfileConfig{
		Profile:         ProfileLightHub,
		HubID:           "hub-1",
		RedisURL:        "redis://127.0.0.1:6379/0",
		RedisStream:     "trouble:ingest:test",
		ConsumerGroup:   "ledger-writers-test",
		Consumer:        "",
		MaxLen:          500000,
		DedupTTL:        "12h",
		RequireRedis:    true,
		DBNamespace:     "trouble/host-1",
		DBEndpoint:      "http://127.0.0.1:3000",
		ArchiveInterval: "30m",
		KeepLocalGens:   4,
		Valid:           true,
		InvalidReason:   "",
	}
	if p != wantProjection {
		t.Errorf("ServerProfile() =\n  %+v\nwant\n  %+v", p, wantProjection)
	}
	// The projection never rewrites the empty-string fallbacks in place: "" means
	// "origin.host_id" at the point of use (SPEC-13 §2.1, §3.3).
	if p.Consumer != "" {
		t.Errorf("consumer = %q, want \"\" (resolved to origin.host_id at the point of use, not in the config)", p.Consumer)
	}
	if err := c.CheckServerProfile(); err != nil {
		t.Errorf("CheckServerProfile = %v, want nil for a complete light-hub profile", err)
	}

	// The explain dump carries the surface: one row per leaf, file provenance.
	dump, err := explainJSON(r.Values)
	if err != nil {
		t.Fatal(err)
	}
	for key := range serverDefaults {
		if n := strings.Count(string(dump), `"key":"`+key+`"`); n != 1 {
			t.Errorf("explain dump carries %d rows for %s, want exactly 1", n, key)
		}
	}
}

// TestServerProfileIncompleteIsRefusedByTheGate is half 3: resolution accepts the
// file (the keys are real), and the GATE refuses it with TROUBLE-HUB-001 and the
// SPEC-TYPES §3.15.11 reason. Separating the two is the point — a config error
// in resolution would name the key, and this refusal must name the profile.
func TestServerProfileIncompleteIsRefusedByTheGate(t *testing.T) {
	complete := `[server]
profile = "light-hub"
hub_id = "hub-1"

[server.redis]
url = "redis://127.0.0.1:6379/0"

[server.duckbrain]
namespace = "trouble/host-1"
`
	cases := []struct {
		name    string
		file    string
		reason  string
		wantKey string
	}{
		{
			name: "light-hub without a Redis URL",
			file: `[server]
profile = "light-hub"

[server.duckbrain]
namespace = "trouble/host-1"
`,
			reason:  "missing_redis_url",
			wantKey: "server.redis.url",
		},
		{
			name: "light-hub without a DuckBrain namespace",
			file: `[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"
`,
			reason:  "missing_namespace",
			wantKey: "server.duckbrain.namespace",
		},
		{
			name: "light-hub on a satellite",
			file: complete + `
[hub]
mode = "satellite"
`,
			reason:  "satellite_profile",
			wantKey: "hub.mode",
		},
		{
			name: "unknown profile value",
			file: `[server]
profile = "lite-hub"
`,
			reason:  "unknown_profile",
			wantKey: "server.profile",
		},
		{
			name: "explicitly empty profile",
			file: `[server]
profile = ""
`,
			reason:  "unknown_profile",
			wantKey: "server.profile",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfigFile(t, c.file)
			r, err := Resolve(nil, nil, path)
			if err != nil {
				t.Fatalf("Resolve = %v, want the KEYS to resolve; the refusal belongs to the profile gate", err)
			}
			p := r.Config.ServerProfile()
			if p.Valid {
				t.Fatalf("ServerProfile().Valid = true, want false (%s)", c.reason)
			}
			if p.InvalidReason != c.reason {
				t.Errorf("InvalidReason = %q, want %q", p.InvalidReason, c.reason)
			}
			err = r.Config.CheckServerProfile()
			if err == nil {
				t.Fatalf("CheckServerProfile = nil, want %s", types.CodeHub001)
			}
			if !errors.Is(err, types.CodeHub001) {
				t.Errorf("CheckServerProfile = %v, want %s", err, types.CodeHub001)
			}
			if got := types.CodeClass[types.CodeHub001]; got != types.ErrClassPermanent {
				t.Errorf("TROUBLE-HUB-001 class = %q, want %q (SPEC-13 §5)", got, types.ErrClassPermanent)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Errorf("refusal %q does not name %q: the operator remedy has to name the key to set", err.Error(), c.wantKey)
			}
		})
	}
}

// TestServerProfileStandaloneIsUnaffectedByPartialDeclaration is the other half
// of the standalone guarantee: a partial [server] table (or a [server] table on
// a non-loopback host, a satellite, etc.) must not turn the default profile into
// a refusal. Only an explicit light-hub/unknown profile triggers the gate.
func TestServerProfileStandaloneIsUnaffectedByPartialDeclaration(t *testing.T) {
	cases := []struct {
		name string
		file string
	}{
		{name: "empty table", file: "[server]\n"},
		{name: "hub_id only", file: "[server]\nhub_id = \"hub-1\"\n"},
		{name: "standalone with redis block", file: "[server]\nprofile = \"standalone\"\n\n[server.redis]\nmaxlen = 10\n"},
		{name: "standalone with duckbrain block", file: "[server]\nprofile = \"standalone\"\n\n[server.duckbrain]\nenabled = true\nnamespace = \"trouble/x\"\n"},
		{name: "satellite with the default profile", file: "[hub]\nmode = \"satellite\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfigFile(t, c.file)
			r, err := Resolve(nil, nil, path)
			if err != nil {
				t.Fatalf("Resolve = %v, want success", err)
			}
			if p := r.Config.ServerProfile(); !p.Valid {
				t.Fatalf("ServerProfile() = %+v, want valid: the default profile must survive a partial declaration", p)
			}
			if err := r.Config.CheckServerProfile(); err != nil {
				t.Errorf("CheckServerProfile = %v, want nil", err)
			}
		})
	}
}

// TestServerProfileUnknownServerKeyIsFatal is half 4: registration adds a KEY
// LIST, not a namespace. A key inside [server] that SPEC-13 §2.1 does not define
// stays fatal by name, so a typo cannot silently run on the default — and the
// first case is the exact live repro recorded on TRBL-027 (the shipped
// deploy/container/config.light-hub.toml declares server.redis.appendonly, which
// SPEC-13 does not define as a trouble key: Redis-side settings are verified at
// preflight through INFO, not configured here).
func TestServerProfileUnknownServerKeyIsFatal(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		wantKey string
	}{
		{
			name: "shipped container light-hub wire-up (server.redis.appendonly)",
			file: `[server]
profile = "light-hub"

[server.redis]
url = "redis://redis:6379/0"
appendonly = true
`,
			wantKey: "server.redis.appendonly",
		},
		{
			name:    "typo in the profile key",
			file:    "[server]\nprofle = \"light-hub\"\n",
			wantKey: "server.profle",
		},
		{
			name: "unknown key in the duckbrain block",
			file: `[server]
profile = "light-hub"

[server.duckbrain]
namespace = "trouble/host-1"
gzip_level = 6
`,
			wantKey: "server.duckbrain.gzip_level",
		},
		{
			name:    "stray root-level server key",
			file:    "server.redis_url = \"redis://x\"\n",
			wantKey: "server.redis_url",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfigFile(t, c.file)
			_, err := Resolve(nil, nil, path)
			if err == nil {
				t.Fatalf("Resolve accepted an unknown key inside [server]: the registry is the only whitelist")
			}
			if !errors.Is(err, types.CodeLifecycle001) {
				t.Errorf("Resolve = %v, want %s", err, types.CodeLifecycle001)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Errorf("refusal %q does not name %q", err.Error(), c.wantKey)
			}
		})
	}
}

// TestServerProfileRedactionRuleHolds pins the redaction behaviour of the new
// surface: server.redis.password_env holds a VARIABLE NAME (where the password
// lives), not a password, so it is not redacted — while a genuine secret-class
// key in the same file still is, and no secret leaks into the dump. Getting this
// backwards would either hide a key an operator must see or print a credential.
func TestServerProfileRedactionRuleHolds(t *testing.T) {
	const fixture = "sk_live_fixture_0001"
	path := writeConfigFile(t, `hub.token = "`+fixture+`"

[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"
password_env = "TROUBLE_REDIS_PASSWORD"

[server.duckbrain]
namespace = "trouble/host-1"
`)
	r, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}

	cv := serverRow(t, r, "server.redis.password_env")
	if cv.Redacted {
		t.Errorf("server.redis.password_env is redacted; it names the EnvironmentFile key, not a credential")
	}
	if got, _ := asString(cv.Value); got != "TROUBLE_REDIS_PASSWORD" {
		t.Errorf("server.redis.password_env = %q, want the declared variable name", got)
	}
	if got := serverRow(t, r, "hub.token").Redacted; !got {
		t.Errorf("hub.token is not redacted: the existing rule must not have been weakened")
	}

	dump, err := explainJSON(r.Values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dump), fixture) {
		t.Errorf("the explain dump carries the secret fixture value %q", fixture)
	}
}

// TestServerProfileBoundariesAreUnimplemented pins the two SPEC-12 §7 items this
// tree cannot satisfy, so the boundary is visible in the suite instead of being
// silently absent. It asserts today's shape, and the assertion is meant to be
// REPLACED (not deleted) when the missing machinery lands:
//
//   - the `trouble topology` profile row (SPEC-12 §3.7a) needs a decision shape
//     that can carry a non-rung row: types.TopologyDecision's From/To are
//     rung-typed (T1..T5) and the profile is orthogonal to the rungs, so a row
//     here would have to invent a rung mapping the spec explicitly denies.
//   - the live profile switch refusal (TROUBLE-HUB-013) needs a reload path that
//     re-runs Resolve while the daemon serves; this tree has none, so there is
//     no switch to refuse.
//
// Both are reported on TRBL-027 as residual, not claimed as done.
func TestServerProfileBoundariesAreUnimplemented(t *testing.T) {
	path := writeConfigFile(t, `[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"

[server.duckbrain]
namespace = "trouble/host-1"
`)
	r, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	rows := TopologyDecisions(r.Config)
	if len(rows) != 4 {
		t.Fatalf("topology rows = %d, want the four T1..T5 rows; if the profile row landed, replace this test with the SPEC-12 §3.7a assertion", len(rows))
	}
	for _, row := range rows {
		for _, key := range row.ConfigKeys {
			if strings.HasPrefix(key, "server.") {
				t.Fatalf("topology row %s→%s names %s: the profile row is not implemented yet — update this test and cover the required keys", row.From, row.To, key)
			}
		}
	}
}
