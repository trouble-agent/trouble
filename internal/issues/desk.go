package issues

import (
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Deps carries every collaborator of the desk. Nothing in this package opens a
// socket except through a driver's HTTP client, and the ledger is the only
// writer of records.
//
// Additions to the §2.2 signature, all recorded in the run report:
//   - Scan rebuilds the anchor index and the caps from the ledger at boot. §3.2
//     and §3.8 require both rebuilds but §2.2 passes no reader.
//   - ProjectOf resolves the project a filing is attributed to. Neither Incident
//     nor Group carries a project_id, and §3.4/§3.10 both key the cap counters
//     and the duckbrain namespace on it.
//   - Group/Incident/OpenBoardRow/OpenBrief are the §3.11 quiet-close gates'
//     inputs (SPEC-07/SPEC-08 own the state; the desk must not import them).
type Deps struct {
	Ledger     Writer
	Scan       Scanner
	Scrub      Scrubber
	Clock      Clock
	HTTPClient *http.Client
	HostID     string
	Actor      types.Actor
	StateRoot  string

	ProjectOf    func(inc types.Incident) string
	Group        func(ctx context.Context, sig string) (types.Group, bool, error)
	Incident     func(ctx context.Context, sig string) (types.Incident, bool, error)
	OpenBoardRow func(ctx context.Context, sig string) (bool, error)
	OpenBrief    func(ctx context.Context, sig string) (bool, error)
	LastSeq      func() uint64
}

// anchorEntry is the in-memory anchor for one (driver, sig, project) (§3.2).
type anchorEntry struct {
	key          string
	driver       string
	sig          string
	project      string
	ref          types.IssueRef
	idem         string
	createdTS    string
	updatedTS    string
	manual       bool
	recurrences  []string
	reopenCount  int
	lastComment  time.Time
	spooled      bool
	canaryMarker bool
}

// Desk is the issue desk.
type Desk struct {
	cfg   types.IssueDeskConfig
	deps  Deps
	clk   Clock
	caps  *capCounters
	spool *spoolStore

	mu      sync.Mutex
	drivers map[string]types.IssueDriver
	drvCfg  map[string]types.IssueDriverConfig
	order   []string // enabled drivers, primary first
	anchors map[string]*anchorEntry
	locks   map[string]*sync.Mutex
	acks    map[string]time.Time
	handled map[string]bool
	health  map[string]*healthState
	gaps    map[string]*gapState
	seq     uint64
	rand    *rand.Rand

	degraded bool
}

// healthState tracks the §3.6 probe state machine.
type healthState struct {
	consecutive int
	failed      bool
	lastOK      bool
	lastRecord  time.Time
	lastProbe   time.Time
	last        types.DriverHealth
}

// gapState is one open driver outage, closed on recovery (§3.6 step 2).
type gapState struct {
	openedTS string
	replayed int
	dropped  int
}

// New builds the desk: it validates the config (so an unknown driver name is a
// boot rejection), constructs the enabled drivers, rebuilds the anchors and the
// cap counters from the ledger, and starts degraded — never failed — when the
// primary driver is not usable at boot (§3.4).
//
// A config with `enabled = false` (§3.4a) builds the desk and nothing else: no
// driver is constructed, no boot probe leaves the process, and every operation
// answers with the disabled reason (requireEnabled) instead of a driver-name
// error that would send an operator looking at the wrong key.
func New(cfg types.IssueDeskConfig, deps Deps) (*Desk, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	d := &Desk{
		cfg:     cfg,
		deps:    deps,
		clk:     deps.Clock,
		drivers: map[string]types.IssueDriver{},
		drvCfg:  map[string]types.IssueDriverConfig{},
		anchors: map[string]*anchorEntry{},
		locks:   map[string]*sync.Mutex{},
		acks:    map[string]time.Time{},
		handled: map[string]bool{},
		health:  map[string]*healthState{},
		gaps:    map[string]*gapState{},
		rand:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if d.clk == nil {
		d.clk = &systemClock{}
	}
	now := d.now
	if deps.StateRoot != "" {
		d.spool = newSpoolStore(spoolRoot(deps.StateRoot), cfg, now)
	} else {
		// No state root: the spool is memory-only. The composition root always
		// passes one; tests may not.
		d.spool = &spoolStore{root: "", cfg: cfg, now: now, memOnly: true}
	}
	d.caps = newCapCounters(cfg.Caps, now)

	if !cfg.Enabled {
		// SPEC-09 §3.4a: the desk is OFF. A disabled desk is BUILT and idle —
		// the subsystem row reads built, not refused — and holds no driver:
		// nothing is constructed, no credential is read and no boot probe leaves
		// the process. The config's github block is left enabled (it is not
		// silently switched off here) so that ENABLING the desk without
		// owner/repo is still refused by ValidateConfig with the key it is
		// missing ("driver github needs owner and repo (SPEC-09 §3.9.1)")
		// instead of building a desk that cannot file.
		return d, nil
	}

	for _, dc := range cfg.Drivers {
		if !dc.Enabled {
			continue
		}
		factory, ok := factoryFor(dc.Name)
		if !ok {
			return nil, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false,
				"driver %q is not registered in this build", dc.Name)
		}
		drv, err := factory(dc, d, deps.HTTPClient)
		if err != nil {
			return nil, err
		}
		d.drivers[dc.Name] = drv
		d.drvCfg[dc.Name] = dc
		d.health[dc.Name] = &healthState{}
	}
	d.order = append(d.order, cfg.PrimaryDriver)
	for _, dc := range cfg.Drivers {
		if dc.Enabled && dc.Name != cfg.PrimaryDriver {
			d.order = append(d.order, dc.Name)
		}
	}
	d.rebuild()
	// §3.4: a primary driver that is not healthy-capable at boot starts the desk
	// degraded (a record, not a boot failure).
	if drv, ok := d.drivers[cfg.PrimaryDriver]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		h, err := drv.Healthcheck(ctx)
		cancel()
		if err != nil || !h.OK {
			d.degraded = true
			if d.health[cfg.PrimaryDriver] == nil {
				d.health[cfg.PrimaryDriver] = &healthState{}
			}
			st := d.health[cfg.PrimaryDriver]
			st.consecutive = d.bootProbeFailures()
			st.failed = true
			d.healthTransition(cfg.PrimaryDriver, st.consecutive, asError(orProbeError(err, h)))
		}
	}
	return d, nil
}

func spoolRoot(stateRoot string) string {
	return stateRoot + "/spool/issues"
}

func (d *Desk) now() time.Time { return d.clk.Now() }

// requireEnabled is the answer of every desk operation while the desk is OFF
// (SPEC-09 §3.4a). A disabled desk holds no driver, so the alternative would be
// the driver-name error ("driver github is not enabled"), which names a key that
// is not what keeps the desk off — this one names the opt-in instead.
func (d *Desk) requireEnabled(op string) error {
	if d.cfg.Enabled {
		return nil
	}
	return newErr(types.CodeIssues003, ReasonConfig, 0, false,
		"the issue desk is disabled (issues.enabled = false, SPEC-09 §3.4a): %s needs an enabled desk — set a driver with owner/repo, or enable the local duckbrain driver", op)
}

// origin is the desk's provenance on every record it writes.
func (d *Desk) origin() types.Origin {
	return types.Origin{HostID: d.hostID(), Source: "issues"}
}

func (d *Desk) record(ctx context.Context, kind types.RecordKind, sig, inc string, payload map[string]any) (types.Record, error) {
	rec, err := d.deps.Ledger.Append(ctx, types.RecordDraft{
		Kind:    kind,
		Sig:     sig,
		Inc:     inc,
		Origin:  d.origin(),
		Actor:   d.deps.Actor,
		Payload: payload,
	})
	if err == nil {
		d.mu.Lock()
		if rec.Seq > d.seq {
			d.seq = rec.Seq
		}
		d.mu.Unlock()
	}
	return rec, err
}

// projectOf resolves the project a filing belongs to: the sentinel project id
// when the caller can supply one, else `host:<host_id>` for sensor findings
// (§3.4, §3.10).
func (d *Desk) projectOf(inc types.Incident) string {
	if d.deps.ProjectOf != nil {
		if p := d.deps.ProjectOf(inc); p != "" {
			return p
		}
	}
	return "host:" + d.hostID()
}

func (d *Desk) anchorKey(driver, sig, project string) string {
	return driver + "|" + sig + "|" + project
}

func (d *Desk) sigLock(key string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.locks[key]
	if !ok {
		l = &sync.Mutex{}
		d.locks[key] = l
	}
	return l
}

// projectFromRecord is the cap rebuild's project attribution.
func (d *Desk) projectFromRecord(r types.Record) string {
	if p := strPayload(r.Payload, "project"); p != "" {
		return p
	}
	return ""
}

// rebuild restores the in-memory state from the ledger: the anchor index
// (inside the retention window) and the cap counters (§3.2, §3.8). A restart can
// therefore neither duplicate an issue nor lift a cap.
func (d *Desk) rebuild() {
	if d.deps.Scan == nil {
		return
	}
	var recs []types.Record
	_ = d.deps.Scan.ScanFrom(0, func(r types.Record) bool {
		switch r.Kind {
		case types.KIssue:
			recs = append(recs, r)
		case types.KGap:
			if strPayload(r.Payload, "sensor") == "issues" {
				recs = append(recs, r)
			}
		}
		return true
	})
	d.caps.rebuild(recs)
	cutoff := d.now().Add(-24 * time.Hour)
	for _, r := range recs {
		if r.Kind != types.KIssue {
			continue
		}
		op := strPayload(r.Payload, "op")
		ts, err := types.ParseUTC(r.TS)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		driver := strPayload(r.Payload, "driver")
		sig := r.Sig
		project := strPayload(r.Payload, "project")
		if driver == "" || sig == "" {
			continue
		}
		key := d.anchorKey(driver, sig, project)
		a, ok := d.anchors[key]
		if !ok {
			a = &anchorEntry{key: key, driver: driver, sig: sig, project: project}
			d.anchors[key] = a
		}
		switch op {
		case "ensure", "replay":
			a.idem = strPayload(r.Payload, "idem_key")
			a.ref.Driver = driver
			a.ref.Sig = sig
			a.ref.ExternalID = strPayload(r.Payload, "external_id")
			a.ref.URL = strPayload(r.Payload, "url")
			if s := strPayload(r.Payload, "state"); s != "" {
				a.ref.State = s
			} else if boolPayload(r.Payload, "created") {
				a.ref.State = types.IssueOpen
			}
			if a.createdTS == "" {
				a.createdTS = r.TS
			}
			a.updatedTS = r.TS
			if a.ref.ID == "" {
				a.ref.ID = strPayload(r.Payload, "issue_id")
			}
		case "comment":
			if boolPayload(r.Payload, "commented") {
				a.ref.Comments++
				a.updatedTS = r.TS
				if l := strPayload(r.Payload, "line"); l != "" {
					a.recurrences = append(a.recurrences, l)
				}
			}
		case "close":
			a.ref.State = types.IssueClosed
			a.updatedTS = r.TS
		case "reopen":
			a.ref.State = types.IssueOpen
			a.reopenCount++
			a.updatedTS = r.TS
		case "link":
			if v := strPayload(r.Payload, "task_id"); v != "" {
				a.ref.TaskID = v
			}
			if v := strPayload(r.Payload, "research_id"); v != "" {
				a.ref.ResearchID = v
			}
		case "ack":
			if until := strPayload(r.Payload, "until"); until != "" {
				if t, err := types.ParseUTC(until); err == nil {
					d.acks[sig] = t
				}
			}
		}
		d.handled[strPayload(r.Payload, "idem_key")] = true
	}
}

// Anchor is the CLI lookup (`issues list --sig`) and the SPEC-08 ordering-race
// lookup. It never invents an id: a value exists only after a driver confirmed
// a ref.
func (d *Desk) Anchor(sig string) (types.IssueRef, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.anchors {
		if a.sig == sig && a.ref.ExternalID != "" {
			return a.ref, true
		}
	}
	return types.IssueRef{}, false
}

// AnchorAt is Anchor for one driver.
func (d *Desk) AnchorAt(driver, sig string) (types.IssueRef, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.anchors {
		if a.driver == driver && a.sig == sig && a.ref.ExternalID != "" {
			return a.ref, true
		}
	}
	return types.IssueRef{}, false
}

// EnqueueSpool places one foreign payload on the desk's durable queue: the
// export of §3.7 to the collaborators that hand work over (the flow's own
// pending-spawn replay rides the same store, so a restart replays both).
// A drop event is one gap record on the desk's own record path.
func (d *Desk) EnqueueSpool(ctx context.Context, e types.SpoolEntry) error {
	if d == nil || d.spool == nil {
		return newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "spool not built")
	}
	if err := d.requireEnabled("EnqueueSpool"); err != nil {
		// A disabled desk runs no replay loop (Run returns early before any
		// driver timer), so an entry accepted here would be "spooled" forever.
		// This spool is the DESK's queue, keyed by driver: Replay walks
		// d.order, so it lists no other tree, and DecodePayload accepts only
		// the desk's own operation shape. A collaborator with its own durable
		// queue owns that queue and the loop that drains it — SPEC-08 §3.9a is
		// the flow's, for exactly this reason. Refusing keeps every caller's
		// record honest.
		return err
	}
	drops, err := d.spool.Put("issue", e)
	for range drops {
		_, _ = d.record(ctx, types.KGap, "", "", map[string]any{
			"est_lost": 1, "cause": types.CauseQueueOverflow, "subsystem": "issues", "scope": "spool",
		})
	}
	return err
}

// EntryFor returns the anchor entry (read-only) for diagnostics and tests.
func (d *Desk) EntryFor(driver, sig, project string) (*anchorEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.anchors[d.anchorKey(driver, sig, project)]
	return a, ok
}

// EnsureBySig is the ladder entry point (AC-8): the primary driver files or
// folds, then every mirror=true driver files or folds the same sig.
func (d *Desk) EnsureBySig(ctx context.Context, inc types.Incident, ev Evidence) (types.IssueRef, error) {
	if err := d.requireEnabled("EnsureBySig"); err != nil {
		return types.IssueRef{}, err
	}
	project := d.projectOf(inc)
	opID := types.NewID(types.PEv)
	ref, err := d.ensureAt(ctx, d.cfg.PrimaryDriver, inc, ev, project, opID, false)
	if err != nil {
		return ref, err
	}
	for _, name := range d.order {
		dc, ok := d.drvCfg[name]
		if !ok || !dc.Mirror || name == d.cfg.PrimaryDriver {
			continue
		}
		mopID := types.NewID(types.PEv)
		if _, merr := d.ensureAt(ctx, name, inc, ev, project, mopID, false); merr != nil {
			// The primary is authoritative: a mirror failure is recorded and the
			// incident continues (§3.6 step 3 applies to every driver).
			continue
		}
	}
	return ref, nil
}

// ensureAt is the per-driver ensure path: anchor hit → fold; miss → cap check →
// driver call; transient failure → spool.
func (d *Desk) ensureAt(ctx context.Context, driver string, inc types.Incident, ev Evidence,
	project, opID string, replayed bool) (types.IssueRef, error) {

	drv, ok := d.drivers[driver]
	if !ok {
		return types.IssueRef{}, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "driver %q is not enabled", driver)
	}
	if err := d.checkSig(inc.Sig); err != nil {
		return types.IssueRef{}, err
	}
	dc := d.drvCfg[driver]
	idem := ensureIdem(driver, inc.Sig, inc.ID, opID)
	lock := d.sigLock(d.anchorKey(driver, inc.Sig, project))
	lock.Lock()
	defer lock.Unlock()

	if d.idemHandled(idem) {
		a, _ := d.EntryFor(driver, inc.Sig, project)
		if a != nil {
			return a.ref, nil
		}
		return types.IssueRef{}, newErr(types.CodeIssues010, ReasonDuplicateIdem, 0, false,
			"idempotency key %s was already handled", idem)
	}

	anchor, hit := d.EntryFor(driver, inc.Sig, project)
	if hit && anchor.ref.ExternalID != "" && anchor.ref.State != types.IssueClosed {
		return d.foldInto(ctx, drv, dc, anchor, inc, ev, idem, replayed)
	}
	if hit && anchor.ref.ExternalID != "" && anchor.ref.State == types.IssueClosed {
		// §3.11 reopen-on-recurrence: the same incident reopens the same issue.
		return d.reopen(ctx, drv, dc, anchor, inc, ev, idem)
	}

	// No anchor: the caps decide whether a create may happen at all (§3.8).
	if capTok, allow := d.caps.allowCreate(inc.Sig, project); !allow {
		d.caps.noteSuppressed()
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, map[string]any{
			"op": "cap", "driver": driver, "sig": inc.Sig, "inc": inc.ID, "project": project,
			"result": "capped", "cap": capTok, "window": windowFor(capTok, d.cfg.Caps),
			"count_in_window": 1, "error_code": string(types.CodeIssues004), "retryable": false,
		})
		return types.IssueRef{}, newErr(types.CodeIssues004, ReasonCap, 0, false,
			"issue cap %s reached for sig %s", capTok, inc.Sig)
	}

	title := d.TitleOf(inc, ev)
	body, redactions, err := d.scrubBody(ctx, d.BodyOf(inc, ev, driver), project)
	if err != nil {
		return types.IssueRef{}, err
	}
	req := types.EnsureBySigRequest{
		Sig:         inc.Sig,
		Title:       title,
		Body:        body,
		Labels:      d.labels(dc, inc, ev),
		DedupWindow: d.dedupWindowDur(dc),
		Severity:    inc.Severity,
	}

	var resp types.EnsureBySigResponse
	attempts, callErr := d.withRetry(ctx, driver, dc, "ensure", idem, func(c context.Context) error {
		out, err := drv.EnsureBySig(c, req)
		if err != nil {
			return err
		}
		resp = out
		return nil
	})
	payload := d.ensurePayload(driver, inc, idem, body, attempts, dc, redactions, project, hit)
	if callErr != nil {
		e := asError(callErr)
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["http_status"] = e.HTTPStatus
		if e.Retryable {
			payload["result"] = "spooled"
			d.spoolEnsure(ctx, driver, drv, inc, req, idem, project, opID, e, attempts)
			_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
			return types.IssueRef{}, e
		}
		payload["result"] = "failed"
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
		return types.IssueRef{}, e
	}

	ref := resp.Ref
	if ref.ID == "" {
		// The desk owns the local id; the driver owns the external one.
		ref.ID = types.NewID(types.PIss)
	}
	ref.Driver = driver
	ref.Sig = inc.Sig
	if ref.CreatedTS == "" {
		ref.CreatedTS = types.FormatUTC(d.now())
	}
	ref.UpdatedTS = types.FormatUTC(d.now())

	switch {
	case resp.Created:
		payload["result"] = "created"
		payload["created"] = true
		payload["commented"] = false
		d.caps.noteCreate(inc.Sig, project, d.now())
	case resp.Commented:
		payload["result"] = "folded"
		payload["created"] = false
		payload["commented"] = true
		d.caps.noteComment(inc.Sig, project, d.now())
	default:
		payload["result"] = "noop"
		payload["created"] = false
		payload["commented"] = false
	}
	payload["external_id"] = ref.ExternalID
	payload["url"] = ref.URL
	payload["state"] = ref.State
	payload["issue_id"] = ref.ID
	if payload["error_code"] == "" {
		delete(payload, "error_code")
	}
	_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)

	d.mu.Lock()
	a, ok := d.anchors[d.anchorKey(driver, inc.Sig, project)]
	if !ok {
		a = &anchorEntry{key: d.anchorKey(driver, inc.Sig, project), driver: driver, sig: inc.Sig, project: project}
		d.anchors[a.key] = a
	}
	a.ref = ref
	a.ref.State = types.IssueOpen
	a.idem = idem
	if a.createdTS == "" {
		a.createdTS = types.FormatUTC(d.now())
	}
	a.updatedTS = types.FormatUTC(d.now())
	d.handled[idem] = true
	d.mu.Unlock()
	return ref, nil
}

// foldInto folds a recurrence into the open anchor (§3.2): inside the dedup
// window a one-line fold comment, outside it a full recurrence block; either way
// Created=false and the comment cadence is bounded by comment_min_interval.
func (d *Desk) foldInto(ctx context.Context, drv types.IssueDriver, dc types.IssueDriverConfig,
	a *anchorEntry, inc types.Incident, ev Evidence, idem string, replayed bool) (types.IssueRef, error) {

	kind := "fold"
	line := d.FoldComment(inc, ev)
	windowExpired := false
	if a.updatedTS != "" {
		if ts, err := types.ParseUTC(a.updatedTS); err == nil {
			if d.now().Sub(ts) > d.dedupWindow(dc) {
				kind = "block"
				line = d.RecurrenceBlock(inc, ev)
				windowExpired = true
			}
		}
	}

	idemKey := commentIdem(a.ref.ID, inc.ID)
	payload := map[string]any{
		"op": "comment", "driver": a.driver, "sig": inc.Sig, "inc": inc.ID, "project": a.project,
		"idem_key": idemKey, "external_id": a.ref.ExternalID, "comment_kind": kind,
		"window_expired": windowExpired, "line": line,
		"created": false, "commented": false, "attempt": 1, "http_status": 0,
	}

	if d.acked(inc.Sig) {
		// Edge case 14: the incident and the anchor still update; the fold comment
		// is suppressed while the ack is active.
		d.caps.noteSuppressed()
		payload["result"] = "suppressed"
		payload["error_code"] = string(types.CodeIssues010)
		payload["reason"] = "ack_active"
		payload["retryable"] = false
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
		d.touchAnchor(a, ev)
		return a.ref, nil
	}
	if capTok, allow := d.caps.allowComment(inc.Sig, a.project); !allow {
		d.caps.noteSuppressed()
		payload["result"] = "suppressed"
		payload["error_code"] = string(types.CodeIssues010)
		payload["reason"] = capTok
		payload["retryable"] = false
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
		d.touchAnchor(a, ev)
		return a.ref, nil
	}

	body, _, err := d.scrubBody(ctx, line+"\n\n"+IdemMarker(idemKey), a.project)
	if err != nil {
		return a.ref, err
	}
	var ref types.IssueRef
	attempts, callErr := d.withRetry(ctx, a.driver, dc, "comment", idemKey, func(c context.Context) error {
		out, err := drv.Comment(c, a.ref, body)
		if err != nil {
			return err
		}
		ref = out
		return nil
	})
	payload["attempt"] = attempts
	if callErr != nil {
		e := asError(callErr)
		payload["result"] = "spooled"
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["http_status"] = e.HTTPStatus
		if e.Retryable {
			d.spoolComment(ctx, a.driver, drv, a.ref, inc, idemKey, body, a.project, e, attempts)
		} else {
			payload["result"] = "failed"
			if e.Code == types.CodeIssues007 {
				payload["result"] = "anchor_lost"
				d.invalidateAnchor(a)
			}
		}
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
		return a.ref, e
	}
	ref.Driver = a.driver
	ref.Sig = inc.Sig
	if ref.ID == "" {
		ref.ID = a.ref.ID
	}
	if ref.ExternalID == "" {
		ref.ExternalID = a.ref.ExternalID
	}
	ref.URL = a.ref.URL
	ref.State = types.IssueOpen
	ref.TaskID = a.ref.TaskID
	ref.ResearchID = a.ref.ResearchID
	payload["result"] = "folded"
	payload["commented"] = true
	payload["external_id"] = ref.ExternalID
	payload["http_status"] = 200
	_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
	d.caps.noteComment(inc.Sig, a.project, d.now())

	d.mu.Lock()
	a.ref = ref
	a.updatedTS = types.FormatUTC(d.now())
	a.recurrences = append(a.recurrences, line)
	if len(a.recurrences) > 20 {
		a.recurrences = a.recurrences[len(a.recurrences)-20:]
	}
	d.handled[idemKey] = true
	d.mu.Unlock()
	return ref, nil
}

func (d *Desk) touchAnchor(a *anchorEntry, ev Evidence) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a.updatedTS = types.FormatUTC(d.now())
	a.recurrences = append(a.recurrences, d.FoldComment(types.Incident{Sig: a.sig}, ev))
}

func (d *Desk) invalidateAnchor(a *anchorEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a.ref.ExternalID = ""
	a.ref.URL = ""
	a.ref.State = ""
}

// Comment appends one comment per distinct trigger key (§2.2). The trigger is a
// ledger rec_id, so a repeat of the same record is a no-op at the driver and at
// the desk.
func (d *Desk) Comment(ctx context.Context, ref types.IssueRef, trigger, body string) (types.IssueRef, error) {
	if err := d.requireEnabled("Comment"); err != nil {
		return ref, err
	}
	drv, ok := d.drivers[ref.Driver]
	if !ok {
		return ref, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "driver %q is not enabled", ref.Driver)
	}
	dc := d.drvCfg[ref.Driver]
	idemKey := commentIdem(ref.ID, trigger)
	if d.idemHandled(idemKey) {
		return ref, nil
	}
	scrubbed, _, err := d.scrubBody(ctx, body+"\n\n"+IdemMarker(idemKey), d.projectOf(types.Incident{ID: ref.Sig}))
	if err != nil {
		return ref, err
	}
	var out types.IssueRef
	attempts, callErr := d.withRetry(ctx, ref.Driver, dc, "comment", idemKey, func(c context.Context) error {
		r, err := drv.Comment(c, ref, scrubbed)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if callErr != nil {
		return ref, callErr
	}
	project := d.anchorProject(ref)
	d.mu.Lock()
	if a, ok := d.anchors[d.anchorKey(ref.Driver, ref.Sig, project)]; ok {
		a.ref = out
		a.updatedTS = types.FormatUTC(d.now())
	}
	d.handled[idemKey] = true
	d.mu.Unlock()
	_, _ = d.record(ctx, types.KIssue, ref.Sig, "", map[string]any{
		"op": "comment", "driver": ref.Driver, "sig": ref.Sig, "idem_key": idemKey,
		"result": "commented", "comment_kind": "manual", "created": false, "commented": true,
		"window_expired": false,
		"external_id":    out.ExternalID, "attempt": attempts, "http_status": 200, "error_code": "",
	})
	return out, nil
}

// Close closes the sig's issue (idempotent per §3.1).
func (d *Desk) Close(ctx context.Context, ref types.IssueRef, reason string) (types.IssueRef, error) {
	if err := d.requireEnabled("Close"); err != nil {
		return ref, err
	}
	drv, ok := d.drivers[ref.Driver]
	if !ok {
		return ref, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "driver %q is not enabled", ref.Driver)
	}
	dc := d.drvCfg[ref.Driver]
	idemKey := closeIdem(ref.ID, reason)
	if ref.State == types.IssueClosed {
		return ref, nil
	}
	var out types.IssueRef
	attempts, callErr := d.withRetry(ctx, ref.Driver, dc, "close", idemKey, func(c context.Context) error {
		r, err := drv.Close(c, ref, reason+"\n\n"+IdemMarker(idemKey))
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	payload := map[string]any{
		"op": "close", "driver": ref.Driver, "sig": ref.Sig, "inc": ref.ResearchID,
		"idem_key": idemKey, "reason": reason, "attempt": attempts,
	}
	if callErr != nil {
		e := asError(callErr)
		payload["result"] = "failed"
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["http_status"] = e.HTTPStatus
		_, _ = d.record(ctx, types.KIssue, ref.Sig, "", payload)
		return ref, e
	}
	payload["result"] = "closed"
	payload["state"] = out.State
	payload["http_status"] = 200
	payload["error_code"] = ""
	out.Driver = ref.Driver
	out.Sig = ref.Sig
	if out.ID == "" {
		out.ID = ref.ID
	}
	_, _ = d.record(ctx, types.KIssue, ref.Sig, "", payload)
	project := d.anchorProject(ref)
	d.mu.Lock()
	if a, ok := d.anchors[d.anchorKey(ref.Driver, ref.Sig, project)]; ok {
		a.ref = out
		a.ref.State = types.IssueClosed
		a.updatedTS = types.FormatUTC(d.now())
	}
	d.handled[idemKey] = true
	d.mu.Unlock()
	return out, nil
}

// Health is the SPEC-12 §3 health aggregator's per-driver probe surface (SPEC-10
// route 8). A probe younger than the driver's interval is served from cache, so
// the aggregator can poll without multiplying outbound calls.
func (d *Desk) Health(ctx context.Context) []types.DriverHealth {
	out := make([]types.DriverHealth, 0, len(d.order))
	for _, name := range d.order {
		out = append(out, d.probe(ctx, name))
	}
	return out
}

func (d *Desk) probe(ctx context.Context, name string) types.DriverHealth {
	drv, ok := d.drivers[name]
	if !ok {
		return types.DriverHealth{Driver: name, OK: false, Detail: "driver not enabled", RateLimitRemaining: -1}
	}
	dc := d.drvCfg[name]
	// Computed before the lock: healthInterval reads the spool and the anchors.
	interval := d.healthInterval(name)
	d.mu.Lock()
	h := d.health[name]
	// A healthy driver's probe is served from cache inside the interval so the
	// health aggregator can poll cheaply; a failing driver is always re-probed,
	// which is what makes `fail_after_probes` reachable.
	cached := h != nil && h.lastOK && !h.failed && !h.lastProbe.IsZero() && d.now().Sub(h.lastProbe) < interval
	prev := types.DriverHealth{}
	if h != nil {
		prev = h.last
	}
	d.mu.Unlock()
	if cached {
		return prev
	}
	tctx, cancel := context.WithTimeout(ctx, pauseTimeout(dc))
	defer cancel()
	got, err := drv.Healthcheck(tctx)
	if err != nil {
		e := asError(err)
		got = types.DriverHealth{Driver: name, OK: false, RateLimitRemaining: -1, Detail: e.Error()}
	}
	got.Driver = name
	if got.CheckedTS == "" {
		got.CheckedTS = types.FormatUTC(d.now())
	}
	if got.RateLimitRemaining == 0 && got.Detail == "" {
		got.RateLimitRemaining = -1
	}
	transition := ""
	d.mu.Lock()
	if d.health[name] == nil {
		d.health[name] = &healthState{}
	}
	st := d.health[name]
	now := d.now()
	st.lastProbe = now
	st.last = got
	if err != nil {
		st.consecutive++
		st.lastOK = false
		first := st.consecutive >= d.cfg.FailAfterProbes && !st.failed
		refresh := st.failed && now.Sub(st.lastRecord) >= 15*time.Minute
		if first || refresh {
			st.failed = true
			st.lastRecord = now
			d.degraded = true
			transition = "fail"
		}
	} else {
		if st.failed {
			transition = "recover"
		}
		st.failed = false
		st.consecutive = 0
		st.lastOK = true
	}
	consecutive := st.consecutive
	d.mu.Unlock()

	switch transition {
	case "fail":
		d.healthTransition(name, consecutive, asError(err))
	case "recover":
		d.recovered(name)
	}
	return got
}

// healthTransition records a failing probe (§3.6 steps 1, 2 and 3): one
// `issue` record, one opening `gap` record per outage, and the ladder-facing
// fact that the incident continues.
func (d *Desk) healthTransition(name string, consecutive int, e *Error) {
	detail := ReasonHealthcheck
	if e != nil {
		detail = sanitizeDetail(e.Error())
	}
	_, _ = d.record(context.Background(), types.KIssue, "", "", map[string]any{
		"op": "healthcheck", "driver": name, "ok": false, "detail": detail,
		"rate_limit_remaining": -1, "rate_limit_reset_ts": "",
		"consecutive_failures": consecutive, "error_code": string(types.CodeIssues009), "retryable": true,
	})
	d.mu.Lock()
	_, open := d.gaps[name]
	if !open {
		d.gaps[name] = &gapState{openedTS: types.FormatUTC(d.now())}
	}
	from := d.gaps[name].openedTS
	d.mu.Unlock()
	if !open {
		_, _ = d.record(context.Background(), types.KGap, "", "", map[string]any{
			"id": types.NewID(types.PEv), "sensor": "issues", "scope": name,
			"from_ts": from, "to_ts": from, "est_lost": -1, "cause": types.CauseDriverDown,
		})
	}
}

// recovered closes the outage gap (§3.6 step 5). The spool drain itself is the
// desk loop's replay tick, which runs before new work is accepted.
func (d *Desk) recovered(name string) {
	d.mu.Lock()
	st := d.health[name]
	last := types.DriverHealth{}
	if st != nil {
		last = st.last
	}
	g, had := d.gaps[name]
	delete(d.gaps, name)
	d.degraded = false
	d.mu.Unlock()
	_, _ = d.record(context.Background(), types.KIssue, "", "", map[string]any{
		"op": "healthcheck", "driver": name, "ok": true, "detail": "recovered",
		"rate_limit_remaining": last.RateLimitRemaining, "rate_limit_reset_ts": last.RateLimitResetTS,
		"consecutive_failures": 0, "error_code": "",
	})
	if had {
		_, _ = d.record(context.Background(), types.KGap, "", "", map[string]any{
			"id": types.NewID(types.PEv), "sensor": "issues", "scope": name,
			"from_ts": g.openedTS, "to_ts": types.FormatUTC(d.now()),
			"est_lost": g.replayed + g.dropped, "cause": types.CauseDriverDown, "closed": true,
		})
	}
}

func (d *Desk) driverFailed(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.health[name]
	return st != nil && st.failed
}

func (d *Desk) driverHealthy(name string) bool { return !d.driverFailed(name) }

// CapState is the `trouble issues health --json` surface.
func (d *Desk) CapState() types.IssueCapState {
	return d.caps.snapshot(d.cfg.PrimaryDriver)
}

// Degraded reports whether any driver is currently failed.
func (d *Desk) Degraded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.degraded
}

// Ack suppresses fold comments for a sig until the deadline; the incident is
// never acked (edge case 14).
func (d *Desk) Ack(ctx context.Context, ref types.IssueRef, until time.Duration) (types.IssueRef, error) {
	if until <= 0 {
		until = d.cfg.AckDefault.Std()
	}
	deadline := d.now().Add(until)
	d.mu.Lock()
	if prev, ok := d.acks[ref.Sig]; !ok || deadline.After(prev) {
		d.acks[ref.Sig] = deadline
	}
	d.mu.Unlock()
	_, err := d.record(ctx, types.KIssue, ref.Sig, "", map[string]any{
		"op": "ack", "driver": ref.Driver, "sig": ref.Sig, "result": "acked",
		"until": types.FormatUTC(deadline), "error_code": "",
	})
	return ref, err
}

func (d *Desk) acked(sig string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.acks[sig]
	if !ok {
		return false
	}
	if d.now().After(t) {
		delete(d.acks, sig)
		return false
	}
	return true
}

func (d *Desk) idemHandled(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handled[key]
}

func (d *Desk) anchorProject(ref types.IssueRef) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.anchors {
		if a.sig == ref.Sig && a.driver == ref.Driver {
			return a.project
		}
	}
	return "host:" + d.hostID()
}

// projectFor resolves a sig's project from the desk's own knowledge: the anchor
// entry when one exists, else the host bucket. It is the desk's answer to the
// duckbrain driver's key layout (which needs a project) and to the cap counters.
func (d *Desk) projectFor(sig string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.anchors {
		if a.sig == sig && a.project != "" {
			return a.project
		}
	}
	return "host:" + d.hostID()
}

// dedupWindow is the effective window for a driver, with the §3.4 clamp.
func (d *Desk) dedupWindow(dc types.IssueDriverConfig) time.Duration {
	if dc.DedupWindow != "" {
		if d := dc.DedupWindow.Std(); d >= time.Minute && d <= 24*time.Hour {
			return d
		}
	}
	if d := d.cfg.DedupWindow.Std(); d > 0 {
		return d
	}
	return 30 * time.Minute
}

// windowFor names the cap window in a cap record.
func windowFor(token string, caps types.IssueCaps) string {
	switch token {
	case "per_sig_creates":
		if caps.PerSigWindow != "" {
			return string(caps.PerSigWindow)
		}
		return "24h"
	case "per_project_creates_h", "per_project_comments_h", "global_creates_h", "global_comments_h":
		return "1h"
	case "per_project_creates_d", "global_creates_d":
		return "24h"
	case "per_sig_comments":
		return "1h"
	}
	return ""
}

func ensureIdem(driver, sig, inc, opID string) string {
	return "issue_ensure|" + driver + "|" + sig + "|" + inc + "|" + opID
}

func commentIdem(issID, trigger string) string {
	return "issue_comment|" + issID + "|" + trigger
}

func closeIdem(issID, reason string) string {
	return "issue_close|" + issID + "|" + reason
}

// scrubBody scrubs one blob against the desk's own target before it is called
// out or persisted (defence in depth for a user-supplied project name, §3.9.3).
func (d *Desk) scrubBody(ctx context.Context, body, project string) (string, int, error) {
	if d.deps.Scrub == nil {
		return body, 0, nil
	}
	out, res, err := d.deps.Scrub.ScrubBytes(ctx, types.TgIssue, project, []byte(body))
	if err != nil {
		return "", 0, err
	}
	return string(out), res.Redactions, nil
}

// labels renders the §3.9.2 label set: base, severity, source, extra. Labels are
// advisory and never a dedup key.
func (d *Desk) labels(dc types.IssueDriverConfig, inc types.Incident, ev Evidence) []string {
	out := make([]string, 0, len(dc.Labels)+len(dc.LabelsExtra)+2)
	out = append(out, dc.Labels...)
	if dc.SeverityLabels != nil {
		if l, ok := dc.SeverityLabels[string(inc.Severity)]; ok && l != "" {
			out = append(out, l)
		}
	}
	if dc.SourceLabels {
		if l := sourceLabel(ev.Source, inc.Sig); l != "" {
			out = append(out, l)
		}
	}
	out = append(out, dc.LabelsExtra...)
	return dedupeStrings(out)
}

func sourceLabel(source, sig string) string {
	tok := source
	if i := indexByte(tok, ':'); i >= 0 {
		tok = tok[:i]
	}
	if tok == "" {
		if s, err := types.ParseSig(sig); err == nil {
			tok = string(s.Source)
		}
	}
	switch tok {
	case string(types.SrcSentinel), string(types.SrcJournald), string(types.SrcPSI), string(types.SrcDBus),
		string(types.SrcDisk), string(types.SrcTimers), string(types.SrcInotify), string(types.SrcUnknown):
		if tok == string(types.SrcSentinel) {
			return "src:sentinel"
		}
		return "src:sensor"
	case string(types.SrcCollector):
		return "src:collector"
	case string(types.SrcGeneric):
		return "src:generic"
	}
	return ""
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// sanitizeDetail keeps a health detail to one scrubbed human line: a token, key
// or URL with a query string can never be part of it (§2.1).
func sanitizeDetail(s string) string {
	if i := indexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := indexByte(s, '?'); i >= 0 {
		if j := indexByte(s, ' '); j > i || j < 0 {
			if j < 0 {
				j = len(s)
			}
			s = s[:i] + " [query stripped]" + s[j:]
		}
	}
	return s
}

// checkSig rejects a malformed sig before any call (§2.1).
func (d *Desk) checkSig(sig string) error {
	if _, err := types.ParseSig(sig); err != nil {
		return newErr(types.CodeIssues003, ReasonValidation, 0, false, "%v", err)
	}
	return nil
}

func asError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	if err == nil {
		return nil
	}
	return wrapErr(types.CodeIssues001, ReasonTransient, 0, true, err)
}

// pauseTimeout bounds a healthcheck attempt by the driver's own timeout.
func pauseTimeout(dc types.IssueDriverConfig) time.Duration {
	if t := dc.Timeout.Std(); t > 0 {
		return t
	}
	return 15 * time.Second
}

// nowSeq is the ledger's last sequence, for the close reason line.
func (d *Desk) nowSeq() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deps.LastSeq != nil {
		return d.deps.LastSeq()
	}
	return d.seq
}
