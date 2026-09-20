package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// testutil_test.go holds the fixtures the llm tests share: a recording httptest
// server, a key resolver that needs no real credential, and a config builder. No
// test opens a socket outside httptest, and no test needs a real key.

// recorder captures every request the client sent, in order, so a test can assert
// what went on the wire (and how many times).
type recorder struct {
	mu       sync.Mutex
	requests []recorded
}

type recorded struct {
	Path  string
	Auth  string
	Body  map[string]any
	Raw   string
	Stamp time.Time
}

func (r *recorder) add(req *http.Request, raw string) {
	var body map[string]any
	_ = json.Unmarshal([]byte(raw), &body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recorded{
		Path: req.URL.Path, Auth: req.Header.Get("Authorization"),
		Body: body, Raw: raw, Stamp: time.Now(),
	})
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recorded, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// scripted is one queued upstream answer: a status, a content type, a body and an
// optional delay.
type scripted struct {
	status  int
	ctype   string
	body    string
	delay   time.Duration
	headers map[string]string
}

// servers builds one httptest server per candidate script and returns the config
// URLs in order. Every server records into the shared recorder.
func servers(t *testing.T, rec *recorder, scripts ...scripted) []string {
	t.Helper()
	urls := make([]string, 0, len(scripts))
	for _, sc := range scripts {
		sc := sc
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := ""
			if r.Body != nil {
				buf, _ := io.ReadAll(r.Body)
				raw = string(buf)
			}
			rec.add(r, raw)
			if sc.delay > 0 {
				time.Sleep(sc.delay)
			}
			for k, v := range sc.headers {
				w.Header().Set(k, v)
			}
			if sc.ctype != "" {
				w.Header().Set("Content-Type", sc.ctype)
			}
			if sc.status != 0 {
				w.WriteHeader(sc.status)
			}
			_, _ = w.Write([]byte(sc.body))
		}))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL+"/v1")
	}
	return urls
}

// completion is a well-formed OpenAI-compatible response body.
func completion(text string, prompt, completionTokens int) string {
	return `{"choices":[{"message":{"content":` + jsonString(text) +
		`}}],"usage":{"prompt_tokens":` + itoa(prompt) + `,"completion_tokens":` + itoa(completionTokens) +
		`,"total_tokens":` + itoa(prompt+completionTokens) + `}}`
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// testResolver is the injected key resolver: it answers with a fixed value for the
// one test key name and never touches the real environment.
func testResolver(ref string) (string, error) {
	if ref != testKeyRef {
		return "", errors.New("unknown key ref " + ref)
	}
	return testKeyValue, nil
}

const (
	testKeyRef = "TROUBLE_TEST_LLM_KEY"
	// testKeyValue is a throwaway value: no provider accepts it and no test asserts
	// it reaches a real endpoint.
	testKeyValue = "unit-test-credential-not-a-real-key"
)

// candidateFor builds one chain entry for a test URL.
func candidateFor(name, url string) Candidate {
	return Candidate{Name: name, BaseURL: url, Model: "test-model-" + name, KeyRef: testKeyRef}
}

// testConfig is the shared config skeleton.
func testConfig(urls ...string) Config {
	cfg := DefaultConfig()
	cfg.MaxTokens = 256
	cfg.Timeout = types.Duration("2s")
	cfg.KeyResolver = testResolver
	for i, u := range urls {
		cfg.Candidates = append(cfg.Candidates, candidateFor("c"+itoa(i+1), u))
	}
	return cfg
}

func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}
