package scrub

import (
	"time"

	"github.com/BurntSushi/toml"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Defaults of the [scrub] subtree (SPEC-02 §3.2).
const (
	DefaultRulesVersion     = 1
	DefaultMaxBytes         = 262144
	DefaultRuleTimeout      = 250 * time.Millisecond
	DefaultPIIMode          = "redact"
	DefaultPathMode         = "keep"
	DefaultBoundaryVerify   = true
	DefaultPrefilter        = true
	DefaultEntropy          = true
	DefaultMaxPayloadBytes  = 262144
	maxBytesDefaultRowValue = 262144 // the per-target rows that follow max_bytes_default
)

// targetRow is the §3.1 baseline of one target.
type targetRow struct {
	maxBytes int
	truncate bool // false = refuse on over-budget
}

// targetRows are the §3.1 defaults, verbatim.
var targetRows = map[types.ScrubTarget]targetRow{
	types.TgEventMsg:       {262144, true},
	types.TgStack:          {262144, true},
	types.TgHeader:         {8192, false},
	types.TgEnv:            {65536, false},
	types.TgJournalTail:    {65536, true},
	types.TgConfigSnapshot: {262144, false},
	types.TgSkill:          {131072, false},
	types.TgIssue:          {65536, false},
	types.TgBoard:          {32768, false},
	types.TgDSN:            {512, false},
	types.TgSpool:          {4194304, false},
}

// targetBudget is a resolved per-target budget.
type targetBudget struct {
	maxBytes int
	truncate bool
}

// configTOML mirrors the [scrub] schema. Every field is decoded strictly: an
// unknown key anywhere under [scrub] is TROUBLE-SCRUB-002, because a typo must
// not silently disable a knob (SPEC-02 §3.2).
type configTOML struct {
	RulesVersion    *int                      `toml:"rules_version"`
	MaxBytesDefault *int                      `toml:"max_bytes_default"`
	RuleTimeout     *string                   `toml:"rule_timeout"`
	Prefilter       *bool                     `toml:"prefilter"`
	BoundaryVerify  *bool                     `toml:"boundary_verify"`
	Entropy         *bool                     `toml:"entropy"`
	PIIMode         *string                   `toml:"pii_mode"`
	PathMode        *string                   `toml:"path_mode"`
	PathAllowlist   []string                  `toml:"path_allowlist"`
	HomeRoots       []string                  `toml:"home_roots"`
	Targets         map[string]targetTOML     `toml:"targets"`
	Rules           map[string]ruleToggleTOML `toml:"rules"`
	Projects        map[string]projectTOML    `toml:"projects"`
}

type targetTOML struct {
	MaxBytes *int   `toml:"max_bytes"`
	OnOver   string `toml:"on_over"`
}

type ruleToggleTOML struct {
	Enabled *bool `toml:"enabled"`
}

type projectTOML struct {
	Disable       []string   `toml:"disable"`
	PIIMode       *string    `toml:"pii_mode"`
	PathMode      *string    `toml:"path_mode"`
	PathAllowlist []string   `toml:"path_allowlist"`
	HomeRoots     []string   `toml:"home_roots"`
	Rules         []ruleTOML `toml:"rules"`
}

type ruleTOML struct {
	Name      string   `toml:"name"`
	Kind      string   `toml:"kind"`
	Pattern   string   `toml:"pattern"`
	Replace   string   `toml:"replace"`
	Mandatory *bool    `toml:"mandatory"`
	Targets   []string `toml:"targets"`
	Rescan    *bool    `toml:"rescan"`
}

// configDoc accepts both shapes internal/lifecycle may hand over: the bytes of
// the `[scrub]` subtree, or a document that still carries the [scrub] header.
type configDoc struct {
	Scrub *configTOML `toml:"scrub"`
}

// projectOverride is one row of [scrub.projects."<id>"].
type projectOverride struct {
	id        string
	disable   map[string]bool
	piiMode   *string
	pathMode  *string
	allowlist []string
	homeRoots []string
	rules     []*compiledRule
}

// resolved is the compiled, validated configuration.
type resolved struct {
	rulesVersion    int
	maxBytesDefault int
	ruleTimeout     time.Duration
	prefilter       bool
	entropy         bool
	piiMode         string
	pathMode        string
	allowlist       []string
	homeRoots       []string
	budgets         map[types.ScrubTarget]targetBudget
	builtinOff      map[string]bool // optional rules removed for every host and project
	projects        map[string]*projectOverride
}

// parseConfig strict-decodes the [scrub] config subtree (SPEC-02 §3.2).
func parseConfig(cfgTOML []byte) (*configTOML, error) {
	if len(cfgTOML) == 0 {
		return &configTOML{}, nil
	}
	var doc configDoc
	md, err := toml.Decode(string(cfgTOML), &doc)
	if err != nil {
		return nil, configError("scrub", "config is not valid TOML: %v", err)
	}
	out := doc.Scrub
	if out == nil {
		// the bytes are the subtree itself
		out = &configTOML{}
		md, err = toml.Decode(string(cfgTOML), out)
		if err != nil {
			return nil, configError("scrub", "config is not valid TOML: %v", err)
		}
	}
	if bad := md.Undecoded(); len(bad) > 0 {
		return nil, configError(bad[0].String(), "unknown key under [scrub]")
	}
	return out, nil
}

// resolveConfig validates the decoded config against the configured projects
// and produces the engine's runtime configuration (SPEC-02 §3.2 table).
func resolveConfig(c *configTOML, projects []types.Project) (*resolved, error) {
	r := &resolved{
		rulesVersion:    DefaultRulesVersion,
		maxBytesDefault: DefaultMaxBytes,
		ruleTimeout:     DefaultRuleTimeout,
		prefilter:       DefaultPrefilter,
		entropy:         DefaultEntropy,
		piiMode:         DefaultPIIMode,
		pathMode:        DefaultPathMode,
		allowlist:       append([]string(nil), DefaultPathAllowlist...),
		homeRoots:       append([]string(nil), DefaultHomeRoots...),
		budgets:         map[types.ScrubTarget]targetBudget{},
		builtinOff:      map[string]bool{},
		projects:        map[string]*projectOverride{},
	}
	// §3.1 budgets
	for _, t := range types.ScrubTargets {
		row := targetRows[t]
		mb := row.maxBytes
		if row.maxBytes == maxBytesDefaultRowValue {
			mb = r.maxBytesDefault
		}
		r.budgets[t] = targetBudget{maxBytes: mb, truncate: row.truncate}
	}

	knownProject := make(map[string]bool, len(projects))
	for _, p := range projects {
		knownProject[p.ID] = true
	}

	// §3.2 scalars
	if c.RulesVersion != nil {
		if *c.RulesVersion < 1 {
			return nil, configError("scrub.rules_version", "rules_version must be >= 1")
		}
		r.rulesVersion = *c.RulesVersion
	}
	if c.MaxBytesDefault != nil {
		if *c.MaxBytesDefault <= 0 {
			return nil, configError("scrub.max_bytes_default", "max_bytes_default must be > 0")
		}
		r.maxBytesDefault = *c.MaxBytesDefault
		for _, t := range types.ScrubTargets {
			row := targetRows[t]
			if row.maxBytes == maxBytesDefaultRowValue {
				r.budgets[t] = targetBudget{maxBytes: r.maxBytesDefault, truncate: row.truncate}
			}
		}
	}
	if c.RuleTimeout != nil {
		d, err := time.ParseDuration(*c.RuleTimeout)
		if err != nil || d <= 0 {
			return nil, configError("scrub.rule_timeout", "rule_timeout must be a positive Go duration")
		}
		r.ruleTimeout = d
	}
	if c.Prefilter != nil {
		r.prefilter = *c.Prefilter
	}
	if c.BoundaryVerify != nil {
		if !*c.BoundaryVerify {
			return nil, configError("scrub.boundary_verify",
				"boundary_verify is fixed: the persistence-boundary re-scan is not configurable")
		}
		r.boundaryVerifyStrict()
	}
	if c.Entropy != nil {
		r.entropy = *c.Entropy
		if !*c.Entropy {
			r.builtinOff["entropy_token"] = true
		}
	}
	if c.PIIMode != nil {
		switch *c.PIIMode {
		case "redact", "keep":
			r.piiMode = *c.PIIMode
		default:
			return nil, configError("scrub.pii_mode", "pii_mode must be redact or keep")
		}
	}
	if c.PathMode != nil {
		switch *c.PathMode {
		case "keep", "redact":
			r.pathMode = *c.PathMode
		default:
			return nil, configError("scrub.path_mode", "path_mode must be keep or redact")
		}
	}
	if c.PathAllowlist != nil {
		r.allowlist = append([]string(nil), c.PathAllowlist...)
	}
	if c.HomeRoots != nil {
		r.homeRoots = append([]string(nil), c.HomeRoots...)
	}

	// §3.1 per-target overrides
	for name, t := range c.Targets {
		tgt := types.ScrubTarget(name)
		if !tgt.Valid() {
			return nil, configError("scrub.targets."+name, "unknown scrub target")
		}
		b := r.budgets[tgt]
		if t.MaxBytes != nil {
			if *t.MaxBytes <= 0 {
				return nil, configError("scrub.targets."+name+".max_bytes", "max_bytes must be > 0")
			}
			b.maxBytes = *t.MaxBytes
		}
		switch t.OnOver {
		case "":
		case "truncate":
			b.truncate = true
		case "refuse":
			b.truncate = false
		default:
			return nil, configError("scrub.targets."+name+".on_over", "on_over must be truncate or refuse")
		}
		r.budgets[tgt] = b
	}

	// §3.2 [scrub.rules.<name>]: only `enabled`, only optional built-ins
	for name, t := range c.Rules {
		if isMandatoryName(name) {
			return nil, configError("scrub.rules."+name,
				"mandatory rules cannot be configured: they are compiled from the binary")
		}
		if !isOptionalName(name) {
			return nil, configError("scrub.rules."+name, "unknown built-in rule")
		}
		if t.Enabled != nil && !*t.Enabled {
			r.builtinOff[name] = true
		}
	}

	// §3.2 [scrub.projects."<id>"]
	for id, p := range c.Projects {
		if !knownProject[id] {
			return nil, configError("scrub.projects."+id, "no project with this id is configured")
		}
		po := &projectOverride{id: id, disable: map[string]bool{}}
		for _, n := range p.Disable {
			if isMandatoryName(n) {
				return nil, configError("scrub.projects."+id+".disable",
					"mandatory rules cannot be disabled")
			}
			if !isOptionalName(n) {
				return nil, configError("scrub.projects."+id+".disable", "unknown built-in rule")
			}
			po.disable[n] = true
		}
		if p.PIIMode != nil {
			switch *p.PIIMode {
			case "redact", "keep":
				po.piiMode = p.PIIMode
			default:
				return nil, configError("scrub.projects."+id+".pii_mode", "pii_mode must be redact or keep")
			}
		}
		if p.PathMode != nil {
			switch *p.PathMode {
			case "keep", "redact":
				po.pathMode = p.PathMode
			default:
				return nil, configError("scrub.projects."+id+".path_mode", "path_mode must be keep or redact")
			}
		}
		po.allowlist = append([]string(nil), p.PathAllowlist...)
		po.homeRoots = append([]string(nil), p.HomeRoots...)
		seen := map[string]bool{}
		rescan := 0
		for i, pr := range p.Rules {
			key := "scrub.projects." + id + ".rules"
			cr, err := compileProjectRule(pr, key, seen)
			if err != nil {
				return nil, err
			}
			if cr.rescan {
				rescan++
				if rescan > MaxProjectRescanRules {
					return nil, configError(key,
						"at most %d project rules may set rescan = true", MaxProjectRescanRules)
				}
			}
			_ = i
			po.rules = append(po.rules, cr)
		}
		r.projects[id] = po
	}
	return r, nil
}

func (r *resolved) boundaryVerifyStrict() {}

// compileProjectRule validates and compiles one [[scrub.projects."<id>".rules]]
// row (SPEC-02 §3.2 + §3.3).
func compileProjectRule(pr ruleTOML, key string, seen map[string]bool) (*compiledRule, error) {
	if !validRuleName.MatchString(pr.Name) {
		return nil, configError(key+".name",
			"rule name must match ^[a-z][a-z0-9_]{2,31}$ so the marker grammar stays unambiguous")
	}
	if isMandatoryName(pr.Name) || isOptionalName(pr.Name) {
		return nil, configError(key+".name", "duplicate rule name: a built-in rule already owns this name")
	}
	if seen[pr.Name] {
		return nil, configError(key+".name", "duplicate rule name")
	}
	seen[pr.Name] = true
	kind := types.RuleKind(pr.Kind)
	switch kind {
	case types.KindRegex, types.KindPrefix, types.KindEntropy:
	case types.KindDSNPart, types.KindPathAllowlist:
		return nil, configError(key+".kind",
			"config-authored dsn_part and path_allowlist rules are refused: those kinds are engine parser paths")
	case "":
		return nil, configError(key+".kind", "kind is required")
	default:
		return nil, configError(key+".kind", "unknown rule kind")
	}
	if pr.Replace == "" {
		return nil, configError(key+".replace", "replace must be a non-empty marker")
	}
	if len(pr.Targets) == 0 {
		return nil, configError(key+".targets", "targets must name at least one scrub target")
	}
	tm := targetMask(0)
	for _, t := range pr.Targets {
		st := types.ScrubTarget(t)
		if !st.Valid() {
			return nil, configError(key+".targets", "unknown scrub target")
		}
		tm |= maskOf([]types.ScrubTarget{st})
	}
	if pr.Mandatory != nil && *pr.Mandatory {
		return nil, configError(key+".mandatory",
			"only the compiled-in rule set is mandatory; a project rule cannot claim the bit")
	}
	c := &compiledRule{
		name: pr.Name, kind: kind, pattern: pr.Pattern,
		replaceS: pr.Replace, replace: []byte(pr.Replace),
		targets: tm,
	}
	if pr.Rescan != nil {
		c.rescan = *pr.Rescan
	}
	switch kind {
	case types.KindRegex:
		if pr.Pattern == "" {
			return nil, configError(key+".pattern", "pattern is required for kind = regex")
		}
		re, err := regexpCompile(pr.Pattern)
		if err != nil {
			return nil, configError(key+".pattern", "pattern does not compile: %v", err)
		}
		c.re, c.groups = re, re.NumSubexp()
	case types.KindPrefix:
		if pr.Pattern == "" {
			return nil, configError(key+".pattern", "pattern is required for kind = prefix")
		}
		c.begins = []string{pr.Pattern}
	}
	return c, nil
}

// buildTables assembles the effective rule table of every project (SPEC-02
// §3.6 rule 1: built-ins in table order, then project rules in declared order).
func (e *Engine) buildTables(compiled []*compiledRule) error {
	global, err := e.tableFor(nil, compiled)
	if err != nil {
		return err
	}
	e.tables = map[string]*ruleTable{"": global}
	for id, po := range e.cfg.projects {
		t, err := e.tableFor(po, compiled)
		if err != nil {
			return err
		}
		e.tables[id] = t
		e.projectIDs = append(e.projectIDs, id)
	}
	return nil
}

// tableFor builds one effective table: the enabled built-ins in table order
// followed by the project's own rules.
func (e *Engine) tableFor(po *projectOverride, compiled []*compiledRule) (*ruleTable, error) {
	t := &ruleTable{
		projectID: "",
		allowlist: e.cfg.allowlist,
		homeRoots: e.cfg.homeRoots,
	}
	pii := e.cfg.piiMode == "redact"
	pathRedact := e.cfg.pathMode == "redact"
	pii, pathRedact = e.projectModes(po, pii, pathRedact)
	t.allowlist, t.homeRoots = e.projectPaths(po)
	for _, c := range compiled {
		switch c.name {
		case "entropy_token":
			if !e.cfg.entropy || e.cfg.builtinOff[c.name] {
				continue
			}
		case "path_disclosure":
			if !pathRedact {
				continue
			}
		case "pii_email_ip", "pii_identity_kv":
			if !pii {
				continue
			}
		}
		if po != nil && po.disable[c.name] {
			continue
		}
		t.rules = append(t.rules, e.entry(c, t.homeRoots))
	}
	if po != nil {
		for _, c := range po.rules {
			t.rules = append(t.rules, e.entry(c, t.homeRoots))
		}
		t.projectID = po.id
	}
	if po != nil && len(po.rules) > MaxProjectRules {
		return nil, configError("scrub.projects."+po.id+".rules",
			"%d project rules, the maximum is %d (13 mandatory + 5 optional + %d project = %d)",
			len(po.rules), MaxProjectRules, MaxProjectRules, RuleTableSize)
	}
	if len(t.rules) > RuleTableSize {
		key := "scrub"
		if po != nil {
			key = "scrub.projects." + po.id
		}
		return nil, configError(key, "effective rule set has %d rules, the maximum is %d",
			len(t.rules), RuleTableSize)
	}
	for _, r := range t.rules {
		t.unionMask = orMaskInto(t.unionMask, r.gate)
		t.unionSignals |= r.signals
	}
	// A rule with no gate (a config-authored project rule, whose trigger
	// alphabet is unknown) makes the table unprovable: the prefilter must not run
	// at all, or it would gate out a rule it cannot see (§3.6 rule 10).
	ungated := false
	for _, r := range t.rules {
		t.unionMask = orMaskInto(t.unionMask, r.gate)
		t.unionSignals |= r.signals
		if r.signals == 0 && maskEmpty(r.gate) {
			ungated = true
		}
	}
	t.prefilter = e.cfg.prefilter && e.trig != nil && !ungated &&
		(!maskEmpty(t.unionMask) || t.unionSignals != 0)
	return t, nil
}

func (e *Engine) projectModes(po *projectOverride, pii, pathRedact bool) (bool, bool) {
	if po == nil {
		return pii, pathRedact
	}
	if po.piiMode != nil {
		pii = *po.piiMode == "redact"
	}
	if po.pathMode != nil {
		pathRedact = *po.pathMode == "redact"
	}
	return pii, pathRedact
}

func (e *Engine) projectPaths(po *projectOverride) ([]string, []string) {
	if po == nil {
		return e.cfg.allowlist, e.cfg.homeRoots
	}
	a := e.cfg.allowlist
	if len(po.allowlist) > 0 {
		a = po.allowlist
	}
	h := e.cfg.homeRoots
	if len(po.homeRoots) > 0 {
		h = po.homeRoots
	}
	return a, h
}

func isMandatoryName(n string) bool {
	for _, m := range mandatoryNames {
		if m == n {
			return true
		}
	}
	return false
}

func isOptionalName(n string) bool {
	for _, m := range optionalNames {
		if m == n {
			return true
		}
	}
	return false
}
