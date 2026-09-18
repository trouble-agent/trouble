package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// projects_test.go pins the `[[projects]]` config surface (SPEC-12 §3.1a): the
// array-of-tables the parser reads, the defaults a minimal declaration inherits,
// the strict refusal of a malformed declaration, the rendered (never revealed)
// explain row, and the shipped example carrying a usable declaration.

const (
	projKeyA = "0123456789abcdef0123456789abcdef"
	projKeyB = "fedcba9876543210fedcba9876543210"
	projSecA = "00112233445566778899aabbccddeeff"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestResolveProjectsArrayOfTables(t *testing.T) {
	path := writeConfig(t, `state_root = "/tmp/projects-test"

[[projects]]
id = "1"
public_key = "`+projKeyA+`"

[[projects]]
id = "7"
slug = "payment-api"
public_key = "`+projKeyB+`"
secret = "`+projSecA+`"
quota_epm = 1200
disk_budget_bytes = 1048576
loss_policy = "spool-if-light"
auth_forms = ["x_sentry_auth", "query_sentry_key"]
enabled = false
`)
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Config.Projects) != 2 {
		t.Fatalf("parsed %d projects, want 2 (%+v)", len(res.Config.Projects), res.Config.Projects)
	}
	// A minimal declaration: slug defaults to the id, enabled defaults true.
	first := res.Config.Projects[0]
	if first.ID != "1" || first.PublicKey != projKeyA {
		t.Errorf("first declaration = %+v", first)
	}
	if !first.Enabled {
		t.Errorf("a declaration without `enabled` must be enabled: %+v", first)
	}
	// Every declared key lands.
	second := res.Config.Projects[1]
	if second.Slug != "payment-api" || second.Secret != projSecA || second.QuotaEPM != 1200 ||
		second.DiskBudgetBytes != 1048576 || second.LossPolicy != "spool-if-light" ||
		len(second.AuthForms) != 2 || second.Enabled {
		t.Errorf("second declaration = %+v", second)
	}

	// The explain row is rendered, not copied: the declared project is visible,
	// its secret is not.
	var row types.ConfigValue
	found := false
	for _, cv := range res.Values {
		if cv.Key == "projects" {
			row, found = cv, true
		}
	}
	if !found {
		t.Fatalf("resolved values carry no projects row")
	}
	summary, _ := row.Value.(string)
	if !strings.Contains(summary, "2 declared") || !strings.Contains(summary, projKeyA) ||
		!strings.Contains(summary, "secret (set)") || !strings.Contains(summary, "disabled") {
		t.Errorf("projects row = %q, want a rendered declaration list", summary)
	}
	b, err := explainJSON(res.Values)
	if err != nil {
		t.Fatalf("explainJSON: %v", err)
	}
	if strings.Contains(string(b), projSecA) {
		t.Errorf("the explain dump leaks a declared project secret: %s", b)
	}
	if !strings.Contains(string(b), projKeyA) {
		t.Errorf("the explain dump does not carry the project public key: %s", b)
	}
}

func TestResolveProjectsWithoutDeclaration(t *testing.T) {
	path := writeConfig(t, "state_root = \"/tmp/no-projects\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Config.Projects) != 0 {
		t.Fatalf("no declaration parsed into %d projects", len(res.Config.Projects))
	}
	for _, cv := range res.Values {
		if cv.Key == "projects" {
			if s, _ := cv.Value.(string); s != "none declared" {
				t.Errorf("projects row = %q, want \"none declared\"", s)
			}
		}
	}
	got, err := res.Config.ProjectsSet()
	if err != nil || len(got) != 0 {
		t.Fatalf("ProjectsSet with no declaration = (%v, %v), want an empty set and no error", got, err)
	}
}

// TestResolveProjectsUnknownKeyInsideTable is the fail-loud half of §3.1a: a
// typo inside a project table is refused by name, never silently ignored.
func TestResolveProjectsUnknownKeyInsideTable(t *testing.T) {
	path := writeConfig(t, `state_root = "/tmp/projects-typo"

[[projects]]
id = "1"
public_key = "`+projKeyA+`"
quota = 600
`)
	_, err := Resolve(nil, nil, path)
	if err == nil {
		t.Fatal("expected a refusal for an unknown key inside a project table")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-001") || !strings.Contains(err.Error(), "quota") {
		t.Errorf("error = %v, want TROUBLE-LIFECYCLE-001 naming the unknown key", err)
	}
}

func TestResolveProjectsFromFlagIsRefused(t *testing.T) {
	dir := t.TempDir()
	_, err := Resolve([]string{"--state_root", dir, "--projects", "1"}, nil, filepath.Join(dir, "missing.toml"))
	if err == nil {
		t.Fatal("expected a refusal: a project set is an array of tables, not a scalar flag")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-001") {
		t.Errorf("error = %v, want TROUBLE-LIFECYCLE-001", err)
	}
}

func TestProjectsSetAppliesDefaultsAndValidates(t *testing.T) {
	base := func(p ProjectConfig) Config {
		return Config{Projects: []ProjectConfig{p}}
	}

	// A minimal declaration is complete.
	got, err := base(ProjectConfig{ID: "3", PublicKey: projKeyA}).ProjectsSet()
	if err != nil {
		t.Fatalf("ProjectsSet: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ProjectsSet returned %d projects, want 1", len(got))
	}
	p := got[0]
	if p.Slug != "3" {
		t.Errorf("slug = %q, want the id", p.Slug)
	}
	if p.QuotaEPM != DefaultProjectQuotaEPM || p.DiskBudget != DefaultProjectDiskBudget {
		t.Errorf("defaults not applied: quota=%d budget=%d", p.QuotaEPM, p.DiskBudget)
	}
	if !p.Enabled {
		t.Errorf("declared project is not enabled: %+v", p)
	}

	bad := []struct {
		name string
		in   []ProjectConfig
		want string
	}{
		{"missing id", []ProjectConfig{{PublicKey: projKeyA}}, "id"},
		{"non-numeric id", []ProjectConfig{{ID: "abc", PublicKey: projKeyA}}, "numeric"},
		{"zero id", []ProjectConfig{{ID: "0", PublicKey: projKeyA}}, "numeric"},
		{"id above the DSN ceiling", []ProjectConfig{{ID: "2147483648", PublicKey: projKeyA}}, "numeric"},
		{"short public key", []ProjectConfig{{ID: "1", PublicKey: "ae86"}}, "32 lowercase hex"},
		{"uppercase public key", []ProjectConfig{{ID: "1", PublicKey: strings.ToUpper(projKeyA)}}, "32 lowercase hex"},
		{"short secret", []ProjectConfig{{ID: "1", PublicKey: projKeyA, Secret: "00112233"}}, "secret"},
		{"duplicate id", []ProjectConfig{{ID: "1", PublicKey: projKeyA}, {ID: "1", PublicKey: projKeyB}}, "duplicate id"},
		{"duplicate key", []ProjectConfig{{ID: "1", PublicKey: projKeyA}, {ID: "2", PublicKey: projKeyA}}, "duplicate public_key"},
		{"negative quota", []ProjectConfig{{ID: "1", PublicKey: projKeyA, QuotaEPM: -1}}, "quota_epm"},
		{"negative budget", []ProjectConfig{{ID: "1", PublicKey: projKeyA, DiskBudgetBytes: -1}}, "disk_budget_bytes"},
		{"unknown loss policy", []ProjectConfig{{ID: "1", PublicKey: projKeyA, LossPolicy: "keep-everything"}}, "loss_policy"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := (Config{Projects: c.in}).ProjectsSet()
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want code 001 naming %q", err, c.want)
			}
		})
	}
}

// TestShippedExampleDeclaresAProject gates the example the README tells a new
// operator to copy: it must parse AND yield a usable project set, or the
// documented on-ramp boots an instance with no ingest plane.
func TestShippedExampleDeclaresAProject(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "config.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the shipped example is missing at %s: %v", path, err)
	}
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("the shipped example does not resolve: %v", err)
	}
	projects, err := res.Config.ProjectsSet()
	if err != nil {
		t.Fatalf("the shipped example declares a malformed project: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("the shipped example declares %d projects, want 1", len(projects))
	}
	if projects[0].ID != "1" || projects[0].QuotaEPM != DefaultProjectQuotaEPM {
		t.Errorf("shipped example project = %+v", projects[0])
	}
	if !res.Config.Ingest.Auth.LoopbackDSN {
		t.Errorf("the shipped example must keep the documented loopback DSN form on: %+v", res.Config.Ingest.Auth)
	}
	// The shipped advertised_host must be a name the sentinel accepts: an IP
	// literal, a bind address or "localhost" is refused at config load
	// (SPEC-04 §2.2), which would leave the shipped boot without an ingest plane.
	if !validAdvertisedName(res.Config.Ingest.AdvertisedHost) {
		t.Errorf("examples/config.toml advertises %q, which the sentinel refuses as a DSN host",
			res.Config.Ingest.AdvertisedHost)
	}
}

// validAdvertisedName mirrors the sentinel's refusal of an IP literal, a
// wildcard bind address or "localhost" as a DSN host.
func validAdvertisedName(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" || h == "localhost" {
		return false
	}
	if strings.Trim(h, "0123456789.") == "" { // py-style IPv4 shape
		return false
	}
	return !strings.Contains(h, ":") // IPv6
}
