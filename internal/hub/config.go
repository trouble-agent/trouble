package hub

import (
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-13 §2.1 default values for the runtime keys that are not already carried
// on types.ProfileConfig. They are repeated here (not re-derived) so the hub can
// be constructed in a test with a zero value and still behave like the shipped
// defaults; lifecycle's registry remains the single source for what a boot
// RESOLVES, and its resolved values overwrite every one of these.
const (
	DefaultRedisStream     = "trouble:ingest"
	DefaultConsumerGroup   = "ledger-writers"
	DefaultDedupPrefix     = "trouble:dedup:"
	DefaultDedupTTL        = 24 * time.Hour
	DefaultDedupLRU        = 65536
	DefaultBatchRecords    = 256
	DefaultBatchBytes      = 524288
	DefaultBlockMS         = 1000
	DefaultClaimMinIdle    = 60 * time.Second
	DefaultDialTimeout     = 2 * time.Second
	DefaultReadTimeout     = 5 * time.Second
	DefaultWriteTimeout    = 2 * time.Second
	DefaultMaxLen          = 1000000
	DefaultFailoverGrace   = 30 * time.Second
	DefaultArchiveInterval = time.Hour
	DefaultArchiveFiles    = 8
	DefaultKeepLocalGens   = 2
	// DefaultSuperviseInterval is the recovery loop's steady-state cadence: how
	// often the supervisor asks whether the queue is still live (attached, group
	// present, consumer draining). It is the cadence only while the queue IS
	// live — while it is not, the rewire is retried on the consumer's bounded
	// backoff (nextBackoff, ≈1–2s), so a Redis that comes back is wired in about
	// a second rather than at the next probe (SPEC-13 §4.3 "Redis returns": the
	// rewire happens with no external trigger).
	//
	// Two seconds is the detection window for the one case the serve path cannot
	// see (§2.1.1 rule 5: a cold server that still answers), and it bounds how
	// long a sender's 429 can outlive the server it was caused by. The probes
	// are a single XPENDING each, an order of magnitude less work than the
	// consumer's own idle blocking read.
	DefaultSuperviseInterval = 2 * time.Second
	// consumerStopTimeout bounds how long a rewire waits for the previous
	// consumer goroutine to return before REFUSING to start a second one
	// (SPEC-13 §1 rule 3: exactly one consumer per state root). A consumer idle
	// in XREADGROUP (BLOCK ≤ 1s) or in a bounded backoff (≤ 2s + jitter) returns
	// as soon as its context is cancelled, so this bound is a multiple of the
	// client's read timeout and cannot fire on a healthy queue.
	consumerStopTimeout = 15 * time.Second
	// DefaultDoorWait bounds how long the ingestion door waits for the
	// consumer's append+fsync before it refuses the request. It is deliberately
	// shorter than the sentinel's LedgerWait (2s) so the refusal — never a
	// silent 200 — is what the sender sees (SPEC-13 §4.2).
	DefaultDoorWait = 1500 * time.Millisecond
	// DefaultGapHold is how many consecutive reclaim rounds with no progress an
	// entry may accumulate before the consumer records TROUBLE-HUB-008.
	DefaultStrandedRounds = 3
)

// RedisConfig is the resolved `[server.redis]` surface the runtime consumes.
// Every field maps to exactly one registered key (SPEC-13 §2.1/§2.1.1); nothing
// here is a second config system.
type RedisConfig struct {
	URL        string
	Password   string // resolved from password_env at boot, never from argv (SPEC-12 §3.2)
	Stream     string
	Group      string
	Consumer   string // "" → HostID (one consumer per state root, SPEC-13 §3.3)
	MaxLen     int64
	DedupPfx   string
	DedupTTL   time.Duration
	BatchRecs  int
	BatchBytes int
	BlockMS    int
	ClaimIdle  time.Duration
	DialTO     time.Duration
	ReadTO     time.Duration
	WriteTO    time.Duration
	// RequireRedis=false means "degrade instead of refusing"; true means a
	// request that cannot be enqueued is never 200-answered (SPEC-13 §4.2).
	RequireRedis bool
	// RequirePersistence=true refuses a Redis whose appendonly is off (016).
	RequirePersistence bool
	// CheckPolicy=true reports a non-noeviction maxmemory-policy on the health
	// surface (a warning, never a refusal: §2.1.1 table).
	CheckPolicy   bool
	FailoverGrace time.Duration
	DedupLRU      int
	HostID        string
	DoorWait      time.Duration
}

// WithDefaults fills every empty field with its SPEC-13 §2.1 default. A zero
// RedisConfig is therefore a usable configuration for a test and never a
// crash-on-nil.
func (c RedisConfig) WithDefaults() RedisConfig {
	if c.Stream == "" {
		c.Stream = DefaultRedisStream
	}
	if c.Group == "" {
		c.Group = DefaultConsumerGroup
	}
	if c.Consumer == "" {
		c.Consumer = c.HostID
	}
	if c.MaxLen <= 0 {
		c.MaxLen = DefaultMaxLen
	}
	if c.DedupPfx == "" {
		c.DedupPfx = DefaultDedupPrefix
	}
	if c.DedupTTL <= 0 {
		c.DedupTTL = DefaultDedupTTL
	}
	if c.BatchRecs <= 0 {
		c.BatchRecs = DefaultBatchRecords
	}
	if c.BatchBytes <= 0 {
		c.BatchBytes = DefaultBatchBytes
	}
	if c.BlockMS <= 0 {
		c.BlockMS = DefaultBlockMS
	}
	if c.ClaimIdle <= 0 {
		c.ClaimIdle = DefaultClaimMinIdle
	}
	if c.DialTO <= 0 {
		c.DialTO = DefaultDialTimeout
	}
	if c.ReadTO <= 0 {
		c.ReadTO = DefaultReadTimeout
	}
	if c.WriteTO <= 0 {
		c.WriteTO = DefaultWriteTimeout
	}
	if c.FailoverGrace <= 0 {
		c.FailoverGrace = DefaultFailoverGrace
	}
	if c.DedupLRU <= 0 {
		c.DedupLRU = DefaultDedupLRU
	}
	if c.DoorWait <= 0 {
		c.DoorWait = DefaultDoorWait
	}
	return c
}

// ArchiveConfig is the resolved `[server.duckbrain]` surface plus the two local
// facts the archival job needs and that are not config keys: where the state
// root is, where the ledger lives, and which ledger file is LIVE (never a
// candidate: the writer is still appending to it).
type ArchiveConfig struct {
	Enabled            bool // implied true when profile = light-hub (SPEC-13 §2.1)
	Namespace          string
	Endpoint           string
	Interval           time.Duration
	BatchFiles         int
	KeepLocalGens      int
	VerifyAfterWriting bool
	Gzip               bool

	// StateRoot is <state_root> (the hub state dir is <StateRoot>/hub).
	StateRoot string
	// LedgerRoot is the ledger directory holding the generation files.
	LedgerRoot string
	// LiveFile is the name (not path) of the file the ledger writer is
	// appending to right now, from ledger.Status().File. An empty value means
	// "unknown", and PlanArchive then refuses to treat any file as closed
	// (011) rather than guessing.
	LiveFile string

	// KeyEnv / KeyFile / KeyHeader resolve the DuckBrain credential: the header
	// NAME is configuration, the VALUE never enters argv, a record or a log
	// (the same rule internal/issues' duckbrain driver follows).
	KeyEnv    string
	KeyFile   string
	KeyHeader string
}

// WithDefaults fills the archival defaults of SPEC-13 §2.1.
func (c ArchiveConfig) WithDefaults() ArchiveConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultArchiveInterval
	}
	if c.BatchFiles <= 0 {
		c.BatchFiles = DefaultArchiveFiles
	}
	if c.KeepLocalGens < 0 {
		c.KeepLocalGens = DefaultKeepLocalGens
	}
	if c.KeyEnv == "" {
		c.KeyEnv = "TROUBLE_DUCKBRAIN_KEY"
	}
	if c.KeyHeader == "" {
		c.KeyHeader = "Authorization"
	}
	return c
}

// ObjectKey is the generation's object path inside the namespace
// (`<namespace>/ledger/<file>.jsonl.gz`, SPEC-13 §3.5 step 2). It is derived
// from the generation file name, which carries the date, so the same
// generation maps to the same key on every host and every re-export.
func (c ArchiveConfig) ObjectKey(file string) string {
	return objectKey(c.Namespace, file, c.Gzip)
}

// SidecarKey is the `.idx.json` companion object that carries the SPEC-01
// §3.7a sidecar verbatim when the ledger wrote one.
func (c ArchiveConfig) SidecarKey(file string) string {
	return c.ObjectKey(file) + ".idx.json"
}

// objectKey is `<namespace>/ledger/<generation-file>[.gz]` (SPEC-13 §3.5 step 2,
// matching the SPEC-TYPES §3.15.11 example where `2026-09-16.1.gen.jsonl`
// becomes `…/ledger/2026-09-16.1.gen.jsonl.gz`). The generation file name
// already ends in `.jsonl`, so nothing is appended but the gzip marker.
func objectKey(ns, file string, gz bool) string {
	if gz {
		return ns + "/ledger/" + file + ".gz"
	}
	return ns + "/ledger/" + file
}

// redisConfigFrom projects the resolved lifecycle keys onto RedisConfig.
// It reads the registry-backed struct, never a second TOML decode, so a key
// added to the registry is visible here without a second registration path.
func redisConfigFrom(cfg lifecycle.Config) RedisConfig {
	r := cfg.Server.Redis
	c := RedisConfig{
		URL:                r.URL,
		Stream:             r.Stream,
		Group:              r.Group,
		Consumer:           r.Consumer,
		MaxLen:             r.MaxLen,
		DedupPfx:           r.DedupPrefix,
		DedupTTL:           r.DedupTTL.Std(),
		BatchRecs:          r.BatchRecords,
		BatchBytes:         r.BatchBytes,
		BlockMS:            r.BlockMS,
		ClaimIdle:          r.ClaimMinIdle.Std(),
		DialTO:             r.DialTimeout.Std(),
		ReadTO:             r.ReadTimeout.Std(),
		WriteTO:            r.WriteTimeout.Std(),
		RequireRedis:       r.RequireRedis,
		RequirePersistence: r.RequirePersistence,
		CheckPolicy:        r.CheckPolicy,
		FailoverGrace:      r.FailoverGrace.Std(),
		DedupLRU:           cfg.Hub.DedupLRU,
		HostID:             cfg.Origin.HostID,
	}
	return c.WithDefaults()
}

// archiveConfigFrom projects the resolved `[server.duckbrain]` keys.
func archiveConfigFrom(cfg lifecycle.Config, stateRoot, ledgerRoot, liveFile string) ArchiveConfig {
	d := cfg.Server.DuckBrain
	c := ArchiveConfig{
		Enabled:            d.Enabled,
		Namespace:          d.Namespace,
		Endpoint:           d.Endpoint,
		Interval:           d.ArchiveInterval.Std(),
		BatchFiles:         d.ArchiveBatchFiles,
		KeepLocalGens:      d.KeepLocalGens,
		VerifyAfterWriting: d.VerifyAfterWrite,
		Gzip:               d.Gzip,
		StateRoot:          stateRoot,
		LedgerRoot:         ledgerRoot,
		LiveFile:           liveFile,
	}
	return c.WithDefaults()
}

// profileConfigFrom projects the resolved `[server]` keys (SPEC-13 §2.1) using
// lifecycle's OWN validation, so the profile rule exists once. It is a pure
// projection: nothing is rewritten in place.
func profileConfigFrom(cfg lifecycle.Config) types.ProfileConfig { return cfg.ServerProfile() }
