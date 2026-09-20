package dashboard

import (
	"fmt"
	"net"

	"github.com/trouble-agent/trouble/internal/types"
)

// Config is the resolved form of the SPEC-10 §3.4 table: one field per
// dashboard.* key (the field name being the key's last element) plus the
// trust-zone inputs of §2.4 (Mandate, ProxyTrusted, ProxyCIDRs, PublicOrigin,
// ProjectScope) and the project count the project_scope rule needs.
//
// Every key has a default (see DefaultConfig) and no fleet value is compiled
// in. The composition root (SPEC-12's config resolution) is expected to pass
// fully-resolved values; normalize() fills only fields that are still zero.
type Config struct {
	// §3.4 keys.
	Bind                 string         // dashboard.bind — listener address
	Port                 int            // dashboard.port — listener port
	Mandate              string         // dashboard.mandate — "proxy" | "tailnet", required off loopback
	Identity             string         // dashboard.identity — the §2.5 seam selector ("token" in v0.1)
	TokenFile            string         // dashboard.token_file — 0600 secret store (§3.2)
	PollMS               int            // dashboard.poll_ms — content-partial poll interval
	StripPollMS          int            // dashboard.strip_poll_ms — accelerator interval
	StallAlertS          int            // dashboard.stall_alert_s — stale banner threshold
	HealthLoopbackExempt bool           // dashboard.health_loopback_exempt
	ReadOnly             bool           // dashboard.read_only — refuse all POSTs
	AllowResume          bool           // dashboard.allow_resume — gate kill-switch clearing
	AllowFull            bool           // dashboard.allow_full — gate mode:"full"
	PageLimit            int            // dashboard.page_limit — rows per page/partial (max 500)
	MaxBodyBytes         int64          // dashboard.max_body_bytes — POST body cap
	ReadRPS              float64        // dashboard.rate.read_rps
	ReadBurst            int            // dashboard.rate.read_burst
	WriteRPS             float64        // dashboard.rate.write_rps
	WriteBurst           int            // dashboard.rate.write_burst
	AuthFailLimit        int            // dashboard.auth_fail_limit
	AuthFailWindow       types.Duration // dashboard.auth_fail_window
	MemPressurePct       int            // dashboard.mem_pressure_pct — RSS share that sheds poll load
	ProxyTrusted         bool           // dashboard.proxy_trusted
	ProxyCIDRs           []string       // dashboard.proxy_cidrs
	PublicOrigin         string         // dashboard.public_origin — exact scheme://host[:port]
	ProjectScope         []string       // dashboard.project_scope
	MaxProjects          int            // configured [[projects]] count (the §2.4 >1-project rule)

	// TokenLenSanity enforces the `tdt_`+43 grammar at load and mint time;
	// zero means the §3.2 default (47) is applied by normalize.
	TokenLenSanity int
}

// DefaultConfig is the SPEC-10 §3.4 default column.
func DefaultConfig() Config {
	return Config{
		Bind:                 "127.0.0.1",
		Port:                 7644,
		Identity:             "token",
		TokenFile:            "~/.config/trouble/dashboard-tokens.json",
		PollMS:               2000,
		StripPollMS:          1000,
		StallAlertS:          90,
		HealthLoopbackExempt: true,
		PageLimit:            100,
		MaxBodyBytes:         4096,
		ReadRPS:              20,
		ReadBurst:            60,
		WriteRPS:             5,
		WriteBurst:           10,
		AuthFailLimit:        10,
		AuthFailWindow:       "60s",
		MemPressurePct:       80,
	}
}

// normalize fills zero fields with the §3.4 defaults and clamps PageLimit to
// its 500-row cap. HealthLoopbackExempt defaults to true; because it is a
// bool, the composition root must pass the resolved value (DefaultConfig
// provides it) — normalize cannot distinguish deliberate false from unset.
func (c *Config) normalize() {
	d := DefaultConfig()
	if c.Bind == "" {
		c.Bind = d.Bind
	}
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.Identity == "" {
		c.Identity = d.Identity
	}
	if c.TokenFile == "" {
		c.TokenFile = d.TokenFile
	}
	if c.PollMS == 0 {
		c.PollMS = d.PollMS
	}
	if c.StripPollMS == 0 {
		c.StripPollMS = d.StripPollMS
	}
	if c.StallAlertS == 0 {
		c.StallAlertS = d.StallAlertS
	}
	if c.PageLimit == 0 {
		c.PageLimit = d.PageLimit
	}
	if c.PageLimit > 500 {
		c.PageLimit = 500
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = d.MaxBodyBytes
	}
	if c.ReadRPS == 0 {
		c.ReadRPS = d.ReadRPS
	}
	if c.ReadBurst == 0 {
		c.ReadBurst = d.ReadBurst
	}
	if c.WriteRPS == 0 {
		c.WriteRPS = d.WriteRPS
	}
	if c.WriteBurst == 0 {
		c.WriteBurst = d.WriteBurst
	}
	if c.AuthFailLimit == 0 {
		c.AuthFailLimit = d.AuthFailLimit
	}
	if c.AuthFailWindow == "" {
		c.AuthFailWindow = d.AuthFailWindow
	}
	if c.MemPressurePct == 0 {
		c.MemPressurePct = d.MemPressurePct
	}
}

// ValidateConfig implements the SPEC-10 §2.4 startup validation. All failures
// are fail-loud, before the listener opens: there is no warn-and-continue
// path for an admin UI exposed by accident.
//
// Cross-area codes are used verbatim: TROUBLE-LIFECYCLE-001 for the two
// configuration shapes that would create a multi-tenant exposure (§2.4, edge
// case 14).
func ValidateConfig(cfg Config) error {
	cfg.normalize()
	loop := isLoopbackAddr(cfg.Bind)
	if !loop {
		if cfg.Mandate == "" {
			// TROUBLE-DASHBOARD-006: non-loopback bind without a mandate.
			return &dashError{Code: types.CodeDashboard006, HTTP: 500, Message: "non-loopback bind requires a mandate", Detail: "mandate_required"}
		}
		if cfg.MaxProjects > 1 && len(cfg.ProjectScope) == 0 {
			return &dashError{Code: types.CodeLifecycle001, HTTP: 500, Message: "project_scope required for multi-project exposure", Detail: "project_scope_required"}
		}
		if cfg.PublicOrigin == "" {
			return &dashError{Code: types.CodeLifecycle001, HTTP: 500, Message: "public_origin required off loopback", Detail: "public_origin_required"}
		}
	}
	return nil
}

// isLoopbackAddr reports whether addr names the loopback interface only
// (127.0.0.0/8, ::1 or a loopback hostname).
func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// An unresolved hostname cannot be proven loopback; treat as
		// non-loopback (default-deny).
		return false
	}
	return ip.IsLoopback()
}

// ipIsLoopback reports whether ip is a loopback address (used for the live
// bind re-check of §2.4 and the /health.json exemption).
func ipIsLoopback(ip net.IP) bool {
	return ip != nil && ip.IsLoopback()
}

// errf is a small fmt.Errorf wrapper to keep error construction terse.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
