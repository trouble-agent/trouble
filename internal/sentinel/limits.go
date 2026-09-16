package sentinel

import (
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ratioGuardFloor is the output size past which the compression-ratio guard of
// §3.7 starts applying: beyond it a compressed:decompressed ratio above 100:1 is
// a bomb, not a payload.
const ratioGuardFloor = 100 * 1024

// ratioGuardMax is the pinned ratio bound (100:1).
const ratioGuardMax = 100

// maxBodyOverread is the slack the limited reader allows past the decompressed
// cap so that "over the cap" is detectable without buffering the rest
// (§6.2): memory bound = cap + 64KB.
const maxBodyOverread = 64 * 1024

// limitedReader reads at most limit+slack bytes and records whether the source
// tried to go past limit. It is the memory bound for one request.
type limitedReader struct {
	r         io.Reader
	remaining int64
	slack     int64
	overCap   bool
	total     int64
}

func newLimitedReader(r io.Reader, limit, slack int64) *limitedReader {
	return &limitedReader{r: r, remaining: limit, slack: slack}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining > 0 {
		if int64(len(p)) > l.remaining {
			p = p[:l.remaining]
		}
	} else if l.slack > 0 {
		if int64(len(p)) > l.slack {
			p = p[:l.slack]
		}
	} else {
		// Read one more byte to learn that the source is over the cap.
		var one [1]byte
		n, err := l.r.Read(one[:])
		if n > 0 {
			l.overCap = true
			return 0, errOverCap
		}
		if err == nil {
			return 0, nil
		}
		return 0, err
	}
	n, err := l.r.Read(p)
	if int64(n) <= l.remaining {
		l.remaining -= int64(n)
	} else {
		over := int64(n) - l.remaining
		l.remaining = 0
		l.slack -= over
		if l.slack < 0 {
			over += l.slack
			n -= int(over)
			l.overCap = true
			err = errOverCap
		}
	}
	l.total += int64(n)
	return n, err
}

var errOverCap = errors.New("sentinel: body exceeds the configured cap")

// readEnvelopeBody returns the request body within the compressed cap and, when
// Content-Encoding is gzip, the decompressed bytes within the decompressed cap
// with the ratio guard of §3.7 applied.
//
// Every path here is a refusal with a pinned code: 002 for the compressed cap,
// 003 for the decompressed cap and the ratio guard, 004 for an invalid gzip
// stream, 022 for an unsupported encoding.
func (s *Server) readEnvelopeBody(h http.ResponseWriter, r *http.Request) ([]byte, *Error) {
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
	case "gzip", "x-gzip":
	default:
		return nil, errf(types.CodeSentinel022, "unsupported content-encoding", causeUnsupportedEnc)
	}

	// The compressed cap is enforced while counting bytes read, so a chunked
	// body with no Content-Length is refused mid-stream without being buffered
	// (§6.2).
	raw := newLimitedReader(r.Body, s.cfg.MaxEnvelopeCompressed, 0)
	compressed, err := io.ReadAll(raw)
	if err != nil {
		if errors.Is(err, errOverCap) {
			return nil, errf(types.CodeSentinel002, "compressed envelope exceeds the cap", causeTooLarge)
		}
		return nil, errf(types.CodeSentinel001, "reading the request body failed", causeFraming)
	}
	if enc != "gzip" && enc != "x-gzip" {
		if int64(len(compressed)) > s.cfg.MaxEnvelopeDecompressed {
			return nil, errf(types.CodeSentinel003, "envelope exceeds the decompressed cap", causeDecompressedCap)
		}
		return compressed, nil
	}
	return s.gunzip(compressed, s.cfg.MaxEnvelopeDecompressed)
}

// gunzip decompresses b with the decompressed cap and the 100:1 ratio guard.
func (s *Server) gunzip(b []byte, limit int64) ([]byte, *Error) {
	zr, err := gzip.NewReader(newByteReader(b))
	if err != nil {
		return nil, errf(types.CodeSentinel004, "gzip stream invalid", causeGzip)
	}
	defer zr.Close()

	lr := newLimitedReader(zr, limit, maxBodyOverread)
	out := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := lr.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			// §3.7 pins the two causes separately, so the response names which
			// bound bit: the absolute decompressed cap, or the bomb heuristic.
			if lr.overCap {
				return nil, errf(types.CodeSentinel003, "decompressed payload exceeds the cap",
					causeDecompressedCap)
			}
			if int64(len(out)) > ratioGuardFloor && int64(len(out)) > int64(len(b))*ratioGuardMax {
				return nil, errf(types.CodeSentinel003, "decompressed payload exceeds the 100:1 compression ratio guard",
					causeCompressionRatio)
			}
		}
		if rerr != nil {
			if rerr == io.EOF || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			if lr.overCap || errors.Is(rerr, gzip.ErrChecksum) || errors.Is(rerr, gzip.ErrHeader) {
				return nil, errf(types.CodeSentinel003, "decompressed payload exceeds the cap", causeDecompressedCap)
			}
			return nil, errf(types.CodeSentinel004, "gzip stream invalid", causeGzip)
		}
		if n == 0 {
			break
		}
	}
	return out, nil
}

// byteReader is a minimal io.Reader over a byte slice (avoids bytes.Reader's
// extra surface here and keeps the memory path obvious).
type byteReader struct {
	b []byte
	i int
}

func newByteReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// semaphore bounds in-flight requests (max_concurrent, §3.7). Over the cap a
// request is queued for at most 1s, then refused with 429/overloaded.
type semaphore struct {
	ch chan struct{}
}

func newSemaphore(n int) *semaphore {
	if n <= 0 {
		n = 1
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

// acquire tries to take a slot, waiting at most wait.
func (s *semaphore) acquire(wait time.Duration) bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.ch <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

func (s *semaphore) release() {
	select {
	case <-s.ch:
	default:
	}
}

// ipLimiter is the per-IP token bucket of §3.7 (600/min, burst 60), keyed by the
// client IP. The IP itself is never stored: only its sha256[:16] (client_ip_hash).
type ipLimiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(perMin, burst float64, now time.Time) *ipLimiter {
	if perMin <= 0 {
		perMin = 600
	}
	if burst <= 0 {
		burst = 60
	}
	return &ipLimiter{perMin: perMin, burst: burst, buckets: map[string]*bucket{}}
}

// allow consumes one token for ip at time now.
func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * (l.perMin / 60.0)
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP applies the proxy-trust policy of §3.7 and returns the client
// address plus the zone it lives in.
//
// XFF is honored only when the immediate peer is trusted; the rightmost address
// that is not itself a trusted proxy is the client.
func (s *Server) clientIP(remoteAddr string, xff string) (ip net.IP, zone string) {
	peer := parseIP(remoteAddr)
	if peer == nil {
		return nil, zoneLoopback
	}
	if !s.cfg.trustsPeer(peer) {
		return peer, zoneOf(peer)
	}
	if s.cfg.ProxyTrust == proxyTrustNone {
		return peer, zoneOf(peer)
	}
	hops := splitXFF(xff)
	for i := len(hops) - 1; i >= 0; i-- {
		hop := net.ParseIP(hops[i])
		if hop == nil {
			continue
		}
		if s.cfg.trustsProxy(hop) {
			continue
		}
		return hop, zoneOf(hop)
	}
	return peer, zoneOf(peer)
}

func parseIP(hostport string) net.IP {
	if hostport == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	return net.ParseIP(host)
}

func splitXFF(xff string) []string {
	if xff == "" {
		return nil
	}
	parts := strings.Split(xff, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// zoneOf classifies an address into the zones of §3.7's bind matrix.
func zoneOf(ip net.IP) string {
	if ip == nil {
		return zonePublic
	}
	if ip.IsLoopback() {
		return zoneLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || isULA(ip) {
		return zoneLAN
	}
	return zonePublic
}

func isULA(ip net.IP) bool {
	v6 := ip.To16()
	return ip.To4() == nil && v6 != nil && v6[0]&0xfe == 0xfc
}

// clientIPHash is sha256(ip)[:16] hex — the only form of an attacker IP that
// may reach a git-distributed ledger (§3.7).
func clientIPHash(ip net.IP) string {
	if ip == nil {
		return ""
	}
	sum := types.SigDigest([]byte(ip.String()))
	return types.DigestShort(sum)
}

// parseHostPort splits an address that may lack a port.
func parseHostPort(addr string) (string, string) {
	if addr == "" {
		return "", ""
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}
