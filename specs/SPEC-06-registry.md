# SPEC-06 — registry: module SDK v1, the 6-stage call contract, plays, do-not-touch, polkit (trouble v0.1)

Spec: SPEC-06
Area prefix: TROUBLE-REGISTRY
Package: internal/registry
Consumed types: Module, Descriptor, Diff, DiffEntry, Result, RollbackHint, VerifyResult, ToolCall, CallStage, Play, PlayTask, DoNotTouch, IdempotencyClass, ErrorClass, ToolCallRequest, PlayRun, TaskRun, RegistryDeps, Actor, Record, RecordKind, Prefix
Local types: targetDeclarer, protectedTarget, policyDecision, moduleEntry, stageClock, configSpan
ACs: AC-7, AC-23
PRD: §06, §11

## 1. Purpose

`internal/registry` is the daemon's entire action surface. Every state-changing act trouble can perform —
from a rule's reflex play, from a human on the CLI, and from a budgeted agent run — is one typed tool call
through this package. There is no second path: the agent harness has no shell, no file API, no `os/exec`
handle and no network client; its only capability is a `types.ToolCallRequest` handed to `Registry.Call`.

This file pins, as build-ready contracts:

1. **The module SDK, frozen at v1** — the four-method `types.Module` interface exactly as typed in
   SPEC-TYPES §3.8, the `Descriptor` shape, the generated JSON schema (dialect, generator, validation),
   and the `testkit` conformance harness that every shipped module must pass (AC-23).
2. **The 6-stage call contract** — `authorize → validate → dry-run → apply → verify → audit` — as one
   exported entry point with six internal stages; per stage: input, output, failure behaviour, and the
   ledger record emitted (AC-7).
3. **Plays as data** — the TOML schema in full, the `when:` expression language (SPEC-03 §3.4, verbatim;
   no second dialect), `register`/`retries`/`on_fail` semantics, the versioned on-disk path, and the five
   shipped default plays.
4. **Do-not-touch** — the file format, the compiled-in mandatory floor, the enforcement point *before*
   validation, and the `policy_refused` class of a hit (never `permanent`, never retried).
5. **The polkit install-time artifact** — the `.rules` file shape, the actions it covers, why `service.*`
   is `auth_admin` on this class of host, the install step, and the distinct POLICY-REFUSED class
   surfaced to the ladder when the artifact is absent.
6. **The v0.1 shipped modules** — `config.{get,set,list}`, `service.{status,reload,restart}`,
   `file.{read,patch}`, `proc.{top,connections}`,
   `flow.{file_issue,create_task,comment_task,spawn_foreman,promote,rollback}` — each with descriptor, args
   schema, check diff, apply semantics, verify method, error codes, do-not-touch interaction and
   idempotency story.

Non-goals of this file: the ladder's rung decisions and autonomy gate matrix (SPEC-05), board/issue
driver internals (SPEC-08, SPEC-09), scrubbing rules (SPEC-02), ledger durability (SPEC-01).

AC coverage: **AC-7** is exercised by the 6-stage call contract and its ledger record (§2, §3.5); **AC-23** is exercised by the `testkit` conformance harness (§7). Sections no AC reaches are **design constraints**: the interface signatures (§2), the do-not-touch and polkit artifacts (§3.3, §4), the play/`when:` schema (§3.4) and the refusal semantics (§5, §6) are obligations of this subsystem rather than user-visible acceptance criteria — the loop records them as constraints per SPEC-INDEX §7 step 2.

## 2. Interface

### 2.1 The module surface (frozen — do not evolve in v0.1)

`types.Module` (SPEC-TYPES §3.8) is the whole SDK:

```go
type Module interface {
    Descriptor() Descriptor                                                     // name, version, schema, scopes, idempotency, check_mode, timeout, mutating
    Check(ctx context.Context, args map[string]any) (Diff, error)               // dry-run: the diff it WOULD apply; never mutates
    Apply(ctx context.Context, args map[string]any) (Result, error)             // converges the target to the described state
    Verify(ctx context.Context, args map[string]any) (VerifyResult, error)      // immediate, local proof the effect happened
}
```

Four methods, no additions, no optional methods promoted to the interface. Two properties follow from that
and are normative:

- The six stages are **not** six methods. `authorize`, `validate` and `audit` are the registry's (daemon
  side); `dry-run`, `apply` and `verify` map to `Check`, `Apply`, `Verify`. The module never learns the
  caller's identity, never sees the grants, and never writes the ledger.
- Anything a module needs beyond the interface (target extraction for do-not-touch, fixture naming for
  `testkit`) is satisfied through an optional, package-private extension. Extra capabilities never widen
  the frozen surface.

```go
// package-private extension; satisfied by every shipped module in package registry.
type targetDeclarer interface {
    ProtectedTargets(args map[string]any) []protectedTarget   // {kind: path|unit, value: canonical string}
}
```

The registry type-asserts it. A module that does not implement it falls back to a key scan over `args`
(`path, paths, file, files, source, target, dest, unit, units`) collecting string values. Every shipped
module implements `ProtectedTargets`; the fallback exists only so a third module cannot silently opt out
of do-not-touch by omission.

**(v1.0 hand-off)** The interface is frozen at v1 but third-party SDK packaging — a versioned
`trouble-module-sdk` Go module, semver'd descriptor compatibility, an out-of-tree build tag and a public
`testkit` documentation site — is not part of v0.1. In v0.1 `testkit` is published inside this repository
(`internal/registry/testkit`) and its API in §2.4 is the seam a v1.0 SDK wraps.

### 2.2 The registry's exported surface

```go
// package registry
func New(deps RegistryDeps) (*Registry, error)              // validates + registers every shipped module; error = TROUBLE-REGISTRY-014
func (r *Registry) List() []types.Descriptor                // stable, name-sorted; the CLI/dashboard inventory
func (r *Registry) Descriptor(name string) (types.Descriptor, error)
func (r *Registry) Call(ctx context.Context, req types.ToolCallRequest) (types.ToolCall, error)
func (r *Registry) RunPlay(ctx context.Context, p types.Play, base types.ToolCallRequest) (types.PlayRun, error)
func (r *Registry) PolicyCheck(ctx context.Context) (types.VerifyResult, error)   // polkit probe at boot + on demand
func LoadPlay(path string) (types.Play, error)              // TOML → Play, static target + when: validation
func LoadDoNotTouch(path string) (types.DoNotTouch, error)  // TOML → DoNotTouch merged with the compiled-in floor

// Runner is the adapter that satisfies SPEC-05 §2's PlayRunner interface (the ladder's consumer view of
// this package). Bound with the caller's context so the ladder passes identity once, not per task.
func (r *Registry) Runner(base types.ToolCallRequest) *Runner
func (x *Runner) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error)
func (x *Runner) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error)
func (x *Runner) Rollback(ctx context.Context, t types.ToolCall) error
```

`Runner` is a thin adapter, not a second contract: `Check`/`Apply` fill `Module`/`Args` on the bound base
request and call `Call` (mode `check_mode` / `apply`), returning the module-level value **and** the audit
record the ladder stores as its `tool_call_id`. `Rollback` re-authorizes `t.Result.Rollback` (module +
args) through the full six stages, so a rollback is itself an audited tool call — and a PONR module's
unsupported hint returns `TROUBLE-REGISTRY-015` without touching a target. The bound base carries `inc`,
`rule`, `sig`, `actor`, `grants` and `DeadlineS`; the adapter derives `IdemKey` per task index (§3.4).

`Call` returns the audit record it appended (never `nil` on a completed call, including refusals) so the
caller — ladder, CLI, tests — holds the in-memory copy of what was durable. `Call` returns a non-nil
`error` only when nothing was appended (see §5, audit-stage failure).

`RegistryDeps` is the whole wiring surface; every collaborator is a function value so that no import cycle
and no cross-package interface is created (`internal/registry` imports `internal/flow` and
`internal/issues` through closures built in `cmd/troubled` and never imports `internal/ladder`):

```go
type RegistryDeps struct {
    Append     func(rec types.Record) (types.Record, error)                       // SPEC-01 writer (single writer)
    Scrub      func(target string, in []byte) (types.ScrubResult, error)          // SPEC-02, ledger copy of args only
    Gates      func() types.AutonomyGates                                        // SPEC-05 snapshot, read once per call
    FileIssue  func(ctx context.Context, a map[string]any) (map[string]any, error) // SPEC-09 EnsureBySig via SPEC-08
    CreateTask func(ctx context.Context, a map[string]any) (map[string]any, error) // SPEC-08 board-jsonl / task-router
    Comment    func(ctx context.Context, a map[string]any) (map[string]any, error) // SPEC-09 Comment
    SkillAuthorize func(ctx context.Context, skillID, module string, scopes []string) error // the Authorizer seam (SPEC-11 implements it); nil denies skill-sourced mutating calls
    DbusAddress string                                                            // registry.dbus_address ("" = systemd discovery)
    Now        func() time.Time                                                   // monotonic-capable clock, injected for tests
}
```

### 2.3 The 6-stage call contract

One call = one `ToolCall` record with a `Stage []CallStage` array in fixed order. `Mode` is
`check_mode | apply` and decides which stages run: a `check_mode` call runs stages 1, 2, 3, 6 (no mutation,
no verify); an `apply` call runs all six.

| # | Stage | Input | Output | Failure behaviour | Ledger / audit record |
|---|---|---|---|---|---|
| 1 | **authorize** | `ToolCallRequest{module, args (raw map), mode, grants, source, inc, idem_key, deadline_s, actor}` + gates snapshot + do-not-touch set + polkit capability | `policyDecision{allow, mode (final), grants[], reason}` | refuse: `Stage[0].OK=false`, the module is never called, no target is touched, the ladder escalates and never retries | `kind=tool_call` outcome record (mode as requested, `Result=nil`) |
| 2 | **validate** | raw `args` + `Descriptor.Schema` | typed args: paths made absolute and symlink-resolved, units normalized, ints accepted only as JSON integers, unknown keys rejected | `TROUBLE-REGISTRY-002` schema violation or `TROUBLE-REGISTRY-018` decode failure; the target's mtime and sha256 are byte-identical afterwards | `kind=tool_call` outcome record, `Stage[1].OK=false` |
| 3 | **dry-run** | typed args | `Diff{Empty, Entries[{path,before,after}], Summary}` from `Module.Check` | `TROUBLE-REGISTRY-003` (transient), `TROUBLE-REGISTRY-011` (permanent), `TROUBLE-REGISTRY-009` (timeout) | `apply` mode → **intent** `kind=tool_call` record carrying stages 0–2, appended *before* any mutation (crash evidence); `check_mode` → the single terminal record (4 stages) |
| 4 | **apply** | typed args (`apply` mode only) | `Result{Changed, Applied, Output, Rollback, DurationMS}` from `Module.Apply` | `TROUBLE-REGISTRY-004`/`010` (transient, retried only per play `retries`), `011` (permanent), `012` (idempotency violation), `009` (timeout) | none at this stage; the result rides the outcome record written at audit |
| 5 | **verify** | typed args + `Result` | `VerifyResult{OK, Method, Detail, Evidence}` from `Module.Verify` | `TROUBLE-REGISTRY-005` → rollback via `RollbackHint` when invertible, else escalate with `TROUBLE-REGISTRY-015`; a timeout or verify-fail is never retried blind | outcome record carries `Verify`; the window verification is SPEC-05's `kind=verify` record, not this one |
| 6 | **audit** | the fully populated `ToolCall` | one append to the ledger (`kind=tool_call`); `kind=play_run` at the end of a play run | append failure → `Stage[5].OK=false`, `payload.audit_failed=true`, `payload.upstream_code` = the SPEC-01 ledger append-failure code (SPEC-01 §5), the ladder raises an applied-unlogged incident | `kind=tool_call` outcome record; `kind=play_run` when the call ran under a play |

Every stage appends a `CallStage{Stage, OK, Detail, MS}` — `Detail` is a one-line, scrubbed, value-free
string. The stage list is therefore the audit trail AC-7 asserts, in order, with `MS >= 0` for each. The
table numbers the stages 1–6; the `CallStage` array is 0-indexed, so `Stage[0]` is authorize, `Stage[3]` is
apply and `Stage[5]` is audit.

**Authorize is a fixed seven-step ladder** (deny by default at every step; the first refusal wins):

| Step | Check | Refusal code |
|---|---|---|
| a1 | module name exists in the registry | `TROUBLE-REGISTRY-001` |
| a2 | kill-switch off (from the SPEC-05 gates snapshot) | `TROUBLE-REGISTRY-006` (`reason=kill_switch`) |
| a3 | **do-not-touch**: `ProtectedTargets(args)` vs the merged set — *before* validation | `TROUBLE-REGISTRY-007` (`policy_refused`) |
| a4 | capability: `mutating` and polkit authorization for the unit verb | `TROUBLE-REGISTRY-006` (`reason=polkit_missing_policy`, `capability_absent`) |
| a5 | skill allowlist when `source` is `skill:<sk_id>` | `TROUBLE-REGISTRY-006` (`reason=skill_allowlist`) |
| a6 | every `Descriptor.Scopes` member present in the effective grants | `TROUBLE-REGISTRY-008` |
| a7 | autonomy gate: shadow forces `check_mode`; a point-of-no-return module needs an explicit module-name grant in every mode | `TROUBLE-REGISTRY-008` (mode forced) / `TROUBLE-REGISTRY-015` (PONR without an explicit grant) |

### 2.4 The conformance harness — `testkit`

```go
// package testkit (internal/registry/testkit)
type Fixture struct {
    Name        string         `json:"name"`         // "file.patch/already-applied"
    Args        map[string]any `json:"args"`         // absolute paths always under the fixture temp root
    Mutates     bool           `json:"mutates"`      // true when Apply is expected to change state
    SecondApply string         `json:"second_apply"` // "ok-noop" (convergent) | "idem-key" (once) | "pure"
}

func TestDescriptor(t *testing.T, m types.Module)                       // schema presence, dialect, scope list, CheckMode, timeout bounds
func TestSchemaRejection(t *testing.T, m types.Module, bad []map[string]any) // every malformed args set must be rejected before any side effect
func TestIdempotency(t *testing.T, m types.Module, f Fixture)
func TestCheckApplyEquivalence(t *testing.T, m types.Module, f Fixture)
func Run(t *testing.T, m types.Module, fixtures ...Fixture)             // the three obligations, in order
func RunAll(t *testing.T, ms map[string]types.Module, dir string)       // every registered module + every fixture in testdata/
func LoadFixtures(dir string) ([]Fixture, error)
func FakeBus(t *testing.T, units ...string) string                      // returns a unix: D-Bus address speaking org.freedesktop.systemd1
```

The three mandatory obligations, exactly as AC-23 words them:

1. **Idempotency, double-apply = ok/ok with exactly one change.** `Apply(args)` → `Changed=true` with
   non-empty `Applied`; `Apply(args)` again → no error, `Changed=false`, `Applied` empty, and the fixture's
   observation log records exactly one mutation. For `convergent` and `pure` modules this is the whole
   assertion. For `once` modules (`service.reload`, `service.restart`, `flow.comment`) the second
   `Apply` carries the **same `IdemKey`** and must return `Changed=false` with exactly one execution
   recorded; a third `Apply` with a *different* `IdemKey` must execute again and return `Changed=true`.
2. **Check-vs-apply equivalence.** `d0 := Check(args)`; `r := Apply(args)`; `d1 := Check(args)`; then
   `d1.Empty == true` and `normalize(d0.Entries) == normalize(r.Applied)` — the diff `Check` predicted is
   exactly what `Apply` changed, compared as a path-keyed set of `{before, after}` values after
   normalization (numbers via `json.Number`, paths via canonical form). For `pure` modules the assertion
   is `d0.Empty == true && r.Changed == false && len(r.Applied) == 0 && len(r.Output) > 0`.
3. **Schema-violation rejection.** Every case in `TestSchemaRejection` — missing required key, wrong
   type, unknown key, out-of-range int, bad regex target — must be refused by `validate` with
   `TROUBLE-REGISTRY-002`, before `Check` or `Apply` is reached, with the target's mtime and sha256
   unchanged.

**The negative modules.** Two deliberate defects ship as test-only packages, never registered and never
linked into the daemon binary (`internal/registry/modules/register.go` is the only registration site):

- `internal/registry/testkit/badmodule/nonidempotent.go` — declares `Idempotency = convergent` and
  `CheckMode = true`, then appends a byte to a counter file on *every* `Apply`. `testkit.Run` must fail it
  with `TROUBLE-REGISTRY-013` on obligation 1.
- `internal/registry/testkit/badmodule/nocheckmode.go` — declares `Mutating = true` with
  `CheckMode = false`. `New()` must refuse it with `TROUBLE-REGISTRY-014` on obligation 0.

**The exact CI commands** (`make conformance` is the one gate; the two commands are what it expands to):

```
go test ./internal/registry/... -count=1 -race
go test ./internal/registry/testkit/... -run 'TestHarnessRejectsNonIdempotent|TestHarnessRejectsNoCheckMode|TestSchemaDialectClosed' -count=1
```

`TestHarnessRejectsNonIdempotent` asserts that `testkit.Run` **fails** the bad module and that the failure
carries `TROUBLE-REGISTRY-013`; a harness that quietly passes a broken module is itself a CI failure.
`TestSchemaDialectClosed` asserts every generated schema declares
`"$schema": "https://json-schema.org/draft/2020-12/schema"` and contains no keyword outside §3.3.

### 2.5 CLI surface (humans only — the agent never gets a shell)

```
trouble tool list [--json]                     # name, version, scopes, idempotency, mutating, capability state
trouble tool check <module> --args-file f.json # stages 1,2,3,6; prints the ToolCall JSON; touches nothing
trouble tool call <module> --args-file f.json --grant <module|scope:x:y>   # stage 4 requires the explicit grant
trouble play list | play check <play> | play run <play>
trouble registry policy                        # polkit authorization + .rules install state, non-mutating
```

`trouble tool call` refuses to apply without `--grant`, and every CLI call is recorded with
`Actor.Kind=human`

## 3. Data model

### 3.1 Types added to SPEC-TYPES by this spec

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

### 3.2 Descriptor, schema generation and descriptor validation

A descriptor is a Go literal in the module's own file — one `var descriptor = types.Descriptor{...}` per
module — and the JSON schema in it is **generated, never hand-written**:

- **Generator:** `internal/registry/schemagen`, first-party, stdlib `reflect` only (no third-party
  dependency: the allowed dependency set of SPECS-BRIEF §3 is untouched). It walks the module's args struct
  and emits `additionalProperties: false`, `required` sorted, `properties` sorted by key, and a
  byte-deterministic output (verified by a 100-iteration golden test).
- **Dialect:** JSON Schema **draft 2020-12**, declared as `"$schema": "https://json-schema.org/draft/2020-12/schema"`
  with `"$id": "https://trouble.local/schema/registry/<module>@<version>"`.
- **Committed artifact:** `make schema` writes `internal/registry/schema/<module>@<version>.json` for every
  registered module; `git diff --exit-code internal/registry/schema/` is part of CI, so a descriptor edit
  without a regenerated schema fails the build.
- **Validation at build time and at boot:** `internal/registry/validate` implements the *closed* keyword
  subset the generator can emit — `type, required, properties, additionalProperties, enum, const, minimum,
  maximum, minLength, maxLength, pattern, items, minItems, maxItems, oneOf, default`. Any other keyword in
  a committed schema is a build failure (`TestSchemaDialectClosed`), so the subset can never silently
  ignore a constraint. At boot, `Register` regenerates the schema in memory and compares it byte-for-byte
  with the embedded file; a mismatch is `TROUBLE-REGISTRY-014` and the daemon exits (a tampered or stale
  schema never reaches a call).
- `Descriptor.CheckMode` MUST be `true` for every module with `Mutating == true` — `TROUBLE-REGISTRY-014`
  otherwise (AC-23 condition 2, enforced at registration, not at call time).

### 3.3 Scope vocabulary (deny by default)

| Scope | Grants | Shipped modules |
|---|---|---|
| `config:read` | read a config file's keys/values | `config.get`, `config.list` |
| `config:write` | write one key in place | `config.set` |
| `service:read` | read unit state | `service.status` |
| `service:write` | reload/restart a unit | `service.reload`, `service.restart` |
| `file:read` | read a file inside the read roots | `file.read` |
| `file:write` | patch a file inside `file.allow_roots` | `file.patch` |
| `proc:read` | read process/fd tables | `proc.top`, `proc.connections` |
| `flow:issue` | ensure/close an issue | `flow.file_issue` |
| `flow:task` | write/comment a board row | `flow.create_task` |
| `flow:comment` | append a comment | `flow.comment` |

Effective grants for a call = `rule.auto_grants` (SPEC-03 `Rule`) ∩ autonomy mode (SPEC-05) ∩ skill
`allowed_modules` when the source is a skill ∩ the do-not-touch scope denies. A grant token is either a
module name (`service.reload`) or a scope wildcard (`scope:service:write`). No rule, no grant, no call.

| Call class | shadow | assisted | full |
|---|---|---|---|
| pure (read-only) | apply | apply | apply |
| convergent (mutating, invertible) | `check_mode` forced | apply iff granted by the rule | apply |
| PONR (all six `flow.*` modules, `service.reload`, `service.restart`) | `check_mode` forced | apply iff granted **by module name** (a `scope:` wildcard never authorizes a PONR call) | same explicit-name requirement |

Step a5 is the **`Authorizer` seam**: when `source` is `skill:<sk_id>` the registry calls
`RegistryDeps.SkillAuthorize` and applies its verdict (SPEC-11 owns the hook implementation and re-checks
`allowed_modules` against the installed skill row; the violation surfaces as `TROUBLE-REGISTRY-006`,
`reason=skill_allowlist`, with SPEC-11's own record alongside). A `nil` hook denies every skill-sourced
mutating call — deny-by-default on the one remotely authored input path in the system.

### 3.4 Idempotency classes and the replay key

| Class | Contract | Modules |
|---|---|---|
| `pure` | `Apply` is a read: `Changed=false`, `Applied` empty, `Output` carries the payload | `config.get`, `config.list`, `service.status`, `file.read`, `proc.top`, `proc.connections` |
| `convergent` | `Apply` drives the target to a described state; a second `Apply` with identical args returns `Changed=false` with an empty diff | `config.set`, `file.patch`, `flow.file_issue`, `flow.create_task` |
| `once` | `Apply` is an event, not a state; each distinct `IdemKey` executes once, a replay of the same key is a no-op returning `Changed=false` | `service.reload`, `service.restart`, `flow.comment_task`, `flow.spawn_foreman`, `flow.promote`, `flow.rollback` |

The `IdemKey` is derived by the caller (§3.1) and is the mechanism that makes daemon restart safe: SPEC-05
parks an in-flight play and resumes it from the ledger, and "never re-execute an applied mutating call" is
implemented as *replay the same `IdemKey`*. For `convergent` modules the guard is the module's own
`Check` (already-applied → `Diff.Empty`), for `once` modules it is the registry: `Call` keeps the last
4096 `(module, IdemKey)` pairs of the current ledger generation in memory (rebuilt at boot from
`play_run`/`tool_call` records) and short-circuits a replay with `Changed=false`, `Stage[3].Detail="replayed"`,
no module invocation.

### 3.5 Rollback and the point-of-no-return rule

`RollbackHint` is filled on every `apply`:

| Module | `Supported` | `Module` | `Args` |
|---|---|---|---|
| `config.set` | true | `config.set` | `{path, key, value: <before>}` |
| `file.patch` | true | `file.patch` | `{path, restore_from: "<backup id>"}` |
| `service.reload`, `service.restart`, `flow.*`, all `pure` modules | false | `""` | `{}` (empty) |

Modules with `Mutating == true` and `Rollback.Supported == false` are the **point-of-no-return (PONR) set**:
`service.reload`, `service.restart`, `flow.file_issue`, `flow.create_task`, `flow.comment_task`,
`flow.spawn_foreman`, `flow.promote`, `flow.rollback`. **A PONR call
is never applied without an explicit grant naming that module** — in every autonomy mode including `full`,
the grant token must be the module name itself; a `scope:` wildcard is refused. Absent that grant the call
is refused at authorize with `TROUBLE-REGISTRY-015` (`permanent`), recorded, and escalated. A rollback
that meets a PONR call in the inverse list ends with `TROUBLE-REGISTRY-015` in the play-run record
(`outcome="rolled_back"`, `Tasks[i].ErrorCode`), never a silent partial rollback.

### 3.6 Do-not-touch: file format, floor, and enforcement point

Format = `types.DoNotTouch` (SPEC-TYPES §3.8), TOML, one file:

```toml
schema_version = 1

# absolute paths; a pattern ending in "/**" matches every path with that prefix
paths = [
  "/etc/shadow", "/etc/gshadow", "/etc/passwd", "/etc/sudoers", "/etc/sudoers.d/**",
  "/etc/ssh/**", "/etc/polkit-1/**", "/etc/trouble/do-not-touch.toml", "/etc/fstab",
  "/boot/**", "/usr/**", "/lib/**", "/lib64/**", "/sbin/**", "/bin/**",
  "/var/lib/trouble/**", "<state_root>/**", "<state_root>/backups/**",
]

# systemd units refused for service.* and unit-scoped proc.*
units = [
  "init.scope", "systemd-journald.service", "systemd-logind.service", "dbus.service",
  "polkit.service", "ssh.service", "sshd.service", "trouble.service", "trouble-escalate@.service",
  "systemd-oomd.service",
]

# scope tokens refused outright, whatever the grant
scopes = ["service:stop", "config:delete", "file:exec"]
```

Resolution order and the floor:

1. `internal/registry/defaults.go` holds the compiled-in floor — the list above with `paths`/`units`/
   `scopes` as shown (`<state_root>` resolved at boot). The floor is **always** applied.
2. `registry.do_not_touch_file` (default `/etc/trouble/do-not-touch.toml` when uid 0, otherwise
   `$XDG_CONFIG_HOME/trouble/do-not-touch.toml`) is merged **additively**. A file that sets
   `mandatory = false`, or that carries a `remove = [...]` key, is refused: the keys are rejected,
   the floor stays, `TROUBLE-REGISTRY-006` (`reason=do_not_touch_weaken_refused`) is recorded, and the
   daemon continues with maximum enforcement. Configuration can widen the deny set, never narrow it.
3. `registry.do_not_touch_paths_extra`, `registry.do_not_touch_units_extra`,
   `registry.do_not_touch_scopes_extra` (default `[]`) append at boot; every value is logged into the
   `kind=config` resolved-config record by SPEC-12.

Matching: the candidate is made absolute, cleaned, and `filepath.EvalSymlinks`-resolved; a path whose
resolution fails is matched in its cleaned form. Patterns use Go `path.Match` semantics with the single
extension that a trailing `/**` matches the prefix itself and everything under it. Units are compared
after systemd normalization (`foo` → `foo.service`, template instances compared by template name).

**Enforcement point: step a3 of authorize — before validate, before `Check`, before any target is
touched.** A hit is `TROUBLE-REGISTRY-007` with class **`policy_refused`** (never `transient`, never
`permanent`, never retried), `Stage[0].OK=false`, `payload.reason` ∈
`do_not_touch_path | do_not_touch_unit | do_not_touch_scope`. The ladder escalates instead of advancing a
rung. Plays are data, so they are also checked *statically at load*: a task whose literal `args` name a
protected path or unit makes `LoadPlay` fail with `TROUBLE-REGISTRY-007` — a protected play never reaches
the runner.

### 3.7 Plays as data

```toml
schema_version = 1
name        = "service-reload-on-pool-exhaustion"
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
```

Field mapping is 1:1 onto `types.Play`/`types.PlayTask` (SPEC-TYPES §3.8) — no invented keys,
`additionalProperties = false`. `Play.CheckMode` is **derived**: true iff every task's tool has
`CheckMode == true`. Absent fields default to `max_runs = 2`, `retries = 0`, `on_fail = "abort"`,
`source = "module-default"`, `when = ""`, `register = ""`.

**`when:` — the condition language of SPEC-03 §3.4, verbatim.** No second dialect, no extra operators, no
new coercions: the play's expression string is parsed and evaluated by the same package and the same
covered operators (`== != >= <= > < ~ in`), with the same numeric/string/bool coercion table. The play
namespace adds exactly these names, and nothing else:

| Name | Bound to |
|---|---|
| `inc`, `sig`, `severity`, `rule`, `unit` | incident context handed in by SPEC-05 |
| `evidence.*` | fields of `Evidence` (SPEC-TYPES §3.7) |
| `<register>` | the registered task's `Result.Output` map (forward-only: a task sees only earlier tasks) |

An expression must evaluate to `bool`; a literal that cannot is a load-time `TROUBLE-REGISTRY-017`, a
run-time non-bool is a task failure with the same code. `when = ""` always runs.

| Field | Semantics |
|---|---|
| `register` | binds `Result.Output` under the name for tasks with a higher index (forward-only); must be a unique identifier per play (duplicate or non-identifier → `TROUBLE-REGISTRY-016`) |
| `retries` | `0..3`; retries happen **only** on `transient`; backoff 1s, 2s, 4s (cap 8s); attempts = `retries+1`. `permanent` and `policy_refused` are never retried |
| `on_fail` | `abort` = stop, `outcome="failed"`, escalate · `continue` = record and proceed (a subsequent `when` may test the register's `ok`) · `rollback` = stop and replay the accumulated `RollbackHint`s in reverse order, stopping at the first PONR with `TROUBLE-REGISTRY-015` |

**Where plays live on disk** (versioned path, all config-driven):

| Origin | Path | Config key |
|---|---|---|
| shipped defaults | embedded `plays/<name>@<version>.toml` (`go:embed`, read-only) | — |
| local / agent-drafted | `<state_root>/plays/<name>@<version>.toml` | `registry.plays_dir` (default `<state_root>/plays`) |
| pulled skill | `<state_root>/skills-local/<skill>/plays/<name>@<version>.toml` | `registry.skills_dir` (default `<state_root>/skills-local`) |

Resolution: skill-local → local → embedded. Duplicate `(name, version)` resolves to the highest `version`
with a conflict record (`Play.Source` set from where it was loaded: `module-default`, `agent-draft`, or
`skill:<sk_id>`). A play whose static targets hit do-not-touch, whose `when:` fails to parse, or whose
tools are unknown never loads.

### 3.8 Shipped modules — descriptors

| Module | ver | Scopes | Idem | check_mode | mutating | timeout_s | capability default |
|---|---|---|---|---|---|---|---|
| `config.get` | 1 | `config:read` | pure | true | false | 5 | always |
| `config.set` | 1 | `config:write` | convergent | true | true | 10 | always |
| `config.list` | 1 | `config:read` | pure | true | false | 5 | always |
| `service.status` | 1 | `service:read` | pure | true | false | 5 | D-Bus read |
| `service.reload` | 1 | `service:write` | once | true | true | 20 | polkit |
| `service.restart` | 1 | `service:write` | once | true | true | 25 | polkit |
| `file.read` | 1 | `file:read` | pure | true | false | 5 | always |
| `file.patch` | 1 | `file:write` | convergent | true | true | 10 | allow-root |
| `proc.top` | 1 | `proc:read` | pure | true | false | 5 | always |
| `proc.connections` | 1 | `proc:read` | pure | true | false | 10 | always |
| `flow.file_issue` | 1 | `flow:issue` | convergent | true | true | 30 | driver health |
| `flow.create_task` | 1 | `flow:task` | convergent | true | true | 30 | driver health |
| `flow.comment` | 1 | `flow:comment` | once | true | true | 30 | driver health |

The PRD's `service.stop`, `service.start`, `config.unset`, `proc.signal` and custom script tools are
**not** in the v0.1 module set: the mutating surface of v0.1 is exactly the thirteen modules above. A tool
name outside this table is `TROUBLE-REGISTRY-001`.

### 3.9 Shipped modules — args schemas and behaviour

Args schemas (generated, draft 2020-12, `additionalProperties:false`; `required` shown):

```json
{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://trouble.local/schema/registry/config.set@1","type":"object","additionalProperties":false,"required":["path","key","value"],"properties":{"path":{"type":"string"},"key":{"type":"string","pattern":"^[A-Za-z0-9_.\\-]{1,128}$"},"value":{},"format":{"enum":["toml","yaml","json","env","ini"]},"create":{"type":"boolean","default":false}}}
{"type":"object","additionalProperties":false,"required":["path","key"],"properties":{"path":{"type":"string"},"key":{"type":"string"},"default":{}}}                                             # config.get
{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"},"prefix":{"type":"string","default":""}}}                                    # config.list
{"type":"object","additionalProperties":false,"required":["unit"],"properties":{"unit":{"type":"string","pattern":"^[A-Za-z0-9@._:\\-]{1,255}$"},"scope":{"enum":["system","user"],"default":"system"}}}  # service.status
{"type":"object","additionalProperties":false,"required":["unit"],"properties":{"unit":{"type":"string"},"scope":{"enum":["system","user"],"default":"system"}}}                        # service.reload
{"type":"object","additionalProperties":false,"required":["unit"],"properties":{"unit":{"type":"string"},"scope":{"enum":["system","user"],"default":"system"},"mode":{"enum":["replace","fail"],"default":"replace"}}}  # service.restart
{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"},"max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"default":262144},"encoding":{"enum":["utf-8","raw"],"default":"utf-8"},"offset":{"type":"integer","minimum":0,"default":0}}}  # file.read
{"type":"object","additionalProperties":false,"required":["path"],"oneOf":[{"required":["patch"]},{"required":["restore_from"]}],"properties":{"path":{"type":"string"},"patch":{"type":"string","maxLength":1048576},"restore_from":{"type":"string"},"strip":{"type":"integer","minimum":0,"maximum":8,"default":0},"backup":{"type":"boolean","default":true}}}  # file.patch
{"type":"object","additionalProperties":false,"properties":{"sort":{"enum":["cpu","rss","io"],"default":"cpu"},"n":{"type":"integer","minimum":1,"maximum":100,"default":15},"unit":{"type":"string"}}}  # proc.top
{"type":"object","additionalProperties":false,"properties":{"unit":{"type":"string"},"pid":{"type":"integer","minimum":1},"proto":{"enum":["tcp","udp","all"],"default":"all"},"states":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer","minimum":1,"maximum":2000,"default":200}}}  # proc.connections
{"type":"object","additionalProperties":false,"required":["sig","title","body"],"properties":{"sig":{"type":"string"},"title":{"type":"string","maxLength":200},"body":{"type":"string","maxLength":65536},"labels":{"type":"array","items":{"type":"string"}},"severity":{"enum":["critical","high","medium","low","info"]},"driver":{"enum":["github","duckbrain"]}}}  # flow.file_issue
{"type":"object","additionalProperties":false,"required":["sig","title"],"properties":{"sig":{"type":"string"},"title":{"type":"string","maxLength":200},"severity":{"enum":["critical","high","medium","low","info"]},"priority":{"enum":["P0","P1","P2","P3"]},"repo":{"type":"string"},"issue_refs":{"type":"array","items":{"type":"string"}}}}  # flow.create_task
{"type":"object","additionalProperties":false,"required":["sig","body"],"properties":{"sig":{"type":"string"},"body":{"type":"string","maxLength":65536},"ref":{"type":"string"}}}  # flow.comment
```

Behaviour, check output, apply, verify, errors, do-not-touch and idempotency per module:

**`config.{get,set,list}`** — reads and span-writes a config file (`toml|yaml|json|env|ini`, inferred from
the extension when `format` is absent). `Check` on `config.set` parses the file, locates the key's value
span through `configSpan`, and returns `Diff{Entries:[{path:"<file>#<key>", before:<old>, after:<new>}]}`;
a key already at the target value returns `Diff{Empty:true}`. `Apply` replaces **only** the value bytes and
atomically renames (`temp in the same directory → fsync → rename`), preserving comments, key order and
formatting; a format that cannot be span-edited safely (duplicate INI keys, non-UTF-8 bytes) is a permanent
failure (`TROUBLE-REGISTRY-011`). `create=true` appends the key; without it a missing key is
`TROUBLE-REGISTRY-011`. Verify = `recheck`: re-read, parse, compare against `Result.Output.after`; mismatch
is `TROUBLE-REGISTRY-005`. Errors: 002/018 (validate), 006 (`file.allow_roots` miss or capability), 007
(do-not-touch path), 003/011 (Check), 004/010/011 (Apply), 005 (verify), 009 (timeout). Do-not-touch:
`path` is a protected target — `/etc/trouble/do-not-touch.toml`, `<state_root>/**`, `/etc/ssh/**` are
refused as paths before validation. Idempotency: convergent — the second `Apply` finds the value equal,
returns `Changed=false` with an empty diff; rollback is `config.set` with the `before` value.

**`service.{status,reload,restart}`** — D-Bus `org.freedesktop.systemd1` on the configured manager
(`scope=system` → the system bus; `scope=user` → the configured user manager, resolved from
`XDG_RUNTIME_DIR/bus` and configured per uid, because this fleet's units are user units and a system-only
watcher reports an all-green lie). Unit names are escaped into object paths (`-` → `_2d`).
`service.status` is `pure`: `Output {active_state, sub_state, main_pid, exec_main_start_ts, n_restarts}`.
`service.reload` `Check` returns the predicate
`Diff{Empty: <ActiveState != "active">, Entries:[{path:"unit:<unit>#reload", before:"<sub_state>", after:"reloading"}]}`
— an empty diff makes the runner skip the task as `ok` (nothing to reload). `Apply` calls
`ReloadUnit(unit, replace)` and, on `AccessDenied`, maps polkit's refusal to
`TROUBLE-REGISTRY-006` with `reason=polkit_missing_policy` (**distinct from `transient` and `permanent`** —
never retried, always escalated with the `.rules` install command in the escalation body). Verify = `probe`:
`GetUnit` → `ActiveState=="active"` and `SubState` not in `{failed, auto-restart}`, read back within 2s;
for `service.restart` the probe additionally requires a **changed `MainPID`** within `restart_probe_s`
(default 10s), which is what makes "the restart actually happened" evidenced rather than assumed.
Rollback: unsupported for both — PONR, so an explicit module-name grant is mandatory (§3.5). Errors:
002/018, 001, 006 (polkit/capability), 007 (protected unit), 008, 015 (PONR without a grant), 003, 004/010,
005, 009, 011 (unit not found / not loadable). Do-not-touch: the `units` list and `registry.service_units`
(the allowlist for mutating service calls; empty means "no unit may be reloaded or restarted") are both
checked at a3. Idempotency: `once` with the replay key — `ReloadUnit` twice with the same `IdemKey` runs
once.

**`file.{read,patch}`** — `file.read` is `pure` and bounded by DAC plus do-not-touch; `encoding=utf-8`
with invalid UTF-8 is `TROUBLE-REGISTRY-011` (and `encoding=raw` returns
`Output{bytes_base64, bytes, sha256}`). `file.patch` applies a unified diff (or restores a backup when
`restore_from` is given). `Check` dry-runs the hunks in memory and returns
`Diff{Entries:[{path:"<file>:<line_no>", before:"<old line>", after:"<new line>"}]}` per hunk; if every
hunk's after-side already matches the file, `Diff{Empty:true}` — that is the idempotency detector and it
never writes. `Apply` writes a backup copy into `<state_root>/backups/` (`<backup_id>-<basename>`), applies
all-or-nothing through a temp file + rename, and returns
`RollbackHint{Supported:true, Module:"file.patch", Args:{path, restore_from:"<backup_id>"}}`. A partially
applied patch (some hunks match the after-side, some the before-side) is `TROUBLE-REGISTRY-011`, never a
guess. Path safety: the resolved path must be inside `file.allow_roots` (default `[]` — nothing is
patchable until configured) **and** inside the containing root after `EvalSymlinks`, else
`TROUBLE-REGISTRY-006` (`reason=allow_root_escape`); a symlink is never written through. Verify = `recheck`:
re-read, sha256 equals `Result.Output.sha256`, every hunk's after-side present; else
`TROUBLE-REGISTRY-005`. Errors: 002/018, 006/007, 003/011, 004/010/011, 005, 009. Idempotency: convergent
(already-applied detection, `Changed=false`); rollback restores the backup.

**`proc.{top,connections}`** — read-only, `proc.root` (default `/proc`) and `proc.top_n` (default 15).
`proc.top` returns `Output{procs:[{pid,comm,cpu_pct,rss_bytes,unit}]}` sorted by `sort`; `proc.connections`
parses `/proc/net/{tcp,tcp6,udp,udp6}` for the socket table and attributes inodes to pids by scanning
`/proc/<pid>/fd` under `proc.fd_scan_max` (default 20000) — a pid whose fds the daemon's uid cannot read
is counted in `Output{counts:{fd_unreadable:N}}` and never fails the call. `Check` returns
`Diff{Empty:true}` with `Summary` = the one-line observation (`"established=412 unit=payment-worker.service"`);
`Apply` is a read (`Changed=false`). Verify = `recheck` (the probe resolves the same unit and re-reads).
Errors: 002/018, 007 (unit-scoped read of a protected unit), 003, 009. Idempotency: pure.

**`flow.{file_issue,create_task,comment}`** — thin wrappers over the SPEC-08 flow subsystem and the
SPEC-09 four-method driver contract; the registry decodes and validates, the subsystem performs. All three
are sig-keyed: `flow.file_issue` → `EnsureBySig` (an existing open issue for the sig is a comment +
counter, never a second issue), `flow.create_task` → board-jsonl append or task-router dispatch with an
existing open row for the sig receiving a comment/cross-ref instead of a duplicate, `flow.comment` →
`Comment`. `Check` returns the diff of the *intent* against the live state
(`{path:"issue:<sig>", before:"absent|open#42", after:"open#42"}`,
`{path:"board:<sig>", before:"absent", after:"todo"}`, `{path:"comment:<sig>", before:"3", after:"4"}`), so
a re-run against an existing row is `Diff{Empty:true}`. Verify = `probe`/`recheck`: re-`EnsureBySig`
returns `Created=false` with the same `external_id`; a board row is re-read and post-append validated
(SPEC-08). Upstream driver failures are classified (`transient` → `TROUBLE-REGISTRY-004`/`010`,
`permanent` → `TROUBLE-REGISTRY-011`) and the driver's own code rides in
`Result.Output.upstream_code` and in the play-run record, never as this area's error code. Rollback:
unsupported (append-only evidence: the corrective action is a comment or a close, not a delete) → PONR,
explicit grant required. Errors: 002/018, 006/007 (scope deny), 008, 015, 003, 004/010, 005, 009, 011.
Idempotency: `convergent` for `file_issue`/`create_task` (sig-keyed), `once` with a body-hash replay key
for `comment`.

### 3.10 The polkit artifact

Shipped at `contrib/polkit/49-trouble.rules` (mode 0644, root:root), installed to
`/etc/polkit-1/rules.d/49-trouble.rules`; the `49-` prefix makes polkitd evaluate it before
`50-default.rules`, so the grant wins.

```js
// 49-trouble.rules — the ONLY privilege escalation trouble installs.
// The unit allowlist mirrors registry.service_units; polkit is the second, independent gate.
polkit.addRule(function(action, subject) {
    if (action.id !== "org.freedesktop.systemd1.manage-units") return polkit.Result.NOT_HANDLED;
    var verb = String(action.lookup("verb"));
    if (verb !== "reload" && verb !== "restart") return polkit.Result.NOT_HANDLED;
    if (subject.user !== "trouble") return polkit.Result.NOT_HANDLED;
    if (!subject.local) return polkit.Result.NOT_HANDLED;
    var unit = String(action.lookup("unit"));
    var allowed = ["payment-worker.service"]; /* generated from registry.service_units */
    if (allowed.indexOf(unit) === -1) return polkit.Result.NOT_HANDLED;
    return polkit.Result.YES;
});
```

- **Actions covered:** `org.freedesktop.systemd1.manage-units`, verbs `reload` and `restart` only, for the
  configured daemon user, on the configured unit allowlist, from a local session.
  `manage-unit-files` is **not** covered — trouble never writes unit files, and the only unit file it
  ever installs is its own (SPEC-12, `trouble install`).
- **Why `auth_admin` on this class of host:** measured on the quorum's reference hosts,
  `org.freedesktop.systemd1.manage-units` resolves to `auth_admin`/`auth_admin_keep` from the distro
  policy, so a non-root `trouble` daemon cannot reload or restart any unit without an installed rule.
  That makes `service.*` an **install-time contract, not a runtime detail**: the module works only when the
  `.rules` file is present and its allowlist matches the configured units.
- **Install step:** `trouble install --polkit` writes the artifact from the embedded copy, regenerating the
  `allowed` array from `registry.service_units`, then `systemctl reload polkit` (or the distro's
  `polkitd` reload) and re-runs the probe. Without root it prints the exact `install -m 0644` command and
  exits non-zero; the daemon still starts.
- **Boot probe:** `Registry.PolicyCheck` calls
  `org.freedesktop.PolicyKit1.Authority.CheckAuthorization` on the system bus with the action id
  `org.freedesktop.systemd1.manage-units`, details `{unit, verb:"reload"}`, `flags=0` (no interactive
  prompt) — a genuinely non-mutating probe. `authorized=false` (or `PolicyKit1` absent) marks
  `service:write` as `capability=policy_refused` for this host; `service.reload`/`service.restart` then
  refuse at a4 with `TROUBLE-REGISTRY-006` (`reason=polkit_missing_policy`), `trouble tool list` shows the
  capability, and the dashboard surfaces it. Every probe emits one reserved audit record (§4.4).
- **Distinct class:** the refusal is `policy_refused` — not `transient` (retrying cannot fix a missing
  policy) and not `permanent` (nothing about the module is broken). The ladder escalates immediately,
  never consumes a rung retry, and the escalation body carries the install command and the missing unit.

### 3.11 No shell, no arbitrary command — without exception

**No module shells out and no module executes an arbitrary command. There is no exception.** Concretely:
`internal/registry/**` contains zero imports of `os/exec` and zero calls to `syscall.Exec`,
`syscall.ForkExec` or `os.StartProcess`, enforced in CI by `TestNoExecInRegistry` (a `go/parser` import
scan over the package tree, not a text grep) and by `TestNoArbitraryCommandModule` (no registered module
may accept a `command`, `cmd`, `argv`, `shell` or `script` args key — a schema test over every generated
descriptor). The registry reaches the outside world through exactly four typed channels: direct file IO
inside an allow root (config/file), `/proc` reads (proc), the systemd D-Bus API (service, mediated by
polkit), and the in-process flow/issue subsystem (flow, which owns its own transport per SPEC-08). Nothing
a rule, a skill, a play or an agent can express turns into a command line: a play task names a registered
module and a JSON args object, and a module that is not in §3.8 cannot be named at all
(`TROUBLE-REGISTRY-001`). This is non-negotiable #3 of the brief and it holds by construction rather than
by policy: there is no code path in the package that can build a shell invocation.

## 4. Wiring

### 4.1 Producer / consumer

| Producer | Consumes | Emits |
|---|---|---|
| `internal/registry` | `Descriptor`, `ToolCall`, `Play`, `DoNotTouch`, `ToolCallRequest`, `RegistryDeps` (SPEC-01 append, SPEC-02 scrub, SPEC-05 gates, SPEC-08/09 outlets) | `tool_call`, `play_run` — the only kinds this area is allowed to emit (SPEC-INDEX §3.4) |

Dependency direction: `internal/registry` imports `internal/types`, `internal/ledger` (writer) and
`internal/flow`/`internal/issues` **only** through the closures in `RegistryDeps`; it never imports
`internal/ladder` (the ladder imports the registry, not the other way) and nothing imports back into it.

### 4.2 Call sequence with records

```
rule/agent/CLI → Registry.Call(req)
  a1..a7 authorize ──refusal──► [tool_call outcome record] ─► ladder escalates, no retry
  validate ─────────refusal──► [tool_call outcome record] ─► fail (permanent) or escalate
  Check (dry-run) ──────────► apply mode: [tool_call INTENT record, stages 0..2]
                              check_mode: [tool_call outcome record, 4 stages] ─► caller
  Apply ────────────────────► mutation happens (temp+rename / D-Bus / driver call)
  Verify ───────────────────► [tool_call outcome record carries Verify]
  audit ────────────────────► append [tool_call outcome record]; under a play, at run end append [play_run record]
```

The intent record is what makes a crash mid-apply auditable: a daemon that dies between the intent append
and the outcome append leaves the intent line plus a `parked` `play_run` record on resume (SPEC-05), and
the resumed run replays the same `IdemKey` (§3.4) — never a blind re-apply. The ledger's group-commit
`fsync_window_ms` (default 200, SPEC-01) is the crash-loss window: an intent record written inside the
open window can be lost, and that loss is stated here because it is the honest bound on this guarantee.

### 4.3 Configuration keys (all with defaults; no fleet value is hardcoded)

| Key | Default | Effect |
|---|---|---|
| `registry.plays_dir` | `<state_root>/plays` | local / agent-drafted plays |
| `registry.skills_dir` | `<state_root>/skills-local` | pulled-skill plays |
| `registry.do_not_touch_file` | `/etc/trouble/do-not-touch.toml` (uid 0) else `$XDG_CONFIG_HOME/trouble/do-not-touch.toml` | additive deny set |
| `registry.do_not_touch_paths_extra` / `_units_extra` / `_scopes_extra` | `[]` | additive deny set |
| `registry.service_units` | `[]` | the mutable-unit allowlist AND the polkit allowlist (empty = `service.*` mutation refused) |
| `registry.dbus_address` | `""` (systemd discovery) | systemd bus address |
| `registry.systemd_scope_user` | `""` | the uid whose user manager is watched/commanded |
| `registry.probe_timeout` | `2s` | polkit probe budget |
| `registry.restart_probe_s` | `10` | `service.restart` MainPID-change window |
| `registry.max_timeout_s` | `60` | hard cap over every descriptor timeout |
| `registry.retry_max` | `3` | hard cap over a task's `retries` |
| `registry.idem_window` | `4096` | replay-key entries held per generation |
| `file.allow_roots` | `[]` | writable roots; empty = nothing patchable |
| `file.backup_dir` | `<state_root>/backups` | patch backups |
| `file.keep_backups` | `20` | backups retained per path |
| `proc.root` / `proc.top_n` / `proc.fd_scan_max` | `/proc` / `15` / `20000` | proc module bounds |

### 4.4 The reserved capability-probe audit record

`Registry.PolicyCheck` emits one `kind=tool_call` record with `Module="registry.capability-probe"`,
`Mode="check_mode"`, `Stage=[authorize ok, validate ok, dry_run ok]` and
`Result.Output={polkit_authorized, polkit_rules_file, rules_file_present, verbs_allowed[], units_allowed[]}`.
The name is reserved and non-dispatchable: `Call` on it returns `TROUBLE-REGISTRY-001`. It exists so that
the polkit state lands in the append-only ledger at boot (this area's only two record kinds are
`tool_call` and `play_run`, so the probe is recorded as a tool call rather than a new kind).

## 5. Errors

Classes are `transient | permanent | policy_refused`. `error_code` on the `tool_call` record is always a
code from this table, and `payload.error_class` carries the class, because the ladder's retry policy keys
on **class, never on the code**.

| Code | Class | Trigger | Stage | Ladder behaviour |
|---|---|---|---|---|
| TROUBLE-REGISTRY-001 | permanent | module not found | authorize a1 | fail, escalate |
| TROUBLE-REGISTRY-002 | permanent | args violate the module schema | validate | fail, escalate |
| TROUBLE-REGISTRY-003 | transient | `Check` failed | dry-run | retry per play `retries`, then next rung |
| TROUBLE-REGISTRY-004 | transient | `Apply` failed | apply | retry per play `retries`, then rollback/escalate |
| TROUBLE-REGISTRY-005 | permanent | `Verify` not ok after `Apply` | verify | rollback when invertible, else escalate |
| TROUBLE-REGISTRY-006 | policy_refused | polkit/do-not-touch-adjacent policy, capability absent, kill-switch, skill allowlist — `payload.reason` names which | authorize a2/a4/a5 | never retried, escalate |
| TROUBLE-REGISTRY-007 | policy_refused | target path/unit/scope on the do-not-touch list | authorize a3 | never retried, escalate |
| TROUBLE-REGISTRY-008 | permanent | required scope not granted by the rule or the autonomy gate | authorize a6/a7 | fail, escalate |
| TROUBLE-REGISTRY-009 | transient | call exceeded the effective timeout | any | retry once if the mutation never started; never after apply |
| TROUBLE-REGISTRY-010 | transient | module returned a transient error | apply | retry per play `retries` |
| TROUBLE-REGISTRY-011 | permanent | module returned a permanent error | dry-run/apply | fail, escalate |
| TROUBLE-REGISTRY-012 | permanent | idempotency violation (a second `Apply` changed state again) | apply | fail, escalate; conformance test failure if in CI |
| TROUBLE-REGISTRY-013 | permanent | conformance harness failure for a shipped module | CI / boot | CI-fatal; at boot, the module is not registered |
| TROUBLE-REGISTRY-014 | permanent | descriptor invalid (missing schema/scope/`check_mode`), or a regenerated schema differs from the committed one | register/boot | daemon exits |
| TROUBLE-REGISTRY-015 | permanent | rollback unsupported for a non-invertible (PONR) call without an explicit module-name grant | authorize a7 / rollback | never retried, escalate for a human or an agent |
| TROUBLE-REGISTRY-016 | permanent | play or do-not-touch TOML schema invalid (unknown key, bad type, duplicate `register`) | load | play/file refused; the floor is kept |
| TROUBLE-REGISTRY-017 | permanent | play `when:` expression invalid | load / run | play refused at load, or task fails at run |
| TROUBLE-REGISTRY-018 | permanent | tool args failed JSON decoding | validate | fail, escalate |

Class mapping from a module error to a code: `errors.Is(err, types.ErrPolicyRefused)` → class
`policy_refused`; `ErrTransient` → `transient`; `ErrPermanent` → `permanent`; **any unmatched error maps to
`permanent`** (an unknown failure is never retried). Modules never mint codes: they return one of the three
sentinels, and the registry attaches the code by stage — `003`/`011` for `Check`, `004`/`010`/`011` for
`Apply`, `005` for `Verify`.

Two class facts this spec pins against the catalog cells: `TROUBLE-REGISTRY-007` is **`policy_refused`**,
not `permanent` — a do-not-touch hit is a policy decision the operator can change, so it must never enter
the permanent-error branch of the ladder (reported as a reclassification in the run report).
`payload.error_code` also mirrors the upstream code of a subsystem failure (e.g.
`payload.upstream_code` = the SPEC-01 append-failure code for an audit-append failure, `payload.upstream_code` for a
flow-driver failure) so SPEC-INDEX §5.3's ledger-mirror rule holds without this area minting a foreign
code.

## 6. Edge cases

1. **Daemon restart mid-apply.** The intent record exists; the play is parked. On resume the run replays
   the same `IdemKey`; `convergent` modules return `Diff.Empty` from `Check` and are skipped,
   `once` modules hit the replay guard (§3.4). An applied mutating call is never repeated.
2. **Kill-switch mid-play.** The in-flight call finishes (its apply+verify complete); the next call is
   refused at a2 with `TROUBLE-REGISTRY-006` (`reason=kill_switch`), the run ends `parked`, and SPEC-05
   records its kill-switch checkpoint refusal (SPEC-05 §5) at that checkpoint. A kill-switch never truncates a half-applied change.
3. **Verify fails with a rollback available.** Rollback runs `RollbackHint` (invertible modules only);
   the play-run outcome is `rolled_back` and the incident escalates.
4. **Verify fails with no rollback (PONR).** `TROUBLE-REGISTRY-015` is recorded, the incident escalates
   with the applied-diff attached, and no automatic second attempt is made.
5. **The `.rules` artifact is absent, stale or names another user.** The D-Bus call returns
   `AccessDenied` → `TROUBLE-REGISTRY-006` (`reason=polkit_missing_policy`). The escalation carries the
   install command and the unit; `service.*` stays in `check_mode` capability until the probe passes.
6. **A config edit tries to narrow do-not-touch.** The weakening keys are rejected, the compiled-in floor
   stands, `TROUBLE-REGISTRY-006` (`reason=do_not_touch_weaken_refused`) is recorded, and the daemon
   continues — enforcement never degrades because of a config error.
7. **Symlink and path-escape attempts.** Every candidate is absolute → cleaned → `EvalSymlinks`-resolved
   before matching and before the allow-root containment check; a symlink whose target escapes the root is
   `TROUBLE-REGISTRY-006` (`reason=allow_root_escape`), and a dangling symlink is matched in its cleaned
   form so it cannot be used to dodge the floor.
8. **Two incidents target the same file or unit.** A per-target mutex (canonical path / unit name) is held
   for the call with a 5s acquire budget; contention expiry is `TROUBLE-REGISTRY-009` (transient). This is
   the same serialization lesson as the repo-level `config.lock` contention class, applied to targets.
9. **A schema violation must be inert.** `validate` runs before any IO beyond the target extraction; the
   test asserts the target's mtime and sha256 are unchanged after a rejected call.
10. **Timeout after apply, before verify.** The outcome record shows `Stage[3].OK=true`, `Stage[4]`
    (verify) present with `OK=false` and `Detail="timeout"`, and `Stage[5]` (audit) appended; the ladder
    treats it as verify-failed and applies rollback when available — never a blind retry, because the
    mutation may have landed.
11. **Ledger append failure at audit.** The mutation happened but is unlogged. `Stage[5].OK=false`,
    `payload.audit_failed=true`, `payload.upstream_code` = the SPEC-01 append-failure code; the ladder raises an
    applied-unlogged incident and the play does not advance. `Call` returns the non-nil error here
    (the one case where nothing was appended).
12. **Non-UTF-8 or binary targets.** `config.set`, `file.patch` and `file.read` with `encoding=utf-8`
    refuse with `TROUBLE-REGISTRY-011`; `file.read` with `encoding=raw` returns base64 so that a
    diagnosis can still read a binary artifact.
13. **An agent-drafted play with no grants.** Every mutating task is refused at a6/a7 (`008`/`015`), the
    play-run outcome is `check_only`, and the drafted play is still recorded — the draft is evidence even
    when nothing may run.
14. **`registry.capability-probe` is called as a tool.** `TROUBLE-REGISTRY-001`; the reserved name exists
    only as an audit subject.
15. **(v1.0 hand-off)** The plays library beyond the five shipped defaults, and a curated
    `plays/` release channel, are a data-packaging seam: v0.1 ships the defaults in §3.7 embedded in the
    binary plus whatever a skill carries in `allowed_modules`; the loader (§3.7) is the interface the
    v1.0 library slots into, and no v0.1 section claims a library beyond these five.
16. **Time.** Stage budgets use the monotonic clock (`time.Since`); every persisted timestamp is RFC3339
    UTC with milliseconds from the wall clock (SPEC-INDEX §6.5). Effective timeout =
    `min(Descriptor.TimeoutS, DeadlineS, registry.max_timeout_s)`.

## 7. Testing

Test files and what each proves (numeric thresholds are pass thresholds, not goals):

| File | Cases | Threshold |
|---|---|---|
| `internal/registry/authorize_test.go` | the a1–a7 ladder, one test per step, including `deny by default` with an empty grant set; shadow forces `check_mode`; `scope:` wildcard cannot authorize a PONR module | 100% of the seven steps have a dedicated refusal case; zero module invocations on refusal |
| `internal/registry/donottouch_test.go` | floor + additive merge, weakening refusal, symlink escape, `/**` prefix match, unit normalization, static play-target check | 24 fixture paths/units; every refusal `policy_refused`; the floor never shrinks |
| `internal/registry/modules_config_test.go`, `modules_service_test.go`, `modules_file_test.go`, `modules_proc_test.go`, `modules_flow_test.go` | per-module units: check-diff shape, apply semantics, verify, every error code in the module's row | ≥ 3 fixtures per module; ≥ 60 fixtures total |
| `internal/registry/play_test.go` | TOML round-trip, unknown key rejection, `when:` parse + evaluation, `register` scoping/forward-only, `retries` bounds, `on_fail` × 3, duplicate `(name,version)` resolution | 100% branch coverage on the three `on_fail` paths |
| `internal/registry/testkit_run_test.go` | `testkit.RunAll` over all 13 modules | full suite ≤ 20s wall, 0 flakes over 20 consecutive `-count=1 -race` runs |
| `internal/registry/testkit/harness_negative_test.go` | `TestHarnessRejectsNonIdempotent` (asserts `testkit.Run` **fails** `badmodule/nonidempotent` with `013`) and `TestHarnessRejectsNoCheckMode` (`Register` refuses `badmodule/nocheckmode` with `014`) | both must fail the module under test; a pass is the CI failure |
| `internal/registry/schema_test.go` | generator determinism, dialect + keyword closure, boot-time regen equality, `TestNoExecInRegistry`, `TestNoArbitraryCommandModule` | 100 iterations byte-identical; zero out-of-subset keywords; zero `os/exec` references |
| `internal/registry/registry_bench_test.go` | authorize+validate latency, `check_mode` call latency, allocations per call | authorize+validate p99 ≤ 2ms; `config.set`/`file.patch` `check_mode` p99 ≤ 25ms; ≤ 40 allocs/call; retained-set delta after 10k calls ≤ 2MB (TRBL-048: the post-GC live-heap delta — Sys measures the allocator's MADV_FREE arena state, not retention, and is held to a separate 16MB arena-blow-up ceiling) |

AC-derived tests:

- **AC-7** (`call_contract_test.go`): one `apply` call of `file.patch` against a fixture file asserts (a)
  `Stage` has exactly six entries in the order authorize→validate→dry-run→apply→verify→audit with
  `OK=true` and `MS >= 0`; (b) the ledger holds **one intent** record (stages 0–2) and **one outcome**
  record (stages 0–5) for the call; (c) one `play_run` record when run under a play; (d) in `shadow`
  autonomy the same call runs `check_mode` only — four stages, no apply stage, and the file's bytes and
  mtime are unchanged.
- **AC-23** (`conformance_test.go` + the two negative tests): the three obligations of §2.4 with the
  double-apply assertion `ok/ok` + exactly one mutation, the check-vs-apply equivalence
  `normalize(d0.Entries) == normalize(r.Applied)`, and the schema-violation rejection with an inert
  target; plus the deliberate non-idempotent module failing CI. The AC passes only when
  `make conformance` is green and `TestHarnessRejectsNonIdempotent` is green.

Regression numbers carried as assertions: 13 registered modules, 18 registry error codes (each with a
dedicated test), 5 shipped default plays, 1 `.rules` artifact, 0 `os/exec` references, and the conformance
suite at ≤ 20s.

## 8. hilo impact

Packages and files created (greenfield repo `~/trouble`; no fleet repository is touched):

| Path | Package | Fan-out (imports) | Fan-in |
|---|---|---|---|
| `internal/registry/registry.go` | registry | types, ledger | ladder, cmd, dashboard (read-only via `List`) |
| `internal/registry/authorize.go` | registry | types | — |
| `internal/registry/donottouch.go` | registry | types | — |
| `internal/registry/defaults.go` | registry | — | (floor of the do-not-touch set) |
| `internal/registry/play.go` | registry | types, SPEC-03 condition engine (read-only use), ledger | skills (loads `play_ref`) |
| `internal/registry/schemagen/` | registry/schemagen | stdlib `reflect`, `encoding/json` | registry (build + boot) |
| `internal/registry/validate/` | registry/validate | stdlib only | registry (validate stage) |
| `internal/registry/modules_*.go` | registry | types, godbus (service), stdlib | — |
| `internal/registry/schema/<module>@<version>.json` | — | generated data | committed, CI-diffed |
| `internal/registry/testkit/` | registry/testkit | types, godbus | CI only; never linked into `troubled` |
| `internal/registry/testkit/badmodule/` | registry/testkit/badmodule | types | CI only (the negative fixtures) |
| `contrib/polkit/49-trouble.rules` | — | — | `trouble install --polkit` |
| `plays/*.toml` (5 shipped defaults) | — | embedded data | registry loader |

Blast radius: `internal/registry` is the second-highest fan-in node after `internal/types` in the action
path — the ladder, the CLI, the skills loop and the dashboard all read its descriptors, and every mutation
in the system passes through `Call`. Its own fan-out is narrow by design: `internal/types`, the
`internal/ledger` writer, and the `RegistryDeps` closures that reach SPEC-08/SPEC-09 without an import
cycle. Changes to the frozen `types.Module` interface are blast-radius-maximal by definition and are
forbidden in v0.1; changes inside the package stay behind `Call`. No fleet repo, unit, board or
configuration is modified by this spec: `${STATE_ROOT}` is a default, not a fleet path, and the polkit
artifact is written only by an explicit `trouble install --polkit` on the host that runs the daemon.
