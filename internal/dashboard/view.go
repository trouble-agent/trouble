package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// View models — SPEC-10 §3.1. These package-private types never appear in an
// exported signature, an HTTP body or a ledger payload. Every string that
// reaches a template passes through clean() (strings.ToValidUTF8) and every
// title through truncRunes (200 runes, §6.7/§6.9): the dashboard is not the
// scrubber (SPEC-02 is), but it must not be the place where invalid bytes
// become an XSS or a blank page either.

// maxTitleRunes is the layout truncation limit of §6.7 (render-time only — the
// ledger keeps the full string).
const maxTitleRunes = 200

// incidentRow is one row of the open-incident table.
type incidentRow struct {
	Inc      string
	Title    string
	Severity types.Severity
	State    types.LadderState
	Rung     types.Rung
	AgeS     float64
	Age      string // display form of AgeS
	LastAgeS float64
	LastAge  string // display form of LastAgeS ("—" when unknown)
	HasAge   bool
	Sig      string
}

// groupRow is one row of the group table.
type groupRow struct {
	ID           string
	Sig          string
	SigShort     string
	Title        string
	Count        uint64
	RateMin      float64
	Rate         string
	LastSeenTS   string
	LastSeen     string
	ReleaseRange []string
	Releases     string
	Source       string
	IncidentID   string
}

// ruleRow is one row of the rules table.
type ruleRow struct {
	Name        string
	Source      types.SigSource
	EntryRung   types.Rung
	Severity    types.Severity
	Enabled     bool
	LastFireTS  string
	LastFire    string
	Fires       uint64
	Suppressed  uint64
	Breaker     types.BreakerState
	BreakerOpen string
}

// breakerRow is one row of the breaker table.
type breakerRow struct {
	Scope     string
	State     types.BreakerState
	OpenUntil string
	Trips     int
	Reason    string
}

// timelineEntry is one <li> of the incident timeline.
type timelineEntry struct {
	TS      string
	Kind    string
	ActorID string
	Summary string
	Inc     string
}

// budgetPanel is the /partials/budget payload (row 18).
type budgetPanel struct {
	RW        types.RuntimeWatermarks
	Autonomy  types.AutonomyGates
	Version   string
	GitSHA    string
	Binary    string
	RSS       string
	Ledger    string
	Spool     string
	Events    string
	Worktrees int
}

// healthStrip is the accelerator strip's payload (§2.1.2 row 12).
type healthStrip struct {
	Seq      uint64
	StallS   float64
	Status   string
	Mode     string
	Kill     bool
	Denied   uint64
	CSRF     uint64
	RL       uint64
	RenderTS string
	Banner   bool
}

// countersView is the §2.1 row 1 counter block.
type countersView struct {
	IncidentsOpen int
	GroupsOpen    int
	EventsPerMin  float64
}

// sanitizers -----------------------------------------------------------------

// clean replaces invalid UTF-8 with U+FFFD (§6.9) and drops NUL bytes (which
// a terminal-oriented ledger line should never carry).
func clean(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	if strings.ContainsRune(s, '\x00') {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// truncRunes truncates a title at 200 runes for layout (§6.7).
func truncRunes(s string) string {
	if utf8.RuneCountInString(s) <= maxTitleRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxTitleRunes]) + "…"
}

// sigShort is the compact sig form used in table cells.
func sigShort(sig string) string {
	sig = clean(sig)
	if len(sig) <= 24 {
		return sig
	}
	return sig[:24] + "…"
}

// fmtTS reduces an RFC3339 timestamp to its time-of-day part for table cells;
// unparsable values render verbatim so the operator sees the raw value.
func fmtTS(ts string) string {
	if ts == "" {
		return "—"
	}
	t, err := types.ParseUTC(ts)
	if err != nil {
		return clean(ts)
	}
	return t.Format("15:04:05")
}

// ageSeconds computes the age of a timestamp at now; ok=false when unknown.
func ageSeconds(ts string, now time.Time) (float64, bool) {
	if ts == "" {
		return 0, false
	}
	t, err := types.ParseUTC(ts)
	if err != nil {
		return 0, false
	}
	d := now.Sub(t).Seconds()
	if d < 0 {
		d = 0
	}
	return d, true
}

// fmtAge renders an age in seconds for a table cell.
func fmtAge(s float64, ok bool) string {
	if !ok {
		return "—"
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%.0fs", s)
	case s < 3600:
		return fmt.Sprintf("%.0fm", s/60)
	default:
		return fmt.Sprintf("%.1fh", s/3600)
	}
}

// fmtRate renders a per-minute rate.
func fmtRate(r float64) string { return fmt.Sprintf("%.1f/min", r) }

// row builders --------------------------------------------------------------

// incidentRows maps open incidents (newest-updated first) into table rows and
// returns the index seq the fragment was rendered from.
func (s *server) incidentRows(ctx context.Context, limit int) ([]incidentRow, uint64) {
	now := s.now()
	incs := s.deps.Index.OpenIncidents(limit)
	rows := make([]incidentRow, 0, len(incs))
	for i := range incs {
		rows = append(rows, s.incidentRow(incs[i], now))
	}
	return rows, s.deps.Index.LastSeq()
}

// incidentRowsSince is the since=<seq> variant (row 13).
func (s *server) incidentRowsSince(ctx context.Context, since uint64, limit int) ([]incidentRow, uint64) {
	now := s.now()
	incs := s.deps.Index.OpenIncidentsSince(since, limit)
	rows := make([]incidentRow, 0, len(incs))
	for i := range incs {
		rows = append(rows, s.incidentRow(incs[i], now))
	}
	return rows, s.deps.Index.LastSeq()
}

func (s *server) incidentRow(inc types.Incident, now time.Time) incidentRow {
	age, ok := ageSeconds(inc.OpenedTS, now)
	if !ok {
		age, _ = ageSeconds(inc.UpdatedTS, now)
	}
	row := incidentRow{
		Inc:      clean(inc.ID),
		Title:    truncRunes(clean(inc.ID)),
		Severity: inc.Severity,
		State:    inc.State,
		Rung:     inc.Rung,
		AgeS:     age,
		Age:      fmtAge(age, true),
		Sig:      clean(inc.Sig),
	}
	if age2, ok := s.deps.Index.LastEventAge(inc.Sig, now); ok {
		row.LastAgeS, row.HasAge = age2, true
	}
	row.LastAge = fmtAge(row.LastAgeS, row.HasAge)
	return row
}

// groupRows maps the ranked group table (row 4).
func (s *server) groupRows(ctx context.Context, limit int) ([]groupRow, uint64) {
	st := s.deps.Index.GroupsSince(0, limit)
	rows := make([]groupRow, 0, len(st))
	for i := range st {
		rows = append(rows, groupRowOf(st[i]))
	}
	rows = rankGroups(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, s.deps.Index.LastSeq()
}

// groupRowsSince is the since=<seq> variant (row 15).
func (s *server) groupRowsSince(ctx context.Context, since uint64, limit int) ([]groupRow, uint64) {
	st := s.deps.Index.GroupsSince(since, limit)
	rows := make([]groupRow, 0, len(st))
	for i := range st {
		rows = append(rows, groupRowOf(st[i]))
	}
	return rows, s.deps.Index.LastSeq()
}

func groupRowOf(g types.GroupStat) groupRow {
	// GroupStat carries no release range (that field lives on the Group
	// projection, which /groups/{id} renders); the list view leaves it empty.
	var rel []string
	title := clean(g.Title)
	if title == "" {
		title = clean(g.Digest)
	}
	return groupRow{
		ID:           clean(g.GroupID),
		Sig:          clean(g.Sig),
		SigShort:     sigShort(g.Sig),
		Title:        truncRunes(title),
		Count:        g.Count,
		RateMin:      g.Rate1m,
		Rate:         fmtRate(g.Rate1m),
		LastSeenTS:   clean(g.LastSeenTS),
		LastSeen:     fmtTS(g.LastSeenTS),
		ReleaseRange: rel,
		Releases:     strings.Join(rel, "…"),
		Source:       clean(g.Source),
		IncidentID:   clean(g.IncidentID),
	}
}

// rankGroups orders group rows by rate, then count, then id (deterministic).
func rankGroups(rows []groupRow) []groupRow {
	out := make([]groupRow, len(rows))
	copy(out, rows)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if b.RateMin > a.RateMin ||
				(b.RateMin == a.RateMin && b.Count > a.Count) ||
				(b.RateMin == a.RateMin && b.Count == a.Count && b.ID < a.ID) {
				out[j-1], out[j] = out[j], out[j-1]
				continue
			}
			break
		}
	}
	return out
}

// ruleRows joins the SPEC-03 rule snapshot with RuleStats and breaker state
// (row 6/16).
func (s *server) ruleRows() []ruleRow {
	breakers := s.breakerByScope()
	now := s.now()
	rows := make([]ruleRow, 0, len(s.deps.Rules))
	for _, rl := range s.deps.Rules {
		row := ruleRow{
			Name:      truncRunes(clean(rl.Name)),
			Source:    rl.Source,
			EntryRung: rl.EntryRung,
			Severity:  rl.Severity,
			Enabled:   rl.Enabled,
		}
		if s.deps.RuleStats != nil {
			lastFire, fires, suppressed := s.deps.RuleStats(rl.Name)
			row.LastFireTS = clean(lastFire)
			row.Fires = fires
			row.Suppressed = suppressed
			if age, ok := ageSeconds(lastFire, now); ok {
				row.LastFire = fmtAge(age, true) + " ago"
			} else if lastFire != "" {
				row.LastFire = clean(lastFire)
			} else {
				row.LastFire = "never"
			}
		} else {
			row.LastFire = "never"
		}
		if b, ok := breakers["rule:"+rl.Name]; ok {
			row.Breaker = b.State
			row.BreakerOpen = clean(b.OpenUntil)
		} else {
			row.Breaker = types.BreakerClosed
		}
		rows = append(rows, row)
	}
	return rows
}

// breakerRows maps the breaker registry (row 7/17).
func (s *server) breakerRows() []breakerRow {
	var src []types.Breaker
	if s.deps.Breakers != nil {
		src = s.deps.Breakers()
	}
	rows := make([]breakerRow, 0, len(src))
	for _, b := range src {
		rows = append(rows, breakerRow{
			Scope:     clean(b.Scope),
			State:     b.State,
			OpenUntil: clean(b.OpenUntil),
			Trips:     b.Trips,
			Reason:    truncRunes(clean(b.Reason)),
		})
	}
	return rows
}

func (s *server) breakerByScope() map[string]types.Breaker {
	out := map[string]types.Breaker{}
	if s.deps.Breakers == nil {
		return out
	}
	for _, b := range s.deps.Breakers() {
		out[b.Scope] = b
	}
	return out
}

// timelineRows maps ledger records into timeline entries (row 14).
func (s *server) timelineRows(inc string, since uint64, limit int) ([]timelineEntry, uint64) {
	recs := s.deps.Index.RecordsForIncident(inc, since, limit)
	rows := make([]timelineEntry, 0, len(recs))
	for i := range recs {
		rows = append(rows, timelineEntryOf(recs[i]))
	}
	return rows, s.deps.Index.LastSeq()
}

func timelineEntryOf(rec types.Record) timelineEntry {
	return timelineEntry{
		TS:      fmtTS(rec.TS),
		Kind:    clean(string(rec.Kind)),
		ActorID: clean(rec.Actor.ID),
		Summary: truncRunes(clean(recordSummary(rec))),
		Inc:     clean(rec.Inc),
	}
}

// recordSummary renders a one-line payload summary without echoing arbitrary
// payload structure: known transition/reason fields first, then kind+sig.
func recordSummary(rec types.Record) string {
	p := rec.Payload
	if p != nil {
		if tr, ok := p["transition"].(string); ok && tr != "" {
			s := "transition " + tr
			if rs, ok := p["reason"].(string); ok && rs != "" {
				s += " · " + rs
			}
			if res, ok := p["resolution"].(string); ok && res != "" {
				s += " · " + res
			}
			if via, ok := p["via"].(string); ok && via != "" {
				s += " · via " + via
			}
			return s
		}
		if st, ok := p["state"].(string); ok && st != "" {
			return "state " + st
		}
		if ec, ok := p["error_code"].(string); ok && ec != "" {
			return "error " + ec
		}
	}
	if rec.Sig != "" {
		return string(rec.Kind) + " " + sigShort(rec.Sig)
	}
	return string(rec.Kind)
}

// sensorList returns the SPEC-03 sensor snapshot for /rules, applying the
// §6.13 "no sample yet" rule so an enabled sensor that has not produced a
// sample is visible as degraded instead of absent.
func (s *server) sensorList() []types.SensorHealth {
	if s.deps.Sensors == nil {
		return nil
	}
	in := s.deps.Sensors()
	out := make([]types.SensorHealth, 0, len(in))
	for _, sh := range in {
		if sh.LastSuccessTS == "" && sh.LastEventTS == "" {
			sh.Enabled = true
			sh.Degraded = true
			if sh.Reason == "" {
				sh.Reason = "no sample yet"
			}
		}
		out = append(out, sh)
	}
	return out
}

// budgetData assembles the runtime watermark panel (row 18).
func (s *server) budgetData() budgetPanel {
	var rw types.RuntimeWatermarks
	if s.deps.Watermarks != nil {
		rw = s.deps.Watermarks()
	}
	var gates types.AutonomyGates
	if s.deps.Autonomy != nil {
		gates = s.deps.Autonomy.Gates()
	}
	version, gitSHA := "", ""
	if s.deps.Version != nil {
		version, gitSHA, _, _ = s.deps.Version()
	}
	return budgetPanel{
		RW:        rw,
		Autonomy:  gates,
		Version:   clean(version),
		GitSHA:    clean(gitSHA),
		Binary:    fmtLen(rw.BinaryBytes),
		RSS:       fmtLen(rw.RSSBytes),
		Ledger:    fmtLen(rw.LedgerBytes),
		Spool:     fmtLen(rw.SpoolBytes),
		Events:    fmt.Sprintf("%.1f/min", rw.EventsPerMin),
		Worktrees: rw.Worktrees,
	}
}

// stripData assembles the accelerator strip (row 12) from the injected health
// assembly plus the dashboard's own refusal counters.
func (s *server) stripData(ctx context.Context) healthStrip {
	hr := s.deps.Health(ctx)
	return s.stripFrom(hr)
}

// stripDataWith is stripData with the autonomy gates overridden by a just-
// written value (row 11's refreshed fragment).
func (s *server) stripDataWith(ctx context.Context, gates types.AutonomyGates) healthStrip {
	hr := s.deps.Health(ctx)
	hr.Autonomy = gates
	return s.stripFrom(hr)
}

func (s *server) stripFrom(hr types.HealthResponse) healthStrip {
	now := s.now()
	seq := s.deps.Index.LastSeq()
	s.noteStrip(seq, hr.LedgerStallS, now)
	st := healthStrip{
		Seq:      seq,
		StallS:   hr.LedgerStallS,
		Status:   clean(hr.Status),
		Mode:     clean(string(hr.Autonomy.Mode)),
		Kill:     hr.Autonomy.KillSwitch,
		Denied:   s.counters.denied.Load(),
		CSRF:     s.counters.csrf.Load(),
		RL:       s.counters.rl.Load(),
		RenderTS: s.renderTS(),
		Banner:   s.bannerState(),
	}
	if st.Status == "" {
		st.Status = "unknown"
	}
	if st.Mode == "" {
		st.Mode = "unknown"
	}
	return st
}

// noteStrip updates the §2.6 stale-render tracker: identical seqs accumulate,
// and the banner turns on when three consecutive identical seqs coincide with
// a stall counter at or above stall_alert_s.
func (s *server) noteStrip(seq uint64, stallS float64, now time.Time) {
	s.stallMu.Lock()
	defer s.stallMu.Unlock()
	if seq == s.stall.LastSeq {
		s.stall.Repeat++
	} else {
		s.stall.LastSeq = seq
		s.stall.Repeat = 0
	}
	s.stall.LastOK = types.FormatUTC(now)
	s.stall.Banner = s.stall.Repeat >= 3 && stallS >= float64(s.cfg.StallAlertS)
}

func (s *server) bannerState() bool {
	s.stallMu.Lock()
	defer s.stallMu.Unlock()
	return s.stall.Banner
}

// healthStall returns the lifecycle-provided ledger stall in seconds — never
// recomputed from wall-clock deltas (§2.9, health.go).
func (s *server) healthStall(ctx context.Context) float64 {
	return s.deps.Health(ctx).LedgerStallS
}

// renderTS is the RFC3339 UTC millisecond render stamp every partial carries.
func (s *server) renderTS() string { return types.FormatUTC(s.now()) }
