# SPEC-05 — ladder: state machine, verification, autonomy gates (trouble v0.1)

Spec: SPEC-05
Area prefix: TROUBLE-LADDER
Package: internal/ladder
Consumed types: Rule, Condition, SensorHealth, SourceLiveness, Record, RecordKind, Sig, SigSource, Actor, Duration, Incident, LadderState, Rung, Severity, Evidence, VerifyResultKind, GapRecord, Breaker, AutonomyGates, AutonomyMode, Play, PlayTask, Diff, DiffEntry, Result, ToolCall, ResearchOutcome, ParkRecord, AgentLease, BudgetState, Subject, ResearchPort, CodeplaneContext, LLMUsage, LLMAttempt, LLMCompaction, AgentOutcome
Local types: Ladder, Deps, Config, Observation, AdmitResult, Transition, Window, ParkReason, ParkReport, ReAdoptReport, PendingItem, PlayRunner, RuleEvaluator, LedgerWriter, IndexReader, Outlet, Notifier, Clock, RunSummary, StabilizationState, SourcePath, AgentPort, SkillLibrary
ACs: AC-1, AC-2, AC-3, AC-4, AC-5, AC-11, AC-20, AC-21, AC-22, AC-26, AC-31
PRD: §05, §06b, §11, §12

## 1. Purpose

`internal/ladder` is the only component that decides what work a detection gets, and the only one that
declares a problem fixed. It owns the record kinds `incident`, `verify` and `breaker` (SPEC-INDEX §3.4) and
is the sole writer of the incident index (sig → open incident). Everything else reports into it:
sensors and sentinel call `Admit`; registry, research, flow and issues are invoked *by* it through
interfaces this package declares (§2) so the dependency graph stays acyclic in the build order of
SPEC-INDEX §4.1.

Non-negotiable #6 (one ladder) and #11 (autonomy + kill-switch) are implemented here; the binding quorum
amendments this spec encodes are **N** (state set, entry rung, `max_runs`, two-strikes, verify window,
recurrence, stabilization, park/resume, host lease, SIGTERM drain), **D** (verification is positive
evidence, never absence), **K** (shadow = detection + research + play drafting with check_mode-only
mutations), **B** (group-commit ledger, ≤200 ms crash-loss window, single writer), **J** (research rung is
a cache lookup with degrade paths) and **R** (v0.1 cut line: no deferred item appears as in-scope here).

Design constraints stated once: verification is an `Evidence` tuple and never a boolean; `passed`
requires that a canary landed; an incident is never resolved on absent evidence; a refused transition
never mutates state.

## 2. Interface

```go
package ladder

// Construction. Deps carries every collaborator; nothing in this package opens a socket or a
// subprocess. cmd/troubled and the package's own tests are the only constructors of Deps.
func New(deps Deps) (*Ladder, error)

// Admission: one scrubbed observation that a rule qualified (SPEC-03 §3.4 evaluation, SPEC-04 for
// sentinel-derived events). Admit is the ONLY creator of incidents.
func (l *Ladder) Admit(ctx context.Context, obs Observation) (AdmitResult, error)

// Driving: the ladder's own timers and hooks advance an incident. Advance is idempotent per
// (inc, transition_seq): a repeated call with the same Transition.Seq is a no-op.
func (l *Ladder) Advance(ctx context.Context, incID string, tr Transition) (Incident, error)
func (l *Ladder) Incident(ctx context.Context, incID string) (Incident, error)
func (l *Ladder) OpenForKey(ctx context.Context, sig, inKey string) (Incident, bool, error)

// Verification (§3.10). Verify is called at window close and at window start; the returned
// Evidence is the only accepted proof of resolution.
func (l *Ladder) Verify(ctx context.Context, incID string, w Window) (Evidence, error)
func (l *Ladder) CanaryObserved(ctx context.Context, projectID, canaryID string, path SourcePath) error
func (l *Ladder) SourceLiveness(ctx context.Context) ([]SourceLiveness, error)

// Lifecycle: park/resume (§3.5), driven by the SPEC-12 SIGTERM drain and boot sequence.
func (l *Ladder) Park(ctx context.Context, reason ParkReason) (ParkReport, error)
func (l *Ladder) ReAdopt(ctx context.Context) (ReAdoptReport, error)
func (l *Ladder) Pending(ctx context.Context) ([]PendingItem, error)

// Autonomy (§3.11) and kill-switch (§3.4).
func (l *Ladder) Gates() AutonomyGates
func (l *Ladder) SetGates(ctx context.Context, g AutonomyGates, actor Actor) (AutonomyGates, error)
func (l *Ladder) Granted(ctx context.Context, rule, module, sig string) bool

// Breakers, suppression (§3.8) and budgets (§3.7).
func (l *Ladder) Breaker(ctx context.Context, scope string) (Breaker, bool)
func (l *Ladder) TripBreaker(ctx context.Context, scope, reason string) (Breaker, error)
func (l *Ladder) Suppress(ctx context.Context, sig string, d Duration, reason string) (Breaker, error)
func (l *Ladder) Budget(ctx context.Context) (BudgetState, error)

// Host-wide agent lease (§3.6).
func (l *Ladder) AcquireLease(ctx context.Context, incID, holder string, pid int, worktree string) (AgentLease, error)
func (l *Ladder) RenewLease(ctx context.Context, leaseID string) (AgentLease, error)
func (l *Ladder) ReleaseLease(ctx context.Context, leaseID, outcome string) error

// Quarantine (§3.3) is the only manual state exit; SPEC-12 exposes the CLI verb.
func (l *Ladder) Quarantine(ctx context.Context, incID, reason string) error
func (l *Ladder) Unquarantine(ctx context.Context, incID, reason string, actor Actor) error
```

Collaborator seam (declared here, implemented elsewhere — dependency inversion keeps `internal/ladder`
compilable before `internal/registry`, `research`, `flow` and `issues` exist in the build order):

```go
type PlayRunner interface { // internal/registry implements; SPEC-06 §2 owns the 6-stage contract
    Run(ctx context.Context, inc Incident, p Play, mode string) (RunSummary, error) // mode: check_mode | apply
    Check(ctx context.Context, tool string, args map[string]any) (Diff, ToolCall, error)
    Apply(ctx context.Context, tool string, args map[string]any) (Result, ToolCall, error)
    Rollback(ctx context.Context, t ToolCall) error
}
type RuleEvaluator interface { // default impl: internal/sensors (the one condition language, SPEC-03 §3.4)
    Match(r Rule, ev Observation) bool
    Stabilized(r Rule, s StabilizationState, now time.Time) bool
}
type ResearchPort interface { // internal/research, SPEC-07 §2
    Request(ctx context.Context, inc Incident, sub Subject) (ResearchOutcome, error)
    Poll(ctx context.Context, resID string) (ResearchOutcome, error)
}
type Outlet interface { // internal/flow + internal/issues fan-out, SPEC-08 §2 / SPEC-09 §2
    EnsureIssue(ctx context.Context, inc Incident) (string, error)     // iss_ id
    EnsureBoardRow(ctx context.Context, inc Incident) (string, error)  // tsk_ id, "" when flow disabled
    Comment(ctx context.Context, inc Incident, body string) error      // recurrence / reopen cross-ref
    RequestHotfix(ctx context.Context, inc Incident) (string, error)   // sp_ id, "" when gated off
    Promote(ctx context.Context, inc Incident, ev Evidence) (string, error)
    CloseOutlets(ctx context.Context, inc Incident, reason string) error
}
type LedgerWriter interface { // internal/ledger, SPEC-01 §2
    Append(ctx context.Context, kind RecordKind, sig string, inc string, payload map[string]any) (Record, error)
    Flush(ctx context.Context) error // forces the group-commit window
}
type IndexReader interface { // internal/ledger's bounded in-memory index, SPEC-01
    OpenIncidentBySig(ctx context.Context, sig string) (Incident, bool, error)
    OpenIncidentByInKey(ctx context.Context, inKey string) (Incident, bool, error)
    ResolvedIncidentBySig(ctx context.Context, sig string, window Duration) (Incident, bool, error)
    EventsInWindow(ctx context.Context, sig string, from, to string) (int, error)
    CountersInWindow(ctx context.Context, from, to string) (map[string]int64, error)
    GapsInWindow(ctx context.Context, from, to string) ([]GapRecord, error)
    Seq() uint64
}
type Notifier interface { Emit(ctx context.Context, inc Incident, class string, detail map[string]any) error }
type Clock interface { Now() time.Time; Monotonic() time.Duration }
```

```go
// The remaining package-private types. None of them reaches a ledger payload, an HTTP body, a TOML
// schema or another spec; they are the seam between cmd/troubled and this package.
type SourcePath string
type RunSummary struct {
    PlayRun     int          `json:"play_run"`
    TasksRun    int          `json:"tasks_run"`
    TasksFailed int          `json:"tasks_failed"`
    Changed     bool         `json:"changed"`
    Applied     []DiffEntry  `json:"applied"`
    LastTool    ToolCall     `json:"last_tool"`
    FailClass   string       `json:"fail_class"`   // "" | transient | permanent | policy_refused
    DiffSummary string       `json:"diff_summary"`
}
type StabilizationState struct {
    FirstQualifyingTS string `json:"first_qualifying_ts"` // reset on any non-qualifying sample or gap
    Resets            int    `json:"resets"`              // gap-driven resets; 2 ⇒ admit degraded
    Degraded          bool   `json:"degraded"`
}
type Subject struct { // the research request payload the driver turns into SPEC-07 §3 fields
    Slug        string         `json:"slug"`
    Description string         `json:"description"`
    Cadence     string         `json:"cadence"`
    Context     map[string]any `json:"context"` // fingerprint, stack, release, unit
}
```

`SourcePath`'s values are `"sentinel"`, `"collector"`, `"journald"`, `"dbus"`, `"psi"`, `"disk"`,
`"timers"`, `"inotify"` — the same strings `Origin.Source` is prefixed with.

```go
// Opaque holder: index cursor, gate snapshot, breaker map, host lease, budget counters, clock.
type Ladder struct{}

// Config is the resolved form of the §4.3 table: one field per key, the field name being the key's last
// element (`verify_default` → VerifyDefault; `source_max_age.sentinel` → SourceMaxAge["sentinel"]).
type Config struct{}

type Deps struct {
    Ledger   LedgerWriter
    Index    IndexReader
    Registry PlayRunner
    Research ResearchPort
    Outlets  Outlet
    Notify   Notifier
    Clock    Clock
    Eval     RuleEvaluator // default implementation: internal/sensors
    Cfg      Config
    Agent    AgentPort     // the agent stage's LLM port (§2a); nil = the stage refuses, never fabricates
    Skills   SkillLibrary  // the local SKILL.md library (§2b); nil = no library is read
}
type Observation struct { // the admission input; scrubbed and sig-keyed before it reaches here
    EventID       string
    TS            string
    Sig           Sig
    InKey         string
    Rule          string
    Source        SourcePath
    Subject       string
    Taxonomy      string
    AppKind       string
    Severity      Severity
    Stabilization StabilizationState
    Detail        map[string]any
    Codeplane     *CodeplaneContext // cross-plane bundle (SPEC-TYPES §3.15.12), nil for pure-system events
}
type AdmitResult struct { Inc string; Created, Folded, Reopened, Refused bool; Reason string }
type Transition struct { Seq uint64; From, To LadderState; Trigger string; Evidence *Evidence; PlayRun int }
type Window struct { Start string; End string; S Duration }
type ParkReason string // sigterm | crash | upgrade | lease_expired | kill_switch
type ParkReport struct { Records []ParkRecord; FlushedSeq uint64; ElapsedMS int }
type ReAdoptReport struct { Resumed, Failed, Orphaned []string }
type PendingItem struct { Inc string; From, To LadderState; SinceTS string; Reason string }
```

### 2a The agent stage's LLM port (v0.1.1b)

The agent rung's model work is one buffered completion per stage, and this spec owns the port that
performs it. The port is an interface here rather than a dependency: `internal/llm` implements it
(`RunAgent`) and imports `internal/types` only, so the one-way dependency direction of §4.2 is
unchanged — the ladder never imports the client, and the client never imports the ladder.

```go
// AgentPort is the agent stage's only path to a model. One buffered
// request/response per call; a stream is not expressible (SPEC-05 §3.7a).
type AgentPort interface {
    // RunAgent performs one buffered completion and reports which chain entry
    // served it (types.AgentOutcome, SPEC-TYPES §3.15.12a). The outcome is returned
    // WITH the error: it is the ledger-facing report even when no candidate
    // served, so the stage record never depends on an error string.
    RunAgent(ctx context.Context, prompt string) (AgentOutcome, error)
}

// RunAgentStage runs one agent-stage completion for an incident that is in
// `agent:running` and records it as an `agent_run` record (§3.12a). It is the
// ladder's only caller of Deps.Agent and the only writer of the outcome.
//
// The stage is gated exactly like every other stage entry (§3.11, §3.4): the
// kill-switch refuses it with TROUBLE-LADDER-010 and records it as pending, and
// the per-day agent budget (§3.7) is checked before the port is reached. The
// per-call caps — the hard token cap and the wall-clock cap — belong to the port's
// configuration (§3.7a) and are enforced there, before and during the call.
//
// A run that fails is recorded with `outcome:"failed"` and the port's
// `failure_class`, and the incident's `AgentResult` carries that class so T28's
// guard (`no usable result`) is satisfied by evidence rather than by a timestamp.
func (l *Ladder) RunAgentStage(ctx context.Context, incID, prompt string) (AgentOutcome, error)
```

`AgentOutcome` is a shared type (SPEC-TYPES §3.15.12a) because both packages name it. Nothing in it can
carry a credential: `Endpoint` is the credential-free host, and a key is named by a `key_ref` (§4.3a).

Refusal contract: with no `Deps.Agent` wired, `RunAgentStage` returns **TROUBLE-LADDER-021**
(`reason=no_agent_port`) and records nothing that looks like a run — a stage that cannot call a model
must say so, never return an empty diagnosis as if a model had answered.

### 2b The local skill library port (v0.1.1b)

The skills stage reads a **local** skill library in the `SKILL.md` + frontmatter format (SPEC-11 §2b)
and can run one of its steps against the runner. The ladder's view is two methods, both typed:

```go
// SkillLibrary is the ladder's read-and-execute view of the local skill library
// (SPEC-11 §2b). Both methods are typed: a step is a PlayTask — a registry module
// name plus literal args — and execution returns the audit record of one typed
// tool call. There is no shell, interpreter, template or script in this path, and
// no method here accepts one.
type SkillLibrary interface {
    // Plays lists the library's skills, each compiled to the play shape the
    // runner already consumes (name, version, tasks[{tool, args}]). Read-only.
    Plays(ctx context.Context) ([]Play, error)
    // RunStep executes step `step` of library skill `name` against the runner:
    // check_mode under the shadow gate, apply only when the gate grants the
    // module. It writes the `skill`-kind step record (SPEC-11 §4.7a) and returns
    // the tool call's audit record.
    RunStep(ctx context.Context, inc Incident, name string, step int, mode string) (ToolCall, error)
}

// SkillPlays is `trouble`'s read surface for the library: the ladder answers with
// the compiled plays, or with an empty list when no library is wired.
func (l *Ladder) SkillPlays(ctx context.Context) ([]Play, error)

// RunSkillStep is the skills stage entry: gate check (§3.11), kill-switch check
// (§3.4) and then one typed step against the runner. Under `shadow` the step runs
// in check_mode and mutates nothing; under `assisted` a mutating step needs the
// module grant (`<rule>|<module>`) the play path already uses; under `full` the
// registry's own six stages still bind. A refused entry is recorded as pending
// with TROUBLE-LADDER-010 or -011 and no step runs.
func (l *Ladder) RunSkillStep(ctx context.Context, incID, name string, step int) (ToolCall, error)
```

The library is an authoring surface, not a distribution channel: nothing here is pulled, merged or
signed, and a step therefore has exactly a locally-authored play's authority — same modules, same
autonomy gate, same authorize stages. SPEC-11 §2b owns the format, the loader's refusals and the
"never a shell" argument; this section owns the two ladder calls.

## 3. Data model

Symbols used in the tables: `I⟨…⟩` = an `incident`-kind record, `V⟨…⟩` = a `verify`-kind record,
`B⟨…⟩` = a `breaker`-kind record, all with `Record.payload` keys named in `⟨…⟩`. Every record also carries
the standard `Record` fields (seq, rec_id, ts, sig, inc, origin, actor, redactions) per SPEC-01 §3.

### 3.1 States and invariants

States are exactly the 19 pinned in amendment N (`LadderState` constants; no additional state is legal):

`detected` · `recorded` · `play:drafted` · `play:check_only` · `play:applied` · `play:failed` ·
`research:requested` · `research:returned` · `research:degraded` · `research:skipped` ·
`agent:running` · `agent:done` · `agent:failed` · `agent:suspended` · `verifying` · `resolved` ·
`escalated` · `suppressed` · `quarantined`.

Terminal-with-exit: `resolved` (exit = reopen to `recorded`), `escalated` (exit = reopen to `recorded`),
`quarantined` (exit = `Unquarantine`, operator actor only). `suppressed` is a waiting state, not terminal.

INV-1 One open incident per `sig`, and one open incident per `inKey` (§3.9). Both maps are enforced by the
same admission critical section; a violation is TROUBLE-LADDER-020.
INV-2 A state transition is persisted before it is observable: the `I⟨…⟩` record is appended (and
acknowledged by the ledger writer) before the in-memory index is updated. On a crash between the two, boot
re-derives the index from the ledger — the ledger is authoritative, the index is a cache.
INV-3 Every stage entry passes a gate check (§3.11) and a checkpoint check (§3.4) immediately before the
action; a denial is recorded with the code, never silently skipped.
INV-4 A mutating tool call is never applied twice for the same `play_run` + task index (§3.5).
INV-5 Once per `(inc, transition_seq)` — a duplicate `Advance` with the same `Transition.Seq` is a no-op
(the ladder's own dedup for timers that fire twice).
INV-6 A terminal state is never left except by the edges in §3.2; a refusal (TROUBLE-LADDER-001) never
mutates state, only increments the incident's `illegal_transitions` counter.

### 3.2 The complete transition table (legal edges)

| # | Edge | Trigger | Guard | Emitted | Error |
|---|---|---|---|---|---|
| T01 | detected → recorded | observation admitted: rule matched, stabilization satisfied (§3.1 of SPEC-03), dedup upsert returns "new" | no open incident for `sig`/`inKey`; `sig` breaker closed or reason≠suppression_window; `illegal_transitions`<3 | I⟨transition, from, to, entry_rung, rung, rule, inKey, sigs[], arrival_paths{path:count}, severity, stabilize_degraded⟩ | — |
| T02 | detected → suppressed | a suppression window is already open when the admission commits | sig breaker `state=open,reason=suppression_window` with `open_until > now` | I⟨transition, suppress_until, suppressed_count:0⟩ | TROUBLE-LADDER-018 |
| T03 | recorded → play:drafted | rung advance (entry rung `play`, or the play rung is the next rung) | ceiling ≥ play; rule/sig breakers closed; per-day play budget has budget left; kill-switch clear | I⟨transition, rung:play, play_runs⟩ | — |
| T04 | recorded → research:requested | rung advance to research (entry rung `research`) | ceiling ≥ research; `research.driver` ≠ `none`; research budget left; kill-switch clear | I⟨transition, rung:research, research_id⟩ | — |
| T05 | recorded → agent:running | rung advance to the agent (entry rung `agent`) | ceiling ≥ agent; lease acquired (§3.6); agent budget left; `strikes(sig,24h)` < 2; kill-switch clear | I⟨transition, rung:agent, lease{lease_id,holder,expires_ts}⟩ | — |
| T06 | recorded → verifying | outlets-complete rule (ceiling `record`) | ceiling == record; outlets complete (issue ensured, board row written or flow disabled) | I⟨transition, rung:outlets, window{start,end,verify_window}⟩ | — |
| T07 | recorded → escalated | entry rung's budget is exhausted before any work runs | budget(play|agent) == 0; outlets complete | I⟨transition, pending_human:true, budget{…}⟩ | TROUBLE-LADDER-013 |
| T08 | play:drafted → play:check_only | run starts; task 0's `Check()` invoked through PlayRunner | descriptor `check_mode=true`; args schema-valid; kill-switch clear | I⟨transition, rung:play, task:0, tool_call:rec_id⟩ | — |
| T09 | play:drafted → play:failed | no runnable task under the current gate | mode == shadow and a mutating task's module declares `check_mode=false` | I⟨transition, reason:no_check_mode, module⟩ | TROUBLE-LADDER-011 |
| T10 | play:check_only → play:applied | `Check()` returned; apply decision taken | (`diff.empty` == false AND allow_play_mutate AND kill-switch clear AND breakers closed) OR `diff.empty` == true | I⟨transition, changed:bool, tool_call:rec_id, diff_summary⟩ | — |
| T11 | play:check_only → play:failed | `Check()` error, module timeout, or policy refused | per-task retries exhausted, or the class is permanent/policy_refused (no retry) | I⟨transition, class, tool_call:rec_id⟩ | SPEC-06 §5 code mirrored |
| T12 | play:applied → verifying | run complete (`Result` returned for every task) | window valid after clamping (§3.10); kill-switch does not block verification | I⟨transition, window{start,end,verify_window}, play_runs⟩ | — |
| T13 | play:failed → play:drafted | retry the play | `play_runs < effective_max_runs` AND failure class transient AND kill-switch clear | I⟨transition, rung:play, play_runs⟩ | — |
| T14 | play:failed → research:requested | rung advance | retries exhausted OR class permanent/policy_refused; ceiling ≥ research; driver ≠ none | I⟨transition, rung:research, research_id⟩ | TROUBLE-LADDER-002 |
| T15 | play:failed → research:skipped | rung advance, research not available | ceiling ≥ research; `research.driver` == `none` | I⟨transition, rung:research, skipped:true⟩ | SPEC-07 §5 code mirrored |
| T16 | play:failed → agent:running | rung advance past a ceiling that excludes research | ceiling == agent (or `outlets` with research disabled for the rule); lease acquired; budget left; strikes < 2 | I⟨transition, rung:agent, lease{…}⟩ | TROUBLE-LADDER-002 |
| T17 | play:failed → quarantined | rollback of an applied call failed twice | `rollback_failures(sig,24h)` ≥ 2 | I⟨transition, quarantine_reason:rollback_failed, worktree⟩ | TROUBLE-LADDER-008 on each attempt, TROUBLE-LADDER-017 on the transition |
| T18 | play:failed → escalated | ceiling reached with nothing applied | ceiling == play; outlets complete; hot-fix lane unavailable or gated off | I⟨transition, pending_human:true, play_runs⟩ | TROUBLE-LADDER-002 |
| T19 | research:requested → research:returned | brief received and validated | `ResearchOutcome.Brief` non-empty and passes local validation (SPEC-07 §3) | I⟨transition, rung:research, research_id, brief_ref⟩ | — |
| T20 | research:requested → research:degraded | lab unreachable, solver unavailable, poll budget exceeded, strict-decoder reject, or no submission_id | no valid brief after the driver's own retry (1) | I⟨transition, rung:research, degraded_reason, research_id⟩ | TROUBLE-LADDER-009 |
| T21 | research:requested → research:skipped | driver disabled at request time | `research.driver` == `none` | I⟨transition, rung:research, skipped:true⟩ | SPEC-07 §5 code mirrored |
| T22 | research:returned → play:drafted | research short-circuit: the brief carries an executable play draft | `research.apply_brief_as_play` == true AND `play_runs < effective_max_runs + research_play_extra` (default +1) AND ceiling ≥ play AND kill-switch clear | I⟨transition, rung:play, research_play:true, play_runs⟩ | — |
| T23 | research:returned → agent:running | the brief carries no executable draft | lease acquired; budget left; strikes < 2; kill-switch clear | I⟨transition, rung:agent, brief_ref, lease{…}⟩ | — |
| T24 | research:degraded → agent:running | proceed degraded | lease acquired; budget left; strikes < 2; kill-switch clear | I⟨transition, rung:agent, degraded:true, lease{…}⟩ | — |
| T25 | research:skipped → agent:running | proceed without research | lease acquired; budget left; strikes < 2; kill-switch clear | I⟨transition, rung:agent, lease{…}⟩ | — |
| T26 | research:{returned,degraded,skipped} → escalated | the research rung is the ceiling, or the agent budget is exhausted | ceiling == research, or agent budget == 0; outlets complete | I⟨transition, pending_human:true, budget{…}⟩ | TROUBLE-LADDER-002 (ceiling) or TROUBLE-LADDER-013 (budget) |
| T27 | agent:running → agent:done | run finished | every tool call has an audit record; no unresolved policy refusal; result artifact present | I⟨transition, rung:agent, agent_runs, strikes⟩ | — |
| T28 | agent:running → agent:failed | run errored, timed out (`agent_timeout`, default 15m), or overran the cost cap | run has no usable result | I⟨transition, rung:agent, strike:n, failure_class⟩ | — |
| T29 | agent:running → agent:suspended | second agent failure for the `sig` inside 24h | `strikes(sig,24h)` == 2 at this transition | I⟨transition, rung:agent, suspend_until, strikes:2⟩ | TROUBLE-LADDER-004 |
| T30 | agent:running → escalated | the run's requested tool is outside `allowed_modules`, or the cost cap tripped mid-run | outlets complete | I⟨transition, pending_human:true, reason⟩ | TROUBLE-LADDER-013 |
| T31 | agent:failed → agent:running | retry the agent run | `strikes(sig,24h)` < 2 AND budget left AND lease re-acquirable AND kill-switch clear | I⟨transition, rung:agent, agent_runs⟩ | — |
| T32 | agent:failed → agent:suspended | two strikes reached | `strikes(sig,24h)` == 2 | I⟨transition, rung:agent, suspend_until, strikes:2⟩ | TROUBLE-LADDER-004 |
| T33 | agent:failed → escalated | budget exhausted, or `strikes(sig,7d)` ≥ 4 | outlets complete | I⟨transition, pending_human:true, strikes, budget{…}⟩ | TROUBLE-LADDER-013 |
| T34 | agent:suspended → agent:running | suspension window elapsed and the sig stayed quiet | `now ≥ suspend_until` AND zero events for the `sig` over one full verify window (§3.10) AND budget left AND lease acquirable | I⟨transition, rung:agent, strike_reset:true⟩ | — |
| T35 | agent:suspended → escalated | a third failure inside the suspension window, or ≥3 recurrences while suspended | outlets complete | I⟨transition, pending_human:true, strikes, terminal:true⟩ | TROUBLE-LADDER-004 |
| T36 | agent:done → verifying | the run applied ≥1 mutating call (or a module-level Verify returned ok) | window valid after clamping; kill-switch does not block verification | I⟨transition, window{start,end,verify_window}, agent_runs⟩ | — |
| T37 | agent:done → escalated | diagnosis only: the run filed an issue/board row and mutated nothing | outlets complete | I⟨transition, pending_human:true, reason:diagnosis_only⟩ | — |
| T38 | verifying → resolved | window closed with `Evidence.Result == passed` | canary seen, zero events in window, no source missing, no gap in window, outlets complete, kill-switch state irrelevant | V⟨evidence{tuple}, window⟩ + I⟨transition, state:resolved, resolved_ts, evidence_ref⟩ | — |
| T39 | verifying → play:drafted | window closed with `Evidence.Result == failed` (recurrence inside the window) | `play_runs < effective_max_runs + verify_reopens` (default +1) AND kill-switch clear | I⟨transition, rung:play, verify_fail:true, evidence_ref⟩ | TROUBLE-LADDER-007 |
| T40 | verifying → escalated | failed with retries exhausted, or invalid twice | (`failed` AND retries exhausted) OR `invalid_count` ≥ 2; outlets complete | I⟨transition, pending_human:true, evidence_ref, invalid_count⟩ | TROUBLE-LADDER-007 (failed path) or TROUBLE-LADDER-006 (invalid path) |
| T41 | verifying → quarantined | the window cannot be evaluated at all | ledger seq stalled for > window, or index/ledger snapshot disagreement, or window bounds invalid and unclampable | I⟨transition, quarantine_reason:evaluation_impossible⟩ | TROUBLE-LADDER-005 |
| T42 | resolved → recorded | recurrence after resolve (reopen the SAME incident, sig-keyed) | arriving `sig` == incident `sig`, or `inKey` match with the app-class merge guard (§3.9); the incident's index entry still exists; `reopen_count < reopen_max` (default 100) | I⟨transition, reopen:true, reopen_count, rung⟩ + recurrence comment via Outlet.Comment | — |
| T43 | escalated → recorded | recurrence on an escalated incident | `sig`/`inKey` match; budget left for the next rung | I⟨transition, reopen:true, reopen_count⟩ + recurrence comment | TROUBLE-LADDER-013 when the budget is gone (the reopen is refused, the incident stays escalated) |
| T44 | suppressed → recorded | suppression window closed with ≥1 suppressed event | `quiet_close` == false OR `suppressed_count` > 0 | I⟨transition, suppressed_count, rung⟩ | — |
| T45 | suppressed → resolved | quiet-close: zero events for the whole window | `quiet_close` == true AND `suppressed_count` == 0 (positive quiet: the window's canary landed) | V⟨evidence{tuple,result:passed}, window⟩ + I⟨transition, state:resolved, quiet_close:true⟩ | — |
| T46 | {detected, recorded, play:\*, research:\*, agent:suspended} → suppressed | a suppression window is declared while the incident is live | takes effect at the next checkpoint (§3.4); no in-flight mutating tool call is interrupted | I⟨transition, suppress_until, suppress_reason⟩ | TROUBLE-LADDER-018 |
| T47 | any non-terminal state → quarantined | `illegal_transitions` ≥ 3, or a state snapshot fails validation | — | I⟨transition, quarantine_reason, illegal_transitions⟩ | TROUBLE-LADDER-017 |
| T48 | (no state change) arrival while a scope breaker is open | an observation arrives for a `sig`/`rule`/`source` whose breaker is `open` | the observation is folded into the group counters and the newest open incident for that source, and never advances a rung | I⟨transition, folded:true, breaker_scope, suppressed_count⟩ | TROUBLE-LADDER-014 (once per window, then every 100th arrival) |

Kill-switch refusals (§3.4) are recorded but are not transitions: every stage-entry edge (T03, T04, T05,
T08, T10 with a mutating diff, T13, T14, T16, T22, T23, T24, T25, T31, T34, T39) is evaluated against
`AutonomyGates.KillSwitch` first and, when set, records `I⟨refused:true, pending:true, resume_from:<state>,
error_code:TROUBLE-LADDER-010⟩` without changing state.

### 3.3 Refused transitions — TROUBLE-LADDER-001

A refusal appends `I⟨refused:true, from, to, illegal:true, error_code:TROUBLE-LADDER-001⟩`, increments the
incident's `illegal_transitions` counter, leaves the state untouched, and returns the error to the caller.

| # | Refused edge | Why it is illegal |
|---|---|---|
| R01 | any → detected | `detected` is entry-only; re-observing a live incident is a merge (§3.9), not a transition |
| R02 | detected → {play:drafted, research:requested, agent:running, verifying} | `recorded` is the only successor of `detected` |
| R03 | recorded → verifying with rung or ceiling ≠ `record` | the outlets-only rule (T06) is the single legitimate path |
| R04 | play:drafted → play:applied | `play:check_only` is mandatory for every mutating apply (non-negotiable #10) |
| R05 | play:check_only → verifying | `play:applied` or `play:failed` is the only exit |
| R06 | play:\* → play:drafted when `play_runs ≥ effective_max_runs` | a retry past the cap must become a rung advance (T14/T16/T18) |
| R07 | research:requested → play:drafted | the short-circuit exists only from `research:returned` (T22) |
| R08 | agent:\* → {play:\*, research:\*} | no backward rungs; the agent's fix leaves via `agent:done` → `verifying` |
| R09 | verifying → {detected, recorded} | an in-window recurrence is a verification failure (T39), never a re-admission |
| R10 | {resolved, escalated} → verifying | a reopen re-enters at `recorded` (T42/T43) |
| R11 | quarantined → any | terminal; `Unquarantine` writes `I⟨state:recorded, unquarantined_by, reason⟩` with a human actor |
| R12 | suppressed → verifying | a suppressed incident collects no evidence window |
| R13 | any stage entry while `KillSwitch` is set | no state change; refused and recorded as pending (TROUBLE-LADDER-010, §3.4) |

### 3.4 Checkpoints, kill-switch, pending work and resume

**Checkpoint granularity = the individual ladder transition**, i.e. one ledger record boundary. The
in-flight unit that may finish is one *tool call*, because the tool-call audit record (SPEC-06) is the
finest durable boundary the ladder can observe.

Kill-switch (`AutonomyGates.KillSwitch == true`) semantics:

1. Detection, scrubbing, ledger append, dedup, grouping, canary injection, source liveness and
   verification are **not** work: they continue, so evidence is preserved and the outage is not blind.
2. Every stage entry (§3.2 list) is refused with TROUBLE-LADDER-010 and recorded as pending.
3. An in-flight play finishes its current tool call and starts no further task; an in-flight agent run is
   parked (§3.5) at its next tool-call boundary, never killed mid-call.
4. `verifying → resolved` is allowed while the switch is set (it is observation, adds no work) but
   promotion, spawn, merge and skill-accept are refused and marked pending.
5. Setting the switch persists to config and is re-derived from the last `config` record at boot (SPEC-12
   §3.5): a restart never silently clears it. The change takes effect at the next checkpoint and never
   aborts in-flight work; clearing it is the same.
6. **Where resume continues from:** resume re-evaluates the *state machine* from each incident's persisted
   state plus the pending-transition rows — no command or tool-call is ever replayed. Concretely, for each
   incident in the pending set (rebuilt by folding `refused:true,pending:true` records newer than the
   incident's last accepted transition): the ladder recomputes the transition from `resume_from` under the
   current gates and runs it; if gates still deny it, the row stays pending. There is no queue of
   commands, so resume is deterministic and idempotent.

Pending set construction and pruning are O(open incidents), bounded by `max_open_incidents` (§3.8).

### 3.5 Park and resume (daemon restart, SIGTERM drain, upgrade)

`ParkRecord` is a record, not a state. Parking never changes `LadderState`: the incident stays in
`play:check_only`/`play:applied`/`agent:running` and the park record says what must be finished.

SIGTERM drain (SPEC-12 calls it): (1) stop admissions; (2) `LedgerWriter.Flush` — one forced
group-commit, so the unflushed tail is bounded by the ≤200 ms window (SPEC-01 §3); (3) park every
in-flight play and agent run; (4) write the park records before exit; the process exits within
`drain_timeout` (default 5s), after which SIGKILL is the supervisor's business.

| Field | Value |
|---|---|
| `id` | `ev_` ULID of the park record itself (the ledger's `rec_id`) |
| `inc`, `sig`, `host_id` | incident, sig, host |
| `kind` | `play` \| `agent` \| `spawn` |
| `state` | the `LadderState` at park time (never mutated by parking) |
| `play_run`, `task_index` | the run counter and the task index the run is inside |
| `tool_call_id` | `rec_id` of the last **completed** tool call (applied or checked) |
| `tools_applied` | list of `{tool, rec_id}` already applied in this run — never re-applied (INV-4) |
| `worktree`, `pid`, `router_ref` | for `agent`/`spawn` kinds |
| `parked_ts`, `resume_deadline` | RFC3339 UTC; deadline default `park_ttl` 24h |
| `reason` | `sigterm` \| `crash` \| `upgrade` \| `lease_expired` \| `kill_switch` |
| `resumed_ts`, `resumed_by` | filled on the resume record that references this park `rec_id` |

Re-adoption algorithm at boot (idempotent; a second run produces no new records — a resume whose park
`rec_id` already has a resume record is skipped):

1. Rebuild the ledger index (SPEC-01) and load every `ParkRecord` without a resume record whose
   `resume_deadline > now`.
2. **play park**: re-run `PlayRunner.Check` for the parked `task_index`. Empty diff → the call is already
   applied → advance to the next task and continue. Non-empty diff → the call never landed → the task
   re-enters the normal gate: check_mode under shadow/assisted-without-grant, apply when granted. An
   already-applied tool is never re-applied even though the module is idempotent — idempotency makes the
   *check* safe, and the check is all that is re-run.
3. **agent park**: resolve liveness from the host process table by `pid` (and, when set, by
   `router_ref`). PID alive → re-adopt: attach a watcher, renew the lease (§3.6), wait for completion.
   PID gone → decide from evidence, not from hope: a committed change in `worktree` plus an agent
   completion record → `agent:done`; otherwise the run is lost → `agent:failed` with
   `failure_class:daemon_restart_lost`, which **does not** increment `strikes` (else a restart storm
   would starve the agent rung).
4. **orphaned worktree**: a registered worktree with a dead `pid` and no completion record is either
   re-adopted by the pair (`pid`, `worktree` path) when `git worktree list` for the owning repo still
   registers that path, or explicitly orphaned: `I⟨orphan:true, worktree, pid, router_ref, reason⟩` plus a
   `Notifier.Emit` and an `Outlet.Comment` on the incident, plus a marker file
   `<worktree>/.trouble-orphan.json` carrying the incident id, the sig and the orphan timestamp. The
   worktree is left in place for the SPEC-08 reaper after `orphan_ttl` (default 24h). The ladder never runs
   `git gc`, `git prune` or `git worktree prune` (amendment I; no git subprocess in this package).
5. Resume ordering: oldest `parked_ts` first, and at most one play/agent resumes at a time (the host
   lease), so a restart with 40 parked runs drains serially instead of stampeding the box.
6. Not resumed, ever: an already-applied mutating call (INV-4); a verify window whose evidence was already
   written (its `Window.TSEnd` is in the past → treated as closed and evaluated, not restarted); an
   accepted spawn (query `router_ref`, never re-spawn — SPEC-08 §3 owns the retry); a kill-switch-refused
   transition (re-evaluated per §3.4.6).

### 3.6 The host agent lease

One agent run at a time per host, always (`agent_concurrency` is 1 and is not configurable in v0.1).
`AgentLease` is appended as an `incident`-record payload (`I⟨lease:{…}⟩`) and is therefore rebuildable at
boot; the in-process mutex only guards the fast path.

| Field | Definition |
|---|---|
| `lease_id` | `inc_`-suffixed ULID of the lease record (`lease_`+ULID string) |
| `inc`, `sig`, `host_id` | holder identity |
| `holder` | `Actor.ID` of the run (model + run id) |
| `pid`, `worktree`, `router_ref` | process facts for the re-adopter |
| `granted_ts`, `expires_ts` | `expires_ts = granted_ts + agent_timeout` (default `15m`), extended by renewal |
| `renewed_ts`, `renew_count` | renewal on every tool call and every `lease_heartbeat` (default `30s`) |
| `state` | `requested` \| `granted` \| `renewed` \| `released` \| `expired` \| `reclaimed` |
| `release_reason` | `completed` \| `failed` \| `suspended` \| `sigterm` \| `stale` |

Acquisition copies the fleet's spawn-overlap lesson (enqueue-first, then recheck): (1) append the
`requested` record — durable before any check, the ledger's single writer being the serialization point;
(2) re-read the lease from the index after `lease_recheck` (default `250ms`, longer than the 200 ms
group-commit window so a concurrent request in the same batch is visible); (3) free, or expired
(`expires_ts < now`), or `stale` (holder PID not alive on this host for > `lease_grace`, default `90s`)
→ append `granted`; (4) held by another live lease → return TROUBLE-LADDER-003 (transient) and requeue the
incident with `lease_retry` (default `60s`) backoff, max 3 attempts, after which the incident simply waits
in its current state with `I⟨lease_wait:true⟩`. Contention never escalates — a busy host is not a fault.

### 3.7 Budgets

Per host, per UTC day, per class. Counters live in the index, are recomputed at boot by folding the day's
`incident`/`verify` records, and roll at 00:00 UTC (`BudgetState.resets_ts`). Exhaustion **escalates
instead of running** (AC-5): the stage is never entered, the outlets still fire so a human sees it.

| Class | Default | On exhaustion |
|---|---|---|
| `agent_runs` | 20/day | T07/T26/T33 → `escalated`, TROUBLE-LADDER-013 |
| `play_runs` | 50/day | T07/T18 → `escalated`, TROUBLE-LADDER-013 |
| `research_requests` | 30/day | rung advances to `research:skipped`, no error code of ours (driver decides) |
| `spawns` | 5/day | hot-fix outlet returns `""`, incident escalates (SPEC-08 §5 gate) |
| `agent_cost` | provider budget passthrough | T30 → `escalated`, TROUBLE-LADDER-013 |

A rollover mid-run never aborts the run: the run's cost is charged to the day it started, and the next
admission is evaluated against the new day.

### 3.7a The agent stage's per-call budgets and context compaction (v0.1.1b)

§3.7 counts a day's runs; this section bounds **one call**, so an agent stage that is entered cannot
run away inside its own budget. The three caps are properties of the configured chain (§4.3a) and are
enforced by `internal/llm`, which is why they are testable without a model:

| Cap | Enforced | Behaviour when exceeded |
|---|---|---|
| `max_tokens` (hard) | **before** the request is built | the request is refused; nothing is sent |
| `max_tokens` (observed) | after the response is decoded | a completion whose reported `usage.completion_tokens` exceeds the cap fails the stage; the text is discarded, never truncated and never used |
| `timeout` (wall clock) | **mid-call**, as a per-attempt deadline | the attempt is cancelled at the cap; the chain may move to the next candidate, but a stage whose caller deadline has expired stops |

**One buffered request per stage.** There is no streaming request and no SSE parse: the request body
pins `stream:false`, and an upstream that answers `text/event-stream` is a contract failure, not a
completion to reassemble. A hung stream is the failure mode this rule refuses.

**Context compaction is a hook, and it never truncates.** Before the request is built, the assembled
context is measured. At or below `compact.budget_tokens` nothing happens. Above it:

1. the context is grouped into at most `compact.max_chunks` groups of about `compact.chunk_tokens`
   each, in order — a chunk is never cut in half and never dropped;
2. **each group is summarised by exactly one** completion through the same ordered chain, with
   `max_tokens = min(compact.max_tokens, max_tokens)` (the stage cap bounds the summariser, so a
   summary can never cost more than the run it compacts for);
3. the summaries replace the groups, and `compaction{in_tokens, out_tokens, chunks, max_chunks,
   candidate, model}` is recorded on the stage record (§3.12a).

A split that would need more groups than `max_chunks`, or a summarisation call that fails, REFUSES the
stage (`failure_class:compaction`). There is no code path that returns a shorter context than it was
given without an accounting that says so: silent truncation is the one outcome the hook exists to
prevent. Token counts come from the provider's `usage` when it reports one and from the documented
byte-based estimator otherwise, and every estimated count is flagged `estimated:true` — an estimate is
never presented as a measurement.

**Failure code.** A stage whose LLM work fails or exceeds a cap returns **TROUBLE-LADDER-021**
(permanent) with a `reason` token that names the branch: `no_agent_port`, `no_candidate`, `transport`,
`attempt_timeout`, `rate_limited`, `server_error`, `credential`, `client_error`, `contract`,
`token_cap_exceeded`, `completion_over_cap`, `streaming_refused`, `compaction_not_cappable`. The
returned code is mirrored into the record's `payload.error_code` and the port's class into
`payload.failure_class`; T28's own guard still decides the strike path, so a budgeted failure is a
failed run with evidence, not a silent retry. Retryability is the port's: transport, timeout, 429, 5xx
and a candidate-local credential failure move the chain to the next candidate; a 4xx that is not
429/401/403 stops it, because a malformed request is malformed for every candidate.

### 3.8 Breakers, suppression windows and quiet-close

`Breaker` state changes are `B⟨…⟩` records (`breaker` kind). Suppression is not a separate mechanism: a
suppression window **is** an open breaker with `scope = sig:<sig>` and `reason = suppression_window`.

| Scope | Trips when | Open for | While open |
|---|---|---|---|
| `rule:<name>` | > 5 new incidents/hour from one rule | `30m` | new arrivals fold (T48); the rung never advances |
| `sig:<sig>` | > 3 new incidents or ≥3 suppression cycles/24h for one sig | `60m` | arrivals fold into the newest open incident; TROUBLE-LADDER-014 once per window, then every 100th |
| `source:<kind>` | > 20 new incidents/hour from one source | `30m` | arrivals are recorded and folded; no new incident advances past `recorded` |
| `global` | `open_incidents > 200` (default) or critical host PSI/memory pressure | `15m` | everything holds at `recorded`; verification still runs |

Half-open: after `open_until`, exactly one incident for the scope is admitted as a probe. Its window
passing closes the breaker (`closed`, counter kept); its failure re-opens with `OpenUntil = now + min(2 ×
previous_duration, 4h)` and `Trips++`. A breaker that re-opens 4 times in 24h raises the source's
severity to `critical` and writes an issue through the outlet (the human path is never fully suppressed).

Suppression windows: opened by (a) a rule's `Cooldown` after `resolved`/`escalated` — the flapping guard
of amendment N and AC-2; (b) the ladder itself after the third suppression cycle for a sig in 24h; (c) an
operator (`Suppress` / a dashboard action in shadow of SPEC-10 §2). Arrivals inside a window increment the
group's `Suppressed` counter and the incident's `suppressed_count`; they never create an incident, never
reopen, and never advance a rung.

Quiet-close (`quiet_close` default true): a suppressed incident with zero events for the whole window and
a landed canary transitions to `resolved` (T45) with a `passed` evidence tuple — quiet is only accepted
when it was *observed*, never inferred.

### 3.9 Dedup and the merge rule (AC-22)

Admission is an idempotent upsert keyed by `(sig, open incident)`, with a second key for cross-plane
correlation:

- **Primary key `sig`** (`<source>:<algo>v<norm>:<hex16>`, SPEC-TYPES §6.3). The upsert is:
  open incident for `sig` → fold (increment `arrival_paths[path]`, `Group.Counters.Events++`, comment on
  the linked issue and board row per the cross-ref rule below); no open incident but a `resolved` one
  inside `reopen_window` → T42 (same incident id, `reopen_count++`); else T01 (new incident).
  The idempotency key inside the fold is the arriving `ev_` event id: replaying the same arrival yields one
  record and one counter increment. **Never a duplicate** — one incident, one group, one issue, one board
  row for a given sig at any time (INV-1).
- **Secondary key `inKey`** (the cross-plane link for AC-22):
  `inKey = "ink_" + hex(sha256(source_class + US + subject + US + taxonomy + US + app_kind))[:16]`
  where `source_class` ∈ {`app` (sentinel, collector, generic), `unit` (journald, dbus),
  `resource` (psi, disk, timers, inotify)}, `subject` = for `unit` the unescaped unit name; for `app`
  `unit:<name>` when the `correlation.unit_alias` config maps the project id to a unit, else
  `project:<project_id>` + the first SDK fingerprint string (or the top non-vendor culprit frame when the
  fingerprint is absent); for `resource` `<sensor>|<scope>`, and `taxonomy` = the shared error-taxonomy
  bucket from the class-slug table (SPEC-07 §3) with `unknown` as the fallback.
  Two sigs with equal `inKey` share **one** incident: the incident keeps `sigs[]` (all arrivals, with the
  first as `primary_sig`) and `related_groups[]` (one group per sig, all carrying the same
  `Group.IncidentID`), so AC-22's "one group" is rendered as one row keyed by `IncidentID` (SPEC-04 §3;
  SPEC-10 renders group rows by incident). Merge safety: an `app`-class arrival merges only when the
  subject matches exactly; an arrival with no resolvable subject never merges and stays sig-keyed. Equal
  `inKey` with a different `norm_version` is a different signature space and is linked as `related` only
  (SPEC-TYPES §6.3), never merged.
- **The three D-Bus arrival paths** (`PropertiesChanged(SubState)`, `JobRemoved`, journald follow) carry
  the same `sig` for the same unit fault and therefore fold into one incident with three
  `arrival_paths` counters — the merge rule the judge asked for, expressed as the upsert above.
- **Cross-ref, never duplicate:** the first recurrence after `resolved` (T42) and every 10th fold add a
  comment to the existing issue and board row through the outlets; a new issue or a new board row is
  created only when none exists for the incident (SPEC-08/09 own their idempotency).
- A non-matching open incident found for the arriving `sig` is a conflict: TROUBLE-LADDER-020 is recorded,
  the mismatched incident is quarantined (T47), the correct incident is created, and both ids appear in
  one record — this is an index-integrity failure and is visible, not silently repaired.

### 3.10 Verification: window, canary, evidence tuple

**Window validity.** `W = Rule.VerifyWin` else `verify_default` (`10m`). Valid range `[1m, 24h]`:
`W == 0` → `10m`; `W < 1m` → `1m`; `W > 24h` → `10m`. Every repair records TROUBLE-LADDER-005 with the
original value. A window is never skipped and never zero (a zero window would make "verified" mean
"nothing was checked"). `TSWindowStart` = the timestamp of the last mutating tool-call audit record (or
the outlets' completion for a `record`-ceiling incident); `TSWindowEnd = start + W`. Comparisons use the
monotonic clock in-process (SPEC-INDEX §6.5); the persisted bounds are RFC3339 UTC.

**Sources that must be quiet.** `SourcesExpected` is computed from the source registry — the set of
`host_id:source` pairs configured as reporting for the incident's project/unit and whose sig space covers
the incident's digest — not from who happened to report. Zone-aware (`Evidence.Zone`): an expected source
in another zone is listed and required; a source that registers mid-window is appended, which invalidates
and restarts the window once. Multi-host verification always states the full list; a verification that
cannot name its sources is `invalid`.

**Per-source liveness and gaps.** Every expected source carries `MaxAgeS` (default `300s` for sentinel
projects, `120s` for sensor sources) and a `SourceLiveness` row (`alive` = a canary or any event inside the
window). A `GapRecord` overlapping the window for an expected source, or a `sources_missing` entry, makes
the result `invalid` — the evidence is incomplete, not negative. Gap records are emitted by the source
owners (SPEC-INDEX §3.4: SPEC-03/04/07/09/12); this package consumes them, references them in
`V⟨gap_refs[]⟩`, and never emits `gap`-kind records itself. If a dropout is visible to the ladder but no
gap record exists, `V⟨gap_missing:true⟩` is written and the incident's severity is raised — a missing gap
is a defect of the source subsystem, not a licence to pass.

**Canary.** Two classes, both landing through the real production path: (a) per sentinel project, an
envelope event with the reserved canary fingerprint every `canary_interval` (default `5m`, injected by
SPEC-04); (b) per enabled sensor, a synthetic event with `SensorEvent.Detail.canary == true` every `5m`
(SPEC-03). The ladder records each observation it sees as `V⟨canary:{id,path,observed_ts}⟩`.
`CanarySeen == false` for the window ⇒ `result = invalid` with TROUBLE-LADDER-006 — **a canary that did
not land can never produce `passed`**, which is the whole point of amendment D (a group that is flat
because nothing is arriving must not read as fixed).

**Result kinds** (`VerifyResultKind`, the only three values):

| Result | Conditions (all must hold) | Downstream |
|---|---|---|
| `passed` | `events_observed == 0`; `canary_seen`; `sources_missing` empty; no gap overlapping the window; counter deltas read from the index (not inferred) | T38, T45 |
| `failed` | canary seen, sources alive, no gap, `events_observed > 0` | T39 (+TROUBLE-LADDER-007) |
| `invalid` | canary not seen, or an expected source missing, or a gap overlapping the window, or bounds unclampable | window restarts once (`verify_retry` = 1); a second invalid → T40/T41 (TROUBLE-LADDER-006 / -005) |

The `Evidence` tuple is the ONLY form of a verification result: field-for-field
`ts_window_start, ts_window_end, window_s, events_observed, canary_seen, canary_id, counter_deltas,
sources_expected, sources_alive, sources_quiet, sources_missing, zone, result` (SPEC-TYPES §3.7). No
boolean, no summary flag, no "ok" — every consumer (flow promotion, dashboard, skills accept) reads the
tuple and re-checks `result` itself.

### 3.11 Autonomy gate matrix (stage × mode)

Default mode is `shadow` (non-negotiable #11). Stages are exactly the ten the judges named.

| Stage | shadow (default) | assisted | full |
|---|---|---|---|
| detect | allow | allow | allow |
| record | always (never gated — evidence is not work) | always | always |
| research | allow | allow | allow |
| play-check | allow | allow | allow |
| play-mutate | **deny** — check_mode only, diff returned, nothing applied | grant only: `AllowPlayMutate` true when the rule or the module appears in `Grants` | allow |
| agent | allow (the run executes; every mutating call it makes re-enters play-mutate and is check_mode-forced) | allow; mutating calls only for granted modules (`grants` entry `<rule>|<module>[|<sig-short>]`) | allow |
| spawn | **deny** — the hot-fix lane drafts the board row and does not spawn | grant only (`spawn:<repo>`) | allow (SPEC-08 gates still apply: allowed_repos, max_concurrent, min_free_disk_gb, one-fix-per-sig) |
| merge | **deny** — PRs stay unmerged | grant only (`merge:<repo>`) | merge-by-policy: only when the PR is authored by the trouble hot-fix lane, the window `passed`, every touched path matches `merge_allow_globs` (empty list ⇒ deny), CI is green, no do-not-touch path, and the sig's breaker is closed; anything else escalates |
| promote | **deny** — `Promotion.Decision = pending_human` | grant only (`promote:<sig>`) | allow after a `passed` window |
| skill-accept | drafts the candidate `sk_` (state `drafted`), never promotes it | grant only (`skill-accept:<name>`) | auto-accept when the artifact is signed by a configured signer key, `success ≥ skill_accept_min_success` (default 3), the canary host passed first, and `min_daemon_version` is satisfied (SPEC-11) |

Grant mechanism for assisted: `AutonomyGates.Grants` holds strings `<rule>|<module>[|<sig-short>]`,
`spawn:<repo>`, `merge:<repo>`, `promote:<sig>`, `skill-accept:<name>`. `Granted(rule, module, sig)`
matches the most specific entry and the whole entry must match exactly (no prefix or glob semantics).
Enforcement: every mutating call is passed to SPEC-06's authorize stage with the resolved grant as the
only authority — a module whose scope is not granted returns the policy-refused class and the ladder
records it; the ladder does not mint a code for a refusal it did not make.

`full` is never unconditional: budgets (§3.7), breakers (§3.8), do-not-touch and `merge_allow_globs`
still bind, and the kill-switch overrides all three modes.

Who may flip modes: a write-scoped dashboard action (SPEC-10 §2 `POST /api/autonomy`) or the CLI with
`--autonomy`; the persisted change is recorded by SPEC-12 as a `config` record (kind ownership), and the
ladder subscribes to the resolved-config change event. A mode change takes effect at the next checkpoint,
never mid-call, and a downgrade never aborts an in-flight mutating call — it finishes, then the new mode
applies.

### 3.12 Record payload schemas emitted by this spec

| Kind (owner) | Payload keys (SPEC-01 `Record.payload`) |
|---|---|
| `incident` (this spec) | `transition`, `from`, `to`, `rung`, `entry_rung`, `rule`, `inKey`, `sigs[]`, `arrival_paths{}`, `reopen`, `reopen_count`, `play_runs`, `agent_runs`, `strikes`, `lease{}`, `budget{}`, `breaker_scope`, `suppress_until`, `suppressed_count`, `park{}`, `resume{}`, `orphan{}`, `refused`, `pending`, `resume_from`, `illegal_transitions`, `window{}`, `evidence_ref`, `research_id`, `issue_id`, `task_id`, `spawn_id`, `pending_human`, `severity`, `stabilize_degraded`, `changed`, `folded` |
| `verify` (this spec) | `evidence` (the complete `Evidence` tuple), `gap_refs[]`, `gap_missing`, `invalid_count`, `canary{id,path,observed_ts}`, `window{}` |
| `breaker` (this spec) | `scope`, `state`, `trips`, `opened_ts`, `open_until`, `reason`, `probe_inc`, `previous_duration_s` |
| `agent_run` (this spec, §3.12a) | `stage`, `outcome`, `serving_candidate`, `model`, `endpoint`, `attempts[]`, `usage{}`, `compaction{}`, `failure_class`, `error_code`, `reason`, `prompt_digest`, `research_id`, `brief_digest`, `budget{}` |

Every failure record mirrors the returned code in `payload.error_code` (SPEC-INDEX §5.3).

### 3.12a The `agent_run` record — the ledger stage record (v0.1.1b)

SPEC-07 §3.7 and §3.9 already require `prompt_digest`, `brief_digest` and `research_id` in "the
`agent_run` record written by SPEC-05", and SPEC-01 §3.1 lists `agent_run` as an audit-spine kind — but
the payload was never pinned here. This section pins it: it is written by `RunAgentStage` (§2a) once per
run, success or failure, and it is the record that answers *which candidate served this run, at what
cost, and over what context*.

| Key | Type | Present | Meaning |
|---|---|---|---|
| `stage` | string | always | `agent` — the agent stage is the only writer of this kind from this spec |
| `outcome` | string | always | `done` \| `failed` — the same value the incident's `AgentResult` carries |
| `serving_candidate` | string | success | the chain entry's configured `name`; `""` on failure — never an index, never a URL |
| `model` | string | success | the model id the serving candidate sent |
| `endpoint` | string | success | the credential-free host of the serving candidate (no path, no query, no credential) |
| `attempts[]` | array | always | one `{candidate, model, status, class, reason, latency_ms}` per attempt, in order — the failover's evidence |
| `usage{}` | object | always | `{prompt_tokens, completion_tokens, total_tokens, estimated}` |
| `compaction{}` | object | always | `{applied, chunks, in_tokens, out_tokens, max_chunks, candidate, model}` (§3.7a) |
| `failure_class` | string | failure | the port's class (`transport`, `rate_limited`, `token_cap_exceeded`, `compaction`, …); `""` on success |
| `error_code` | string | failure | the mirrored code — TROUBLE-LADDER-021 (SPEC-INDEX §5.3 rule) |
| `reason` | string | failure | the stable reason token of §3.7a |
| `prompt_digest` | string | when a prompt was sent | `Digest(prompt)` — the join SPEC-07 §3.7 requires |
| `research_id`, `brief_digest` | string | when research informed the run | the `res_` id and the brief digest (SPEC-07 §3.9) |
| `budget{}` | object | always | the §3.7 day counters after the run, `agent_runs` included |
| `inc`, `sig` | string | always | the incident and its sig (`Rec.inc` / `Rec.sig`) |

Rules:

1. **One record per run**, written before the stage returns. A run whose outcome cannot be recorded is
   not a completed run: the ledger is authoritative over the incident's cached state (INV-2).
2. **No credential can appear in it.** The candidate is recorded by name; the key is a `key_ref` that
   never leaves the config (§4.3a); `endpoint` is the host, so a support bundle can name the upstream
   that served without naming a token.
3. `serving_candidate` is empty exactly when `outcome` is `failed`. There is no "unknown" value, and a
   failed run never claims a candidate.
4. It is the size-capped audit-spine kind of SPEC-01 §3.1: refused rather than truncated, and its
   attempt list is bounded by the chain length (`max_attempts`, §4.3a).

### 3.13 Types added to SPEC-TYPES by this spec

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

### 3.13a Cross-plane context — the codeplane bundle (v0.1.1a)

An incident opened by a **code error** and an incident opened by **system pressure** are the same
object to the operator; the agent that works them must see the same picture. The bundle makes
"what is going on" explicit instead of leaving the agent to correlate a stack trace against a PSI
spike by reading two dashboards:

- **Carried on admission.** `Observation.Codeplane` (SPEC-TYPES §3.15.12) carries the code-plane
  facts when `internal/sentinel` admits: `sig`, `group`, `release`, `regressed`, the top-5 recent
  signatures with counts, and the representative sample reference. Sensor-born observations leave
  it nil.
- **Filled by convergence.** When a sensor rule fires on a host whose convergence map (SPEC-04 §3.9)
  already links the event to an open sentinel group (SPEC-04 §3.9a; e.g. a crash-loop that is filling the disk),
  the ladder fills `Codeplane` from the group store before entering the agent rung: the agent's
  prompt then reads "disk full because `payment-worker` crash-looped 412× since release 4f2a1c",
  not "disk full".
- **Persisted.** The bundle is written to the incident record (`Incident.Codeplane`) so park/resume,
  reopens and post-hoc review keep the same material.
- **Copied out.** The research request (SPEC-07) and the issue body (SPEC-09) embed the bundle
  verbatim — one context, every consumer.
- **Never trusted blindly.** The bundle is context, not evidence: the agent's proposed fix still
  passes the verification tuple of §3.10, and a bundle whose `release` disagrees with the running
  release is discarded with a `gap` record rather than shown as fact.

## 4. Wiring

### 4.1 Inbound (who calls the ladder)

| Caller | Call | When |
|---|---|---|
| `internal/sensors` (SPEC-03) | `Admit` | a rule matched and the stabilization window is satisfied |
| `internal/sentinel` (SPEC-04) | `Admit` | a group's event crossed the rule threshold or a release regression opened; the call carries `Observation.Codeplane` (§3.13a) |
| `internal/sentinel` / `internal/sensors` | `CanaryObserved` | every canary injection that is observed in the ledger |
| `internal/sentinel` (SPEC-04) | `SourceLiveness` read | the dashboard and the ladder share one expectation table |
| `internal/lifecycle` (SPEC-12) | `Park` on SIGTERM/upgrade, `ReAdopt` at boot | drain and resume |
| `internal/dashboard` (SPEC-10) | `Gates`, `SetGates`, `Suppress`, `Quarantine`, `Budget` | operator actions; writes carry a write-scoped token |

### 4.2 Outbound (who the ladder calls)

`LedgerWriter.Append/Flush` (SPEC-01) for every record; `PlayRunner` (SPEC-06) for check/apply/rollback;
`ResearchPort` (SPEC-07) for the research rung; `Outlet` (SPEC-08 flow + SPEC-09 issues) for issue,
board row, comment, hot-fix spawn, promotion and close; `IndexReader` (SPEC-01) for the dedup keys,
window counters and gaps; `Notifier` (SPEC-12 escalation path) for orphan and quarantine notices;
`AgentPort` (implemented by SPEC-05 §2a's `internal/llm` client) for the agent rung's one buffered
completion; `SkillLibrary` (implemented by SPEC-11 §2b's local library) for the skills stage's typed
steps. Both newer ports are interface declarations in this package, so the direction below is
unchanged: the implementers import `internal/types`, never `internal/ladder`.
Dependency direction is one-way: the ladder imports `internal/types` (mandatorily), `internal/ledger`
(writer + index), and — for the default evaluator only — `internal/sensors`; `registry`, `research`,
`flow`, `issues`, `dashboard` and `skills` depend on the ladder through the interfaces above, never the
reverse, which is what keeps the build order of SPEC-INDEX §4.1 acyclic.

### 4.3 Config (all keys have defaults; no fleet value is compiled in)

| Key | Default | Meaning |
|---|---|---|
| `ladder.verify_default` | `10m` | window when the rule sets none (valid range `1m`–`24h`) |
| `ladder.verify_retry` | `1` | invalid-window restarts before escalation |
| `ladder.verify_reopens` | `1` | extra play runs allowed after a failed window |
| `ladder.research_play_extra` | `1` | extra play run allowed for a research-supplied draft |
| `ladder.play_max_runs_default` | `2` | effective play cap when `Rule.MaxRuns` is 0 |
| `ladder.play_max_runs_cap` | `5` | values above the cap clamp and record TROUBLE-LADDER-002 |
| `ladder.stabilize_default` | `2m` | replacement for an invalid `for=` |
| `ladder.agent_timeout` / `agent_strikes_window` | `15m` / `24h` | run cap; two-strikes window per sig |
| `ladder.lease_recheck` / `lease_grace` / `lease_retry` | `250ms` / `90s` / `60s` | enqueue-first recheck; stale-holder grace; contention backoff |
| `ladder.kill_switch` / `ladder.autonomy_mode` | `false` / `shadow` | operator state (SPEC-12 precedence, `config` record) |
| `ladder.agent_runs_per_day` / `play_runs_per_day` / `research_per_day` / `spawns_per_day` | `20` / `50` / `30` / `5` | §3.7 |
| `ladder.canary_interval` | `5m` | canary cadence (SPEC-03/04 inject; the ladder observes) |
| `ladder.quiet_close` | `true` | whether a silent suppression window resolves |
| `ladder.reopen_max` | `100` | reopen cap per incident before a new incident opens |
| `ladder.merge_allow_globs` | `[]` | full-mode merge-by-policy allowlist; empty denies |
| `ladder.skill_accept_min_success` | `3` | full-mode auto-accept threshold (SPEC-11) |
| `ladder.correlation.unit_alias` | `{}` | project id → unit name (cross-plane dedup, §3.9) |
| `ladder.source_max_age.{sentinel,sensor}` | `300s` / `120s` | per-source liveness expectation |
| `ladder.breaker.*` | §3.8 table | trip thresholds, open durations, `max_open_incidents` 200 |
| `ladder.drain_timeout` / `park_ttl` / `orphan_ttl` | `5s` / `24h` / `24h` | §3.4–§3.5 |

### 4.3a The `[llm]` table — the agent stage's chain and caps (v0.1.1b)

The agent stage's model work is configured by a top-level `[llm]` table (declared like the subsystem
tables of SPEC-12 §3.1b, and handed to `internal/llm`'s strict decoder as verbatim text). Every key has
a safe default, the compiled default chain is **empty**, and no fleet endpoint, model or key is ever
compiled in: with no table declared, `Deps.Agent` stays unwired and the agent stage refuses with
TROUBLE-LADDER-021 (`reason=no_agent_port`) instead of inventing a model.

| Key | Default | Meaning |
|---|---|---|
| `llm.max_tokens` | `4096` | the HARD completion cap of one stage call (validated `> 0`) |
| `llm.timeout` | `120s` | the wall-clock cap of ONE attempt, applied as a context deadline |
| `llm.max_attempts` | `0` | how many chain entries one stage run may try; `0` = the whole chain (bounded failover) |
| `llm.max_response_bytes` | `1048576` | the buffered response cap; a larger body is refused, not read |
| `llm.fallback_chain` | `[]` | the ordered list of candidate NAMES; omitted = the `[[llm.candidates]]` declaration order |
| `llm.candidates[].name` | required | the stable chain-entry id the `agent_run` record carries |
| `llm.candidates[].base_url` | required | the OpenAI-compatible root (`http`/`https`, no embedded credentials) |
| `llm.candidates[].model` | required | the model id sent in the body |
| `llm.candidates[].key_ref` | required | the NAME of the environment variable holding the key |
| `llm.candidates[].max_tokens` | `0` | this candidate's cap; `0` inherits `llm.max_tokens`, and a larger value is refused |
| `llm.candidates[].timeout` | `0` | this candidate's wall-clock cap; `0` inherits `llm.timeout` |
| `llm.compact.enabled` | `false` | the §3.7a compaction hook; off means an over-budget context is refused |
| `llm.compact.budget_tokens` | `24000` | the assembled-context budget |
| `llm.compact.chunk_tokens` | `6000` | the target size of one summarisation group |
| `llm.compact.max_chunks` | `8` | the summarisation-call cap; a context needing more groups is refused |
| `llm.compact.max_tokens` | `800` | the summarisation completion cap, bounded by `llm.max_tokens` |

Rules:

1. **`key_ref` is a NAME, never a value.** It must match `^[A-Z][A-Z0-9_]{2,63}$`; anything else — a
   provider token, a base64 blob, a path — is a construction refusal. The value is read from the
   process environment at call time (SPEC-12's `[secrets] environment_file` is how it gets there), so
   rotation needs no restart, and no config dump, boot record, record payload or error message can
   carry it.
2. **Strict decode.** An unknown key inside `[llm]` is refused by name, the same rule SPEC-11 §2
   applies to `[skills]` — a typo that silently keeps a cap is how a budget stops existing.
3. **No default endpoint, no default model, no default key.** A host that declares the table owns the
   chain; a host that does not declares nothing.
4. **A declared-but-unbuildable table is a boot refusal** (SPEC-12 §3.1c), recorded with
   TROUBLE-LIFECYCLE-001 naming the key's own message — never a silent fall back to "no model".
5. `llm.candidates` entries are ordered by declaration; `llm.fallback_chain` (the brief's spelling of
   the same idea) selects the order and the subset, and names each entry at most once.

`internal/llm`'s own local types are `LLMConfig` (the resolved table above), `Candidate` (one chain
entry), `CompactConfig` (the hook's four caps) and `LLMRequest`/`LLMResponse` (one buffered call). They
are opaque to this spec's interface: the ladder sees `AgentPort` and `AgentOutcome` only (SPEC-TYPES
§3.15.12a), which is what keeps the client swappable and the record shape stable.

### 4.4 Lifecycle hooks

Boot: rebuild the index → load `AutonomyGates` from config → `ReAdopt` → resume pending transitions.
SIGTERM/upgrade: stop admissions → `Park` → `Flush` → exit. Ledger sequence stall (`/health.json`
`ledger_stall_s` above `60s`, SPEC-12's external checker): the ladder marks any open verify window
`invalid` at close rather than trusting a possibly frozen index (T41).

## 5. Errors

| Code | Class | Raised when | Recovery |
|---|---|---|---|
| TROUBLE-LADDER-001 | permanent | illegal transition attempted (§3.3) | state unchanged; `illegal_transitions++`; 3 refusals → T47 |
| TROUBLE-LADDER-002 | permanent | play `max_runs` exhausted → rung advance | advance to research/agent/outlets; the code is recorded, not fatal |
| TROUBLE-LADDER-003 | transient | host agent lease held by another incident | requeue with `lease_retry`, max 3; then wait |
| TROUBLE-LADDER-004 | permanent | two-strikes per sig per 24h | `agent:suspended`; window end may resume (T34); third failure escalates (T35) |
| TROUBLE-LADDER-005 | permanent | verify window invalid (0, <1m, >24h, negative) | clamp/repair to §3.10 defaults and record; unclampable → T41 |
| TROUBLE-LADDER-006 | permanent | canary not seen in the window → `invalid` | restart the window once; a second invalid escalates (T40) |
| TROUBLE-LADDER-007 | permanent | recurrence inside the window → `failed` | T39 re-run the play while `play_runs < cap + 1`, else T40 |
| TROUBLE-LADDER-008 | transient | rollback of an applied call failed | retry once; two failures in 24h → T17 quarantine |
| TROUBLE-LADDER-009 | transient | research unavailable → `research:degraded` | continue to the agent rung with the degraded marker |
| TROUBLE-LADDER-010 | permanent | kill-switch active at a stage checkpoint | refuse, record pending, resume from the persisted state on clear |
| TROUBLE-LADDER-011 | permanent | autonomy gate denied for this stage | record and stop that stage; shadow blocks only mutations, never detection |
| TROUBLE-LADDER-012 | permanent | incident id not found in the index | re-derive from the ledger; a persistent miss quarantines the caller's incident |
| TROUBLE-LADDER-013 | permanent | per-day budget exhausted | escalate instead of running; the outlets still fire |
| TROUBLE-LADDER-014 | permanent | breaker open for the scope | fold the arrivals (T48); half-open probe after `open_until` |
| TROUBLE-LADDER-015 | transient | park failed (in-flight play not persisted) | abort the drain, keep running, retry the park; the ledger flush has priority |
| TROUBLE-LADDER-016 | transient | resume failed (parked play not re-adoptable) | leave the park record open until `resume_deadline`; then orphan it with a notice |
| TROUBLE-LADDER-017 | permanent | quarantined (≥3 illegal transitions or a corrupt snapshot) | `Unquarantine` with a human actor, or leave it and let retention compact it |
| TROUBLE-LADDER-018 | permanent | suppression window active for this sig | fold arrivals; quiet-close resolves (T45) |
| TROUBLE-LADDER-019 | permanent | stabilization window invalid for this rule | replace with `stabilize_default`; a gap inside the interval resets once, the second reset admits with `stabilize_degraded` |
| TROUBLE-LADDER-020 | permanent | reopen found an open incident whose sig does not match | quarantine the mismatched incident, open the correct one, record both ids |
| TROUBLE-LADDER-021 | permanent | the agent stage's LLM call failed or exceeded a budgeted cap (§3.7a): no port wired, no candidate served, token cap, wall clock, or a compaction that could not be capped | the run is recorded as `failed` with `failure_class` + `reason` (never as an empty diagnosis); T28/T31/T32 decide the strike path |

Precedence when several apply at one checkpoint: TROUBLE-LADDER-010 > -014 > -013 > -011 > -001 > the
stage's own outcome. Every returned code is mirrored into the emitted record's `payload.error_code`
(SPEC-INDEX §5.3). Codes owned by other areas are referenced by their owning spec (§5 there) and are never
re-minted here.

## 6. Edge cases

1. **Daemon restart mid-play** — §3.5: park, resume, never re-apply. A crash (no park record) is
   indistinguishable from a park with `reason:crash`; both are recovered from the ledger's last
   `tool_call` record.
2. **Kill-switch flipped while an agent is mid-tool-call** — the call completes, the run parks, nothing new
   starts, and every refused stage entry is a pending row (§3.4).
3. **Two admissions in one group-commit batch for the same sig** — admission serializes on the ledger
   write and re-checks INV-1 after the append; the second arrival becomes a fold, not a second incident.
4. **Clock jump during a window** — in-process comparisons use the monotonic clock; a skew beyond
   tolerance across hosts marks the affected zone's verification `invalid` (SPEC-INDEX §6.5).
5. **Ledger sequence stall during a window** — `IndexReader.Seq()` frozen for more than `W` → T41
   quarantine-or-escalate instead of a `passed` nobody can trust.
6. **Canary lands after the window closes** — an invalid evidence tuple; the window restarts once.
7. **A source registers mid-window** — appended to `SourcesExpected`, which invalidates and restarts the
   window (a new source's silence was never observed).
8. **Flapping sig (3 suppression cycles in 24h)** — the sig breaker opens (TROUBLE-LADDER-014) and arrivals
   fold instead of spraying plays; 4 re-opens in 24h file an issue for a human (§3.8).
9. **Budget rollover at midnight UTC mid-run** — the run is charged to its start day; the next admission
   sees the new counters (§3.7).
10. **Lease holder dead, PID recycled** — a stale lease past `lease_grace` is reclaimed with a
    `reclaimed` record; the new grant and the reclaim are both in the ledger, in order.
11. **Incident id missing from the index (TROUBLE-LADDER-012)** — re-derive from the ledger once; a repeat
    miss quarantines the caller's incident and notifies.
12. **Dry-run-only incident (shadow)** — a play whose diff is non-empty and never applied (T09/T10 under
    the shadow gate) leaves nothing to verify, so the ladder completes the outlets rung and escalates
    (T18): the issue carries the diff, and a human applies it. `verifying` is entered only after a real
    mutation (T12, T36).
13. **The rule's `for=` spans a gap record** — the interval resets once (TROUBLE-LADDER-019), the second
    consecutive reset admits with `stabilize_degraded:true` rather than losing the incident.
14. **A play whose target resolves to `trouble` itself or its state root** — refused by SPEC-06's
    do-not-touch authorize stage (its policy-refused class); the ladder records the mirror of that code and
    the incident escalates. The daemon is never in the dependency chain of what it repairs.
15. **Two incidents for the same `inKey` from different sig spaces (`norm_version` differs)** — linked as
    `related`, never merged (SPEC-TYPES §6.3); both keep their own evidence windows.
16. **`Unquarantine` on an incident whose worktree is orphaned** — honoured, and the orphan reaper's TTL
    still applies to the worktree (SPEC-08 §3).
17. **The chain is configured but every candidate is down** — the stage fails with TROUBLE-LADDER-021
    and `failure_class` naming the LAST attempt's class; the `agent_run` record carries the whole
    attempt list, so "which upstreams were tried, and what each said" is answerable from the ledger
    alone. The incident gets a strike through T28/T31/T32 — a broken provider is a failed run, never a
    free retry loop.
18. **The context is over budget and the summariser is unreachable** — the compaction pass refuses
    (`failure_class:compaction`) and nothing is sent. The stage fails budgeted rather than sending an
    over-budget context or a truncated one.
19. **A candidate's key_ref does not resolve** — that candidate fails (`class:credential`) and the chain
    moves on; the request is never sent without a credential. With the whole chain unresolvable the
    stage fails with the attempt list naming each `key_ref` by NAME only.

## 7. Testing

`internal/ladder/statemachine_test.go`
- The transition table is data: 48 legal vectors (T01–T48) + 13 refusal vectors (R01–R13) asserted
  cell-by-cell — resulting `LadderState`, emitted record kind, the exact `payload` keys, and the exact
  error code. A count guard asserts `len(legal)=48 && len(refused)=13` so a table edit cannot silently drop
  an edge. Threshold: 0 uncovered rows.
- INV-5 idempotency: 1,000 repeated `Advance` calls with the same `Transition.Seq` produce exactly 1 record
  and 1 state change.
- Illegal transitions: every refusal leaves the state byte-identical and increments
  `illegal_transitions`; the third refusal reaches `quarantined` with TROUBLE-LADDER-017.

`internal/ladder/dedup_test.go` (AC-22)
- Three D-Bus arrival paths (`PropertiesChanged`, `JobRemoved`, journald) with one digest → 1 incident,
  3 `arrival_paths` counters, 0 extra issues, 0 extra board rows.
- Cross-plane: journald(unit) + sentinel(app, unit alias) + collector(unit) → 1 incident, 3 sigs,
  3 groups all carrying the same `IncidentID`, 1 issue, 1 board row, 1 comment on the first recurrence.
- Idempotency: the same arrival replayed 100× → 1 incident, 1 counter increment.
- Volume: 10,000 admissions of one sig in one batch → 1 incident, no duplicate records, admission
  throughput ≥ 2,000/s with group-commit (baseline measured 6,199 req/s at the ingestion edge).
- Reopen: resolve → recurrence → same `inc_id`, `reopen_count == 1`, 1 additional comment, no new issue.
- Reopen conflict: a mismatched open incident → TROUBLE-LADDER-020, the mismatched one quarantined.

`internal/ladder/verify_test.go` (AC-20, AC-21, AC-26)
- The `Evidence` fixture from SPEC-TYPES §3.7 verbatim as the round-trip fixture.
- Canary absent → `invalid` + TROUBLE-LADDER-006, and an assertion that no code path can produce `passed`
  with `canary_seen=false` (grep-style guard on the result constructor).
- One expected source missing → `invalid`; `sources_quiet` lists exactly the quiet-but-alive sources.
- A gap overlapping the window → `invalid`, `gap_refs` non-empty, no `gap`-kind record emitted by this
  package (ownership assertion).
- Recurrence at 9m59s of a 10m window → `failed` + TROUBLE-LADDER-007 → T39 re-run.
- Canary at 10m01s → `invalid` (outside the window).
- Window repair: `0` → 10m, `30s` → 1m, `48h` → 10m, each with TROUBLE-LADDER-005.
- Quiet-close: suppressed incident, canary landed, zero events → `resolved` with `passed` (T45).
- Research degraded path: driver 503 → `research:degraded` → agent rung runs, ledger holds the degraded
  reason and the brief link (AC-20).

`internal/ladder/lease_test.go` (AC-5)
- 8 goroutines acquire concurrently → exactly 1 grant, 7 TROUBLE-LADDER-003, and the `requested` record's
  seq precedes the `granted` record's seq in every case (enqueue-first).
- Killed holder PID → reclaimable after `lease_grace` (90s simulated) with a `reclaimed` record.
- Renewal extends `expires_ts`; a park + `ReAdopt` re-adopts a live PID and orphans a dead one.

`internal/ladder/park_test.go`
- SIGTERM mid-play: park record written, ledger flushed, exit within `drain_timeout`; the applied tool
  appears once in the ledger and a resume re-runs `Check` only (asserted with a counting fake PlayRunner:
  `apply_count` for the parked task == 1).
- Orphan: dead PID + registered worktree → orphan record + marker file + one notification; the ladder
  issues no git command (the fake's git counter is 0).
- `ReAdopt` twice → 0 additional park/resume records (idempotency).
- 40 parked runs drain serially: at most 1 agent run and 1 play run in flight at any instant.

`internal/ladder/gates_test.go` (AC-26)
- All 30 matrix cells asserted; shadow denies `play-mutate`, `spawn`, `merge`, `promote`, `skill-accept`
  and allows detect/record/research/check/agent; the fake registry's `apply_count` is 0 in shadow after a
  full incident lifecycle, and the diff is non-empty (play drafting still works).
- Assisted: a grant for `rule-a|service.reload` allows that call and only that call; an ungranted module
  returns the policy-refused class.
- Full: merge-by-policy with `merge_allow_globs = []` → denied (`pending_human`); with the glob matching
  the only touched path → promoted; a single non-matching path → denied.
- Kill-switch: set mid-incident → 1 refusal per attempted stage entry with TROUBLE-LADDER-010, 0 new tool
  calls, state unchanged; clear → resume continues from `resume_from` with no replayed call.
- Mode downgrade mid-call: the in-flight call finishes, the next call is gated.

`internal/ladder/budget_breaker_test.go` (AC-4, AC-5)
- Budget 20/day: run 21 escalates with TROUBLE-LADDER-013 and the agent-run counter stays at 20.
- Rollover at 00:00 UTC: counters reset, the day field changes.
- Breaker trips on the 6th incident/hour → `open` 30m; the half-open probe passing closes it; a failing
  probe re-opens with the doubled duration capped at 4h.
- Suppression: 1 code record at the window's first arrival plus every 100th; 3 cycles in 24h opens the sig
  breaker; arrivals never advance a rung (AC-4).

`internal/ladder/stabilize_test.go` (AC-2, AC-3)
- `for=2m` measured from the last qualifying event; a false sample at 1m50s resets; a gap in the interval
  resets once with TROUBLE-LADDER-019 and the second reset admits with `stabilize_degraded`.
- `for=-1s` and `for=25h` → `stabilize_default` + TROUBLE-LADDER-019; `for=0` admits immediately.
- Entry rungs: `record`, `play`, `research`, `agent` each walk the expected rung sequence (AC-3), and a
  ceiling of `play` ends at outlets + escalate rather than entering research.

`internal/ladder/codeplane_test.go` (AC-31)
- Sentinel-born admission: the `Admit` call that carries `Observation.Codeplane` persists it to
  `Incident.Codeplane` byte-identically, and a park + resume re-reads the same bytes.
- Sensor-born admission with a convergence-map hit (SPEC-04 §3.9a): the rung's `Incident.Codeplane` holds
  BOTH planes — `Side`, `RuleID` and `Readings` from the firing rule, `Sig`/`GroupID`/`Release`/`Regressed`/
  `Recent` from the sentinel accessor; with no hit the field stays nil and the rung runs unchanged.
- The bundle changes no rung: the same scripted stream with and without a bundle produces the same
  `LadderState` sequence and the same record kinds (context, never evidence — §3.10 is untouched).
- A bundle whose `Release` disagrees with the running release never arrives: the admission is processed with
  `Codeplane == nil` and the run holds exactly the one `gap` record the sentinel wrote (SPEC-04 §3.9a) and
  **0 ladder-side gap records** (ownership assertion).
- Copy-out join: the `Subject.Context["codeplane"]` handed to the `ResearchPort` fake is byte-equal to
  `Incident.Codeplane` (SPEC-07 §3.10a), and the issue body rendered by the `Outlet` fake contains the same
  bytes (SPEC-09 §3.13a).

Whole package: `go test -race -count=1 ./internal/ladder/...` ≤ 60s, zero skipped tests, and the
`Evidence` fixture file shared with SPEC-TYPES' JSON round-trip test so the tuple cannot drift.

`internal/ladder/agent_test.go` (AC-3, AC-5 — the §3.7a/§3.12a amendment)
- The port is satisfied by the shipped client: `var _ AgentPort = (*llm.Client)(nil)` (a broken
  signature is a compile error, not a runtime surprise).
- A serving run: the fake port answers with a candidate name and usage; `RunAgentStage` writes exactly
  ONE `agent_run` record whose `payload` carries `serving_candidate`, `model`, `endpoint`, `usage{}`,
  `compaction{}`, `attempts[]` and `outcome:"done"`, and whose `inc`/`sig` are the incident's.
- A failed run: the fake port returns an outcome with `failure_class` set; the record carries
  `outcome:"failed"`, `serving_candidate:""`, `error_code:TROUBLE-LADDER-021` and the class, and the
  incident's `AgentResult` carries the same class so T28's guard is satisfied by evidence.
- No port wired → TROUBLE-LADDER-021, `reason=no_agent_port`, **zero** `agent_run` records (a stage
  that cannot call a model must not look like one that did).
- Kill-switch set → the stage is refused with TROUBLE-LADDER-010, recorded pending, one refusal record,
  zero calls into the port.
- Budget: 21 runs against a 20/day counter escalates through T26/T33 with TROUBLE-LADDER-013 and the
  counter stays at 20 — the per-call caps are the port's and the per-day cap is this spec's.

`internal/ladder/library_test.go` (AC-24)
- `SkillPlays` reads through the port and returns the compiled plays in name order; with no library
  wired it returns an empty list and no error (the read surface is not a failure).
- `RunSkillStep` under `shadow` calls the port with `mode=check_mode` and mutates nothing; under
  `assisted` with the module grant it asks for `apply`; without the grant it refuses with
  TROUBLE-LADDER-011 and the port is never called.
- Kill-switch mid-run: the step entry is refused with TROUBLE-LADDER-010 and recorded as pending.

## 8. hilo impact

Files created (all new, greenfield repo `~/trouble`):
`internal/ladder/{ladder.go, statemachine.go, transition.go, dedup.go, inkey.go, verify.go, evidence.go,
canary.go, liveness.go, gates.go, grants.go, killswitch.go, pending.go, lease.go, budget.go, breaker.go,
suppress.go, park.go, readopt.go, orphan.go, config.go, errors.go}` plus the seven `_test.go` files in
§7.

Added by the v0.1.1b amendment (§2a, §2b, §3.7a, §3.12a, §4.3a):

| Path | Contents |
|---|---|
| `internal/ladder/agent.go` | `AgentPort`, `RunAgentStage`, the `agent_run` payload builder (§3.12a) |
| `internal/ladder/library.go` | `SkillLibrary`, `SkillPlays`, `RunSkillStep` at the §3.11/§3.4 gates |
| `internal/ladder/agent_test.go`, `internal/ladder/library_test.go` | the §7 batteries above |
| `internal/llm/client.go` | the buffered OpenAI-compatible client, the ordered chain, the per-attempt wall-clock cap |
| `internal/llm/compact.go` | the capped single-shot compaction pass and the token estimator |
| `internal/llm/config.go` | the strict `[llm]` decoder, the `key_ref` shape rule, the defaults |
| `internal/llm/errors.go` | the failure classes, retryability and the stable reason tokens |
| `internal/llm/{client_test.go, fallback_test.go, compact_test.go, testutil_test.go}` | the contract, fallback and budget batteries (httptest servers, no socket, no real key) |
| `internal/types/llm.go` | `LLMUsage`, `LLMAttempt`, `LLMCompaction`, `AgentOutcome` (SPEC-TYPES §3.15.12a) |

Dependency direction (one-way, per SPEC-INDEX §4.2): `internal/ladder` **imports** `internal/types`
(mandatory, highest fan-in node in the repo), `internal/ledger` (writer + index read), and
`internal/sensors` (the default `RuleEvaluator` only). It **exports** the interfaces in §2, so
`internal/registry`, `internal/research`, `internal/flow`, `internal/issues` and `internal/dashboard`
depend on the ladder without the ladder depending on them — the fan-out stays inside `Deps` and the
build order of SPEC-INDEX §4.1 stays acyclic. Nothing imports the ladder from `internal/types`.

Blast radius: the ladder is the highest fan-in behaviour package (every detection path calls `Admit`,
every outlet is called from it) and the owner of three record kinds. A change to the transition table,
the `Evidence` semantics, the gate matrix or a payload key is a cross-spec change: it touches AC-3, AC-4,
AC-5, AC-20, AC-21, AC-22 and AC-26 and therefore requires a SPEC-INDEX §3.2 review in the same commit.
No existing repository is modified: trouble is greenfield, and this spec creates no artifact outside
`~/trouble`.
