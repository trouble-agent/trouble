package llm

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/trouble-agent/trouble/internal/types"
)

// keyRefRE is the only legal shape of a candidate's `key_ref`: the NAME of an
// environment variable. A literal credential (a value with lowercase letters, a
// dash, a provider token prefix) does not match, and that refusal is the point — a
// key written into a config file is a key in every config dump, boot record and
// support bundle (SPEC-05 §4.3a, SPEC-12 §3.1).
var keyRefRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// Candidate is one entry of the ordered fallback chain (SPEC-05 §4.3a).
type Candidate struct {
	// Name is the ledger-visible identifier of this chain entry. It is what the
	// `agent_run` payload records as `serving_candidate`, so it is stable across
	// re-configuration: never an index, never a URL.
	Name string `json:"name"`
	// BaseURL is the OpenAI-compatible root, e.g. `https://host/v1`; the client
	// appends `/chat/completions`. Credentials never appear in it.
	BaseURL string `json:"base_url"`
	// Model is the model id sent in the request body.
	Model string `json:"model"`
	// KeyRef is the NAME of the environment variable holding the API key.
	KeyRef string `json:"key_ref"`
	// MaxTokens overrides the config-level cap for this candidate (0 = inherit).
	MaxTokens int `json:"max_tokens"`
	// Timeout overrides the config-level wall-clock cap (0 = inherit).
	Timeout types.Duration `json:"timeout"`
}

// CompactConfig is the context-compaction hook (SPEC-05 §3.7a).
//
// The pass is MAP-style and SINGLE-SHOT: the context is split into at most
// `MaxChunks` groups, each group is summarised by exactly one capped completion,
// and the summaries replace the groups. There is no second pass, no iterative
// refinement and no truncation: a split that would need more than `MaxChunks`
// groups REFUSES the pass and the stage fails, because dropping content quietly is
// the one outcome this hook exists to prevent.
type CompactConfig struct {
	// Enabled turns the hook on. Off means an over-budget context is a refusal
	// (ClassCompaction), never a truncation.
	Enabled bool `json:"enabled"`
	// BudgetTokens is the assembled-context budget. At or below it nothing is
	// compacted and no summarisation request is sent.
	BudgetTokens int `json:"budget_tokens"`
	// ChunkTokens is the target size of one summarisation group.
	ChunkTokens int `json:"chunk_tokens"`
	// MaxChunks caps the number of summarisation calls.
	MaxChunks int `json:"max_chunks"`
	// MaxTokens is the completion cap of each summarisation call, bounded by the
	// stage cap: a summary may never be allowed to cost more than the run it
	// summarises for.
	MaxTokens int `json:"max_tokens"`
}

// Config is the resolved `[llm]` table (SPEC-05 §4.3a).
type Config struct {
	// Candidates is the ordered fallback chain: index 0 is tried first and every
	// later entry only after a retryable failure (see Retryable).
	Candidates []Candidate `json:"candidates"`
	// MaxTokens is the HARD completion cap of the stage. A request that asks for
	// more is refused before it is sent.
	MaxTokens int `json:"max_tokens"`
	// Timeout is the wall-clock cap of one attempt: a context deadline on the HTTP
	// request, so a hung upstream costs exactly this and no more.
	Timeout types.Duration `json:"timeout"`
	// MaxAttempts caps the number of chain entries tried in one stage run (0 = all
	// of them). Failover is bounded, so a fully broken chain fails fast.
	MaxAttempts int `json:"max_attempts"`
	// MaxResponseBytes caps the buffered response body. A larger body is refused
	// (ClassContract) rather than read into memory.
	MaxResponseBytes int64         `json:"max_response_bytes"`
	Compact          CompactConfig `json:"compact"`

	// KeyResolver resolves a `key_ref` to its value. The default reads the process
	// environment, which is where SPEC-12's `[secrets] environment_file` puts it;
	// tests inject a resolver so no test needs a real credential in the
	// environment.
	KeyResolver func(ref string) (string, error) `json:"-"`
	// HTTPClient overrides the transport (tests point it at an httptest server).
	HTTPClient *http.Client `json:"-"`
}

// Defaults. Every value is safe and none is fleet-specific (SPEC-05 §4.3a): the
// chain is EMPTY, so a build that declares no `[llm]` table compiles no endpoint,
// no model and no key reference, and the agent stage's LLM port stays unwired.
const (
	DefaultMaxTokens        = 4096
	DefaultTimeout          = types.Duration("120s")
	DefaultMaxResponseBytes = int64(1 << 20) // 1 MiB of buffered response
	DefaultCompactBudget    = 24000
	DefaultCompactChunk     = 6000
	DefaultCompactMaxChunks = 8
	DefaultCompactMaxTokens = 800
)

// DefaultConfig is the compiled default: an empty chain and the caps above.
func DefaultConfig() Config {
	return Config{
		MaxTokens:        DefaultMaxTokens,
		Timeout:          DefaultTimeout,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Compact: CompactConfig{
			Enabled:      false,
			BudgetTokens: DefaultCompactBudget,
			ChunkTokens:  DefaultCompactChunk,
			MaxChunks:    DefaultCompactMaxChunks,
			MaxTokens:    DefaultCompactMaxTokens,
		},
	}
}

// wireDoc is the TOML shape of the `[llm]` table. `[[llm.candidates]]` is an array
// of tables, hence ordered by definition — the one place in this config where
// order carries meaning.
type wireDoc struct {
	LLM *llmWire `toml:"llm"`
}

type llmWire struct {
	MaxTokens        *int            `toml:"max_tokens"`
	Timeout          string          `toml:"timeout"`
	MaxAttempts      *int            `toml:"max_attempts"`
	MaxResponseBytes *int64          `toml:"max_response_bytes"`
	Candidates       []candidateWire `toml:"candidates"`
	// FallbackChain is the ordered list of candidate NAMES. Declaring it selects
	// the order (and a subset) of the declared candidates; omitting it means the
	// declaration order is the chain order. The brief's `fallback_chain` key is
	// this key — one spelling for the order, one for the connection facts.
	FallbackChain []string     `toml:"fallback_chain"`
	Compact       *compactWire `toml:"compact"`
}

type candidateWire struct {
	Name      string `toml:"name"`
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	KeyRef    string `toml:"key_ref"`
	MaxTokens *int   `toml:"max_tokens"`
	Timeout   string `toml:"timeout"`
}

type compactWire struct {
	Enabled      *bool `toml:"enabled"`
	BudgetTokens *int  `toml:"budget_tokens"`
	ChunkTokens  *int  `toml:"chunk_tokens"`
	MaxChunks    *int  `toml:"max_chunks"`
	MaxTokens    *int  `toml:"max_tokens"`
}

// LoadConfig decodes a `[llm]` document over the defaults and validates it.
//
// The decode is STRICT: a key this build does not know is refused by name, never
// decoded past — the same rule SPEC-11 §2 applies to `[skills]`, because a typo
// that silently keeps a default is how a budget cap stops existing.
func LoadConfig(doc []byte) (Config, error) {
	if len(doc) == 0 {
		return DefaultConfig(), nil
	}
	var w wireDoc
	md, err := toml.Decode(string(doc), &w)
	if err != nil {
		return Config{}, newErr(ClassContract, "", ReasonEnvelope, "llm config: %v", err)
	}
	if bad := undecodedKeys(md); len(bad) > 0 {
		return Config{}, newErr(ClassContract, "", ReasonEnvelope,
			"llm config: unknown key %q (the accepted keys are SPEC-05 §4.3a's)", bad[0])
	}
	if w.LLM == nil {
		return DefaultConfig(), nil
	}
	cfg := DefaultConfig()
	lw := w.LLM
	if lw.MaxTokens != nil {
		cfg.MaxTokens = *lw.MaxTokens
	}
	if lw.Timeout != "" {
		cfg.Timeout = types.Duration(lw.Timeout)
	}
	if lw.MaxAttempts != nil {
		cfg.MaxAttempts = *lw.MaxAttempts
	}
	if lw.MaxResponseBytes != nil {
		cfg.MaxResponseBytes = *lw.MaxResponseBytes
	}
	if lw.Compact != nil {
		cw := lw.Compact
		if cw.Enabled != nil {
			cfg.Compact.Enabled = *cw.Enabled
		}
		if cw.BudgetTokens != nil {
			cfg.Compact.BudgetTokens = *cw.BudgetTokens
		}
		if cw.ChunkTokens != nil {
			cfg.Compact.ChunkTokens = *cw.ChunkTokens
		}
		if cw.MaxChunks != nil {
			cfg.Compact.MaxChunks = *cw.MaxChunks
		}
		if cw.MaxTokens != nil {
			cfg.Compact.MaxTokens = *cw.MaxTokens
		}
	}
	for _, cw := range lw.Candidates {
		cand := Candidate{Name: cw.Name, BaseURL: cw.BaseURL, Model: cw.Model, KeyRef: cw.KeyRef}
		if cw.MaxTokens != nil {
			cand.MaxTokens = *cw.MaxTokens
		}
		if cw.Timeout != "" {
			cand.Timeout = types.Duration(cw.Timeout)
		}
		cfg.Candidates = append(cfg.Candidates, cand)
	}
	if len(lw.FallbackChain) > 0 {
		if err := cfg.reorder(lw.FallbackChain); err != nil {
			return Config{}, err
		}
	}
	return cfg, cfg.Validate()
}

// reorder applies an explicit `fallback_chain` to the declared candidates: the
// chain becomes exactly the named entries in the named order. A name that is not
// declared, or that is named twice, is refused — the chain has one order and each
// entry appears in it once.
func (c *Config) reorder(chain []string) error {
	byName := map[string]Candidate{}
	for _, cand := range c.Candidates {
		byName[cand.Name] = cand
	}
	ordered := make([]Candidate, 0, len(chain))
	seen := map[string]bool{}
	for _, raw := range chain {
		name := strings.TrimSpace(raw)
		cand, ok := byName[name]
		if !ok {
			return newErr(ClassContract, name, ReasonNoCandidate,
				"fallback_chain names candidate %q, which no [[llm.candidates]] entry declares", name)
		}
		if seen[name] {
			return newErr(ClassContract, name, ReasonNoCandidate,
				"fallback_chain names candidate %q twice; each chain entry appears once", name)
		}
		seen[name] = true
		ordered = append(ordered, cand)
	}
	c.Candidates = ordered
	return nil
}

// CompactCap is the effective completion cap of one summarisation call: the
// compaction cap, bounded by the stage cap, so a summary can never be allowed to
// cost more than the run it compacts for. The bound is downward only (a smaller cap
// is always the safe direction), which is why it is applied here rather than
// refused at validation time.
func (c Config) CompactCap() int {
	if c.Compact.MaxTokens <= 0 {
		return c.MaxTokens
	}
	if c.Compact.MaxTokens > c.MaxTokens {
		return c.MaxTokens
	}
	return c.Compact.MaxTokens
}

// Validate enforces the §4.3a field rules. The chain ORDER is whatever the config
// declared; only duplicate candidate names are refused, because the ledger records
// a name and a duplicate would make the record ambiguous.
func (c Config) Validate() error {
	if c.MaxTokens <= 0 {
		return newErr(ClassBudget, "", ReasonTokenCap, "llm max_tokens must be > 0")
	}
	if c.Timeout.Std() <= 0 {
		return newErr(ClassContract, "", ReasonTimeout, "llm timeout must be > 0")
	}
	if c.MaxResponseBytes <= 0 {
		return newErr(ClassContract, "", ReasonResponseTooLarge, "llm max_response_bytes must be > 0")
	}
	if c.MaxAttempts < 0 {
		return newErr(ClassContract, "", ReasonEnvelope, "llm max_attempts must be >= 0")
	}
	if c.Compact.Enabled {
		switch {
		case c.Compact.BudgetTokens <= 0:
			return newErr(ClassCompaction, "", ReasonCompactionCapped, "compact budget_tokens must be > 0")
		case c.Compact.ChunkTokens <= 0:
			return newErr(ClassCompaction, "", ReasonCompactionCapped, "compact chunk_tokens must be > 0")
		case c.Compact.MaxChunks <= 0:
			return newErr(ClassCompaction, "", ReasonCompactionCapped, "compact max_chunks must be > 0")
		case c.Compact.MaxTokens <= 0:
			return newErr(ClassCompaction, "", ReasonCompactionCapped, "compact max_tokens must be > 0")
		}
	}
	seen := map[string]bool{}
	for i, cand := range c.Candidates {
		if cand.Name == "" {
			return newErr(ClassContract, "", ReasonNoCandidate, "candidate %d has no name", i)
		}
		if seen[cand.Name] {
			return newErr(ClassContract, cand.Name, ReasonNoCandidate, "candidate %q is declared twice", cand.Name)
		}
		seen[cand.Name] = true
		if err := cand.validate(c); err != nil {
			return err
		}
	}
	return nil
}

func (cand Candidate) validate(c Config) error {
	u, err := url.Parse(cand.BaseURL)
	if err != nil {
		return newErr(ClassContract, cand.Name, ReasonBadBaseURL, "base_url is unparsable: %v", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return newErr(ClassContract, cand.Name, ReasonBadBaseURL, "base_url scheme %q is not http|https", u.Scheme)
	case u.Host == "":
		return newErr(ClassContract, cand.Name, ReasonBadBaseURL, "base_url has no host")
	case u.User != nil:
		return newErr(ClassContract, cand.Name, ReasonBadBaseURL,
			"base_url carries embedded credentials; the key is named by key_ref, never by URL")
	}
	if cand.Model == "" {
		return newErr(ClassContract, cand.Name, ReasonEnvelope, "model is required")
	}
	if !keyRefRE.MatchString(cand.KeyRef) {
		return newErr(ClassContract, cand.Name, ReasonKeyRefShape,
			"key_ref %q is not the NAME of an environment variable (^[A-Z][A-Z0-9_]{2,63}$); a literal credential is never written into config", cand.KeyRef)
	}
	if cand.MaxTokens < 0 {
		return newErr(ClassBudget, cand.Name, ReasonTokenCap, "max_tokens must be >= 0")
	}
	if cand.Timeout != "" && cand.Timeout.Std() <= 0 {
		return newErr(ClassContract, cand.Name, ReasonTimeout, "timeout must be > 0")
	}
	if eff := cand.EffectiveMaxTokens(c); eff > c.MaxTokens {
		return newErr(ClassBudget, cand.Name, ReasonTokenCap,
			"candidate max_tokens %d exceeds the stage cap %d", eff, c.MaxTokens)
	}
	return nil
}

// EffectiveMaxTokens is the candidate's cap or the stage cap.
func (cand Candidate) EffectiveMaxTokens(c Config) int {
	if cand.MaxTokens > 0 {
		return cand.MaxTokens
	}
	return c.MaxTokens
}

// EffectiveTimeout is the candidate's wall-clock cap or the stage cap.
func (cand Candidate) EffectiveTimeout(c Config) types.Duration {
	if cand.Timeout != "" {
		return cand.Timeout
	}
	return c.Timeout
}

// Redacted renders a candidate for a log line or a record: the host, the model and
// the key's NAME — never a value, never a query string.
func (cand Candidate) Redacted() string {
	host := cand.BaseURL
	if u, err := url.Parse(cand.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return cand.Name + " " + host + " " + cand.Model + " key_ref=" + cand.KeyRef
}

// undecodedKeys lists, sorted, the keys the document carried that no field
// claimed: exactly the unknown keys a strict decode refuses, by name.
func undecodedKeys(md toml.MetaData) []string {
	bad := md.Undecoded()
	if len(bad) == 0 {
		return nil
	}
	out := make([]string, 0, len(bad))
	for _, k := range bad {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return out
}
