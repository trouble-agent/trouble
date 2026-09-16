package sentinel

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
	_ = readBody(t, resp)
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
	_ = readBody(t, resp)
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
func TestBootValidation(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		code  types.ErrorCode
		cause string
	}{
		{"ip host", func(c *Config) { c.AdvertisedHost = "10.0.0.5" }, types.CodeSentinel009, causeAdvertisedHost},
		{"bind host", func(c *Config) { c.AdvertisedHost = "0.0.0.0" }, types.CodeSentinel009, causeAdvertisedHost},
		{"localhost", func(c *Config) { c.AdvertisedHost = "localhost" }, types.CodeSentinel009, causeAdvertisedHost},
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
