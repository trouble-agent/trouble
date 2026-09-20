package skills

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// goldenArtifactTOML is the §3.1 example, byte-for-byte (including the pinned
// signature, whose private key is the spec author's and is not in this repo).
const goldenArtifactTOML = `name               = "payment-worker-queue-wedge"
version            = 3
sigs               = ["sentinel:sha256v1:9f2c1d3e4b5a6c7d", "journald:sha256v1:2ab4c6d8e0f1a3b5"]
play_ref           = "plays/payment-worker-queue-wedge@3.toml"
min_daemon_version = "0.1.0"
allowed_modules    = ["proc.connections", "service.reload", "config.set"]
signer_key_id      = "skills-2026"
signature          = "EhHNesvuVNHJa+6HNj2xKS0FGfHrs76+pmGNUBB7+hVzHMphJ/GJD7NKwGolDngaoqL/m/Rq/MRjk6wBb17ZBA=="

[guards]
verify_window = "10m"
max_runs      = "3/day"
escalate_on   = "verify_fail"

[provenance]
incidents  = ["inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"]
research   = ["res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"]
author     = "troubled@hostA"
created_ts = "2026-09-11T04:00:00.000Z"
`

// GoldenCanonicalJSON is the §3.2 pinned projection.
const GoldenCanonicalJSON = `{"name":"payment-worker-queue-wedge","version":3,"sigs":["sentinel:sha256v1:9f2c1d3e4b5a6c7d","journald:sha256v1:2ab4c6d8e0f1a3b5"],"play_ref":"plays/payment-worker-queue-wedge@3.toml","guards":{"verify_window":"10m","max_runs":"3/day","escalate_on":"verify_fail"},"provenance":{"incidents":["inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"],"research":["res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"],"author":"troubled@hostA","created_ts":"2026-09-11T04:00:00.000Z"},"min_daemon_version":"0.1.0","allowed_modules":["proc.connections","service.reload","config.set"],"signer_key_id":"skills-2026"}`

// goldenCanonicalSHA256 and goldenPlaySHA256 are the §3.2 pinned digests. The
// play digest cannot be reproduced here: SPEC-11's §7.1 play fixture is absent
// from the spec, so the suite verifies the canonical form and the signature over
// the pinned bytes instead (see TestGoldenCanonicalFormPinnedSignature).
const (
	goldenCanonicalSHA256 = "b1ecc0e5d86018696bc2f25cb850e99d56350b33af8e74febde6ad2222de895c"
	goldenPlaySHA256      = "b4ae5a5b5788e560f03d11fe365a48aa02b1a03c995c069a09485a4d0d0bddec"
	goldenPublicKey       = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	goldenSignature       = "EhHNesvuVNHJa+6HNj2xKS0FGfHrs76+pmGNUBB7+hVzHMphJ/GJD7NKwGolDngaoqL/m/Rq/MRjk6wBb17ZBA=="
)

// playFixture is the three-task play the golden vector names (never nginx-*
// targets: the play only reads connections, reloads a user unit and sets a
// config key, which is what SPEC-06 §3.7's example does).
const playFixture = `schema_version = 1
name        = "payment-worker-queue-wedge"
version     = 3
source      = "module-default"
max_runs    = 2

[[task]]
name     = "count-connections"
tool     = "proc.connections"
args     = { unit = "payment-worker.service", proto = "tcp" }
when     = ""
register = "conns"
retries  = 0
on_fail  = "abort"

[[task]]
name     = "reload"
tool     = "service.reload"
args     = { unit = "payment-worker.service", scope = "user" }
when     = "conns.counts.established > 400"
register = "reload_result"
retries  = 1
on_fail  = "rollback"

[[task]]
name     = "raise-pool-max"
tool     = "config.set"
args     = { file = "/etc/payment-worker/app.toml", key = "pool.max", value = 250 }
when     = ""
register = "cfg"
retries  = 0
on_fail  = "abort"
`

func playBytes() []byte { return []byte(playFixture) }

// ── ledger double ───────────────────────────────────────────────────────────

type fakeLedger struct {
	mu   sync.Mutex
	recs []types.Record
	seq  uint64
	now  func() time.Time
	// failNext makes the next Append fail (the install-rollback path).
	failNext bool
}

func (f *fakeLedger) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return types.Record{}, fmt.Errorf("ledger write refused")
	}
	f.seq++
	ts := time.Now()
	if f.now != nil {
		ts = f.now()
	}
	rec := types.Record{
		Seq: f.seq, RecID: types.NewID(types.PEv), TS: types.FormatUTC(ts),
		Kind: d.Kind, SchemaVersion: 1, Sig: d.Sig, Inc: d.Inc,
		Origin: d.Origin, Actor: d.Actor, Redactions: d.Redactions, Payload: d.Payload,
	}
	f.recs = append(f.recs, rec)
	return rec, nil
}

func (f *fakeLedger) records() []types.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.Record(nil), f.recs...)
}

func (f *fakeLedger) phases() []string {
	var out []string
	for _, r := range f.records() {
		if r.Kind == types.KSkill {
			out = append(out, str(r.Payload, "phase"))
		}
	}
	return out
}

func (f *fakeLedger) byPhase(phase string) []types.Record {
	var out []types.Record
	for _, r := range f.records() {
		if r.Kind == types.KSkill && str(r.Payload, "phase") == phase {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeLedger) countPhase(phase string) int { return len(f.byPhase(phase)) }

func str(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// ── clock double ────────────────────────────────────────────────────────────

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	t0  time.Time
}

func newFakeClock() *fakeClock {
	t := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	return &fakeClock{now: t, t0: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.t0)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ── signing helper ──────────────────────────────────────────────────────────

// keyPair is a test signer: the suite mints its own ed25519 key rather than
// using the spec's (whose private half is not in the repo).
type keyPair struct {
	Signer types.SkillSigner
	priv   ed25519.PrivateKey
}

func newKeyPair(t *testing.T, keyID, trust string) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return keyPair{
		Signer: types.SkillSigner{
			KeyID: keyID, PublicKey: base64.StdEncoding.EncodeToString(pub),
			Trust: trust, Enabled: true, AddedTS: "2026-09-01T00:00:00.000Z",
		},
		priv: priv,
	}
}

// sign renders an artifact TOML with a valid signature for its canonical bytes.
func (kp keyPair) sign(t *testing.T, tomlBody string, play []byte) (types.Skill, string) {
	t.Helper()
	unsigned := strings.Replace(tomlBody, `signature          = "`+goldenSignature+`"`, `signature          = ""`, 1)
	skill, err := LoadArtifact([]byte(unsigned), play)
	if err != nil {
		t.Fatalf("load unsigned artifact: %v", err)
	}
	skill.SignerKeyID = kp.Signer.KeyID
	canonical, err := CanonicalBytes(skill, play)
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(kp.priv, canonical))
	signed := replaceSignatureLine(unsigned, sig)
	if _, err := LoadArtifact([]byte(signed), play); err != nil {
		t.Fatalf("load signed artifact: %v", err)
	}
	return skill, signed
}

// ── store/package helpers ───────────────────────────────────────────────────

func testDeps(t *testing.T, led *fakeLedger, clk *fakeClock, root string, registered ...string) Deps {
	t.Helper()
	return Deps{
		Ledger:        led,
		Clock:         clk,
		HostID:        "7f3a91c2d4e5b607",
		Actor:         types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0", GitSHA: "9c1f0ab"},
		StateRoot:     root,
		DaemonVersion: "0.1.0",
		Registered:    func() []string { return registered },
	}
}

func newTestSkills(t *testing.T, cfg types.SkillsConfig, registered ...string) (*Skills, *fakeLedger, *fakeClock) {
	t.Helper()
	led := &fakeLedger{}
	clk := newFakeClock()
	led.now = clk.Now
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(t.TempDir(), "skills-local")
	}
	s, err := New(cfg, testDeps(t, led, clk, t.TempDir(), registered...))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, led, clk
}

// ── git channel fixture ─────────────────────────────────────────────────────

// gitFixture writes a real release-channel repo: a work tree with a git dir, tags
// and artifacts, created with argv-only git so the fixture and the puller agree
// on what the channel is.
type gitFixture struct {
	dir string
	t   *testing.T
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "channel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture dir: %v", err)
	}
	f := &gitFixture{dir: dir, t: t}
	f.git(dir, "init", "--quiet", "-b", "main")
	f.git(dir, "config", "user.email", "troubled@hostA")
	f.git(dir, "config", "user.name", "troubled")
	return f
}

func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// writeSkill writes skills/<name>/SKILL.toml plus its play.
func (f *gitFixture) writeSkill(name string, version int, kp keyPair, sigs []string, modules []string, minDaemon string) string {
	f.t.Helper()
	dir := filepath.Join(f.dir, "skills", name)
	if err := os.MkdirAll(filepath.Join(dir, "plays"), 0o755); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	play := strings.Replace(playFixture, "payment-worker-queue-wedge", name, 1)
	play = strings.Replace(play, "version     = 3", fmt.Sprintf("version     = %d", version), 1)
	playPath := filepath.Join(dir, "plays", fmt.Sprintf("%s@%d.toml", name, version))
	if err := os.WriteFile(playPath, []byte(play), 0o644); err != nil {
		f.t.Fatalf("write play: %v", err)
	}
	body := fmt.Sprintf(`name               = %q
version            = %d
sigs               = [%s]
play_ref           = %q
min_daemon_version = %q
allowed_modules    = [%s]
signer_key_id      = %q
signature          = ""

[guards]
verify_window = "10m"
max_runs      = "3/day"
escalate_on   = "verify_fail"

[provenance]
incidents  = ["inc_8812"]
research   = ["res_2214"]
author     = "troubled@hostA"
created_ts = "2026-09-11T04:00:00.000Z"
`, name, version, quoteJoin(sigs), fmt.Sprintf("plays/%s@%d.toml", name, version),
		minDaemon, quoteJoin(modules), kp.Signer.KeyID)
	_, signed := kp.sign(f.t, body, []byte(play))
	if err := os.WriteFile(filepath.Join(dir, "SKILL.toml"), []byte(signed), 0o644); err != nil {
		f.t.Fatalf("write artifact: %v", err)
	}
	return signed
}

func (f *gitFixture) commit(msg string) {
	f.t.Helper()
	f.git(f.dir, "add", "-A")
	f.git(f.dir, "commit", "--quiet", "-m", msg)
}

func (f *gitFixture) tag(name string) {
	f.t.Helper()
	f.git(f.dir, "tag", name)
}

// bare makes a bare mirror of the work tree: the air-gapped / test source form.
func (f *gitFixture) bare() string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "release.git")
	f.git(f.dir, "clone", "--quiet", "--bare", f.dir, dir)
	return dir
}

// seedChannel writes a two-version channel and returns its bare path.
func seedChannel(t *testing.T, kp keyPair) (string, string) {
	t.Helper()
	f := newGitFixture(t)
	f.writeSkill("payment-worker-queue-wedge", 1, kp,
		[]string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d"}, []string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("v1")
	f.tag("v1.0.0")
	f.writeSkill("payment-worker-queue-wedge", 3, kp,
		[]string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d", "journald:sha256v1:2ab4c6d8e0f1a3b5"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("v3")
	f.tag("v1.2.0")
	return f.bare(), f.dir
}

// pullerCfg is a cfg pointing at a local channel with a review policy. The loop
// is enabled EXPLICITLY: SPEC-11 §2a ships the compiled default OFF, and a pull
// test wants a live loop.
func pullerCfg(source string, kp keyPair) types.SkillsConfig {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.SourcePath = source
	cfg.Approve = "auto"
	cfg.RequireSignature = true
	cfg.Signers = []types.SkillSigner{kp.Signer}
	cfg.PullMaxBytes = 1 << 20
	cfg.PullTimeout = "30s"
	cfg.PullInterval = "15m"
	return cfg
}
