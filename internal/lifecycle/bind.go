package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// bindProbe is a resolved listener and its held net.Listener.
type bindProbe struct {
	Name     string
	Bind     string
	Host     string
	Port     int
	Listener net.Listener
}

// AuthForm names the ingestion auth surface (SPEC-12 §3.7).
type AuthForm string

const (
	AuthLoopback AuthForm = "loopback"
	AuthToken    AuthForm = "token"
	AuthProxy    AuthForm = "proxy"
)

// PreflightBinds resolves both listeners, validates the bind matrix, and keeps
// the listeners open (SPEC-12 §3.2). Returns TROUBLE-LIFECYCLE-003 on refusal.
func PreflightBinds(cfg Config) ([]bindProbe, error) {
	declared := []struct {
		name   string
		bind   string
		ingest bool
	}{
		{"dashboard", cfg.Dashboard.Bind, false},
		{"ingest", cfg.Ingest.Bind, true},
	}

	seen := make(map[string]bool)
	var probes []bindProbe
	for _, d := range declared {
		host, portStr, err := splitHostPort(d.bind)
		if err != nil || host == "" {
			return nil, fmt.Errorf("%w: %s bind %q has empty host", types.CodeLifecycle003, d.name, d.bind)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("%w: %s bind %q has invalid port", types.CodeLifecycle003, d.name, d.bind)
		}
		pair := host + ":" + portStr
		if seen[pair] {
			return nil, fmt.Errorf("%w: duplicate listener %s and another on %s", types.CodeLifecycle003, d.name, pair)
		}
		seen[pair] = true

		if d.ingest {
			if !isLoopbackHost(host) && cfg.Ingest.AdvertisedHost == "" {
				return nil, fmt.Errorf("%w: non-loopback ingest.bind %q requires ingest.advertised_host", types.CodeLifecycle003, d.bind)
			}
			if isPublicHost(host) && cfg.Ingest.Auth.PublicRequireProxy && cfg.Ingest.Auth.NonloopbackMode != "proxy" {
				return nil, fmt.Errorf("%w: public ingest.bind %q requires proxy mode", types.CodeLifecycle003, d.bind)
			}
		}

		lc := net.ListenConfig{}
		ln, err := lc.Listen(context.Background(), "tcp", d.bind)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot bind %s %q: %v (hint: ss -tlnp | grep %d)", types.CodeLifecycle003, d.name, d.bind, err, port)
		}
		probes = append(probes, bindProbe{Name: d.name, Bind: d.bind, Host: host, Port: port, Listener: ln})
	}
	return probes, nil
}

// isLoopbackHost reports whether host is a loopback name or address.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "ip6-localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isPublicHost(host string) bool {
	if host == "" || host == "0.0.0.0" || host == "::" || host == "::0" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // hostname: treat as not-public for this check
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast()
}

// ZoneOf returns the network zone of a bind/source/auth-form tuple.
func ZoneOf(bind, source string, authForm AuthForm) string {
	host, _, _ := splitHostPort(bind)
	if host == "" {
		host = bind
	}
	if isLoopbackHost(host) {
		return "loopback"
	}
	if strings.Contains(source, ".tail") || strings.HasSuffix(source, ".ts.net") {
		return "tailnet"
	}
	if authForm == AuthProxy {
		return "public"
	}
	if isPublicHost(host) {
		return "public"
	}
	return "lan"
}

// ScanProcCmdline scans /proc/self/cmdline for secret-shaped material using the
// SPEC-02 mandatory rule set (SPEC-12 §3.2). Returns TROUBLE-LIFECYCLE-013.
func ScanProcCmdline() error {
	b, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		return fmt.Errorf("%w: cannot read /proc/self/cmdline: %v", types.CodeLifecycle013, err)
	}
	b = bytes.ReplaceAll(b, []byte{0}, []byte{'\n'})
	if err := scrub.MandatoryScan(context.Background(), b); err != nil {
		return fmt.Errorf("%w: secret-shaped material in /proc/self/cmdline: %v", types.CodeLifecycle013, err)
	}
	return nil
}

// releaseBinds closes held listeners (test helper).
func releaseBinds(probes []bindProbe) {
	for _, p := range probes {
		if p.Listener != nil {
			_ = p.Listener.Close()
		}
	}
}
