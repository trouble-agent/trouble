package research

// slug.go — class-slug derivation, the real work of SPEC-07 §3.1.
//
// The Off-by-One lab is keyed on a kebab-case `problem_class`; trouble holds
// signatures and units. Everything between them is this file: a pure, total
// function (no error return, no network, no filesystem) that turns a sig plus
// its subject facts into a ClassSlug, and never blocks a rung.

import (
	"regexp"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// subjectFacts is everything the derivation reads about an incident's subject:
// the sig, the unit/app/scope name and the texts the taxonomy matches over.
type subjectFacts struct {
	Sig        string
	Source     types.SigSource
	Subject    string // pre-derived subject when the caller has one
	Unit       string // journald _SYSTEMD_UNIT, dbus object-path unit, timer unit
	ObjectPath string // dbus: the full path, for the unit fallback
	Project    string // sentinel: project slug
	App        string // collector: configured app name
	Mount      string // disk: mount point
	Path       string // inotify: watched path
	Scope      string // psi: cpu | memory | io
	Origin     string // Origin.Source ("journald:payment-worker")
	Message    string
	Stack      string
	AppKind    string // explicit app kind when the config table pins one
	UnitKind   map[string]string
	Markers    []string
}

// maxSubjectChars is the §3.1 truncation boundary for a normalized subject.
const maxSubjectChars = 40

// maxSlugChars is the §3.1 truncation boundary for a composed slug.
const maxSlugChars = 64

// DeriveClassSlug is the spec's total derivation. A caller-supplied slug wins
// (Request checks it before calling); this function is the sensor-sourced path
// where nothing but the sig and its facts exist.
func DeriveClassSlug(sig types.Sig, f subjectFacts, t *table) types.ClassSlug {
	if t == nil {
		t = newTable()
	}
	if f.Sig == "" {
		f.Sig = sig.String()
	}
	if f.Source == "" {
		f.Source = sig.Source
	}
	subject := kebabSubject(subjectOf(f), f)
	appKind := appKindOf(f, subject)
	taxonomy := classifyFacts(t.rules(), f)

	cs := types.ClassSlug{Source: string(f.Source), AppKind: appKind, Taxonomy: taxonomy}

	// (1) exact — (source, subject, taxonomy) all equal
	// (2) source wildcard — the entry's subject is "*"
	// (3) taxonomy wildcard — the entry's taxonomy is "*"
	if e, ok := t.lookup(string(f.Source), subject, taxonomy); ok {
		cs.Slug = truncSlug(render(e.Template, subject, taxonomy))
		return cs
	}

	// (4) composition — kebab(subject) + "-" + kebab(taxonomy)
	if subject != "" {
		cs.Slug = truncSlug(kebab(subject) + "-" + kebab(taxonomy))
		return cs
	}
	if taxonomy != "" && taxonomy != TaxError {
		cs.Slug = truncSlug(kebab(taxonomy))
		return cs
	}

	// (5) fallback — nothing identified the subject and nothing matched a rule.
	// The bucket is deliberately not used as a slug here: submitting under a
	// shared generic key would pollute a shared cache (§6.3).
	cs.Slug = kebab(t.fallbackSlug())
	cs.Fallback = true
	return cs
}

// fallbackSlug resolves the configured fallback slug.
func (t *table) fallbackSlug() string {
	if t.fallback != "" {
		return t.fallback
	}
	return defaultFallbackSlug
}

// defaultFallbackSlug mirrors the research.fallback_slug default; the config
// path overwrites it via SetFallbackSlug before any derivation runs.
const defaultFallbackSlug = "unknown"

// rules returns the active taxonomy rules (the table holds them so a config
// reload can swap both the entries and the rules in one step).
func (t *table) rules() []taxonomyRule {
	if t.tax != nil {
		return t.tax
	}
	return builtinTaxonomy
}

// subjectOf derives the raw subject per source (SPEC-07 §3.1 table). An explicit
// Subject always wins; the rest is the per-source reading of the incident's
// identity, with Origin.Source's suffix as the documented fallback.
func subjectOf(f subjectFacts) string {
	if f.Subject != "" {
		return f.Subject
	}
	switch f.Source {
	case types.SrcJournald:
		if f.Unit != "" {
			return f.Unit
		}
	case types.SrcDBus:
		if f.ObjectPath != "" {
			if u := unitFromObjectPath(f.ObjectPath); u != "" {
				return u
			}
		}
		if f.Unit != "" {
			return f.Unit
		}
	case types.SrcSentinel:
		if f.Project != "" {
			return f.Project
		}
	case types.SrcCollector:
		if f.App != "" {
			return f.App
		}
	case types.SrcDisk:
		if f.Mount != "" {
			return f.Mount
		}
	case types.SrcInotify:
		if f.Path != "" {
			return pathBase(f.Path)
		}
	case types.SrcTimers:
		if f.Unit != "" {
			return f.Unit
		}
	case types.SrcPSI:
		if f.Scope != "" {
			return f.Scope
		}
	}
	return originSuffix(f.Origin)
}

// originSuffix returns the part of Origin.Source after the first ':' — the
// documented fallback for every source ("journald:payment-worker" →
// "payment-worker").
func originSuffix(origin string) string {
	if i := strings.IndexByte(origin, ':'); i >= 0 && i+1 < len(origin) {
		return origin[i+1:]
	}
	return ""
}

// unitFromObjectPath pulls the unit out of a dbus object path. The path escapes
// '-' as "_2d" (e.g. /org/freedesktop/systemd1/unit/payment_2dworker_2eservice).
func unitFromObjectPath(p string) string {
	i := strings.LastIndex(p, "/unit/")
	if i < 0 {
		return ""
	}
	u := p[i+len("/unit/"):]
	u = strings.ReplaceAll(u, "_2d", "-")
	u = strings.ReplaceAll(u, "_2e", ".")
	u = strings.ReplaceAll(u, "_5f", "_")
	u = strings.TrimSuffix(u, "_2f")
	if j := strings.IndexByte(u, '/'); j >= 0 {
		u = u[:j]
	}
	return u
}

// pathBase is the basename of a watched path, without a trailing separator.
func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// kebabSubject normalizes a raw subject: unit suffixes stripped, instance ids
// collapsed, container ids removed, then kebab() and the 40-char boundary.
func kebabSubject(raw string, f subjectFacts) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if isHexID(s) {
		// A 64-hex container id never enters a slug (§6.10): fall back to the
		// configured app kind, which is the useful identity of a container.
		k := f.AppKind
		if k == "" {
			k = "container"
		}
		return k
	}
	s = stripUnitSuffix(s)
	s = instanceToAt(s)
	s = kebab(s)
	return truncBoundary(s, maxSubjectChars)
}

// unitSuffixes are the systemd suffixes the subject normalization strips.
var unitSuffixes = []string{".service", ".timer", ".socket", ".scope", ".target", ".mount", ".device", ".path"}

// stripUnitSuffix removes one trailing systemd unit suffix (case-insensitive,
// before kebab so the '.' never becomes a '-').
func stripUnitSuffix(s string) string {
	low := strings.ToLower(s)
	for _, suf := range unitSuffixes {
		if strings.HasSuffix(low, suf) {
			return s[:len(s)-len(suf)]
		}
	}
	return s
}

// instanceToAt collapses a templated unit instance: "name@1234" → "name@",
// which is what groups every instance of one templated unit under one class.
func instanceToAt(s string) string {
	i := strings.IndexByte(s, '@')
	if i < 0 {
		return s
	}
	j := i + 1
	for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '-' || s[j] == '.') {
		j++
	}
	if j == i+1 {
		return s
	}
	return s[:i+1] + s[j:]
}

// hexIDRe matches a container-shaped hex id (12..64 lowercase hex chars).
var hexIDRe = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// isHexID reports whether s is a container-shaped hex id.
func isHexID(s string) bool { return hexIDRe.MatchString(s) }

// nonAlnumRe is the kebab delimiter class: every run of non [a-z0-9] becomes one
// dash.
var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

// kebab lowercases, collapses every non-alphanumeric run into a single dash and
// trims the edges. An empty result stays empty (the caller decides the fallback).
func kebab(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonAlnumRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// truncBoundary cuts s to at most n bytes, preferring a dash boundary so a slug
// is a word prefix and never a half-token.
func truncBoundary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '-'); i > 0 {
		return cut[:i]
	}
	return strings.TrimRight(cut, "-")
}

// truncSlug applies the 64-char boundary to a composed slug.
func truncSlug(s string) string { return truncBoundary(kebab(s), maxSlugChars) }

// appKindOf classifies the subject (SPEC-07 §3.1): an explicit table entry wins,
// then container markers over Origin.Source, then systemd unit shape, else
// unknown.
func appKindOf(f subjectFacts, subject string) string {
	if k := f.UnitKind[subject]; k != "" {
		return k
	}
	if f.AppKind != "" {
		return f.AppKind
	}
	low := strings.ToLower(f.Origin)
	for _, m := range f.Markers {
		if m == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(m)) {
			return "container"
		}
	}
	if strings.Contains(low, "docker") || strings.Contains(low, "podman") || strings.Contains(low, "containerd") {
		return "container"
	}
	if f.Unit != "" {
		return "unit"
	}
	switch f.Source {
	case types.SrcJournald, types.SrcDBus, types.SrcTimers:
		return "unit"
	case types.SrcCollector, types.SrcGeneric:
		return "script"
	}
	return "unknown"
}

// SetFallbackSlug overrides the fallback slug (research.fallback_slug).
func (t *table) SetFallbackSlug(s string) {
	if k := kebab(s); k != "" {
		t.fallback = k
	}
}

// SetTaxonomy replaces the active rule list (config reload path).
func (t *table) SetTaxonomy(rules []taxonomyRule) {
	if len(rules) > 0 {
		t.tax = rules
	}
}
