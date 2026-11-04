package app

// plays_test.go — the play MATERIAL path (SPEC-06 §3.7, TRBL-123).
//
// Without these the ladder could advance a rung and still draft nothing: the
// shipped example plays must load through the registry's own parser, the daemon's
// rule→play lookup must resolve a rule-named play and the `default` fallback for a
// rule-less (sentinel-born) incident, and the resolved `registry.plays_dir` must
// be the directory actually read.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/registry"
	"github.com/trouble-agent/trouble/internal/types"
)

// shippedPlaysDir is the example plays directory, relative to this package.
const shippedPlaysDir = "../../examples/plays"

// TestShippedExamplePlaysLoadThroughTheRegistryParser is the docs-example gate:
// every play shipped in examples/plays parses and statically validates through
// registry.LoadPlay, so the material an operator copies cannot be a file the
// loader refuses.
func TestShippedExamplePlaysLoadThroughTheRegistryParser(t *testing.T) {
	entries, err := os.ReadDir(shippedPlaysDir)
	if err != nil {
		t.Fatalf("read %s: %v", shippedPlaysDir, err)
	}
	loaded := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		p, perr := registry.LoadPlay(filepath.Join(shippedPlaysDir, e.Name()))
		if perr != nil {
			t.Errorf("%s: registry.LoadPlay refused the shipped play: %v", e.Name(), perr)
			continue
		}
		if len(p.Tasks) == 0 {
			t.Errorf("%s: loaded a play with no tasks", e.Name())
		}
		loaded++
	}
	if loaded == 0 {
		t.Fatalf("no play loaded from %s", shippedPlaysDir)
	}
	// The two names the ladder resolves: a rule-named play and the rule-less
	// fallback.
	for _, want := range []string{"self_memory_pressure@1.toml", "default@1.toml"} {
		if _, err := os.Stat(filepath.Join(shippedPlaysDir, want)); err != nil {
			t.Errorf("the shipped play %s is missing: %v", want, err)
		}
	}
}

// playLookupTestDaemon builds the minimum daemon the lookup needs: a resolved
// config (state root + plays dir) and a logger.
func playLookupTestDaemon(t *testing.T, stateRoot, playsDir string) *Daemon {
	t.Helper()
	cfg := lifecycle.Config{StateRoot: stateRoot}
	cfg.Registry.PlaysDir = playsDir
	return &Daemon{
		Cfg: cfg,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// installPlay copies one shipped example play into dir under its own name.
func installPlay(t *testing.T, dir, file string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(shippedPlaysDir, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// TestPlayLookupResolvesRuleNamedAndDefaultPlays pins the resolution rule: a
// rule-driven incident drafts `plays/<rule>@<version>.toml`; a rule-less
// (sentinel-born) incident drafts `plays/default@<version>.toml`; a rule with no
// play resolves nothing, so the ladder degrades through its own T09 row instead
// of drafting an unrelated play.
func TestPlayLookupResolvesRuleNamedAndDefaultPlays(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "plays")
	installPlay(t, dir, "self_memory_pressure@1.toml")
	installPlay(t, dir, "default@1.toml")
	d := playLookupTestDaemon(t, root, dir)

	p, ok := d.playLookup("self_memory_pressure")
	if !ok {
		t.Fatal("playLookup(self_memory_pressure) found no play")
	}
	if p.Name != "self_memory_pressure" || p.Version != 1 {
		t.Fatalf("playLookup(self_memory_pressure) = %s@%d, want self_memory_pressure@1", p.Name, p.Version)
	}

	def, ok := d.playLookup("")
	if !ok {
		t.Fatal("playLookup(\"\") found no play: a rule-less sentinel incident must draft `default`")
	}
	if def.Name != "default" {
		t.Fatalf("playLookup(\"\") = %s, want default", def.Name)
	}

	if p, ok := d.playLookup("no_such_rule"); ok {
		t.Fatalf("playLookup(no_such_rule) = %s, want no play (the fallback is for a RULE-LESS incident only)", p.Name)
	}
}

// TestPlaysDirFollowsTheResolvedConfig pins that the lookup reads the resolved
// `registry.plays_dir` rather than a second hardcoded join.
func TestPlaysDirFollowsTheResolvedConfig(t *testing.T) {
	root := t.TempDir()
	configured := filepath.Join(root, "elsewhere")
	installPlay(t, configured, "default@1.toml")
	d := playLookupTestDaemon(t, root, configured)

	if _, ok := d.playLookup(""); !ok {
		t.Fatal("the play in the configured registry.plays_dir was not read")
	}
	if got := d.playsDir(); got != configured {
		t.Fatalf("playsDir() = %q, want the configured %q", got, configured)
	}

	// With no configured value the default is <state_root>/plays (SPEC-06 §3.7).
	fallback := playLookupTestDaemon(t, root, "")
	if got, want := fallback.playsDir(), filepath.Join(root, "plays"); got != want {
		t.Fatalf("playsDir() with no configured value = %q, want %q", got, want)
	}
}

// TestProductionDriverAdvancesASentinelIncidentToThePlayRung is the end-to-end
// regression for TRBL-123 on the REAL wiring: the daemon's own ladder, its own
// play lookup over a play file on disk, the sentinel→ladder hand-off, then the
// driver the cadence loop calls. No dashboard action, no direct Advance.
func TestProductionDriverAdvancesASentinelIncidentToThePlayRung(t *testing.T) {
	h := bootDaemon(t)
	if h.d.Ladder == nil || h.d.Store == nil {
		t.Fatal("the boot built no ladder/store")
	}
	installPlay(t, h.d.Cfg.Registry.PlaysDir, "default@1.toml")

	ctx := context.Background()
	sig := types.NewSig(types.SigSource("sentinel"), "sha256", 1, []byte("trbl-123-e2e-digest-0001"))
	rec, err := h.d.Store.Append(ctx, types.KGroup, sig.String(), "", map[string]any{
		"op": "create", "digest": sig.DigestHex(),
		"group_id": "grp_trbl123_0001", "title": "trbl-123 e2e class",
		"count": 1, "aggregate": true, "source": "collector",
		"first_seen_ts": types.FormatUTC(time.Now()), "last_seen_ts": types.FormatUTC(time.Now()),
		"events_upper_seq": 1, "norm_version": types.NormVersionV1,
	})
	if err != nil {
		t.Fatalf("group record: %v", err)
	}
	sentinelToLadder(h.d, rec)

	var incID string
	for _, o := range h.d.Ledger.DashReader().OpenIncidents(100) {
		if o.Sig == sig.String() {
			incID = o.ID
		}
	}
	if incID == "" {
		t.Fatal("the sentinel group's incident did not open")
	}

	rep, derr := h.d.Ladder.Drive(ctx)
	if derr != nil {
		t.Fatalf("Drive: %v", derr)
	}
	if len(rep.Advanced) != 1 || rep.Advanced[0] != incID {
		t.Fatalf("Drive advanced %v, want [%s] (considered %d, failed %v)", rep.Advanced, incID, rep.Considered, rep.Failed)
	}
	got, ierr := h.d.Ladder.Incident(ctx, incID)
	if ierr != nil {
		t.Fatalf("Incident: %v", ierr)
	}
	if got.State != types.StPlayDrafted || got.Rung != types.RungPlay {
		t.Fatalf("state/rung = %q/%q, want %q/%q", string(got.State), string(got.Rung),
			string(types.StPlayDrafted), string(types.RungPlay))
	}
}

// TestTheCadenceLoopAdvancesWithoutAnExplicitDrive is the AC's wiring gate: it
// proves the COMPOSITION ROOT drives the ladder, not that the driver works when
// something calls it. No Drive call is made anywhere in this test — the incident
// is injected the way a sensor or the sentinel injects one, and the only thing
// that may move it is the cadence goroutine started in the background_loops
// phase (SPEC-05 §4.4a).
//
// It is deliberately falsifiable: disable that goroutine and this test hangs out
// its deadline and fails, while the driver-level tests still pass — which is
// exactly why the row needed both.
func TestTheCadenceLoopAdvancesWithoutAnExplicitDrive(t *testing.T) {
	// The cadence is a resolved key, so the test passes it the way an operator
	// would (flag > env > file > default). The default 30s would make this test
	// a 30-second sleep; 50ms makes it a wiring assertion.
	bootExtraArgs = []string{"--ladder-drive_interval", "50ms"}
	h := bootDaemon(t)
	if got := h.d.Cfg.Ladder.DriveInterval; string(got) != "50ms" {
		t.Fatalf("resolved ladder.drive_interval = %q, want 50ms (the argv seam must reach the resolved config)", string(got))
	}
	if h.d.Ladder == nil || h.d.Store == nil {
		t.Fatal("the boot built no ladder/store")
	}
	installPlay(t, h.d.Cfg.Registry.PlaysDir, "default@1.toml")

	ctx := context.Background()
	sig := types.NewSig(types.SigSource("sentinel"), "sha256", 1, []byte("trbl-123-cadence-digest-0001"))
	rec, err := h.d.Store.Append(ctx, types.KGroup, sig.String(), "", map[string]any{
		"op": "create", "digest": sig.DigestHex(),
		"group_id": "grp_trbl123_cadence", "title": "trbl-123 cadence class",
		"count": 1, "aggregate": true, "source": "collector",
		"first_seen_ts": types.FormatUTC(time.Now()), "last_seen_ts": types.FormatUTC(time.Now()),
		"events_upper_seq": 1, "norm_version": types.NormVersionV1,
	})
	if err != nil {
		t.Fatalf("group record: %v", err)
	}
	sentinelToLadder(h.d, rec)

	var incID string
	for _, o := range h.d.Ledger.DashReader().OpenIncidents(100) {
		if o.Sig == sig.String() {
			incID = o.ID
		}
	}
	if incID == "" {
		t.Fatal("the sentinel group's incident did not open")
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		got, ierr := h.d.Ladder.Incident(ctx, incID)
		if ierr != nil {
			t.Fatalf("Incident: %v", ierr)
		}
		if got.State == types.StPlayDrafted && got.Rung == types.RungPlay {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("state/rung = %q/%q after 20s with NO Drive call: the cadence loop never ran (SPEC-05 §4.4a)", string(got.State), string(got.Rung))
		}
		time.Sleep(25 * time.Millisecond)
	}
}
