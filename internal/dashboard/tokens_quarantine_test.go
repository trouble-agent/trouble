package dashboard

// TRBL-103: one malformed ENTRY in dashboard-tokens.json must not fail-close
// the whole store. Before the fix, a single bad entry (unknown scope, empty
// id, short/non-hex hash, wrong-typed field) marked the entire set invalid:
// every valid token answered 503 + TROUBLE-DASHBOARD-013. The contract after
// the fix (SPEC-10 §3.2 fail-closed scoped to the FILE, not the entry):
//
//   - whole-file unreadable/unparsable → invalid set, 503 + 013 (unchanged);
//   - one malformed entry → SKIPPED with a WARN naming the entry (index, id,
//     reason); valid tokens keep working; the skipped entry never
//     authenticates (it never enters byHash/byID).
//
// These tests are written against the BROKEN behavior first (RED), then the
// fix in tokens.go turns them GREEN without touching auth.go/identity.go.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// writeRawTokenFile writes dashboard-tokens.json from raw JSON so tests can
// inject entries the typed writer cannot express (wrong-typed hash field).
func writeRawTokenFile(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "dashboard-tokens.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write raw token file: %v", err)
	}
	return path
}

// malformedStoreContent builds a store with two valid tokens (the env's read
// and write plaintexts, looked up by their deterministic hashes) plus one
// malformed entry in the middle. shape selects the malformed form under test.
func malformedStoreContent(shape string) string {
	_, readHash := tokenFor(1)
	_, writeHash := tokenFor(2)
	bad := map[string]string{
		"short-hash":  `{"id":"broken@entry","hash":"abcd","scopes":["read"]}`,
		"nonhex-hash": `{"id":"broken@entry","hash":"zzzz0000000000000000000000000000000000000000000000000000000000","scopes":["read"]}`,
		"empty-id":    `{"id":"","hash":"` + strings.Repeat("ab", 32) + `","scopes":["read"]}`,
		"bad-scope":   `{"id":"broken@entry","hash":"` + strings.Repeat("ab", 32) + `","scopes":["teleport"]}`,
		"no-scope":    `{"id":"broken@entry","hash":"` + strings.Repeat("ab", 32) + `","scopes":[]}`,
		"wrong-type":  `{"id":"broken@entry","hash":123,"scopes":["read"]}`,
	}[shape]
	if bad == "" {
		panic("unknown malformed shape " + shape)
	}
	return `{"version":1,"tokens":[` +
		`{"id":"` + labelRead + `","hash":"` + readHash + `","scopes":["read"],"created_ts":"2026-09-16T09:00:00.000Z"},` +
		bad + `,` +
		`{"id":"` + labelWrite + `","hash":"` + writeHash + `","scopes":["write"],"created_ts":"2026-09-16T09:00:00.000Z"}` +
		`]}`
}

// TestMalformedEntryDoesNotLockTheStore is the TRBL-103 acceptance: with one
// malformed entry among valid ones, a VALID token authenticates (200, not
// 503/013) for every malformed shape; the skipped entry's material (if any
// plaintext ever matched it) can never authenticate; a grammar-invalid token
// still gets 401 + 002; loopback /health.json keeps serving.
func TestMalformedEntryDoesNotLockTheStore(t *testing.T) {
	for _, shape := range []string{"short-hash", "nonhex-hash", "empty-id", "bad-scope", "no-scope", "wrong-type"} {
		t.Run(shape, func(t *testing.T) {
			env := newEnv(t, envOptions{noRefresh: true})
			content := malformedStoreContent(shape)
			if err := os.WriteFile(env.tokenFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(2 * time.Second)
			if err := os.Chtimes(env.tokenFile, future, future); err != nil {
				t.Fatal(err)
			}

			// The valid read token must WORK — this is the 503 today.
			resp, body := env.get("/", env.readPlain)
			if resp.StatusCode == http.StatusServiceUnavailable {
				t.Fatalf("valid token got 503 with one malformed entry (shape %s): store-wide lockout, body %.200s", shape, body)
			}
			wantStatus(t, resp, body, http.StatusOK)

			// The write token (valid, write scope) works too.
			resp, body = env.get("/groups", env.writePlain)
			wantStatus(t, resp, body, http.StatusOK)

			// A token that is not in the store at all stays 401 + 002 —
			// the skip must not turn into a store-wide open door.
			unknown, _ := tokenFor(9)
			resp, body = env.get("/", unknown)
			wantStatus(t, resp, body, http.StatusUnauthorized)
			eb := decodeError(t, body)
			if eb.Error.Code != string(types.CodeDashboard002) {
				t.Fatalf("unknown token code = %s, want %s", eb.Error.Code, types.CodeDashboard002)
			}

			// Loopback health exemption still serves.
			resp, _ = env.get("/health.json", "")
			wantStatus(t, resp, body, http.StatusOK)
		})
	}
}

// TestMalformedEntrySkippedWithWarningNamesIt: the WARN must name the
// offending entry (index and/or id + reason) so the operator can fix the
// line, and must never carry token material.
func TestMalformedEntrySkippedWithWarningNamesIt(t *testing.T) {
	dir := t.TempDir()
	logger, buf := testLogger()
	path := writeRawTokenFile(t, dir, malformedStoreContent("short-hash"))

	store, err := LoadTokenStore(path, nil, logger)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("one malformed entry must not invalidate the store: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "broken@entry") {
		t.Fatalf("warning does not name the offending entry id: %q", logs)
	}
	if !strings.Contains(logs, "skip") && !strings.Contains(logs, "malformed") {
		t.Fatalf("warning does not say the entry was skipped/malformed: %q", logs)
	}
	// Entry index 1 (0-based) is the malformed one.
	if !strings.Contains(logs, "1") {
		t.Fatalf("warning does not name the entry index: %q", logs)
	}
	// No token material in the log: none of the valid plaintexts, no hash.
	if _, hash := tokenFor(1); strings.Contains(logs, hash) {
		t.Fatalf("warning leaked a token hash: %q", logs)
	}

	// The skipped entry must never authenticate: its (shape-valid-looking)
	// 64-hex hash is not in byHash. Use the empty-id shape's hash.
	badHash := strings.Repeat("ab", 32)
	if _, ok := store.lookup(badHash); ok {
		t.Fatalf("the skipped entry's hash authenticated — a skipped entry must never enter the lookup")
	}
}

// TestMalformedWrongTypeEntrySkippedPerEntry pins the decode shape: a
// wrong-TYPED field in one entry must be a per-entry skip, not a whole-file
// parse failure (the typed json.Unmarshal fails the entire array today).
func TestMalformedWrongTypeEntrySkippedPerEntry(t *testing.T) {
	dir := t.TempDir()
	logger, buf := testLogger()
	path := writeRawTokenFile(t, dir, malformedStoreContent("wrong-type"))

	store, err := LoadTokenStore(path, nil, logger)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("wrong-typed hash field in ONE entry must not invalidate the store: %v", err)
	}
	if !strings.Contains(buf.String(), "broken@entry") {
		t.Fatalf("warning does not name the offending entry: %q", buf.String())
	}
}

// TestAllEntriesMalformedIsValidEmptyStore: every entry malformed → a valid
// EMPTY store (presented tokens 401, not 503) with the warnings, per the
// contract that fail-closed 013 is for an unreadable/unparsable FILE.
func TestAllEntriesMalformedIsValidEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := writeRawTokenFile(t, dir, `{"version":1,"tokens":[{"id":"broken@entry","hash":"short","scopes":["read"]}]}`)

	store, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("all-malformed entries must stay a valid (empty) store: %v", err)
	}
	plain, _ := tokenFor(1)
	if _, ok := store.lookup(plain); ok {
		t.Fatalf("a token authenticated against an all-malformed store")
	}
}

// TestWholeFileCorruptionStillFailClosed guards the boundary the fix must NOT
// move: an unparsable FILE stays invalid (503 + 013 at first use).
func TestWholeFileCorruptionStillFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := writeRawTokenFile(t, dir, `{"version":1,"tokens":[`) // truncated JSON
	store, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if err := store.invalid(); err == nil {
		t.Fatalf("an unparsable token FILE must stay fail-closed (invalid)")
	}
}

// TestSkippedEntriesSurviveSave: the skip is in-memory quarantine — a Save
// (LastUsedTS write, CLI) must not silently delete the operator's malformed
// entry from the file; it is preserved byte-wise and never authenticated.
func TestSkippedEntriesSurviveSave(t *testing.T) {
	dir := t.TempDir()
	path := writeRawTokenFile(t, dir, malformedStoreContent("short-hash"))

	store, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save over a store with a skipped entry: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatalf("saved file unparsable: %v", err)
	}
	found := false
	for _, tok := range tf.Tokens {
		if tok.ID == "broken@entry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Save dropped the malformed entry: %s", b)
	}
	// And the reloaded file still skips it, still serves the valid ones.
	store2, err := LoadTokenStore(path, nil, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := store2.invalid(); err != nil {
		t.Fatalf("reload invalid: %v", err)
	}
}
