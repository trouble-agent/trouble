# SPEC-TYPES — shared type source of truth (trouble v0.1)

Spec: SPEC-TYPES
Area prefix: (none — this file defines the error-CODE catalog for every area)
Package: `internal/types`
Consumed types: — (this file defines them)
Local types: —
ACs: AC-1..AC-31 (all — every AC depends on the shared record/identity model)
PRD: §03, §06, §06b, §06c, §07, §08, §11

## 1. Purpose

Every Go type that crosses a package boundary in trouble is defined here, once, with a complete JSON
example. A spec may define package-private types, but only inside its own package and only when the type
never appears in an exported signature, an HTTP body, a TOML schema, a ledger payload, or another spec.
`internal/types` imports nothing from `internal/{ledger,scrub,sensors,sentinel,ladder,registry,research,flow,issues,dashboard,skills,lifecycle,hub}`
(dependency direction is one-way: subsystems import types, never the reverse).

This file is also the canonical error-code catalog (§5) and the canonical constants/enums (§6).

## 2. Interface

```go
// internal/types — package types. No exported funcs beyond the helpers named here.
func NewID(prefix Prefix) string          // prefix + ULID, monotonic within process
func ParseID(prefix Prefix, s string) (string, error)
func NewSig(source SigSource, algo SigAlgo, normVersion int, digest []byte) Sig
func (s Sig) String() string              // canonical sig string (see §6.3)
func ParseSig(s string) (Sig, error)
func (t PageToken) String() string        // "{generation_file}::{byte_offset}::{seq}" (§3.15.1)
func ParsePageToken(s string) (PageToken, error) // "" parses to the start token; malformed → error
func (d RouteDecision) Valid() bool       // "A" | "B" (§3.15.3)
func NowUTC() string                      // RFC3339 UTC, millisecond precision, always "Z"
func (t SigSource) Valid() bool
func (e ErrorCode) Is(class ErrorClass) bool
```

Sentinels:

```go
var (
    ErrPolicyRefused = errors.New("policy refused")   // maps to ErrorClassPolicyRefused
    ErrTransient     = errors.New("transient")
    ErrPermanent     = errors.New("permanent")
)
```

## 3. Data model

### 3.1 Envelope, identity, provenance

```go
type Origin struct {
    HostID string `json:"host_id"`   // stable machine id, NOT hostname: sha256(hardware+install-salt)[:16] or configured
    HubID  string `json:"hub_id"`    // "" for root hub; else the hub this record was received from
    Source string `json:"source"`    // free-form producer: "psi", "journald:payment-worker", "sentinel:myapp", "collector:legacy"
    Route  RouteDecision `json:"route"` // how the event reached the system: "A" direct (loopback) | "B" proxied (hub); "" for non-sensor paths (SPEC-04 §3.10a)
}

type Actor struct {
    Kind      ActorKind `json:"kind"`       // daemon | agent | play | human | satellite | external
    ID        string    `json:"id"`         // "troubled" | model name+run id | token label | satellite host_id
    Version   string    `json:"version"`    // binary semver of the acting process ("" for human)
    GitSHA    string    `json:"git_sha"`    // build git sha of the acting process
    BuildTime string    `json:"build_time"` // RFC3339 UTC
}

type Prefix string
const (
    PInc   Prefix = "inc_"  // incident
    PGrp   Prefix = "grp_"  // group
    PIss   Prefix = "iss_"  // issue
    PTsk   Prefix = "tsk_"  // board task row
    PSk    Prefix = "sk_"   // skill candidate / skill version
    PEv    Prefix = "ev_"   // ledger record id (rec_id) and canonical event id
    PRes   Prefix = "res_"  // research submission id (local)
    PSpawn Prefix = "sp_"   // spawn request id
)
```

```json
{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker","route":"A"}
{"kind":"daemon","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z"}
```

### 3.2 Records (ledger)

```go
type RecordKind string
const (
    KEvent     RecordKind = "event"      // scrubbed observation from any sensor/collector/sentinel path
    KGroup     RecordKind = "group"      // group create/update/compaction
    KIncident  RecordKind = "incident"   // incident create/transition/park/resume/reopen
    KPlayRun   RecordKind = "play_run"   // play draft/check/apply/fail
    KAgentRun  RecordKind = "agent_run"  // agent start/finish/suspend (budgeted)
    KToolCall  RecordKind = "tool_call"  // registry audit record (authorize→…→audit)
    KResearch  RecordKind = "research"   // research request/return/degraded/skipped
    KVerify    RecordKind = "verify"     // evidence-tuple verification result
    KIssue     RecordKind = "issue"      // issue ensure/comment/close/healthcheck
    KFlow      RecordKind = "flow"       // board row written, router dispatched
    KSpawn     RecordKind = "spawn"      // spawn requested/pending/accepted/failed/leased
    KSkill     RecordKind = "skill"      // candidate/promote/pull/refuse/local-stats
    KBreaker   RecordKind = "breaker"    // breaker open/half-open/close
    KGap       RecordKind = "gap"        // sensor/collector/PSI/verification dropout
    KCanary    RecordKind = "canary"     // canary injection + canary observation
    KConfig    RecordKind = "config"     // resolved-config snapshot / config change
    KLifecycle RecordKind = "lifecycle"  // boot, shutdown, park, upgrade, heartbeat-miss
)
```
(17 kinds. A subsystem may only emit the kinds listed for it in SPEC-INDEX §3.4.)

```go
type Record struct {
    Seq           uint64         `json:"seq"`            // monotonic per ledger file; writer is the ONLY allocator
    RecID         string         `json:"rec_id"`         // ev_ + ULID
    TS            string         `json:"ts"`             // RFC3339 UTC, millisecond precision, "Z"
    Kind          RecordKind     `json:"kind"`
    SchemaVersion int            `json:"schema_version"` // 1 for v0.1
    Sig           string         `json:"sig"`            // canonical sig string; "" when the record is not sig-keyed
    Inc           string         `json:"inc,omitempty"`  // inc_ + ULID when the record belongs to an incident
    Origin        Origin         `json:"origin"`
    Actor         Actor          `json:"actor"`
    Redactions    int            `json:"redactions"`     // count of values scrubbed (SPEC-02); never the values
    Payload       map[string]any `json:"payload"`        // kind-specific; schema pinned per kind in each spec
}
```

```json
{"seq":41207,"rec_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WD","ts":"2026-09-16T09:14:03.221Z","kind":"incident","schema_version":1,"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker","route":"A"},"actor":{"kind":"daemon","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z"},"redactions":3,"payload":{"transition":"detected→recorded","entry_rung":"play"}}
```

### 3.3 Scrubbing

```go
type ScrubRule struct {
    Name      string   `json:"name"`       // e.g. bearer_token, env_assign, private_key
    Kind      RuleKind `json:"kind"`       // regex | prefix | path_allowlist | dsn_part | entropy
    Pattern   string   `json:"pattern"`
    Replace   string   `json:"replace"`    // always a fixed marker: "[REDACTED:<name>]"
    Mandatory bool     `json:"mandatory"`  // mandatory rules cannot be disabled by config
    Targets   []string `json:"targets"`    // event_msg | stack | header | env | journal_tail | config_snapshot | skill | issue | board
}

type ScrubResult struct {
    Value      []byte `json:"value"`      // scrubbed bytes (never logged raw)
    Redactions int    `json:"redactions"`
    ByRule     map[string]int `json:"by_rule"`   // rule name → count (counts only, never values)
    Truncated  bool   `json:"truncated"`
    BytesIn    int    `json:"bytes_in"`
}
```

```json
{"name":"bearer_token","kind":"regex","pattern":"(?i)bearer\\s+[A-Za-z0-9._\\-]{8,}","replace":"[REDACTED:bearer_token]","mandatory":true,"targets":["header","event_msg","journal_tail"]}
```

### 3.4 Signature / dedup core

```go
type SigSource string
const (
    SrcSentinel  SigSource = "sentinel"
    SrcJournald  SigSource = "journald"
    SrcPSI       SigSource = "psi"
    SrcDBus      SigSource = "dbus"
    SrcDisk      SigSource = "disk"
    SrcTimers    SigSource = "timers"
    SrcInotify   SigSource = "inotify"
    SrcCollector SigSource = "collector"
    SrcGeneric   SigSource = "generic"
    SrcUnknown   SigSource = "unknown"
)

type Sig struct {
    Source      SigSource `json:"source"`
    Algo        string    `json:"algo"`         // "sha256" (only value in v0.1)
    NormVersion int       `json:"norm_version"` // normalization version; v0.1 = 1
    Digest      []byte    `json:"digest"`       // full 32-byte sha256 (grouping/dedup truth)
    Short       string    `json:"short"`        // hex(Digest)[:16] — display + ledger sig string
}
```

```json
{"source":"sentinel","algo":"sha256","norm_version":1,"digest":"1f8a...","short":"9f2c1d3e4b5a6c7d"}
```

### 3.5 Sensor / rule types (SPEC-03)

```go
type SensorKind string
const (
    SenPSI SensorKind = "psi"; SenJournald SensorKind = "journald"; SenDBus SensorKind = "dbus"
    SenDisk SensorKind = "disk"; SenTimers SensorKind = "timers"; SenInotify SensorKind = "inotify"
)

type SensorHealth struct {
    Sensor        SensorKind `json:"sensor"`
    Enabled       bool       `json:"enabled"`
    Degraded      bool       `json:"degraded"`
    Reason        string     `json:"reason"`          // "" when healthy; else human-readable cause
    LastSuccessTS string     `json:"last_success_ts"` // RFC3339 UTC
    LastEventTS   string     `json:"last_event_ts"`
    LastEventAgeS float64    `json:"last_event_age_s"`
    EventsTotal   uint64     `json:"events_total"`
    Gaps          int        `json:"gaps"`            // gap records emitted since boot
    Dropped       uint64     `json:"dropped"`         // bounded-queue drops
}

type SensorEvent struct {
    ID      string     `json:"id"`        // ev_ + ULID
    TS      string     `json:"ts"`
    Sensor  SensorKind `json:"sensor"`
    Scope   string     `json:"scope"`     // "cpu" | "memory" | "io" | unit name | mount | path | timer unit
    Sig     Sig        `json:"sig"`
    Value   float64    `json:"value"`
    Unit    string     `json:"unit"`      // "pct" | "bytes" | "count" | "seconds" | ""
    Detail  map[string]any `json:"detail"`
    Wake    bool       `json:"wake"`      // true when produced by a PSI trigger wake (accelerator), false when sampled
}

type Rule struct {
    Name        string      `json:"name"`
    Enabled     bool        `json:"enabled"`
    Source      SigSource   `json:"source"`
    Match       []Condition `json:"match"`
    For         Duration    `json:"for"`             // stabilization: condition must hold continuously
    EntryRung   Rung        `json:"entry_rung"`
    Severity    Severity    `json:"severity"`
    Cooldown    Duration    `json:"cooldown"`
    MaxRuns     int         `json:"max_runs"`        // play retries before next rung
    VerifyWin   Duration    `json:"verify_window"`
    AutoGrants  []string    `json:"auto_grants"`     // module/scope names auto-granted (assisted mode)
    Hotfix      bool        `json:"hotfix"`          // eligible for the hot-fix lane (SPEC-08)
}

type Condition struct {
    Field string `json:"field"`  // e.g. some_avg10, full_avg60, value, count, substr, unit_substate, free_pct
    Op    string `json:"op"`     // == != >= <= > < ~ (regex) in
    Value string `json:"value"`
    ValueType string `json:"value_type"` // number | string | bool
}

type Breaker struct {
    Scope      string `json:"scope"`       // rule:<name> | sig:<sig> | source:<kind> | global
    State      BreakerState `json:"state"` // closed | open | half_open
    OpenedTS   string `json:"opened_ts"`
    OpenUntil  string `json:"open_until"`
    Trips      int    `json:"trips"`
    Reason     string `json:"reason"`
}
```

```json
{"id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WF","ts":"2026-09-16T09:14:03.221Z","sensor":"psi","scope":"io","sig":"psi:sha256v1:5b1e77aa9c0d4f21","value":41.7,"unit":"pct","detail":{"some_avg10":41.7,"full_avg60":12.3,"armed":"some 150000 2000000"},"wake":true}
```

### 3.6 Sentinel types (SPEC-04)

```go
type Project struct {
    ID         string   `json:"id"`          // numeric-as-string, 1..2^31-1; path element in the DSN
    Slug       string   `json:"slug"`
    PublicKey  string   `json:"public_key"`  // 32 hex; the ONLY unredacted credential in the system
    SecretKey  string   `json:"secret_key"`  // 32 hex; "" when unused for this project
    AuthForms  []string `json:"auth_forms"`  // observed forms: x_sentry_auth | query_sentry_key | envelope_dsn
    QuotaEPM   int      `json:"quota_epm"`   // events per minute
    DiskBudget int64    `json:"disk_budget_bytes"`
    LossPolicy LossPolicy `json:"loss_policy"`
    Enabled    bool     `json:"enabled"`
    CreatedTS  string   `json:"created_ts"`
}

type LossPolicy string
const (
    LossSample        LossPolicy = "sample"          // deterministic 1/N sampling, counted
    LossDropCounter   LossPolicy = "drop-with-counter" // reject + 429, counted
    LossSpoolIfLight  LossPolicy = "spool-if-light"    // spool to disk when spool budget allows
)

type SentryEvent struct {
    ID         string     `json:"id"`         // SDK native event_id (32 hex), stored as native_id
    RecID      string     `json:"rec_id"`     // ev_ + ULID (canonical, hub-minted)
    Project    string     `json:"project"`
    TS         string     `json:"ts"`
    Level      string     `json:"level"`      // fatal|error|warning|info|debug
    Message    string     `json:"message"`
    Culprit    string     `json:"culprit"`
    Stack      string     `json:"stack"`      // canonicalized frames, already scrubbed
    Fingerprint []string  `json:"fingerprint"` // SDK-supplied strings, may contain "{{ default }}"
    Release    string     `json:"release"`
    Env        string     `json:"env"`
    Sig        Sig        `json:"sig"`
    Redactions int        `json:"redactions"`
    ItemTypes  []string   `json:"item_types"`  // every item type seen in the envelope, incl. dropped ones
}

type ClientReport struct {
    Project   string         `json:"project"`
    TS        string         `json:"ts"`
    Discarded []DiscardCount `json:"discarded"` // reason → count (queue_overflow, network_error, sample_rate, backpressure, ratelimit_backoff)
}

type DiscardCount struct {
    Reason string `json:"reason"`
    Category string `json:"category"` // error | transaction | session | attachment | other
    Quantity int  `json:"quantity"`
}

type RateLimitDecision struct {
    Allowed   bool   `json:"allowed"`
    Reason    string `json:"reason"`      // quota_epm | disk_budget | breaker | kill_switch | overloaded
    RetryAfterS int  `json:"retry_after_s"`
    Categories []string `json:"categories"`
    Header    string `json:"header"`      // exact X-Sentry-Rate-Limits value emitted
}
```

```json
{"id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f","rec_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WG","project":"1","ts":"2026-09-16T09:14:03.221Z","level":"error","message":"queue wedge: pool exhausted","culprit":"worker.claim","stack":"worker.py:118 claim\n  queue.py:44 get","fingerprint":["queue-wedge"],"release":"payment-api@2.4.1","env":"prod","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","redactions":1,"item_types":["event","client_report","session"]}
```

### 3.7 Group, incident, verification (SPEC-05)

```go
type Group struct {
    ID          string   `json:"id"`           // grp_ + ULID
    Sig         string   `json:"sig"`
    Digest      string   `json:"digest"`       // hex of the full 32-byte digest (dedup truth)
    Source      SigSource `json:"source"`
    Title       string   `json:"title"`
    FirstSeenTS string   `json:"first_seen_ts"`
    LastSeenTS  string   `json:"last_seen_ts"`
    Count       uint64   `json:"count"`         // events, forever
    Counters    GroupCounters `json:"counters"`
    ReleaseRange []string `json:"release_range"` // first_seen_release, last_seen_release
    IncidentID  string   `json:"incident_id"`   // current/open incident
    CompactedTS string   `json:"compacted_ts"`
}

type GroupCounters struct {
    Events     uint64 `json:"events"`
    Suppressed uint64 `json:"suppressed"`
    Redacted   uint64 `json:"redacted_values"`
    Dropped    uint64 `json:"dropped_events"`
    SampleRate float64 `json:"sample_rate"`
}

type Severity string
const (SevCritical Severity = "critical"; SevHigh Severity = "high"; SevMedium Severity = "medium"; SevLow Severity = "low"; SevInfo Severity = "info")

type Rung string
const (RungRecord Rung = "record"; RungPlay Rung = "play"; RungResearch Rung = "research"; RungAgent Rung = "agent"; RungOutlets Rung = "outlets")

type LadderState string
const (
    StDetected   LadderState = "detected"
    StRecorded   LadderState = "recorded"
    StPlayDrafted LadderState = "play:drafted"
    StPlayCheck  LadderState = "play:check_only"
    StPlayApplied LadderState = "play:applied"
    StPlayFailed LadderState = "play:failed"
    StResRequested LadderState = "research:requested"
    StResReturned LadderState = "research:returned"
    StResDegraded LadderState = "research:degraded"
    StResSkipped LadderState = "research:skipped"
    StAgentRunning LadderState = "agent:running"
    StAgentDone  LadderState = "agent:done"
    StAgentFailed LadderState = "agent:failed"
    StAgentSuspended LadderState = "agent:suspended"
    StVerifying  LadderState = "verifying"
    StResolved   LadderState = "resolved"
    StEscalated  LadderState = "escalated"
    StSuppressed LadderState = "suppressed"
    StQuarantined LadderState = "quarantined"
)

type Incident struct {
    ID          string      `json:"id"`          // inc_ + ULID
    Sig         string      `json:"sig"`
    GroupID     string      `json:"grp"`
    State       LadderState `json:"state"`
    EntryRung   Rung        `json:"entry_rung"`
    Rung        Rung        `json:"rung"`        // current rung
    Severity    Severity    `json:"severity"`
    OpenedTS    string      `json:"opened_ts"`
    UpdatedTS   string      `json:"updated_ts"`
    ResolvedTS  string      `json:"resolved_ts"`
    ReopenCount int         `json:"reopen_count"`
    PlayRuns    int         `json:"play_runs"`
    AgentRuns   int         `json:"agent_runs"`
    VerifyWin   Duration    `json:"verify_window"`
    LeaseID     string      `json:"lease_id"`    // agent lease holder, "" when free
    IssueID     string      `json:"issue_id"`    // iss_ + ULID
    TaskID      string      `json:"task_id"`     // tsk_ + ULID (board row)
    ResearchID  string      `json:"research_id"` // res_ + ULID
    Codeplane   *CodeplaneContext `json:"codeplane,omitempty"` // cross-plane bundle (SPEC-05 §3.13a)
    Evidence    *Evidence   `json:"evidence,omitempty"`
}

type Evidence struct {                            // verification is an EVIDENCE TUPLE, never a boolean
    TSWindowStart   string   `json:"ts_window_start"`
    TSWindowEnd     string   `json:"ts_window_end"`
    WindowS         float64  `json:"window_s"`
    EventsObserved  int      `json:"events_observed"`    // events with this sig inside the window
    CanarySeen      bool     `json:"canary_seen"`        // canary event observed inside the window
    CanaryID        string   `json:"canary_id"`
    CounterDeltas   map[string]int64 `json:"counter_deltas"` // per-source counter deltas inside the window
    SourcesExpected []string `json:"sources_expected"`   // host_id:source strings that MUST be quiet
    SourcesAlive    []string `json:"sources_alive"`      // subset that reported liveness in the window
    SourcesQuiet    []string `json:"sources_quiet"`
    SourcesMissing  []string `json:"sources_missing"`    // expected but no liveness → verification INVALID
    Zone            string   `json:"zone"`               // loopback | lan | tailnet | public
    Result          VerifyResultKind `json:"result"`
}

type VerifyResultKind string
const (VerifyPassed VerifyResultKind = "passed"; VerifyFailed VerifyResultKind = "failed"; VerifyInvalid VerifyResultKind = "invalid")

type GapRecord struct {
    ID       string `json:"id"`        // ev_ + ULID
    Sensor   string `json:"sensor"`    // sensor kind, collector name, "sentinel", "verification"
    Scope    string `json:"scope"`
    FromTS   string `json:"from_ts"`
    ToTS     string `json:"to_ts"`
    EstLost  int    `json:"est_lost"`  // best estimate; -1 when unknowable
    Cause    string `json:"cause"`     // open value set, e.g. cursor_invalid | child_died | queue_overflow | bus_down | psi_unarmed | canary_missing | heartbeat_stale | journal_unprivileged | file_rotated | file_truncated | file_removed | parser_error | spool_write_failed | ingest_reject_storm | driver_down | scrub_invalid_utf8 | research_lab_unreachable | research_strict_decoder_reject | research_solver_unavailable | research_poll_timeout | research_corpus_unreadable | forward_rejected | spool_evicted | codeplane_release_mismatch
}

type AutonomyMode string
const (AutoShadow AutonomyMode = "shadow"; AutoAssisted AutonomyMode = "assisted"; AutoFull AutonomyMode = "full")

type AutonomyGates struct {
    Mode            AutonomyMode `json:"mode"`
    KillSwitch      bool         `json:"kill_switch"`
    AllowDetect     bool         `json:"allow_detect"`
    AllowResearch   bool         `json:"allow_research"`
    AllowPlayMutate bool         `json:"allow_play_mutate"` // false in shadow: check_mode only
    AllowAgent      bool         `json:"allow_agent"`
    AllowSpawn      bool         `json:"allow_spawn"`
    AllowMerge      bool         `json:"allow_merge"`
    AllowPromote    bool         `json:"allow_promote"`
    AllowSkillAccept bool        `json:"allow_skill_accept"`
    Grants          []string     `json:"grants"`           // assisted-mode per-rule execute grants
    ChangedBy       string       `json:"changed_by"`       // Actor.ID
    ChangedTS       string       `json:"changed_ts"`
}
```

```json
{"id":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","grp":"grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH","state":"verifying","entry_rung":"play","rung":"outlets","severity":"high","opened_ts":"2026-09-16T09:14:03.221Z","updated_ts":"2026-09-16T09:15:41.009Z","verify_window":"10m","evidence":{"ts_window_start":"2026-09-16T09:15:41.009Z","ts_window_end":"2026-09-16T09:25:41.009Z","window_s":600,"events_observed":0,"canary_seen":true,"canary_id":"can_01J9Z6","counter_deltas":{"sentinel:payment-worker":3,"journald:payment-worker":0},"sources_expected":["7f3a91c2d4e5b607:sentinel:payment-worker","7f3a91c2d4e5b607:journald:payment-worker"],"sources_alive":["7f3a91c2d4e5b607:sentinel:payment-worker","7f3a91c2d4e5b607:journald:payment-worker"],"sources_quiet":["7f3a91c2d4e5b607:sentinel:payment-worker"],"sources_missing":[],"zone":"loopback","result":"passed"}}
```

### 3.8 Registry / module / play (SPEC-06)

```go
type IdempotencyClass string
const (IdemPure IdempotencyClass = "pure"; IdemConvergent IdempotencyClass = "convergent"; IdemOnce IdempotencyClass = "once")

type ErrorClass string
const (ErrClassTransient ErrorClass = "transient"; ErrClassPermanent ErrorClass = "permanent"; ErrClassPolicyRefused ErrorClass = "policy_refused")

type Descriptor struct {
    Name        string           `json:"name"`         // "config.set"
    Version     int              `json:"version"`       // module version, monotonic int
    Schema      map[string]any   `json:"schema"`        // JSON Schema (draft 2020-12), generated from the Go struct
    Scopes      []string         `json:"scopes"`        // required authorize scopes, e.g. ["config:write"]
    Idempotency IdempotencyClass `json:"idempotency"`
    CheckMode   bool             `json:"check_mode"`    // MUST be true for every shipped mutating module
    TimeoutS    int              `json:"timeout_s"`
    Mutating    bool             `json:"mutating"`
}

type DiffEntry struct {
    Path   string `json:"path"`
    Before any    `json:"before"`
    After  any    `json:"after"`
}

type Diff struct {
    Empty   bool        `json:"empty"`
    Entries []DiffEntry `json:"entries"`
    Summary string      `json:"summary"`
}

type Result struct {
    Changed   bool           `json:"changed"`
    Applied   []DiffEntry    `json:"applied"`
    Output    map[string]any `json:"output"`
    Rollback  *RollbackHint  `json:"rollback,omitempty"`
    DurationMS int           `json:"duration_ms"`
}

type RollbackHint struct {
    Supported bool   `json:"supported"`
    Module    string `json:"module"`   // module that inverts this call ("" when unsupported)
    Args      map[string]any `json:"args"`
}

type VerifyResult struct {
    OK      bool           `json:"ok"`
    Method  string         `json:"method"`   // probe | recheck | command | sensor
    Detail  map[string]any `json:"detail"`
    Evidence []DiffEntry   `json:"evidence"`
}

type Module interface {
    Descriptor() Descriptor
    Check(ctx context.Context, args map[string]any) (Diff, error)
    Apply(ctx context.Context, args map[string]any) (Result, error)
    Verify(ctx context.Context, args map[string]any) (VerifyResult, error)
}

type ToolCall struct {
    ID          string       `json:"id"`       // ev_ + ULID (ledger rec_id of this audit record)
    Module      string       `json:"module"`
    Args        map[string]any `json:"args"`   // post-scrub, post-redaction
    Mode        string       `json:"mode"`     // check_mode | apply
    Stage       []CallStage  `json:"stage"`    // authorize, validate, dry_run, apply, verify, audit
    CheckDiff   *Diff        `json:"check_diff,omitempty"`
    Result      *Result      `json:"result,omitempty"`
    Verify      *VerifyResult `json:"verify,omitempty"`
    Rolled      bool         `json:"rolled_back"`
    ErrorCode   string       `json:"error_code"`
    LatencyMS   int          `json:"latency_ms"`
    Inc         string       `json:"inc"`
    Actor       Actor        `json:"actor"`
}

type CallStage struct {
    Stage   string `json:"stage"`
    OK      bool   `json:"ok"`
    Detail  string `json:"detail"`
    MS      int    `json:"ms"`
}

type PlayTask struct {
    Name    string         `json:"name"`
    Tool    string         `json:"tool"`
    Args    map[string]any `json:"args"`
    When    string         `json:"when"`        // expression in the shared condition language (SPEC-03 §3.4)
    Register string        `json:"register"`    // variable name for this task's Result.Output
    Retries int            `json:"retries"`
    OnFail  string         `json:"on_fail"`     // abort | continue | rollback
}

type Play struct {
    Name       string     `json:"name"`
    Version    int        `json:"version"`
    Tasks      []PlayTask `json:"tasks"`
    CheckMode  bool       `json:"check_mode"`   // derived: true when every task's tool has CheckMode
    MaxRuns    int        `json:"max_runs"`
    Source     string     `json:"source"`       // "module-default" | "skill:<sk_id>" | "agent-draft"
}

type DoNotTouch struct {
    Paths  []string `json:"paths"`   // absolute paths or globs refused before validation
    Units  []string `json:"units"`   // systemd unit names refused for service.* / flow modules
    Scopes []string `json:"scopes"`  // scope names refused outright
}
```

```json
{"name":"config.set","version":1,"schema":{"type":"object","required":["path","key","value"],"properties":{"path":{"type":"string"},"key":{"type":"string"},"value":{}}},"scopes":["config:write"],"idempotency":"convergent","check_mode":true,"timeout_s":10,"mutating":true}
```

### 3.9 Research (SPEC-07)

```go
type ClassSlug struct {
    Slug       string `json:"slug"`        // kebab-case, from the config-maintained table
    Source     string `json:"source"`      // derived from Sig.Source
    AppKind    string `json:"app_kind"`    // unit | container | script | unknown
    Taxonomy   string `json:"taxonomy"`    // error taxonomy bucket, e.g. crash_loop, resource_exhaustion, config_error
    Fallback   bool   `json:"fallback"`    // true when the derivation fell back to "unknown"
}

type DiscoverRequest struct {
    ProblemClass string `json:"problem_class"`
    Env          string `json:"env"`
    Lang         string `json:"lang"`
    Version      string `json:"version"`
}

type DiscoverResponse struct {
    Found    bool           `json:"found"`
    Solution map[string]any `json:"solution"`
    CachedTS string         `json:"cached_ts"`
}

type SubmitRequest struct {                 // ONLY these four top-level fields — strict decoder
    ProblemClass string         `json:"problem_class"`
    Description  string         `json:"description"`
    Cadence      string         `json:"cadence"`   // enum accepted by the lab
    Context      map[string]any `json:"context"`   // carries fingerprint + stack + release + repro
}

type SubmitResponse struct {
    SubmissionID string `json:"submission_id"`
    Duplicate    bool   `json:"duplicate"`   // 409 → treat as queued
}

type QueueStatus struct {
    SubmissionID string `json:"submission_id"`
    State        string `json:"state"`       // queued | solving | solved | failed
    Solution     map[string]any `json:"solution"`
    ElapsedS     float64 `json:"elapsed_s"`
}

type ResearchOutcome struct {
    ID          string `json:"id"`           // res_ + ULID
    Inc         string `json:"inc"`
    Slug        ClassSlug `json:"slug"`
    State       string `json:"state"`        // requested | returned | degraded | skipped
    Driver      string `json:"driver"`       // off-by-one | none | webhook
    SubmissionID string `json:"submission_id"`
    Brief       map[string]any `json:"brief"`
    DegradedReason string `json:"degraded_reason"` // lab_unreachable | solver_unavailable | strict_decoder_reject | poll_timeout
    CorpusGrepHit bool `json:"corpus_grep_hit"`
    RequestedTS string `json:"requested_ts"`
    ReturnedTS  string `json:"returned_ts"`
}
```

```json
{"problem_class":"svc-crash-loop","description":"payment-worker restart loops, OOM-killed by cgroup","cadence":"recurring","context":{"fingerprint":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","stack":"worker.py:118 claim","release":"payment-api@2.4.1","unit":"payment-worker"}}
```

### 3.10 Flow / board / hot-fix (SPEC-08)

```go
type BoardRow struct {                      // fleet board-jsonl row shape (field-for-field)
    ID              string   `json:"id"`              // tsk_ + ULID (trouble allocates from board max+1 per the writer recipe)
    Title           string   `json:"title"`
    Status          string   `json:"status"`          // todo | in_progress | done | blocked
    Priority        string   `json:"priority"`        // P0..P3
    Complexity      string   `json:"complexity"`      // S | M | L
    DependsOn       []string `json:"depends_on"`
    Blocks          []string `json:"blocks"`
    PrimaryModel    string   `json:"primary_model"`
    PrimaryProvider string   `json:"primary_provider"`
    FallbackModel   string   `json:"fallback_model"`
    FallbackProvider string  `json:"fallback_provider"`
    Reasoning       string   `json:"reasoning"`
    CapabilityTags  []string `json:"capability_tags"`
    WorkerStatus    string   `json:"worker_status"`
    DispatchedAt    string   `json:"dispatched_at"`
    CompletedAt     string   `json:"completed_at"`
    Sig             string   `json:"sig"`             // trouble extension: dedup key
    Inc             string   `json:"inc"`             // trouble extension: incident id
    IssueRefs       []string `json:"issue_refs"`      // trouble extension
    Repo            string   `json:"repo"`            // trouble extension: target repo for hot-fix rows
}

type SpawnRequest struct {
    ID          string `json:"id"`           // sp_ + ULID
    Inc         string `json:"inc"`
    Sig         string `json:"sig"`
    Repo        string `json:"repo"`
    Worktree    string `json:"worktree"`     // <repo>/.worktrees/<task-id> — created by the repo's owning component
    TaskID      string `json:"task_id"`
    PriorityClass string `json:"priority_class"`
    Brief       string `json:"brief"`        // full foreman brief (scrubbed)
    ResearchBriefID string `json:"research_brief_id"`
    RouterRef   string `json:"router_ref"`   // router_spawn handle/run id
    State       string `json:"state"`        // requested | spawn_pending | accepted | failed | leased | released
    Attempts    int    `json:"attempts"`
    LastError   string `json:"last_error"`
    RequestedTS string `json:"requested_ts"`
}

type HotfixLease struct {
    Sig       string `json:"sig"`
    Inc       string `json:"inc"`
    Holder    string `json:"holder"`
    GrantedTS string `json:"granted_ts"`
    ExpiresTS string `json:"expires_ts"`
}

type Promotion struct {
    Inc       string `json:"inc"`
    TaskID    string `json:"task_id"`
    VerifyEvidence Evidence `json:"verify_evidence"`
    Decision  string `json:"decision"`     // promoted | discarded | pending_human
    DecidedBy string `json:"decided_by"`
    PRURL     string `json:"pr_url"`
    Worktree  string `json:"worktree"`
    DecidedTS string `json:"decided_ts"`
}

type FlowConfig struct {
    Driver                   string            `json:"driver"`                      // board-jsonl | task-router | none
    ReviewMode               string            `json:"review_mode"`                // auto | review | never
    BoardPath                string            `json:"board_path"`
    IDPrefix                 string            `json:"id_prefix"`                 // default "tsk_"
    ValidateCmd              string            `json:"validate_cmd"`               // "" = in-process validation only
    DependsOn                []string          `json:"depends_on"`
    ExtraTags                []string          `json:"extra_tags"`
    RegistrationProbeEvery   Duration          `json:"registration_probe_interval"` // default "5m"
    RegistrationStaleMax     Duration          `json:"registration_stale_max"`      // default "1h"
    SchedulerEndpoint        string            `json:"scheduler_endpoint"`         // registration proof probe
    SchedulerTokenFile       string            `json:"scheduler_token_file"`       // 0600, never argv
    Router                   RouterConfig      `json:"router"`
    Projects                 map[string]FlowProject `json:"projects"`
    Hotfix                   HotfixConfig      `json:"hotfix"`
}

type HotfixConfig struct {
    Enabled               bool              `json:"enabled"`                  // default false
    AllowedRepos          []string          `json:"allowed_repos"`             // default []
    UnitRepoMap           map[string]string `json:"unit_repo_map"`             // unit → repo root
    ForemanSpawn          string            `json:"foreman_spawn"`             // "router_spawn" only in v0.1
    PriorityClass         string            `json:"priority_class"`            // default "hotfix"
    VerifyWindow          Duration          `json:"verify_window"`             // default "10m"
    Promote               string            `json:"promote"`                   // human | auto-after-verify
    MaxConcurrent         int               `json:"max_concurrent"`            // default 2, hard cap 4
    MinFreeDiskGB         int64             `json:"min_free_disk_gb"`          // default 10
    WorktreeBase          string            `json:"worktree_base"`             // default ".worktrees" (repo-relative)
    WorktreeExempt        []string          `json:"worktree_exempt"`           // huge checkouts
    LeaseTTL              Duration          `json:"lease_ttl"`                 // default "30m"
    SpawnAckTimeout       Duration          `json:"spawn_ack_timeout"`         // default "5s"
    SpawnWorktreeTimeout  Duration          `json:"spawn_worktree_timeout"`    // default "30s"
    MaxAttempts           int               `json:"max_attempts"`              // default 5
    MinSeverity           Severity          `json:"min_severity"`              // default "high"
    MutexWait             Duration          `json:"mutex_wait"`                // default "5s"
    TestHintCmd           string            `json:"test_hint_cmd"`             // "" = no hints
    Models                map[string]string `json:"models"`                    // primary/fallback model+provider
    CapabilityTags        []string          `json:"capability_tags"`           // default ["hotfix","trouble"]
}
```

```json
{"id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","title":"hotfix: queue wedge in payment-worker (sentinel:sha256v1:9f2c1d3e4b5a6c7d)","status":"todo","priority":"P0","complexity":"S","depends_on":[],"blocks":[],"primary_model":"","primary_provider":"","fallback_model":"","fallback_provider":"","reasoning":"trouble hot-fix lane; sig-keyed; see inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","capability_tags":["hotfix","trouble"],"worker_status":"","dispatched_at":"","completed_at":"","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","issue_refs":["iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK"],"repo":"/srv/src/payment-api"}
```

### 3.11 Issues (SPEC-09)

```go
type IssueRef struct {
    ID         string `json:"id"`         // iss_ + ULID
    Driver     string `json:"driver"`     // github | duckbrain
    Sig        string `json:"sig"`
    ExternalID string `json:"external_id"` // gh issue number | duckbrain key
    URL        string `json:"url"`
    State      string `json:"state"`      // open | closed
    CreatedTS  string `json:"created_ts"`
    UpdatedTS  string `json:"updated_ts"`
    Comments   int    `json:"comments"`
    TaskID     string `json:"task_id"`
    ResearchID string `json:"research_id"`
}

type EnsureBySigRequest struct {
    Sig        string   `json:"sig"`
    Title      string   `json:"title"`
    Body       string   `json:"body"`         // scrubbed evidence bundle
    Labels     []string `json:"labels"`
    DedupWindow Duration `json:"dedup_window"`
    Severity   Severity `json:"severity"`
}

type EnsureBySigResponse struct {
    Ref      IssueRef `json:"ref"`
    Created  bool     `json:"created"`   // false → existing issue matched inside the dedup window
    Commented bool    `json:"commented"`
}

type IssueDriver interface {
    Name() string
    Healthcheck(ctx context.Context) (DriverHealth, error)
    EnsureBySig(ctx context.Context, req EnsureBySigRequest) (EnsureBySigResponse, error)
    Comment(ctx context.Context, ref IssueRef, body string) (IssueRef, error)
    Close(ctx context.Context, ref IssueRef, reason string) (IssueRef, error)
}

type DriverHealth struct {
    Driver    string `json:"driver"`
    OK        bool   `json:"ok"`
    Detail    string `json:"detail"`
    RateLimitRemaining int `json:"rate_limit_remaining"`
    RateLimitResetTS string `json:"rate_limit_reset_ts"`
    CheckedTS string `json:"checked_ts"`
}
```

```json
{"id":"iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK","driver":"github","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","external_id":"42","url":"https://github.com/acme/payment-api/issues/42","state":"open","created_ts":"2026-09-16T09:15:00.000Z","updated_ts":"2026-09-16T09:15:00.000Z","comments":1,"task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","research_id":"res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"}
```

### 3.12 Dashboard (SPEC-10)

```go
type HealthResponse struct {                 // GET /health.json — also consumed by the external stall checker
    Status         string            `json:"status"`      // ok | degraded | stalled
    Version        string            `json:"version"`
    GitSHA         string            `json:"git_sha"`
    BuildTime      string            `json:"build_time"`
    UptimeS        float64           `json:"uptime_s"`
    LedgerLastSeq  uint64            `json:"ledger_last_seq"`
    LedgerLastTS   string            `json:"ledger_last_ts"`
    LedgerStallS   float64           `json:"ledger_stall_s"`
    Sensors        []SensorHealth    `json:"sensors"`
    Sources        []SourceLiveness  `json:"sources"`
    Autonomy       AutonomyGates     `json:"autonomy"`
    Breakers       []Breaker         `json:"breakers"`
    RW             RuntimeWatermarks `json:"runtime_watermarks"`
    Hub            *HubStatus        `json:"hub,omitempty"` // server-profile stanza (SPEC-13); absent in `standalone`
    Subsystems     []SubsystemHealth `json:"subsystems"`    // one row per late-landing subsystem (SPEC-12 §3.3a)
}

type SubsystemHealth struct {                // per-subsystem built/refused row (SPEC-12 §3.3a)
    Name    string `json:"name"`             // sentinel | issues | research | flow | skills
    Built   bool   `json:"built"`            // true only while the composition root holds a live subsystem
    Refused bool   `json:"refused"`          // true when the boot recorded a refusal for this subsystem
    Code    string `json:"code,omitempty"`   // the refusal's TROUBLE-*-NNN code ("" when it carries none)
    Reason  string `json:"reason,omitempty"` // the refusal's own detail: a config key or an unwired driver, never a secret
}

type SourceLiveness struct {                 // per-source liveness expectation (verification input)
    HostID        string `json:"host_id"`
    Source        string `json:"source"`
    Zone          string `json:"zone"`
    Expected      bool   `json:"expected"`
    Alive         bool   `json:"alive"`
    LastEventTS   string `json:"last_event_ts"`
    LastEventAgeS float64 `json:"last_event_age_s"`
    MaxAgeS       float64 `json:"max_age_s"`
}

type RuntimeWatermarks struct {
    BinaryBytes   int64 `json:"binary_bytes"`
    RSSBytes      int64 `json:"rss_bytes"`
    RSSPeakBytes  int64 `json:"rss_peak_bytes"`
    MemHighBytes  int64 `json:"mem_high_bytes"`
    MemMaxBytes   int64 `json:"mem_max_bytes"`
    LedgerBytes   int64 `json:"ledger_bytes"`
    SpoolBytes    int64 `json:"spool_bytes"`
    SpoolBudgetBytes int64 `json:"spool_budget_bytes"`
    EventsPerMin  float64 `json:"events_per_min"`
    GroupsOpen    int   `json:"groups_open"`
    IncidentsOpen int   `json:"incidents_open"`
    Worktrees     int   `json:"worktrees"`
}

type Token struct {
    ID       string   `json:"id"`         // token label, e.g. "dash-read@phone"
    Hash     string   `json:"hash"`       // sha256(token)[:32] hex — plaintext never stored
    Scopes   []Scope  `json:"scopes"`     // read | write | autonomy
    CreatedTS string  `json:"created_ts"`
    Revoked  bool     `json:"revoked"`
    LastUsedTS string `json:"last_used_ts"`
}

type Scope string
const (ScopeRead Scope = "read"; ScopeWrite Scope = "write"; ScopeAutonomy Scope = "autonomy")
```

```json
{"status":"ok","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z","uptime_s":3881.4,"ledger_last_seq":41207,"ledger_last_ts":"2026-09-16T09:14:03.221Z","ledger_stall_s":1.2,"sensors":[{"sensor":"psi","enabled":true,"degraded":false,"reason":"","last_success_ts":"2026-09-16T09:14:03.000Z","last_event_ts":"2026-09-16T09:14:03.221Z","last_event_age_s":0.2,"events_total":912,"gaps":0,"dropped":0}],"sources":[{"host_id":"7f3a91c2d4e5b607","source":"sentinel:payment-worker","zone":"loopback","expected":true,"alive":true,"last_event_ts":"2026-09-16T09:14:03.221Z","last_event_age_s":0.2,"max_age_s":300}],"autonomy":{"mode":"shadow","kill_switch":false,"allow_detect":true,"allow_research":true,"allow_play_mutate":false,"allow_agent":true,"allow_spawn":true,"allow_merge":false,"allow_promote":false,"allow_skill_accept":false,"grants":[],"changed_by":"troubled","changed_ts":"2026-09-16T09:00:00.000Z"},"breakers":[],"runtime_watermarks":{"binary_bytes":10485760,"rss_bytes":41943040,"rss_peak_bytes":52428800,"mem_high_bytes":201326592,"mem_max_bytes":268435456,"ledger_bytes":30408704,"spool_bytes":0,"spool_budget_bytes":268435456,"events_per_min":18.4,"groups_open":7,"incidents_open":2,"worktrees":1},"subsystems":[{"name":"sentinel","built":true,"refused":false},{"name":"issues","built":true,"refused":false},{"name":"research","built":true,"refused":false},{"name":"flow","built":true,"refused":false},{"name":"skills","built":true,"refused":false}],"hub":{"enabled":true,"profile":"light-hub","since":"2026-09-16T09:00:00.000Z","redis":{"stream":"trouble:ingest","group":"ledger-writers","consumer":"7f3a91c2d4e5b607","stream_len":18422,"pending":12,"lag":181,"last_delivered_id":"1758012841221-318","last_acked_id":"1758012841221-306","reclaims":2,"dedup_hits":37,"dedup_misses":18422,"dedup_conflicts":1,"dedup_window":"redis","backpressure_total":0,"degraded":false,"degraded_reason":"","since":"2026-09-16T09:00:00.000Z"},"archive_queue":3,"archive_last_ts":"2026-09-16T08:20:00.000Z","archived_files":41,"droppable_generations":2,"marker_pending":3,"route_counters":{"A":9021,"B":9401},"degraded":false,"degraded_reason":""}}
```
A refused subsystem replaces its row instead of the status string: `{"name":"sentinel","built":false,"refused":true,"code":"TROUBLE-LIFECYCLE-001","reason":"TROUBLE-LIFECYCLE-001: no sentinel projects configured (SPEC-12 §3.1)"}` with `status` at most `degraded` (SPEC-12 §3.3a).

### 3.13 Skills (SPEC-11)

```go
type Skill struct {                          // SKILL.toml — IMMUTABLE artifact from the skills repo
    Name         string     `json:"name"`
    Version      int        `json:"version"`             // int, monotonic per name
    Sigs         []string   `json:"sigs"`
    PlayRef      string     `json:"play_ref"`            // plays/<name>@<version>.toml
    Guards       SkillGuards `json:"guards"`
    Provenance   Provenance `json:"provenance"`
    MinDaemonVersion string `json:"min_daemon_version"`
    AllowedModules []string `json:"allowed_modules"`
    Signature    string     `json:"signature"`           // ed25519 over the canonical artifact bytes
    SignerKeyID  string     `json:"signer_key_id"`
}

type SkillGuards struct {
    VerifyWindow Duration `json:"verify_window"`
    MaxRuns      string   `json:"max_runs"`       // "<n>/day"
    EscalateOn   string   `json:"escalate_on"`    // verify_fail | tool_error | never
}

type Provenance struct {
    Incidents []string `json:"incidents"`
    Research  []string `json:"research"`
    Author    string   `json:"author"`
    CreatedTS string   `json:"created_ts"`
}

type SkillStats struct {                     // LOCAL ONLY — never written back into the artifact
    Name      string `json:"name"`
    Version   int    `json:"version"`
    Applied   int    `json:"applied"`
    Success   int    `json:"success"`
    LastUsedTS string `json:"last_used_ts"`
    Refusals  int    `json:"refusals"`
}

type SkillCandidate struct {
    ID         string `json:"id"`         // sk_ + ULID
    Name       string `json:"name"`
    Version    int    `json:"version"`
    Sig        string `json:"sig"`
    Inc        string `json:"inc"`
    Play       Play   `json:"play"`
    ResearchID string `json:"research_id"`
    State      string `json:"state"`      // drafted | reviewed | promoted | rejected | pulled | refused
    ReviewActor string `json:"review_actor"`
    BranchOrPR string `json:"branch_or_pr"`
    CreatedTS  string `json:"created_ts"`
}
```

```json
{"name":"payment-worker-queue-wedge","version":3,"sigs":["sentinel:sha256v1:9f2c1d3e4b5a6c7d","journald:sha256v1:2ab4c6d8e0f1a3b5"],"play_ref":"plays/payment-worker-queue-wedge@3.toml","guards":{"verify_window":"10m","max_runs":"3/day","escalate_on":"verify_fail"},"provenance":{"incidents":["inc_8812","inc_8907"],"research":["res_2214"],"author":"troubled@hostA","created_ts":"2026-09-11T04:00:00.000Z"},"min_daemon_version":"0.1.0","allowed_modules":["proc.connections","service.reload","config.set"],"signature":"base64-ed25519-signature","signer_key_id":"skills-2026"}
```

### 3.14 Lifecycle / topology / forwarding (SPEC-12)

```go
type ConfigValue struct {
    Key        string `json:"key"`         // dotted path, e.g. sentinel.bind
    Value      any    `json:"value"`
    Source     string `json:"source"`      // flag | env | file | default
    SourceRef  string `json:"source_ref"`  // "--sentinel-bind" | "TROUBLE_SENTINEL_BIND" | "/etc/trouble/config.toml:12" | "builtin"
    Redacted   bool   `json:"redacted"`
}

type Heartbeat struct {
    TS            string `json:"ts"`
    PID           int    `json:"pid"`
    Version       string `json:"version"`
    GitSHA        string `json:"git_sha"`
    LedgerLastSeq uint64 `json:"ledger_last_seq"`
    LedgerLastTS  string `json:"ledger_last_ts"`
    Sensors       map[string]string `json:"sensors"`   // sensor → last-success RFC3339
}

type ForwardEnvelope struct {                 // satellite→hub (or proxy→hub): THE SENTINEL WIRE FORMAT
    ProtocolVersion int    `json:"protocol_version"`   // 1
    IdempotencyKey  string `json:"idempotency_key"`    // sig + "|" + norm_version + "|" + host_id
    HostID          string `json:"host_id"`
    HubID           string `json:"hub_id"`
    Origin          Origin `json:"origin"`
    Records         []Record `json:"records"`          // complete ledger records, hub assigns canonical ids
    Ack             uint64 `json:"ack"`                // ordered cursor: highest hub seq the satellite may forget
}

type SpoolEntry struct {
    ID        string `json:"id"`        // ev_ + ULID
    TS        string `json:"ts"`
    Kind      string `json:"kind"`      // forward | issue | spawn | skill
    Payload   []byte `json:"payload"`
    Attempts  int    `json:"attempts"`
    IdemKey   string `json:"idem_key"`
    NextTryTS string `json:"next_try_ts"`
}

type Topology string
const (T1 Topology = "T1"; T2 Topology = "T2"; T3 Topology = "T3"; T4 Topology = "T4"; T5 Topology = "T5")

type TopologyDecision struct {
    From, To            Topology `json:"from,to"`
    Decision            string   `json:"decision"`
    ConfigKeys          []string `json:"config_keys"`
}

type Duration string   // canonical Go type for every duration in TOML/JSON: "10m", "2s", "1h30m"; parsed by time.ParseDuration
type Prefix… // (see §3.1)
```

```json
{"protocol_version":1,"idempotency_key":"sentinel:sha256v1:9f2c1d3e4b5a6c7d|1|7f3a91c2d4e5b607","host_id":"7f3a91c2d4e5b607","hub_id":"c0ffee1234567890","origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"c0ffee1234567890","source":"sentinel:payment-worker"},"records":[],"ack":41207}
```

### 3.15 Types contributed by the area specs (folded — this file is the single source of truth)

Each subsection below is the **authoritative** definition of the named types; the contributing spec restates them
with documentation comments, and the signatures must remain compatible apart from those comments. A type may not
be defined with a different shape in two places: a field change lands here first. Package-private plumbing
(`Config`, `Deps`, `Observation`, `driverXxx`-style structs) is package-scoped and repeats per package by design —
it is never a shared type.

#### 3.15.1 Contributed by SPEC-01

_SPEC-01 — ledger: append-only audit ledger, durability and query index (trouble v0.1)_

```go
type RecordDraft struct {
    Kind       RecordKind     `json:"kind"`
    Sig        string         `json:"sig"`
    Inc        string         `json:"inc"`
    Origin     Origin         `json:"origin"`
    Actor      Actor          `json:"actor"`
    Redactions int            `json:"redactions"`
    Payload    map[string]any `json:"payload"`
}

type QueryInfo struct {
    Indexed           bool   `json:"indexed"`
    Partial           bool   `json:"partial"`
    Degraded          bool   `json:"degraded"`
    Reason            string `json:"reason"`             // "" | outside_index_window | cold_read_cap | ledger_degraded
    ScannedBytes      int64  `json:"scanned_bytes"`
    ScannedLines      int64  `json:"scanned_lines"`
    TruncatedBeforeTS string `json:"truncated_before_ts"`
    ElapsedMS         int    `json:"elapsed_ms"`
}

type IndexStats struct {
    Entries            int     `json:"entries"`
    Groups             int     `json:"groups"`
    GroupsCold         int     `json:"groups_cold"`
    Incidents          int     `json:"incidents"`
    Sources            int     `json:"sources"`
    Days               int     `json:"days"`
    FilesScanned       int     `json:"files_scanned"`
    BytesScanned       int64   `json:"bytes_scanned"`
    LinesScanned       int64   `json:"lines_scanned"`
    TornLines          int     `json:"torn_lines"`
    CorruptLines       int     `json:"corrupt_lines"`
    SkippedNewerSchema int     `json:"skipped_newer_schema"`
    BuildMS            int     `json:"build_ms"`
    BudgetMS           int     `json:"budget_ms"`
    BudgetBytes        int64   `json:"budget_bytes"`
    IndexBytes         int64   `json:"index_bytes"`
    ScanRateMiBs       float64 `json:"scan_rate_mibs"`
    Degraded           bool    `json:"degraded"`
    DegradedReason     string  `json:"degraded_reason"`   // "" | budget_exceeded | io_error | version_gate
    TruncatedBeforeTS  string  `json:"truncated_before_ts"`
    ColdEvictionsPerMin float64 `json:"cold_evictions_per_min"`
}

type LedgerStatus struct {
    LastSeq           uint64    `json:"last_seq"`           // durable watermark (fsynced)
    LastSeqPending    uint64    `json:"last_seq_pending"`   // allocated, not yet fsynced
    LastTS            string    `json:"last_ts"`
    StallS            float64   `json:"stall_s"`
    LossWindowMS      int       `json:"loss_window_ms"`     // §2.1, = fsync_window_ms
    Day               string    `json:"day"`
    Gen               int       `json:"gen"`
    Part              int       `json:"part"`
    File              string    `json:"file"`
    Bytes             int64     `json:"bytes"`
    Records           int64     `json:"records"`
    FsyncCalls        int64     `json:"fsync_calls"`
    FsyncPerRecord    float64   `json:"fsync_per_record"`
    QueueDepth        int       `json:"queue_depth"`
    QueueCap          int       `json:"queue_cap"`
    BackpressureTotal uint64    `json:"backpressure_total"`
    WriterPID         int       `json:"writer_pid"`
    WriterVersion     string    `json:"writer_version"`
    DiskBytes         int64     `json:"disk_bytes"`
    DiskBudgetBytes   int64     `json:"disk_budget_bytes"`
    Index             IndexStats `json:"index"`
}

type GroupStat struct {
    GroupID     string        `json:"group_id"`
    Sig         string        `json:"sig"`
    Digest      string        `json:"digest"`
    MergeKey    string        `json:"merge_key"`
    Source      string        `json:"source"`
    Title       string        `json:"title"`
    Count       uint64        `json:"count"`
    Rate1m      float64       `json:"rate_1m"`
    Rate5m      float64       `json:"rate_5m"`
    Rate60m     float64       `json:"rate_60m"`
    Trend       float64       `json:"trend"`       // rate_5m / previous 5m − 1
    FirstSeenTS string        `json:"first_seen_ts"`
    LastSeenTS  string        `json:"last_seen_ts"`
    IncidentID  string        `json:"incident_id"`
    Counters    GroupCounters `json:"counters"`
    Cold        bool          `json:"cold"`
}

type SourceAge struct {
    HostID        string  `json:"host_id"`
    Source        string  `json:"source"`
    Zone          string  `json:"zone"`         // loopback | lan | tailnet | public
    LastEventTS   string  `json:"last_event_ts"`
    LastEventAgeS float64 `json:"last_event_age_s"`
    LastSeq       uint64  `json:"last_seq"`
    EventsTotal   uint64  `json:"events_total"`
    Events24h     uint64  `json:"events_24h"`
    Gaps24h       int     `json:"gaps_24h"`
    Dropped24h    uint64  `json:"dropped_24h"`
    Redactions24h uint64  `json:"redactions_24h"`
    CanarySeen    bool    `json:"canary_seen"`
    CanaryLastTS  string  `json:"canary_last_ts"`
}

type EvidenceBundle struct {
    Inc        string           `json:"inc"`
    Sig        string           `json:"sig"`
    Digest     string           `json:"digest"`
    MergeKey   string           `json:"merge_key"`
    GroupID    string           `json:"group_id"`
    FirstSeq   uint64           `json:"first_seq"`
    LastSeq    uint64           `json:"last_seq"`
    Records    []Record         `json:"records"`
    KindCounts map[string]int   `json:"kind_counts"`
    RefCount   int              `json:"ref_count"`
    Overflow   int              `json:"overflow"`   // refs dropped by index_inc_per_incident
    Gaps       []GapRecord      `json:"gaps"`
    Partial    bool             `json:"partial"`
    Info       QueryInfo        `json:"info"`
}

type RotationPolicy struct {
    Cadence         string `json:"cadence"`          // "daily"
    AtUTC           string `json:"at_utc"`           // "00:00:00Z"
    MaxBytes        int64  `json:"max_bytes"`        // intra-day part rollover
    PartSuffix      string `json:"part_suffix"`      // ".p%02d"
    FsyncWindowMS   int    `json:"fsync_window_ms"`
    MaxBatchRecords int    `json:"max_batch_records"`
    QueueCapRecords int    `json:"queue_cap_records"`
    MaxEnqueueWait  string `json:"max_enqueue_wait"`
    Fdatasync       bool   `json:"fdatasync"`
    MaxRecordBytes  int64  `json:"max_record_bytes"`
}

type RetentionPolicy struct {
    RawKeep            Duration `json:"raw_keep"`
    CompactedKeep      Duration `json:"compacted_keep"`
    PayloadTTL         Duration `json:"payload_ttl"`
    TombstonesKeep     bool     `json:"tombstones_keep"`     // always true
    CompactionInterval Duration `json:"compaction_interval"`
    CompactionMinAge   Duration `json:"compaction_min_age"`
    CompactionMinBytes int64    `json:"compaction_min_bytes"`
    DiskBudgetBytes    int64    `json:"disk_budget_bytes"`
    DiskWarnPct        int      `json:"disk_warn_pct"`
    SpineKinds         []string `json:"spine_kinds"`
}

type CompactionResult struct {
    Day                 string   `json:"day"`
    FromFiles           []string `json:"from_files"`
    ToFile              string   `json:"to_file"`
    FromGen             int      `json:"from_gen"`
    ToGen               int      `json:"to_gen"`
    LinesIn             int64    `json:"lines_in"`
    LinesOut            int64    `json:"lines_out"`
    BytesIn             int64    `json:"bytes_in"`
    BytesOut            int64    `json:"bytes_out"`
    RecordsKept         int64    `json:"records_kept"`
    Aggregates          int64    `json:"aggregates"`
    PayloadsExpired     int64    `json:"payloads_expired"`
    PayloadsDropped     int64    `json:"payloads_dropped"`
    TombstoneCountsKept bool     `json:"tombstone_counts_kept"`
    ElapsedMS           int      `json:"elapsed_ms"`
    DryRun              bool     `json:"dry_run"`
    ErrorCode           string   `json:"error_code"`
}
```

```json
{"last_seq":41207,"last_seq_pending":41209,"last_ts":"2026-09-16T09:14:03.221Z","stall_s":1.2,"loss_window_ms":200,"day":"2026-09-16","gen":0,"part":1,"file":"2026-09-16.jsonl","bytes":30408704,"records":41207,"fsync_calls":11,"fsync_per_record":0.000267,"queue_depth":0,"queue_cap":65536,"backpressure_total":0,"writer_pid":2028599,"writer_version":"0.1.0","disk_bytes":30408704,"disk_budget_bytes":2147483648,"index":{"entries":41207,"groups":7,"groups_cold":0,"incidents":2,"sources":3,"days":1,"files_scanned":1,"bytes_scanned":30408704,"lines_scanned":41207,"torn_lines":1,"corrupt_lines":0,"skipped_newer_schema":0,"build_ms":412,"budget_ms":9000,"budget_bytes":536870912,"index_bytes":4194304,"scan_rate_mibs":148.2,"degraded":false,"degraded_reason":"","truncated_before_ts":"","cold_evictions_per_min":0}}
{"host_id":"7f3a91c2d4e5b607","source":"sentinel:payment-worker","zone":"loopback","last_event_ts":"2026-09-16T09:14:03.221Z","last_event_age_s":0.2,"last_seq":41207,"events_total":6401,"events_24h":6401,"gaps_24h":0,"dropped_24h":0,"redactions_24h":3,"canary_seen":true,"canary_last_ts":"2026-09-16T09:13:00.000Z"}
{"group_id":"grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","digest":"9f2c1d3e4b5a6c7d0123456789abcdef0123456789abcdef0123456789abcdef","merge_key":"bcc48494f2190f4f","source":"sentinel","title":"queue wedge: pool exhausted","count":6401,"rate_1m":18.4,"rate_5m":12.1,"rate_60m":6.7,"trend":0.52,"first_seen_ts":"2026-09-16T06:00:00.000Z","last_seen_ts":"2026-09-16T09:14:03.221Z","incident_id":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","counters":{"events":6401,"suppressed":0,"redacted_values":3,"dropped_events":0,"sample_rate":1},"cold":false}
```

**Page and sidecar types (SPEC-01 §2.3a, §3.7a):**

```go
type PageToken struct {                 // ledger page cursor; opaque to clients, stable within a generation
    Generation string `json:"generation"`  // "2026-09-16.1.gen.jsonl" — the file the cursor points into
    ByteOffset int64  `json:"byte_offset"` // offset into Generation, ≤ the file's size when the token was minted
    Seq        uint64 `json:"seq"`         // resume point if the file changed under the walk (part rollover, compaction)
}

type GenerationIndex struct {           // the {file}.idx sidecar: written at close, rebuilt if missing/mismatched
    File         string   `json:"file"`          // generation file name
    Records      int64    `json:"records"`
    Bytes        int64    `json:"bytes"`
    FirstSeq     uint64   `json:"first_seq"`
    LastSeq      uint64   `json:"last_seq"`
    MinTS        string   `json:"min_ts"`
    MaxTS        string   `json:"max_ts"`
    Offsets      []int64  `json:"offsets"`       // byte offset every OffsetStride records
    OffsetStride int      `json:"offset_stride"` // default 256
    TornLines    int      `json:"torn_lines"`
    Sha256       string   `json:"sha256"`        // hex of the file's bytes at close; the archive marker id derives from it
}
```

```json
{"generation":"2026-09-16.1.gen.jsonl","byte_offset":1835008,"seq":41207}
{"file":"2026-09-16.1.gen.jsonl","records":41207,"bytes":30408704,"first_seq":1,"last_seq":41207,"min_ts":"2026-09-16T00:00:00.104Z","max_ts":"2026-09-16T23:59:59.887Z","offsets":[0,262144,524288,786432,1048576,1310720,1572864,1835008],"offset_stride":256,"torn_lines":0,"sha256":"3f9a1c0d5b7e2416a8c93d0e1f4b6275c8d9e0f1a2b3c4d5e6f708192a3b4c5d"}
```

#### 3.15.2 Contributed by SPEC-02

_SPEC-02 — scrubbing subsystem (trouble v0.1)_

```go
type ScrubTarget string
const (
    TgEventMsg       ScrubTarget = "event_msg"
    TgStack          ScrubTarget = "stack"
    TgHeader         ScrubTarget = "header"
    TgEnv            ScrubTarget = "env"
    TgJournalTail    ScrubTarget = "journal_tail"
    TgConfigSnapshot ScrubTarget = "config_snapshot"
    TgSkill          ScrubTarget = "skill"
    TgIssue          ScrubTarget = "issue"
    TgBoard          ScrubTarget = "board"
    TgDSN            ScrubTarget = "dsn"
    TgSpool          ScrubTarget = "spool"
)

func (t ScrubTarget) Valid() bool

type ScrubStats struct {
    Calls            uint64            `json:"calls"`
    BytesIn          uint64            `json:"bytes_in"`
    BytesOut         uint64            `json:"bytes_out"`
    Redactions       uint64            `json:"redactions"`
    ByRule           map[string]uint64 `json:"by_rule"`
    Truncated        uint64            `json:"truncated"`
    RefusedBytes     uint64            `json:"refused_bytes"`
    InvalidUTF8      uint64            `json:"invalid_utf8"`
    Timeouts         uint64            `json:"timeouts"`
    FailClosed       uint64            `json:"fail_closed"`
    BoundaryRefusals uint64            `json:"boundary_refusals"`
    RulesVersion     int               `json:"rules_version"`
}
```

```json
"event_msg"
{"calls":41207,"bytes_in":43819008,"bytes_out":43819008,"redactions":912,"by_rule":{"env_assign":611,"bearer_token":188,"dsn_secret":113},"truncated":2,"refused_bytes":0,"invalid_utf8":0,"timeouts":0,"fail_closed":0,"boundary_refusals":0,"rules_version":1}
```

`ScrubRule` and `ScrubResult` already exist in SPEC-TYPES §3.3 and are used unchanged; `ScrubTarget` and
`ScrubStats` are reported as TYPES-GAP for the index owner to fold in.

#### 3.15.3 Contributed by SPEC-04

_SPEC-04 — sentinel: ingestion contract, grouping, releases, collectors (trouble v0.1)_

```go
type ProjectRuntime struct {          // per-project runtime state for SPEC-10 (quota usage, canary, loss)
    Project              string         `json:"project"`
    QuotaEPM             int            `json:"quota_epm"`
    WindowS              float64        `json:"window_s"`
    EventsWindow         int            `json:"events_window"`
    Remaining            int            `json:"remaining"`
    RejectedTotal        uint64         `json:"rejected_total"`
    DroppedTotal         uint64         `json:"dropped_total"`
    SpooledTotal         uint64         `json:"spooled_total"`
    SampledTotal         uint64         `json:"sampled_total"`
    LegacyStoreTotal     uint64         `json:"legacy_store_total"`
    UnknownItemsTotal    uint64         `json:"unknown_items_total"`
    ClientReportDiscards map[string]int `json:"client_report_discards"`
    AuthForms            []string       `json:"auth_forms"`
    LastEventTS          string         `json:"last_event_ts"`
    CanaryLastTS         string         `json:"canary_last_ts"`
    CanaryLastOK         bool           `json:"canary_last_ok"`
    DiskBytes            int64          `json:"disk_bytes"`
    DiskBudgetBytes      int64          `json:"disk_budget_bytes"`
}

type CollectorParser struct {         // config + health surface for the log collectors (SPEC-04 §3.5)
    Name          string     `json:"name"`           // "go-panic" | "py-traceback" | "node-reject"
    Enabled       bool       `json:"enabled"`
    Kind          string     `json:"kind"`           // multiline
    StartPattern  string     `json:"start_pattern"`  // RE2
    Continuation  []string   `json:"continuation"`   // RE2 list
    FlushTimeout  Duration   `json:"flush_timeout"`
    MaxEventBytes int        `json:"max_event_bytes"`
    Level         string     `json:"level"`
    SigFields     []string   `json:"sig_fields"`
    Sources       []string   `json:"sources"`        // "journal:<unit>" | "file:<path>"
}
```

```json
{"project":"7","quota_epm":600,"window_s":60,"events_window":41,"remaining":559,"rejected_total":3,"dropped_total":3,"spooled_total":0,"sampled_total":0,"legacy_store_total":1,"unknown_items_total":12,"client_report_discards":{"queue_overflow":2,"network_error":5},"auth_forms":["x_sentry_auth","query_sentry_key"],"last_event_ts":"2026-09-16T09:14:03.221Z","canary_last_ts":"2026-09-16T09:10:00.004Z","canary_last_ok":true,"disk_bytes":1048576,"disk_budget_bytes":2147483648}
{"name":"py-traceback","enabled":true,"kind":"multiline","start_pattern":"^Traceback \\(most recent call last\\):","continuation":["^  ","^File \"","^\\s+\\^","^raise ","^During handling","^The above exception"],"flush_timeout":"200ms","max_event_bytes":1048576,"level":"error","sig_fields":["exception_class","frame_last.function","frame_last.file"],"sources":["journal:legacy-daemon","file:/var/log/legacy/err.log"]}
```

**Sensor transport route types (SPEC-04 §3.10a):**

```go
type RouteMode string
const (
    RouteAuto   RouteMode = "auto"    // resolve per event: B when a hub endpoint is configured, else A
    RouteDirect RouteMode = "direct"  // force the local hop (A)
    RouteProxy  RouteMode = "proxy"   // force the hub hop (B); refused at boot when no hub endpoint is configured
)

type RouteDecision string
const (
    RouteA RouteDecision = "A"        // direct: sensor → local daemon over loopback
    RouteB RouteDecision = "B"        // proxied: sensor → local daemon → hub daemon (satellite forward path, SPEC-12 §3.7)
)

type RouteConfig struct {              // [sentinel.routes]
    Default  RouteMode            `json:"default"`    // auto | direct | proxy
    PerClass map[string]RouteMode `json:"per_class"`  // sig-prefix → route; longest prefix wins
}
```

```json
{"default":"auto","per_class":{"psi:io_pressure":"direct","sentinel:sha256v1":"proxy"}}
```

`GapRecord.Cause` gains the value `codeplane_release_mismatch` (SPEC-04 §3.9a) — reported as a TYPES-GAP line
so SPEC-TYPES §3.7's cause set stays the single source of truth.

#### 3.15.4 Contributed by SPEC-05

_SPEC-05 — ladder: state machine, verification, autonomy gates (trouble v0.1)_

`ParkRecord` — appears in a ledger payload and is cross-referenced by SPEC-08 (worktree reaping) and
SPEC-12 (upgrade/resume):

```go
type ParkRecord struct {
    ID             string   `json:"id"`              // ev_ + ULID: the park record's rec_id
    Inc            string   `json:"inc"`
    Sig            string   `json:"sig"`
    HostID         string   `json:"host_id"`
    Kind           string   `json:"kind"`            // play | agent | spawn
    State          LadderState `json:"state"`
    PlayRun        int      `json:"play_run"`
    TaskIndex      int      `json:"task_index"`
    ToolCallID     string   `json:"tool_call_id"`    // last COMPLETED tool call
    ToolsApplied   []string `json:"tools_applied"`   // rec_ids already applied in this run
    Worktree       string   `json:"worktree"`
    PID            int      `json:"pid"`
    RouterRef      string   `json:"router_ref"`
    ParkedTS       string   `json:"parked_ts"`
    ResumeDeadline string   `json:"resume_deadline"`
    Reason         string   `json:"reason"`          // sigterm | crash | upgrade | lease_expired | kill_switch
    ResumedTS      string   `json:"resumed_ts"`
    ResumedBy      string   `json:"resumed_by"`
}
```

```json
{"id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","host_id":"7f3a91c2d4e5b607","kind":"play","state":"play:check_only","play_run":1,"task_index":2,"tool_call_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WN","tools_applied":["service.reload"],"worktree":"","pid":0,"router_ref":"","parked_ts":"2026-09-16T09:31:00.004Z","resume_deadline":"2026-09-17T09:31:00.004Z","reason":"sigterm","resumed_ts":"","resumed_by":""}
```

`AgentLease` — ledger payload, surfaced by SPEC-10 `/health.json`, consumed by SPEC-12 at boot:

```go
type AgentLease struct {
    LeaseID       string `json:"lease_id"`
    Inc           string `json:"inc"`
    Sig           string `json:"sig"`
    HostID        string `json:"host_id"`
    Holder        string `json:"holder"`
    PID           int    `json:"pid"`
    Worktree      string `json:"worktree"`
    RouterRef     string `json:"router_ref"`
    State         string `json:"state"`       // requested | granted | renewed | released | expired | reclaimed
    GrantedTS     string `json:"granted_ts"`
    ExpiresTS     string `json:"expires_ts"`
    RenewedTS     string `json:"renewed_ts"`
    RenewCount    int    `json:"renew_count"`
    ReleaseReason string `json:"release_reason"`
}
```

```json
{"lease_id":"lease_01J9Z6Q0M2X4T8V1K7B3N5R8WP","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","host_id":"7f3a91c2d4e5b607","holder":"trouble-agent/run-4471","pid":88123,"worktree":"/srv/src/payment-api/.worktrees/tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","router_ref":"sp_01J9Z6Q0M2X4T8V1K7B3N5R8WQ","state":"granted","granted_ts":"2026-09-16T09:15:41.009Z","expires_ts":"2026-09-16T09:30:41.009Z","renewed_ts":"2026-09-16T09:20:41.009Z","renew_count":10,"release_reason":""}
```

`BudgetState` — ledger-derived counters, surfaced on the dashboard overview (SPEC-10 §2):

```go
type BudgetState struct {
    HostID   string           `json:"host_id"`
    Day      string           `json:"day"`        // YYYY-MM-DD (UTC)
    ResetsTS string           `json:"resets_ts"`
    Limits   map[string]int64 `json:"limits"`     // class → per-day limit
    Used     map[string]int64 `json:"used"`       // class → consumed
    Exhausted []string        `json:"exhausted"`  // classes at 0 remaining
}
```

```json
{"host_id":"7f3a91c2d4e5b607","day":"2026-09-16","resets_ts":"2026-09-17T00:00:00.000Z","limits":{"agent_runs":20,"play_runs":50,"research_requests":30,"spawns":5},"used":{"agent_runs":3,"play_runs":11,"research_requests":4,"spawns":1},"exhausted":[]}
```

#### 3.15.5 Contributed by SPEC-06

_SPEC-06 — registry: module SDK v1, the 6-stage call contract, plays, do-not-touch, polkit (trouble v0.1)_

```go
type ToolCallRequest struct {
    Module    string         `json:"module"`
    Args      map[string]any `json:"args"`
    Mode      string         `json:"mode"`        // check_mode | apply
    IdemKey   string         `json:"idem_key"`    // "<inc>|<play>@<ver>|<task_index>|<module>|<sha256(canonical args)[:16]>"
    Grants    []string       `json:"grants"`      // "service.reload" | "scope:service:write"
    Source    string         `json:"source"`      // "rule:<name>" | "play:<name>@<ver>" | "skill:<sk_id>" | "agent:<run_id>" | "cli"
    Inc       string         `json:"inc"`
    Rule      string         `json:"rule"`
    Actor     Actor          `json:"actor"`
    DeadlineS int            `json:"deadline_s"`  // caller budget; the effective timeout is min(Descriptor.TimeoutS, DeadlineS)
}

type PlayRun struct {
    ID        string    `json:"id"`             // ev_ + ULID (the play_run ledger record's rec_id)
    Play      string    `json:"play"`
    PlayVer   int       `json:"play_version"`
    Inc       string    `json:"inc"`
    Sig       string    `json:"sig"`
    Source    string    `json:"source"`         // module-default | skill:<sk_id> | agent-draft
    Mode      string    `json:"mode"`           // check_mode | apply
    StartedTS string    `json:"started_ts"`
    EndedTS   string    `json:"ended_ts"`
    Outcome   string    `json:"outcome"`        // drafted | check_only | applied | failed | rolled_back | parked
    Changed   bool      `json:"changed"`
    Tasks     []TaskRun `json:"tasks"`
}

type TaskRun struct {
    Index      int            `json:"index"`
    Name       string         `json:"name"`
    Tool       string         `json:"tool"`
    Status     string         `json:"status"`       // ok | changed | skipped | failed | refused
    SkippedBy  string         `json:"skipped_by"`   // the when: expression that evaluated false, "" otherwise
    Registers  map[string]any `json:"registers"`
    ToolCallID string         `json:"tool_call_id"`
    ErrorCode  string         `json:"error_code"`
    MS         int            `json:"ms"`
}

type RegistryDeps struct { /* §2.2 — function-typed collaborators; no cross-package interface */ }
```

JSON examples:

```json
{"module":"service.reload","args":{"unit":"payment-worker.service","scope":"user"},"mode":"apply","idem_key":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|service-reload-on-pool-exhaustion@3|1|service.reload|4b1e77aa9c0d4f21","grants":["proc.connections","service.reload"],"source":"play:service-reload-on-pool-exhaustion@3","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","rule":"pool-exhaustion","actor":{"kind":"play","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z"},"deadline_s":20}
{"id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WN","play":"service-reload-on-pool-exhaustion","play_version":3,"inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","source":"module-default","mode":"apply","started_ts":"2026-09-16T09:15:02.010Z","ended_ts":"2026-09-16T09:15:02.884Z","outcome":"applied","changed":true,"tasks":[{"index":0,"name":"count-connections","tool":"proc.connections","status":"ok","skipped_by":"","registers":{"conns":412},"tool_call_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","error_code":"","ms":6},{"index":1,"name":"reload","tool":"service.reload","status":"changed","skipped_by":"","registers":{"reloaded":true},"tool_call_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WP","error_code":"","ms":291}]}
{"append":"ledger.Writer","scrub":"scrub.Pipeline","gates":"ladder.Gates","file_issue":"flow.Subsystem","create_task":"flow.Subsystem","comment":"issues.Desk","dbus_address":"","now":"time.Now"}
```

(The third example is the JSON projection of `RegistryDeps` — Go function values have no JSON form, so
`trouble registry wiring --json` reports the bound implementation of each field.)

#### 3.15.6 Contributed by SPEC-08

_SPEC-08 — flow: board-jsonl row, task-router driver, hot-fix spawn (trouble v0.1)_

```go
type BoardEvent struct {
    ID      uint64         `json:"id"`       // MAX(id)+1 across the board's event file
    Type    string         `json:"type"`     // task_created | task_comment | review_approve | task_closed
    TaskID  string         `json:"task_id"`
    TS      string         `json:"ts"`
    Actor   string         `json:"actor"`    // "troubled@0.1.0" | "human:<label>"
    Detail  map[string]any `json:"detail"`
}

type FileTaskRequest struct {
    Project    string      `json:"project"`
    BoardPath  string      `json:"board_path"`
    Row        BoardRow    `json:"row"`
    Event      BoardEvent  `json:"event"`
    ReviewMode string      `json:"review_mode"`  // auto | review | never
    IdemKey    string      `json:"idem_key"`     // == Row.ID
    DryRun     bool        `json:"dry_run"`      // check_mode: returns the diff, writes nothing
}

type FileTaskResult struct {
    Wrote      bool      `json:"wrote"`
    TaskID     string    `json:"task_id"`
    EventID    string    `json:"event_id"`
    DupOf      string    `json:"dup_of"`         // existing row id when the sig already had one
    Diff       Diff      `json:"diff"`           // one DiffEntry per file, byte-exact line as "after"
    Valid      bool      `json:"valid"`
    ValidateRC int       `json:"validate_rc"`
    LatencyMS  int       `json:"latency_ms"`
}

type FlowDriver interface {
    Name() string
    Healthcheck(ctx context.Context) (DriverHealth, error)
    FileTask(ctx context.Context, req FileTaskRequest) (FileTaskResult, error)
    CommentTask(ctx context.Context, row BoardRow, body string) (BoardRow, error)
}

type FlowTimelineStep struct {                 // read model for the dashboard (AC-19)
    Stage      string `json:"stage"`        // filed | foreman | patch | verify | promote
    TS         string `json:"ts"`
    State      string `json:"state"`        // done | running | pending | failed | denied
    Detail     string `json:"detail"`
    RecIDs     []string `json:"rec_ids"`    // ledger records that prove the step
    TaskID     string `json:"task_id"`
    SpawnID    string `json:"spawn_id"`
    Worktree   string `json:"worktree"`
    PRURL      string `json:"pr_url"`
    ErrorCode  string `json:"error_code"`
}
```

JSON example (check_mode / dry-run of a filing — the shadow-mode path):

```json
{"project":"payment-api","board_path":"/srv/src/payment-api/.board","review_mode":"auto","idem_key":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","dry_run":true,"row":{"id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","status":"todo","priority":"P1"},"event":{"id":4107,"type":"task_created","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ"},"result":{"wrote":false,"dup_of":"","valid":true,"latency_ms":0}}
```

```json
{"stage":"verify","ts":"2026-09-16T09:35:01.000Z","state":"done","detail":"evidence tuple result=passed, window 600s, 0 events, canary seen","rec_ids":["ev_01J9Z6Q0M2X4T8V1K7B3N5R8WR"],"task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","spawn_id":"sp_01J9Z6Q0M2X4T8V1K7B3N5R8WP","worktree":"/srv/src/payment-api/.worktrees/tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","pr_url":"https://github.com/acme/payment-api/pull/42","error_code":""}
```

`IdempotencyClass` (`pure|convergent|once`) is declared per flow module in §2; `Prefix` supplies the
`tsk_`, `sp_`, `inc_` and `iss_` tokens used by this package (SPEC-TYPES §3.1).

Two existing structs are extended by this spec (the index owner folds the added fields into
SPEC-TYPES §3.10; nothing already there changes shape):



```json
{"driver":"board-jsonl","review_mode":"auto","board_path":"/srv/src/payment-api/.board","id_prefix":"tsk_","validate_cmd":"boardctl -C /srv/src/payment-api validate","registration_probe_interval":"5m","registration_stale_max":"1h","scheduler_endpoint":"http://127.0.0.1:9090","scheduler_token_file":"/etc/trouble/scheduler.token","projects":{"payment-api":{"name":"payment-api","repo":"/srv/src/payment-api","board_path":"/srv/src/payment-api/.board","enabled":true,"hotfix":true,"scheduler":"payment-api","registered":true,"ticked":true,"last_probe_ts":"2026-09-16T09:14:00.000Z","reason":""}},"hotfix":{"enabled":false,"allowed_repos":["/srv/src/payment-api"],"unit_repo_map":{"payment-worker":"/srv/src/payment-api"},"foreman_spawn":"router_spawn","priority_class":"hotfix","verify_window":"10m","promote":"human","max_concurrent":2,"min_free_disk_gb":10,"worktree_base":".worktrees","worktree_exempt":["/srv/src/warehouse-index"],"lease_ttl":"30m","spawn_ack_timeout":"5s","spawn_worktree_timeout":"30s","max_attempts":5,"min_severity":"high","mutex_wait":"5s","test_hint_cmd":"","models":{"primary_model":"","primary_provider":"","fallback_model":"","fallback_provider":""},"capability_tags":["hotfix","trouble"]}}
```

#### 3.15.7 Contributed by SPEC-09

_SPEC-09 — issue desk: driver contract, github + duckbrain drivers (trouble v0.1)_

```go
type IssueDeskConfig struct {
    Enabled           bool               `json:"enabled"`
    PrimaryDriver     string             `json:"primary_driver"`
    DedupWindow       Duration           `json:"dedup_window"`
    QuietClose        Duration           `json:"quiet_close"`
    QuietCloseSweep   Duration           `json:"quiet_close_sweep"`
    OpDeadline        Duration           `json:"op_deadline"`
    BodyMaxBytes      int                `json:"body_max_bytes"`
    TitleMaxChars     int                `json:"title_max_chars"`
    ReplayInterval    Duration           `json:"replay_interval"`
    ReplayBatch       int                `json:"replay_batch"`
    HealthcheckEvery  Duration           `json:"healthcheck_interval"`
    HealthcheckIdle   Duration           `json:"healthcheck_idle"`
    FailAfterProbes   int                `json:"fail_after_probes"`
    SpoolBudgetBytes  int64              `json:"spool_budget_bytes"`
    SpoolMaxEntries   int                `json:"spool_max_entries"`
    SpoolTTL          Duration           `json:"spool_ttl"`
    SpoolMinRetention Duration           `json:"spool_min_retention"`
    MaxAttemptsPerOp  int                `json:"max_attempts_per_op"`
    AckDefault        Duration           `json:"ack_default"`
    Caps              IssueCaps          `json:"caps"`
    Drivers           []IssueDriverConfig `json:"drivers"`
}

type IssueCaps struct {
    PerSigCreates      int      `json:"per_sig_creates"`
    PerSigWindow       Duration `json:"per_sig_window"`
    PerSigComments     int      `json:"per_sig_comments"`
    CommentMinInterval Duration `json:"comment_min_interval"`
    PerProjectCreatesH int      `json:"per_project_creates_h"`
    PerProjectCreatesD int      `json:"per_project_creates_d"`
    PerProjectCommentsH int     `json:"per_project_comments_h"`
    GlobalCreatesH     int      `json:"global_creates_h"`
    GlobalCreatesD     int      `json:"global_creates_d"`
    GlobalCommentsH    int      `json:"global_comments_h"`
}

type IssueDriverConfig struct {
    Name            string            `json:"name"`              // github | duckbrain
    Enabled         bool              `json:"enabled"`
    Primary         bool              `json:"primary"`
    Mirror          bool              `json:"mirror"`            // secondary driver for the same sig
    DedupWindow     Duration          `json:"dedup_window"`      // "" → IssueDeskConfig.DedupWindow
    MaxAttempts     int               `json:"max_attempts"`
    BaseBackoff     Duration          `json:"base_backoff"`
    MaxBackoff      Duration          `json:"max_backoff"`
    Jitter          float64           `json:"jitter"`
    Timeout         Duration          `json:"timeout"`
    MinRemaining    int               `json:"min_remaining"`     // 0 → no quota pause (duckbrain)
    SearchMinInterval Duration        `json:"search_min_interval"`
    Owner           string            `json:"owner,omitempty"`
    Repo            string            `json:"repo,omitempty"`
    APIBase         string            `json:"api_base,omitempty"`
    TokenFile       string            `json:"token_file,omitempty"`
    TokenEnv        string            `json:"token_env,omitempty"`
    Labels          []string          `json:"labels,omitempty"`
    LabelsExtra     []string          `json:"labels_extra,omitempty"`
    SeverityLabels  map[string]string `json:"severity_labels,omitempty"`
    SourceLabels    bool              `json:"source_labels,omitempty"`
    BaseURL         string            `json:"base_url,omitempty"`
    APIKeyFile      string            `json:"api_key_file,omitempty"`
    APIKeyEnv       string            `json:"api_key_env,omitempty"`
    APIKeyHeader    string            `json:"api_key_header,omitempty"` // header NAME only; never a value
    APIKeyScheme    string            `json:"api_key_scheme,omitempty"`
    NSPrefix        string            `json:"ns_prefix,omitempty"`
}

type IssueCapState struct {                      // runtime counters; rebuilt from the ledger at boot
    Driver          string         `json:"driver"`
    WindowTS        string         `json:"window_ts"`          // RFC3339 UTC of the oldest counted record
    SigCreates      map[string]int `json:"sig_creates"`        // sig → creates inside PerSigWindow
    SigComments     map[string]int `json:"sig_comments"`       // sig → comments inside 1h
    LastCommentTS   map[string]string `json:"last_comment_ts"` // sig → RFC3339 UTC
    ProjectCreatesH map[string]int `json:"project_creates_h"`
    ProjectCreatesD map[string]int `json:"project_creates_d"`
    ProjectCommentsH map[string]int `json:"project_comments_h"`
    GlobalCreatesH  int            `json:"global_creates_h"`
    GlobalCreatesD  int            `json:"global_creates_d"`
    GlobalCommentsH int            `json:"global_comments_h"`
    Suppressed      int            `json:"suppressed"`          // operations folded or capped since boot
}

type IssueAttempt struct {
    Op           string `json:"op"`              // ensure | comment | close | reopen | link | healthcheck
    Driver       string `json:"driver"`
    IdemKey      string `json:"idem_key"`
    Attempt      int    `json:"attempt"`
    MaxAttempts  int    `json:"max_attempts"`
    Outcome      string `json:"outcome"`         // ok | retry | give_up | spooled
    HTTPStatus   int    `json:"http_status"`
    ErrorCode    string `json:"error_code"`
    Retryable    bool   `json:"retryable"`
    BackoffMS    int    `json:"backoff_ms"`
    NextTryTS    string `json:"next_try_ts"`
    Remaining    int    `json:"rate_limit_remaining"`
    ResetTS      string `json:"rate_limit_reset_ts"`
}
```

```json
{"enabled":true,"primary_driver":"github","dedup_window":"30m","quiet_close":"24h","quiet_close_sweep":"5m","op_deadline":"45s","body_max_bytes":60000,"title_max_chars":256,"replay_interval":"5s","replay_batch":100,"healthcheck_interval":"60s","healthcheck_idle":"5m","fail_after_probes":2,"spool_budget_bytes":67108864,"spool_max_entries":20000,"spool_ttl":"72h","spool_min_retention":"1h","max_attempts_per_op":20,"ack_default":"24h","caps":{"per_sig_creates":1,"per_sig_window":"24h","per_sig_comments":6,"comment_min_interval":"10m","per_project_creates_h":20,"per_project_creates_d":200,"per_project_comments_h":200,"global_creates_h":100,"global_creates_d":1000,"global_comments_h":600},"drivers":[{"name":"github","enabled":true,"primary":true,"mirror":false,"dedup_window":"30m","max_attempts":5,"base_backoff":"1s","max_backoff":"60s","jitter":0.25,"timeout":"15s","min_remaining":100,"search_min_interval":"6s","owner":"acme","repo":"payment-api","api_base":"https://api.github.com","token_file":"~/.config/trouble/github.token","token_env":"TROUBLE_GITHUB_TOKEN","labels":["trouble","auto-filed"],"labels_extra":[],"severity_labels":{"critical":"sev:critical","high":"sev:high","medium":"sev:medium","low":"sev:low","info":"sev:info"},"source_labels":true}]}
```

```json
{"driver":"github","window_ts":"2026-09-16T09:00:00.000Z","sig_creates":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":1},"sig_comments":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":2},"last_comment_ts":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":"2026-09-16T09:15:41.009Z"},"project_creates_h":{"1":3},"project_creates_d":{"1":9},"project_comments_h":{"1":3},"global_creates_h":3,"global_creates_d":9,"global_comments_h":3,"suppressed":4}
```

```json
{"op":"ensure","driver":"github","idem_key":"issue_ensure|github|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","attempt":1,"max_attempts":5,"outcome":"ok","http_status":201,"error_code":"","retryable":false,"backoff_ms":0,"next_try_ts":"","rate_limit_remaining":4821,"rate_limit_reset_ts":"2026-09-16T09:36:00.000Z"}
```

`GapRecord.Cause` gains the value `driver_down` (§3.3) — reported as a TYPES-GAP line so SPEC-TYPES §3.7's
cause set stays the single source of truth.

#### 3.15.8 Contributed by SPEC-11

_SPEC-11 — skills: artifact schema, local promote loop, pull distribution (trouble v0.1)_

```go
type SkillsConfig struct {                                   // [skills] — the config surface of this spec
    Enabled             bool          `toml:"enabled" json:"enabled"`
    SourcePath          string        `toml:"source_path" json:"source_path"`
    SourceURL           string        `toml:"source_url" json:"source_url"`
    SourceRef           string        `toml:"source_ref" json:"source_ref"`
    RefMode             string        `toml:"ref_mode" json:"ref_mode"`               // tag | branch
    PullInterval        Duration      `toml:"pull_interval" json:"pull_interval"`
    PullJitterPct       int           `toml:"pull_jitter_pct" json:"pull_jitter_pct"`
    PullOnBoot          bool          `toml:"pull_on_boot" json:"pull_on_boot"`
    PullTimeout         Duration      `toml:"pull_timeout" json:"pull_timeout"`
    PullMaxBytes        int64         `toml:"pull_max_bytes" json:"pull_max_bytes"`
    RequireSignature    bool          `toml:"require_signature" json:"require_signature"`
    Approve             string        `toml:"approve" json:"approve"`                 // auto | review | never
    CanaryHostID        string        `toml:"canary_host_id" json:"canary_host_id"`
    CanaryValidity      Duration      `toml:"canary_validity" json:"canary_validity"`
    CanaryOverride      bool          `toml:"canary_override" json:"canary_override"`
    AutoAcceptEnabled   bool          `toml:"auto_accept_enabled" json:"auto_accept_enabled"`
    AutoAcceptThreshold int           `toml:"auto_accept_threshold" json:"auto_accept_threshold"`
    AutoAcceptWindow    Duration      `toml:"auto_accept_window" json:"auto_accept_window"`
    AutoAcceptModules   []string      `toml:"auto_accept_modules" json:"auto_accept_modules"`
    MaxHold             Duration      `toml:"max_hold" json:"max_hold"`
    DemoteAfterFailures int           `toml:"demote_after_failures" json:"demote_after_failures"`
    RetainVersions      int           `toml:"retain_versions" json:"retain_versions"`
    StateDir            string        `toml:"state_dir" json:"state_dir"`
    GitBinary           string        `toml:"git_binary" json:"git_binary"`
    LocalEnabled        bool          `toml:"local_enabled" json:"local_enabled"`               // §2b — read the local SKILL.md library
    LocalDir            string        `toml:"local_dir" json:"local_dir"`                       // §2b — one <name>/SKILL.md per skill
    LocalMaxBytes       int64         `toml:"local_max_bytes" json:"local_max_bytes"`
    LocalMaxSkills      int           `toml:"local_max_skills" json:"local_max_skills"`
    LocalMaxSteps       int           `toml:"local_max_steps" json:"local_max_steps"`
    LocalStepTimeout    Duration      `toml:"local_step_timeout" json:"local_step_timeout"`
    Signers             []SkillSigner `toml:"signers" json:"signers"`
}

type SkillSigner struct {
    KeyID     string `toml:"key_id" json:"key_id"`         // ^[a-z0-9][a-z0-9._-]{2,63}$
    PublicKey string `toml:"public_key" json:"public_key"` // base64 std, 32-byte ed25519
    Trust     string `toml:"trust" json:"trust"`           // release | local
    Enabled   bool   `toml:"enabled" json:"enabled"`
    AddedTS   string `toml:"added_ts" json:"added_ts"`     // RFC3339 UTC
}

type SkillsStatus struct {                                  // `trouble skills status --json`
    HostID        string     `json:"host_id"`
    DaemonVersion string     `json:"daemon_version"`
    Enabled       bool       `json:"enabled"`
    Source        string     `json:"source"`          // resolved path/URL, credentials stripped
    Ref           string     `json:"ref"`
    RefMode       string     `json:"ref_mode"`
    LastPullTS    string     `json:"last_pull_ts"`
    LastPullSHA   string     `json:"last_pull_sha"`
    PullFailures  int        `json:"pull_failures"`
    NextPullTS    string     `json:"next_pull_ts"`
    Degraded      bool       `json:"degraded"`
    Approve       string     `json:"approve"`
    CanaryHostID  string     `json:"canary_host_id"`
    Signers       []string   `json:"signers"`          // key_id[:trust], enabled only
    Skills        []SkillRow `json:"skills"`
}

type SkillRow struct {
    Name        string `json:"name"`
    Version     int    `json:"version"`
    State       string `json:"state"`      // installed|pending_review|held|refused|conflict|canary_blocked|floor_blocked|quarantined
    Origin      string `json:"origin"`     // local | pull
    SignerKeyID string `json:"signer_key_id"`
    Sigs        int    `json:"sigs"`
    Applied     int    `json:"applied"`
    Success     int    `json:"success"`
    Refusals    int    `json:"refusals"`
    ErrorCode   string `json:"error_code"`
    Reason      string `json:"reason"`
    InstalledTS string `json:"installed_ts"`
}
```

```json
{"host_id":"7f3a91c2d4e5b607","daemon_version":"0.1.0","enabled":true,"source":"/srv/skills/release.git","ref":"v*","ref_mode":"tag","last_pull_ts":"2026-09-16T09:15:00.000Z","last_pull_sha":"9c1f0ab7d2e", "pull_failures":0,"next_pull_ts":"2026-09-16T09:30:00.000Z","degraded":false,"approve":"review","canary_host_id":"c0ffee1234567890","signers":["skills-2026:release","local@7f3a91c2:local"],"skills":[{"name":"payment-worker-queue-wedge","version":3,"state":"installed","origin":"pull","signer_key_id":"skills-2026","sigs":2,"applied":4,"success":4,"refusals":0,"error_code":"","reason":"","installed_ts":"2026-09-16T09:15:00.000Z"}]}
```

#### 3.15.9 The research seam (SPEC-07 driver, SPEC-05 consumer port)

`ResearchDriver` is the driver contract (SPEC-07 owns it; `internal/research` implements it). `ResearchPort` is the
ladder's consumer port (SPEC-05 §2) — a narrow, dependency-inverted view of the same capability, satisfied by a
one-line adapter over `ResearchDriver` (`Request → Discover + Submit`, `Poll → Poll`). Both are exported so the
adapter compiles and is testable in isolation; they must not drift.

```go
type ResearchDriver interface {
    Name() string                                                              // off-by-one | none | webhook
    Discover(ctx context.Context, req DiscoverRequest) (DiscoverResponse, error)
    Submit(ctx context.Context, req SubmitRequest) (SubmitResponse, error)
    Poll(ctx context.Context, submissionID string) (QueueStatus, error)
    Health(ctx context.Context) (labHealth, error)
    Stats(ctx context.Context) (labHealth, error)
}

type ResearchPort interface { // internal/research, SPEC-07 §2
    Request(ctx context.Context, inc Incident, sub Subject) (ResearchOutcome, error)
    Poll(ctx context.Context, resID string) (ResearchOutcome, error)
}

type Subject struct { // the research request payload the driver turns into SPEC-07 §3 fields
    Slug        string         `json:"slug"`
    Description string         `json:"description"`
    Cadence     string         `json:"cadence"`
    Context     map[string]any `json:"context"` // fingerprint, stack, release, unit, codeplane (SPEC-07 §3.10a)
}
```

```json
{"slug":"svc-crash-loop","description":"payment-worker restart loops, cgroup OOM-killed","cadence":"recurring","context":{"fingerprint":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","stack":"worker.py:118 claim","release":"payment-api@2.4.1","unit":"payment-worker"}}
```

#### 3.15.10 Contributed by SPEC-08 (definitions located outside its folded section)

`FlowProject` (SPEC-08 §3.5), `RouterConfig` (SPEC-08 §3.6) and `ForemanBrief` (SPEC-08 §3.14) are defined in that
spec's body rather than in its folded block; they are listed here so the inventory is complete.

```go
type FlowProject struct {
    Name        string `json:"name"`         // unit name, service name, or project slug trouble files for
    Repo        string `json:"repo"`         // absolute repo root ("" for sensor-only projects)
    BoardPath   string `json:"board_path"`   // board directory the scheduler ticks
    Enabled     bool   `json:"enabled"`      // trouble-side gate: filing allowed at all
    Hotfix      bool   `json:"hotfix"`       // project is hot-fix eligible
    Scheduler   string `json:"scheduler"`    // name the project must appear as in the scheduler
    Registered  bool   `json:"registered"`   // last probe result
    Ticked      bool   `json:"ticked"`       // last probe: the scheduler reported it ticked
    LastProbeTS string `json:"last_probe_ts"`
    Reason      string `json:"reason"`       // "" when the proof holds; else the failing check
}

type RouterConfig struct {
    Mode         string   `json:"mode"`          // http | cli
    Endpoint     string   `json:"endpoint"`      // http mode, base URL
    DispatchPath string   `json:"dispatch_path"` // default "/dispatch"
    CLIPath      string   `json:"cli_path"`      // cli mode, absolute path to the router CLI
    TokenEnv     string   `json:"token_env"`     // env var name, default "TROUBLE_ROUTER_TOKEN"
    TokenFile    string   `json:"token_file"`    // 0600 file; wins over TokenEnv when both exist
    Timeout      Duration `json:"timeout"`       // default "10s"
    Retries      int      `json:"retries"`       // default 3 (immediate path), then the spool
}

type ForemanBrief struct {
    Sig            string         `json:"sig"`
    Inc            string         `json:"inc"`
    TaskID         string         `json:"task_id"`
    Title          string         `json:"title"`            // ≤200 chars, scrubbed
    Severity       Severity       `json:"severity"`
    Repo           string         `json:"repo"`
    Worktree       string         `json:"worktree"`
    BoardPath      string         `json:"board_path"`
    EvidenceBundle map[string]any `json:"evidence_bundle"`  // stack ≤8KiB, ≤10 samples, journal tail ≤64KiB
    ResearchBrief  map[string]any `json:"research_brief"`   // {} when no brief was returned
    FailingTests   []string       `json:"failing_test_hints"`
    ToolContract   string         `json:"tool_contract"`    // "registry-only" — the only value in v0.1
    AllowedModules []string       `json:"allowed_modules"`
    DoNotTouch     DoNotTouch     `json:"do_not_touch"`
    VerifyWindow   Duration       `json:"verify_window"`
    Promote        string         `json:"promote"`
    Models         map[string]string `json:"models"`        // primary/fallback model+provider pins, "" = router decides
    Budget         map[string]any `json:"budget"`           // {"max_attempts":5,"wall_clock":"30m"}
    Constraints    []string       `json:"constraints"`      // no shell; no fetch/gc/prune; PR-only; worktree-only
    DaemonVersion  string         `json:"daemon_version"`
    GitSHA         string         `json:"git_sha"`
    CreatedTS      string         `json:"created_ts"`
}
```

```json
{"name":"payment-api","repo":"/srv/src/payment-api","board_path":"/srv/src/payment-api/.board","enabled":true,"hotfix":true,"scheduler":"payment-api","registered":true,"ticked":true,"last_probe_ts":"2026-09-16T09:14:00.000Z","reason":""}
{"mode":"http","endpoint":"http://127.0.0.1:9091","dispatch_path":"/dispatch","cli_path":"/usr/local/bin/router","token_env":"TROUBLE_ROUTER_TOKEN","token_file":"/etc/trouble/router.token","timeout":"10s","retries":3}
{"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","title":"hotfix: queue wedge in payment-worker","severity":"high","repo":"/srv/src/payment-api","worktree":"/srv/src/payment-api/.worktrees/tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","board_path":"/srv/src/payment-api/.board","evidence_bundle":{"stack":"worker.py:118 claim","samples":1,"journal_tail":"","release":"payment-api@2.4.1","redactions":1},"research_brief":{},"failing_test_hints":[],"tool_contract":"registry-only","allowed_modules":["file.patch","service.reload"],"do_not_touch":{"paths":[],"units":[],"scopes":[]},"verify_window":"10m","promote":"human","models":{"primary_model":"","primary_provider":""},"budget":{"max_attempts":5,"wall_clock":"30m"},"daemon_version":"0.1.0","git_sha":"9c1f0ab","created_ts":"2026-09-16T09:14:10.221Z"}
```


#### 3.15.11 Contributed by SPEC-13

_SPEC-13 — server profiles: standalone and light-hub (Redis ingestion buffer + DuckBrain archival) (trouble v0.1.1)_

```go
type ProfileConfig struct {             // the resolved [server] profile and the keys it requires (SPEC-13 §2.1)
    Profile          string   `json:"profile"`              // "standalone" | "light-hub"
    HubID            string   `json:"hub_id"`               // "" → origin.host_id
    RedisURL         string   `json:"redis_url"`            // required when profile=light-hub; password redacted in explain dumps
    RedisStream      string   `json:"redis_stream"`         // "trouble:ingest"
    ConsumerGroup    string   `json:"consumer_group"`       // "ledger-writers"
    Consumer         string   `json:"consumer"`             // "" → origin.host_id (one consumer per state root)
    MaxLen           int64    `json:"maxlen"`               // 1000000, approximate trim; un-acked entries are never trimmed
    DedupTTL         Duration `json:"dedup_ttl"`            // "24h"
    RequireRedis     bool     `json:"require_redis"`        // false → degrade to the standalone path instead of refusing
    DBNamespace      string   `json:"duckbrain_namespace"`  // required when profile=light-hub
    DBEndpoint       string   `json:"duckbrain_endpoint"`
    ArchiveInterval  Duration `json:"archive_interval"`     // "1h"
    KeepLocalGens    int      `json:"keep_local_generations"` // 2
    Valid            bool     `json:"valid"`                // false → TROUBLE-HUB-001 at boot, exit 13
    InvalidReason    string   `json:"invalid_reason"`       // "" | missing_redis_url | missing_namespace | satellite_profile | unknown_profile
}

type RedisStreamOffsets struct {        // live queue state; the zero value when profile=standalone
    Stream          string `json:"stream"`
    Group           string `json:"group"`
    Consumer        string `json:"consumer"`
    StreamLen       int64  `json:"stream_len"`
    Pending         int64  `json:"pending"`          // delivered, not yet acked
    Lag             int64  `json:"lag"`              // stream_len − acked position
    LastDeliveredID string `json:"last_delivered_id"`
    LastAckedID     string `json:"last_acked_id"`
    Reclaims        int64  `json:"reclaims"`         // XAUTOCLAIM rounds that took back stranded entries
    DedupHits       int64  `json:"dedup_hits"`
    DedupMisses     int64  `json:"dedup_misses"`
    DedupConflicts  int64  `json:"dedup_conflicts"`
    AOF             bool   `json:"aof"`              // server preflight: appendonly enabled (§2.1.1 rule 1)
    Policy          string `json:"policy"`           // server preflight: maxmemory-policy (REQUIRED "noeviction")
    EvictedKeys     int64  `json:"evicted_keys"`     // server preflight: INFO evicted_keys — non-zero is a red flag
    OptionsChecked  bool   `json:"options_checked"`  // false when the connection lacks INFO/CONFIG rights (managed Redis)
    DedupWindow     string `json:"dedup_window"`     // "redis" (24h TTL) | "lru" (degraded, hub.dedup_lru)
    Backpressure    int64  `json:"backpressure_total"`
    Degraded        bool   `json:"degraded"`
    DegradedReason  string `json:"degraded_reason"`  // "" | redis_unavailable | redis_refusing | redis_auth | archive_paused
    Since           string `json:"since"`
}

type CodeplaneContext struct {          // SPEC-05 §3.13a — the sensor⇄sentinel cross-plane bundle (§3.15.12)
    Side       string         `json:"side"`                 // "sentinel" | "sensor" — which plane produced it
    Sig        string         `json:"sig,omitempty"`        // sentinel: the group signature
    GroupID    string         `json:"grp,omitempty"`        // sentinel: the group id (SPEC-04)
    Project    string         `json:"project,omitempty"`    // sentinel: project id
    Release    string         `json:"release,omitempty"`    // sentinel: the running release of the errored code
    Regressed  bool           `json:"regressed,omitempty"`  // sentinel: release regression currently open
    Recent     []SigCount     `json:"recent,omitempty"`     // sentinel: top-5 recent signatures, count-descending
    Sample     string         `json:"sample,omitempty"`     // sentinel: rec_id of the representative event
    RuleID     string         `json:"rule_id,omitempty"`    // sensor: the rule that fired
    Readings   map[string]string `json:"readings,omitempty"` // sensor: rule id / metric → stabilized reading ("io.full.avg10":"3.11")
    TS         string         `json:"ts"`                   // bundle assembly time (RFC3339 ms UTC)
}

type SigCount struct {
    Sig   string `json:"sig"`
    Count int64  `json:"count"`
    First string `json:"first_ts"`
    Last  string `json:"last_ts"`
}

type HubStatus struct {                 // HealthResponse.Hub — the profile's stanza in the one health surface
    Enabled          bool               `json:"enabled"`              // false in standalone
    Profile          string             `json:"profile"`
    Since            string             `json:"since"`
    Redis            RedisStreamOffsets `json:"redis"`
    ArchiveQueue     int64              `json:"archive_queue"`        // closed generations waiting to be exported
    ArchiveLastTS    string             `json:"archive_last_ts"`
    ArchivedFiles    int64              `json:"archived_files"`
    DroppableGens    int                `json:"droppable_generations"` // verified exports the sweep may now delete
    MarkerPending    int64              `json:"marker_pending"`
    RouteCounters    map[string]int64   `json:"route_counters"`       // {"A":n,"B":n} — local vs relayed (SPEC-04 §3.10a)
    Degraded         bool               `json:"degraded"`
    DegradedReason   string             `json:"degraded_reason"`
}

type LedgerArchiveMarker struct {       // one append-only object per generation transition (SPEC-13 §3.5)
    MarkerID   string `json:"marker_id"`   // hex(sha256(file bytes))[:16] — content-derived, so a re-export is the same marker
    File       string `json:"file"`
    Namespace  string `json:"namespace"`
    ObjectKey  string `json:"object_key"`  // <namespace>/ledger/<file>.jsonl.gz
    Bytes      int64  `json:"bytes"`
    GzipBytes  int64  `json:"gzip_bytes"`
    Sha256     string `json:"sha256"`
    Records    int64  `json:"records"`
    FirstSeq   uint64 `json:"first_seq"`
    LastSeq    uint64 `json:"last_seq"`
    MinTS      string `json:"min_ts"`
    MaxTS      string `json:"max_ts"`
    State      string `json:"state"`       // pending | exported | verified | dropped | failed
    VerifiedTS string `json:"verified_ts"`
    ErrorCode  string `json:"error_code"`  // mirrored into the accompanying lifecycle record (SPEC-INDEX §5.3)
    TS         string `json:"ts"`
}
```

```json
{"profile":"light-hub","hub_id":"","redis_url":"redis://127.0.0.1:6379/0","redis_stream":"trouble:ingest","consumer_group":"ledger-writers","consumer":"","maxlen":1000000,"dedup_ttl":"24h","require_redis":false,"duckbrain_namespace":"trouble/7f3a91c2d4e5b607","duckbrain_endpoint":"http://127.0.0.1:3000","archive_interval":"1h","keep_local_generations":2,"valid":true,"invalid_reason":""}
{"stream":"trouble:ingest","group":"ledger-writers","consumer":"7f3a91c2d4e5b607","stream_len":18422,"pending":12,"lag":181,"last_delivered_id":"1758012841221-318","last_acked_id":"1758012841221-306","reclaims":2,"dedup_hits":37,"dedup_misses":18422,"dedup_conflicts":1,"dedup_window":"redis","backpressure_total":0,"degraded":false,"degraded_reason":"","since":"2026-09-16T09:00:00.000Z"}
{"enabled":true,"profile":"light-hub","since":"2026-09-16T09:00:00.000Z","redis":{"stream":"trouble:ingest","group":"ledger-writers","consumer":"7f3a91c2d4e5b607","stream_len":18422,"pending":12,"lag":181,"last_delivered_id":"1758012841221-318","last_acked_id":"1758012841221-306","reclaims":2,"dedup_hits":37,"dedup_misses":18422,"dedup_conflicts":1,"dedup_window":"redis","backpressure_total":0,"degraded":false,"degraded_reason":"","since":"2026-09-16T09:00:00.000Z"},"archive_queue":3,"archive_last_ts":"2026-09-16T08:20:00.000Z","archived_files":41,"droppable_generations":2,"marker_pending":3,"route_counters":{"A":9021,"B":9401},"degraded":false,"degraded_reason":""}
{"marker_id":"3f9a1c0d5b7e2416","file":"2026-09-16.1.gen.jsonl","namespace":"trouble/7f3a91c2d4e5b607","object_key":"trouble/7f3a91c2d4e5b607/ledger/2026-09-16.1.gen.jsonl.gz","bytes":30408704,"gzip_bytes":10643046,"sha256":"3f9a1c0d5b7e2416a8c93d0e1f4b6275c8d9e0f1a2b3c4d5e6f708192a3b4c5d","records":41207,"first_seq":1,"last_seq":41207,"min_ts":"2026-09-16T00:00:00.104Z","max_ts":"2026-09-16T23:59:59.887Z","state":"verified","verified_ts":"2026-09-16T09:20:41.004Z","error_code":"","ts":"2026-09-16T09:20:41.004Z"}
```

#### 3.15.12a Contributed by SPEC-05 §3.7a/§3.12a — the agent-stage LLM outcome (v0.1.1b)

_SPEC-05 — the agent stage's buffered completion, its ordered fallback chain and its context-compaction
accounting. Produced by `internal/llm`, recorded by `internal/ladder` in the `agent_run` payload
(SPEC-05 §3.12a), and consumed by nobody else: the client and the ladder share these four shapes and
neither package imports the other._

```go
type LLMUsage struct {                  // the token accounting one completion is checked against
    PromptTokens     int  `json:"prompt_tokens"`
    CompletionTokens int  `json:"completion_tokens"`
    TotalTokens      int  `json:"total_tokens"`
    Estimated        bool `json:"estimated"`      // true when the byte-based estimator supplied the counts
}

type LLMAttempt struct {                // one chain entry's try, in order
    Candidate string `json:"candidate"`           // the configured candidate NAME, never an index
    Model     string `json:"model"`
    Status    int    `json:"status"`              // HTTP status, 0 when no response arrived
    Class     string `json:"class"`               // failure class, "" for the attempt that served
    Reason    string `json:"reason"`              // stable token, never prose
    LatencyMS int    `json:"latency_ms"`
}

type LLMCompaction struct {             // one context-compaction pass (SPEC-05 §3.7a)
    Applied   bool   `json:"applied"`             // false = the context fit; the counts are still recorded
    Chunks    int    `json:"chunks"`              // summarisation groups actually run
    InTokens  int    `json:"in_tokens"`           // the assembled context, measured before the pass
    OutTokens int    `json:"out_tokens"`          // the summaries, measured after the pass
    MaxChunks int    `json:"max_chunks"`          // the cap the split had to respect
    Candidate string `json:"candidate"`           // which chain entry summarised
    Model     string `json:"model"`
}

type AgentOutcome struct {              // one buffered agent-stage completion
    Text          string        `json:"text"`
    Candidate     string        `json:"serving_candidate"`   // "" exactly when FailureClass is set
    Model         string        `json:"model"`
    Endpoint      string        `json:"endpoint"`            // credential-free host
    Usage         LLMUsage      `json:"usage"`
    Compaction    LLMCompaction `json:"compaction"`
    Attempts      []LLMAttempt  `json:"attempts"`
    FailureClass  string        `json:"failure_class"`       // "" on success
}
```

Rules:

1. **No field can carry a credential.** A key is named by a `key_ref` in the config (SPEC-05 §4.3a);
   `Endpoint` is the host, so an outcome can name the upstream that served without naming a token.
2. `Candidate` is empty **exactly** when the run failed. There is no "unknown" value and a failed run
   never claims a candidate.
3. `Usage.Estimated` is set when the provider reported no `usage` object and the documented byte-based
   estimator supplied the counts: an estimate is never presented as a measurement.
4. The chain is tried in config order and `Attempts` preserves that order, one entry per try, so the
   failover is answerable from the record alone (SPEC-05 §3.12a).

## 4. Wiring

| Producer | Consumes | Emits kinds |
|---|---|---|
| internal/ledger | every kind | (writer of all) |
| internal/scrub | nothing (pure) | — (returns ScrubResult) |
| internal/sensors | Record, Rule, Breaker | event, gap, canary |
| internal/sentinel | Record, Project, Group, CodeplaneContext | event, group, gap, canary |
| internal/ladder | Incident, Evidence, AutonomyGates, CodeplaneContext, AgentOutcome, Play, ToolCall | incident, verify, breaker, agent_run |
| internal/registry | Descriptor, ToolCall, Play | tool_call, play_run |
| internal/research | ResearchOutcome, CodeplaneContext | research, gap |
| internal/flow | BoardRow, SpawnRequest, Promotion | flow, spawn |
| internal/issues | IssueRef, DriverHealth, CodeplaneContext | issue, gap |
| internal/dashboard | HealthResponse (read-only consumer) | (none; reads ledger + index) |
| internal/skills | Skill, SkillCandidate, SkillStats, Play, ToolCall | skill |
| internal/llm | AgentOutcome, LLMUsage, LLMAttempt, LLMCompaction | (none; returns AgentOutcome — the ladder writes agent_run) |
| internal/lifecycle | ConfigValue, Heartbeat, ForwardEnvelope | config, lifecycle |
| internal/hub | ProfileConfig, HubStatus, RedisStreamOffsets, LedgerArchiveMarker, ForwardEnvelope, Record, GapRecord | lifecycle (config), gap (archive loss) |

## 5. Errors — the canonical catalog

Every error code in every spec MUST be one of the codes below (specs may not invent new codes; if a spec
needs one, it is added here first). Format: `TROUBLE-<AREA>-<NNN>`. Areas are exactly the package names
allocated in SPEC-INDEX §3.5 (twelve subsystems plus `internal/hub` since v0.1.1).

| Code | Class | Meaning | Owning spec |
|---|---|---|---|
| TROUBLE-LEDGER-001 | permanent | append/write to the ledger file failed | SPEC-01 |
| TROUBLE-LEDGER-002 | permanent | sequence gap detected on read (non-monotonic or missing seq) | SPEC-01 |
| TROUBLE-LEDGER-003 | transient | torn last line detected; truncated on read, recorded | SPEC-01 |
| TROUBLE-LEDGER-004 | permanent | record schema_version newer than this binary supports | SPEC-01 |
| TROUBLE-LEDGER-005 | permanent | second writer attempted a ledger file (single-writer violation) | SPEC-01 |
| TROUBLE-LEDGER-006 | transient | rotation failed (target file uncreatable) | SPEC-01 |
| TROUBLE-LEDGER-007 | transient | compaction (generation rewrite) failed | SPEC-01 |
| TROUBLE-LEDGER-008 | transient | in-memory index rebuild failed at boot | SPEC-01 |
| TROUBLE-LEDGER-009 | permanent | retention blocked: compaction cannot free space without violating tombstone counts | SPEC-01 |
| TROUBLE-LEDGER-010 | transient | disk budget exceeded (rotation/compaction cannot reclaim enough) | SPEC-01 |
| TROUBLE-LEDGER-011 | permanent | corruption detected (payload hash mismatch / unparsable interior line) | SPEC-01 |
| TROUBLE-LEDGER-012 | permanent | state root missing/unwritable/wrong mode | SPEC-01 |
| TROUBLE-SCRUB-001 | permanent | scrub rule failed to compile | SPEC-02 |
| TROUBLE-SCRUB-002 | permanent | scrub config invalid (mandatory rule disabled or malformed) | SPEC-02 |
| TROUBLE-SCRUB-003 | transient | scrub input exceeded the scrubbing byte budget → truncated + flagged | SPEC-02 |
| TROUBLE-SCRUB-004 | permanent | redaction counter overflow (rule fired > 2^31 times on one value) | SPEC-02 |
| TROUBLE-SCRUB-005 | transient | scrub rule evaluation exceeded its timeout | SPEC-02 |
| TROUBLE-SCRUB-006 | permanent | mandatory rule missing from the active rule set (refuse to persist) | SPEC-02 |
| TROUBLE-SCRUB-007 | permanent | input is not valid UTF-8 and is not on a binary-allowed target | SPEC-02 |
| TROUBLE-SCRUB-008 | permanent | persistence-boundary re-scan hit: a serialized record still matches a mandatory pattern → the record is refused by the ledger | SPEC-02 |
| TROUBLE-SENSORS-001 | permanent | kernel below the PSI floor (5.15) | SPEC-03 |
| TROUBLE-SENSORS-002 | permanent | PSI trigger grammar rejected (EINVAL) at startup probe | SPEC-03 |
| TROUBLE-SENSORS-003 | permanent | PSI trigger already armed (EBUSY) on the fd | SPEC-03 |
| TROUBLE-SENSORS-004 | permanent | PSI source gone (POLLERR on an armed fd) | SPEC-03 |
| TROUBLE-SENSORS-005 | permanent | PSI trigger arming denied for this uid → sampling-only degradation | SPEC-03 |
| TROUBLE-SENSORS-006 | permanent | journalctl binary missing | SPEC-03 |
| TROUBLE-SENSORS-007 | transient | journal cursor invalid → --since fallback + rescan + gap record | SPEC-03 |
| TROUBLE-SENSORS-008 | transient | journal child process died → restart with backoff | SPEC-03 |
| TROUBLE-SENSORS-009 | permanent | journal read denied: uid not in adm/systemd-journal | SPEC-03 |
| TROUBLE-SENSORS-010 | transient | journal bounded queue overflow → explicit drop accounting | SPEC-03 |
| TROUBLE-SENSORS-011 | transient | D-Bus connect failed for a configured manager | SPEC-03 |
| TROUBLE-SENSORS-012 | policy_refused | polkit refused a manage-units action (POLICY_REFUSED) | SPEC-03, SPEC-06 |
| TROUBLE-SENSORS-013 | permanent | a configured manager (system/user for uid N) is not being watched | SPEC-03 |
| TROUBLE-SENSORS-014 | transient | NameOwnerChanged on org.freedesktop.systemd1 → re-subscribe + resync | SPEC-03 |
| TROUBLE-SENSORS-015 | transient | D-Bus Subscribe/AddMatch failed | SPEC-03 |
| TROUBLE-SENSORS-016 | permanent | systemd-oomd not available (masked) → capability probe + documented no-op | SPEC-03 |
| TROUBLE-SENSORS-017 | permanent | rule file schema invalid | SPEC-03 |
| TROUBLE-SENSORS-018 | permanent | rule condition expression invalid | SPEC-03 |
| TROUBLE-SENSORS-019 | transient | rule hot-reload failed; previous rule set kept active | SPEC-03 |
| TROUBLE-SENSORS-020 | transient | inotify watch limit reached | SPEC-03 |
| TROUBLE-SENSORS-021 | transient | inotify queue overflow → gap record | SPEC-03 |
| TROUBLE-SENSORS-022 | permanent | statfs/statvfs call failed for a configured mount | SPEC-03 |
| TROUBLE-SENSORS-023 | permanent | systemd timer list parse failed | SPEC-03 |
| TROUBLE-SENSORS-024 | transient | sensor heartbeat stale beyond its expectation → gap + degraded | SPEC-03 |
| TROUBLE-SENSORS-025 | permanent | sensor disabled: host capability absent (documented no-op) | SPEC-03 |
| TROUBLE-SENTINEL-001 | permanent | envelope malformed (bad header/length framing) | SPEC-04 |
| TROUBLE-SENTINEL-002 | permanent | envelope exceeds the compressed cap (200KB) | SPEC-04 |
| TROUBLE-SENTINEL-003 | permanent | decompressed payload exceeds the cap (1MB) → gzip-bomb guard | SPEC-04 |
| TROUBLE-SENTINEL-004 | permanent | gzip stream invalid | SPEC-04 |
| TROUBLE-SENTINEL-005 | permanent | no auth material on the request | SPEC-04 |
| TROUBLE-SENTINEL-006 | permanent | auth material invalid (unknown key / wrong project) | SPEC-04 |
| TROUBLE-SENTINEL-007 | permanent | unknown project_id | SPEC-04 |
| TROUBLE-SENTINEL-008 | permanent | project disabled | SPEC-04 |
| TROUBLE-SENTINEL-009 | permanent | DSN host does not match an advertised bind host | SPEC-04 |
| TROUBLE-SENTINEL-010 | transient | project quota exceeded → 429 + Retry-After + X-Sentry-Rate-Limits | SPEC-04 |
| TROUBLE-SENTINEL-011 | transient | global disk budget exceeded → loss policy applies | SPEC-04 |
| TROUBLE-SENTINEL-012 | permanent | legacy /store/ endpoint used (accepted + counter, deprecated) | SPEC-04 |
| TROUBLE-SENTINEL-013 | permanent | unknown envelope item type dropped (200 + counter, never a reject) | SPEC-04 |
| TROUBLE-SENTINEL-014 | permanent | event dropped by the configured loss policy (sample/drop) | SPEC-04 |
| TROUBLE-SENTINEL-015 | permanent | event dropped by spool-if-light because the spool budget was full | SPEC-04 |
| TROUBLE-SENTINEL-016 | permanent | fingerprint computation failed; fell back to the message-derived sig | SPEC-04 |
| TROUBLE-SENTINEL-017 | permanent | collector parser failed on a matched line | SPEC-04 |
| TROUBLE-SENTINEL-018 | permanent | multi-line assembly timed out; partial event emitted with a marker | SPEC-04 |
| TROUBLE-SENTINEL-019 | permanent | generic JSON endpoint body invalid | SPEC-04 |
| TROUBLE-SENTINEL-020 | transient | spool write failed | SPEC-04 |
| TROUBLE-SENTINEL-021 | permanent | request method not allowed on an ingestion route | SPEC-04 |
| TROUBLE-SENTINEL-022 | permanent | unsupported Content-Type on an ingestion route | SPEC-04 |
| TROUBLE-SENTINEL-023 | permanent | invalid `[sentinel.routes]` table at boot: unknown route, empty sig-prefix key, one prefix mapped to two routes, or `proxy` without a configured hub endpoint | SPEC-04 |
| TROUBLE-LADDER-001 | permanent | illegal ladder transition attempted for the current state | SPEC-05 |
| TROUBLE-LADDER-002 | permanent | play configured max_runs exhausted → escalate to the next rung | SPEC-05 |
| TROUBLE-LADDER-003 | transient | agent lease held by another incident on this host | SPEC-05 |
| TROUBLE-LADDER-004 | permanent | agent two-strikes rule hit (per-sig per-24h) → suspend | SPEC-05 |
| TROUBLE-LADDER-005 | permanent | verify window invalid (zero/negative/over cap) | SPEC-05 |
| TROUBLE-LADDER-006 | permanent | verification INVALID: canary not seen inside the window | SPEC-05 |
| TROUBLE-LADDER-007 | permanent | recurrence inside the verify window → verification failed | SPEC-05 |
| TROUBLE-LADDER-008 | transient | rollback of the applied play failed | SPEC-05 |
| TROUBLE-LADDER-009 | transient | research unavailable → research:degraded + continue | SPEC-05 |
| TROUBLE-LADDER-010 | permanent | kill-switch active: stage refused at its checkpoint | SPEC-05 |
| TROUBLE-LADDER-011 | permanent | autonomy gate denied for this stage (shadow/assisted) | SPEC-05 |
| TROUBLE-LADDER-012 | permanent | incident id not found in the index | SPEC-05 |
| TROUBLE-LADDER-013 | permanent | budget exceeded (per-day agent/play budget) | SPEC-05 |
| TROUBLE-LADDER-014 | permanent | breaker open for this scope → suppression instead of work | SPEC-05 |
| TROUBLE-LADDER-015 | transient | park failed (in-flight play could not be persisted) | SPEC-05 |
| TROUBLE-LADDER-016 | transient | resume failed (parked play could not be re-adopted) | SPEC-05 |
| TROUBLE-LADDER-017 | permanent | incident quarantined (repeated illegal transitions / corrupt state) | SPEC-05 |
| TROUBLE-LADDER-018 | permanent | suppression window active for this sig | SPEC-05 |
| TROUBLE-LADDER-019 | permanent | stabilization window invalid for this rule | SPEC-05 |
| TROUBLE-LADDER-020 | permanent | reopen found an existing open incident whose sig does not match | SPEC-05 |
| TROUBLE-LADDER-021 | permanent | the agent-stage LLM call failed or exceeded a budgeted cap (token cap, wall clock, no candidate served, compaction not cappable) | SPEC-05 |
| TROUBLE-REGISTRY-001 | permanent | module not found in the registry | SPEC-06 |
| TROUBLE-REGISTRY-002 | permanent | args violate the module JSON schema (rejected at validate) | SPEC-06 |
| TROUBLE-REGISTRY-003 | transient | Check() failed | SPEC-06 |
| TROUBLE-REGISTRY-004 | transient | Apply() failed | SPEC-06 |
| TROUBLE-REGISTRY-005 | permanent | Verify() returned not-ok after Apply | SPEC-06 |
| TROUBLE-REGISTRY-006 | permanent | POLICY_REFUSED: polkit/do-not-touch/capability refused the call | SPEC-06 |
| TROUBLE-REGISTRY-007 | permanent | target path/unit is on the do-not-touch list | SPEC-06 |
| TROUBLE-REGISTRY-008 | permanent | required scope not granted by the rule or autonomy gate | SPEC-06 |
| TROUBLE-REGISTRY-009 | transient | tool call exceeded the module timeout | SPEC-06 |
| TROUBLE-REGISTRY-010 | transient | module returned a transient error | SPEC-06 |
| TROUBLE-REGISTRY-011 | permanent | module returned a permanent error | SPEC-06 |
| TROUBLE-REGISTRY-012 | permanent | idempotency violation detected (second Apply changed state again) | SPEC-06 |
| TROUBLE-REGISTRY-013 | permanent | conformance harness failure for a shipped module (CI-fatal) | SPEC-06 |
| TROUBLE-REGISTRY-014 | permanent | module descriptor invalid (missing schema/scope/check_mode) | SPEC-06 |
| TROUBLE-REGISTRY-015 | permanent | rollback unsupported for a non-invertible applied call | SPEC-06 |
| TROUBLE-REGISTRY-016 | permanent | play TOML schema invalid | SPEC-06 |
| TROUBLE-REGISTRY-017 | permanent | play `when:` expression invalid | SPEC-06 |
| TROUBLE-REGISTRY-018 | permanent | tool args failed JSON decoding | SPEC-06 |
| TROUBLE-RESEARCH-001 | transient | lab unreachable (connect/timeout) → degraded | SPEC-07 |
| TROUBLE-RESEARCH-002 | permanent | lab rejected the payload with the strict decoder (400) | SPEC-07 |
| TROUBLE-RESEARCH-003 | permanent | duplicate submission (409) → treated as queued | SPEC-07 |
| TROUBLE-RESEARCH-004 | transient | solver unavailable (503) → degraded | SPEC-07 |
| TROUBLE-RESEARCH-005 | permanent | class-slug derivation fell back to "unknown" | SPEC-07 |
| TROUBLE-RESEARCH-006 | transient | queue poll exceeded the poll budget | SPEC-07 |
| TROUBLE-RESEARCH-007 | permanent | corpus grep found no cached answer (found:false ≠ absent) | SPEC-07 |
| TROUBLE-RESEARCH-008 | permanent | research driver disabled (driver = none) | SPEC-07 |
| TROUBLE-RESEARCH-009 | permanent | returned brief failed local validation | SPEC-07 |
| TROUBLE-RESEARCH-010 | permanent | discover/submit response carried no submission_id | SPEC-07 |
| TROUBLE-FLOW-001 | permanent | flow driver disabled by config | SPEC-08 |
| TROUBLE-FLOW-002 | transient | board row append failed | SPEC-08 |
| TROUBLE-FLOW-003 | permanent | board row failed post-append validation | SPEC-08 |
| TROUBLE-FLOW-004 | permanent | board id conflict (id already present) | SPEC-08 |
| TROUBLE-FLOW-005 | transient | task-router dispatch failed | SPEC-08 |
| TROUBLE-FLOW-006 | permanent | hot-fix lane disabled on this host | SPEC-08 |
| TROUBLE-FLOW-007 | permanent | repo not in allowed_repos | SPEC-08 |
| TROUBLE-FLOW-008 | permanent | severity below the hot-fix threshold | SPEC-08 |
| TROUBLE-FLOW-009 | permanent | one-fix-per-sig lease held by another incident | SPEC-08 |
| TROUBLE-FLOW-010 | transient | router_spawn call failed → spawn_pending + bounded retry | SPEC-08 |
| TROUBLE-FLOW-011 | transient | spawn still pending after the retry budget → durable queue | SPEC-08 |
| TROUBLE-FLOW-012 | permanent | worktree gate refused: free disk below min_free_disk_gb | SPEC-08 |
| TROUBLE-FLOW-013 | transient | per-repo worktree mutex acquire timed out | SPEC-08 |
| TROUBLE-FLOW-014 | permanent | repo is worktree-exempt (huge checkout) → no worktree created | SPEC-08 |
| TROUBLE-FLOW-015 | permanent | verify window failed for a hot-fix spawn → rollback + reopen | SPEC-08 |
| TROUBLE-FLOW-016 | permanent | promotion denied by policy (promote=human in shadow) | SPEC-08 |
| TROUBLE-FLOW-017 | permanent | rollback refused/not possible for this spawn | SPEC-08 |
| TROUBLE-FLOW-018 | permanent | target project is not registered/enabled in the scheduler (row would never be worked) | SPEC-08 |
| TROUBLE-FLOW-019 | permanent | board comment/cross-ref conflict on an existing sig row | SPEC-08 |
| TROUBLE-ISSUES-001 | transient | driver API error | SPEC-09 |
| TROUBLE-ISSUES-002 | transient | driver rate-limited | SPEC-09 |
| TROUBLE-ISSUES-003 | permanent | driver auth failed | SPEC-09 |
| TROUBLE-ISSUES-004 | permanent | issue cap reached for the sig/window | SPEC-09 |
| TROUBLE-ISSUES-005 | transient | driver spool full | SPEC-09 |
| TROUBLE-ISSUES-006 | transient | spool replay failed | SPEC-09 |
| TROUBLE-ISSUES-007 | permanent | referenced issue not found at the driver | SPEC-09 |
| TROUBLE-ISSUES-008 | permanent | close refused by the driver (already closed / locked) | SPEC-09 |
| TROUBLE-ISSUES-009 | transient | driver healthcheck failed | SPEC-09 |
| TROUBLE-ISSUES-010 | permanent | duplicate suppressed inside the dedup window | SPEC-09 |
| TROUBLE-DASHBOARD-001 | permanent | auth material missing | SPEC-10 |
| TROUBLE-DASHBOARD-002 | permanent | auth material invalid | SPEC-10 |
| TROUBLE-DASHBOARD-003 | permanent | token lacks the required scope | SPEC-10 |
| TROUBLE-DASHBOARD-004 | permanent | CSRF token missing/invalid on a POST | SPEC-10 |
| TROUBLE-DASHBOARD-005 | permanent | token was presented in a URL → refused | SPEC-10 |
| TROUBLE-DASHBOARD-006 | permanent | non-loopback bind without a reverse proxy mandate | SPEC-10 |
| TROUBLE-DASHBOARD-007 | transient | template render failed | SPEC-10 |
| TROUBLE-DASHBOARD-008 | permanent | partial/view not found | SPEC-10 |
| TROUBLE-DASHBOARD-009 | permanent | route not found | SPEC-10 |
| TROUBLE-DASHBOARD-010 | permanent | write action refused (kill-switch, read-only mode, or autonomy=never) | SPEC-10 |
| TROUBLE-DASHBOARD-011 | permanent | request body too large for a write action | SPEC-10 |
| TROUBLE-DASHBOARD-012 | transient | dashboard rate limit hit | SPEC-10 |
| TROUBLE-DASHBOARD-013 | permanent | identity provider unavailable (seam impl missing) | SPEC-10 |
| TROUBLE-SKILLS-001 | permanent | skill artifact schema invalid | SPEC-11 |
| TROUBLE-SKILLS-002 | permanent | skill signature verification failed | SPEC-11 |
| TROUBLE-SKILLS-003 | permanent | signer key id not in the configured signer list | SPEC-11 |
| TROUBLE-SKILLS-004 | permanent | min_daemon_version not satisfied by this daemon | SPEC-11 |
| TROUBLE-SKILLS-005 | permanent | play references a module outside allowed_modules | SPEC-11 |
| TROUBLE-SKILLS-006 | permanent | two skills match one sig at the same version → conflict recorded | SPEC-11 |
| TROUBLE-SKILLS-007 | permanent | version downgrade refused | SPEC-11 |
| TROUBLE-SKILLS-008 | permanent | provenance missing (no incidents/research links) | SPEC-11 |
| TROUBLE-SKILLS-009 | permanent | candidate rejected at review | SPEC-11 |
| TROUBLE-SKILLS-010 | transient | pull from the skills repo failed → keep the current set | SPEC-11 |
| TROUBLE-SKILLS-011 | transient | skills repo unreachable (offline) → retry at interval | SPEC-11 |
| TROUBLE-SKILLS-012 | permanent | canary host not yet verified for this skill version | SPEC-11 |
| TROUBLE-SKILLS-013 | permanent | skill refused on this host (refusal record written) | SPEC-11 |
| TROUBLE-SKILLS-014 | transient | local skill stats write failed | SPEC-11 |
| TROUBLE-LIFECYCLE-001 | permanent | config file invalid | SPEC-12 |
| TROUBLE-LIFECYCLE-002 | permanent | config precedence conflict (same key from two operator sources — flag/env/file, never a compiled default — with different values → flag wins, recorded) | SPEC-12 |
| TROUBLE-LIFECYCLE-003 | permanent | bind preflight: port already in use → fail loud | SPEC-12 |
| TROUBLE-LIFECYCLE-004 | permanent | state root not writable | SPEC-12 |
| TROUBLE-LIFECYCLE-005 | permanent | state root mode wrong (must be 0700) | SPEC-12 |
| TROUBLE-LIFECYCLE-006 | transient | unit install/repair failed | SPEC-12 |
| TROUBLE-LIFECYCLE-007 | transient | sd_notify watchdog ping missed (WatchdogSec) | SPEC-12 |
| TROUBLE-LIFECYCLE-008 | transient | heartbeat file stale | SPEC-12 |
| TROUBLE-LIFECYCLE-009 | permanent | ledger sequence stall detected by the external checker | SPEC-12 |
| TROUBLE-LIFECYCLE-010 | transient | escalation hook (OnFailure target) failed | SPEC-12 |
| TROUBLE-LIFECYCLE-011 | transient | upgrade park failed; resume not possible from the ledger | SPEC-12 |
| TROUBLE-LIFECYCLE-012 | permanent | record schema_version downgrade refused | SPEC-12 |
| TROUBLE-LIFECYCLE-013 | permanent | secret-bearing file has wrong permissions (must be 0600) | SPEC-12 |
| TROUBLE-LIFECYCLE-014 | permanent | forward protocol version unsupported | SPEC-12 |
| TROUBLE-LIFECYCLE-015 | transient | spool budget exceeded → drop-oldest + ledger note | SPEC-12 |
| TROUBLE-LIFECYCLE-016 | permanent | unit is missing OnFailure/escalation wiring | SPEC-12 |
| TROUBLE-LIFECYCLE-017 | transient | clock skew beyond tolerance across hosts | SPEC-12 |
| TROUBLE-HUB-001 | permanent | invalid/incomplete server profile (unknown profile, light-hub without a Redis URL or a DuckBrain namespace, light-hub on a satellite) | SPEC-13 |
| TROUBLE-HUB-002 | permanent | Redis connection or credentials rejected (bad URL scheme, NOAUTH/WRONGPASS, unusable DB) | SPEC-13 |
| TROUBLE-HUB-003 | transient | Redis unreachable at boot (degraded start, or exit 13 when `require_redis=true`) | SPEC-13 |
| TROUBLE-HUB-004 | transient | stream write failed: senders get 429/503 + `Retry-After`, local spools hold | SPEC-13 |
| TROUBLE-HUB-005 | permanent | consumer-group operation failed in a way BUSYGROUP does not explain (mistyped group, non-stream key) | SPEC-13 |
| TROUBLE-HUB-006 | transient | dedup gate unavailable → bounded LRU fallback for the window, counted once | SPEC-13 |
| TROUBLE-HUB-007 | permanent | stream entry does not decode as a `ForwardEnvelope` → dead-lettered with a gap record | SPEC-13 |
| TROUBLE-HUB-008 | transient | entries stranded past `claim_min_idle` × 3 with no active consumer → reclaimed | SPEC-13 |
| TROUBLE-HUB-009 | permanent | archival target unusable (empty/unresolvable namespace, invalid driver config) | SPEC-13 |
| TROUBLE-HUB-010 | transient | DuckBrain unreachable during export → job stays pending, queue depth visible, ingestion unaffected | SPEC-13 |
| TROUBLE-HUB-011 | permanent | export refused: generation not closed (no `.idx`) or its bytes changed between plan and export | SPEC-13 |
| TROUBLE-HUB-012 | transient | export verification failed (read-back sha256/byte mismatch) → marker pending, retried by key | SPEC-13 |
| TROUBLE-HUB-013 | permanent | live profile switch requested via reload → refused until restart | SPEC-13 |
| TROUBLE-HUB-014 | permanent | retention tried to drop a generation whose archive marker is not `exported` and verified | SPEC-13 |
| TROUBLE-HUB-015 | permanent | Redis deployment topology refused at boot preflight: cluster mode, or a `maxmemory-policy` other than `noeviction` (SPEC-13 §2.1.1 rule 2/3) | SPEC-13 |
| TROUBLE-HUB-016 | permanent | Redis preflight failed with `server.redis.require_persistence=true` and `appendonly=no` — the queue would have an unbounded loss window (SPEC-13 §2.1.1 rule 1) | SPEC-13 |

## 6. Constants, enums and pinned formats

### 6.1 Pinned decisions (single source of truth)

| Decision | Value |
|---|---|
| Module path | `github.com/totalwindupflightsystems/trouble` (rename-if-republished is a single sed over go.mod + imports; recorded in SPEC-INDEX §6) |
| Go version / build | Go 1.26, `CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"` |
| State root | `~/.local/state/trouble` (0700, never /tmp) with `ledger/ spool/ worktrees-meta/ skills-local/ backups/` |
| Ledger file set | `ledger/YYYY-MM-DD.jsonl` + `ledger/YYYY-MM-DD.N.gen.jsonl` generations after compaction |
| Ledger writer | hub daemon only, one process, one file at a time |
| Durability | group-commit, `fsync_window_ms` default 200 (≤200ms crash-loss window, stated in every doc) |
| Ingestion port | 7643 (config-driven) |
| Dashboard port | 7644 (config-driven) |
| Memory | `MemoryHigh=192M` soft, `MemoryMax=256M` hard, `OOMScoreAdjust=-500` (host contention only) |
| RSS budget | binary 8–15MB measured; steady RSS ≤80MB; load ≤192MB |
| ID format | `<prefix>_<ULID>` (26-char Crockford base32) |
| Timestamps | RFC3339 UTC, millisecond precision, always `Z` |
| Duration format | Go duration strings (`"10m"`, `"2s"`, `"1h30m"`) |
| Sig string | `<source>:<algo>v<norm_version>:<hex16>` |
| DSN | `{scheme}://{pubkey}[:{secret}]@{host}[:{port}]/{project_id}` |
| Autonomy default | `shadow` |
| Hot-fix default | `enabled=false`, `allowed_repos=[]`, `promote=human` |
| Server profile | `standalone` (zero external deps); `light-hub` = Redis queue + dedup gate and DuckBrain archival (SPEC-13) |
| Ledger pagination | `page_size` default 500 / max 5000; token `{generation_file}::{byte_offset}::{seq}`; `.idx` sidecar `offset_stride` 256 |
| Sensor route default | `[sentinel.routes] default="auto"` → B when a hub endpoint is configured on a satellite, else A; `origin.route` on every sensor-written record |

### 6.2 ID prefixes

`inc_ grp_ iss_ tsk_ sk_ ev_ res_ sp_` (see §3.1).

### 6.3 Sig canonical form and normalization version

`sig = source + ":" + algo + "v" + itoa(norm_version) + ":" + hex(digest)[:16]`, e.g.
`sentinel:sha256v1:9f2c1d3e4b5a6c7d`. `digest = sha256(normalized_bytes)` where `normalized_bytes` is
defined per source in SPEC-01 §3.3 with published test vectors. Grouping compares `digest` (full 32
bytes); the 16-hex short form is for display, ledger `sig`, leases and dedup keys. Changing a
normalization function REQUIRES bumping `norm_version`; two different `norm_version` values are two
different signature spaces and are never merged (they are linked as "related" on the dashboard only).

## 7. Testing

1. `internal/types/types_test.go`: JSON round-trip (marshal → unmarshal → deep-equal) for every type in
   §3, using the JSON examples in this file verbatim as fixtures.
2. `NewID`/`ParseID` property test: 100k ids monotonic, prefix-correct, and parseable.
3. `Sig.String()`/`ParseSig()` round-trip + the SPEC-01 test vectors.
4. Error-catalog lint: every `TROUBLE-<AREA>-<NNN>` in every spec exists in §5, is unique, and its class
   matches; a CI grep enforces it (SPEC-INDEX §7 step 4).
5. `NowUTC()` format test: regex `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`.
6. v0.1.1 type round-trips: `PageToken.String()`/`ParsePageToken()` over the §3.15.1 vectors (including the
   `""` start token and three malformed forms), `RouteDecision.Valid()` over `A`/`B`/`""`/`"a"`,
   `RouteConfig` prefix-table round-trip, and JSON round-trips for `GenerationIndex`, `ProfileConfig`,
   `RedisStreamOffsets`, `HubStatus` and `LedgerArchiveMarker` using the §3.15 examples verbatim; plus a
   decode test asserting `Origin.Route` and `HealthResponse.Hub` are absent from the JSON of a record and a
   health body that never had them (no schema change for existing producers).

## 8. hilo impact

- New package: `internal/types` (no dependencies beyond stdlib: `encoding/json`, `crypto/sha256`,
  `time`, `errors`).
- Every other `internal/*` package depends on this one; hilo graph: `internal/types` is the highest
  fan-in node in the repo. Changes here are blast-radius-maximal → the file carries a CODEOWNERS-style
  header in SPEC-INDEX §4.
- No existing repo is touched (greenfield: `~/trouble`), so hilo impact on the fleet is zero
  until trouble is published.
