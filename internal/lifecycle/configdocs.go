package lifecycle

// configdocs.go — the generated configuration reference (DOC-24).
//
// docs/reference/config.md is written by `trouble config explain --markdown`,
// which calls ConfigReferenceMarkdown. The page is derived, never hand-written:
// the key set, the TOML table, the type cell and the default cell all come out
// of the registry and the Config struct, and the meanings come out of
// configKeyDocs below. configdocs_doc_test.go is the drift gate — it fails when
// the registry grows a key with no meaning, when a meaning outlives its key,
// when a key has no struct field to take a type from, or when the committed
// page differs from what this generator prints today.
//
// Why a generator rather than a page plus a key-list test: the page states its
// own measured key count, and the count is the thing that rots. Here the count
// is len(registry), so it cannot.

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// configReferenceAbsentPath is the config file the page is generated against: a
// path that cannot exist. Resolve then reports every key at its built-in
// default, which is what this page documents, and the bytes are the same on a
// configured host and on a bare one.
const configReferenceAbsentPath = "/nonexistent/trouble-config-reference/config.toml"

// configDocGroupOrder fixes the order of the page's tables. It is not derived
// on purpose: a reader expects the lifecycle surface before the sensors, and
// alphabetical order would scatter related tables. The gate fails when a group
// exists that this list does not name, so a new table forces a deliberate
// placement instead of landing at the bottom.
var configDocGroupOrder = []string{
	"", // top-level keys
	"secrets",
	"lifecycle",
	"lifecycle.migrate",
	"stall",
	"checker",
	"ingest",
	"ingest.auth",
	"sentinel",
	"sentinel.routes",
	"registry",
	"ladder",
	"dashboard",
	"dashboard.auth",
	"dashboard.rate",
	"sensors",
	"sensors.psi",
	"sensors.journald",
	"sensors.dbus",
	"sensors.disk",
	"sensors.timers",
	"sensors.inotify",
	"sensors.rules",
	"sensors.limits",
	"hub",
	"server",
	"server.redis",
	"server.duckbrain",
	"spool",
	"flow",
	"verify",
	"escalate",
	"fs",
	"origin",
}

// configDocTypeOverrides carries the type of the keys that have no struct field
// to reflect on: the three subsystem tables are held verbatim under
// `toml:"-"`, so the walk in configStructFieldTypes never sees them.
var configDocTypeOverrides = map[string]reflect.Type{
	"issues": reflect.TypeOf(SubsystemTable{}),
	"skills": reflect.TypeOf(SubsystemTable{}),
	"llm":    reflect.TypeOf(SubsystemTable{}),
}

// configDocStructPath maps a registry key to its path in the Config struct when
// the two spellings differ. `Routes` is tagged `routes` but registered leaf by
// leaf under `sentinel.routes.*` (TRBL-061), which is the only divergence.
var configDocStructPath = map[string]string{
	"sentinel.routes.default":   "routes.default",
	"sentinel.routes.per_class": "routes.per_class",
}

// configDocSecretCell is the default cell printed for a redacted key. The value
// of such a key is a credential, and the page never carries one: it names the
// indirection instead (see the key's own meaning).
const configDocSecretCell = `""`

// configDocPathSecret is the one redacted key whose value is not a credential
// but the path to one — the operator has to know where the env file lives, and
// knowing the path reveals nothing about what is inside it.
const configDocPathSecret = "secrets.environment_file"

// ConfigReferenceMarkdown renders docs/reference/config.md.
//
// The command that writes the page is:
//
//	go run ./cmd/trouble config explain --markdown > docs/reference/config.md
//
// and re-running it on an unchanged tree produces no diff — the drift gate
// compares the committed file against this function's output byte for byte.
func ConfigReferenceMarkdown() (string, error) {
	res, err := Resolve(nil, []string{}, configReferenceAbsentPath)
	if err != nil {
		return "", fmt.Errorf("config reference: resolve defaults: %w", err)
	}
	rows, err := Explain(res, nil)
	if err != nil {
		return "", fmt.Errorf("config reference: explain: %w", err)
	}
	return renderConfigReference(rows, res.Config)
}

// renderConfigReference is the pure half: it takes the explain rows (one per
// registry key, in registry order) plus the config they were resolved into, and
// produces the page.
func renderConfigReference(rows []types.ConfigValue, cfg Config) (string, error) {
	fieldTypes, err := configStructFieldTypes()
	if err != nil {
		return "", err
	}

	// Index the rows and check the meaning table in both directions: a key with
	// no meaning is the drift this page exists to catch, and a meaning that
	// outlives its key is the mirror image of it.
	byKey := make(map[string]types.ConfigValue, len(rows))
	for _, r := range rows {
		byKey[r.Key] = r
	}
	var problems []string
	for _, r := range rows {
		if _, ok := configKeyDocs[r.Key]; !ok {
			problems = append(problems, fmt.Sprintf("registry key %q has no entry in configKeyDocs", r.Key))
		}
		if _, ok := configDocTypeOverrides[r.Key]; !ok {
			path := configDocStructPath[r.Key]
			if path == "" {
				path = r.Key
			}
			if _, ok := fieldTypes[path]; !ok {
				problems = append(problems, fmt.Sprintf("registry key %q has no field in the Config struct (looked for %q)", r.Key, path))
			}
		}
	}
	for key := range configKeyDocs {
		if _, ok := byKey[key]; !ok {
			problems = append(problems, fmt.Sprintf("configKeyDocs carries %q, which the registry no longer exposes", key))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return "", fmt.Errorf("config reference: %s", strings.Join(problems, "; "))
	}

	// Group by TOML table: a key's table is its key minus the last element, so
	// `sensors.journald.queue` files under `[sensors.journald]`.
	groups := make(map[string][]types.ConfigValue)
	for _, r := range rows {
		groups[configDocGroup(r.Key)] = append(groups[configDocGroup(r.Key)], r)
	}
	var unknown []string
	known := make(map[string]bool, len(configDocGroupOrder))
	for _, g := range configDocGroupOrder {
		known[g] = true
	}
	for g := range groups {
		if !known[g] {
			unknown = append(unknown, g)
		}
	}
	for _, g := range configDocGroupOrder {
		if len(groups[g]) == 0 {
			unknown = append(unknown, g+" (empty)")
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", fmt.Errorf("config reference: configDocGroupOrder is out of step with the registry: %s — place the new table deliberately", strings.Join(unknown, ", "))
	}

	var b strings.Builder
	paths := configDocPathsFor()

	fmt.Fprintf(&b, "# Configuration reference\n\n")
	fmt.Fprintf(&b, "Every configuration key the binary exposes at HEAD, grouped by TOML table.\n")
	fmt.Fprintf(&b, "The page is generated from the registry that backs `trouble config explain`:\n")
	fmt.Fprintf(&b, "the key set, the types and the defaults are read out of the binary, so a key\n")
	fmt.Fprintf(&b, "cannot be resolvable and undocumented at the same time.\n\n")
	fmt.Fprintf(&b, "* **Keys at HEAD (measured on this tree):** %d\n", len(rows))
	fmt.Fprintf(&b, "* **TOML tables on this page:** %d\n", len(configDocGroupOrder))
	fmt.Fprintf(&b, "* **Generated by:** `go run ./cmd/trouble config explain --markdown > docs/reference/config.md`\n")
	fmt.Fprintf(&b, "* **Drift gate:** `go test ./internal/lifecycle/ -run TestConfigReference` — fails when the\n")
	fmt.Fprintf(&b, "  registry grows a key this page does not carry, when a meaning outlives its key,\n")
	fmt.Fprintf(&b, "  when a key has no default to print, or when the committed page and the generator\n")
	fmt.Fprintf(&b, "  disagree.\n\n")
	fmt.Fprintf(&b, "Re-running the generator on this tree produces no diff.\n\n")

	fmt.Fprintf(&b, "## How to read a row\n\n")
	fmt.Fprintf(&b, "* **key** — the config key, spelled as the TOML key inside its table.\n")
	fmt.Fprintf(&b, "* **type** — the type of the `Config` field the key sets, read by reflection.\n")
	fmt.Fprintf(&b, "* **default** — the built-in default: what a host with no config file, no\n")
	fmt.Fprintf(&b, "  `TROUBLE_*` variable and no key flag runs. Paths are written as templates\n")
	fmt.Fprintf(&b, "  (`${XDG_STATE_HOME:-~/.local/state}`, `~`) rather than one host's paths, so the\n")
	fmt.Fprintf(&b, "  cell is the same bytes on every host.\n")
	fmt.Fprintf(&b, "* **from** — the layer that supplied the value printed on this page. It reads\n")
	fmt.Fprintf(&b, "  `default` on every row: the page is generated against an absent config file with\n")
	fmt.Fprintf(&b, "  no environment and no key flag, so the built-in default is always what won.\n")
	fmt.Fprintf(&b, "  `trouble config explain` on a configured host reports `file`, `env` or `flag` for\n")
	fmt.Fprintf(&b, "  the keys that host overrides.\n")
	fmt.Fprintf(&b, "* **meaning** — what the key does to the running daemon, not a restatement of its\n")
	fmt.Fprintf(&b, "  name.\n\n")
	fmt.Fprintf(&b, "Five keys are **declarations** rather than scalars — `projects`, `issues`,\n")
	fmt.Fprintf(&b, "`skills`, `llm` and `sensors.inotify.paths`. They are arrays of tables, they are\n")
	fmt.Fprintf(&b, "marked `file only` in the type column, and a scalar flag or variable cannot\n")
	fmt.Fprintf(&b, "express one (TROUBLE-LIFECYCLE-001).\n\n")

	fmt.Fprintf(&b, "## Precedence and flag spelling\n\n")
	fmt.Fprintf(&b, "Quoted from `deploy/README.md`, which is where the precedence sentence is\n")
	fmt.Fprintf(&b, "stated; every other operator doc points here instead of restating it:\n\n")
	fmt.Fprintf(&b, "> Precedence is flag > env > file > default.\n\n")
	fmt.Fprintf(&b, "So a key may be set four ways, and the first layer that supplies it wins:\n")
	fmt.Fprintf(&b, "the daemon's `--<flag>`, then its `TROUBLE_*` variable\n")
	fmt.Fprintf(&b, "(see [env-vars.md](env-vars.md)), then a key in this table's TOML section,\n")
	fmt.Fprintf(&b, "then the default on this page.\n\n")
	fmt.Fprintf(&b, "A key's flag spelling, quoted from `docs/cmd.md` rather than restated:\n\n")
	fmt.Fprintf(&b, "> any registered key: `a.b_c` → `--a-b_c`\n\n")
	fmt.Fprintf(&b, "`trouble config explain` accepts only `--state_root` of its own (plus `--key`,\n")
	fmt.Fprintf(&b, "`--json` and `--markdown`); the rest of the flag layer belongs to the daemon.\n\n")
	fmt.Fprintf(&b, "## Secrets\n\n")
	fmt.Fprintf(&b, "No credential value appears on this page. A key that names a credential shows\n")
	fmt.Fprintf(&b, "`\"\"` as its default and states in its meaning which environment variable or\n")
	fmt.Fprintf(&b, "file supplies the value; `secrets.environment_file` is the path of the 0600 file\n")
	fmt.Fprintf(&b, "resolution loads variables from, which is an indirection rather than a\n")
	fmt.Fprintf(&b, "credential.\n\n")

	for _, group := range configDocGroupOrder {
		grows := groups[group]
		sort.Slice(grows, func(i, j int) bool { return grows[i].Key < grows[j].Key })
		if group == "" {
			fmt.Fprintf(&b, "## Top level\n\n")
			fmt.Fprintf(&b, "Keys written at the root of the config file, outside any table.\n\n")
		} else {
			fmt.Fprintf(&b, "## `[%s]`\n\n", group)
		}
		fmt.Fprintf(&b, "| key | type | default | from | meaning |\n")
		fmt.Fprintf(&b, "| --- | --- | --- | --- | --- |\n")
		for _, r := range grows {
			short := r.Key
			if group != "" {
				short = strings.TrimPrefix(r.Key, group+".")
			}
			typ, err := configDocType(r.Key, fieldTypes)
			if err != nil {
				return "", err
			}
			def, err := configDocDefault(r.Key, r, paths)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", short, typ, def, r.Source, configKeyDocs[r.Key])
		}
		fmt.Fprintf(&b, "\n")
	}

	return b.String(), nil
}

// configDocGroup returns the TOML table a key belongs to (its key minus the last
// element), or "" for a top-level key.
func configDocGroup(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[:i]
	}
	return ""
}

// configDocPaths are the three host directories the generated page templates
// away: the two XDG bases and the home directory. They are read from the same
// environment the defaults use, so a host that sets XDG_STATE_HOME or
// XDG_CONFIG_HOME still produces the same bytes as one that does not.
type configDocPaths struct {
	home       string
	stateBase  string
	configBase string
}

// configDocPathsFor reads the directories to template from the environment.
func configDocPathsFor() configDocPaths {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	stateBase := os.Getenv("XDG_STATE_HOME")
	if stateBase == "" && home != "" {
		stateBase = filepath.Join(home, ".local", "state")
	}
	configBase := os.Getenv("XDG_CONFIG_HOME")
	if configBase == "" && home != "" {
		configBase = filepath.Join(home, ".config")
	}
	return configDocPaths{home: home, stateBase: stateBase, configBase: configBase}
}

// template replaces the generating host's directories with the templates the
// page documents, longest prefix first.
func (p configDocPaths) template(text string) string {
	repl := []struct{ from, to string }{
		{p.stateBase, "${XDG_STATE_HOME:-~/.local/state}"},
		{p.configBase, "${XDG_CONFIG_HOME:-~/.config}"},
		{p.home, "~"},
	}
	for _, r := range repl {
		if r.from == "" || r.from == "/" {
			continue
		}
		text = strings.ReplaceAll(text, r.from, r.to)
	}
	return text
}

// configStructFieldTypes walks the Config struct and returns the field type of
// every TOML key path, e.g. "sensors.journald.queue_bytes" → int.
//
// It descends only into structs this package declares (plus anonymous ones such
// as the `routes` table), so a named type from another package — types.Duration
// above all — is a leaf and not mistaken for a table.
func configStructFieldTypes() (map[string]reflect.Type, error) {
	out := make(map[string]reflect.Type)
	pkgPath := reflect.TypeOf(Config{}).PkgPath()

	var walk func(prefix string, t reflect.Type) error
	walk = func(prefix string, t reflect.Type) error {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("toml")
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			ft := f.Type
			if ft.Kind() == reflect.Struct && (ft.PkgPath() == "" || ft.PkgPath() == pkgPath) {
				if err := walk(path, ft); err != nil {
					return err
				}
				continue
			}
			if _, dup := out[path]; dup {
				return fmt.Errorf("config reference: two Config fields carry the TOML path %q", path)
			}
			out[path] = ft
		}
		return nil
	}
	if err := walk("", reflect.TypeOf(Config{})); err != nil {
		return nil, err
	}
	return out, nil
}

// configDocType is the type cell for one key.
func configDocType(key string, fieldTypes map[string]reflect.Type) (string, error) {
	t, ok := configDocTypeOverrides[key]
	if !ok {
		path := configDocStructPath[key]
		if path == "" {
			path = key
		}
		t, ok = fieldTypes[path]
		if !ok {
			return "", fmt.Errorf("config reference: key %q has no type (%q is not a field of Config)", key, path)
		}
	}
	label, err := configTypeLabel(t)
	if err != nil {
		return "", fmt.Errorf("config reference: key %q: %w", key, err)
	}
	if configDocFileOnly(t) {
		return label + " (file only)", nil
	}
	return label, nil
}

// configTypeLabel names a Go type the way an operator writing TOML would.
func configTypeLabel(t reflect.Type) (string, error) {
	// types.Duration is a string type with a duration grammar, so it has to be
	// recognised before the Kind switch below calls it a plain string.
	if t == reflect.TypeOf(types.Duration("")) {
		return "duration", nil
	}
	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Int, reflect.Int64:
		return "integer", nil
	case reflect.Float64:
		return "float", nil
	case reflect.Slice:
		switch t.Elem().Kind() {
		case reflect.String:
			return "string array", nil
		case reflect.Slice:
			return "array of string arrays", nil
		case reflect.Map, reflect.Struct:
			return "array of tables", nil
		}
		return "", fmt.Errorf("unexpected slice element %s", t.Elem())
	case reflect.Map:
		switch t.Elem().Kind() {
		case reflect.String:
			return "string map", nil
		case reflect.Struct:
			return "duration map", nil
		}
		return "", fmt.Errorf("unexpected map value %s", t.Elem())
	case reflect.Struct:
		if t == reflect.TypeOf(SubsystemTable{}) {
			return "table", nil
		}
		return "", fmt.Errorf("unexpected struct type %s", t)
	}
	return "", fmt.Errorf("unexpected type %s", t)
}

// configDocFileOnly reports whether a key is a declaration: an array of tables,
// or a subsystem table. Those are the values a scalar flag or variable cannot
// express (TROUBLE-LIFECYCLE-001), so the page says so in the type cell.
func configDocFileOnly(t reflect.Type) bool {
	if t == reflect.TypeOf(SubsystemTable{}) {
		return true
	}
	if t.Kind() != reflect.Slice {
		return false
	}
	switch t.Elem().Kind() {
	case reflect.Map, reflect.Struct:
		return true
	}
	return false
}

// configDocDefault is the default cell for one key: the built-in default this
// page documents, templated so the bytes do not depend on the generating host.
func configDocDefault(key string, row types.ConfigValue, paths configDocPaths) (string, error) {
	if key == configDocPathSecret {
		// The redaction flag masks this row too, but the value is a path, and
		// the operator is the one who has to create the file: document the
		// built-in default from the resolver that produces it.
		return paths.template(defaultEnvFile()), nil
	}
	if row.Redacted {
		return configDocSecretCell, nil
	}
	text, err := configValueText(row.Value)
	if err != nil {
		return "", fmt.Errorf("config reference: key %q: %w", key, err)
	}
	return paths.template(text), nil
}

// configValueText renders a resolved value as TOML-ish text for a table cell.
func configValueText(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "`[]`", nil
	case string:
		if x == "" {
			return `""`, nil
		}
		return "`" + x + "`", nil
	case bool:
		return fmt.Sprintf("`%t`", x), nil
	case int:
		return fmt.Sprintf("`%d`", x), nil
	case int64:
		return fmt.Sprintf("`%d`", x), nil
	case float64:
		return fmt.Sprintf("`%s`", strings.TrimSuffix(fmt.Sprintf("%v", x), ".0")), nil
	case types.Duration:
		return "`" + string(x) + "`", nil
	case []string:
		if len(x) == 0 {
			return "`[]`", nil
		}
		return "`" + strings.Join(x, "`, `") + "`", nil
	case [][]string:
		if len(x) == 0 {
			return "`[]`", nil
		}
		parts := make([]string, 0, len(x))
		for _, row := range x {
			parts = append(parts, strings.Join(row, " "))
		}
		return "`" + strings.Join(parts, "`, `") + "`", nil
	case []map[string]any:
		// A declaration key: the default is none declared, and a host that
		// declares some is not what this page documents.
		if len(x) == 0 {
			return "`[]`", nil
		}
		return fmt.Sprintf("`%d declared`", len(x)), nil
	case map[string]string:
		return "`" + configDocPairs(len(x), func(add func(string)) {
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(k + "=" + x[k])
			}
		}) + "`", nil
	case map[string]types.Duration:
		return "`" + configDocPairs(len(x), func(add func(string)) {
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(k + "=" + string(x[k]))
			}
		}) + "`", nil
	}
	return "", fmt.Errorf("no cell format for %T (add it to configValueText)", v)
}

// configDocPairs joins key=value pairs as TOML inline-table text, sorted by the
// caller so the bytes are stable.
func configDocPairs(n int, fill func(add func(string))) string {
	if n == 0 {
		return "{}"
	}
	parts := make([]string, 0, n)
	fill(func(s string) { parts = append(parts, s) })
	return strings.Join(parts, ", ")
}

// configKeyDocs is the meaning column: one sentence per registry key, saying
// what the key does to the running daemon.
//
// The gate enforces the table both ways — a registry key with no entry here
// stops the generator, and an entry whose key the registry dropped stops it
// too — so this map cannot silently fall behind the binary.
var configKeyDocs = map[string]string{
	// Top level.
	"state_root":  "Root of every directory the daemon writes — heartbeat, checker state, spool, plays and the hub's own state — so moving this one key moves all of them.",
	"config_path": "The TOML file resolution reads; a path that does not exist is treated as no file at all, not as an error.",
	"health_url":  "URL the stall checker and the upgrade READY wait poll for health; empty derives it from `dashboard.bind`.",
	"projects":    "The `[[projects]]` declarations: one table per reporter project (id, public_key, optional secret, quota, disk budget, loss policy), and the set the sentinel opens ingest for — an empty set never opens the port.",
	"issues":      "The `[issues]` table exactly as declared; core config carries it verbatim and the issues subsystem's own loader is the only judge of its contents.",
	"skills":      "The `[skills]` table exactly as declared; core config carries it verbatim and the skills loop's own loader is the only judge of its contents.",
	"llm":         "The agent stage's `[llm]` table exactly as declared; unlike the other subsystem tables a declaration that cannot be built refuses the boot.",

	// [secrets]
	"secrets.environment_file": "Path of the 0600 env file resolution loads variable values from; the credentials live in that file, and this page carries the path only.",

	// [lifecycle]
	"lifecycle.systemd_scope":           "Whether the shipped units run under the user manager (`user`) or the system manager (`system`), which decides where `systemctl` looks for them.",
	"lifecycle.unit_name":               "Name of the daemon's own unit, used by drain, upgrade and the checker's restart probe.",
	"lifecycle.escalate_unit":           "Template unit the escalation ladder instantiates per rung (`trouble-escalate@.service`).",
	"lifecycle.checker_unit":            "Name of the stall checker's unit, which the daemon restarts when the checker stops probing.",
	"lifecycle.sandbox":                 "Hardening profile applied to the shipped unit files; `standard` is the documented posture.",
	"lifecycle.user":                    "Account the units run as under the system manager; empty means the invoking user under the user manager.",
	"lifecycle.heartbeat_path":          "File the daemon rewrites each heartbeat; the checker reads it to decide whether the daemon is alive.",
	"lifecycle.heartbeat_interval":      "How often the daemon rewrites its heartbeat file while it is working.",
	"lifecycle.heartbeat_stale_after":   "Age at which the checker calls the heartbeat stale, so it must stay above the heartbeat interval.",
	"lifecycle.idle_heartbeat_interval": "Heartbeat cadence while the daemon has no work, so an idle daemon still proves it is alive.",
	"lifecycle.watchdog_sec":            "systemd watchdog window the daemon must feed, and the shutdown budget the unit is given on stop.",
	"lifecycle.drain_timeout":           "How long a drain may take before the daemon stops waiting for in-flight work and exits.",
	"lifecycle.upgrade_ready_timeout":   "How long an upgrade waits for the new binary to report READY before rolling back.",
	"lifecycle.rollback_depth":          "How many previous binary generations are kept on disk for rollback.",
	"lifecycle.self_rss_warn":           "Resident-set size at which the daemon warns about its own memory use, written with a unit suffix (`80MB`).",
	"lifecycle.clock_skew_tolerance":    "Skew allowed between the daemon's clock and the timestamps it reads before it treats the difference as an incident.",

	// [lifecycle.migrate]
	"lifecycle.migrate.downgrade_ok": "Allow a binary older than the ledger schema to migrate the store down instead of refusing the boot.",

	// [stall]
	"stall.max_seq_age": "Age of the newest sequence number at which the ingest stream counts as stalled rather than quiet.",

	// [checker]
	"checker.interval":      "How often the stall checker probes the health surface.",
	"checker.confirm_runs":  "Consecutive failed probes required before the checker declares the service stalled, so one missed probe is forgiven.",
	"checker.state_file":    "File holding the checker's probe history and confirmation counters, so a checker restart does not reset them.",
	"checker.alarm_file":    "File the checker appends its alarm records to, one record per confirmed stall.",
	"checker.alarm_command": "argv the checker runs when an alarm fires; empty records the alarm file and notifies nobody.",

	// [ingest]
	"ingest.bind":                      "Host:port the ingest listener binds; a loopback bind keeps reports on this host.",
	"ingest.advertised_host":           "Host name reporters should use in the DSN handed to them; empty derives `localhost` when the bind is loopback, and stays empty on any other bind so the boot preflight can refuse it.",
	"ingest.auth.loopback_dsn":         "Accept DSN authentication from loopback reporters without a per-project secret.",
	"ingest.auth.nonloopback_mode":     "How a non-loopback reporter authenticates: `token` asks for a declared project secret, `proxy` trusts the terminating proxy.",
	"ingest.auth.public_require_proxy": "Refuse a public reporter that arrives without the proxy in front, so a token alone cannot expose ingest to the internet.",

	// [sentinel]
	"sentinel.dedup_window": "How far back the sentinel folds an identical generic JSON payload into the event it already recorded.",

	// [sentinel.routes]
	"sentinel.routes.default":   "Route mode applied to a signature with no class entry; empty leaves routing to the sentinel's own compiled default.",
	"sentinel.routes.per_class": "Signature-prefix → route-mode table the sentinel's resolver consults before falling back to the default.",

	// [registry]
	"registry.plays_dir": "Directory the play registry and the ladder both read (`<name>@<version>.toml`); empty derives `<state_root>/plays`, and the single row is deliberate so the two consumers cannot disagree.",

	// [ladder]
	"ladder.drive_interval": "How often the ladder's rung driver re-evaluates which rung is active.",

	// [dashboard]
	"dashboard.bind":                   "Host:port the dashboard listener binds; a non-loopback bind is what turns the §2.4 exposure checks on.",
	"dashboard.port":                   "Port the dashboard advertises in links and redirects; the listener itself binds `dashboard.bind`.",
	"dashboard.mandate":                "Declared exposure mandate (`proxy` or `tailnet`), required as soon as the bind leaves loopback — the boot refuses without it.",
	"dashboard.token_file":             "0600 store the dashboard API tokens are minted into and read from; the `dashboard token` verbs address this file.",
	"dashboard.poll_ms":                "Client poll interval for the console's content partial, in milliseconds.",
	"dashboard.strip_poll_ms":          "Poll interval for the strip accelerator, in milliseconds, so the header refreshes faster than the body.",
	"dashboard.stall_alert_s":          "Age at which the console shows its stale banner because the newest sequence has not moved.",
	"dashboard.health_loopback_exempt": "Answer `/health.json` from the loopback interface without a session, so a local probe does not need a token.",
	"dashboard.read_only":              "Refuse every mutating request, leaving the console as a viewer.",
	"dashboard.allow_resume":           "Permit the endpoints that clear a kill-switch, which are otherwise refused.",
	"dashboard.allow_full":             "Permit the full-timeline gate mode instead of the bounded window.",
	"dashboard.page_limit":             "Rows per page and per partial response; the dashboard clamps it at 500.",
	"dashboard.max_body_bytes":         "Largest request body the dashboard accepts before it refuses the POST.",
	"dashboard.auth_fail_limit":        "Failed authentications allowed per window before the client is refused.",
	"dashboard.auth_fail_window":       "Window the failed-authentication counter is measured over.",
	"dashboard.mem_pressure_pct":       "Share of the memory high-water mark at which the dashboard sheds poll load instead of growing.",
	"dashboard.proxy_trusted":          "Trust the forwarding headers of a proxy in front of the dashboard for the client address.",
	"dashboard.proxy_cidrs":            "Networks whose forwarding headers are trusted; only consulted when `dashboard.proxy_trusted` is on.",
	"dashboard.public_origin":          "Exact `scheme://host[:port]` the console is reached on, required off loopback so cookies and links are built against it.",
	"dashboard.project_scope":          "Projects a multi-project console may show; required when more than one project is declared and the bind is not loopback.",

	// [dashboard.auth]
	"dashboard.auth.transport":         "How the session credential travels — `cookie` is the shipped default for browser sessions.",
	"dashboard.auth.identity_provider": "Which identity seam authenticates the console (`token` in v0.1); it selects the session/header handling the dashboard runs.",
	"dashboard.auth.session_ttl":       "Lifetime of an authenticated dashboard session before the credential has to be presented again.",

	// [dashboard.rate]
	"dashboard.rate.read_rps":    "Sustained read requests per second a single client may issue.",
	"dashboard.rate.read_burst":  "Read requests a client may issue back-to-back before the sustained rate applies.",
	"dashboard.rate.write_rps":   "Sustained mutating requests per second a single client may issue.",
	"dashboard.rate.write_burst": "Mutating requests a client may issue back-to-back before the sustained rate applies.",

	// [sensors]
	"sensors.sample_fold_window": "How far back sensor samples are folded into one incident before a new one is opened.",
	"sensors.merge_window":       "Window inside which two detections of the same signal merge instead of forming separate incidents.",

	// [sensors.psi]
	"sensors.psi.enabled":         "Run the pressure-stall-information sensor, which reports CPU, memory and IO contention before it becomes a stall.",
	"sensors.psi.sample_interval": "How often PSI counters are sampled.",
	"sensors.psi.window":          "Averaging window each PSI sample reports over, which must stay at or above the sample interval to mean anything.",

	// [sensors.journald]
	"sensors.journald.enabled":                "Run the journald sensor, which turns the journal into events.",
	"sensors.journald.units":                  "Units whose journal entries the sensor follows; `file only` in practice means an empty list follows the units the daemon itself knows about.",
	"sensors.journald.follow_all":             "Follow every unit in the journal rather than the configured list, which raises volume accordingly.",
	"sensors.journald.follow_all_max_entries": "Entries the follow-all cursor may scan per pass, bounding the work a busy journal creates.",
	"sensors.journald.queue":                  "Entries buffered between the journal reader and the ingest path.",
	"sensors.journald.queue_bytes":            "Byte budget of that buffer, so one large burst of messages cannot grow the queue without limit.",
	"sensors.journald.max_entry":              "Largest journal entry the sensor forwards; a longer entry is truncated rather than dropped.",
	"sensors.journald.probe_interval":         "How often the sensor re-checks that the journal it reads is still the one it started on.",

	// [sensors.dbus]
	"sensors.dbus.enabled":             "Run the D-Bus sensor, which reports unit and service state through the message bus.",
	"sensors.dbus.user_managers":       "User managers the sensor watches; `self` means the manager of the user the daemon runs as.",
	"sensors.dbus.ping_interval":       "How often the bus connection is pinged to prove it is still usable.",
	"sensors.dbus.reconcile_interval":  "How often the sensor re-reads the bus's object tree to resynchronise after missed signals.",
	"sensors.dbus.oomd_probe_interval": "How often the oomd state is probed, which is a slower cadence because oomd's own counters move slowly.",

	// [sensors.disk]
	"sensors.disk.enabled":  "Run the disk sensor, which reports space and inode pressure per mount.",
	"sensors.disk.mounts":   "Mount points the sensor reports on; an empty list reports nothing, so the default `/` is what keeps the root filesystem watched.",
	"sensors.disk.interval": "How often the watched mounts are re-measured.",

	// [sensors.timers]
	"sensors.timers.enabled":  "Run the timer sensor, which reports systemd timer state and lateness.",
	"sensors.timers.interval": "How often timer state is re-read.",

	// [sensors.inotify]
	"sensors.inotify.enabled":          "Run the inotify sensor, which reports file changes under the declared paths.",
	"sensors.inotify.paths":            "The `[[sensors.inotify.paths]]` declarations — one table per watched path (path/mask/recursive/max_depth/rule) — and the one sensors key that is a declaration rather than a scalar.",
	"sensors.inotify.recheck_interval": "How often the watch set is rebuilt, so a directory that appears or disappears is picked up.",
	"sensors.inotify.max_depth":        "Directory depth the watch descends under each declared path.",

	// [sensors.rules]
	"sensors.rules.dir":             "Directory the detection rules are loaded from; empty derives `<state_root>/../config/rules.d`, so moving the state root moves the rules with it.",
	"sensors.rules.reload_debounce": "Quiet period after a rules directory change before the rule set is reloaded, so a batch of writes causes one reload.",

	// [sensors.limits]
	"sensors.limits.rule_per_min":     "Detections a single rule may emit per minute before the sensor drops the rest.",
	"sensors.limits.source_per_min":   "Detections a single source may emit per minute, which stops one noisy unit from crowding out the rest.",
	"sensors.limits.global_per_min":   "Detections all rules together may emit per minute, which is the ceiling that protects the ingest path.",
	"sensors.limits.incidents_per_5m": "Incidents that may be opened per five minutes before further detections are folded into existing ones.",

	// [hub]
	"hub.mode":                  "Which side of a hub/edge pair this daemon runs as (`hub` or an edge that forwards to one).",
	"hub.url":                   "Base URL of the hub an edge forwards to; empty means this daemon forwards nowhere.",
	"hub.forward_project_id":    "Project the forwarded records are attributed to on the hub, when the edge's own project ids must not be reused.",
	"hub.token":                 "Shared secret an edge presents to its hub — supplied by `TROUBLE_HUB_TOKEN` or the `[hub]` table, and never printed here.",
	"hub.protocol_version":      "Forwarding protocol version the edge speaks, which the hub refuses when it no longer supports it.",
	"hub.forward_batch_records": "Records per forwarding request, which trades latency against per-request overhead.",
	"hub.forward_batch_bytes":   "Byte cap per forwarding request, so a batch of large records is split rather than sent as one body.",
	"hub.retry_base":            "First backoff step after a failed forward.",
	"hub.retry_max":             "Backoff ceiling, so a hub that stays down is retried slowly rather than hammered.",
	"hub.dedup_lru":             "Records the edge remembers to suppress a record it already forwarded, before the oldest are forgotten.",

	// [server]
	"server.profile": "Which server shape this daemon boots as — `standalone` or `light-hub` — and therefore which of the redis and duckbrain keys below are read at all.",
	"server.hub_id":  "Identity this server presents to its peers, recorded as the origin of the records it writes.",

	// [server.redis]
	"server.redis.url":                 "Redis endpoint the queue and the dedup gate connect to; a password is not part of this value, it comes from `server.redis.password_env`.",
	"server.redis.password_env":        "NAME of the environment variable that holds the Redis password (default `TROUBLE_REDIS_PASSWORD`); the password itself is never a config value, so this page can name the variable and nothing more.",
	"server.redis.stream":              "Stream the daemon reads and writes events on.",
	"server.redis.group":               "Consumer group the queue's readers belong to, which decides how a record is claimed exactly once.",
	"server.redis.consumer":            "Name this instance uses inside the consumer group; empty derives one from the host identity.",
	"server.redis.maxlen":              "Approximate stream length the daemon trims to, which bounds memory at the cost of old entries.",
	"server.redis.dedup_prefix":        "Key prefix of the dedup gate, which is what keeps two trouble deployments on one Redis from colliding.",
	"server.redis.dedup_ttl":           "How long a dedup key is remembered, and therefore the widest window in which a duplicate can be suppressed.",
	"server.redis.batch_records":       "Records per read request.",
	"server.redis.batch_bytes":         "Byte cap per read request, so a batch of large records is split.",
	"server.redis.block_ms":            "How long a read blocks waiting for new records before it returns empty, which trades latency against polling.",
	"server.redis.claim_min_idle":      "Idle time before a record claimed by a dead consumer is reclaimed by a live one.",
	"server.redis.dial_timeout":        "Connect timeout for the Redis socket.",
	"server.redis.read_timeout":        "Read timeout for a Redis request.",
	"server.redis.write_timeout":       "Write timeout for a Redis request.",
	"server.redis.require_redis":       "Refuse to boot when Redis is unreachable rather than degrade to a local-only queue.",
	"server.redis.require_persistence": "Refuse a Redis whose persistence is not configured, because a restart would then lose queued events.",
	"server.redis.check_policy":        "Check the Redis eviction and persistence policy at boot, which is what catches a cache-only Redis before it drops events.",
	"server.redis.failover_grace":      "How long reconnection is retried before the queue is declared failed.",

	// [server.duckbrain]
	"server.duckbrain.enabled":                "Archive older ledger generations to DuckBrain, so local storage does not grow without bound.",
	"server.duckbrain.namespace":              "DuckBrain namespace the archives are written under.",
	"server.duckbrain.endpoint":               "DuckBrain endpoint the archives are sent to; empty means the client's own resolution decides.",
	"server.duckbrain.archive_interval":       "How often the archiver looks for generations that are old enough to ship.",
	"server.duckbrain.archive_batch_files":    "Files per archive request, which bounds the size of one upload.",
	"server.duckbrain.keep_local_generations": "Ledger generations kept on local disk after archiving, which is the rollback depth for a bad archive.",
	"server.duckbrain.verify_after_write":     "Re-read an archived generation after writing it, so a silent upload failure does not pass as success.",
	"server.duckbrain.gzip":                   "Compress archive bodies, trading local CPU for upload size.",

	// [spool]
	"spool.budget_bytes":      "Total disk the spool may occupy; past it the daemon refuses new spooled records instead of filling the disk.",
	"spool.gap_reserve_bytes": "Headroom kept free inside the budget so a gap marker can always be written even when the spool is full.",
	"spool.fsync":             "Durability policy for spooled records — `group` coalesces fsyncs over `spool.fsync_window_ms` instead of syncing each write.",
	"spool.fsync_window_ms":   "Window the grouped fsync policy collects writes over before it syncs.",

	// [flow]
	"flow.spool_max_entries":  "Records the dispatch queue may spool before it starts dropping under its loss policy.",
	"flow.spool_ttl":          "Age at which a spooled record is abandoned rather than replayed, so a stuck queue does not replay ancient events.",
	"flow.spool_max_attempts": "Delivery attempts before a spooled record is given up on.",
	"flow.replay_every":       "How often the spool is replayed.",
	"flow.replay_batch":       "Records per replay pass, which bounds how much one pass can push.",

	// [verify]
	"verify.zone_windows": "How long a verification result stays valid per trust zone (`loopback`, `lan`, `tailnet`, `public`), i.e. how often each zone is re-verified.",

	// [escalate]
	"escalate.channels": "The channels an escalation rung runs, one argv per channel; empty means a rung can only be observed, not executed.",
	"escalate.timeout":  "How long one escalation channel may run before the rung is treated as failed.",

	// [fs]
	"fs.forbidden_state_roots": "Filesystem paths the daemon refuses as a state root, because a volatile mount there would lose the ledger on reboot.",
	"fs.remote_types":          "Filesystem types refused for the state root, because a network mount would put the ledger behind a link that can vanish.",

	// [origin]
	"origin.host_id": "Identity stamped on the records this daemon originates, which is how the hub tells two hosts' events apart.",
	"origin.hub_id":  "Hub identity stamped on records that arrived through a hub, so their origin survives a re-forward.",
}
