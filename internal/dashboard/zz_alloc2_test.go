package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func allocPer(t *testing.T, n int, fn func()) int64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for i := 0; i < n; i++ {
		fn()
	}
	runtime.ReadMemStats(&m1)
	return int64(m1.TotalAlloc-m0.TotalAlloc) / int64(n)
}

func TestZZAllocBreakdown(t *testing.T) {
	// (a) trivial handler over TCP: client + server + net/http overhead
	trivial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer trivial.Close()
	client := trivial.Client()
	get := func(url string) func() {
		return func() {
			req, _ := http.NewRequest("GET", url, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	t.Logf("trivial handler over TCP: %d bytes/req", allocPer(t, 200, get(trivial.URL)))

	// (b) the dashboard over TCP
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	t.Logf("dashboard over TCP: %d bytes/req", allocPer(t, 200, get(env.srv.URL+"/partials/incidents")))

	// (c) the same dashboard, gzip disabled by the client
	plain := &http.Client{}
	noGzipGet := func() {
		req, _ := http.NewRequest("GET", env.srv.URL+"/partials/incidents", nil)
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Authorization", "Bearer "+env.readPlain)
		resp, err := plain.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	t.Logf("dashboard over TCP, no gzip: %d bytes/req", allocPer(t, 200, noGzipGet))

	// (d) the route handler only (no net/http server), via ServeHTTP
	sGet := func() {
		req := env.req("GET", "/partials/incidents", nil, bearer(env.readPlain))
		env.s.ServeHTTP(&discardRecorder2{}, req)
	}
	t.Logf("handler only (no server, no client): %d bytes/req", allocPer(t, 200, sGet))

	// (e) gzip writer cost: a fresh flate writer vs a pooled one
	t.Logf("strings.Contains sanity: %v", strings.Contains("abc", "b"))
}

type discardRecorder2 struct{ h http.Header }

func (d *discardRecorder2) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discardRecorder2) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardRecorder2) WriteHeader(int)             {}
