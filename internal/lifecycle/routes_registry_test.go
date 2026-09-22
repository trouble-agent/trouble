package lifecycle

// routes_registry_test.go — AC-28 / TRBL-061: the [sentinel.routes] surface is
// a registered key surface (SPEC-12 §3.1), exactly the shape TRBL-036 proved
// for the sensors plane.
//
// What this file proves, and what each level can prove that the others cannot:
//
//	defaults   — no [sentinel.routes] table resolves to the documented posture:
//	             default empty (= auto at the consumer), PerClass empty;
//	file       — a `[sentinel.routes]` table with the spec's own inline-table
//	             example resolves into the typed map;
//	precedence — flag > env > file for `sentinel.routes.default`, with the
//	             winning source/source_ref on each row;
//	scalar     — per_class accepts the scalar spelling a flag or an env var can
//	             carry (`prefix=route,prefix=route`);
//	explain    — both keys appear exactly once in the dump, unredacted, and
//	             the --key filter returns exactly the asked row;
//	refusals   — a per_class value that is not a table is refused at
//	             resolution with TROUBLE-LIFECYCLE-001 naming the key (the
//	             VALUE vocabulary is the sentinel's 023 boot refusal — the
//	             same resolve-here/judge-there split the sensors plane uses).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func writeRoutesConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestRoutesDefaultsAreTheDocumentedPosture: no [sentinel.routes] table runs
// auto with no overrides — the consumer, not the registry, spells auto.
func TestRoutesDefaultsAreTheDocumentedPosture(t *testing.T) {
	r, err := Resolve(nil, nil, emptyConfigPath(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Config.Routes.Default != "" {
		t.Errorf("routes.default = %q, want empty (auto is the consumer's spelling)", r.Config.Routes.Default)
	}
	if len(r.Config.Routes.PerClass) != 0 {
		t.Errorf("routes.per_class = %v, want empty", r.Config.Routes.PerClass)
	}
	for _, cv := range r.Values {
		if cv.Key == "sentinel.routes.default" {
			if cv.Source != "default" || cv.SourceRef != "builtin" {
				t.Errorf("sentinel.routes.default row source = %s/%s, want default/builtin", cv.Source, cv.SourceRef)
			}
		}
	}
}

// TestRoutesFileTableResolves is the spec's own §3.10a example through the
// file: default plus the inline per_class table, typed.
func TestRoutesFileTableResolves(t *testing.T) {
	path := writeRoutesConfig(t, `
[sentinel.routes]
default = "auto"
per_class = { "psi:io_pressure" = "direct", "sentinel:sha256v1" = "proxy" }
`)
	r, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", path, err)
	}
	if r.Config.Routes.Default != "auto" {
		t.Errorf("routes.default = %q, want auto", r.Config.Routes.Default)
	}
	if got := r.Config.Routes.PerClass["psi:io_pressure"]; got != "direct" {
		t.Errorf("per_class[psi:io_pressure] = %q, want direct", got)
	}
	if got := r.Config.Routes.PerClass["sentinel:sha256v1"]; got != "proxy" {
		t.Errorf("per_class[sentinel:sha256v1] = %q, want proxy", got)
	}
	if len(r.Config.Routes.PerClass) != 2 {
		t.Errorf("per_class has %d entries, want 2", len(r.Config.Routes.PerClass))
	}
	for _, cv := range r.Values {
		if cv.Key == "sentinel.routes.per_class" {
			if cv.Source != "file" {
				t.Errorf("per_class row source = %s, want file", cv.Source)
			}
		}
	}
}

// TestRoutesFlagBeatsFile is the ordinary precedence on the new keys: flag >
// env > file, with the winning provenance on the row.
func TestRoutesFlagBeatsFile(t *testing.T) {
	path := writeRoutesConfig(t, `
[sentinel.routes]
default = "direct"
`)
	args := []string{"--sentinel-routes-default", "proxy"}
	env := []string{"TROUBLE_SENTINEL_ROUTES_DEFAULT=satellite-never-valid", "TROUBLE_SENTINEL_ROUTES_PER_CLASS=psi:io_pressure=proxy"}
	r, err := Resolve(args, env, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Config.Routes.Default != "proxy" {
		t.Errorf("routes.default = %q, want proxy (flag wins)", r.Config.Routes.Default)
	}
	for _, cv := range r.Values {
		if cv.Key == "sentinel.routes.default" {
			if cv.Source != "flag" || cv.SourceRef != "--sentinel-routes-default" {
				t.Errorf("default row = %s/%s, want flag/--sentinel-routes-default", cv.Source, cv.SourceRef)
			}
		}
	}
	// env alone beats the file too.
	r2, err := Resolve(nil, env, path)
	if err != nil {
		t.Fatalf("Resolve(env): %v", err)
	}
	if r2.Config.Routes.Default != "satellite-never-valid" {
		t.Errorf("routes.default = %q, want the env value (env beats file; the vocabulary is the sentinel's refusal)", r2.Config.Routes.Default)
	}
	if got := r2.Config.Routes.PerClass["psi:io_pressure"]; got != "proxy" {
		t.Errorf("per_class[psi:io_pressure] = %q, want proxy (scalar env spelling)", got)
	}
}

// TestRoutesExplainCarriesBothKeys is the `trouble config explain` contract:
// both keys exactly once, unredacted (routing policy is not a secret), and
// the --key filter returns exactly the asked row.
func TestRoutesExplainCarriesBothKeys(t *testing.T) {
	r, err := Resolve(nil, nil, emptyConfigPath(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, key := range []string{"sentinel.routes.default", "sentinel.routes.per_class"} {
		rows, err := Explain(r, []string{key})
		if err != nil {
			t.Fatalf("Explain(%s): %v", key, err)
		}
		if len(rows) != 1 {
			t.Fatalf("explain carries %d rows for %s, want exactly 1", len(rows), key)
		}
		if rows[0].Redacted {
			t.Errorf("%s is redacted; routing policy is not a secret", key)
		}
	}
}

// TestRoutesPerClassNonTableRefused: the table's SHAPE is this package's
// refusal (TROUBLE-LIFECYCLE-001 naming the key); the VALUE vocabulary is the
// sentinel's boot refusal (TROUBLE-SENTINEL-023).
func TestRoutesPerClassNonTableRefused(t *testing.T) {
	path := writeRoutesConfig(t, `
[sentinel.routes]
default = "auto"
per_class = 5
`)
	_, err := Resolve(nil, nil, path)
	if err == nil {
		t.Fatal("Resolve accepted per_class = 5")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) {
		t.Errorf("refusal %q does not carry TROUBLE-LIFECYCLE-001", err)
	}
	if !strings.Contains(err.Error(), "sentinel.routes.per_class") {
		t.Errorf("refusal %q does not name the key", err)
	}
}
