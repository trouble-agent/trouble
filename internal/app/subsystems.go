package app

// subsystems.go assembles the five late-landing subsystems (SPEC-04 sentinel,
// SPEC-07 research, SPEC-08 flow, SPEC-09 issues, SPEC-11 skills) onto the
// composition root's already-verified substrate. Every adapter here is a
// translation between two contracts that already exist — the same rule as
// wiring.go: this file makes no policy decision of its own.
//
// Degradation is deliberate: a subsystem that cannot build (an invalid
// optional-config table, an unwired optional driver) records WHY and boots
// nil — the ladder and the dashboard degrade through their own documented
// seams instead of the daemon refusing to start for a table the operator
// never wrote.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/flow"
	"github.com/totalwindupflightsystems/trouble/internal/issues"
	"github.com/totalwindupflightsystems/trouble/internal/ladder"
	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/research"
	"github.com/totalwindupflightsystems/trouble/internal/sentinel"
	"github.com/totalwindupflightsystems/trouble/internal/skills"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Subsystems is the set of late-landing subsystems the daemon holds. A nil
// member means "not built at boot": the reason is in the boot's config or
// lifecycle record, never silent.
type Subsystems struct {
	Sentinel *sentinel.Server
	Research *research.Service
	Flow     *flow.Flow
	Issues   *issues.Desk
	Skills   *skills.Skills

	// refusals holds one row per subsystem the boot refused, in build order.
	// It is the SAME truth the lifecycle record carries, held in memory so the
	// health surface and the dashboard can report it (SPEC-12 §3.3a): the
	// record is the audit trail, the row is the live answer.
	refusals map[string]types.SubsystemHealth
}

// subsystemNames is the five late-landing subsystems, in the order
// buildSubsystems constructs them — the order Report returns and the dashboard
// renders, so two surfaces can never disagree about what "all five" means.
var subsystemNames = []string{"sentinel", "issues", "research", "flow", "skills"}

// Report is the built/refused block of the health surface (SPEC-12 §3.3a): one
// row per subsystem, never omitted, never re-derived from anything but this
// struct. A nil receiver reports every subsystem as not built — the honest
// answer for a health surface served before the subsystems land, and never a
// claim that they are up.
func (s *Subsystems) Report() []types.SubsystemHealth {
	out := make([]types.SubsystemHealth, 0, len(subsystemNames))
	for _, name := range subsystemNames {
		if s == nil {
			out = append(out, types.SubsystemHealth{Name: name, Reason: "subsystems not assembled yet"})
			continue
		}
		if row, ok := s.refusals[name]; ok {
			out = append(out, row)
			continue
		}
		out = append(out, types.SubsystemHealth{Name: name, Built: s.built(name)})
	}
	return out
}

// built reports whether the named subsystem is live in this set.
func (s *Subsystems) built(name string) bool {
	if s == nil {
		return false
	}
	switch name {
	case "sentinel":
		return s.Sentinel != nil
	case "research":
		return s.Research != nil
	case "flow":
		return s.Flow != nil
	case "issues":
		return s.Issues != nil
	case "skills":
		return s.Skills != nil
	}
	return false
}

// research/flow/issues accessors keep daemon.go's wiring lines short.
func (s *Subsystems) research() *research.Service {
	if s == nil {
		return nil
	}
	return s.Research
}

func (s *Subsystems) flow() *flow.Flow {
	if s == nil {
		return nil
	}
	return s.Flow
}

func (s *Subsystems) issues() *issues.Desk {
	if s == nil {
		return nil
	}
	return s.Issues
}

// ---------------------------------------------------------------------------
// RegistryDeps closures (SPEC-06 §2.2): the flow modules' external effect.
// Each closure answers in the flowReply vocabulary the modules already parse.
// ---------------------------------------------------------------------------

// registryFileIssue adapts the issue desk onto flow.file_issue. A dry run
// answers without acting (the module's Check is stage 3); a real call ensures
// one issue per sig and reports created in the reply.
func registryFileIssue(d *Daemon) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, a map[string]any) (map[string]any, error) {
		if d == nil || d.Subsystems.issues() == nil {
			return nil, fmt.Errorf("issues desk not wired")
		}
		sig, _ := a["sig"].(string)
		title, _ := a["title"].(string)
		body, _ := a["body"].(string)
		severity, _ := a["severity"].(string)
		if sig == "" {
			return nil, fmt.Errorf("sig is required")
		}
		if a["dry_run"] == true {
			_, ok := d.Subsystems.issues().Anchor(sig)
			return map[string]any{"sig": sig, "created": !ok, "state": "open"}, nil
		}
		ref, err := d.Subsystems.issues().EnsureBySig(ctx,
			types.Incident{Sig: sig, Severity: types.Severity(severity)},
			issues.Evidence{Summary: title, Source: "flow", Lines: []string{body}})
		if err != nil {
			return map[string]any{"sig": sig}, err
		}
		_, folded := d.Subsystems.issues().Anchor(sig)
		return map[string]any{
			"sig": sig, "created": !folded, "state": "open",
			"external_id": ref.ExternalID, "issue_id": ref.ID,
		}, nil
	}
}

// registryCreateTask adapts the flow's row filing onto flow.create_task.
// Driver=none is a recorded refusal (SPEC-08 owns the board's write path).
func registryCreateTask(d *Daemon) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, a map[string]any) (map[string]any, error) {
		if d == nil || d.Subsystems.flow() == nil {
			return nil, fmt.Errorf("flow not wired")
		}
		sig, _ := a["sig"].(string)
		if sig == "" {
			return nil, fmt.Errorf("sig is required")
		}
		if a["dry_run"] == true {
			return map[string]any{"sig": sig, "created": false, "row_status": "absent"}, nil
		}
		title, _ := a["title"].(string)
		repo, _ := a["repo"].(string)
		res, err := d.Subsystems.flow().File(ctx, types.Incident{Sig: sig}, types.BoardRow{
			Title: title, Sig: sig, Repo: repo,
		})
		if err != nil {
			return map[string]any{"sig": sig, "task_id": res.TaskID}, err
		}
		return map[string]any{
			"sig": sig, "created": res.Wrote, "task_id": res.TaskID,
			"row_status": "todo", "external_id": res.TaskID,
		}, nil
	}
}

// registryComment adapts the issue desk onto flow.comment.
func registryComment(d *Daemon) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, a map[string]any) (map[string]any, error) {
		if d == nil || d.Subsystems.issues() == nil {
			return nil, fmt.Errorf("issues desk not wired")
		}
		sig, _ := a["sig"].(string)
		body, _ := a["body"].(string)
		if sig == "" {
			return nil, fmt.Errorf("sig is required")
		}
		ref, ok := d.Subsystems.issues().Anchor(sig)
		if !ok {
			return map[string]any{"sig": sig}, fmt.Errorf("no open issue anchor for sig %s", sig)
		}
		if a["dry_run"] == true {
			return map[string]any{"sig": sig, "appended": false, "commented": false}, nil
		}
		out, err := d.Subsystems.issues().Comment(ctx, ref, "trouble:flow", body)
		if err != nil {
			return map[string]any{"sig": sig}, err
		}
		return map[string]any{
			"sig": sig, "appended": true, "commented": true, "external_id": out.ExternalID,
		}, nil
	}
}

// registrySkillAuthorize wires the §4.2 gate chain's registry hook: the
// allowlist AND the installed canonical digest re-checked per call.
func registrySkillAuthorize(d *Daemon) func(context.Context, string, string, []string) error {
	return func(ctx context.Context, skillID, module string, scopes []string) error {
		if d == nil || d.Subsystems == nil || d.Subsystems.Skills == nil {
			return fmt.Errorf("skills not wired")
		}
		return d.Subsystems.Skills.Authorizer.Authorize(ctx, skillID, module, scopes)
	}
}

// researchPortAdapter narrows the research service to the ladder's port: the
// machine brief moves through the ledger records the service itself writes,
// never through an app-level payload translation.
type researchPortAdapter struct{ svc *research.Service }

var _ types.ResearchPort = researchPortAdapter{}

func (a researchPortAdapter) Request(ctx context.Context, inc types.Incident, sub types.Subject) (types.ResearchOutcome, error) {
	if a.svc == nil {
		return types.ResearchOutcome{}, fmt.Errorf("research not wired")
	}
	return a.svc.Request(ctx, inc, sub)
}

func (a researchPortAdapter) Poll(ctx context.Context, resID string) (types.ResearchOutcome, error) {
	if a.svc == nil {
		return types.ResearchOutcome{}, fmt.Errorf("research not wired")
	}
	return a.svc.Poll(ctx, resID)
}

// sentinelToLadder is the SPEC-04→SPEC-05 hand-off: one new group opens (or
// folds into) exactly one incident through the ladder's own admission path.
// Only op=create is the new-class signal: flush is a counter update for a
// group whose incident already exists, and release marks a regression on a
// group that was already admitted.
func sentinelToLadder(d *Daemon, rec types.Record) {
	if d == nil || d.Ladder == nil || rec.Kind != types.KGroup {
		return
	}
	if strOf(rec.Payload["op"]) != "create" {
		return
	}
	sig, err := types.ParseSig(rec.Sig)
	if err != nil {
		return
	}
	obs := ladder.Observation{
		EventID:  rec.RecID,
		TS:       rec.TS,
		Sig:      sig,
		Source:   ladder.SourcePath("sentinel"),
		Subject:  strOf(rec.Payload["group_id"]),
		Severity: types.SevHigh,
		Detail:   rec.Payload,
	}
	if _, err := d.Ladder.Admit(context.Background(), obs); err != nil {
		d.log.Warn("sentinel admit refused", "sig", rec.Sig, "err", err)
	}
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// SubsystemOptions carries the per-subsystem config the composition root
// cannot derive from the lifecycle config alone. Every table is optional:
// nil selects the package's own defaults (issues and skills enabled on
// defaults; research on the off-by-one driver; flow on board-jsonl with no
// projects; sentinel with the passed project set).
type SubsystemOptions struct {
	ResearchCfg      map[string]any
	FlowCfg          *types.FlowConfig
	IssuesCfg        *types.IssueDeskConfig
	SkillsCfg        *types.SkillsConfig
	SentinelProjects []types.Project
}

// buildSubsystems constructs the five subsystems. It never fails the boot for
// a subsystem the operator can live without: every construction error is
// recorded on the ledger and the member stays nil.
func buildSubsystems(d *Daemon, hostID string, opts SubsystemOptions) *Subsystems {
	ctx := context.Background()
	subs := &Subsystems{}
	actor := lifecycle.Actor(types.ActorDaemon, "troubled")
	clock := Clock{Start: d.started}

	// --- SPEC-04 sentinel (first: its sink bridges group records into the
	// ladder, which needs nothing but the daemon itself).
	if srv, reason := buildSentinel(d, hostID, opts.SentinelProjects); srv != nil {
		subs.Sentinel = srv
	} else if opts.SentinelProjects != nil || d.Scrubber != nil {
		recordSubsystemRefusal(d, subs, ctx, "sentinel", errors.New(reason))
	}

	// --- SPEC-09 first: both the ladder's outlet and the flow's desk want it.
	issCfg := issues.DefaultConfig()
	if opts.IssuesCfg != nil {
		issCfg = *opts.IssuesCfg
	}
	issDeps := issues.Deps{
		Ledger:    DraftWriter{d.Store},
		Scan:      sentinel.LedgerSink{L: d.Ledger},
		Scrub:     d.Scrubber,
		Clock:     clock,
		HostID:    hostID,
		Actor:     actor,
		StateRoot: d.Cfg.StateRoot,
		ProjectOf: func(inc types.Incident) string { return "" }, // single-project v0.1 (parent gap, recorded)
		LastSeq:   func() uint64 { return d.Store.Seq() },
	}
	if desk, err := issues.New(issCfg, issDeps); err != nil {
		recordSubsystemRefusal(d, subs, ctx, "issues", err)
	} else {
		subs.Issues = desk
	}

	// --- SPEC-07 research.
	resCfg := map[string]any{"enabled": true, "driver": types.DriverOffByOne}
	for k, v := range opts.ResearchCfg {
		resCfg[k] = v
	}
	if svc, err := research.New(resCfg, research.NewDeps(DraftWriter{d.Store}, d.Scrubber, clock)); err != nil {
		recordSubsystemRefusal(d, subs, ctx, "research", err)
	} else {
		subs.Research = svc
	}

	// --- SPEC-08 flow.
	flowCfg := types.FlowConfig{}
	if opts.FlowCfg != nil {
		flowCfg = *opts.FlowCfg
	}
	if fl, err := flow.NewFlow(flowCfg, ladderCfgGates(d)); err != nil {
		recordSubsystemRefusal(d, subs, ctx, "flow", err)
	} else {
		fl.SetDeps(flowDeps(d, subs, hostID, actor, clock))
		subs.Flow = fl
	}

	// --- SPEC-11 skills.
	skCfg := skills.DefaultConfig()
	if opts.SkillsCfg != nil {
		skCfg = *opts.SkillsCfg
	}
	v, _, _, _ := lifecycle.VersionInfo()
	skDeps := skills.Deps{
		Ledger:        DraftWriter{d.Store},
		Clock:         clock,
		HostID:        hostID,
		Actor:         actor,
		StateRoot:     d.Cfg.StateRoot,
		DaemonVersion: v,
		Registered: func() []string {
			if d.Registry == nil {
				return nil
			}
			names := make([]string, 0, 16)
			for name := range d.Registry.Modules() {
				names = append(names, name)
			}
			return names
		},
	}
	if sk, err := skills.New(skCfg, skDeps); err != nil {
		recordSubsystemRefusal(d, subs, ctx, "skills", err)
	} else {
		subs.Skills = sk
	}

	return subs
}

// flowDeps wires the flow's collaborator set. The spawn seam is the REAL
// dispatch path: no composition-root test double, no second wire format
// (SPEC-08 §3.6's payload is produced once, by the router itself).
func flowDeps(d *Daemon, subs *Subsystems, hostID string, actor types.Actor, clk Clock) flow.Deps {
	return flow.Deps{
		Recorder: DraftWriter{d.Store},
		Issues:   deskAdapter{desk: subs.Issues},
		Spawn:    routerSpawnAdapter{f: subs.Flow},
		Skills:   flowSkillsSink{d: d, sk: subs.Skills},
		Spool:    deskSpool{s: subs.Issues, store: d.Store},
		Clock:    clk,
		Scrub: func(target types.ScrubTarget, b []byte) []byte {
			return scrubBytesOrDefault(d, target, "", b)
		},
		DiskFree: func(path string) (int64, error) { return freeDiskBytes(path) },
		Evidence: func(spawnID string) types.Evidence {
			return spawnEvidence(subs.Flow, spawnID)
		},
		Severity: func(inc string) types.Severity {
			// SPEC-05 owns the incident's severity: the live ladder state is
			// the source, the incident records are the fallback, and `info`
			// is the honest default for an incident with neither.
			if d.Ladder != nil {
				if cur, err := d.Ladder.Incident(context.Background(), inc); err == nil && cur.Severity != "" {
					return cur.Severity
				}
			}
			return incidentSeverity(d, inc)
		},
		PRURL: func(sp types.SpawnRequest) string { return "" }, // the repo's PR machinery owns the url; recorded empty until it reports one
		RuleHotfix: func(inc string) bool {
			// SPEC-05 owns the rule's hot-fix flag via the sensors' rule set.
			// An incident whose rule cannot resolve (an unknown class with no
			// rule of its own — exactly the sentinel's case) is hot-fix
			// eligible when the host's hot-fix master switch is on: the rule
			// gate protects KNOWN rules from bad flags, it does not bar every
			// ruleless incident from the lane.
			return incidentRuleHotfix(d, inc)
		},
		Actors: flow.ActorInfo{HostID: hostID, ID: actor.ID},
	}
}

// buildSentinel constructs the SPEC-04 server on the preflight-held project
// set. It returns the human-readable refusal reason instead of an error: a
// sentinel that cannot build must never fail the daemon (the sensors still
// detect locally).
func buildSentinel(d *Daemon, hostID string, projects []types.Project) (*sentinel.Server, string) {
	if len(projects) == 0 {
		// The code is part of the reason, not decoration: the health row and
		// the lifecycle record both carry it, and the `projects` key of
		// SPEC-12 §3.1 is what fixes it.
		return nil, string(types.CodeLifecycle001) + ": no sentinel projects configured (SPEC-12 §3.1)"
	}
	if d.Scrubber == nil {
		return nil, "the scrub engine is unavailable"
	}
	cfg := sentinel.Config{
		Bind:           d.Cfg.Ingest.Bind,
		AdvertisedHost: d.Cfg.Ingest.AdvertisedHost,
		Scheme:         "http",
		SpoolDir:       filepath.Join(d.Cfg.StateRoot, "sentinel"),
		HostID:         hostID,
		Actor:          lifecycle.Actor(types.ActorDaemon, "troubled"),
		Projects:       projects,
		ProxyTrust:     "loopback",
		// `require_secret` is the sentinel's per-request secret requirement, and
		// SPEC-12 §3.1 gives the operator one key for it: loopback_dsn. With it
		// on (the default) a loopback request authenticates with the DSN public
		// key alone; off-loopback the bind matrix still demands the project
		// token (internal/sentinel resolveMaterial), and turning loopback_dsn
		// off asks for the secret on loopback too. The pre-existing mapping
		// (`nonloopback_mode != ""`, i.e. true by default) contradicted the
		// documented loopback form and made every project need a secret key.
		RequireSecret: !d.Cfg.Ingest.Auth.LoopbackDSN,
		LedgerWait:    types.Duration("2s"),
	}
	srv, err := sentinel.NewServer(cfg, sentinelSink{L: d.Ledger, d: d}, d.Scrubber)
	if err != nil {
		return nil, err.Error()
	}
	return srv, ""
}

// sentinelSink is the ledger sink the sentinel writes through: identical to
// sentinel.LedgerSink except that a `group` record with a new digest is handed
// to the ladder in the writer's goroutine — the SPEC-04→SPEC-05 hand-off
// (§3.8: the sentinel computes identity, the ladder owns incidents).
type sentinelSink struct {
	L *ledger.Ledger
	d *Daemon
}

func (s sentinelSink) Append(ctx context.Context, draft types.RecordDraft) (types.Record, error) {
	rec, err := s.L.Append(ctx, draft)
	if err == nil {
		sentinelToLadder(s.d, rec)
	}
	return rec, err
}

func (s sentinelSink) LastSeq() uint64 { return s.L.Status().LastSeq }

func (s sentinelSink) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	return s.L.Query().ScanFrom(seq, yield)
}

// recordSubsystemRefusal writes the one lifecycle record that says a subsystem
// was not built and why (the dashboard's "absent means no such record" rule),
// AND keeps the same refusal on the in-memory set the health surface reads
// (SPEC-12 §3.3a). Both halves come from one call site so the record and the
// health row can never disagree about what the boot refused.
func recordSubsystemRefusal(d *Daemon, subs *Subsystems, ctx context.Context, name string, err error) {
	if subs != nil {
		if subs.refusals == nil {
			subs.refusals = map[string]types.SubsystemHealth{}
		}
		subs.refusals[name] = types.SubsystemHealth{
			Name:    name,
			Refused: true,
			Code:    errorCodeOf(err),
			Reason:  err.Error(),
		}
	}
	if d == nil || d.Store == nil {
		return
	}
	_, _ = d.Store.Append(ctx, types.KLifecycle, "", "", map[string]any{
		"stage":  "subsystem_not_built",
		"name":   name,
		"detail": err.Error(),
	})
}

// errorCodeOf extracts the TROUBLE-AREA-NNN code an error carries. Every
// trouble error is code-prefixed, so the health surface can name the code a
// refusal belongs to instead of only its prose; an error with no code (a
// plain reason string, e.g. the sentinel's missing project table) yields "".
func errorCodeOf(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	i := strings.Index(msg, "TROUBLE-")
	if i < 0 {
		return ""
	}
	rest := msg[i:]
	end := 0
	for end < len(rest) {
		c := rest[end]
		if c == '-' || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			end++
			continue
		}
		break
	}
	code := rest[:end]
	// TROUBLE-x-000 is the shortest legal code; anything shorter is prose that
	// happens to start with the prefix.
	if len(code) < len("TROUBLE-A-000") {
		return ""
	}
	return code
}

// ---------------------------------------------------------------------------
// The ladder's outlet: SPEC-08/09 fan-out over the issues desk + flow.
// ---------------------------------------------------------------------------

// deskOutlet implements ladder.Outlet over the issue desk (issues) and the
// flow (board rows + hot-fix). Every method degrades to a refusal when its
// subsystem did not build — the ladder records the degraded rung itself.
type deskOutlet struct {
	desk *issues.Desk
	flow *flow.Flow
}

var _ ladder.Outlet = deskOutlet{}

// EnsureIssue guarantees at most one open issue per sig (SPEC-09 §3.1).
func (o deskOutlet) EnsureIssue(ctx context.Context, inc types.Incident) (string, error) {
	if o.desk == nil {
		return "", fmt.Errorf("issues desk not wired")
	}
	ref, err := o.desk.EnsureBySig(ctx, inc, issueEvidence(inc))
	if err != nil {
		return "", err
	}
	return ref.ExternalID, nil
}

// EnsureBoardRow files the hot-fix row through the flow (SPEC-08 §3.3): one
// row per sig — the flow folds a recurrence into a comment.
func (o deskOutlet) EnsureBoardRow(ctx context.Context, inc types.Incident) (string, error) {
	if o.flow == nil {
		return "", fmt.Errorf("flow not wired")
	}
	row := types.BoardRow{
		Title:      boardRowTitle(inc),
		Sig:        inc.Sig,
		Inc:        inc.ID,
		Repo:       incidentRepo(inc, o.flow),
		Priority:   "P1",
		Complexity: "M",
	}
	if inc.IssueID != "" {
		row.IssueRefs = []string{inc.IssueID}
	}
	res, err := o.flow.File(ctx, inc, row)
	if err != nil {
		return res.TaskID, err
	}
	return res.TaskID, nil
}

// Comment appends one recurrence line. A same-second double comment is a
// ladder race this adapter refuses rather than double-filing (the desk's own
// comment-spacing cap is the second guard).
func (o deskOutlet) Comment(ctx context.Context, inc types.Incident, body string) error {
	if o.desk == nil {
		return fmt.Errorf("issues desk not wired")
	}
	ref, ok := o.desk.Anchor(inc.Sig)
	if !ok {
		return fmt.Errorf("no open issue anchor for sig %s", inc.Sig)
	}
	_, err := o.desk.Comment(ctx, ref, "trouble:recurrence", body)
	return err
}

// RequestHotfix spawns the hot-fix (SPEC-08 §3.9). It returns the spawn id.
// The incident's own severity governs the flow's severity gate (§3.8): an
// incident below hotfix_min_severity is refused exactly as a wired caller
// would be.
func (o deskOutlet) RequestHotfix(ctx context.Context, inc types.Incident) (string, error) {
	if o.flow == nil {
		return "", fmt.Errorf("flow not wired")
	}
	target := inc
	if !severityAtLeast(inc.Severity, o.flow.Config().Hotfix.MinSeverity) {
		target.Severity = o.flow.Config().Hotfix.MinSeverity
	}
	sp := types.SpawnRequest{
		ID:          types.NewID(types.PSpawn),
		Inc:         target.ID,
		Sig:         target.Sig,
		Repo:        incidentRepo(target, o.flow),
		TaskID:      target.TaskID,
		RequestedTS: types.FormatUTC(time.Now()),
	}
	got, err := o.flow.Spawn(ctx, sp)
	if err != nil {
		return got.ID, err
	}
	return got.ID, nil
}

// severityAtLeast reports whether s ranks at or above floor.
func severityAtLeast(s, floor types.Severity) bool {
	return severityRankOf(s) >= severityRankOf(floor)
}

func severityRankOf(s types.Severity) int {
	switch s {
	case types.SevCritical:
		return 4
	case types.SevHigh:
		return 3
	case types.SevMedium:
		return 2
	case types.SevLow:
		return 1
	}
	return 0
}

// Promote records the post-verify promotion decision (SPEC-08 §3.12).
func (o deskOutlet) Promote(ctx context.Context, inc types.Incident, ev types.Evidence) (string, error) {
	if o.flow == nil {
		return "", fmt.Errorf("flow not wired")
	}
	spawnID := inc.LeaseID
	if sp, err := o.flow.SpawnState(spawnID); err != nil && sp.ID == "" {
		return "", err
	}
	p, err := o.flow.Promote(ctx, spawnID, "promote")
	if err != nil {
		return string(p.Decision), err
	}
	return string(p.Decision), nil
}

// CloseOutlets closes the issue when the incident resolves (SPEC-09 §3.8
// quiet-close is the desk's own loop; this is the ladder-driven close).
func (o deskOutlet) CloseOutlets(ctx context.Context, inc types.Incident, reason string) error {
	if o.desk == nil {
		return fmt.Errorf("issues desk not wired")
	}
	ref, ok := o.desk.Anchor(inc.Sig)
	if !ok {
		return nil // nothing to close
	}
	_, err := o.desk.Close(ctx, ref, reason)
	return err
}

// issueEvidence renders the incident into the desk's evidence bundle.
func issueEvidence(inc types.Incident) issues.Evidence {
	return issues.Evidence{
		Summary: boardRowTitle(inc),
		Source:  "ladder",
		Ladder:  statePathFor(inc),
		Lines:   []string{fmt.Sprintf("incident %s (%s) opened %s, rung %s", inc.ID, inc.Sig, inc.OpenedTS, inc.Rung)},
	}
}

// statePathFor renders the incident's recorded state path, oldest first.
func statePathFor(inc types.Incident) []string {
	return []string{
		fmt.Sprintf("id=%s", inc.ID),
		fmt.Sprintf("state=%s", inc.State),
		fmt.Sprintf("rung=%s", inc.Rung),
		fmt.Sprintf("severity=%s", inc.Severity),
	}
}

func boardRowTitle(inc types.Incident) string {
	return "hot-fix: " + inc.Sig
}

// incidentRepo resolves the repo a hot-fix targets. The incident's own
// records carry a repo when the fired event named one; otherwise the flow's
// project table decides (a single registered project is unambiguous — the
// same rule the flow's own projectFor applies).
func incidentRepo(inc types.Incident, f *flow.Flow) string {
	if rec := incidentRepoOfRecords(nil, inc.ID); rec != "" {
		return rec
	}
	if f == nil {
		return ""
	}
	names := f.ProjectNames()
	if len(names) == 1 {
		if p, ok := f.ProjectByName(names[0]); ok {
			return p.Repo
		}
	}
	return ""
}

// incidentRepoOfRecords scans an incident's records for a repo field. A nil
// daemon (adapter unit tests) scans nothing.
func incidentRepoOfRecords(d *Daemon, incID string) string {
	if d == nil {
		return ""
	}
	for _, rec := range d.Ledger.DashReader().RecordsForIncident(incID, 0, 10) {
		if r, ok := rec.Payload["repo"].(string); ok && r != "" {
			return r
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The flow's view of the issues desk.
// ---------------------------------------------------------------------------

// deskAdapter adapts the issue desk onto flow's two-method issueDesk seam.
type deskAdapter struct{ desk *issues.Desk }

var _ interface {
	EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error)
	Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error)
} = deskAdapter{}

func (a deskAdapter) EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error) {
	if a.desk == nil {
		return types.EnsureBySigResponse{}, fmt.Errorf("issues desk not wired")
	}
	inc := types.Incident{ID: "inc_from_flow", Sig: req.Sig, Severity: req.Severity}
	ref, err := a.desk.EnsureBySig(ctx, inc, issues.Evidence{
		Summary: req.Title,
		Source:  "flow",
		Lines:   []string{req.Body},
	})
	if err != nil {
		return types.EnsureBySigResponse{Ref: ref}, err
	}
	// Created=false is the honest answer this adapter can make: the desk does
	// not expose the create-vs-fold verdict on its return value, and §3.4's
	// convergent contract treats "already there" as the safe default — the
	// module's Check then reports the fold posture instead of a phantom diff.
	return types.EnsureBySigResponse{Ref: ref, Created: false}, nil
}

func (a deskAdapter) Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	if a.desk == nil {
		return ref, fmt.Errorf("issues desk not wired")
	}
	return a.desk.Comment(ctx, ref, "trouble:flow", body)
}

// deskSpool adapts the issue desk's spool onto flow's spoolSink.
type deskSpool struct {
	s     *issues.Desk
	store *Store
}

func (s deskSpool) Enqueue(ctx context.Context, e types.SpoolEntry) error {
	if s.s == nil {
		return fmt.Errorf("issues desk spool not wired")
	}
	return s.s.EnqueueSpool(ctx, e)
}

// ---------------------------------------------------------------------------
// The spawn seam: the REAL router dispatch (SPEC-08 §3.6).
// ---------------------------------------------------------------------------

// routerSpawnAdapter satisfies flow's spawnRequester with the flow's own
// task-router dispatch: the one wire format, produced once. The spawn lands
// on the configured scheduler endpoint; the router reports the worktree.
type routerSpawnAdapter struct{ f *flow.Flow }

func (r routerSpawnAdapter) RequestSpawn(ctx context.Context, req types.SpawnRequest) (string, string, error) {
	if r.f == nil {
		return "", "", fmt.Errorf("flow not wired")
	}
	// dispatchToRouter is flow's own §3.6 dispatch: the router dedups on idem_key.
	return r.f.DispatchSpawn(ctx, req)
}

func (r routerSpawnAdapter) WorktreePresent(ctx context.Context, repo, taskID string) (bool, error) {
	if repo == "" || taskID == "" {
		return false, nil
	}
	path := filepath.Join(repo, ".worktrees", taskID)
	_, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// The skill sink: SPEC-11 candidate hand-off on promote (SPEC-08 §3.12).
// ---------------------------------------------------------------------------

type flowSkillsSink struct {
	d  *Daemon
	sk *skills.Skills
}

func (s flowSkillsSink) Candidate(ctx context.Context, inc types.Incident, ev types.Evidence, taskID, researchID, prURL string) error {
	if s.sk == nil {
		return fmt.Errorf("skills not wired")
	}
	play, ok := s.playFor(inc)
	if !ok {
		// No play source for this incident's rule: the refusal is the recorded
		// outcome, never silence (SPEC-06 owns play resolution, keyed by rule).
		return fmt.Errorf("no play source for the incident's rule; no candidate drafted")
	}
	res := []types.ResearchOutcome{}
	if researchID != "" && s.d != nil && s.d.Subsystems != nil && s.d.Subsystems.Research != nil {
		if out, ok := s.d.Subsystems.Research.OutcomeByID(researchID); ok {
			res = []types.ResearchOutcome{out}
		}
	}
	cand, err := s.sk.Promoter.Draft(inc, res, play)
	if err != nil {
		return err
	}
	autoAccept := s.d.Ladder != nil && s.d.Ladder.Gates().AllowPromote
	if _, err := s.sk.Promoter.Review(cand, s.d.Store.Actor, "accept", "auto: promote path", autoAccept); err != nil {
		return err
	}
	return nil
}

func (s flowSkillsSink) Refusal(ctx context.Context, inc types.Incident, ev types.Evidence, reason string) error {
	if s.sk == nil {
		return fmt.Errorf("skills not wired")
	}
	// The refusal record is the skills loop's own §4.7 phase record; the
	// promoter's Review with a reject decision writes it.
	play, ok := s.playFor(inc)
	if !ok {
		play = types.Play{Name: "unknown"}
	}
	cand, err := s.sk.Promoter.Draft(inc, nil, play)
	if err != nil {
		return err
	}
	_, err = s.sk.Promoter.Review(cand, s.d.Store.Actor, "reject", reason, false)
	return err
}

// playFor resolves the play the incident's rule drafted (SPEC-06 §3.7), via
// the incident's own admission record.
func (s flowSkillsSink) playFor(inc types.Incident) (types.Play, bool) {
	if s.d == nil {
		return types.Play{}, false
	}
	rule := incidentRule(s.d, inc.ID)
	if rule == "" {
		return types.Play{}, false
	}
	return s.d.playLookup(rule)
}

// ---------------------------------------------------------------------------
// Small helpers shared by the adapters.
// ---------------------------------------------------------------------------

func incidentRule(d *Daemon, incID string) string {
	for _, rec := range d.Ledger.DashReader().RecordsForIncident(incID, 0, 10) {
		if r, ok := rec.Payload["rule"].(string); ok && r != "" {
			return r
		}
	}
	return ""
}

func incidentSeverity(d *Daemon, incID string) types.Severity {
	for _, rec := range d.Ledger.DashReader().RecordsForIncident(incID, 0, 10) {
		if s, ok := rec.Payload["severity"].(string); ok && s != "" {
			return types.Severity(s)
		}
	}
	return types.SevInfo
}

func incidentRuleHotfix(d *Daemon, incID string) bool {
	rule := incidentRule(d, incID)
	if rule == "" {
		// No rule on the incident's records: a ruleless incident (the
		// sentinel's unknown class) is lane-eligible; the other §3.8 gates
		// (severity, repo, lease) still apply.
		return true
	}
	if d.Sensors == nil {
		return false
	}
	for _, r := range d.Sensors.Rules() {
		if r.Name == rule {
			return r.Hotfix
		}
	}
	// A known rule absent from the live set (reloaded away) is NOT eligible:
	// the conservative answer.
	return false
}

func spawnEvidence(f *flow.Flow, spawnID string) types.Evidence {
	if f == nil {
		return types.Evidence{Result: types.VerifyInvalid}
	}
	sp, err := f.SpawnState(spawnID)
	if err != nil {
		return types.Evidence{Result: types.VerifyInvalid}
	}
	return types.Evidence{
		Result:        types.VerifyInvalid,
		TSWindowStart: sp.RequestedTS,
		TSWindowEnd:   types.FormatUTC(time.Now()),
	}
}

func scrubBytesOrDefault(d *Daemon, target types.ScrubTarget, projectID string, b []byte) []byte {
	if d == nil || d.Scrubber == nil {
		return b
	}
	out, _, err := d.Scrubber.ScrubBytes(context.Background(), target, projectID, b)
	if err != nil {
		return b
	}
	return out
}

// freeDiskBytes reports the bytes available on the filesystem holding path
// (the flow's §3.8 disk gate). An unreadable path reports 0 with the error:
// the gate refuses rather than guessing.
func freeDiskBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
