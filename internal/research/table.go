package research

// table.go — the class-slug config table (SPEC-07 §3.1).
//
// The table is the operator's mapping from (source, subject, taxonomy) onto a
// lab class slug. Built-in entries cover every shipped source generically and
// carry no fleet value; a `research.table` file merges over them (file entries
// win, built-ins that the file does not mention stay). A malformed file is
// refused by construction and never fails a rung: the caller (SPEC-12 config)
// reports it and the active table stays in place.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// slugEntry is one table row: a match triple, an optional template and a note.
type slugEntry struct {
	Slug     string
	Source   string // "*" matches anything
	Subject  string
	Taxonomy string
	Template string // {subject} / {taxonomy} placeholders
	Note     string
	Builtin  bool
}

// table is the ordered entry list plus the lookup index used by the derivation.
// The list order is the resolution order (exact → source wildcard → taxonomy
// wildcard); the index is a map from the concrete triple to its entry.
type table struct {
	entries  []slugEntry
	exact    map[string]int
	bySrc    map[string][]int // source → indices with subject=="*"
	byTax    map[string][]int // taxonomy → indices with taxonomy=="*"
	src      string           // path the file came from ("" = built-in only)
	tax      []taxonomyRule   // active taxonomy rules (nil = built-ins)
	fallback string           // research.fallback_slug ("" = the built-in default)
}

// tableFile is the TOML shape of a research class table.
type tableFile struct {
	ClassSlug []entryTOML `toml:"class_slug"`
}

type entryTOML struct {
	Slug     string            `toml:"slug"`
	Match    map[string]string `toml:"match"`
	Template string            `toml:"template"`
	Note     string            `toml:"note"`
}

// LoadTable loads a class table file and merges it over the built-in entries.
// An empty path returns the built-ins. A malformed file (bad slug, empty match,
// unknown match key, unreadable path) is an error: SPEC-07 §3.1 makes a table
// failure a config-load failure, never a rung failure.
func LoadTable(path string) (*table, error) {
	t := newTable()
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("research table %s: %w", path, err)
	}
	var doc tableFile
	if err := toml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("research table %s: %w", path, err)
	}
	for i, e := range doc.ClassSlug {
		ent, err := normalizeEntry(e, fmt.Sprintf("%s:class_slug[%d]", path, i))
		if err != nil {
			return nil, err
		}
		t.add(ent)
	}
	t.src = path
	return t, nil
}

// newTable returns the built-in table of SPEC-07 §3.1: one generic entry per
// shipped source, no fleet mapping in any default.
func newTable() *table {
	t := &table{}
	for _, e := range builtinEntries() {
		t.add(e)
	}
	return t
}

// builtinEntries is the §3.1 default set. Order matters: exact psi entries
// precede the crash-loop wildcards so a PSI resource event resolves exactly.
func builtinEntries() []slugEntry {
	return []slugEntry{
		{Slug: "host-cpu-pressure", Template: "host-cpu-pressure", Source: "psi", Subject: "cpu", Taxonomy: TaxResourceExhaustion, Builtin: true},
		{Slug: "host-memory-pressure", Template: "host-memory-pressure", Source: "psi", Subject: "memory", Taxonomy: TaxResourceExhaustion, Builtin: true},
		{Slug: "host-io-pressure", Template: "host-io-pressure", Source: "psi", Subject: "io", Taxonomy: TaxResourceExhaustion, Builtin: true},
		{Slug: "{subject}-crash-loop", Source: "journald", Subject: "*", Taxonomy: TaxCrashLoop, Template: "{subject}-crash-loop", Builtin: true},
		{Slug: "{subject}-crash-loop", Source: "dbus", Subject: "*", Taxonomy: TaxCrashLoop, Template: "{subject}-crash-loop", Builtin: true},
		{Slug: "{subject}-unhandled-exception", Source: "sentinel", Subject: "*", Taxonomy: TaxCrashLoop, Template: "{subject}-unhandled-exception", Builtin: true},
		{Slug: "{subject}-unhandled-exception", Source: "collector", Subject: "*", Taxonomy: TaxCrashLoop, Template: "{subject}-unhandled-exception", Builtin: true},
		{Slug: "{subject}-permission-denied", Source: "journald", Subject: "*", Taxonomy: TaxPermissionDenied, Template: "{subject}-permission-denied", Builtin: true},
		{Slug: "{subject}-permission-denied", Source: "dbus", Subject: "*", Taxonomy: TaxPermissionDenied, Template: "{subject}-permission-denied", Builtin: true},
		{Slug: "{subject}-config-error", Source: "journald", Subject: "*", Taxonomy: TaxConfigError, Template: "{subject}-config-error", Builtin: true},
		{Slug: "{subject}-network-timeout", Source: "*", Subject: "*", Taxonomy: TaxNetworkTimeout, Template: "{subject}-network-timeout", Builtin: true},
		{Slug: "{subject}-queue-wedge", Source: "*", Subject: "*", Taxonomy: TaxQueueWedge, Template: "{subject}-queue-wedge", Builtin: true},
		{Slug: "{subject}-data-corruption", Source: "*", Subject: "*", Taxonomy: TaxDataCorruption, Template: "{subject}-data-corruption", Builtin: true},
		{Slug: "{subject}-dependency-failure", Source: "*", Subject: "*", Taxonomy: TaxDependencyFailure, Template: "{subject}-dependency-failure", Builtin: true},
	}
}

// add inserts an entry and refreshes the indexes; a file entry with the same
// match triple replaces the built-in in place, so operator intent wins without
// reordering the table.
func (t *table) add(e slugEntry) {
	for i, old := range t.entries {
		if old.Source == e.Source && old.Subject == e.Subject && old.Taxonomy == e.Taxonomy {
			t.entries[i] = e
			t.reindex()
			return
		}
	}
	t.entries = append(t.entries, e)
	t.reindex()
}

func (t *table) reindex() {
	t.exact = make(map[string]int, len(t.entries))
	t.bySrc = map[string][]int{}
	t.byTax = map[string][]int{}
	for i, e := range t.entries {
		t.exact[key3(e.Source, e.Subject, e.Taxonomy)] = i
		if e.Subject == "*" {
			t.bySrc[e.Source] = append(t.bySrc[e.Source], i)
		}
		if e.Taxonomy == "*" {
			t.byTax[e.Taxonomy] = append(t.byTax[e.Taxonomy], i)
		}
	}
}

// lookup resolves the entry for a concrete triple using the §3.1 resolution
// order: exact → source wildcard → taxonomy wildcard. The bool is false when no
// entry matched, which is the composition/fallback path.
func (t *table) lookup(source, subject, taxonomy string) (slugEntry, bool) {
	// (1) exact.
	if i, ok := t.exact[key3(source, subject, taxonomy)]; ok {
		return t.entries[i], true
	}
	// (2) source wildcard: the entry matches every subject of this source.
	for _, i := range t.bySrc[source] {
		e := t.entries[i]
		if e.Taxonomy == taxonomy || e.Taxonomy == "*" {
			return e, true
		}
	}
	// (3) taxonomy wildcard: the entry matches every source/subject for this bucket.
	for _, e := range t.entries {
		if e.Taxonomy != taxonomy && e.Taxonomy != "*" {
			continue
		}
		if e.Source != "*" && e.Source != source {
			continue
		}
		if e.Subject != "*" && e.Subject != subject {
			continue
		}
		return e, true
	}
	return slugEntry{}, false
}

// subjectMatches reports whether an entry's subject pattern accepts a subject.
func subjectMatches(pattern, subject string) bool {
	return pattern == "*" || pattern == subject
}

// key3 is the exact-index key. The separator is a byte no slug field can carry
// (fields are kebab-cased, so they hold [a-z0-9-] only).
func key3(source, subject, taxonomy string) string {
	return source + "\x1f" + subject + "\x1f" + taxonomy
}

// normalizeEntry validates one file entry into a slugEntry.
func normalizeEntry(e entryTOML, where string) (slugEntry, error) {
	if e.Slug == "" && e.Template == "" {
		return slugEntry{}, fmt.Errorf("research table %s: slug or template is required", where)
	}
	if e.Slug != "" && !isKebab(e.Slug) {
		return slugEntry{}, fmt.Errorf("research table %s: slug %q is not kebab-case", where, e.Slug)
	}
	if e.Template != "" {
		if !strings.Contains(e.Template, "{subject}") && !strings.Contains(e.Template, "{taxonomy}") {
			return slugEntry{}, fmt.Errorf("research table %s: template %q has no {subject}/{taxonomy} placeholder", where, e.Template)
		}
		if e.Slug != "" && !isKebab(strings.NewReplacer("{subject}", "s", "{taxonomy}", "t").Replace(e.Template)) {
			return slugEntry{}, fmt.Errorf("research table %s: template %q is not kebab-case", where, e.Template)
		}
	}
	ent := slugEntry{Slug: e.Slug, Template: e.Template, Note: e.Note}
	for k, v := range e.Match {
		if v == "" {
			return slugEntry{}, fmt.Errorf("research table %s: match.%s is empty", where, k)
		}
		switch k {
		case "source":
			ent.Source = v
		case "subject":
			ent.Subject = v
		case "taxonomy":
			ent.Taxonomy = v
		default:
			return slugEntry{}, fmt.Errorf("research table %s: unknown match key %q", where, k)
		}
	}
	if ent.Source == "" || ent.Subject == "" || ent.Taxonomy == "" {
		return slugEntry{}, fmt.Errorf("research table %s: match needs source, subject and taxonomy", where)
	}
	if ent.Slug == "" {
		ent.Slug = ent.Template
	}
	if ent.Template == "" {
		ent.Template = ent.Slug
	}
	return ent, nil
}

// isKebab reports whether s is lowercase kebab-case: [a-z0-9]+(-[a-z0-9]+)*.
func isKebab(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	prevDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevDash = false
		case c == '-':
			if prevDash {
				return false
			}
			prevDash = true
		default:
			return false
		}
	}
	return true
}

// render fills a template's placeholders. There is no template language: the
// two placeholders are textual, and an absent value leaves the token out.
func render(tpl, subject, taxonomy string) string {
	s := strings.ReplaceAll(tpl, "{subject}", subject)
	s = strings.ReplaceAll(s, "{taxonomy}", taxonomy)
	return s
}

// errNoTablePath is returned by LoadTable when a caller asks for a file table
// but resolves an empty path with a non-default expectation.
var errNoTablePath = errors.New("research: table path is empty")
