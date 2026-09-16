package dashboard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Token store — SPEC-10 §3.2.
//
// The file lives outside the state root (deliberately: credentials never ride
// along in a ledger backup), mode 0600, hashed at rest, plaintext shown once.
// Plaintext is `tdt_` + 43 base64url chars (32 CSPRNG bytes) = 47 chars; the
// prefix makes a leaked token greppable by the secret scanner. At rest:
// sha256(token)[:32] hex only.
//
// Reload semantics: the file is stat'ed per request; an mtime/size change
// re-reads it into a new immutable set swapped via atomic.Pointer. A
// parse/IO failure is fail-closed — every route except loopback /health.json
// answers 503 + TROUBLE-DASHBOARD-013 (no last-known-good fallback: a broken
// credential file must not silently keep a rotated-away token alive). In-flight
// requests keep the set they loaded.

// tokenFile is the on-disk wrapper (§3.1).
type tokenFile struct {
	Version int           `json:"version"`
	Tokens  []types.Token `json:"tokens"`
}

// tokenGrammar is the plaintext form: `tdt_` + 43 base64url chars (SPEC-10
// §2.2, §3.2).
var tokenGrammar = regexp.MustCompile(`^tdt_[A-Za-z0-9_-]{43}$`)

// tokenLen is the full plaintext length (prefix + 43).
const tokenPrefix = "tdt_"
const tokenPlainLen = 47

// scopeSet is the expanded scope bit set (§3.1): bit 0 read, bit 1 write,
// bit 2 autonomy.
type scopeSet uint8

const (
	scopeRead     scopeSet = 1 << 0
	scopeWrite    scopeSet = 1 << 1
	scopeAutonomy scopeSet = 1 << 2
)

// expandScopes applies the linear hierarchy autonomy ⇒ write ⇒ read (§2.2).
func expandScopes(scopes []types.Scope) (scopeSet, error) {
	var set scopeSet
	for _, sc := range scopes {
		switch sc {
		case types.ScopeRead:
			set |= scopeRead
		case types.ScopeWrite:
			set |= scopeWrite | scopeRead
		case types.ScopeAutonomy:
			set |= scopeAutonomy | scopeWrite | scopeRead
		default:
			return 0, errf("unknown scope %q", sc)
		}
	}
	if set == 0 {
		return 0, errors.New("no scopes")
	}
	return set, nil
}

func (s scopeSet) has(sc types.Scope) bool {
	switch sc {
	case types.ScopeRead:
		return s&scopeRead != 0
	case types.ScopeWrite:
		return s&scopeWrite != 0
	case types.ScopeAutonomy:
		return s&scopeAutonomy != 0
	}
	return false
}

func (s scopeSet) String() string {
	var parts []string
	if s&scopeRead != 0 {
		parts = append(parts, "read")
	}
	if s&scopeWrite != 0 {
		parts = append(parts, "write")
	}
	if s&scopeAutonomy != 0 {
		parts = append(parts, "autonomy")
	}
	return strings.Join(parts, ",")
}

type tokenEntry struct {
	types.Token
	scopes scopeSet
}

// tokenSet is one immutable snapshot of the store. invalid replaces the whole
// set on a parse/IO failure (fail-closed, §3.2).
type tokenSet struct {
	byHash     map[string]tokenEntry
	byID       map[string]tokenEntry
	invalid    bool
	invalidErr error
	modTime    time.Time
	size       int64
}

// TokenStore is the 0600 token store: an immutable snapshot swapped per
// request plus the file-write path (Mint/Rotate/Revoke/Save).
type TokenStore struct {
	path      string
	forbidden []string // project key plaintexts (DSN PublicKey/SecretKey)

	cur           atomic.Pointer[tokenSet]
	reloadMu      sync.Mutex
	lastUsed      sync.Mutex
	lastUsedWrite map[string]time.Time
	parentOK      bool // parent dir verified 0700
}

// TokenStoreInvalid is the fail-closed marker error; routes map it to 503 +
// TROUBLE-DASHBOARD-013.
var errTokenStoreInvalid = errors.New("token store invalid")

// LoadTokenStore reads the token file at path. A missing file is a valid empty
// store (no credentials yet — the CLI creates them); a wrong file mode is a
// boot error (TROUBLE-LIFECYCLE-013, §3.2); a parse or IO failure marks the
// store invalid (fail-closed 503 at first use, §3.2) rather than failing the
// boot. A stored hash equal to any forbidden project key is a boot error
// (TROUBLE-DASHBOARD-002 detail equals_ingestion_key, §4.3.3).
func LoadTokenStore(path string, forbidKeys []string) (*TokenStore, error) {
	ts := &TokenStore{
		path:          path,
		forbidden:     forbidKeys,
		lastUsedWrite: map[string]time.Time{},
	}
	tf, err := ts.readFileIntoSet()
	if err != nil {
		var de *dashError
		if errors.As(err, &de) {
			return nil, de
		}
		// IO failure reading an existing file: fail-closed invalid store.
		set := &tokenSet{invalid: true, invalidErr: err}
		ts.cur.Store(set)
		return ts, nil
	}
	ts.cur.Store(tf)
	return ts, nil
}

// readFileIntoSet loads and validates the current file content. A missing file
// yields a valid empty set. Mode violations are boot errors; content problems
// produce an invalid set (returned as nil error — the invalid flag rides on the
// set, so boot proceeds and every request fails closed).
func (ts *TokenStore) readFileIntoSet() (*tokenSet, error) {
	set := &tokenSet{
		byHash: map[string]tokenEntry{},
		byID:   map[string]tokenEntry{},
	}
	fi, err := os.Stat(ts.path)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil // no credentials yet — valid empty store
		}
		return nil, &dashError{Code: types.CodeLifecycle013, HTTP: 500, Message: "token store unstatable", Detail: "token_file_mode"}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, &dashError{Code: types.CodeLifecycle013, HTTP: 500, Message: "token file mode not 0600", Detail: "token_file_mode"}
	}
	b, err := os.ReadFile(ts.path)
	if err != nil {
		// IO failure: fail-closed invalid store (per-request 503 + 013).
		set.invalid = true
		set.invalidErr = fmt.Errorf("token store read: %w", err)
		return set, nil
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		set.invalid = true
		set.invalidErr = fmt.Errorf("token store parse: %w", err)
		return set, nil
	}
	set.modTime = fi.ModTime()
	set.size = fi.Size()

	for i := range tf.Tokens {
		t := tf.Tokens[i]
		scopes, err := expandScopes(t.Scopes)
		if err != nil {
			set.invalid = true
			set.invalidErr = fmt.Errorf("token %q: %w", t.ID, err)
			return set, nil
		}
		if t.ID == "" || !isHexHash(t.Hash) {
			set.invalid = true
			set.invalidErr = fmt.Errorf("token %q: malformed entry", t.ID)
			return set, nil
		}
		for _, key := range ts.forbidden {
			if t.Hash == hashOf(key) {
				return nil, &dashError{Code: types.CodeDashboard002, HTTP: 500, Message: "token equals an ingestion key", Detail: "equals_ingestion_key"}
			}
		}
		entry := tokenEntry{Token: t, scopes: scopes}
		set.byHash[t.Hash] = entry
		set.byID[t.ID] = entry
	}
	return set, nil
}

func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// hashOf is sha256(s)[:32] hex — the at-rest token form (§3.2).
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:32])
}

// snapshot returns the current immutable set (atomic).
func (ts *TokenStore) snapshot() *tokenSet { return ts.cur.Load() }

// refresh stats the file and re-reads on an mtime/size change (§3.2). A
// reload failure produces a new invalid set — never the last-known-good one.
// Concurrent requests: one re-read happens; the rest see the swapped set.
func (ts *TokenStore) refresh(now time.Time) {
	cur := ts.cur.Load()
	fi, err := os.Stat(ts.path)
	if err != nil {
		if os.IsNotExist(err) && cur.modTime.IsZero() {
			return // still empty-valid
		}
		ts.reloadMu.Lock()
		defer ts.reloadMu.Unlock()
		// File vanished or is unstatable: fail closed, unless the store was
		// already invalid (keep the stale invalid marker).
		if cur.invalid {
			return
		}
		ts.cur.Store(&tokenSet{invalid: true, invalidErr: fmt.Errorf("token store stat: %w", err)})
		return
	}
	if !fi.ModTime().Equal(cur.modTime) || fi.Size() != cur.size {
		ts.reloadMu.Lock()
		defer ts.reloadMu.Unlock()
		// Double-checked: another goroutine may have reloaded already.
		cur = ts.cur.Load()
		if !fi.ModTime().Equal(cur.modTime) || fi.Size() != cur.size {
			set, err := ts.readFileIntoSet()
			if err != nil {
				// Wrong mode mid-run or ingestion-key injection: fail closed.
				ts.cur.Store(&tokenSet{invalid: true, invalidErr: err})
				return
			}
			ts.cur.Store(set)
		}
	}
}

// lookup finds the authenticatable entry whose stored hash equals the presented
// token's hash. Revoked entries are present for audit but never authenticate.
// Comparison of the derived hashes is constant-time against the found entry
// (SPEC-10 §2.2): the plaintext is never held beyond this function.
func (ts *TokenStore) lookup(plaintext string) (tokenEntry, bool) {
	set := ts.cur.Load()
	if set.invalid {
		return tokenEntry{}, false
	}
	h := hashOf(plaintext)
	entry, ok := set.byHash[h]
	if !ok {
		// Constant-time fallback so absence does not leak timing.
		var dummy tokenEntry
		dummy.Hash = strings.Repeat("0", 64)
		subtleEqual(h, dummy.Hash)
		return tokenEntry{}, false
	}
	if !subtleEqual(h, entry.Hash) {
		return tokenEntry{}, false
	}
	if entry.Revoked {
		return tokenEntry{}, false
	}
	return entry, true
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// invalid reports the fail-closed state and its cause.
func (ts *TokenStore) invalid() error {
	if set := ts.cur.Load(); set.invalid {
		if set.invalidErr != nil {
			return set.invalidErr
		}
		return errTokenStoreInvalid
	}
	return nil
}

// markUsed stamps LastUsedTS at most once per 60s per token (§3.2 — write
// amplification control). A crash loses ≤60s of LastUsedTS, never a
// credential; a Save failure is logged, never fatal.
func (ts *TokenStore) markUsed(label string, now time.Time) {
	ts.lastUsed.Lock()
	if last, ok := ts.lastUsedWrite[label]; ok && now.Sub(last) < 60*time.Second {
		ts.lastUsed.Unlock()
		return
	}
	ts.lastUsedWrite[label] = now
	cur := ts.cur.Load()
	if cur.invalid {
		ts.lastUsed.Unlock()
		return
	}
	entry, ok := cur.byID[label]
	if !ok || entry.Revoked {
		ts.lastUsed.Unlock()
		return
	}
	// Copy-on-write the snapshot so in-flight requests keep the old set.
	next := &tokenSet{modTime: cur.modTime, size: cur.size}
	next.byID = make(map[string]tokenEntry, len(cur.byID))
	for id, e := range cur.byID {
		if id == label {
			e.LastUsedTS = types.FormatUTC(now)
		}
		next.byID[id] = e
	}
	next.byHash = make(map[string]tokenEntry, len(next.byID))
	for _, e := range next.byID {
		next.byHash[e.Hash] = e
	}
	ts.cur.Store(next)
	ts.lastUsed.Unlock()
	if err := ts.Save(); err != nil {
		// Non-fatal: loses ≤60s of LastUsedTS, never a credential.
		return
	}
}

// Mint creates a new token: plaintext `tdt_` + 43 base64url chars, shown to
// the caller exactly once (the CLI prints it), hashed at rest. Refuses a
// plaintext byte-equal to any forbidden ingestion key (TROUBLE-DASHBOARD-002,
// detail equals_ingestion_key — §4.3.2).
func (ts *TokenStore) Mint(label string, scopes []types.Scope, now time.Time) (types.Token, string, error) {
	tok, plaintext, err := ts.mint(label, scopes, now)
	if err != nil {
		return types.Token{}, "", err
	}
	if err := ts.add(tok); err != nil {
		return types.Token{}, "", err
	}
	return tok, plaintext, nil
}

// mint generates a token without writing it: entropy, the §3.2 grammar check
// and the §4.3.2 ingestion-key refusal.
func (ts *TokenStore) mint(label string, scopes []types.Scope, now time.Time) (types.Token, string, error) {
	if label == "" {
		return types.Token{}, "", errors.New("token label required")
	}
	if _, err := expandScopes(scopes); err != nil {
		return types.Token{}, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return types.Token{}, "", fmt.Errorf("token entropy: %w", err)
	}
	plaintext := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(plaintext) != tokenPlainLen {
		return types.Token{}, "", fmt.Errorf("token plaintext length %d, want %d", len(plaintext), tokenPlainLen)
	}
	for _, key := range ts.forbidden {
		if key != "" && plaintext == key {
			return types.Token{}, "", &dashError{Code: types.CodeDashboard002, HTTP: 500, Message: "token equals an ingestion key", Detail: "equals_ingestion_key"}
		}
	}
	return types.Token{
		ID:        label,
		Hash:      hashOf(plaintext),
		Scopes:    append([]types.Scope(nil), scopes...),
		CreatedTS: types.FormatUTC(now),
	}, plaintext, nil
}

// Rotate mints a new label (<label>+<yyyymmdd>) and revokes the old entry in
// the same atomic file rewrite — no grace window (§3.2): a grace window is an
// undocumented second credential.
func (ts *TokenStore) Rotate(label string, now time.Time) (types.Token, string, error) {
	set := ts.cur.Load()
	if set.invalid {
		return types.Token{}, "", errTokenStoreInvalid
	}
	old, ok := set.byID[label]
	if !ok {
		return types.Token{}, "", fmt.Errorf("token %q not found", label)
	}
	if old.Revoked {
		return types.Token{}, "", fmt.Errorf("token %q already revoked", label)
	}
	newLabel := label + "+" + now.UTC().Format("20060102")
	tok, plaintext, err := ts.mint(newLabel, old.Scopes, now)
	if err != nil {
		return types.Token{}, "", err
	}
	if err := ts.rewrite(func(tf *tokenFile) {
		for i := range tf.Tokens {
			if tf.Tokens[i].ID == label {
				tf.Tokens[i].Revoked = true
			}
		}
		tf.Tokens = append(tf.Tokens, tok)
	}); err != nil {
		return types.Token{}, "", err
	}
	return tok, plaintext, nil
}

// Revoke marks a label revoked and rewrites the file atomically. Revoked
// entries stay in the file (audit) with revoked:true (§3.2).
func (ts *TokenStore) Revoke(label string, now time.Time) error {
	set := ts.cur.Load()
	if set.invalid {
		return errTokenStoreInvalid
	}
	if _, ok := set.byID[label]; !ok {
		return fmt.Errorf("token %q not found", label)
	}
	_ = now
	return ts.rewrite(func(tf *tokenFile) {
		for i := range tf.Tokens {
			if tf.Tokens[i].ID == label {
				tf.Tokens[i].Revoked = true
			}
		}
	})
}

// add inserts a minted token into the file and refreshes the snapshot in the
// same atomic rewrite.
func (ts *TokenStore) add(tok types.Token) error {
	return ts.rewrite(func(tf *tokenFile) {
		for i := range tf.Tokens {
			if tf.Tokens[i].ID == tok.ID {
				tf.Tokens[i] = tok // idempotent re-mint replaces
				return
			}
		}
		tf.Tokens = append(tf.Tokens, tok)
	})
}

// Save re-serializes the current snapshot to disk (temp + fsync + rename, mode
// 0600, §3.2) — used after LastUsedTS updates and by the CLI.
func (ts *TokenStore) Save() error {
	set := ts.cur.Load()
	if set.invalid {
		return errTokenStoreInvalid
	}
	tf := tokenFile{Version: 1}
	for _, e := range set.byID {
		tf.Tokens = append(tf.Tokens, e.Token)
	}
	return ts.writeFile(&tf)
}

// rewrite applies a mutation to the on-disk file and then reloads the in-memory
// snapshot so the CLI and the daemon observe the same bytes.
func (ts *TokenStore) rewrite(mut func(*tokenFile)) error {
	var tf tokenFile
	b, err := os.ReadFile(ts.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("token store read: %w", err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &tf); err != nil {
			return fmt.Errorf("token store parse: %w", err)
		}
	}
	if tf.Version == 0 {
		tf.Version = 1
	}
	if tf.Tokens == nil {
		tf.Tokens = []types.Token{}
	}
	mut(&tf)
	if err := ts.writeFile(&tf); err != nil {
		return err
	}
	// Refresh the in-memory snapshot from the bytes we just wrote.
	set, err := ts.readFileIntoSet()
	if err != nil {
		return err
	}
	ts.cur.Store(set)
	return nil
}

// writeFile is the atomic write path: temp in the same directory, fsync,
// rename; mode 0600, parent 0700 (§3.2).
func (ts *TokenStore) writeFile(tf *tokenFile) error {
	dir := filepath.Dir(ts.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("token store dir: %w", err)
	}
	b, err := json.Marshal(tf)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tokens-*.tmp")
	if err != nil {
		return fmt.Errorf("token store temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("token store chmod: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("token store write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("token store fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("token store close: %w", err)
	}
	if err := os.Rename(tmpName, ts.path); err != nil {
		return fmt.Errorf("token store rename: %w", err)
	}
	return nil
}

// sortTokens keeps the file deterministic for review.
func sortTokens(toks []types.Token) {
	sort.Slice(toks, func(i, j int) bool { return toks[i].ID < toks[j].ID })
}
