# SPEC-08 — flow: board-jsonl row, task-router driver, hot-fix spawn (trouble v0.1)

Spec: SPEC-08
Area prefix: TROUBLE-FLOW
Package: internal/flow
Consumed types: BoardRow, BoardEvent, FileTaskRequest, FileTaskResult, FlowDriver, FlowProject, FlowTimelineStep, RouterConfig, ForemanBrief, SpawnRequest, HotfixLease, Promotion, FlowConfig, HotfixConfig, DriverHealth, IssueRef, SpoolEntry, Record, RecordKind, Sig, Duration, Prefix, Actor, Origin, Severity, Incident, AutonomyGates, Evidence, VerifyResultKind, Rule, ResearchOutcome, DoNotTouch, ToolCall, Descriptor, Diff, DiffEntry, IdempotencyClass, ErrorClass
Local types: rowStyle, boardIndex, leaseKey, spawnKey, budgetClock, recordSink, issueDesk, briefSource, moduleRegistrar
ACs: AC-9, AC-19, AC-21, AC-22, AC-26
PRD: §08, §11, §12

## 1. Purpose

`internal/flow` turns a finding into work in a work system, and — for a confirmed code bug on an
enabled host — into a running foreman inside an isolated git worktree. It owns PRD §08 (flow control,
the hot-fix lane, the timing-authority exception), the `flow` and `spawn` record kinds
(SPEC-INDEX §3.4), and the error range TROUBLE-FLOW-001..019. Four obligations:

1. **File (AC-9).** One board row per (sig, board) through one of two first-class drivers:
   `board-jsonl` (direct append; works when the router is down, and the only driver the hot-fix lane
   uses) and `task-router` (the standing doctrine for normal, non-urgent filing).
2. **Detail the row field-for-field (AC-9, AC-19)** — the fleet board schema plus the four trouble
   extensions (`sig`, `inc`, `issue_refs`, `repo`), so a hot-fix timeline filed → foreman → verify →
   promote is reconstructible from the ledger and the board alone.
3. **Spawn (AC-21).** On an enabled host, for an allowed repo, at or above the hot-fix severity
   threshold, request a spawn through the scheduler's admission path (`router_spawn`) inside the 60 s
   trigger→spawn budget. trouble is a **client** with a priority class, budget accounting and a
   one-fix-per-sig lease; it never hand-runs `git worktree add` on a repo the scheduler owns.
4. **Promote (AC-21, AC-26).** After SPEC-05's verify window, promote (merge through the repo's own PR
   policy) or discard the worktree, and capture the remedy as a skill candidate.

**Design constraint (non-negotiable #3):** every flow action is a registry tool call (`flow.*`,
SPEC-06 §6) — schema'd, dry-run-able, audited; no stage opens a shell. Paths, boards, repos, endpoints
and model pins are configuration, and fleet material appears only in `examples/` (non-negotiable #1).

## 2. Interface

```go
// internal/flow — package flow. FlowDriver (the two-implementation driver contract) and the request
// and result types it carries are defined once, in §3.15; never repeated here.
func NewFlow(cfg FlowConfig, autonomy AutonomyGates) (*Flow, error)
func (f *Flow) Start(ctx context.Context) error            // boot reconcile + registration probes
func (f *Flow) Stop(ctx context.Context) error             // flush queue + release leases
func (f *Flow) File(ctx context.Context, inc Incident, row BoardRow) (FileTaskResult, error)
func (f *Flow) Comment(ctx context.Context, row BoardRow, body string) (BoardRow, error)
func (f *Flow) Spawn(ctx context.Context, req SpawnRequest) (SpawnRequest, error)
func (f *Flow) SpawnState(spawnID string) (SpawnRequest, error)
func (f *Flow) Promote(ctx context.Context, spawnID string, decision string) (Promotion, error)
func (f *Flow) Rollback(ctx context.Context, spawnID string, reason string) (SpawnRequest, error)
func (f *Flow) Timeline(ctx context.Context, inc string) ([]FlowTimelineStep, error) // dashboard AC-19
func (f *Flow) Probe(ctx context.Context) error             // scheduler registration probe, 5m cadence
func (f *Flow) Reconcile(ctx context.Context) error         // boot: meta vs router vs board
```

Collaborators are injected by the wiring layer (§4) through four **package-private** consumer-side
interfaces declared in `driver.go` (`recordSink`, `issueDesk`, `briefSource`, `moduleRegistrar`), each
satisfied by the concrete subsystem. Package-private by design: they are this package's view of its
collaborators, so no shared type is minted for a seam and the one-way dependency direction
(subsystems → `internal/types`) holds.

`FileTask` is invoked by the ladder through the registry module `flow.create_task` (SPEC-06 §6);
`Spawn` through `flow.spawn_foreman`; `Promote`/`Rollback` through `flow.promote` / `flow.rollback`;
`Comment` through `flow.comment_task`. Module descriptors: scopes `flow:write`, `flow:spawn`,
`flow:promote`; `CheckMode=true` for all four; idempotency classes `convergent` (create_task,
comment_task), `once` (spawn_foreman, promote, rollback); `timeout_s` 10 / 30 / 15 / 15.
Each descriptor is a `Descriptor` value (SPEC-TYPES §3.8) registered at boot; every invocation is a
`ToolCall` audit record, and a module error carries an `ErrorClass` from
`{transient, permanent, policy_refused}`.

CLI (clients of the same code paths; the CLI never bypasses a gate):

```
trouble flow file --inc <inc_id> [--driver board-jsonl|task-router] [--dry-run]
trouble flow list [--repo <path>] [--sig <sig>] [--status todo|blocked|done]
trouble flow comment --task <tsk_id> --body-file <path>
trouble flow hotfix status [--json]            # lanes, leases, queue depth, 60s budget hits
trouble flow spawn --inc <inc_id> [--dry-run]  # same gate chain as the automatic path
trouble flow promote <sp_ulid> [--discard]     # human decision; writes kind=flow + a Promotion
trouble flow reconcile [--prune]               # worktrees-meta vs router registry vs boards
trouble flow explain <sig>                     # which rows/leases/dispatches this sig produced
```

Dashboard surfaces this subsystem fills for AC-19, on SPEC-10 §2.1's route table (rows 3, 8, 14, 18 —
the routes are SPEC-10's, the data is `Flow.Timeline`): `/incidents/{id}` renders the incident story
(the `tsk_` board row, the `SpawnRequest`, the `Promotion` and the PR/worktree links) in the order
filed → foreman picked up → patch → verify window → promoted; `/partials/incidents/{id}/timeline`
streams the same steps as ledger records; `/health.json` and `/partials/budget` carry
`RuntimeWatermarks.Worktrees` and the `spawn_pending` count.

## 3. Data model

### 3.1 The board row, field-for-field

`BoardRow` (SPEC-TYPES §3.10) is the only row shape. Field fill rules — who writes what, from where:

| Field | Type | Value at write | Afterwards |
|---|---|---|---|
| `id` | string | `tsk_` + ULID allocated from the board's max id (§3.2) | immutable; trouble never rewrites it |
| `title` | string | `hotfix: <culprit/short message> (<sig>)`; non-hotfix: `<severity>: <title> (<sig>)`, scrubbed, ≤200 chars | immutable |
| `status` | string | `todo` (review_mode=auto/full) · `blocked` (review_mode=review, awaiting approval) | owner only (board/repo owner, router) |
| `priority` | string | severity map §3.3: critical→`P0`, high→`P1`, medium→`P2`, low→`P3`, info→`P3` | owner only |
| `complexity` | string | severity map §3.3: critical→`L`, high→`M`, medium→`M`, low→`S`, info→`S`; per-rule override | owner only |
| `depends_on` | []string | `[]`; set from `flow.depends_on` when a project pins a parent/epic row | owner only |
| `blocks` | []string | `[]` always (trouble does not declare what it blocks) | owner only |
| `primary_model` | string | `[flow.hotfix] models.primary_model` default `""` | router wins |
| `primary_provider` | string | `models.primary_provider` default `""` | router wins |
| `fallback_model` | string | `models.fallback_model` default `""` | router wins |
| `fallback_provider` | string | `models.fallback_provider` default `""` | router wins |
| `reasoning` | string | `trouble <version> flow; sig <sig>; inc <inc>; severity <sev>; driver <driver>; <rung chain>` | immutable |
| `capability_tags` | []string | `["hotfix","trouble"]` for hot-fix rows, `["trouble"]` otherwise, plus `flow.extra_tags` | router wins |
| `worker_status` | string | `""` | router/worker only |
| `dispatched_at` | string | `""` | router only |
| `completed_at` | string | `""` | owner only |
| `sig` | string | canonical sig (SPEC-TYPES §6.3) — the trouble extension and the dedup key | immutable |
| `inc` | string | `inc_` + ULID of the incident that filed it | updated only in comments |
| `issue_refs` | []string | `iss_` ids returned by SPEC-09 `EnsureBySig` (may be `[]` when the issue desk is degraded) | appended in comments |
| `repo` | string | absolute repo root for hot-fix rows, `""` for sensor-only rows | immutable |

**Single-writer per row (invariant).** trouble is a **create-only** writer of a board: it appends one
task row + one `task_created` event per (sig, board) and never mutates a row it did not create and
never mutates a row after creation. Every subsequent transition (`status`, `worker_status`,
`dispatched_at`, `completed_at`) belongs to the component that owns the repo/board. This is why the
routine `boardctl`-style `update` path is deliberately **not** used by trouble: a second writer with a
full-file rewrite over a git-tracked JSONL board is the fleet's documented dirty-board class.

Row JSON exactly as appended (fixture for the round-trip test):

```json
{"id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","title":"hotfix: queue wedge in payment-worker (sentinel:sha256v1:9f2c1d3e4b5a6c7d)","status":"todo","priority":"P1","complexity":"M","depends_on":[],"blocks":[],"primary_model":"","primary_provider":"","fallback_model":"","fallback_provider":"","reasoning":"trouble 0.1.0 flow; sig sentinel:sha256v1:9f2c1d3e4b5a6c7d; inc inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE; severity high; driver board-jsonl; record→play→agent","capability_tags":["hotfix","trouble"],"worker_status":"","dispatched_at":"","completed_at":"","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","issue_refs":["iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK"],"repo":"/srv/src/payment-api"}
```

The companion event row (second file of the two-file contract):

```json
{"id":4107,"type":"task_created","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","ts":"2026-09-16T09:14:05.221Z","actor":"troubled@0.1.0","detail":{"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"}}
```

`BoardEvent.ID` = `MAX(id)+1` across the board's event file — the fleet's own allocation rule, and the
reason `BoardEvent` is typed rather than inlined.

### 3.2 The writer recipe (append-only, never rewrite)

1. **Resolve the board.** `flow.projects.<name>.board_path` → the board directory (it holds
   `tasks.jsonl`, `events.jsonl`). The path is resolved once at boot with `filepath.Clean` +
   symlink resolution and cached with a `(dev, inode, mtime, size)` stamp; a change re-reads the head.
2. **Index.** `boardIndex` holds, per board: max ULID of `tasks.jsonl`, max numeric event id of
   `events.jsonl`, the detected `rowStyle` (compact vs spaced, key order, escaping), and the
   sig→task-id map. Cold build is a single streaming pass; warm lookups are map hits.
3. **Allocate the id.** `tsk_` + ULID seeded so the new suffix is strictly greater than the board's
   max existing suffix (compare the 26-char Crockford suffix; seed = max(now, max_suffix+1)). Two
   concurrent allocations inside one process are serialized by the package mutex; a computed id that
   already exists (foreign writer raced) retries up to 8 times, then TROUBLE-FLOW-004.
4. **Serialize the row.** Key order = the §3.1 schema order; the byte style follows `rowStyle` so the
   git diff is one line.
5. **Append.** One `O_APPEND` write of exactly one line, `\n`-terminated, then `fsync` on the file
   descriptor. **Append-only: never rewrite, never reorder, never compact a board file** (compaction
   belongs to the board owner). A partially written line is detected by the boot scan and reported as
   TROUBLE-FLOW-003 (the board's tolerant reader law applies to reads, never to writes).
6. **Two-file order.** `tasks.jsonl` first, then `events.jsonl` (MAX(id)+1). Both writes are inside
   one flow unit; the ledger `flow` record lists both ids, and a boot reconcile that finds a task row
   without its `task_created` event re-appends the missing event exactly once (idempotent by
   `(task_id, type)`).
7. **Post-validate.** Always: in-process schema validation of the appended bytes (mandatory, cannot be
   disabled). When `flow.validate_cmd` is set (the board's own validate command, e.g. a `boardctl`-class
   CLI in `examples/`), it is executed with a 10 s timeout in the board directory:
   - rc=0 → `FileTaskResult{Valid:true}`;
   - rc=1 and the finding names our new id → TROUBLE-FLOW-003, row quarantined by id, issue desk
     comment on the incident, dashboard banner;
   - rc=1 with findings that do not name our id → recorded as a pre-existing board finding in the
     `flow` payload (`validate_findings`), not an error for this file;
   - rc=2 (usage/board-not-found) or a timeout → TROUBLE-FLOW-002.
8. **Never file into an unregistered project.** Before step 3 the registration proof of §3.5 must hold,
   else TROUBLE-FLOW-018 and **no bytes are written**: a row in a project the scheduler does not tick
   is a row nobody works — the fleet's inert-feature class, and the exact lie a green board tells.

### 3.3 Severity → priority, complexity, and the review_mode gate

| Severity | Priority | Complexity | Hot-fix eligible (default) |
|---|---|---|---|
| `critical` | `P0` | `L` | yes |
| `high` | `P1` | `M` | yes |
| `medium` | `P2` | `M` | no (below threshold) |
| `low` | `P3` | `S` | no (below threshold) |
| `info` | `P3` | `S` | no (below threshold) |

`review_mode` (FlowConfig, overridable per project, then per rule — precedence project > rule > global):

| Mode | What is written | When |
|---|---|---|
| `auto` | task row `status="todo"` + event row, immediately | default |
| `review` | task row `status="blocked"` + event row; the row is visible and inert until approved; approval appends a `review_approve` event naming the approver (`Actor.Kind=human`), never a row rewrite | any severity; mandatory for `critical` when a project sets `flow.review_critical=true` |
| `never` | nothing on the board; the `flow` record carries `decision="skipped"`, `reason="review_mode_never"` | flow detection stays on, filing is off |

`review_mode=never` is not an error (the driver is not disabled); `[flow] driver="none"` is
TROUBLE-FLOW-001 and refuses every filing call. A duplicate rule that resolves to `never` while
`hotfix=true` is a config contradiction and is refused at load (TROUBLE-LIFECYCLE-001).

### 3.4 Dedup: one row per (sig, board) — comment, never a duplicate

The dedup core (SPEC-01) gives one sig space; flow enforces it on the board:

1. Lookup `boardIndex.sig → task_id`. Hit → **comment/cross-ref path**: `CommentTask` appends a
   `task_comment` event to `events.jsonl` carrying the new `inc`, the rung reached, the recurrence
   count and the issue `Comment` (SPEC-09) body; `issue_refs` on the comment lists every issue for the
   sig. No second row is ever appended (AC-22's "one board row (comments on recurrence)").
2. Miss → if the same sig has a row on a **different** board, the reuse decision is per project: when
   both projects share a repo root the older row wins and the newer board receives a cross-ref comment;
   when they do not share a repo root, each board gets its own row and both rows carry the same `sig`
   (the ledger `flow` record's `dup_of` names the other row).
3. Any failure to attach the comment/cross-ref — row id absent from the index, the referenced row was
   rewritten by the owner so the id no longer resolves, or the sig row belongs to a board that is
   currently unreadable — is TROUBLE-FLOW-019 and is recorded with the *intended* row id. The result
   is never "write a second row to be safe".
4. In-memory guard: a per-sig in-flight set closes the write-then-read race between two incidents
   detected inside the same second (the D-Bus/PropertiesChanged + JobRemoved + journald three-arrival
   case). One writer, one row, one lease.

### 3.5 The registration proof (TROUBLE-FLOW-018)

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
```

Probe: `GET {flow.scheduler.endpoint}/projects` with the token from the 0600 token file
(`flow.scheduler.token_file`), every `flow.registration_probe_interval` (default `5m`) and on every
boot. Three checks, all mandatory:

- the configured `scheduler` name is present in the response;
- the response's `enabled` is `true` (a project present but disabled is not worked);
- the response's board path equals `FlowProject.BoardPath` after `Clean` + symlink resolution.

Any mismatch → `Registered=false`, TROUBLE-FLOW-018 recorded once per state change (edge-triggered,
not per event), a dashboard banner, and filing refuses. A probe that cannot reach the scheduler is
**not** a registration failure: it is TROUBLE-FLOW-005-class degradation (transient), the last known
`Registered` value holds for `flow.registration_stale_max` (default `1h`), and after that filing
refuses with TROUBLE-FLOW-018 (`reason="proof_stale"`) — an unproven project is not filed into.

### 3.6 The task-router driver

```go
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
```

HTTP mode: `POST {endpoint}{dispatch_path}`, `Content-Type: application/json`,
`Authorization: Bearer <token>`. CLI mode: `{cli_path} --json dispatch` with the identical JSON on
stdin, JSON on stdout. The wire payload is the same in both modes:

```json
{"idem_key":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","board_path":"/srv/src/payment-api/.board","repo":"/srv/src/payment-api","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","title":"hotfix: queue wedge in payment-worker","priority":"P1","complexity":"M","capability_tags":["hotfix","trouble"],"severity":"high","host_id":"7f3a91c2d4e5b607","submitted_ts":"2026-09-16T09:14:05.221Z"}
```

- **Idempotency** is the task id: `idem_key == task_id`. The router must treat a repeated `task_id` as
  accepted-and-idempotent. trouble additionally never dispatches a row it has not just allocated, and
  never dispatches twice for one (sig, board) because §3.4 returns before the call.
- **2xx** → dispatched. **409/`duplicate:true`** → treated as success (idempotent replay after a
  spool flush). **429** → transient, honours `Retry-After`. **4xx other** → permanent
  (TROUBLE-FLOW-005, recorded with the body's error string, no retry). **5xx, timeout, connection
  refused** → transient: immediate retry × `retries` with 1 s/3 s/9 s backoff, then a `SpoolEntry`
  (kind `spawn`, `IdemKey=task_id`) for replayed re-dispatch (bounded drop-oldest with a ledger note)
  → TROUBLE-FLOW-005 + a `flow` record with `dispatch_state="spooled"`. That entry's store, its bounds
  and the loop that drains it are §3.9a: the flow owns both, and `dispatch_state="spooled"` is written
  only when the entry landed on the queue §3.9a describes.
- **Driver choice.** `board-jsonl` is the hot-fix lane's driver (the row must exist even when the
  router is down: the fleet's own `spawn_pending` path is what carries the work forward) and is the
  default for any incident at or above `high`. `task-router` is the doctrine for normal filing
  (`medium`/`low`/`info`), where router-side dedup, queueing and ordering are worth the extra hop.
  `driver="none"` → TROUBLE-FLOW-001. Both drivers produce the identical `BoardRow`; only the write
  path differs.

### 3.7 The hot-fix lane — configuration in full

```toml
[flow]
driver = "board-jsonl"                 # board-jsonl | task-router | none
review_mode = "auto"                   # auto | review | never
board_path = "/srv/src/payment-api/.board"   # default project's board directory
id_prefix = "tsk_"                     # row id prefix; ULID suffix is always allocated from the board max
validate_cmd = "boardctl -C /srv/src/payment-api validate"  # board's own validate command ("" = in-process only)
registration_probe_interval = "5m"
registration_stale_max = "1h"
depends_on = []                        # default depends_on for every row this host files
extra_tags = []
scheduler_endpoint = "http://127.0.0.1:9090"
scheduler_token_file = "/etc/trouble/scheduler.token"

[flow.router]
mode = "http"                          # http | cli
endpoint = "http://127.0.0.1:9091"
dispatch_path = "/dispatch"
cli_path = "/usr/local/bin/router"
token_env = "TROUBLE_ROUTER_TOKEN"
token_file = "/etc/trouble/router.token"   # 0600, never argv, never a URL
timeout = "10s"
retries = 3

[flow.projects.payment-api]
name = "payment-api"
repo = "/srv/src/payment-api"
board_path = "/srv/src/payment-api/.board"
enabled = true
hotfix = true
scheduler = "payment-api"

[flow.hotfix]
enabled = false                        # host master gate; OFF by default (PRD §11 guardrail)
allowed_repos = ["/srv/src/payment-api"]
unit_repo_map = { "payment-worker" = "/srv/src/payment-api" }
foreman_spawn = "router_spawn"         # the only value in v0.1
priority_class = "hotfix"              # must exist in the scheduler's admission registry
verify_window = "10m"
promote = "human"                      # human | auto-after-verify
max_concurrent = 2                     # per host, worktrees only; hard cap 4 (load refuses more)
min_free_disk_gb = 10
worktree_base = ".worktrees"           # relative to the repo root; absolute/.. /temp paths refused
worktree_exempt = []                   # huge-checkout repos (see examples/)
lease_ttl = "30m"
spawn_ack_timeout = "5s"               # router_spawn acknowledgement budget
spawn_worktree_timeout = "30s"         # worktree must exist within this after acceptance
max_attempts = 5
min_severity = "high"                  # hot-fix threshold; below it → TROUBLE-FLOW-008
models = { primary_model = "", primary_provider = "", fallback_model = "", fallback_provider = "" }
capability_tags = ["hotfix", "trouble"]
```

Gate chain, evaluated in this order, first failure wins and is recorded:

1. `[flow.hotfix] enabled` → else TROUBLE-FLOW-006.
2. `AutonomyGates.AllowSpawn` → else the row is drafted/written and **no spawn** (shadow mode,
   SPEC-05 §3 gate matrix, TROUBLE-LADDER-011 recorded when an explicit grant is required).
3. severity ≥ `min_severity` → else TROUBLE-FLOW-008.
4. `Rule.Hotfix == true` → else TROUBLE-FLOW-008 (`reason="rule_not_hotfix"`).
5. `repo ∈ allowed_repos` (Clean + symlink-resolved equality) and `repo == projects.<p>.repo` →
   else TROUBLE-FLOW-007.
6. registration proof holds (§3.5) → else TROUBLE-FLOW-018.
7. one-fix-per-sig lease free (§3.10) → else TROUBLE-FLOW-009.
8. `max_concurrent` free slots → else the request waits on the per-repo mutex with
   `flow.hotfix.mutex_wait` (default `5s`) → TROUBLE-FLOW-013 on timeout.
9. disk gate: `free_gb ≥ max(min_free_disk_gb, 3 × repo_checkout_gb)` → else TROUBLE-FLOW-012.
10. exempt list → a hit is TROUBLE-FLOW-014 with `worktree_mode="owner_serialized"`: the row and the
    spawn request are still filed, `worktree=""`, and the repo owner's own queue manages the tree.

### 3.8 Worktree discipline

- **Convention adopted, not minted:** `<repo>/<worktree_base>/<task-id>` with `worktree_base=".worktrees"`
  → `<repo>/.worktrees/<tsk_id>`. trouble never invents a second layout, never uses an absolute or
  temp-directory base, and never places a worktree under `/tmp` (a worktree under `/tmp` lands in a
  namespace a spawned foreman cannot see once a unit sets `PrivateTmp=yes`, and `/tmp` is a disk-fill
  waiting for a flood — both measured fleet classes).
- **The repo's owner creates it.** `SpawnRequest.Worktree` is *reported by the router*, not computed by
  trouble: trouble sends `<repo>`, `<worktree_base>` and the task id, and the router_spawn path that
  owns the repo performs `git worktree add`, submodule init, and the untracked-input copy (`.env`,
  dependency dirs) that a worktree lacks — the measured cause of "the fix doesn't build" false
  negatives. trouble never executes a git command on a repo the scheduler owns.
- **Per-repo mutex.** Acquisition order: `flow.hotfix.max_concurrent` (host) → per-repo lock
  (`<state_root>/worktrees-meta/<repo-slug>.lock`, `O_CREAT|O_EXCL`, holder = spawn id, TTL = lease
  TTL, stale-lock detection by mtime heartbeat every 15 s). Concurrent worktree creation contends on
  the shared `.git/config.lock` (a measured, well-documented parallel-agent failure class); the mutex
  serializes creation and removal per repo, never the foreman's own work inside its worktree.
- **Foreman rules (in the brief, enforced by the router):** inside a foreman worktree — no `git fetch`,
  no `git gc`, no `git prune`, no `git worktree` mutation. Those run in the main checkout only, by the
  repo's owner. Two foremen running repack/branch operations on a shared object store can corrupt it;
  the ban is unconditional.
- **Prune and reap.** `flow.reconcile` runs at boot and on `trouble flow reconcile`: read
  `worktrees-meta/*.json`, compare with the router's worktree registry, and (a) request prune via the
  owner for terminal spawns whose directory is gone (a hand-deleted worktree stays `prunable` in the
  registry), (b) ask the owner to reap the tree for spawns whose lease expired with no state change,
  (c) never delete a directory trouble does not own. Boot reconcile emits one `spawn` record per
  action; orphan count is surfaced in `/health.json` `runtime_watermarks.worktrees`.
- **Cost is a full checkout** of the tracked tree: measured on comparable repos at 68 MB, 242 MB,
  464 MB, and 8.9 GB. Hence the disk gate, the concurrency cap, and `worktree_exempt` for the huge
  ones (`examples/config.toml` carries the exempt entry for a large warehouse/index checkout).

### 3.9 Spawn state machine, durable queue, bounded retry

```
requested ──ack──► accepted ──worktree within spawn_worktree_timeout──► leased ──► released
    │                  │                                                   ▲
    └──no ack──► spawn_pending ──retry──► accepted ──► leased               │
                        │                                                   │
                        └──budget exhausted──► failed (durable queue holds it) ──► promoted|discarded
```

- `requested` is written **before** the router call; `spawn_pending` is written whenever the call does
  not produce an acknowledgement inside `spawn_ack_timeout`, and a `SpoolEntry` (kind `spawn`) is
  enqueued with `IdemKey = task_id` (§3.9a names that store and the loop that drains it). **A spawn
  failure is never silent**: the failing path is exactly
  the path the fleet walks when it is sick, and a silent degradation there is the worst outcome for
  this feature.
- Bounded retry: attempts 1..5 at 5 s / 15 s / 45 s / 2 m / 5 m; after attempt 5 the entry stays in the
  flow's durable queue (§3.9a, bounded drop-oldest-with-ledger-note), the `spawn` record stays
  `spawn_pending`, and the incident escalates one rung with TROUBLE-FLOW-011. The reconciled
  `spawn_pending` count is the first number of the dashboard's budget panel (SPEC-10 §2.1 row 18).
- `failed` (permanent router refusal: repo rejected by the admission gate, unknown priority class) is
  terminal for the spawn: TROUBLE-FLOW-010 with `reason`, no retry, the incident continues on the
  ladder — the row remains and the owner works it through its normal queue.
- **60 s trigger→spawn budget (AC-21)** — measured, not aspirational, and recorded on every spawn
  record as `trig_to_spawn_ms`:

| Stage | Budget |
|---|---|
| trigger → event recorded (≥2 s PSI sampling window, ±wake) | ≤ 3 000 ms |
| ladder record → play → agent rung, flow stage reached | ≤ 1 000 ms |
| board append + two-file write + post-validate | ≤ 500 ms |
| lease grant + mutex acquisition | ≤ 1 000 ms |
| `router_spawn` call and acknowledgement | ≤ 5 000 ms |
| worktree present and foreman leased | ≤ 30 000 ms |
| **total** | **≤ 60 000 ms** (18 s of slack; the AC fails loudly above 60 000 ms) |

Above budget: the run still completes, the `spawn` record carries `budget_exceeded=true`, a `flow`
record is written with `stage="budget"`, and the dashboard banner + escalation fire. Nothing about the
budget is allowed to be an invisible miss — AC-21 is measured per spawn, not asserted once.

**Priority class and budget accounting.** `priority_class` is sent on every spawn request and must be
present in the scheduler's admission registry (probed at boot; absent → TROUBLE-LIFECYCLE-001 at load,
because an unadmitted class degrades silently to the default lane). trouble records the class and the
attempt count in the `spawn` payload, so hot-fix work competing with fleet foremen for slots is
auditable, and it never bypasses the scheduler's `max_concurrent`.

### 3.9a The flow-owned dispatch queue and its own replay loop

§3.6 and §3.9 hand every dispatch that cannot be delivered to a `SpoolEntry` (kind `spawn`,
`IdemKey = task_id`) for replayed re-dispatch. This section says **where that entry lives, which loop
drains it, and what the record may claim about it** — the seam where SPEC-08 §3.6/§3.9 meets SPEC-09
§3.6/§3.7.

- **The queue is the flow's own, and so is the drain.** The store is
  `<state_root>/spool/flow/spawn/<ev_ULID>.json` (SPEC-12 §3.2's `spool/` subtree, in a subdirectory no
  other subsystem lists). The loop is `Flow.Run` (`internal/flow`), which drains it on its own cadence
  beside the registration probe. SPEC-09's desk spool is **not** this queue and must not be wired as it:
  its replay walks the configured DRIVER names (`github`/`duckbrain`), so no loop lists a foreign tree;
  its `DecodePayload` accepts the desk's own operation shape, so a flow dispatch payload is undecodable
  there; and the desk takes no foreign payload at all — it exposes no entry point for one (SPEC-09 §3.7),
  so a dispatch left there could not even be offered for replay. A dispatch queued there is durable in
  name only, which is exactly the claim this section exists to make impossible.
- **Format.** One JSON object per file, one entry per file, mode `0600`, written atomically: temp file in
  the SAME directory → `fsync` file → `rename` → `fsync` directory, so a torn entry can never be
  replayed. The entry is `SpoolEntry` (SPEC-TYPES §3.14) with `Kind = "spawn"`; its `payload` is the
  **sealed dispatch envelope** of §3.9b — `{"sealed":true,"source":"dispatch"|"spawn","task_id","inc",
  "sig","payload":{… the §3.6 wire body …}}` — already scrubbed (SPEC-02), so the queue is not a leak
  surface. The directory is `0700` and is proved writable when the store is built: a store that cannot be
  built is a recorded refusal and leaves the flow with no queue rather than an in-memory illusion.
- **Bounds.** Every bound is an operator-tunable registry key — the five `flow.*` keys of SPEC-12 §3.1d
  (`flow.spool_max_entries`, `flow.spool_ttl`, `flow.spool_max_attempts`, `flow.replay_every`,
  `flow.replay_batch`) — and every bound has a default taken from the number this suite already pins, which
  is the value a host that declares no `[flow]` table runs. Every eviction is recorded: the bound costs
  evidence, never silence. The composition root resolves the five keys once and hands the SAME resolved set
  to both halves, the store (`flow.NewSpool`) and the flow (`flow.NewFlowWithBounds`), so neither the queue's
  limits nor the loop that drains it can end up on the compiled constants while an operator's key sits
  unread. A key that is not strictly positive (zero, negative, `0s`, or a duration string that does not
  parse) is refused at resolution with TROUBLE-LIFECYCLE-001 naming the key, before the store is built: a
  resolved zero would be an unbounded queue, which is the posture this section exists to make impossible.

| Bound | SPEC-12 key | Default | Behaviour at the bound |
|---|---|---|---|
| `max_entries` | `flow.spool_max_entries` | 256 (the §3.9 in-memory bound) | evict the OLDEST entry — never the incoming one — and record the eviction |
| `spool_ttl` | `flow.spool_ttl` | 72 h (SPEC-09 §3.7's TTL) | drop the entry with `{"stage":"dispatch","decision":"failed","dispatch_state":"dropped","drop_reason":"ttl"}` |
| `max_attempts` | `flow.spool_max_attempts` | 5 (§3.9's bounded retry budget) | drop the entry with `drop_reason:"attempts"` |
| an undecodable `payload` | — (not a bound) | — | drop the entry with `drop_reason:"corrupt"` (a shape this build cannot read is not a retryable failure) |
| replay cadence | `flow.replay_every` | 5 s | the drain tick; a queue with nothing due costs one directory listing |
| `replay_batch` | `flow.replay_batch` | 100 entries | entries per drain |

- **Replay rules.** Order is `(next_try_ts asc, ts asc, id asc)`. One in-flight dispatch per `IdemKey`,
  so a replay can never race the original attempt or a second replay of the same entry. Re-dispatch
  delivers the entry's **sealed payload** (§3.9b) through the same §3.6 attempt sequence with the **same**
  `IdemKey` — the body is never rendered a second time — which is what makes a replay idempotent: the
  router's own `task_id` dedup (409/`duplicate:true` = accepted) is the guard, so no second spawn is
  created for work the first attempt already placed. On success the entry is **deleted**;
  on failure the attempt is counted **against that entry** (never a fresh entry — a failed replay that
  re-enqueues would grow the queue on every attempt) and `next_try_ts` is shifted along §3.9's schedule
  (5 s / 15 s / 45 s / 2 m / 5 m).
- **Every state is a ledger record**, each carrying `inc` + `sig` + `task_id`: request → retries
  exhausted → `dispatch_state="spooled"`; replay success → `"replayed"` + a `spawn` record at `leased`
  + the §3.9b delivery fields (`payload_origin`, `payload_sha256`, `delivered`, and `diverged_fields`
  when the live inputs have moved); replay failure → `"replay_failed"` with `attempts`, `next_try_ts`
  and `payload_origin`; drop → `"dropped"` with `drop_reason` **and** a `spawn` record naming the state.
  Drops are written as `flow` + `spawn` records: `gap` is not this subsystem's kind (SPEC-INDEX §3.4).
- **Durability is claimed only when it exists.** `dispatch_state="spooled"` is written **only** when the
  entry landed on a queue this subsystem replays. A sink that can be written but not replayed (an
  `Enqueue`-only adapter), a store that refused the write, and a flow with no store at all are each
  recorded as `dispatch_state="unspooled"` or `"spool_failed"` with a `reason` **and** a `coupling` field
  naming `SPEC-08 §3.6/§3.9 × SPEC-09 §3.6/§3.7`, so an operator reads the coupling from the record
  instead of reconstructing the wiring. This is the AC1 choice: a dispatch that cannot be delivered is
  EITHER durably queued in a place that is actually replayed OR refused with a code and a record that
  names the coupling — never a silent drop, and never a durability claim the loop cannot honour.
- **The in-memory queue is the dashboard view, not the durable one.** `f.queue` (≤256,
  drop-oldest-with-a-ledger-note, §3.9) remains the reconciled `spawn_pending` count the budget panel
  reports (SPEC-10 §2.1 row 18). It does not survive a restart; the store above does, and that is the
  half that keeps a pending spawn alive across one.
- **Wiring.** The composition root builds the store (`newFlowSpool`, under the resolved `state_root`) and
  passes it as `flow.Deps.Spool`; `Flow.SetDeps` adopts it only if it can be replayed, and a nil store is
  passed as a nil INTERFACE — a typed-nil pointer satisfies the replay assertion behind the interface and
  panics on the first dispatch, so it is refused as "no queue" rather than adopted. The daemon starts the
  loop beside `Flow.Start` (`go Flow.Run(ctx)`). The desk's own spool and replay semantics are untouched:
  this section adds a queue, it does not change SPEC-09.
- **No codes of its own.** §3.9a introduces no new `TROUBLE-FLOW-*` code. It reuses TROUBLE-FLOW-005 for
  a dispatch that could not be delivered, TROUBLE-FLOW-011 for a spawn still pending past the retry
  budget, TROUBLE-FLOW-010 for a failed replay attempt, and TROUBLE-LIFECYCLE-015 for a bound-driven
  drop (the cross-area code §5 already lists for the spool budget).
- **Test.** `internal/flow/spool_test.go` asserts both halves of AC1 and the whole drain, with no
  wall-clock budget except the one loop test: an `Enqueue`-only sink produces EXACTLY ONE outcome — an
  honest `unspooled` record naming the coupling — and zero `spooled` claims in the run; a typed-nil store
  is not adopted; with the flow's own store the parked spawn is one `0600` file under the §3.9a path and
  the record says `spooled`; a drain re-dispatches with the SAME idem key against a real HTTP router,
  deletes the entry on success, does nothing on a second pass, and leaves an empty queue after a
  simulated restart; an entry written by one store instance is listed, decoded and replayed by a second
  one; attempts, `ttl`, `corrupt` and overflow each drop the entry with a record pair and zero `gap`
  records; overflow evicts the OLDEST entry and never the incoming one. §3.9b adds its own file,
  `internal/flow/replay_seal_test.go`. `internal/app/flow_spool_wiring_test.go`
  asserts the composition root: with the issue desk OFF (the shipped posture) `flowDeps` still hands the
  flow a queue it can replay, the wired sink writes one `0600` entry under the state root, and a store
  that cannot be built leaves the flow reporting "no queue" instead of claiming durability.
- **The bounds are proven where they are wired.** `internal/lifecycle/flow_bounds_test.go` pins the five
  SPEC-12 §3.1d keys (defaults, a half-specified `[flow]` table, flag > env > file > default, the explain
  rows, and the non-positive/unparsable refusals with the key named);
  `internal/app/flow_bounds_wiring_test.go` drives the composition root's own seams (`flowSpoolBounds`,
  `newFlowSpool`, `newFlowSubsystem`) and proves an operator's key is the bound in force on BOTH halves: four
  writes at `flow.spool_max_entries = 3` leave three entries and evict the oldest where the compiled default
  keeps four, and a two-hour-old entry is dropped with `drop_reason:"ttl"` under `flow.spool_ttl = 1h` where
  the compiled default never TTL-drops it. Reverting either half of the wiring — the projection or the
  flow's construction — makes that test fail, which is what makes it evidence rather than a restatement.

### 3.9b A replayed dispatch delivers its SEALED payload

§3.9a's entry is written for a dispatch that could not be delivered. What that entry CARRIES decides what
a later replay sends, and the queue outlives a config change, a board move and a rebuild: the payload is
therefore sealed at write time and re-delivered, not re-derived. This sub-§ states the rule, the fields a
replay may not invent, and what the ledger says about the two.

- **The payload is rendered ONCE, at write time, and sealed.** `wirePayload` is the only renderer of the
  §3.6 body; both write seams call it exactly once and store the result in the entry: `enqueueDispatch`
  seals the payload the failed attempt just sent, and the §3.9 `spawn_pending` path — which never reached
  the wire — renders the payload it *would* have sent and seals that. A replay unmarshals the sealed
  envelope and delivers the body it carries through the same §3.6 attempt sequence. Consequence: no
  config change, board move, registration flip or rebuild on another host retro-mutates a queued
  dispatch, so `board_path`, `title`, `priority`, `complexity`, `capability_tags`, `severity` and
  `host_id` describe the REQUEST that produced the entry rather than whatever the config says when the
  queue finally drains. Choosing this posture over documented re-derivation is deliberate: re-derivation
  is not merely hard to read in the ledger, it can dispatch a queued incident's work at a board the
  incident was never filed to.
- **Idempotency is untouched.** The stable fields are `idem_key`, `task_id`, `inc` and `sig` — sealed with
  the payload and re-checked against the entry's own `idem_key` — so the router's `task_id` dedup
  (409/`duplicate:true` = accepted) still makes a replay idempotent, and one in-flight dispatch per
  `idem_key` (§3.9a) still keeps two drains from racing. `submitted_ts` is the **seal stamp** (when the
  payload was rendered), not the moment of delivery; the delivery instant is the replay record's own
  timestamp.
- **Requested vs delivered is readable from the ledger.** A replay of a sealed entry records
  `payload_origin:"sealed"`, `payload_sha256` over the delivered bytes, and `delivered` — the field set
  above as a flat map, so it diffs field by field in a ledger query. When the live inputs have moved
  since the entry was written, the same record carries `diverged_fields` (every field where a
  re-derivation would have produced something else) and `live_fields` (what that re-derivation would
  have sent). The divergence is therefore visible in the evidence trail instead of silent in the wire
  body, and the delivered payload is unaffected by it: a reader can always tell what was requested from
  what was delivered.
- **An entry written before this sub-§ stays drainable, and says what it is.** A payload that is the bare
  marshalled `SpawnRequest` (the above shape's predecessor, with no `sealed` marker) is **decoded**, never
  dropped as `corrupt`: its replay re-derives the body from live state — the posture §3.9b replaces — and
  the record states `payload_origin:"rederived"` plus `rederived_fields` naming the eight fields that
  render is taken from (`board_path`, `title`, `priority`, `complexity`, `capability_tags`, `severity`,
  `host_id`, `submitted_ts`). An upgrade therefore loses no queued dispatch AND never passes a
  re-derivation off as a delivered-as-written payload. Only a payload this build cannot decode at all is
  dropped, and it is dropped with `drop_reason:"corrupt"` (§3.9a's bound table).
- **No codes of its own.** §3.9b mints no `TROUBLE-FLOW-*` code: a sealed delivery that fails is
  TROUBLE-FLOW-005 through the §3.6 path, and a failed replay is TROUBLE-FLOW-010 (as in §3.9a).
- **Test.** `internal/flow/replay_seal_test.go` reproduces the mutation and then pins the posture: one
  §3.6 dispatch 500s and parks, the project's board path MOVES and the daemon reports another `host_id`
  while the entry is queued, and the replay's body is then byte-identical to the failed attempt's — the
  same `<sha>`-pinned field set, the same `submitted_ts`, the same `idem_key`/`task_id` — with the record
  carrying `payload_origin:"sealed"`, `delivered` and `diverged_fields` naming `board_path`, `priority`,
  `complexity`, `capability_tags`, `severity` and `host_id` (the fields the pre-§3.9b path mutated on
  exactly that input change: the RED run shows the replay sending the moved board path, `P1` for `P3`,
  `high` for `low`, no tags and the new host id while the task id stayed stable). A second drain
  dispatches nothing. A pre-§3.9b entry (the bare marshalled `SpawnRequest`) is replayed — delivered, not
  dropped — with `payload_origin:"rederived"` and `rederived_fields` present.

### 3.10 One-fix-per-sig lease

`HotfixLease{Sig, Inc, Holder, GrantedTS, ExpiresTS}` — keyed by **sig**, holder = `inc_<ULID>`,
`ExpiresTS = GrantedTS + lease_ttl` (default `30m`), persisted in the ledger and mirrored in
`worktrees-meta/<repo-slug>.lock` metadata. One lease per sig across the whole host, regardless of
repo; a second incident with the same sig gets TROUBLE-FLOW-009 plus a cross-ref comment on the
existing row (§3.4) — two foremen on one sig is two patches for one bug and two chances to corrupt a
repo. The lease is renewed on every state change (`accepted`, `leased`, `released`) and released on
promotion, discard, rollback, or expiry; expiry while `leased` records a `spawn` record and leaves the
incident in SPEC-05's verify stage (the lease gates a *new* spawn, never the incident's own window).

### 3.11 The verify window (owned by SPEC-05) and rollback

trouble does not run the window. SPEC-05 owns its state machine, its monotonic-clock bookkeeping and
its **evidence tuple** `{ts_window_start, ts_window_end, window_s, events_observed, canary_seen,
counter_deltas, sources_expected/alive/quiet/missing, zone, result}`. The flow stage consumes the one
bit it needs and the tuple it must store:

- `result=passed` → the promote path (§3.12) opens; `Promotion.VerifyEvidence` is the tuple verbatim.
- `result=failed` → TROUBLE-FLOW-015, then rollback-or-discard (§below) and the incident reopens
  sig-keyed (SPEC-05 §3). A recurrence inside the window never becomes a second row.
- `result=invalid` → **not** a pass: the spawn stays `leased`, the incident stays `verifying`, and a
  canary/liveness gap record (SPEC-04/SPEC-05) is attached. Flow records it and takes no promote
  action; an invalid window that cannot be repaired escalates.

Rollback/discard on failure: the worktree's patch is discarded by its owner (branch deleted, tree
reaped), trouble's contribution is the record — decision `discarded`, the tuple, the reason, and
`RollbackHint` semantics from the registry where a deploy step was applied. If the owner reports the
tree or branch cannot be returned to its pre-spawn state, the record is TROUBLE-FLOW-017 with the
owner's detail; the incident escalates instead of claiming a clean rollback. **A verify failure never
leaves a half-applied worktree unrecorded.**

### 3.12 Promotion

```json
{"inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","verify_evidence":{"ts_window_start":"2026-09-16T09:25:00.000Z","ts_window_end":"2026-09-16T09:35:00.000Z","window_s":600,"events_observed":0,"canary_seen":true,"canary_id":"can_01J9Z6","counter_deltas":{"sentinel:payment-worker":3},"sources_expected":["7f3a91c2d4e5b607:sentinel:payment-worker"],"sources_alive":["7f3a91c2d4e5b607:sentinel:payment-worker"],"sources_quiet":["7f3a91c2d4e5b607:sentinel:payment-worker"],"sources_missing":[],"zone":"loopback","result":"passed"},"decision":"pending_human","decided_by":"","pr_url":"","worktree":"/srv/src/payment-api/.worktrees/tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","decided_ts":"2026-09-16T09:35:01.000Z"}
```

- `promote="human"` (default): decision `pending_human` on a passed tuple; the dashboard prompt and
  `trouble flow promote <sp_id>` are the two human entry points; both write an `Actor.Kind=human`
  record and flip `decision` to `promoted` (or `discarded`). Applies in **every** mode including
  `full` unless the operator sets `auto-after-verify`.
- `promote="auto-after-verify"`: decision `promoted` immediately on a passed tuple, and only when
  `AutonomyGates.AllowPromote` is true (i.e. `full`, AC-26). In `shadow`/`assisted` the promotion is
  denied by policy → TROUBLE-FLOW-016, the tuple is stored, and the decision stays `pending_human`.
- **Merging is not trouble's job.** The worktree is merged through its own PR by the repo's PR
  machinery under the `allow_merge` gate; trouble records `pr_url` and never pushes, never merges,
  never force-updates a branch. When no PR exists and the repo has no PR path, promotion records
  `decision="promoted"` with `pr_url=""` and the owner applies the change from the worktree — the
  discard/merge call is the owner's, auditable from the ledger.
- **Remedy capture:** on promotion the flow stage emits a `skill` candidate (SPEC-11) with
  `Provenance.incidents=[inc]`, `Provenance.research=[res]` when a research brief existed, and
  `Sig`-bound play reference; on discard it emits a `skill` refusal record naming the failed tuple.
  Either way the learning step is never skipped (AC-26).
- Promotion closes the loop in the board: task `completed_at`/`status` remain the owner's writes; the
  `flow`/`spawn` records plus `Promotion` are the audit trail AC-19 renders.

### 3.13 Ledger payloads (`flow`, `spawn`)

`flow` (kind `flow`; one record per filing, comment, dispatch, gate refusal, promotion decision):

```json
{"seq":41310,"rec_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","ts":"2026-09-16T09:14:05.400Z","kind":"flow","schema_version":1,"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker"},"actor":{"kind":"daemon","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z"},"redactions":2,"payload":{"stage":"file","decision":"created","driver":"board-jsonl","review_mode":"auto","project":"payment-api","board_path":"/srv/src/payment-api/.board","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","event_id":"4107","row_status":"todo","priority":"P1","complexity":"M","repo":"/srv/src/payment-api","issue_refs":["iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK"],"dup_of":"","idem_key":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","validate_cmd":"boardctl -C /srv/src/payment-api validate","validate_rc":0,"latency_ms":41,"error_code":""}}
```

Pinned `flow` payload keys: `stage` (`file|comment|dispatch|gate|budget|promote|rollback|reap`),
`decision` (`created|commented|dispatched|skipped|drafted|failed|promoted|discarded|pending_human`),
`driver`, `review_mode`, `project`, `board_path`, `task_id`, `event_id`, `row_status`, `priority`,
`complexity`, `repo`, `issue_refs`, `dup_of`, `idem_key`, `validate_cmd`, `validate_rc`,
`validate_findings`, `trig_to_spawn_ms`, `budget_exceeded`, `latency_ms`, `router_ref`, `reason`,
`error_code`.

`spawn` (kind `spawn`; one record per state change):

```json
{"seq":41311,"rec_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WN","ts":"2026-09-16T09:14:06.900Z","kind":"spawn","schema_version":1,"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker"},"actor":{"kind":"daemon","id":"troubled","version":"0.1.0","git_sha":"9c1f0ab","build_time":"2026-09-16T09:00:00.000Z"},"redactions":0,"payload":{"stage":"spawn","state":"accepted","spawn_id":"sp_01J9Z6Q0M2X4T8V1K7B3N5R8WP","task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","repo":"/srv/src/payment-api","worktree":"/srv/src/payment-api/.worktrees/tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ","worktree_mode":"owner_created","priority_class":"hotfix","router_ref":"spawn-8f21c0","attempts":1,"mutex_wait_ms":12,"free_disk_gb":184,"checkout_gb":0.46,"lease_id":"lease_01J9Z6Q0M2X4T8V1K7B3N5R8WQ","lease_expires_ts":"2026-09-16T09:44:06.900Z","trig_to_spawn_ms":4820,"budget_ms":60000,"budget_exceeded":false,"brief_sha256":"5c1f...","error_code":""}}
```

Pinned `spawn` payload keys: `stage` (`request|gate|spawn|lease|renew|promote|rollback|reap`),
`state`, `spawn_id`, `task_id`, `repo`, `worktree`, `worktree_mode` (`owner_created|owner_serialized`),
`priority_class`, `router_ref`, `attempts`, `next_try_ts`, `mutex_wait_ms`, `free_disk_gb`,
`checkout_gb`, `lease_id`, `lease_expires_ts`, `trig_to_spawn_ms`, `budget_ms`, `budget_exceeded`,
`brief_sha256`, `reason`, `error_code`.

### 3.14 The foreman brief

`SpawnRequest.Brief` is the marshalled `ForemanBrief`; `brief_sha256` in the `spawn` payload pins it so
a reviewer can prove what the foreman was told. Every field is scrubbed before it is written
(SPEC-02 `targets=["board","issue","skill"]`) and every size is capped, so a flood cannot turn a brief
into a ledger bomb:

```go
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

`EvidenceBundle` keys, all optional: `stack` (canonical frames, scrubbed), `samples` (≤10 scrubbed
event/message excerpts), `journal_tail` (≤64 KiB, scrubbed, unit-scoped), `release`, `env`, `culprit`,
`counters` (group count/rate at filing), `redactions` (count only). `ResearchBrief` carries the
SPEC-07 outcome subset `{submission_id, slug, state, brief}` and is `{}` when research was skipped or
degraded. `FailingTests` is a **hint list** — the daemon fills it from a configured probe command
(`flow.hotfix.test_hint_cmd`, default `""`); the foreman discovers the rest. `AllowedModules` mirrors
the rule's grants ∩ the shipped module set; `ToolContract="registry-only"` means the agent's only
capability is registry tool calls, schema'd, check_mode-able, audited (non-negotiable #3).

### 3.15 Types added to SPEC-TYPES by this spec

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

```go
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
{"driver":"board-jsonl","review_mode":"auto","board_path":"/srv/src/payment-api/.board","id_prefix":"tsk_","validate_cmd":"boardctl -C /srv/src/payment-api validate","registration_probe_interval":"5m","registration_stale_max":"1h","scheduler_endpoint":"http://127.0.0.1:9090","scheduler_token_file":"/etc/trouble/scheduler.token","projects":{"payment-api":{"name":"payment-api","repo":"/srv/src/payment-api","board_path":"/srv/src/payment-api/.board","enabled":true,"hotfix":true,"scheduler":"payment-api","registered":true,"ticked":true,"last_probe_ts":"2026-09-16T09:14:00.000Z","reason":""}},"hotfix":{"enabled":false,"allowed_repos":["/srv/src/payment-api"],"unit_repo_map":{"payment-worker":"/srv/src/payment-api"},"foreman_spawn":"router_spawn","priority_class":"hotfix","verify_window":"10m","promote":"human","max_concurrent":2,"min_free_disk_gb":10,"worktree_base":".worktrees","worktree_exempt":["/srv/src/warehouse-index"],"lease_ttl":"30m","spawn_ack_timeout":"5s","spawn_worktree_timeout":"30s","max_attempts":5,"min_severity":"high","mutex_wait":"5s","test_hint_cmd":"","models":{"primary_model":"","primary_provider":"","fallback_model":"","fallback_provider":""},"capability_tags":["hotfix","trouble"]}}
```

### 3.16 Autonomy interaction (design constraint)

| Mode | Row | Spawn | Verify | Promote | Merge |
|---|---|---|---|---|---|
| `shadow` | drafted only: `flow.create_task` runs check_mode, the `Diff` (exact bytes) is recorded in the `flow` payload, nothing is appended → `decision="drafted"` | not attempted; `decision="drafted"`, reason `autonomy_shadow` | SPEC-05 window still runs on the incident | denied (TROUBLE-FLOW-016) | never |
| `assisted` | appended when the rule/incident carries a `flow:write` execute grant, else drafted | requested when `AutoGrants` include `flow:spawn` | SPEC-05 | denied unless a `flow:promote` grant exists | repo policy |
| `full` | appended | requested | SPEC-05 | auto when `promote="auto-after-verify"`, else human | repo policy + `allow_merge` |

Kill-switch: no new filing or spawn starts after the flag is set; in-flight spawns park at their next
state boundary (a pending lease is released, an accepted spawn is re-adopted on the next boot), and the
parked `spawn` record carries `reason="kill_switch"` (SPEC-05 §3 owns the checkpoint semantics).

## 4. Wiring

| Producer | Consumes | Emits (kinds) |
|---|---|---|
| internal/ladder | `Flow.File`, `Flow.Comment`, `Flow.Spawn`, `Flow.Promote` via `flow.*` modules | `flow`, `spawn` (forwarded through the ladder's stage) |
| internal/registry | `Descriptor` for `flow.create_task/comment_task/spawn_foreman/promote/rollback` | `tool_call` |
| internal/issues | `IssueRef` for `issue_refs`; `Comment` on recurrence | `issue` |
| internal/research | `ResearchOutcome` for `ForemanBrief.ResearchBrief` | `research` |
| internal/skills | `Promotion` + `Incident` for the candidate | `skill` |
| internal/dashboard | `Flow.Timeline`, `Flow.SpawnState` | (none; reads) |
| internal/lifecycle | config resolve/precedence, state root, version stamping, spool | `config`, `lifecycle` |

Order of operations for one hot-fix filing (every step ledger-visible):

```
sensor/sentinel event → dedup (sig) → incident detected → record
  → flow gate chain (§3.7, 10 checks, first failure recorded)
  → flow.create_task  (authorize → validate → check_mode diff → apply(append+fsync) → verify(validate_cmd) → audit)
  → lease grant (sig-keyed) → spawn request (requested) → router_spawn
  → accepted → worktree exists in <repo>/.worktrees/<tsk_id> → leased + brief_sha256
  → SPEC-05 verify window (evidence tuple)
  → promotion (human default) → PR merge by the repo → skill candidate
  → reconcile: worktree reaped, lease released, worktrees-meta/<spawn>.json closed
```

Invariants this subsystem holds: trouble never writes a row it did not create; one row per (sig,
board); one spawn per sig host-wide; one writer per worktree registry; append-only board writes; no git
command against a repo the scheduler owns; no shell for the agent; every flow/spawn record carries
`origin{host_id, hub_id, source}` so a multi-host flow history stays mergeable (T5 enabler).

Config keys introduced: `flow.{driver,review_mode,board_path,id_prefix,validate_cmd,depends_on,
extra_tags,registration_probe_interval,registration_stale_max,scheduler_endpoint,scheduler_token_file,
router.*,projects.*}` and
`flow.hotfix.{enabled,allowed_repos,unit_repo_map,foreman_spawn,priority_class,verify_window,promote,
max_concurrent,min_free_disk_gb,worktree_base,worktree_exempt,lease_ttl,spawn_ack_timeout,
spawn_worktree_timeout,max_attempts,min_severity,models,capability_tags,mutex_wait,test_hint_cmd}`.
`trouble config explain` prints each resolved value with its provenance (flag > env > file > default) —
for `flow.hotfix.enabled` that includes the fact that the default is `false`.

`examples/config.toml` (fleet-shaped, every value an example): `allowed_repos = ["/srv/src/payment-api"]`,
`unit_repo_map = {"payment-worker" = "/srv/src/payment-api"}`,
`worktree_exempt = ["/srv/src/warehouse-index"]` (an 8.9 GB measured checkout), `min_free_disk_gb = 10`,
`validate_cmd` set to the board's own validate command, `promote = "human"`.

## 5. Errors

Every code below exists in SPEC-TYPES §5 inside TROUBLE-FLOW-001..019. Each is mirrored into
`Record.payload.error_code` on the record that describes the failure (SPEC-INDEX §5 rule 3).

| Code | Class | Trigger | Recovery |
|---|---|---|---|
| TROUBLE-FLOW-001 | permanent | `[flow] driver = "none"` or unknown driver; any filing call | refuse, record, banner on the dashboard budget panel |
| TROUBLE-FLOW-002 | transient | board append failed (missing board dir, EROFS, ENOSPC, validate rc=2/timeout) | retry via the spool with backoff, then escalate with TROUBLE-FLOW-011 |
| TROUBLE-FLOW-003 | permanent | post-append validation failed for our id (in-process schema or `validate_cmd` rc=1 naming the id) | quarantine the id, comment on the incident, banner; never rewrite the file |
| TROUBLE-FLOW-004 | permanent | id conflict: the computed `tsk_` id already exists after 8 allocation attempts | allocate from the re-read max, record; repeated conflict escalates |
| TROUBLE-FLOW-005 | transient | task-router dispatch failed (5xx/timeout/refused) | immediate retries, then `SpoolEntry` replay; permanent 4xx recorded without retry |
| TROUBLE-FLOW-006 | permanent | hot-fix lane disabled on this host (`enabled=false`) | row still filed; no spawn; `decision="skipped"`, reason recorded |
| TROUBLE-FLOW-007 | permanent | repo not in `allowed_repos` (or not equal to the project's repo) | refuse the spawn, keep the row, escalate |
| TROUBLE-FLOW-008 | permanent | severity below `min_severity`, or `Rule.Hotfix=false` | normal rungs only; recorded with `reason` |
| TROUBLE-FLOW-009 | permanent | one-fix-per-sig lease held by another incident | cross-ref comment on the existing row; the second incident waits on the first |
| TROUBLE-FLOW-010 | transient | `router_spawn` call failed (no ack, transport error) | `spawn_pending` + bounded retry (5 attempts) |
| TROUBLE-FLOW-011 | transient | spawn still pending after the retry budget | durable queue holds the entry, `spawn_pending` persists, incident escalates one rung |
| TROUBLE-FLOW-012 | permanent | disk gate refused: `free_gb < max(min_free_disk_gb, 3 × checkout_gb)` | no worktree; row + request recorded; operator frees disk or exempts the repo |
| TROUBLE-FLOW-013 | transient | per-repo worktree mutex acquire timed out (`mutex_wait`) | retry with backoff; a stale lock is broken by TTL + heartbeat |
| TROUBLE-FLOW-014 | permanent | repo is in `worktree_exempt` (huge checkout) | `worktree_mode="owner_serialized"`; the owner manages the tree |
| TROUBLE-FLOW-015 | permanent | verify window failed for a hot-fix spawn | rollback/discard by the owner, incident reopens sig-keyed (SPEC-05) |
| TROUBLE-FLOW-016 | permanent | promotion denied by policy (`promote=auto-after-verify` outside `full`) | tuple stored, decision `pending_human` |
| TROUBLE-FLOW-017 | permanent | rollback refused/not possible for this spawn | record the owner's detail, escalate; never claim a clean rollback |
| TROUBLE-FLOW-018 | permanent | target project not registered/enabled in the scheduler, board path mismatch, or the registration proof went stale | write nothing, banner, re-probe every interval; filing resumes when the proof returns |
| TROUBLE-FLOW-019 | permanent | board comment/cross-ref conflict on an existing sig row | record the intended row id, retry the comment once, escalate; never append a duplicate row |

Cross-area codes this subsystem raises (owned elsewhere, listed for the audit trail):
TROUBLE-LADDER-011 (autonomy gate denied for the spawn stage), TROUBLE-LIFECYCLE-001 (config refused
at load: bad `worktree_base`, `max_concurrent > 4`, unknown `priority_class`, `review_mode=never` with
`hotfix=true`), TROUBLE-SCRUB-* (brief scrubbing), TROUBLE-ISSUES-* (issue desk degradation while the
`issue_refs` are empty), TROUBLE-LEDGER-* (record append), TROUBLE-LIFECYCLE-015 (spool budget).

## 6. Edge cases

| # | Case | Rule |
|---|---|---|
| 1 | Router down, hot-fix on | `board-jsonl` files the row; the call degrades to `spawn_pending` + durable queue; pending count on the dashboard; escalate (FLOW-010/011) |
| 2 | The rung never reaches flow (play resolved it) | nothing is written — a row for an already-fixed bug is noise the fleet pays for |
| 3 | Same sig, second incident, first row open | comment + cross-ref only (§3.4); FLOW-019 on conflict; test asserts exactly 1 000 rows for 1 000 sigs |
| 4 | Same sig, row closed, recurrence | reopen the same incident (SPEC-05); comment on the closed row; a new row only when the repo or board changed, and `dup_of` names the old row |
| 5 | Foreign writer rewrites the board between index and append | the `(dev,inode,mtime)` stamp invalidates the index, the id is reallocated from the fresh max; a race on the exact id → FLOW-004 after 8 attempts |
| 6 | Empty board (no neighbour row to mirror) | the §3.1 schema is authored by trouble; the first row uses the configured `rowStyle` default (compact, schema order) |
| 7 | Non-strict board (legacy multi-line rows) | detected at boot; append refused with FLOW-002 `reason="non_strict_board"` — an append that cannot be re-read is a corrupt board |
| 8 | Board path is a symlink or a superproject subdir | resolved once (`Clean` + symlinks) and compared byte-for-byte with the scheduler's reported path; mismatch → FLOW-018 |
| 9 | Registration lost mid-flight (project disabled while leased) | the running foreman is not killed; no new filing or spawn; the state change is recorded once |
| 10 | Disk fills during a spawn | the gate runs pre-mutex (FLOW-012) and the post-acceptance check re-runs on the router's free-space sample; failures are recorded, never retried into a full disk |
| 11 | Stale mutex or lease after a crash | lock = 15 s mtime heartbeat, lease = `ExpiresTS`; boot reconcile breaks locks past TTL and releases expired leases, one `spawn` record each; a live heartbeat is never stolen |
| 12 | Daemon restart mid-window | `worktrees-meta/<spawn>.json` + the ledger re-adopt the spawn (`leased` restored, lease re-granted if unexpired, remaining window recomputed by SPEC-05); an unadoptable spawn is orphaned with a record — never left ambiguous |
| 13 | Two incidents, two repos, one budget | `max_concurrent` is host-wide; the third request waits on its mutex then fails FLOW-013 rather than exceeding the cap (a worktree is a full checkout, so the cap is a disk decision) |
| 14 | Clock skew across hosts | leases/mutexes use the monotonic clock, persisted timestamps are RFC3339 UTC; skew past tolerance marks the verification `invalid` (SPEC-TYPES §6.5, TROUBLE-LIFECYCLE-017) and flow promotes nothing |
| 15 | Promotion with no PR path | `pr_url=""`, the owner applies the change from the worktree, the deciding actor is recorded — no claim of a merge that never happened |
| 16 | Verify fails while a human is mid-promotion | the failure wins: `pending_human` is superseded by `discarded` and the human action is refused with FLOW-015 |
| 17 | Kill-switch between request and acceptance | the spawn parks, the lease releases, the parked record names the kill-switch; resuming re-requests the same `task_id` (idempotent by §3.6) |
| 18 | Brief exceeds its caps | samples/stack/journal tails are truncated to the §3.14 limits and `redactions` is recorded; a brief is trimmed, never dropped, and the trim is visible in the `brief_sha256`-pinned content |
| 19 | Research degraded, hot-fix still on | the spawn proceeds with `ResearchBrief={}` (research never blocks the lane, AC-20); the incident links the degraded research record |
| 20 | Acknowledged, but no worktree within `spawn_worktree_timeout` | `spawn` record `state="spawn_pending"`, `reason="worktree_timeout"`, one retry, then escalate (FLOW-011) |

Every case above is exercised by a named test in §7; a case with no test is a case that will be
discovered in production instead.

## 7. Testing

All tests are `internal/flow` package tests plus one end-to-end harness; `-count=1` everywhere
(no cached greens). Numeric thresholds are pass/fail, not guidance.

| File | Cases | Threshold / assertion |
|---|---|---|
| `board_test.go` | row round-trip against the §3.1 fixture; style preservation (compact/spaced, key order) against 5 real-shaped boards; schema-order stability | byte-exact line equals the fixture; a rewrite of the same row produces an identical diff hash |
| `writer_test.go` | id allocation from max (10 000-row board, cold and warm); 8-attempt conflict path → FLOW-004; append-only proof (`tasks.jsonl` line count +1, no other line changes, `git diff --numstat` = 1 insertion); two-file order; missing-event reconcile is idempotent (run twice, one event) | cold allocation ≤100 ms, warm ≤5 ms on 10 000 rows; append+fsync+validate p99 ≤250 ms; 0 non-appended lines touched |
| `validate_test.go` | in-process schema failure; `validate_cmd` rc=0 / rc=1-names-our-id → FLOW-003 / rc=1-foreign → recorded / rc=2 → FLOW-002; validate command hanging past 10 s | each path asserts the exact code; hanging command returns within 11 s |
| `review_test.go` | `auto` → `status="todo"`; `review` → `status="blocked"` + `review_approve` event (no row rewrite); `never` → zero bytes written + `decision="skipped"`; precedence project > rule > global | 0 row rewrites in every mode; `never` yields an empty board diff |
| `dedup_test.go` | 1 000 sigs filed concurrently; same sig ×3 incidents; same sig on two boards (shared vs unshared repo); missing-row comment → FLOW-019 | exactly 1 000 rows; 0 duplicates; 3 comments on the recurring row; FLOW-019 recorded once with the intended id |
| `registration_test.go` | proof held; name missing; `enabled=false`; board path mismatch; scheduler unreachable then stale past `registration_stale_max` | each failure → FLOW-018 with **zero bytes written**; unreachable ≤stale → filing proceeds on the last proof |
| `router_test.go` | http + cli modes (httptest + a stub binary); payload equality between modes; 2xx / 409-duplicate / 429 / 400 / 5xx / timeout; idempotent replay from the spool; the token comes from the 0600 file and never appears in argv or a URL | 409 treated as success; 3 immediate retries then 1 spool entry; token bytes absent from `ps`-visible argv and from every logged string |
| `hotfix_gate_test.go` | the 10-check chain, one test per check, first-failure-wins ordering; exempt repo → FLOW-014 + `worktree_mode="owner_serialized"`; `max_concurrent > 4` and `worktree_base="/tmp/x"` → load refused (TROUBLE-LIFECYCLE-001) | one code per case; refused configs never reach `Start()` |
| `lease_test.go` | 100 parallel incidents, one sig → exactly 1 lease; second lease attempt → FLOW-009; expiry releases; renewal on state change; stale lease after simulated crash | 1 grant, 99 FLOW-009, lease never double-held; expiry observed within lease_ttl + 1 s |
| `spawn_test.go` | state machine transitions; 5-attempt backoff then durable; `spawn_pending` never dropped (kill the router stub mid-run and assert the entry survives a restart); worktree timeout path; boot reconcile adopts/breaks locks; lease+meta files reaped on terminal states | 0 lost spawn requests; `trig_to_spawn_ms` recorded on every spawn; after 5 failures the entry is still queued with `state="spawn_pending"` |
| `promote_test.go` | human default (pending → promoted/discarded with an actor record); `auto-after-verify` in `full`; denied in `shadow` → FLOW-016; verify failure → discard + FLOW-015; rollback refusal → FLOW-017; skill candidate on success, refusal record on discard | exactly one skill record per outcome; no promotion without a `result=passed` tuple; shadow promotes nothing |
| `contract_test.go` | SPEC-06 conformance for `flow.create_task/comment_task/spawn_foreman/promote/rollback`: double-apply idempotency, check_mode returns the true diff without writing, schema violation rejected at validate | passes the shipped testkit; double-apply → one appended line total |
| `flow_e2e_test.go` | **AC-21**: scripted bad line in an allowed repo, `hotfix.enabled=true` → direct row + spawn within 60 s, patch lands in the worktree only (`git -C <main> status --porcelain` empty), window passes → promotion prompt, recurrence → rollback + reopen. **AC-9**: both drivers file a row and the router path round-trips. **AC-19**: the timeline function returns filed → foreman → patch → verify → promote with PR link. **AC-26**: `full` + `auto-after-verify` runs detection → row → spawn → verify → promote → skill candidate with zero human actions, and the kill-switch before the spawn yields exactly one parked stage and no spawn | AC-21 `trig_to_spawn_ms ≤ 60 000` (asserted on the recorded value); main checkout diff empty; AC-19 timeline complete and ordered; AC-26 zero human actors in the ledger slice |
| `testdata/` | strict board (10 000 rows), non-strict legacy board, empty board, foreign-rewrite board, config set (valid, `/tmp` base, `max_concurrent=5`, unknown priority class) | fixtures reused by every test above |
| `spool_test.go` | §3.9a on the shipped store: an `Enqueue`-only sink (the desk adapter's shape) yields EXACTLY ONE outcome — an honest `unspooled` record naming the coupling — and zero `spooled` claims; a typed-nil store is not adopted; with the flow's own store a parked spawn is one `0600` file under `<state_root>/spool/flow/spawn/` and the record says `spooled`; a drain re-dispatches with the SAME idem key against a real HTTP router, deletes the entry on success, re-drains to zero, and leaves an empty queue after a simulated restart; an entry written by one store instance is listed, decoded and replayed by a second one; `attempts`, `ttl`, `corrupt` and overflow each drop the entry with a `flow` + `spawn` record pair and zero `gap` records; overflow evicts the OLDEST entry, never the incoming one; `Flow.Run` drains the queue on its ticker. `flow_spool_wiring_test.go` (internal/app) drives the REAL `flowDeps` with the issue desk OFF (the shipped posture): the flow still receives a replayable queue, the wired sink writes one `0600` entry under the state root, and a store that cannot be built leaves the flow reporting "no queue" | 0 `spooled` records while no replayable queue is wired; exactly 1 file per entry, mode `0600`, directory `0700`; queue depth 0 after a successful replay and 0 after the restart; 2 overflow drops for 4 writes at a bound of 2; the loop test's deadline is generous by design (wall-clock driven), every other assertion is clock-injected |
| `replay_seal_test.go` | §3.9b, reproduction first and the posture second: one §3.6 dispatch 500s and parks; the project's board path MOVES and the daemon reports another `host_id` while the entry is queued; the replay's body is then byte-identical to the failed attempt's, and the record carries `payload_origin:"sealed"`, `delivered` and `diverged_fields` naming `board_path`, `priority`, `complexity`, `capability_tags`, `severity` and `host_id`; a second drain dispatches nothing; a pre-§3.9b entry (the bare marshalled `SpawnRequest`) still replays — delivered, `payload_origin:"rederived"` + `rederived_fields` — instead of being dropped as corrupt | the replayed body equals the failed attempt's on the canonical (key-sorted) JSON; 2 router requests for 2 attempts, both carrying the same `idem_key`; 0 further dispatches on a second drain; RED against the pre-§3.9b code shows the replay mutating 6 secondary fields (`board_path` moved, `P1` for `P3`, `high` for `low`, no tags, empty complexity, the new `host_id`) plus `title`/`submitted_ts` while `idem_key`/`task_id` stayed stable |
| `internal/app/flow_bounds_wiring_test.go` | the §3.9a bounds as resolved SPEC-12 §3.1d keys at the composition root: `flowSpoolBounds` over a config file with `[flow] spool_max_entries = 3` carries 3 and the §3.9a defaults for the four undeclared keys; four `Put`s through `newFlowSpool` leave three entries, report exactly one `overflow` drop for the OLDEST id and leave it off disk — where the same four writes with no `[flow]` table leave four; a two-hour-old, undecodable entry is dropped with `drop_reason:"ttl"` through `newFlowSubsystem` + `Replay` under `flow.spool_ttl = 1h`, and never with `"ttl"` under the compiled default | 2 eviction cases (configured 3 → depth 3 + oldest gone; default → depth 4); the TTL case is a 2-row table whose control row must NOT produce a `ttl` drop; reverting either the projection or the flow's construction fails the suite |

Regression numbers carried from the judges' measurements: worktree creation cost is a full checkout
(68 MB / 242 MB / 464 MB / 8.9 GB measured) so the disk gate and cap are exercised with those sizes
synthesised as fake `checkout_gb` values; a single worktree `add` on a small repo is ~7 ms, which is
the number the ≤30 s worktree budget must never approach in a test.

## 8. hilo impact

- **New package:** `internal/flow` with `flow.go` (Flow, gates, Start/Stop), `driver.go` (`FlowDriver`,
  driver selection), `boardjsonl.go` (index, id allocation, append, style, two-file write, validate),
  `router.go` (http + cli dispatch, retry, spool), `project.go` (registration proof, probes),
  `hotfix.go` (gate chain, priority class, disk/mutex gates), `spawn.go` (state machine, durable
  queue, reconcile), `lease.go` (sig leases), `promote.go` (promotion, rollback, skill hand-off),
  `brief.go` (`ForemanBrief` assembly + caps), `budget.go` (the 60 s clock and per-spawn measurement),
  `timeline.go` (dashboard read model), `spool.go` + `replay.go` (§3.9a: the flow-owned dispatch queue,
  its bounds and the drain `Flow.Run` owns; §3.9b: the sealed entry payload a replay delivers, and the
  divergence record that tells a delivered payload from a re-derived one).
- **Fan-out:** `internal/flow` imports `internal/types` (all shared types) and calls
  `internal/ledger` (record append), `internal/issues` (EnsureBySig/Comment), `internal/research`
  (brief read), `internal/skills` (candidate/refusal), `internal/registry` (module registration for
  `flow.*`), `internal/scrub` (brief and comment bodies), and `internal/lifecycle` (config, state root,
  spool, version stamping). Nothing in `internal/flow` is imported by `internal/types`.
- **Fan-in:** `internal/ladder` (the flow stage and the promotion hand-off), `internal/registry`
  (module descriptors), `internal/dashboard` (the `Flow.Timeline` read model behind
  `/incidents/{id}` and `RuntimeWatermarks.Worktrees`), `internal/lifecycle` (boot reconcile), `internal/skills`
  (promotion provenance). Five importers, one export surface (`Flow` + the `flow.*` descriptors).
- **Blast radius:** greenfield repo `~/trouble`; no fleet repository, board, worktree or
  scheduler unit is modified by this spec — trouble *requests* spawns through the scheduler's admission
  path and never touches a repo it does not own. The only external touches at runtime are the
  configured board files (append-only, one line per row), the configured router endpoint/CLI, and
  `worktrees-meta/` under the state root. Changing `BoardRow` touches SPEC-08, SPEC-09 (issue task
  links) and SPEC-10 (timeline), which is why the row schema lives in SPEC-TYPES §3.10 and not here.
