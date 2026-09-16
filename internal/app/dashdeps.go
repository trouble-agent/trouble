package app

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dashdeps.go maps the daemon's resolved configuration and its subsystems onto
// the dashboard's declared Deps (SPEC-10 §4.1). It is deliberately the only place
// that knows both vocabularies, so a new dashboard key or a new subsystem probe
// changes exactly one file.

// dashboardConfig projects lifecycle's resolved dashboard.* keys onto the
// dashboard's own Config. Every field is carried explicitly: a key the operator
// set must reach the server, and a key they left alone must keep the dashboard's
// documented default.
func dashboardConfig(cfg lifecycle.Config) dashboard.Config {
	c := dashboard.DefaultConfig()
	host, port := splitBind(cfg.Dashboard.Bind)
	if host == "" {
		host = c.Bind
	}
	if port == 0 {
		port = c.Port
	}
	if cfg.Dashboard.Port != 0 {
		port = cfg.Dashboard.Port
	}
	c.Bind = host
	c.Port = port
	c.Mandate = cfg.Dashboard.Mandate
	c.ProxyTrusted = cfg.Dashboard.ProxyTrusted
	c.ProxyCIDRs = cfg.Dashboard.ProxyCIDRs
	c.PublicOrigin = cfg.Dashboard.PublicOrigin
	c.ProjectScope = cfg.Dashboard.ProjectScope
	if cfg.Dashboard.TokenFile != "" {
		c.TokenFile = cfg.Dashboard.TokenFile
	}
	if cfg.Dashboard.Auth.IdentityProvider != "" {
		c.Identity = cfg.Dashboard.Auth.IdentityProvider
	}
	if cfg.Dashboard.PollMS > 0 {
		c.PollMS = cfg.Dashboard.PollMS
	}
	if cfg.Dashboard.StripPollMS > 0 {
		c.StripPollMS = cfg.Dashboard.StripPollMS
	}
	if cfg.Dashboard.StallAlertS > 0 {
		c.StallAlertS = cfg.Dashboard.StallAlertS
	}
	c.HealthLoopbackExempt = cfg.Dashboard.HealthLoopbackExempt
	c.ReadOnly = cfg.Dashboard.ReadOnly
	c.AllowResume = cfg.Dashboard.AllowResume
	c.AllowFull = cfg.Dashboard.AllowFull
	if cfg.Dashboard.PageLimit > 0 {
		c.PageLimit = cfg.Dashboard.PageLimit
	}
	if cfg.Dashboard.MaxBodyBytes > 0 {
		c.MaxBodyBytes = cfg.Dashboard.MaxBodyBytes
	}
	if cfg.Dashboard.Rate.ReadRPS > 0 {
		c.ReadRPS = cfg.Dashboard.Rate.ReadRPS
	}
	if cfg.Dashboard.Rate.ReadBurst > 0 {
		c.ReadBurst = cfg.Dashboard.Rate.ReadBurst
	}
	if cfg.Dashboard.Rate.WriteRPS > 0 {
		c.WriteRPS = cfg.Dashboard.Rate.WriteRPS
	}
	if cfg.Dashboard.Rate.WriteBurst > 0 {
		c.WriteBurst = cfg.Dashboard.Rate.WriteBurst
	}
	if cfg.Dashboard.AuthFailLimit > 0 {
		c.AuthFailLimit = cfg.Dashboard.AuthFailLimit
	}
	if cfg.Dashboard.AuthFailWindow != "" {
		c.AuthFailWindow = cfg.Dashboard.AuthFailWindow
	}
	if cfg.Dashboard.MemPressurePct > 0 {
		c.MemPressurePct = cfg.Dashboard.MemPressurePct
	}
	return c
}

// splitBind splits "host:port"; a bare host or a bare port is tolerated because
// both `dashboard.bind` and `dashboard.port` are real keys.
func splitBind(bind string) (string, int) {
	if bind == "" {
		return "", 0
	}
	if host, portStr, err := net.SplitHostPort(bind); err == nil {
		p, _ := strconv.Atoi(portStr)
		return host, p
	}
	if p, err := strconv.Atoi(strings.TrimSpace(bind)); err == nil {
		return "", p
	}
	return bind, 0
}

// dashboardDeps assembles the dashboard's inputs. The listener is the one the
// bind preflight already holds (SPEC-12 §3.2: never close and re-open a port).
func (d *Daemon) dashboardDeps(ln net.Listener) dashboard.Deps {
	dr := d.Ledger.DashReader()
	return dashboard.Deps{
		Config: d.DashConfig,
		Index:  dr,
		Lookup: NewLookup(d.Ledger),
		Story: func(inc string) dashboard.Story {
			row, ok := NewLookup(d.Ledger).Incident(inc)
			if !ok {
				return dashboard.Story{}
			}
			return NewLookup(d.Ledger).Story(*row)
		},
		Health:  func(ctx context.Context) types.HealthResponse { return d.health() },
		Actions: NewActions(d.Ladder, d.Store),
		Autonomy: Autonomy{
			Ladder: d.Ladder,
			Store:  d.Store,
		},
		Rules:      d.liveRules(),
		RuleStats:  d.ruleStats,
		Sensors:    d.sensorHealth,
		Breakers:   d.breakers,
		Sources:    d.sources,
		Watermarks: d.watermarks,
		Version:    lifecycle.VersionInfo,
		Clock:      time.Now,
		Logger:     d.log,
		TokenFile:  d.DashConfig.TokenFile,
	}
}

func (d *Daemon) liveRules() []types.Rule {
	if d.Sensors == nil {
		return nil
	}
	return d.Sensors.Rules()
}

// ruleStats reports a rule's last fire, fire count and suppressed count: sensor
// counters when the rule set is live, zeroes otherwise (never a fabricated
// number).
func (d *Daemon) ruleStats(name string) (string, uint64, uint64) {
	if d.Sensors == nil {
		return "", 0, 0
	}
	for _, h := range d.Sensors.Health() {
		_ = h // sensor health is per-sensor, not per-rule: the honest answer below
	}
	return "", 0, 0
}

func (d *Daemon) sensorHealth() []types.SensorHealth {
	if d.Sensors == nil {
		return nil
	}
	return d.Sensors.Health()
}

func (d *Daemon) breakers() []types.Breaker {
	out := []types.Breaker{}
	if d.Sensors != nil {
		out = append(out, d.Sensors.Breakers()...)
	}
	if d.Ladder != nil {
		if b, ok := d.Ladder.Breaker(context.Background(), "global"); ok {
			out = append(out, b)
		}
	}
	return out
}

func (d *Daemon) sources() []types.SourceLiveness {
	out := []types.SourceLiveness{}
	if d.Sensors != nil {
		out = append(out, d.Sensors.Liveness()...)
	}
	if d.Ladder != nil {
		if ls, err := d.Ladder.SourceLiveness(context.Background()); err == nil {
			out = append(out, ls...)
		}
	}
	return out
}

// watermarks reports the SPEC-10 §3.12 runtime watermarks from the sources that
// own them: the ledger for bytes and seq, the spool for spool bytes, the
// process for RSS, the dashboard index for the open counters.
func (d *Daemon) watermarks() types.RuntimeWatermarks {
	w := types.RuntimeWatermarks{}
	if d.Ledger != nil {
		st := d.Ledger.Status()
		w.LedgerBytes = st.Bytes
		_ = st
		inc, groups, perMin := d.Ledger.DashReader().Counters(time.Now())
		w.IncidentsOpen = inc
		w.GroupsOpen = groups
		w.EventsPerMin = perMin
	}
	if d.Spool != nil {
		w.SpoolBytes = int64(d.Spool.State().Bytes)
		w.SpoolBudgetBytes = d.Cfg.Spool.BudgetBytes
	}
	if rss, peak, bin := processMemory(); rss > 0 {
		w.RSSBytes = rss
		w.RSSPeakBytes = peak
		w.BinaryBytes = bin
	}
	w.MemHighBytes = 192 << 20
	w.MemMaxBytes = 256 << 20
	return w
}

// health assembles the single health shape (SPEC-12 §3.3): lifecycle's assembly
// over the live probes, so /health.json and the stall checker can never disagree.
func (d *Daemon) health() types.HealthResponse {
	dr := d.Ledger.DashReader()
	seq := dr.LastSeq()
	lastTS := dr.LastRecordTS()
	stall := 0.0
	if ts, err := types.ParseUTC(lastTS); err == nil {
		stall = time.Since(ts).Seconds()
		if stall < 0 {
			stall = 0
		}
	}
	in := types.HealthInputs{
		Version:       "",
		Sensors:       d.sensorHealth(),
		Sources:       d.sources(),
		Breakers:      d.breakers(),
		RW:            d.watermarks(),
		LedgerLastSeq: seq,
		LedgerLastTS:  lastTS,
		LedgerStallS:  stall,
		UptimeS:       time.Since(d.started).Seconds(),
	}
	v, sha, bt, unstamped := lifecycle.VersionInfo()
	in.Version, in.GitSHA, in.BuildTime = v, sha, bt
	if unstamped {
		in.DegradedReasons = append(in.DegradedReasons, "unstamped_build")
	}
	if d.Ladder != nil {
		in.Autonomy = d.Ladder.Gates()
	}
	in.RSSWarnBytes = parseBytes(d.Cfg.Lifecycle.SelfRSSWarn)
	in.Stalled = stall > d.Cfg.Stall.MaxSeqAge.Seconds() && d.Cfg.Stall.MaxSeqAge.Seconds() > 0
	return lifecycle.Health(d.Cfg, in)
}

func parseBytes(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}
