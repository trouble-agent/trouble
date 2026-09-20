# SPEC-11 — skills: artifact schema, local promote loop, pull distribution (trouble v0.1)

Spec: SPEC-11
Area prefix: TROUBLE-SKILLS
Package: internal/skills
Consumed types: Skill, SkillGuards, Provenance, SkillStats, SkillCandidate, Play, PlayTask, Descriptor, Incident, ResearchOutcome, Evidence, Record, Actor, Origin, Duration, SkillsConfig, SkillSigner, SkillsStatus, SkillRow
Local types: skillArtifact, canonicalProjection, localIndex, installRow, canaryRecord, holdEntry, matchResult, pullReport, authorizer, Library, LibraryConfig, LocalSkill, SkillStep, StepRunner
ACs: AC-24 (partial — see SPEC-INDEX §6.1), AC-25, AC-26
PRD: §06c, §12

## 1. Purpose

`internal/skills` turns a verified incident fix into a signed, immutable skill artifact (procedural memory),
distributes skills **down** into hosts over a pull-only release channel, and decides — on every recurrence —
whether a skill may be applied to a signature. It exists so that the second occurrence of a bug costs zero
tokens (ladder rung ②, SPEC-05 §3) while the first occurrence remains the only one that spends an agent run.

Design constraint: a pulled skill is **executable material**, therefore this spec is a trust boundary, not a
file-format spec. Four defects of the PRD v2.3 §06c example are fixed here and are the reason the schema is
stated formally rather than by example:

1. **`[stats]` in the artifact is a contradiction.** The example marks `applied/success/last_used` as "live,
   updated by the daemon" while the same file is git-distributed. N hosts writing one git file produce merge
   conflicts on every host and make review diffs meaningless. §3.5: `[stats]` never appears in the artifact;
   local stats live in a local file under the state root. A `stats` key or table is **rejected**, not ignored.
2. **The example is not valid TOML.** `[play]` followed by `- tool = "…"` lines is array-of-tables written in
   list syntax; TOML requires `[[play]]` tables with `key = value` lines, and the example also puts two keys on
   one line (`incidents = […]  research = […]`). A conforming parser rejects the PRD's own artifact, so the
   schema is pinned in §3.1 with valid TOML in every example.
3. **The example's sigs are not sigs.** `"journal:payment-worker:restart-loop"` is free text: source `journal`
   is not a `SigSource` (`journald` is) and the tail is not 16 hex chars. Free-text sigs would silently weaken
   dedup and skill matching forever. §3.3: **every** sig must parse as `<source>:<algo>v<norm>:<hex16>`.
4. **The trust model is absent.** In `autonomy=full` a pulled skill executes typed tool calls, i.e. skill
   distribution is fleet-wide code execution. §2/§4 pin: ed25519 signatures over a canonical byte form, a
   config-listed signer set, `allowed_modules` allowlists, canary-host-first rollout, a per-host approve
   policy, a `min_daemon_version` floor, and refusal **records** (never silent skips).

Out of scope by the frozen cut line (SPEC-INDEX §3.3): PR-creating **push** automation. Pull-only in v0.1.

## 2. Interface

```go
package skills // internal/skills — imports internal/types (+ internal/ledger for append, internal/registry for descriptor names)

// ── artifact: parse, validate, canonicalize, verify ───────────────────────────────────────────────
func LoadArtifact(tomlBytes, playBytes []byte) (Skill, error)                 // strict decode; TROUBLE-SKILLS-001
func CanonicalBytes(s Skill, playBytes []byte) ([]byte, error)                // §3.2 — the exact signed material
func Verify(s Skill, playBytes []byte, signers []SkillSigner) error            // TROUBLE-SKILLS-002 / -003

// ── gates (pure; the same functions run host-side and hub-side for the version matrix) ─────────────
func GateSignature(s Skill, signers []SkillSigner, requireSignature bool) error // -002 / -003
func GateFloor(s Skill, daemonVersion string) error                            // semver; TROUBLE-SKILLS-004
func GateModules(s Skill, play Play, registered []string) error                // TROUBLE-SKILLS-005

// ── distribution: pull only (v0.1) ────────────────────────────────────────────────────────────────
type Puller struct{ /* unexported: cfg, store, ledger, git path, clock */ }
func (p *Puller) Run(ctx context.Context)                                     // on-boot + interval + jitter
func (p *Puller) PullOnce(ctx context.Context) (pullReport, error)            // TROUBLE-SKILLS-010 / -011
func (p *Puller) Approve(name string, version int, actor Actor) error         // per-host approve policy (review)
func (p *Puller) OverrideCanary(name string, version int, actor Actor, reason string) error

// ── local promote loop ────────────────────────────────────────────────────────────────────────────
type Promoter struct{ /* unexported */ }
func (p *Promoter) Draft(inc Incident, res []ResearchOutcome, play Play) (SkillCandidate, error)      // -008
func (p *Promoter) Review(c SkillCandidate, actor Actor, decision, reason string) (SkillCandidate, error) // -009
func (p *Promoter) Promote(c SkillCandidate) (Skill, error)                   // signs with the local key; §3.4

// ── resolution at recurrences ─────────────────────────────────────────────────────────────────────
type Resolver struct{ /* unexported */ }
func (r *Resolver) Match(sig string) (matchResult, error)                     // exact string match; -006 / -007
func (r *Resolver) Hold(sig string, open []verifyWindow) (holdEntry, bool)    // §3.6 hold rule
func (r *Resolver) RecordRun(name string, version int, success bool, ev Evidence) error // -013 on failure paths

// ── authorize hook: the ONLY path from an artifact to a capability ────────────────────────────────
type authorizer struct{ /* unexported: resolver, registry descriptors */ }
func (a *authorizer) Authorize(ctx context.Context, source, module string, args map[string]any) error // -005

// ── read surfaces ─────────────────────────────────────────────────────────────────────────────────
func (s *Store) Stats(name string, version int) SkillStats                    // local file; never the artifact
func (s *Store) Status() SkillsStatus                                         // `trouble skills status --json`
```

CLI (the operator and approval surface in v0.1 — SPEC-10's route set is frozen and gains no skills route):

| Command | Behaviour |
|---|---|
| `trouble skills status [--json]` | emits `SkillsStatus` (§3.7): installed/pending/held/refused rows, pull state, signer set |
| `trouble skills list [--state S]` | one row per (name, version) with state and reason |
| `trouble skills approve <name>@<v>` | approves a `pending_review` artifact; ledger record, `actor.kind=human` |
| `trouble skills approve <name>@<v> --override-canary` | canary override (§4.4); requires a non-empty `--reason` |
| `trouble skills reject <name>@<v> --reason R` | writes TROUBLE-SKILLS-009; the candidate/version is not installed |
| `trouble skills refusals [--since T]` | refusal records (name, version, host, error_code, reason, count) |
| `trouble skills explain <name>@<v>` | every gate result in order (signature, floor, canary, approve, modules) |

The CLI never mutates an artifact file; it only moves an artifact between the `pending/` and `installed/`
directories (§3.5) and appends ledger records. `approve`/`reject`/`--override-canary` require a write-scope
dashboard token (SPEC-10 §4) because they are policy actions, and the actor id is the token label.

Configuration (TOML, `[skills]` in the daemon config; all defaults safe → `enabled=false` means the daemon
pulls nothing and no skill is ever applied):

```toml
[skills]
enabled               = true
source_path           = "/srv/skills/release.git"   # exactly one of path|url is set
source_url            = ""                          # https://host/org/skills.git
source_ref            = "v*"                        # ref_mode=tag: tag pattern; ref_mode=branch: branch name
ref_mode               = "tag"                      # tag (default) | branch
pull_interval         = "15m"
pull_jitter_pct       = 10
pull_on_boot          = true
pull_timeout          = "2m"
pull_max_bytes        = 67108864
require_signature     = true                        # may be false only with a local_path source + approve="review"
approve               = "review"                     # auto | review | never
canary_host_id        = ""                          # "" disables the canary gate
canary_validity       = "7d"
canary_override       = false
auto_accept_enabled   = false
auto_accept_threshold = 1
auto_accept_window    = "24h"
auto_accept_modules   = ["proc.connections", "proc.top"]
max_hold              = "30m"
demote_after_failures = 2
state_dir             = ""                          # default: <state root>/skills-local
git_binary            = "git"
local_enabled         = false                      # §2b — the local SKILL.md library, off by default
local_dir             = ""                         # §2b — one <name>/SKILL.md per skill
local_max_bytes       = 1048576
local_max_skills      = 64
local_max_steps       = 16
local_step_timeout    = "60s"

[[signers]]
key_id     = "skills-2026"
public_key = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="  # base64 std, 32-byte ed25519 public key
trust      = "release"
enabled    = true
added_ts   = "2026-09-01T00:00:00.000Z"
```

### 2a The shipped compiled default: the loop is OFF

The `[skills]` table above is the **operator's opt-in**, not the compiled default. The shipped default column
has `enabled = false`: `internal/skills.DefaultConfig()` returns the loop disabled, and `DisabledConfig()` is
now the same value — the two named "off" constructors cannot drift apart. Every other key keeps the default
shown above. With `enabled = false` the daemon pulls nothing and no skill is ever applied (the rule at the top
of this section), and the loop is BUILT rather than refused, so the `/health.json` subsystem row of SPEC-12
§3.3a reports `built=true, refused=false`.

Why off: no distribution channel is compiled in and this build names none — neither `source_path` nor
`source_url`. An *enabled* loop with neither key is refused at boot with `TROUBLE-SKILLS-001: exactly one of
source_path or source_url must be set`. That is the correct answer for a loop someone ASKED for and cannot be
built, and the wrong one for a stock boot that never asked: it would refuse the skill loop and degrade
`/health.json` on the operator's first run, permanently, for a subsystem the operator never configured.

Enabling the loop is the key that turns it on — exactly one of the two §4.1 source forms:

| opt-in | keys |
|---|---|
| local channel (a git dir or bare repo) | `[skills] enabled = true, source_path = "/srv/skills/release.git"` |
| remote channel | `[skills] enabled = true, source_url = "https://host/org/skills.git"` |

Both keys at once, or neither while `enabled = true`, stays a boot rejection (`TROUBLE-SKILLS-001`), and every
other §4.1 and §4.3 rule — a credential-bearing URL, an unknown `ref_mode` or `approve`, a
`require_signature = false` loop that is not a review-policy local channel — applies unchanged the moment the
loop is on.

### 2b The local skill library — `SKILL.md` + frontmatter (v0.1.1b)

The brief's other half of the skill story is authoring: a remote agent writes a skill in the Claude-skill
format (`SKILL.md` with YAML frontmatter), the daemon reads the directory, and a skill step runs against
the runner. That is a **local authoring surface**, not a distribution channel, and the distinction is the
whole security argument for this section:

* the **signed** `SKILL.toml` artifact of §3.1 stays the only form that crosses a host boundary (pull,
  canary, approve, floor, conflict). Nothing in this section is pulled, merged, signed or version-negotiated;
* a library step therefore has exactly a locally-authored play's authority — the same descriptor
  allowlist, the same `authorize → validate → dry_run → apply → verify → audit` stages, the same
  autonomy gate (SPEC-05 §3.11). It can never exceed what a rule's play already can do;
* the library is **off** unless `local_enabled = true` and `local_dir` names a directory. Nothing is
  read, nothing is executed, and no directory is created when it is off.

Configuration (added to the `[skills]` table as six keys; the parser that reads them is the same strict
decoder as §2):

| Key | Default | Meaning |
|---|---|---|
| `local_enabled` | `false` | read `local_dir` at all |
| `local_dir` | `""` | the library root: one `<name>/SKILL.md` per skill (absolute; no `..`, no URL) |
| `local_max_bytes` | `1048576` | per-file cap; a larger `SKILL.md` is refused, never truncated |
| `local_max_skills` | `64` | scan cap; more files than this is a refusal naming the cap |
| `local_max_steps` | `16` | per-skill step cap |
| `local_step_timeout` | `60s` | the wall clock of one step (passed to the runner's own deadline) |

The loader's contract:

```go
// Library is the local SKILL.md library (SPEC-11 §2b). It reads a directory and
// runs a step through the runner — both actions typed, neither parameterisable
// with code.
type Library struct{ /* cfg, deps, runner, clock */ }

type LibraryConfig struct {                 // the local_* keys, resolved
    Enabled     bool
    Dir         string
    MaxBytes    int64
    MaxSkills   int
    MaxSteps    int
    StepTimeout types.Duration
}

type LocalSkill struct {                    // one <name>/SKILL.md, parsed
    Name        string
    Description string
    Version     int
    Dir         string                      // <local_dir>/<name>
    File        string                      // <dir>/SKILL.md
    Digest      string                      // hex64(sha256(SKILL.md bytes))
    Frontmatter map[string]string           // every scalar the file declared, for reporting
    Steps       []SkillStep
}

type SkillStep struct {                     // one typed step from the frontmatter
    Index  int
    Title  string
    Module string                          // an exact registry descriptor name
    Args   map[string]any                   // literals only
}

// StepRunner is the typed execution view of SPEC-06's registry; *registry.Runner
// satisfies it, and so does the ladder's PlayRunner adapter. There is no method
// that takes a command line, a script, a template or an interpreter.
type StepRunner interface {
    Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error)
    Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error)
}

func NewLibrary(cfg types.SkillsConfig, deps Deps, runner StepRunner) (*Library, error) // -001 on a malformed config
func (l *Library) Scan(ctx context.Context) ([]LocalSkill, error)                        // read-only, name order
func (l *Library) Get(name string) (LocalSkill, bool)
func (l *Library) Plays(ctx context.Context) ([]types.Play, error)                        // SPEC-05 §2b's read side
func (l *Library) RunStep(ctx context.Context, inc types.Incident, name string, step int, mode string) (types.ToolCall, error)
```

The library file is `<local_dir>/<name>/SKILL.md`, whose frontmatter carries the standard skill keys
(`name`, `description`, `version`, plus any other scalar an author wants) and, optionally, a namespaced
step list:

```markdown
---
name: payment-worker-queue-wedge
description: >-
  The payment worker's queue wedges when the socket backlog fills; check the
  connection count, then reload the unit.
version: 1
trouble:
  steps:
    - title: count the workers' sockets
      module: proc.connections
      args:
        pid: 4242
    - title: reload the unit
      module: service.reload
      args_json: '{"unit":"payment-worker.service"}'
---

Body: the prose a human (or an agent) reads. It is NEVER executed, never parsed
for commands, and never interpreted as tool calls.
```

Rules, each of which a §7 test falsifies:

1. **A step is typed or it does not exist.** `module` must be an exact descriptor name (no globs, no
   `pkg.*`), and `args` are literals: scalars in a `args:` block, or one strict JSON object in
   `args_json:`. A key named `shell`, `exec`, `cmd`, `command`, `script`, `eval`, `run` or `interpreter`
   anywhere under `trouble:` is **TROUBLE-SKILLS-001** (`reason=unknown_key`) — that is the "never
   arbitrary code" half of the contract, and it is a schema refusal, not a convention.
2. **The body is data.** A fenced command block in the markdown body is never run, never extracted and
   never passed to a runner: `RunStep` takes a step INDEX and reads it from the parsed frontmatter.
3. **The parse is a documented subset, and unsupported syntax FAILS LOUD.** Supported: flat `key:`
   scalars, one level of nesting (the `trouble:` block), `- ` list items with scalar keys, block
   scalars (`>`, `>-`, `|`, `|-`) for multi-line values, and `#` comments. Unsupported (a deeper
   nesting, a list of lists, a tab indent, a duplicate key, a `[[…]]` header, a value that runs past
   the frontmatter delimiter) is refused with the file named — plus the **line** where the syntax is
   line-oriented (an unparsable line, a tab indent, a header, a duplicate key, a mis-indented key, an
   unterminated frontmatter) and the **key** where the refusal is about a value's shape (a nested arg
   value, a module name that is not a descriptor name, a missing name or description). A hand-rolled
   subset that silently ignores what it cannot read is how a step disappears from a reviewed skill.
4. **Per-file isolation.** One malformed `SKILL.md` is refused (a `refused` record with the reason) and
   the rest of the directory still loads: a broken draft must not blind the whole library.
5. **The directory must not be a local privilege escalation.** A `local_dir` (or any ancestor up to the
   state root) that is group- or world-writable is refused (`reason=dir_writable`): a library whose
   files another user can replace is a capability handed to that user.
6. **Enable-time, not call-time, detection of a missing directory**: `local_enabled = true` with a
   `local_dir` that does not exist or is not a directory is a construction refusal
   (`reason=dir_missing`), the same posture §2a takes for a declared loop with no channel.

`Plays` compiles each skill to the shape the runner already consumes: `name`, `version`,
`source = "skill-local:<name>@<version>"`, and one `PlayTask` per step (`tool` = the module, `args` =
the literals). That is what makes "the ladder reads the library" a read of typed plays rather than a
new execution language.

## 3. Data model

### 3.1 The artifact — `skills/<name>/SKILL.toml`, schema generation 1

The artifact is **immutable**: it is written by an author (human or the promote loop in §3.4), merged by PR
review in the release-channel repo, and thereafter only read. The daemon never writes a pulled artifact, never
rewrites a comment, and never adds a key. The repo layout the puller expects:

```
skills/<name>/SKILL.toml                       # the artifact
skills/<name>/plays/<name>@<version>.toml      # the play named by play_ref (relative path, forward slashes)
keys/<key_id>.pub                              # informational only; the authoritative signer set is config
```

Valid example (this exact artifact is the §7 golden vector):

```toml
name               = "payment-worker-queue-wedge"
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
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `name` | string | yes | `^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$` (≤64, kebab-case); must equal the `<name>` directory |
| `version` | int | yes | `>= 1`, monotonic per name (§3.3); the version token in `play_ref` must match it |
| `sigs` | array<string> | yes | 1–64 entries, unique, **each parses by §3.3**; order is signed but matching is set-based |
| `play_ref` | string | yes | relative, no `..`, no URL, no absolute path, no glob; file must exist in the same tree |
| `min_daemon_version` | string | yes | semver `X.Y.Z[-pre][+build]` (§4.3) |
| `allowed_modules` | array<string> | yes | 1–32 descriptor names (`config.get`, `service.reload`, `flow.*` globs are **not** allowed; exact names only) |
| `signer_key_id` | string | yes | `^[a-z0-9][a-z0-9._-]{2,63}$`; resolved from `[[signers]]` |
| `signature` | string | yes unless `require_signature=false` | base64 std **with padding** over the canonical bytes (§3.2); 88 chars for ed25519 |
| `[guards].verify_window` | duration | yes | `> 0`, `<= 1h`; the window the sig must stay quiet after apply |
| `[guards].max_runs` | string | yes | `"<n>/day"`, `n = 1..20`; caps skill application per sig per host per day |
| `[guards].escalate_on` | string | yes | `verify_fail \| tool_error \| never` |
| `[provenance].incidents` | array<string> | yes (≥1 unless research-only) | `inc_` ids that taught the skill |
| `[provenance].research` | array<string> | no | `res_` ids of returned briefs |
| `[provenance].author` | string | yes | non-empty; the promoting actor (`Actor.ID` + `"@"` + host id for local promotion) |
| `[provenance].created_ts` | string | yes | RFC3339 UTC, millisecond precision, `Z` |

Strict decoder: any key or table outside the set above — including `stats`, `shell`, `exec`, `cmd`, `script`,
`command`, or any unknown name — is **TROUBLE-SKILLS-001** (`reason=unknown_key|unknown_table`), not a warning.
This is what makes the artifact incapable of expressing anything except typed registry tool calls: there is no
field in which arbitrary code could be written, and `allowed_modules` selects capabilities, it does not grant
new ones. A future schema generation cannot be misread as generation 1, because the generation token is part of
the signed material (§3.2) and the accepted key set is exact.

### 3.2 The canonical byte form (the exact signed material)

The signature is **not** over the TOML file bytes: TOML has no canonical form (comments, whitespace, key order,
inline-vs-table layout are all free), so byte-signing would invalidate an artifact for a reformat and would let
two byte-different files carry the same declared semantics. The signed material is a deterministic projection:

```
canonical_bytes(skill, play_bytes) =
    "trouble.skill.v1\n"
  + canonical_json + "\n"
  + "play_sha256=" + hex64(sha256(play_bytes)) + "\n"
```

`canonical_json` is the compact (`separators (',',':')`, no HTML escaping, UTF-8) JSON encoding of the fields
below, in **exactly this order**, nested objects in the field order shown, `sigs`/`allowed_modules` in the
artifact's declared order:

```json
{"name":"payment-worker-queue-wedge","version":3,"sigs":["sentinel:sha256v1:9f2c1d3e4b5a6c7d","journald:sha256v1:2ab4c6d8e0f1a3b5"],"play_ref":"plays/payment-worker-queue-wedge@3.toml","guards":{"verify_window":"10m","max_runs":"3/day","escalate_on":"verify_fail"},"provenance":{"incidents":["inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"],"research":["res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"],"author":"troubled@hostA","created_ts":"2026-09-11T04:00:00.000Z"},"min_daemon_version":"0.1.0","allowed_modules":["proc.connections","service.reload","config.set"],"signer_key_id":"skills-2026"}
```

`signature` is excluded from the projection (it signs it). `signer_key_id` **is** included, so swapping the key
id invalidates the signature. `play_sha256` binds the payload: signing a manifest without the play bytes would
be theater — the play is what executes — so a play file edited after review fails verification (TROUBLE-SKILLS-002)
even though `SKILL.toml` is untouched. The generation prefix `trouble.skill.v1` inside the signed bytes is the
artifact's schema version.

Golden vector (pinned; §7 asserts it byte-for-byte): the play fixture used is the three-task `proc.connections`
→ `service.reload` → `config.set` play shown in §7.1. `play_sha256 =
b4ae5a5b5788e560f03d11fe365a48aa02b1a03c995c069a09485a4d0d0bddec`; `sha256(canonical_bytes) =
b1ecc0e5d86018696bc2f25cb850e99d56350b33af8e74febde6ad2222de895c` (662 bytes); `signer_key_id = "skills-2026"`,
public key `A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=`, signature
`EhHNesvuVNHJa+6HNj2xKS0FGfHrs76+pmGNUBB7+hVzHMphJ/GJD7NKwGolDngaoqL/m/Rq/MRjk6wBb17ZBA==`.

Verification order (first failure wins; the numeric code is recorded, and an artifact that fails any step is
never installed): (1) TOML parse (001); (2) key set + field rules (001); (3) `play_ref` resolves and reads (001);
(4) signature base64/64-byte decode (002); (5) signer id lookup in `[[signers]]` (003); (6) ed25519 verify over
`canonical_bytes` (002); (7) `play_sha256` recomputed from the file (002).

### 3.3 Sig binding and versioning rules

**Binding is exact string match.** A skill matches a recurrence if and only if `sig` equals one of the
artifact's `sigs` entries character-for-character. No globs, no prefixes, no case folding, no normalization at
match time. Consequence, pinned: `sentinel:sha256v1:…` and `sentinel:sha256v2:…` are different signature spaces
(SPEC-TYPES §6.3) and a skill written for `v1` does not match a `v2` sig — the group is shown as "related" on
the dashboard only, and the incident takes its normal rung path. Grammar enforced at load:

```
sig      = source ":" "sha256" "v" norm_version ":" hex16
source   = "sentinel"|"journald"|"psi"|"dbus"|"disk"|"timers"|"inotify"|"collector"|"generic"|"unknown"
hex16    = 16 lowercase hex chars        norm_version = [1-9][0-9]*        regex: ^(…):sha256v([1-9][0-9]*):([0-9a-f]{16})$
```

A free-text entry (for example the PRD's `journal:payment-worker:restart-loop`) fails the regex →
TROUBLE-SKILLS-001 (`reason=sig_grammar`). Every sig in the artifact, the ledger, a lease and a dedup key is
produced by `types.Sig.String()`; a skill may never introduce a sig the daemon could not have computed.

**Versioning.** `version` is a positive integer, monotonic per `name`. The author bumps it; the daemon never
bumps a pulled version. Rules:

| Rule | Behaviour |
|---|---|
| Same version, different canonical bytes | TROUBLE-SKILLS-006 conflict **and** TROUBLE-SKILLS-013 refusal of both candidates (§4.5) — an ambiguous executable is never resolved by ordering |
| Lower version than the highest installed for that name | TROUBLE-SKILLS-007; the artifact is not installed. Only `trouble skills approve <name>@<v> --allow-downgrade` (human, reason required, ledger `actor.kind=human`) installs it |
| Higher version, different play bytes | normal upgrade; the previous version's directory is kept until `skills.retain_versions` (default 2) is exceeded, then removed. An applied play is never re-executed from a removed directory (SPEC-05 parks in-flight plays) |
| Version reused by a *pull* after the same int was consumed by a **local** promotion | TROUBLE-SKILLS-013 (`reason=name_owned_by_release`): a locally promoted name may not collide with a release-channel name, because the channel's int sequence is authoritative and a local int would eventually collide on pull |
| Local promotion version allocation | `1 + max(version over every row for that name in the local index, including refused/pending/quarantined)` — monotonic by construction, so a local int is never recycled |

A signature change alone (same version, same play bytes, a new `signer_key_id`/`signature`) is **not** an
upgrade, is not installable, and is reported as TROUBLE-SKILLS-006 (same version, different canonical bytes,
because `signer_key_id` is signed). A signature change **with** a version bump is an ordinary upgrade and
re-verification is mandatory before that version can be applied.

### 3.4 The local promote loop (candidate path)

State machine over `SkillCandidate.State` (`drafted → reviewed → promoted`, with `rejected` and `refused` as
terminal failures; `pulled` marks a candidate that arrived from the channel, which is the pull path of §4.1 and
carries no local review):

| Step | Trigger | Written | Rules |
|---|---|---|---|
| `drafted` | an incident whose rung ③ agent fix passed its verify window (SPEC-05: evidence tuple, `canary_seen=true`, `sources_missing=[]`) | ledger `skill` record, `phase=candidate_drafted`, `sig=<incident sig>`; `SkillCandidate{ID: sk_…, Name, Version, Sig, Inc, Play, ResearchID, State:"drafted", CreatedTS}` + `examples/skills/<name>/SKILL.toml` draft on disk under `skills-local/candidates/<sk_id>/` | TROUBLE-SKILLS-008 if `provenance.incidents` would be empty or the research link is missing while the research rung returned. The candidate's `Play.Source = "skill:<sk_id>"` and the play must be check_mode-clean (`play_run` records with `mode=check_mode`) |
| `reviewed` | review decision | ledger `phase=candidate_reviewed`, `ReviewActor` = `Actor.ID` of the reviewer | Default reviewer is **human** in `shadow` and `assisted`. In `autonomy=full` only, auto-accept applies (§4.6) and the reviewer actor is `troubled` with `auto_accept=true` in the payload |
| `promoted` | promote step | ledger `phase=promoted`; the artifact is signed with the local key and written to `skills-local/installed/<name>/<version>/` | The promoted version is usable for recurrences on **this** host only. Cross-host distribution requires the release channel; the promotion step's hand-off seam to that channel is `(v1.0 hand-off)` — the candidate path ends at the local artifact in v0.1 and the CLI prints the artifact path plus the `SkillCandidate.BranchOrPR` field left empty |
| `rejected` | review `reject` | ledger `phase=candidate_reviewed`, `error_code=TROUBLE-SKILLS-009` | The play is not deleted locally (audit), but its content hash is recorded as rejected and no promotion may reuse it |
| `refused` | any gate refusal after drafting | ledger `phase=refused` with the cause `error_code` | §5 |

Local promotion signs with the host's local ed25519 key (`[[signers]]` entry with `trust="local"`, private key
`<state root>/skills-local/local.ed25519`, mode 0600, generated on first boot). A `local` trust key is accepted
**only** for artifacts whose `provenance.author` ends with `@<this host_id>` and whose install row records
`origin=local`; it can never sign an artifact this host pulls from the channel. The canary gate does not apply
to a local artifact — it was born and verified on this host, and the incident's `Evidence` tuple is copied into
the install row as its canary evidence.

### 3.5 Local persistence under `<state root>/skills-local/` (0770 → files 0640, dir 0700)

```
skills-local/
  repo/                       # git cache clone of the release channel (read-only use; never pushed to)
  staging/<ulid>/             # one pull in flight: verified before any rename
  pending/<name>/<version>/   # verified but awaiting review (approve="review")
  installed/<name>/<version>/ # SKILL.toml + <play_ref> verbatim; install row state=installed
  quarantine/<name>/<version>/# installed and then failed re-verification, or demoted; never re-installed
  candidates/<sk_id>/         # local drafts (§3.4)
  local.ed25519               # local signing key, 0600
  index.json                  # localIndex: install rows, canary results, refusal counters
  stats.json                  # SkillStats rows (§3.6)
  pull-state.json             # last_pull_ts, last_pull_sha, failures, next_pull_ts, last_error
```

`index.json` (package-private `localIndex`; local persistence only — never a wire format, never a ledger
payload, never an exported parameter):

```json
{"schema_version":1,"host_id":"7f3a91c2d4e5b607","revision":214,"rows":[{"name":"payment-worker-queue-wedge","version":3,"state":"installed","origin":"local","dir":"installed/payment-worker-queue-wedge/3","signer_key_id":"skills-2026","canonical_sha256":"b1ecc0e5d86018696bc2f25cb850e99d56350b33af8e74febde6ad2222de895c","play_sha256":"b4ae5a5b5788e560f03d11fe365a48aa02b1a03c995c069a09485a4d0d0bddec","sigs_sha256":"5d4b1f0c2a7e93d6","installed_ts":"2026-09-16T09:14:03.221Z","canary":{"result":"green","host_id":"c0ffee1234567890","ts":"2026-09-15T22:10:00.000Z","apply_mode":true},"refusals":0,"refusal_code":"","reason":""}]}
```

Every boot re-verifies each `installed/` row (steps 1–7 of §3.2) against `canonical_sha256`/`play_sha256`. A
row that no longer matches is moved to `quarantine/`, marked `state=refused`, and reported with
TROUBLE-SKILLS-002 + a refusal record: local tampering is detected, never executed.

### 3.6 Stats — the `[stats]` → ledger split

`SkillStats` is **local state**, never artifact state. `stats.json` holds one row per (name, version):

```json
{"schema_version":1,"host_id":"7f3a91c2d4e5b607","rows":[{"name":"payment-worker-queue-wedge","version":3,"applied":4,"success":4,"last_used_ts":"2026-09-16T09:14:03.221Z","refusals":0}]}
```

- Write path: `internal/skills` increments the row in memory when a skill-sourced play run finishes
  (`applied`/`success`/`refusals`) and flushes with an atomic `write+rename` at most every 5 s and on shutdown;
  the crash-loss window for counters is therefore ≤ 5 s and is stated in docs. A flush failure emits
  TROUBLE-SKILLS-014 (transient), is retried at the next flush, and **never** blocks or fails a play: counters
  are not evidence (evidence is the ledger's `play_run`/`verify` records, SPEC-05).
- `stats.json` is never part of `canonical_bytes` (adding stats to an artifact changes the canonical form and
  fails signature verification: 002), is never written to the channel repo, and is never forwarded as artifact
  content; hosts that need fleet-wide stats read the ledger, not this file.
- A `stats` key or table in an artifact is TROUBLE-SKILLS-001 (`reason=stats_in_artifact`) — the exact fix for
  the PRD's contradiction, and the reason the distribution path stays unidirectional (pull only).

### 3.7 Types added to SPEC-TYPES by this spec

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

## 4. Wiring

### 4.1 Pull loop (pull only — the channel is read, never written)

| Aspect | Pinned behaviour |
|---|---|
| Source | exactly one of `source_path` (git dir/bare repo; the air-gapped and test path) or `source_url`; a URL with embedded credentials is rejected at config validation (SPEC-12 §5) and the source is logged with the userinfo stripped |
| Release channel discipline | the daemon only reads. Commands are limited to `git ls-remote`, `fetch`, `rev-parse`, `checkout --detach`, `cat-file`, executed as an **argv array** (`git_binary`), never through a shell — the agent never invokes git at all. No `push`, `commit`, `merge`, `gc` or `prune` is ever issued against the source repo: the channel is a release channel (protected branch + PR review + tags), not a live-write share |
| Ref policy | `ref_mode=tag` (default): resolve `source_ref` as a tag pattern, select the highest semver tag (`v` prefix optional); `ref_mode=branch`: resolve the branch tip (legal, but recorded in the ledger with `ref_mode=branch` so an operator can see a mutable head was trusted) |
| Cadence | one pull immediately after `READY`, then every `pull_interval` ± `pull_jitter_pct` (jitter is per-host, derived from `sha256(host_id)` so N hosts do not stampede one repo), single-flight (a mutex; a second trigger is dropped) |
| Timeout/budget | the pull runs under `pull_timeout` ("2m") as a process group; on expiry the group is killed → TROUBLE-SKILLS-011. The staging tree is capped at `pull_max_bytes` (64 MiB); exceeding it aborts → TROUBLE-SKILLS-010 |
| Install atomicity | staging → per-artifact verify → gate order (§4.2) → `rename(2)` into `pending/` or `installed/`. One artifact's refusal never blocks another's install; every refusal is its own record |
| No-op | when the resolved sha equals `last_pull_sha` the pull writes **no** ledger record (pull activity is visible in `SkillsStatus` and in `/health`); one `skill` record is written per state change, so the ledger does not grow at the pull interval |
| Offline | source unreachable, DNS/TLS failure, `git` binary missing → TROUBLE-SKILLS-011, the current set is kept exactly as installed, retry at the interval with backoff `min(interval * 2^failures, 30m)`; `consecutive_failures >= 5` marks skills degraded in `SkillsStatus`/`/health`. A reachable source that fails on ref resolution or checkout → TROUBLE-SKILLS-010. Neither path ever removes or disables an installed artifact |
| Read surface | the daemon publishes nothing to internal/dashboard; SPEC-10 renders the current set read-only from the ledger index (`kind=skill`, latest row per name) on its existing overview route, and `trouble skills status --json` is the machine surface. No new route, no new type |

### 4.2 Gate order at pull (first failure wins; every failure writes a refusal record)

```
1 parse + strict schema        → 001   (never installed)
2 signature + play_sha256      → 002 / 003
3 modules present              → 005 (allowlist) / 013 reason=missing_module
4 floor: min_daemon_version    → 004   → refusal 013 reason=floor
5 canary                       → 012   → held (retry at the next pull)
6 approve policy               → review → pending_review ; never → 013 reason=approve_never
7 conflict / version rules     → 006 / 007 / 013 reason=name_owned_by_release
8 install (atomic rename)      → ledger phase=installed | pending_review
```

This ordering is the reason a floor-blocked artifact is recorded as a refusal (TROUBLE-SKILLS-013 with
`payload.error_code=TROUBLE-SKILLS-004`) instead of being skipped: on a satellite below the floor the hub sees
`floor_blocked` for that (host, name, version) in the forwarded records and does not count the host as running
the version — the version matrix on the dashboard is derived from these records, and the "hub refuses" path is
exactly this gate run against the hub's own daemon version for hub-local decisions.

### 4.3 `min_daemon_version` floor

Semver `^v?(\d+)\.(\d+)\.(\d+)(-([0-9A-Za-z.-]+))?(\+([0-9A-Za-z.-]+))?$`; numeric compare on
major/minor/patch; a prerelease ranks **lower** than the same release (`0.1.0-rc1 < 0.1.0`); build metadata is
ignored. The daemon version is the ldflags-stamped `Actor.Version` (SPEC-12 §3). An unstamped build reports
`0.0.0-dev`, which fails every floor `>= 0.0.1` → TROUBLE-SKILLS-004 (`reason=unstamped_binary`): an
unidentifiable binary may not run distributed remedies, which is the same posture as "dev (unknown)" scar in the
fleet. There is no version-override config key by design. A skill unsatisfied by the floor stays
`floor_blocked`; a **newer** artifact whose floor this daemon satisfies installs normally.

### 4.4 Canary host first, then per-host approve

- `canary_host_id` configured and equal to this host: the canary gate passes, the artifact is installed, and the
  first real or synthetic recurrence of one of its sigs runs the play. The outcome is the canary result:
  `green` when the play ran with `mode=apply` (not check_mode) and the verify window passed with a complete
  evidence tuple; `failed` on verify failure, tool error, or escalation. `green` expires after `canary_validity`
  (default 7d), after which it must be refreshed by another green run.
- Any other host: a pulled version is acceptable only when the local index holds `canary.result=green` for
  `(name, version)` with `canary.host_id == canary_host_id` inside the validity window. Otherwise
  TROUBLE-SKILLS-012: the artifact is **held** (not installed, retried at the next pull interval). A `failed`
  canary result is terminal for that version: refusal 013 (`reason=canary_failed`) and other hosts refuse while
  a failed version is the highest available.
- Shadow mode cannot certify a mutating remedy: a check_mode-only run is not a green canary (recorded as
  `canary.result=inconclusive`, `reason=canary_needs_apply`), so with a canary configured, shadow hosts stay at
  012. The two legal ways forward are `autonomy=assisted` (per-rule execute grant) on the canary host or the
  explicit override below.
- Override: `trouble skills approve <name>@<v> --override-canary --reason R` installs on **this** host only when
  `canary_override=true` (default false) and `approve != "never"`; the install row records
  `canary_override=true`, the actor, the reason, and the ledger record carries them. The version stays blocked on
  every other host — the override is per host and never propagates.
- Approve policy (per host, default safe): `never` → pulled artifacts are refused (013 `reason=approve_never`),
  local promotion unaffected; `review` (default) → installed into `pending/`, state `pending_review`, never
  executed until `trouble skills approve`; `auto` → installed and eligible for application subject to the ladder
  autonomy gates. Installation is not authority: in `shadow`, an `auto` host still executes mutating skill plays
  as check_mode downstream (SPEC-05 §3), so the worst case of a bad `auto` install is a diff on a dashboard.

### 4.5 Conflict rules

1. **Two skills match one sig.** Selection is deterministic: highest `version` wins; the loser is named in the
   conflict record (TROUBLE-SKILLS-006; payload carries `winner{name,version}` and `loser{name,version}`). The
   losing version is never applied while the winner is installed, and the conflict is not suppressed by the
   record-dedup window (§4.7) — it is re-checked at every resolution.
2. **Equal version, different canonical bytes** (two skills with the same name+version from different sources, or
   the same version with a different signature/play): no winner is derivable and none is invented. Both are
   refused (TROUBLE-SKILLS-006 + TROUBLE-SKILLS-013, `reason=version_ambiguity`), the signature falls back to
   the previously installed version for that name if one exists, and otherwise the recurrence takes the normal
   ladder path (agent rung). Ambiguity in an executable path is fail-closed.
3. **Overlapping sig sets during an open verify window.** If skill A (any version) has an applied, unfinished
   verify window whose sig is in skill B's sig set, B's application for that sig is **held** until the window
   closes (SPEC-05 owns windows; skills reads them through the ledger index). The incident is not blocked: it
   continues with the next rung and B is retried when the window closes. If the hold exceeds `max_hold`
   (default "30m") the hold is converted into a refusal record (TROUBLE-SKILLS-013, `reason=hold_expired`) so it
   never becomes a silent drop. Window passed → the sig is resolved and the hold is released; window failed →
   the failed skill's `failures` counter increments and, at `demote_after_failures` (default 2) consecutive
   verify failures for that (name, version), the version is demoted: state `quarantined`, ledger
   `phase=demoted`, TROUBLE-SKILLS-013, and the sig escalates to the agent rung.
4. **Budget guard.** `[guards].max_runs` caps applications per sig per host per day; exceeding it is a refusal
   record with `reason=max_runs` and no play run.

### 4.6 Allowed modules — the only capability path

- A skill can do exactly one thing: cause typed registry tool calls. There is no shell, no interpreter, no
  template, no `exec`, no arbitrary file write; the artifact schema (§3.1) has no key that could express them
  and the strict decoder rejects unknown keys.
- `allowed_modules` is an allowlist over **descriptor names**. The install-time check collects every
  `PlayTask.Tool` and requires the set to be a subset of `allowed_modules` (TROUBLE-SKILLS-005) and of the
  locally registered descriptors (TROUBLE-SKILLS-013 `reason=missing_module`). An allowlist entry with no local
  descriptor is recorded as `missing_modules` in the install payload but does not block, because an unused
  capability is not a failure.
- At call time the registry's authorize stage consults the `Authorizer` hook, which resolves
  `source == "skill:<sk_id>"` against the installed row and re-checks `allowed_modules` (TROUBLE-SKILLS-005) —
  defense in depth against a locally mutated play file or a stale in-memory task list. The hook is injected by
  the composition root (`cmd/troubled`); `internal/registry` imports `internal/types` only and never
  `internal/skills`, so no import cycle exists.
- `args` are literals (no interpolation exists in the play schema); the only dynamic element is `when:`,
  evaluated in the shared condition language (SPEC-03 §3.4). A tool call still passes the module's JSON schema
  (SPEC-06) and the autonomy gates, so a signed skill cannot exceed an unsigned one's authority.

### 4.7 Promote loop wiring, ledger records and read surfaces

Incident → rung ③ agent fix → verify window passes → `Promoter.Draft` (provenance: incident + research brief;
TROUBLE-SKILLS-008 if either is missing while required) → review (human default; auto only in `autonomy=full`)
→ `Promoter.Promote` (local key signs; version = local max+1) → installed → **next recurrence of any of its
sigs on any host with the version runs rung ② with zero agent runs** (AC-24). Auto-accept (AC-26) requires
**all** of: `autonomy=full` with `AllowSkillAccept`, `auto_accept_enabled=true`, the candidate's play inside
`auto_accept_modules`, a passed verify window with positive evidence, no prior rejection of the same play
content hash, no open conflict or refusal for the sig, and at least `auto_accept_threshold` clean rehearsals
inside `auto_accept_window` (default 1 inside "24h"; each further recurrence inside the window re-runs the play
in check_mode and counts as a rehearsal).

Ledger records (`kind=skill`, owned by this spec per SPEC-INDEX §3.4; `origin.source="skills"`, `sig` set for
incident-linked phases and `""` for distribution phases, `payload.error_code` mirroring SPEC-INDEX §5 rule 3):

| `payload.phase` | Emitted when | Required payload fields |
|---|---|---|
| `candidate_drafted` | draft created | `name, version, inc, sig, research_id, play_sha256, state` |
| `candidate_reviewed` | review decision | `name, version, decision, actor, reason, auto_accept` |
| `promoted` | local artifact signed + installed | `name, version, sig, signer_key_id, canonical_sha256` |
| `demoted` | `demote_after_failures` reached | `name, version, failures, error_code` |
| `pulled` | pull summary (only when the sha changed) | `ref, ref_mode, resolved_sha, seen, installed, refused, missing_modules` |
| `installed` / `pending_review` | artifact installed | `name, version, origin, signer_key_id, canonical_sha256, canary` |
| `refused` | any gate refusal | `name, version, error_code, reason, count` |
| `conflict` | §4.5 rule 1 or 2 | `sig, winner{name,version}, loser{name,version}, count` |
| `hold` / `hold_expired` | §4.5 rule 3 | `sig, held{name,version}, window_end, count` |
| `canary_ok` / `canary_failed` | canary run finished | `name, version, host_id, result, apply_mode, verify_result` |
| `stats` | stats flush after a skill-sourced play | `name, version, applied, success, refusals` |
| `pull_failed` | source unreachable / checkout failure | `error_code, failures, next_pull_ts` |

Refusal records are deduplicated on `(name, version, phase, error_code, reason)` with a `count` that increments;
the record is re-emitted at most once per 24 h with the accumulated count, so a permanently refused artifact on
every pull interval cannot flood the ledger — and no refusal is ever dropped.

### 4.7a Reading and executing a library step (v0.1.1b)

Wiring, one hop each way:

| Direction | Call | Notes |
|---|---|---|
| composition root → library | `skills.NewLibrary(cfg, deps, runner)` | `runner` is the registry adapter (`*registry.Runner` or the ladder's `PlayRunner` adapter) — the strictest available one, so a step passes the six stages |
| ladder → library | `SkillLibrary.Plays` / `RunStep` (SPEC-05 §2b) | the ladder is the only caller; it owns the gate decision and passes the `mode` |
| library → runner | `StepRunner.Check` / `StepRunner.Apply` | `check_mode` under `shadow`, `apply` only when the gate granted the module |
| library → ledger | one `skill` record per scan and per step | phases below |

Ledger records (kind `skill`, `origin.source="skills"`; the §4.7 dedup rule applies to the refusal row):

| `payload.phase` | Emitted when | Required payload fields |
|---|---|---|
| `library_loaded` | a scan completed | `dir, digest, skills, refused, steps, max_skills` |
| `library_refused` | one `SKILL.md` was refused | `dir, file, reason, line, error_code` |
| `step_executed` | one step finished against the runner | `name, version, step, module, mode, changed, tool_call, error_code` |

`RunStep` refuses rather than guesses:

* an unknown skill name, a step index outside the skill's steps, or a `mode` that is not `check_mode`/
  `apply` → TROUBLE-SKILLS-013 (`reason=unknown_skill` / `unknown_step` / `bad_mode`), no runner call;
* a step whose module is not an exact registered descriptor name → TROUBLE-SKILLS-013
  (`reason=missing_module`), no runner call — the same rule §4.6 applies to a play;
* a runner refusal (do-not-touch, ungranted scope, kill-switch) is passed through with the runner's own
  code and class: this package never re-mints another area's code;
* with `local_enabled = false` (the shipped default) every method is a no-op: `Plays` returns an empty
  list, `RunStep` refuses, and nothing on disk is read.

Authority, stated once: the library is local and unsigned, so it is exactly as powerful as a play a rule
already carries — no more. It cannot pull, cannot cross a host boundary, cannot mint a capability the
build does not register, and cannot express anything except typed tool calls (§2b rule 1).

## 5. Errors

| Code | Class | Fires when | Record / effect |
|---|---|---|---|
| TROUBLE-SKILLS-001 | permanent | artifact parse/schema invalid: bad TOML, unknown key or table, `stats` present, name/path/sig-grammar/range violation, missing required field | refusal record; nothing installed; `reason` names the field |
| TROUBLE-SKILLS-002 | permanent | signature verification failed (bad base64/length, ed25519 verify false, `play_sha256` mismatch, boot re-verification mismatch) | refusal record (boot case: quarantine + disable) |
| TROUBLE-SKILLS-003 | permanent | `signer_key_id` unknown, disabled, or wrong `trust` class for the artifact's origin | refusal record |
| TROUBLE-SKILLS-004 | permanent | `min_daemon_version` > daemon version, or unstamped build | refusal record; state `floor_blocked`; hub marks the host as not running the version |
| TROUBLE-SKILLS-005 | permanent | play references a module outside `allowed_modules` (install-time or at authorize) | refusal record / failed tool call with the audit record |
| TROUBLE-SKILLS-006 | permanent | two skills match one sig; or same version with different canonical bytes | conflict record naming winner and loser; ambiguity branch also refuses both |
| TROUBLE-SKILLS-007 | permanent | version lower than the highest installed for that name | refusal record; only an explicit human `--allow-downgrade` overrides |
| TROUBLE-SKILLS-008 | permanent | provenance missing (no incidents and no research, or empty author) at draft time | draft refused; no candidate written |
| TROUBLE-SKILLS-009 | permanent | candidate rejected at review | review record with `decision=reject`; content hash marked rejected |
| TROUBLE-SKILLS-010 | transient | pull failed against a reachable source: ref not found, checkout failure, staging exceeds `pull_max_bytes`, corrupt cache clone | `pull_failed` record; current set kept; backoff retry |
| TROUBLE-SKILLS-011 | transient | skills repo unreachable (connect/timeout/`git` missing), or the pull exceeded `pull_timeout` | `pull_failed` record; current set kept; retry at interval, degraded flag at 5 consecutive failures |
| TROUBLE-SKILLS-012 | permanent | canary host has no green result for this version inside `canary_validity` | held (state `canary_blocked`), retried at the next pull; not installed |
| TROUBLE-SKILLS-013 | permanent | skill refused on this host: `missing_module`, `name_owned_by_release`, `version_ambiguity`, `canary_failed`, `approve_never`, `hold_expired`, `max_runs`, `demoted`, operator reject | refusal record with `reason` + cause `error_code`; never a silent skip |
| TROUBLE-SKILLS-014 | transient | local stats write (atomic rename) failed | counter kept in memory; retried next flush; play unaffected |

## 6. Edge cases

1. **Artifact valid but play missing/changed after review** → step 3/7 of §3.2: TROUBLE-SKILLS-001 or -002, not installed. The reverse (play changed, artifact unchanged) is caught by `play_sha256`.
2. **Two tags point at the same commit** → the highest semver tag is selected; the tie is impossible because tags are unique names.
3. **Tag rewritten on the remote** (same name, new commit) → the resolved sha differs from `last_pull_sha`, artifacts are re-verified and re-gated; the ledger records the new `resolved_sha`. Signature verification is what makes a rewritten tag harmless.
4. **`approve=review` host receives a version for an already-installed name** → the new version goes to `pending/` while the old stays `installed/` and remains the version applied; approving promotes the new one and retains the old per `retain_versions`.
5. **Canary host goes offline** → other hosts stay at TROUBLE-SKILLS-012 (held) with the last known green expiring at `canary_validity`; no host ever applies an uncertified mutating remedy by timeout.
6. **Canary host runs a *different* build** → the canary result records `host_id` and `daemon_version`; a green result from a build below the artifact's floor is ignored as `canary_wrong_version` (012).
7. **Source path is a working tree, not a bare repo** → read-only commands only; the daemon never creates a branch, commit, or stash there, so an operator's checkout is never modified. A dirty working tree does not change the artifact bytes read (checkout is done in `skills-local/repo`).
8. **Two hosts promote the same name locally** → both allocate `local max+1` independently, so both may produce version N. They never meet, because local artifacts are not distributed in v0.1; if the same version is pulled again at any time, rule 2 of §4.5 refuses both on the affected host.
9. **Ledger write fails while installing** → the install is rolled back (the rename is only executed after the record is appended) and the pull reports TROUBLE-SKILLS-010; skills never exist without an audit record.
10. **`allowed_modules` names a descriptor that this build removed** → TROUBLE-SKILLS-013 `reason=missing_module` if the play calls it; if the play does not, the entry is only reported as `missing_modules`.
11. **Clock jump / stale `canary_validity`** → validity uses the wall clock (it compares two persisted timestamps, per SPEC-INDEX §6.5); a backwards jump can only hold an artifact longer, never release it early.
12. **`sigs` arrays that differ only in order between two versions** → same version + different canonical bytes → conflict rule 2; a version bump makes it an ordinary upgrade.
13. **Local key rotated** → artifacts signed by a removed local key stop verifying at boot re-verification and are quarantined (002) with a refusal record; the previous key stays in `[[signers]]` with `enabled=false` until the operator confirms, so rotation is visible rather than silent.
14. **Repo grows beyond `pull_max_bytes`** → the staging copy aborts at the cap (010) and the current set is untouched; the cache clone is not extended.

## 7. Testing

`internal/skills` tests are table-driven and use the §3.2 golden vector as the fixture.

1. `artifact_test.go` — valid fixture parses to the JSON in §3.7's example; 16 invalid fixtures each yield **001** with the expected `reason`: the PRD §06c artifact byte-for-byte (invalid TOML), `[stats]` present, a free-text sig (`journal:payment-worker:restart-loop`), an uppercase/`0x` hex sig, 17-hex sig, duplicate sigs, empty `sigs`, `version=0`, name with `_`, `play_ref` with `..`/absolute/URL, an unknown key, an unknown table, `allowed_modules` with a glob, non-RFC3339 `created_ts`. Budget: 1000 artifacts parsed+validated ≤ 500 ms.
2. `canonical_test.go` — golden vector: `CanonicalBytes` returns exactly 662 bytes, `sha256 = b1ecc0e5…de895c`, `play_sha256 = b4ae5a5b…d0bddec`; invariance under comments, key reordering, inline-vs-table layout and whitespace; **non**-invariance under `sigs` reordering (different bytes → same-version conflict), under a changed `signer_key_id`, and under a one-byte play edit.
3. `verify_test.go` — the fixture verifies with the pinned public key/signature; wrong key → 002; unknown `signer_key_id` → 003; `enabled=false` → 003; `trust=local` key on a pulled artifact → 003; truncated/space-padded base64 → 002. Budget: 100 artifacts (parse + canonicalize + ed25519 verify) ≤ 200 ms; ed25519 verify alone ≤ 1 ms per artifact.
4. `version_test.go` — semver table: `0.1.0` vs `0.1.0` pass, `0.1.0-rc1` < `0.1.0` fail, `0.2.0` > `0.1.9` pass, `dev` → 004 `unstamped_binary`; downgrade → 007; equal-version different bytes → 006 + 013; local version allocation is monotonic across refused/pending/quarantined rows.
5. `dist_test.go` — a fixture bare repo (created with the same argv-only git helper) with three tags and four artifacts: tag mode selects the highest; branch mode records `ref_mode=branch`; unreachable source → 011 with the installed set unchanged; ref not found → 010; per-artifact isolation (one bad artifact refuses, the others install); no-op pull writes no `pulled` record; the git invocation log contains no `push|commit|merge|gc|prune|stash`.
6. `gate_test.go` — gate order is asserted by observing which code a fixture with two simultaneous defects returns (schema wins over signature, signature over floor, floor over canary, canary over approve, approve over conflict), and that every refusal path produced a `refused` record; refusal dedup: 10 pulls of a floor-blocked artifact produce 1 record with `count=10`.
7. `promote_test.go` — **AC-24**: draft from `inc_…` + brief, review by a human actor, promote, then three state roots (A local, B and C pull from the fixture repo) → a synthetic recurrence on B runs rung ②: assert zero `agent_run` records on B, one `play_run` with `source=skill:<sk_id>`, and `inc_8812` visible in B's ledger (provenance in the `installed` record and in the artifact). **AC-26**: `autonomy=full` + `auto_accept_enabled` + threshold 1 → ledger sequence detection → research → play → spawn → verify → promote → `candidate_drafted` → `candidate_reviewed(auto)` → `promoted` with zero `actor.kind=human` records, then kill-switch → the next stage refused (TROUBLE-LADDER-010) inside one stage. Also: threshold 2 requires two rehearsals; a play outside `auto_accept_modules` never auto-accepts.
8. `conflict_test.go` — two skills, one sig: higher version wins and the loser is named; equal version → both refused; hold while an A window is open (B not applied, incident advances a rung); hold expiry → 013; window failed ×2 → `demoted` + 013 + escalation.
9. `authorizer_test.go` — allowlist subset enforced at install (005) and at authorize (005); `source="skill:<sk_id>"` for an unknown id is refused; a registry-side call with `source="rule:<name>"` is unaffected (the hook only constrains skill sources); no artifact key can express a shell (a `shell` key → 001).
10. `stats_test.go` — increments, flush within 5 s, atomic rename, 014 on a read-only directory with the play still succeeding; an artifact containing `[stats]` is rejected so stats can never round-trip into the channel.
11. `boot_reverify_test.go` — a tampered installed artifact is quarantined at boot (002 + refusal record) and never applied; the process stays green on `/health` while `SkillsStatus.degraded` reports skills.
12. Integration (`test/skills_e2e_test.go`) — 200-artifact pull: install ≤ 2 s wall (excluding the fetch), RSS delta ≤ 8 MiB (steady RSS budget ≤ 80 MB per SPEC-TYPES §6.1), ledger records appended through the group-commit writer with no per-record fsync (regression: 512 rec/s per-line vs ≈ 515k rec/s amortized), and `pulled` count equals `installed + pending_review + refused` exactly.
13. `library_test.go` (§2b / §4.7a, v0.1.1b) — a fixture library under `t.TempDir()` with two valid skills and one malformed file:
    - the valid pair parses to their frontmatter values (name, description, version) and their steps in order; `Plays` returns them in name order with `source = "skill-local:<name>@<version>"` and one task per step;
    - the malformed file is refused with the file, line and reason while the other two still load (per-file isolation), and the refusal is one `library_refused` record;
    - the subset is falsified: a tab indent, a duplicate key, a deeper nesting, a `[[…]]` header, a list-of-lists and an unterminated frontmatter each yield **TROUBLE-SKILLS-001**, never a silently shorter step list (assert the parsed step count is 0 for the refused file);
    - the never-arbitrary-code rule: a step key named `shell`/`exec`/`cmd`/`command`/`script`/`eval`/`run`/`interpreter` is **001** with `reason=unknown_key`, and a body carrying a fenced `bash` block executes nothing (a fake runner records zero calls);
    - `RunStep` under `check_mode` calls the fake runner's `Check` exactly once and `Apply` zero times; under `apply` it calls `Apply` once; an unknown name/step/mode and a module outside the registered set refuse with **TROUBLE-SKILLS-013** and zero runner calls;
    - `local_enabled = false` (the shipped default) reads nothing: zero `skill` records, zero runner calls, empty `Plays`;
    - a group- or world-writable `local_dir` refuses at construction with `reason=dir_writable`; a missing dir with `local_enabled = true` refuses with `reason=dir_missing`.

## 8. hilo impact

Packages/files created (all new, `github.com/trouble-agent/trouble`):

| File | Contents |
|---|---|
| `internal/skills/artifact.go` | `LoadArtifact`, strict decoder, field rules, `CanonicalBytes`, `skillArtifact`, `canonicalProjection` |
| `internal/skills/verify.go` | `Verify`, `GateSignature`, `GateFloor`, `GateModules` |
| `internal/skills/dist.go` | `Puller` (git argv helper, staging, gates, atomic install, offline/backoff) |
| `internal/skills/promote.go` | `Promoter` (`Draft`, `Review`, `Promote`, local key, version allocation) |
| `internal/skills/resolve.go` | `Resolver` (`Match`, `Hold`, `RecordRun`), `conflict`/`hold` records, demotion |
| `internal/skills/authorize.go` | `authorizer` — the registry hook, implementing SPEC-06's `Authorizer` seam |
| `internal/skills/store.go` | `localIndex`, `installRow`, `canaryRecord`, `Stats`, `Status`, atomic persistence |
| `internal/skills/library.go` | `Library` (§2b), `LibraryConfig`, `LocalSkill`, `SkillStep`, `StepRunner`, the frontmatter subset parser, `Plays`, `RunStep` |
| `cmd/trouble/skills.go` | the `trouble skills …` CLI |
| `test/skills_e2e_test.go` | AC-24 / AC-26 end-to-end fixtures |

Fan-out (imports): `internal/types` (Skill, Play, Candidate, Record, Actor…), `internal/ledger` (append one
`skill` record per state change, group-committed), `internal/registry` (descriptor name lookup for the
allowlist check) — plus stdlib `crypto/ed25519`, `encoding/json`, `os/exec` (argv-only git). Fan-in: `cmd/troubled`
builds the `Puller`/`Promoter`/`Resolver`; `internal/ladder` calls `Resolver.Match`/`Hold` for sig resolution and
`RecordRun` after a skill-sourced verify; `internal/registry` receives the `authorizer` through the hook
interface and never imports this package; `internal/dashboard` reads `skill` records from the ledger index only.
No package imports `internal/skills` from `internal/types` (one-way dependency), so no cycle exists.

Blast radius: greenfield repo `~/trouble`; no fleet repository, board, unit, or path is referenced, and
every path/interval/host id in this spec is configuration with the default `examples/skills/*` treated as
examples (non-negotiable #1). The only host-visible new surfaces are the `skills-local/` subtree under the state
root, one `git` subprocess on a configured schedule, and the `trouble skills` CLI.
