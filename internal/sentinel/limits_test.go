package sentinel

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestGzipBombIsRefused pins §3.7/§6.1: the decompressed cap is a limited
// reader, and the 100:1 ratio guard refuses a bomb past 100KB of output.
func TestGzipBombIsRefused(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	bomb := gzipBytes(t, bytes.Repeat([]byte("0"), 4<<20))
	body := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: []byte(`{"message":"x"}`), Length: true})
	_ = body

	resp := ts.post(t, "/api/1/envelope/", map[string]string{
		"X-Sentry-Auth":    ts.authHeader("1"),
		"Content-Encoding": "gzip",
	}, bomb)
	if resp.StatusCode != 413 {
		t.Fatalf("gzip bomb status = %d, want 413", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel003) {
		t.Fatalf("gzip bomb code = %q, want 003", got)
	}
	// 4MB of zeros compresses to ~4KB, so the ratio guard's threshold (~410KB
	// of output) is crossed long before the 1MB cap: the stream reports the
	// ratio branch. §3.7 pins the two causes separately and the response has to
	// name the one that bit.
	respBody := readBody(t, resp)
	if cause := firstCause(t, respBody); cause != causeCompressionRatio {
		t.Errorf("gzip bomb cause = %q, want %q (%s)", cause, causeCompressionRatio, respBody)
	}
	if ts.s.counters.countersReason("decompressed") == 0 {
		t.Error("reject_total{reason=decompressed} did not move")
	}
}

// TestRatioGuardRefusesHighlyCompressiblePayload pins the ratio guard: a body
// that expands past 100:1 once output passes 100KB is refused even when it stays
// under the absolute cap.
func TestRatioGuardRefusesHighlyCompressiblePayload(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	// 900KB of a highly compressible pattern: under the 1MB cap, far past 100:1.
	payload := bytes.Repeat([]byte("A"), 900*1024)
	compressed := gzipBytes(t, payload)
	resp := ts.post(t, "/api/1/envelope/", map[string]string{
		"X-Sentry-Auth":    ts.authHeader("1"),
		"Content-Encoding": "gzip",
	}, compressed)
	if resp.StatusCode != 413 {
		t.Fatalf("ratio-guard status = %d, want 413", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel003) {
		t.Fatalf("ratio-guard code = %q, want 003", got)
	}
	// 900KB of a repeated byte stays under the 1MB cap, so the only bound that
	// can refuse it is the ratio guard: the cause must say so (§3.7).
	ratioBody := readBody(t, resp)
	if cause := firstCause(t, ratioBody); cause != causeCompressionRatio {
		t.Errorf("ratio-guard cause = %q, want %q (%s)", cause, causeCompressionRatio, ratioBody)
	}
}

// TestInvalidGzipStreamIs004 pins §5's gzip row.
func TestInvalidGzipStreamIs004(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	resp := ts.post(t, "/api/1/envelope/", map[string]string{
		"X-Sentry-Auth":    ts.authHeader("1"),
		"Content-Encoding": "gzip",
	}, []byte("not-a-gzip-stream"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid gzip status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel004) {
		t.Fatalf("invalid gzip code = %q, want 004", got)
	}
	_ = readBody(t, resp)
}

// TestCompressedCapIsEnforcedWithoutContentLength pins §6.2: the compressed cap
// is enforced by counting bytes read, so a chunked body is refused mid-stream
// without buffering the rest.
func TestCompressedCapIsEnforcedWithoutContentLength(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.MaxEnvelopeCompressed = 4096 })
	defer ts.close()
	host := strings.TrimPrefix(ts.ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("z"), 64*1024)
	// A chunked request with no Content-Length: Transfer-Encoding: chunked.
	head := fmt.Sprintf("POST /api/1/envelope/ HTTP/1.1\r\nHost: %s\r\nX-Sentry-Auth: %s\r\nTransfer-Encoding: chunked\r\n\r\n", host, ts.authHeader("1"))
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("%x\r\n", len(payload)))); err != nil {
		t.Fatalf("write chunk header: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	// Terminate the chunked body so the server can answer; the cap must already
	// have been enforced while reading, long before the payload ends.
	if _, err := conn.Write([]byte("0\r\n\r\n")); err != nil {
		t.Fatalf("write terminator: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	buf, err := io.ReadAll(conn)
	if err != nil && len(buf) == 0 {
		t.Fatalf("read response: %v", err)
	}
	got := string(buf)
	if !strings.Contains(got, "413") {
		t.Fatalf("chunked over-cap response = %q, want 413", firstLine(got))
	}
	if !strings.Contains(got, string(types.CodeSentinel002)) {
		t.Fatalf("chunked over-cap response missing code 002: %q", firstLine(got))
	}
}

// TestClientAbortMidBody pins §6.3: a declared length that never arrives is 001
// (and never a partial ledger write).
func TestClientAbortMidBody(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	before := ts.sink.count()
	host := strings.TrimPrefix(ts.ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	head := fmt.Sprintf("POST /api/1/envelope/ HTTP/1.1\r\nHost: %s\r\nX-Sentry-Auth: %s\r\nContent-Length: 4096\r\n\r\n", host, ts.authHeader("1"))
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := conn.Write([]byte(`{"event_id":"x"}`)); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts.s.counters.rejects.Get() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ts.sink.count() != before {
		t.Fatalf("an aborted body wrote %d ledger records, want 0", ts.sink.count()-before)
	}
}

// TestConcurrencyCapQueuesThenRefuses pins §3.7: over max_concurrent a request is
// queued for at most 1s, then refused with 429 and the pinned header.
func TestConcurrencyCapQueuesThenRefuses(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.MaxConcurrent = 1
	})
	defer ts.close()
	ts.sink.setDelay(1500 * time.Millisecond)
	defer ts.sink.setDelay(0)

	done := make(chan struct{})
	go func() {
		_ = readBody(t, ts.postEvent(t, "1", fmt.Sprintf("%032x", 1)))
		close(done)
	}()
	// Give the first request the slot.
	time.Sleep(150 * time.Millisecond)
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 2))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Rate-Limits"); got != overloadHeader {
		t.Errorf("over-cap header = %q, want %q", got, overloadHeader)
	}
	_ = readBody(t, resp)
	<-done
}

// TestPerIPRateLimit pins §3.7's per-IP bucket and its header.
func TestPerIPRateLimit(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.PerIPRate = "60/min, burst 2"
	})
	defer ts.close()
	seen := 0
	for i := 0; i < 6; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 100+i))
		if resp.StatusCode == http.StatusTooManyRequests {
			if got := resp.Header.Get("X-Sentry-Rate-Limits"); got != ipHeader {
				t.Errorf("per-IP header = %q, want %q", got, ipHeader)
			}
			seen++
		}
		_ = readBody(t, resp)
	}
	if seen == 0 {
		t.Fatal("per-IP burst never refused a request")
	}
}

// TestXFFTrustPolicy pins §3.7: XFF is honored only from a trusted peer, using
// the rightmost-untrusted-hop algorithm.
func TestXFFTrustPolicy(t *testing.T) {
	// proxy_trust = loopback (the default): XFF is ignored, the peer is the
	// client, so a bare public key is accepted.
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/api/1/event/", strings.NewReader(`{"message":"x"}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Sentry-Auth", ts.authHeader("1"))
	resp, err := ts.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with proxy_trust=loopback XFF must be ignored: status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// proxy_trust = explicit-list with the loopback peer trusted: XFF is honored,
	// the client is classified as public, and the request is accepted through the
	// proxy with the public key alone (§3.7's public row: Sentry's model).
	ts2 := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.ProxyTrust = proxyTrustExplicit
		c.TrustedProxies = []string{"127.0.0.1/8", "::1/128"}
	})
	defer ts2.close()
	ip, zone := ts2.s.clientIP("127.0.0.1:4444", "203.0.113.9")
	if ip.String() != "203.0.113.9" || zone != zonePublic {
		t.Fatalf("proxied client classification = (%v,%s), want (203.0.113.9,public)", ip, zone)
	}
	req2, _ := http.NewRequest(http.MethodPost, ts2.ts.URL+"/api/1/event/", strings.NewReader(`{"message":"x"}`))
	req2.Header.Set("X-Forwarded-For", "203.0.113.9")
	req2.Header.Set("X-Sentry-Auth", ts2.authHeader("1"))
	resp2, err := ts2.ts.Client().Do(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("a proxied public client with a trusted proxy in front must be accepted: status %d (%s)",
			resp2.StatusCode, readBody(t, resp2))
	}
	_ = readBody(t, resp2)
	// The same client with NO trusted proxy in front is refused at the resolver
	// level (covered below): the bind matrix is what protects a direct bind.
}

// TestRightmostUntrustedHop pins the XFF algorithm directly.
func TestRightmostUntrustedHop(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.ProxyTrust = proxyTrustExplicit
		c.TrustedProxies = []string{"127.0.0.1/8", "10.0.0.0/8"}
	})
	defer ts.close()
	ip, zone := ts.s.clientIP("127.0.0.1:5555", "203.0.113.9, 10.0.0.7")
	if ip == nil || ip.String() != "203.0.113.9" {
		t.Fatalf("client IP = %v, want 203.0.113.9 (skip trusted hops, take the first untrusted)", ip)
	}
	if zone != zonePublic {
		t.Fatalf("zone = %q, want %q", zone, zonePublic)
	}
	// A trusted-only XFF chain leaves the peer as the client.
	ip, zone = ts.s.clientIP("127.0.0.1:5555", "10.0.0.7")
	if ip == nil || ip.String() != "127.0.0.1" {
		t.Fatalf("client IP = %v, want the peer when every hop is trusted", ip)
	}
	if zone != zoneLoopback {
		t.Fatalf("zone = %q, want loopback", zone)
	}
}

// TestBindMatrixRefusals pins §3.7's bind matrix at the resolver level: a
// non-loopback zone without a trusted proxy needs the project token, and a query
// key off loopback is refused.
func TestBindMatrixRefusals(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	// Project 2 has no secret: a LAN client cannot authenticate at all.
	m := authMaterial{form: fmtXSentryAuth, key: testPubB, hasKey: true}
	if _, err := ts.s.resolveMaterial(m, "2", zoneLAN, false, time.Now()); err == nil {
		t.Fatal("a bare public key from a LAN zone must be refused")
	} else if !containsCause(err, causeTokenRequired) {
		t.Fatalf("LAN refusal causes = %v, want token_required", err.Causes)
	}
	// With the right token it is accepted.
	m.secret, m.hasSecret = testSecretA, true
	if _, err := ts.s.resolveMaterial(authMaterial{form: fmtXSentryAuth, key: testPubA, secret: testSecretA, hasKey: true, hasSecret: true}, "1", zoneLAN, false, time.Now()); err != nil {
		t.Fatalf("LAN client with the project token must be accepted: %v", err)
	}
	// A trusted proxy in front lifts the token requirement.
	if _, err := ts.s.resolveMaterial(m, "1", zoneLAN, true, time.Now()); err != nil {
		t.Fatalf("a proxied request must not need the token: %v", err)
	}
	// A query-string key off loopback is refused on both transports.
	if _, err := ts.s.resolveMaterial(authMaterial{form: fmtQueryKey, key: testPubA, hasKey: true}, "1", zoneLAN, true, time.Now()); err == nil {
		t.Fatal("a query key off loopback must be refused")
	} else if !containsCause(err, causeQueryKeyRemote) {
		t.Fatalf("query-key refusal causes = %v, want query_key_remote", err.Causes)
	}
	// require_secret refuses a request without the secret (project 1 has one).
	ts3 := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.RequireSecret = true
		c.Projects = c.Projects[:1]
	})
	defer ts3.close()
	if _, err := ts3.s.resolveMaterial(authMaterial{form: fmtXSentryAuth, key: testPubA, hasKey: true}, "1", zoneLoopback, false, time.Now()); err == nil {
		t.Fatal("require_secret must refuse a request with no secret")
	}
}

// TestBootValidation pins §4.2 step 1: a name (never an IP or bind address) for
// advertised_host, https implying a terminator, 32-hex keys and quota > 0.
//
// The DSN-host rows are the §2.3a precondition, not a flat blacklist: the
// loopback NAME is refused while the bind is NOT loopback (a reporter on another
// host resolves it to itself and reports nowhere) and accepted while it is —
// the accept side, both surfaces, and the ingest round trip that proves it are
// TestAdvertisedHostMatrix and TestLoopbackDSNHostServesIngest in
// advertised_host_test.go.
func TestBootValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		code  types.ErrorCode
		cause string
	}{
		{"ip host", func(c *Config) { c.AdvertisedHost = "10.0.0.5" }, types.CodeSentinel009, causeAdvertisedHost},
		{"bind host", func(c *Config) { c.AdvertisedHost = "0.0.0.0" }, types.CodeSentinel009, causeAdvertisedHost},
		{"missing host", func(c *Config) { c.AdvertisedHost = "" }, types.CodeSentinel009, causeAdvertisedHost},
		{"localhost on a wildcard bind", func(c *Config) {
			c.Bind = "0.0.0.0:7643"
			c.AdvertisedHost = "localhost"
		}, types.CodeSentinel009, causeAdvertisedHost},
		{"localhost on a LAN bind", func(c *Config) {
			c.Bind = "10.0.0.5:7643"
			c.AdvertisedHost = "localhost"
		}, types.CodeSentinel009, causeAdvertisedHost},
		{"advertised_hosts entry is an IP literal", func(c *Config) {
			c.AdvertisedHosts = []string{"trouble.example.net", "10.0.0.5"}
		}, types.CodeSentinel009, causeAdvertisedHost},
		{"advertised_hosts entry is localhost off loopback", func(c *Config) {
			c.Bind = "0.0.0.0:7643"
			c.AdvertisedHosts = []string{"localhost"}
		}, types.CodeSentinel009, causeAdvertisedHost},
		{"https without terminator", func(c *Config) {
			c.Scheme = "https"
			c.ProxyTrust = proxyTrustNone
		}, types.CodeSentinel009, causeHTTPSNoTerminator},
		{"short public key", func(c *Config) { c.Projects[0].PublicKey = strings.Repeat("a", 16) }, types.CodeSentinel006, causeSecretLength},
		{"16-hex secret", func(c *Config) { c.Projects[0].SecretKey = strings.Repeat("b", 16) }, types.CodeSentinel006, causeSecretLength},
		{"zero quota", func(c *Config) { c.Projects[0].QuotaEPM = 0 }, types.CodeSentinel007, causeProjectUnknown},
		{"bad project id", func(c *Config) { c.Projects[0].ID = "abc" }, types.CodeSentinel007, causeProjectUnknown},
		{"bad loss policy", func(c *Config) { c.Projects[0].LossPolicy = "nope" }, types.CodeSentinel009, causeAdvertisedHost},
	}
	for _, tc := range cases {
		cfg := testConfig(t, tc.mut)
		if _, err := NewServer(cfg, &memSink{}, nil); err == nil {
			t.Errorf("%s: NewServer accepted an invalid config", tc.name)
			continue
		} else if !isCode(err, tc.code) {
			t.Errorf("%s: code %v, want %v", tc.name, err, tc.code)
		} else if !containsCause(err, tc.cause) {
			t.Errorf("%s: causes %v, want %q", tc.name, err.(*Error).Causes, tc.cause)
		}
	}
}

// TestDSNGrammarAndGeneration pins §2.3.
func TestDSNGrammarAndGeneration(t *testing.T) {
	cfg := testConfig(t, func(c *Config) {
		c.RequireSecret = true
		c.Bind = "127.0.0.1:7643"
	})
	dsn, err := GenerateDSN(cfg, cfg.Projects[0])
	if err != nil {
		t.Fatalf("GenerateDSN: %v", err)
	}
	want := "http://" + testPubA + ":" + testSecretA + "@trouble.example.net:7643/1"
	if dsn != want {
		t.Fatalf("dsn = %q, want %q", dsn, want)
	}
	d, perr := ParseDSN(cfg, dsn)
	if perr != nil {
		t.Fatalf("ParseDSN: %v", perr)
	}
	if d.ProjectID != "1" || d.Host != "trouble.example.net" || d.Port != "7643" {
		t.Fatalf("parsed dsn = %+v", d)
	}
	if got := d.APIBase(); got != "http://trouble.example.net:7643/api/1/" {
		t.Fatalf("APIBase = %q", got)
	}
	// Refusals: a path with /api, a trailing slash, a query, an unadvertised host,
	// a 16-hex secret and an IP host.
	bad := []string{
		"http://" + testPubA + "@trouble.example.net:7643/1/api",
		"http://" + testPubA + "@trouble.example.net:7643/1/",
		"http://" + testPubA + "@trouble.example.net:7643/1?x=1",
		"http://" + testPubA + "@other.example.net:7643/1",
		"http://" + testPubA + ":" + strings.Repeat("c", 16) + "@trouble.example.net:7643/1",
		"http://" + testPubA + "@10.0.0.5:7643/1",
	}
	for _, raw := range bad {
		if _, err := ParseDSN(cfg, raw); err == nil {
			t.Errorf("ParseDSN accepted %q", raw)
		}
	}
	// Generation refuses a bind-address host.
	badCfg := cfg
	badCfg.AdvertisedHost = "0.0.0.0"
	if _, err := GenerateDSN(badCfg, cfg.Projects[0]); err == nil {
		t.Error("GenerateDSN accepted an IP/bind host")
	}
	// ...and the loopback NAME once the bind is not loopback (§2.3a): a DSN is
	// minted before an app is deployed, so generation is the earlier of the two
	// surfaces that must refuse it.
	lanCfg := cfg
	lanCfg.Bind = "0.0.0.0:7643"
	lanCfg.AdvertisedHost = "localhost"
	if _, err := GenerateDSN(lanCfg, cfg.Projects[0]); err == nil {
		t.Error("GenerateDSN accepted the loopback host on a non-loopback bind")
	}
	// The same rule, accepted: a loopback bind mints the loopback host (what
	// SPEC-12 §3.1 derives) and the minted DSN parses back against the config.
	loopCfg := testConfig(t, func(c *Config) { c.Bind = "127.0.0.1:7643"; c.AdvertisedHost = "localhost" })
	ldsn, lerr := GenerateDSN(loopCfg, loopCfg.Projects[0])
	if lerr != nil {
		t.Fatalf("GenerateDSN for a loopback bind: %v", lerr)
	}
	if want := "http://" + testPubA + "@localhost:7643/1"; ldsn != want {
		t.Fatalf("loopback dsn = %q, want %q", ldsn, want)
	}
	if _, perr := ParseDSN(loopCfg, ldsn); perr != nil {
		t.Fatalf("the loopback dsn does not parse back: %v", perr)
	}
}

// TestKeyRotationAndRevocation pins §2.3's overlap window: both keys resolve to
// one project inside it, and a revoked key fails 006.
func TestKeyRotationAndRevocation(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	rot, _ := NewKey()
	horizon := time.Now().Add(24 * time.Hour)
	if err := ts.s.projects.addRotationKey(rot, "1", horizon); err != nil {
		t.Fatalf("addRotationKey: %v", err)
	}
	entry, err := ts.s.projects.byPublicKey(rot, time.Now())
	if err != nil {
		t.Fatalf("the rotation key must resolve: %v", err)
	}
	if entry.proj.ID != "1" {
		t.Fatalf("rotation key resolved to project %s, want 1", entry.proj.ID)
	}
	// Past valid_until the key is expired.
	if _, err := ts.s.projects.byPublicKey(rot, horizon.Add(time.Second)); err == nil {
		t.Fatal("a key past valid_until must not resolve")
	} else if !containsCause(err, causeKeyExpired) {
		t.Fatalf("expired key causes = %v, want key_expired", err.Causes)
	}
	// Revocation sets valid_until = now.
	if err := ts.s.projects.revokeKey(testPubA, time.Now()); err != nil {
		t.Fatalf("revokeKey: %v", err)
	}
	if _, err := ts.s.projects.byPublicKey(testPubA, time.Now().Add(time.Second)); err == nil {
		t.Fatal("a revoked key must not resolve")
	}
	if _, err := ts.s.projects.byPublicKey("f"+"f"+strings.Repeat("0", 30), time.Now()); err == nil {
		t.Fatal("an unknown key must not resolve")
	}
}

// TestMemoryBoundPerRequest pins §6.1's memory claim: one request's decompression
// footprint is bounded by the cap plus the 64KB overread slack.
func TestMemoryBoundPerRequest(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	body := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true})
	gz := gzipBytes(t, body)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := ts.post(t, "/api/1/envelope/", map[string]string{
				"X-Sentry-Auth":    ts.authHeader("1"),
				"Content-Encoding": "gzip",
			}, gz)
			_ = readBody(t, resp)
		}()
	}
	wg.Wait()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	bound := uint64(ts.s.cfg.MaxEnvelopeDecompressed+maxBodyOverread) * 64
	if after.HeapInuse > before.HeapInuse+bound {
		t.Fatalf("heap grew past the per-request bound: %d -> %d bytes (bound +%d)", before.HeapInuse, after.HeapInuse, bound)
	}
}

// containsCause reports whether a sentinel error carries a cause string.
func containsCause(err error, cause string) bool {
	var se *Error
	if e, ok := err.(*Error); ok {
		se = e
	} else {
		return false
	}
	for _, c := range se.Causes {
		if c == cause {
			return true
		}
	}
	return false
}

// firstLine returns the first line of a raw HTTP response.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// countersReason reads a reject_total{reason} counter.
func (c *counters) countersReason(reason string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byReason[reason]
}

// gzipRoundTrip is a helper used by the load test: it compresses once so the
// live path measures the server, not the test's compressor.
func gzipRoundTrip(tb testing.TB, b []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := zw.Write(b); err != nil {
		tb.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// incompressibleBytes returns n bytes with no exploitable repetition, so a
// gzip round trip stays near 1:1 and only the absolute cap can refuse it.
func incompressibleBytes(n int) []byte {
	out := make([]byte, n)
	var x uint64 = 0x9e3779b97f4a7c15
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = byte(x >> 24)
	}
	return out
}

// TestGunzipCausePerBranch pins §3.7's two causes to the two bounds, one branch
// at a time, and ties them to the compat doc: the doc-text check alone cannot
// catch a branch that reports the wrong cause.
func TestGunzipCausePerBranch(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()

	// Cap branch: output past limit + the 64KB overread slack, with a ratio of
	// ~1:1, so the ratio guard can never be the reason.
	big := incompressibleBytes(int(maxBodyOverread) + 8*1024)
	_, err := ts.s.gunzip(gzipBytes(t, big), 1024)
	if err == nil {
		t.Fatal("cap branch: expected a refusal")
	}
	if err.Code != types.CodeSentinel003 {
		t.Errorf("cap branch: code %q, want 003", err.Code)
	}
	if len(err.Causes) != 1 || err.Causes[0] != causeDecompressedCap {
		t.Errorf("cap branch: causes %v, want [%s]", err.Causes, causeDecompressedCap)
	}

	// Ratio branch: 900KB of a repeated byte is under the cap but far past
	// 100:1, so the only bound that can refuse it is the ratio guard.
	_, err = ts.s.gunzip(gzipBytes(t, bytes.Repeat([]byte("A"), 900*1024)), 1<<20)
	if err == nil {
		t.Fatal("ratio branch: expected a refusal")
	}
	if err.Code != types.CodeSentinel003 {
		t.Errorf("ratio branch: code %q, want 003", err.Code)
	}
	if len(err.Causes) != 1 || err.Causes[0] != causeCompressionRatio {
		t.Errorf("ratio branch: causes %v, want [%s]", err.Causes, causeCompressionRatio)
	}

	doc := readCompatDoc(t)
	for _, cause := range []string{causeDecompressedCap, causeCompressionRatio} {
		if !strings.Contains(doc, "`"+cause+"`") {
			t.Errorf("docs/sentinel-compat.md does not document the shipped cause %q", cause)
		}
	}
}

// TestResponseScratchIsPerRequest pins the response-scoped state of §3.2/§3.9:
// the 200-level 013 code (like the proactive backoff header) belongs to the
// request that produced it, so concurrent envelopes must never trade codes —
// the answer a caller reads has to describe the caller's own items.
func TestResponseScratchIsPerRequest(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	auth := ts.authHeader("1")
	url := ts.ts.URL + "/api/1/envelope/"

	type attempt struct {
		body        []byte
		unknownItem bool
	}
	const rounds = 24
	attempts := make([]attempt, 0, rounds*2)
	for i := 0; i < rounds; i++ {
		cleanID := fmt.Sprintf("%032x", i+1)
		cleanBody := eventJSON(t, func(o map[string]any) { o["event_id"] = cleanID })
		attempts = append(attempts, attempt{body: envelopeBytes(t, map[string]any{"event_id": cleanID},
			envelopeFixtureItem{Type: "event", Body: cleanBody, Length: true})})

		mixedID := fmt.Sprintf("%032x", 1000+i+1)
		mixedBody := eventJSON(t, func(o map[string]any) { o["event_id"] = mixedID })
		attempts = append(attempts, attempt{unknownItem: true,
			body: envelopeBytes(t, map[string]any{"event_id": mixedID},
				envelopeFixtureItem{Type: "check_in", Body: []byte(`{"check_in_id":"x"}`), Length: true},
				envelopeFixtureItem{Type: "event", Body: mixedBody, Length: true})})
	}

	codes := make([]string, len(attempts))
	statuses := make([]int, len(attempts))
	var wg sync.WaitGroup
	for i, a := range attempts {
		wg.Add(1)
		go func(i int, a attempt) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(a.body))
			if err != nil {
				return
			}
			req.Header.Set("X-Sentry-Auth", auth)
			resp, err := ts.ts.Client().Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[i] = resp.Header.Get("X-Sentry-Error")
			statuses[i] = resp.StatusCode
		}(i, a)
	}
	wg.Wait()

	for i, a := range attempts {
		if statuses[i] != http.StatusOK {
			t.Fatalf("attempt %d: status %d, want 200", i, statuses[i])
		}
		want := ""
		if a.unknownItem {
			want = string(types.CodeSentinel013)
		}
		if codes[i] != want {
			t.Errorf("attempt %d (unknown item: %v): X-Sentry-Error %q, want %q",
				i, a.unknownItem, codes[i], want)
		}
	}
}

// lowEntropyFiller returns n printable bytes that gzip at a middling ratio:
// small enough that the 200KB compressed cap can never be the bound, and far
// under 100:1 so the ratio guard can never be the bound either, while the byte
// count stays exact.
func lowEntropyFiller(n, seed int) []byte {
	out := make([]byte, n)
	x := uint64(0x2545f4914f6cdd1d) + uint64(seed)*0x9e3779b97f4a7c15
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		if (x>>31)&1 == 0 {
			out[i] = 'A'
		} else {
			out[i] = 'B'
		}
	}
	return out
}

// envelopeOfDecompressedSize builds a valid envelope whose framing is exactly n
// bytes. The items are attachments: each stays inside max_item_bytes, carries
// lowEntropyFiller so neither the item cap nor the ratio guard can be the bound,
// and is dropped without being scrubbed — so the case measures the reader's cap
// rather than the scrubber's per-KiB budget, which is a separate timing property.
func envelopeOfDecompressedSize(tb testing.TB, n int) []byte {
	tb.Helper()
	const items = 5
	header := map[string]any{"event_id": "5c1a7b9d2e3f405162738495a6b7c8d9"}

	build := func(filler []int) []byte {
		fit := make([]envelopeFixtureItem, 0, len(filler))
		for i, k := range filler {
			fit = append(fit, envelopeFixtureItem{Type: "attachment", Body: lowEntropyFiller(k, i), Length: true})
		}
		return envelopeBytes(tb, header, fit...)
	}

	lens := make([]int, items)
	for i := range lens {
		lens[i] = n/items - 1024
	}
	env := build(lens)
	// The message is plain 'A'/'B', so growing one filler by d grows the framing
	// by exactly d; the item's length field is the only other thing that moves
	// and it settles within a step or two of its digit count changing.
	for i := 0; i < 8 && len(env) != n; i++ {
		lens[0] += n - len(env)
		env = build(lens)
	}
	if len(env) != n {
		tb.Fatalf("built a %d-byte envelope, wanted %d", len(env), n)
	}
	for i, k := range lens {
		if k <= 0 {
			tb.Fatalf("item %d got a %d-byte filler", i, k)
		}
	}
	return env
}

// compatRow returns the compat-matrix row whose first cell is exactly label.
func compatRow(tb testing.TB, label string) string {
	tb.Helper()
	b, err := os.ReadFile(compatDocPath)
	if err != nil {
		tb.Fatalf("read %s: %v", compatDocPath, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 2 && strings.TrimSpace(fields[1]) == label {
			return line
		}
	}
	tb.Fatalf("docs/sentinel-compat.md has no %q row", label)
	return ""
}

// assertDecompressedCapRefusal checks the pinned refusal shape of §3.7/§5. It
// takes the concrete *Error so a nil refusal cannot arrive as a non-nil error
// interface and turn the assertion into a nil dereference.
func assertDecompressedCapRefusal(tb testing.TB, what string, err *Error) {
	tb.Helper()
	if err == nil {
		tb.Fatalf("%s: expected a refusal, got none", what)
	}
	if err.Code != types.CodeSentinel003 {
		tb.Errorf("%s: code %q, want 003", what, err.Code)
	}
	if len(err.Causes) != 1 || err.Causes[0] != causeDecompressedCap {
		tb.Errorf("%s: causes %v, want [%s]", what, err.Causes, causeDecompressedCap)
	}
}

// TestDecompressedCapBoundary pins the decompressed cap to the number
// docs/sentinel-compat.md §3 states: exactly MaxEnvelopeDecompressed bytes are
// accepted and the first byte past it is 413/003 with cause decompressed_cap.
// `cap + 64KB` is the reader's memory bound (§6.1) and never an accepted
// payload, so the window between the two is a refusal, on both encodings.
func TestDecompressedCapBoundary(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	capBytes := ts.s.cfg.MaxEnvelopeDecompressed
	if capBytes != 1<<20 {
		t.Fatalf("default decompressed cap is %d, want 1MB", capBytes)
	}

	// The row has to state the number the server enforces, with the 64KB named
	// as the memory bound rather than as part of the threshold.
	row := compatRow(t, "decompressed envelope")
	if !strings.Contains(row, "memory bound") {
		t.Errorf("compat row does not name cap + 64KB as the memory bound: %s", row)
	}
	cell := strings.Fields(strings.TrimSpace(strings.Split(row, "|")[2]))
	if len(cell) < 2 || cell[1] != "MB" {
		t.Fatalf("compat row no longer states the cap in MB: %s", row)
	}
	stated, cerr := strconv.Atoi(cell[0])
	if cerr != nil {
		t.Fatalf("compat row's cap number %q is not an integer: %s", cell[0], row)
	}
	if int64(stated)<<20 != capBytes {
		t.Errorf("compat row states %s MB but the server enforces %d bytes", cell[0], capBytes)
	}

	// Direct: gunzip is the bound under test, with ~1:1 output so the ratio
	// guard cannot be the reason either way.
	out, err := ts.s.gunzip(gzipBytes(t, incompressibleBytes(int(capBytes))), capBytes)
	if err != nil {
		t.Fatalf("exactly-cap decompressed payload was refused: %v", err)
	}
	if int64(len(out)) != capBytes {
		t.Fatalf("accepted %d decompressed bytes, want %d", len(out), capBytes)
	}
	_, err = ts.s.gunzip(gzipBytes(t, incompressibleBytes(int(capBytes)+1)), capBytes)
	assertDecompressedCapRefusal(t, "gunzip at cap+1", err)

	// The wire: a valid envelope of exactly cap bytes is 200 …
	auth := map[string]string{"X-Sentry-Auth": ts.authHeader("1"), "Content-Encoding": "gzip"}
	atCap := envelopeOfDecompressedSize(t, int(capBytes))
	gz := gzipBytes(t, atCap)
	ratio := float64(len(atCap)) / float64(len(gz))
	t.Logf("boundary fixture: %d bytes decompressed, %d gzipped (%.1f:1)", len(atCap), len(gz), ratio)
	if int64(len(gz)) > ts.s.cfg.MaxEnvelopeCompressed || ratio >= float64(ratioGuardMax) {
		t.Fatalf("fixture does not isolate the decompressed cap: %d compressed bytes (cap %d), ratio %.1f:1",
			len(gz), ts.s.cfg.MaxEnvelopeCompressed, ratio)
	}
	resp := ts.post(t, "/api/1/envelope/", auth, gz)
	if resp.StatusCode != 200 {
		t.Fatalf("exactly %d decompressed bytes: status %d (%s)", capBytes, resp.StatusCode, readBody(t, resp))
	}
	// 200 alone is not enough: the envelope has to have parsed and been admitted.
	if id := respID(t, resp); id != "5c1a7b9d2e3f405162738495a6b7c8d9" {
		t.Errorf("exactly-cap envelope answered with id %q, want the header's event_id", id)
	}

	// … and one byte past it is refused — including cap + the reader's 64KB
	// slack, the window that used to be accepted.
	for _, over := range []int64{1, maxBodyOverread} {
		resp := ts.post(t, "/api/1/envelope/", auth, gzipBytes(t, envelopeOfDecompressedSize(t, int(capBytes+over))))
		body := readBody(t, resp)
		if resp.StatusCode != 413 {
			t.Fatalf("cap+%d: status %d, want 413 (%s)", over, resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel003) {
			t.Errorf("cap+%d: X-Sentry-Error %q, want 003", over, got)
		}
		if got := firstCause(t, body); got != causeDecompressedCap {
			t.Errorf("cap+%d: cause %q, want %q", over, got, causeDecompressedCap)
		}
	}

	// An identity body is refused at the same number, with the compressed cap
	// raised so it cannot be the bound.
	ts2 := newTestServer(t, func(c *Config) { c.MaxEnvelopeCompressed = capBytes + 1 })
	defer ts2.close()
	resp = ts2.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts2.authHeader("1")},
		envelopeOfDecompressedSize(t, int(capBytes)+1))
	body := readBody(t, resp)
	if resp.StatusCode != 413 || firstCause(t, body) != causeDecompressedCap {
		t.Errorf("identity cap+1: %d cause %q, want 413/%q", resp.StatusCode, firstCause(t, body), causeDecompressedCap)
	}
}
