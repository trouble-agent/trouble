package sentinel

import (
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// advertised_host_test.go pins the DSN-host gate of SPEC-04 §2.3/§2.3a and
// TRBL-017. The gate exists for ONE class: a DSN whose host a reporter cannot
// reach is a fleet-wide silent-no-report, because the DSN is baked into every
// deployed app. `localhost` is that class for every bind EXCEPT a loopback one —
// a loopback listener is reachable from this host alone, so the loopback name is
// exactly what every reporter that can reach it resolves, and it is the
// documented default of `ingest.advertised_host` (SPEC-12 §3.1).
//
// The matrix below therefore drives the same table through BOTH surfaces §2.3
// names — config load (NewServer) and DSN generation (GenerateDSN) — so the two
// cannot drift, and it keeps every genuinely unreachable host refused: an IP
// literal (including the bind address spelled as an address), a wildcard, the
// empty value, and the loopback name once the bind leaves loopback.

// dsnHostCase is one row of the §2.3a matrix.
type dsnHostCase struct {
	name   string
	bind   string
	host   string
	accept bool
}

func dsnHostCases() []dsnHostCase {
	return []dsnHostCase{
		// --- accepted: the loopback name while the bind IS loopback ---------
		{name: "loopback bind + localhost", bind: "127.0.0.1:7643", host: "localhost", accept: true},
		{name: "loopback bind + LOCALHOST", bind: "127.0.0.1:7643", host: "LOCALHOST", accept: true},
		{name: "loopback bind + trailing root dot", bind: "127.0.0.1:7643", host: "localhost.", accept: true},
		{name: "ipv6 loopback bind + localhost", bind: "[::1]:7643", host: "localhost", accept: true},
		{name: "bind spelled as the loopback name + localhost", bind: "localhost:7643", host: "localhost", accept: true},
		{name: "a loopback address in 127/8 + localhost", bind: "127.0.0.2:7643", host: "localhost", accept: true},
		{name: "loopback bind + a declared name", bind: "127.0.0.1:7643", host: "trouble.example.net", accept: true},
		// --- refused: the loopback name once the bind can be reached off-host -
		{name: "wildcard bind + localhost", bind: "0.0.0.0:7643", host: "localhost", accept: false},
		{name: "ipv6 wildcard bind + localhost", bind: "[::]:7643", host: "localhost", accept: false},
		{name: "LAN bind + localhost", bind: "10.0.0.5:7643", host: "localhost", accept: false},
		{name: "public bind + localhost", bind: "203.0.113.7:7643", host: "localhost", accept: false},
		{name: "wildcard bind + the root-dot spelling", bind: "0.0.0.0:7643", host: "localhost.", accept: false},
		{name: "LAN bind + the root-dot spelling", bind: "10.0.0.5:7643", host: "LocalHost.", accept: false},
		// --- refused at EVERY bind: the forms no reporter can resolve --------
		{name: "the bind address as an IP literal", bind: "127.0.0.1:7643", host: "127.0.0.1", accept: false},
		{name: "an unroutable IP literal", bind: "127.0.0.1:7643", host: "10.0.0.5", accept: false},
		{name: "an ipv6 literal", bind: "127.0.0.1:7643", host: "fd00::1", accept: false},
		{name: "the wildcard address", bind: "0.0.0.0:7643", host: "0.0.0.0", accept: false},
		{name: "the ipv6 wildcard", bind: "[::]:7643", host: "::", accept: false},
		{name: "the bracketed ipv6 wildcard", bind: "127.0.0.1:7643", host: "[::]", accept: false},
		{name: "the empty host", bind: "127.0.0.1:7643", host: "", accept: false},
	}
}

// TestAdvertisedHostMatrix drives every row through config load AND DSN
// generation: a row accepted by one surface and refused by the other is the
// drift §2.3's "refused at generation and at config load" forbids.
func TestAdvertisedHostMatrix(t *testing.T) {
	for _, tc := range dsnHostCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, func(c *Config) {
				c.Bind = tc.bind
				c.AdvertisedHost = tc.host
			})

			// Surface 1: config load (TROUBLE-SENTINEL-009 on refusal).
			srv, err := NewServer(cfg, &memSink{}, newTestScrubber(t, cfg.Projects))
			if err != nil {
				if tc.accept {
					t.Fatalf("NewServer refused bind %q with advertised_host %q: %v", tc.bind, tc.host, err)
				}
				if !isCode(err, types.CodeSentinel009) {
					t.Fatalf("NewServer refused with %v, want TROUBLE-SENTINEL-009", err)
				}
				if !containsCause(err, causeAdvertisedHost) {
					t.Fatalf("NewServer refusal %v does not carry cause %q", err, causeAdvertisedHost)
				}
				// The refusal must NAME the fix, not restate the rule: this is
				// what an operator reading the boot record acts on.
				if isLocalhostName(tc.host) && !strings.Contains(err.Error(), "declare a name") {
					t.Errorf("the localhost refusal does not name the fix: %v", err)
				}
			} else {
				if !tc.accept {
					t.Fatalf("NewServer ACCEPTED bind %q with advertised_host %q", tc.bind, tc.host)
				}
				if srv.spool != nil {
					_ = srv.spool.Close()
				}
			}

			// Surface 2: generation — the other half of the same rule.
			raw, gerr := GenerateDSN(cfg, cfg.Projects[0])
			if gerr != nil {
				if tc.accept {
					t.Fatalf("GenerateDSN refused bind %q with advertised_host %q: %v", tc.bind, tc.host, gerr)
				}
				if !isCode(gerr, types.CodeSentinel009) {
					t.Fatalf("GenerateDSN refused with %v, want TROUBLE-SENTINEL-009", gerr)
				}
				return
			}
			if !tc.accept {
				t.Fatalf("GenerateDSN minted %q for bind %q with advertised_host %q", raw, tc.bind, tc.host)
			}
			d, perr := ParseDSN(cfg, raw)
			if perr != nil {
				t.Fatalf("the DSN the config generates for %q does not parse back: %v", tc.host, perr)
			}
			if !cfg.acceptsHost(d.Host) {
				t.Fatalf("generated DSN host %q is outside the advertised set", d.Host)
			}
		})
	}
}

// TestLoopbackDSNHostServesIngest is the accepted half of TRBL-017 end to end:
// the documented default (`advertised_host` empty on a loopback bind, derived to
// "localhost" by SPEC-12 §3.1) must not merely pass validation — the DSN the
// config mints for that host must carry a real request through the listener and
// be recorded. Before the §2.3a exception the same config was refused with
// TROUBLE-SENTINEL-009 and the port never served, which is why the shipped
// example had to advertise a placeholder it could not resolve.
func TestLoopbackDSNHostServesIngest(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.Bind = "127.0.0.1:7643"
		c.AdvertisedHost = "localhost" // what SPEC-12 §3.1 derives for this bind
	})
	defer ts.close()

	// Premise: the derived host really is the loopback name, so a green result
	// below cannot be a test that advertised something else.
	if !isLocalhostName(ts.s.cfg.AdvertisedHost) {
		t.Fatalf("test premise: advertised host = %q, want the loopback name", ts.s.cfg.AdvertisedHost)
	}

	raw, err := GenerateDSN(ts.s.cfg, ts.s.cfg.Projects[0])
	if err != nil {
		t.Fatalf("GenerateDSN for the loopback DSN host: %v", err)
	}
	d, perr := ParseDSN(ts.s.cfg, raw)
	if perr != nil {
		t.Fatalf("ParseDSN(%q): %v", raw, perr)
	}
	if !isLocalhostName(d.Host) {
		t.Fatalf("generated DSN host = %q, want the loopback name", d.Host)
	}

	// The full DSN rides the §2.4 envelope_dsn form, so the request carries the
	// HOST and not only the key: this is the path that validates the advertised
	// host, and it is the one an SDK on a local app uses.
	ev := eventJSON(t, nil)
	body := envelopeBytes(t,
		map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "dsn": raw},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true, ContentType: "application/json"})
	resp := ts.post(t, "/api/1/envelope/", nil, body)
	if resp.StatusCode != 200 {
		t.Fatalf("POST the generated loopback DSN = %d %s", resp.StatusCode, readBody(t, resp))
	}
	id := respID(t, resp)
	if id == "" {
		t.Fatal("the generated loopback DSN produced no event id")
	}

	// The record exists and names the form, so the accept is not an empty 200.
	var recorded bool
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		if form, _ := rec.Payload["auth_form"].(string); form == fmtEnvelopeDSN {
			recorded = true
		}
	}
	if !recorded {
		t.Error("no event record carries the envelope_dsn auth form: the accepted DSN did not reach the ledger")
	}
}
