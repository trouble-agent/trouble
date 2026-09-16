package registry

// donottouch_test.go — do-not-touch per SPEC-06 §3.3 (the scope vocabulary and
// the deny-by-default shape), §3.6 (the file format, the compiled-in floor, the
// additive merge, the weakening refusal, path resolution and `/ **` matching,
// unit normalization, the enforcement point at step a3, the static play check)
// and the §7 row for this file:
//
//	floor + additive merge, weakening refusal, symlink escape, `/**` prefix match,
//	unit normalization, static play-target check | 24 fixture paths/units; every
//	refusal policy_refused; the floor never shrinks.
//
// The fixture count is met by driving all 18 compiled-in floor paths and all 10
// floor units through the real pipeline: TestDoNotTouchFloorPathsAreRefused and
// TestDoNotTouchFloorUnitsAreRefused are 28 fixture cases.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The floor as SPEC-06 §3.6 spells it. It is restated here rather than derived
// from floorPaths on purpose: a test that re-derives the floor from the same
// variable cannot notice the floor shrinking.
var (
	documentedFloorPaths = []string{
		"/etc/shadow", "/etc/gshadow", "/etc/passwd", "/etc/sudoers", "/etc/sudoers.d/**",
		"/etc/ssh/**", "/etc/polkit-1/**", "/etc/trouble/do-not-touch.toml", "/etc/fstab",
		"/boot/**", "/usr/**", "/lib/**", "/lib64/**", "/sbin/**", "/bin/**",
		"/var/lib/trouble/**", "<state_root>/**", "<state_root>/backups/**",
	}
	documentedFloorUnits = []string{
		"init.scope", "systemd-journald.service", "systemd-logind.service", "dbus.service",
		"polkit.service", "ssh.service", "sshd.service", "trouble.service", "trouble-escalate@.service",
		"systemd-oomd.service",
	}
	documentedFloorScopes = []string{"service:stop", "config:delete", "file:exec"}
)

// TestFloorIsExactlyTheDocumentedSet pins §3.6 rule 1: the compiled-in floor is
// the documented list, with <state_root> resolved at boot.
func TestFloorIsExactlyTheDocumentedSet(t *testing.T) {
	const root = "/state-root-fixture"
	floor := FloorDoNotTouch(root)

	resolve := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			out = append(out, strings.ReplaceAll(p, "<state_root>", root))
		}
		return out
	}
	assertStrings(t, "paths", floor.Paths, resolve(documentedFloorPaths))
	assertStrings(t, "units", floor.Units, documentedFloorUnits)
	assertStrings(t, "scopes", floor.Scopes, documentedFloorScopes)

	// The §7 threshold counts 24 fixtures; the floor alone is 28.
	if n := len(floor.Paths) + len(floor.Units); n < 24 {
		t.Fatalf("the floor carries %d path/unit fixtures, the §7 threshold is 24", n)
	}
}

// TestFloorNeverShrinks pins the other half of §3.6 rule 1: whatever the file and
// the *_extra keys say, the merged set is a superset of the floor.
func TestFloorNeverShrinks(t *testing.T) {
	floor := FloorDoNotTouch("/state-root-fixture")

	t.Run("merge-of-nothing-keeps-the-floor", func(t *testing.T) {
		got := mergeDoNotTouch(floor, types.DoNotTouch{}, nil, nil, nil)
		assertContainsAll(t, "paths", got.Paths, floor.Paths)
		assertContainsAll(t, "units", got.Units, floor.Units)
		assertContainsAll(t, "scopes", got.Scopes, floor.Scopes)
	})

	t.Run("a-widening-file-and-extra-keys-only-add", func(t *testing.T) {
		file := types.DoNotTouch{
			Paths:  []string{"/srv/from-file/**"},
			Units:  []string{"from-file.service"},
			Scopes: []string{"from-file:scope"},
		}
		got := mergeDoNotTouch(floor, file, []string{"/srv/from-config/**"}, []string{"from-config.service"}, []string{"from-config:scope"})
		assertContainsAll(t, "paths", got.Paths, floor.Paths)
		assertContainsAll(t, "paths", got.Paths, file.Paths)
		assertContainsAll(t, "paths", got.Paths, []string{"/srv/from-config/**"})
		assertContainsAll(t, "units", got.Units, floor.Units)
		assertContainsAll(t, "units", got.Units, []string{"from-file.service", "from-config.service"})
		assertContainsAll(t, "scopes", got.Scopes, floor.Scopes)
		assertContainsAll(t, "scopes", got.Scopes, []string{"from-file:scope", "from-config:scope"})
	})

	// The order is floor → file → config extras, so a reviewer can read the
	// resolved set top-down (SPEC-06 §3.6 rule 3).
	t.Run("the-merge-order-is-floor-file-config", func(t *testing.T) {
		got := mergeDoNotTouch(
			types.DoNotTouch{Paths: []string{"floor"}},
			types.DoNotTouch{Paths: []string{"file"}},
			[]string{"config"}, nil, nil)
		assertStrings(t, "paths", got.Paths, []string{"floor", "file", "config"})
	})

	t.Run("a-live-registry-keeps-the-floor-after-a-weakening-file", func(t *testing.T) {
		root := t.TempDir()
		dntFile := writeTempFile(t, root, "do-not-touch.toml", "schema_version = 1\nmandatory = false\n")
		r, spy, log := newRegTestRegistry(t, regTestOpts{
			Config: cfgValues("registry.do_not_touch_file", dntFile),
		})
		assertContainsAll(t, "paths", r.dnt.Paths, FloorDoNotTouch(r.cfg.StateRoot).Paths)
		assertContainsAll(t, "units", r.dnt.Units, FloorDoNotTouch(r.cfg.StateRoot).Units)

		tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
			map[string]any{"path": "/etc/shadow"}, nil))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
	})
}

// TestAdditiveMergeWidensTheDenySet covers §3.6 rules 2 and 3 wired end to end:
// a do-not-touch file and the *_extra keys can widen the set, and the widened
// entries refuse at a3 exactly like a floor entry.
func TestAdditiveMergeWidensTheDenySet(t *testing.T) {
	root := t.TempDir()
	extra := filepath.Join(root, "ops")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	fromFile := filepath.Join(root, "srv")
	if err := os.MkdirAll(fromFile, 0o755); err != nil {
		t.Fatal(err)
	}
	dntFile := writeTempFile(t, root, "do-not-touch.toml",
		"schema_version = 1\npaths = [\""+fromFile+"/**\"]\nunits = [\"from-file.service\"]\nscopes = [\"config:rotate\"]\n")

	r, spy, log := newRegTestRegistry(t, regTestOpts{
		Config: append(
			cfgValues("registry.do_not_touch_file", dntFile),
			cfgValues("registry.do_not_touch_paths_extra", []string{extra + "/**"})...),
	})

	// The file's manifest is loaded: json → the file's own entries are present.
	fileDNT, err := LoadDoNotTouch(dntFile)
	if err != nil {
		t.Fatalf("a widening file must load: %v", err)
	}
	assertContainsAll(t, "paths", fileDNT.Paths, []string{fromFile + "/**"})
	if len(fileDNT.Units) != 1 || fileDNT.Units[0] != "from-file.service" {
		t.Fatalf("file units = %v", fileDNT.Units)
	}
	if len(fileDNT.Scopes) != 1 || fileDNT.Scopes[0] != "config:rotate" {
		t.Fatalf("file scopes = %v", fileDNT.Scopes)
	}

	cases := []struct {
		name string
		req  types.ToolCallRequest
	}{
		{"extra-path", testRequest("file.read", types.ModeCheck, map[string]any{"path": extra + "/notes.txt"}, nil)},
		{"file-path", testRequest("file.read", types.ModeCheck, map[string]any{"path": fromFile + "/notes.txt"}, nil)},
		{"file-unit", testRequest("service.status", types.ModeCheck, map[string]any{"unit": "from-file.service"}, nil)},
		{"floor-path-still-refused", testRequest("file.read", types.ModeCheck, map[string]any{"path": "/etc/shadow"}, nil)},
		{"floor-unit-still-refused", testRequest("service.status", types.ModeCheck, map[string]any{"unit": "sshd.service"}, nil)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			spy.reset()
			log.reset()
			tc, err := r.Call(context.Background(), c.req)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, "")
		})
	}
}

// TestWeakeningFileIsRefusedAndWarned is §3.6 rule 2 / §6.6: a file that tries to
// narrow the deny set is refused, its keys are rejected, the floor stands, and the
// daemon continues with the refusal recorded.
func TestWeakeningFileIsRefusedAndWarned(t *testing.T) {
	files := map[string]string{
		"mandatory-false": "schema_version = 1\nmandatory = false\npaths = [\"/srv/ignored/**\"]\n",
		"remove-entry":    "schema_version = 1\npaths = [\"/srv/ignored/**\"]\nremove = [\"/etc/shadow\", \"/etc/ssh/**\"]\n",
	}
	for name, body := range files {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeTempFile(t, root, "do-not-touch.toml", body)

			// LoadDoNotTouch reports the refusal as a recorded warning and hands
			// back no narrowing.
			dnt, err := LoadDoNotTouch(path)
			if err == nil {
				t.Fatalf("a weakening file must be refused")
			}
			var w *bootWarning
			if !errors.As(err, &w) {
				t.Fatalf("warning type = %T, want *bootWarning", err)
			}
			if w.Code != types.CodeRegistry006 {
				t.Fatalf("warning code = %s, want %s", w.Code, types.CodeRegistry006)
			}
			if w.Reason != reasonDoNotTouchWeaken {
				t.Fatalf("warning reason = %q, want %q", w.Reason, reasonDoNotTouchWeaken)
			}
			if len(dnt.Paths) != 0 || len(dnt.Units) != 0 || len(dnt.Scopes) != 0 {
				t.Fatalf("a refused file must contribute nothing, got %+v", dnt)
			}

			// The same file at boot: the daemon starts, records the refusal and
			// keeps maximum enforcement.
			r, spy, log := newRegTestRegistry(t, regTestOpts{
				Config: cfgValues("registry.do_not_touch_file", path),
			})
			warns := r.Warnings()
			if len(warns) != 1 {
				t.Fatalf("boot warnings = %d (%v), want 1", len(warns), warns)
			}
			if warns[0].Reason != reasonDoNotTouchWeaken || warns[0].Code != types.CodeRegistry006 {
				t.Fatalf("boot warning = %+v", warns[0])
			}
			assertContainsAll(t, "paths", r.dnt.Paths, FloorDoNotTouch(r.cfg.StateRoot).Paths)

			tc, cerr := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
				map[string]any{"path": "/etc/shadow"}, nil))
			if cerr != nil {
				t.Fatalf("call: %v", cerr)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
		})
	}
}

// TestProtectedPathsAreRefused is the §7 fixture sweep for paths: all 18 compiled
// floor paths refuse at a3 as policy_refused, and the near-miss controls are not
// swept up by the patterns.
func TestProtectedPathsAreRefused(t *testing.T) {
	if got := types.CodeClass[types.CodeRegistry007]; got != types.ErrClassPolicyRefused {
		t.Fatalf("TROUBLE-REGISTRY-007 class = %q, want %q (SPEC-06 §5)", got, types.ErrClassPolicyRefused)
	}
	r, spy, log := newRegTestRegistry(t, regTestOpts{})
	floor := FloorDoNotTouch(r.cfg.StateRoot)
	if len(floor.Paths) < 18 {
		t.Fatalf("floor paths = %d, the documented floor is 18", len(floor.Paths))
	}
	for _, p := range floor.Paths {
		p := p
		t.Run("floor:"+p, func(t *testing.T) {
			spy.reset()
			log.reset()
			tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
				map[string]any{"path": p}, nil))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
		})
	}

	// Near-miss controls: siblings of a `/**` pattern are not protected
	// (SPEC-06 §3.6 matching), so a3 must not refuse them.
	for _, p := range []string{"/etc/sshrc", "/etc/sudoers.dfoo", "/etc/ssh_config", "/var/lib/troubled/notes.txt", "/etc/hostname"} {
		mod, ok := r.module("file.read")
		if !ok {
			t.Fatalf("file.read is not registered")
		}
		if e := r.checkProtected(mod, map[string]any{"path": p}); e != nil {
			t.Fatalf("a3 refused the unprotected %s: %v", p, e)
		}
	}
}

// TestProtectedUnitsAreRefused is the §7 fixture sweep for units: all 10 compiled
// floor units refuse at a3, including the template instance of a template unit.
func TestProtectedUnitsAreRefused(t *testing.T) {
	r, spy, log := newRegTestRegistry(t, regTestOpts{})
	floor := FloorDoNotTouch(r.cfg.StateRoot)

	if len(floor.Units) < 10 {
		t.Fatalf("floor units = %d, the documented floor is 10", len(floor.Units))
	}
	for _, u := range floor.Units {
		u := u
		t.Run("floor:"+u, func(t *testing.T) {
			spy.reset()
			log.reset()
			tc, err := r.Call(context.Background(), testRequest("service.status", types.ModeCheck,
				map[string]any{"unit": u}, nil))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchUnit)
		})
	}
}

// TestSymlinkEscapeIsPolicyRefused is §3.6 matching and §6.7: every candidate is
// absolute → cleaned → EvalSymlinks-resolved before it is matched, so a symlink
// (or a symlinked directory) cannot be used to reach a protected file, and a
// symlink that leaves file.allow_roots is the distinct allow_root_escape refusal.
func TestSymlinkEscapeIsPolicyRefused(t *testing.T) {
	r, spy, log := newRegTestRegistry(t, regTestOpts{})

	t.Run("symlink-to-a-protected-file", func(t *testing.T) {
		spy.reset()
		log.reset()
		link := filepath.Join(t.TempDir(), "innocent.conf")
		if err := os.Symlink("/etc/shadow", link); err != nil {
			t.Skipf("cannot create the symlink fixture: %v", err)
		}
		tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
			map[string]any{"path": link}, nil))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
	})

	t.Run("symlinked-directory-to-a-protected-tree", func(t *testing.T) {
		spy.reset()
		log.reset()
		dir := t.TempDir()
		if err := os.Symlink("/etc", filepath.Join(dir, "etc-link")); err != nil {
			t.Skipf("cannot create the symlink fixture: %v", err)
		}
		tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
			map[string]any{"path": filepath.Join(dir, "etc-link", "shadow")}, nil))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
	})

	t.Run("symlink-out-of-the-allow-root", func(t *testing.T) {
		allowed := t.TempDir()
		outside := t.TempDir()
		real := writeTempFile(t, outside, "app.conf", ladderFixture)
		link := filepath.Join(allowed, "app.conf")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("cannot create the symlink fixture: %v", err)
		}
		r2, spy2, log2 := newRegTestRegistry(t, regTestOpts{Config: allowRootsConfig(allowed)})
		tc, err := r2.Call(context.Background(), testRequest("file.patch", types.ModeApply,
			map[string]any{"path": link, "patch": ladderPatch}, []string{"file.patch"}))
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		assertRefused(t, tc, spy2, log2, types.CodeRegistry006, reasonAllowRootEscape)
	})
}

// TestDoubleStarPrefixMatch pins the single extension §3.6 makes to Go's
// path.Match: a trailing "/**" matches the prefix itself and everything under it.
func TestDoubleStarPrefixMatch(t *testing.T) {
	cases := []struct {
		pattern, candidate string
		want               bool
	}{
		// the prefix itself...
		{"/etc/ssh/**", "/etc/ssh", true},
		{"/etc/sudoers.d/**", "/etc/sudoers.d", true},
		{"/usr/**", "/usr", true},
		// ...and everything under it
		{"/etc/ssh/**", "/etc/ssh/ssh_config", true},
		{"/etc/ssh/**", "/etc/ssh/ssh_config.d/00.conf", true},
		{"/etc/sudoers.d/**", "/etc/sudoers.d/admin", true},
		// but never a sibling whose name merely shares the prefix
		{"/etc/ssh/**", "/etc/sshrc", false},
		{"/etc/ssh/**", "/etc/ssh_config", false},
		{"/etc/sudoers.d/**", "/etc/sudoers", false},
		{"/usr/**", "/usrlocal/x", false},
		// a pattern without /** is an exact match
		{"/etc/shadow", "/etc/shadow", true},
		{"/etc/shadow", "/etc/shadow2", false},
		{"/etc/shadow", "/etc/shadow/x", false},
		// path.Match semantics survive for a glob pattern
		{"/etc/*.conf", "/etc/foo.conf", true},
		{"/etc/*.conf", "/etc/sub/foo.conf", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.candidate); got != c.want {
			t.Errorf("matchPath(%q, %q) = %t, want %t", c.pattern, c.candidate, got, c.want)
		}
	}

	// The same extension, live: the prefix and a child refuse, the sibling does
	// not.
	r, spy, log := newRegTestRegistry(t, regTestOpts{})
	for _, p := range []string{"/etc/ssh", "/etc/ssh/ssh_config"} {
		p := p
		t.Run("protected:"+p, func(t *testing.T) {
			spy.reset()
			log.reset()
			tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
				map[string]any{"path": p}, nil))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
		})
	}
	mod, _ := r.module("file.read")
	if e := r.checkProtected(mod, map[string]any{"path": "/etc/sshrc"}); e != nil {
		t.Fatalf("the sibling prefix /etc/sshrc must not be refused by /etc/ssh/**: %v", e)
	}
}

// TestUnitNormalization pins §3.6's systemd normalization and its limits.
func TestUnitNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sshd", "sshd.service"},
		{"sshd.service", "sshd.service"},
		{"  sshd  ", "sshd.service"},
		{"", ""},
		{"init.scope", "init.scope"},
		{"systemd-journald.service", "systemd-journald.service"},
		// A template instance is compared by its template name.
		{"trouble-escalate@1.service", "trouble-escalate@.service"},
		{"trouble-escalate@.service", "trouble-escalate@.service"},
		// An instance of a name that is not a template keeps its own, empty
		// template: `ssh.service@foo` is NOT `ssh.service` — that is the
		// documented limit of the rule, and a4's allowlist is the gate for a
		// name of that shape.
		{"ssh.service@foo", "ssh.service@"},
	}
	for _, c := range cases {
		if got := normalizeUnit(c.in); got != c.want {
			t.Errorf("normalizeUnit(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	r, spy, log := newRegTestRegistry(t, regTestOpts{})
	refused := []string{"sshd", "ssh", "init.scope", "trouble-escalate@1.service"}
	for _, u := range refused {
		u := u
		t.Run("refused:"+u, func(t *testing.T) {
			spy.reset()
			log.reset()
			tc, err := r.Call(context.Background(), testRequest("service.status", types.ModeCheck,
				map[string]any{"unit": u}, nil))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchUnit)
		})
	}
	mod, _ := r.module("service.status")
	for _, u := range []string{"payment-worker.service", "ssh.service@foo"} {
		if e := r.checkProtected(mod, map[string]any{"unit": u}); e != nil {
			t.Fatalf("a3 refused the unprotected unit %s: %v", u, e)
		}
	}
}

// TestScopeDenyListRefusesOutright covers §3.3's scope deny list. No shipped
// module declares a scope-shaped target, so the check is exercised through a
// test-only module that implements the package-private targetDeclarer extension.
func TestScopeDenyListRefusesOutright(t *testing.T) {
	assertStrings(t, "floor scopes", FloorDoNotTouch("/x").Scopes, documentedFloorScopes)

	r, spy, log := newRegTestRegistry(t, regTestOpts{Extra: []types.Module{scopeTargetModule{}}})

	tc, err := r.Call(context.Background(), testRequest("test.scope_target", types.ModeCheck,
		map[string]any{"scope_token": "service:stop"}, nil))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchScope)

	// A scope token outside the deny list is not refused by a3.
	spy.reset()
	log.reset()
	ok, err := r.Call(context.Background(), testRequest("test.scope_target", types.ModeCheck,
		map[string]any{"scope_token": "service:start"}, nil))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !ok.Stage[0].OK {
		t.Fatalf("a token outside the deny list must clear a3: %s", stageDetails(ok))
	}
	if n := len(spy.snapshot()); n != 1 {
		t.Fatalf("an allowed check_mode call appends one record, got %d", n)
	}
}

// TestModuleWithoutTargetDeclarerStillHitsTheFloor is §2.1: the
// targetDeclarer extension is optional, and a module that omits it falls back to
// the args key scan so it cannot silently opt out of do-not-touch by omission.
func TestModuleWithoutTargetDeclarerStillHitsTheFloor(t *testing.T) {
	r, spy, log := newRegTestRegistry(t, regTestOpts{Extra: []types.Module{plainTargetModule{}}})

	tc, err := r.Call(context.Background(), testRequest("test.plain_target", types.ModeCheck,
		map[string]any{"file": "/etc/shadow"}, nil))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertRefused(t, tc, spy, log, types.CodeRegistry007, reasonDoNotTouchPath)
}

// TestLoadPlayRefusesProtectedTargets is §3.6's static check: a play whose literal
// args name a protected path or unit never loads — a protected play cannot reach
// the runner.
func TestLoadPlayRefusesProtectedTargets(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()

	play := func(body string) string {
		return writeTempFile(t, root, "play-"+randomSuffix()[:8]+".toml", body)
	}

	t.Run("protected-path", func(t *testing.T) {
		path := play(`schema_version = 1
name = "protected-path"
version = 1

[[task]]
name  = "patch"
tool  = "file.patch"
args  = { path = "/etc/shadow", patch = "@@ -1,1 +1,1 @@\n-a\n+b\n" }
`)
		_, err := LoadPlay(path)
		if err == nil {
			t.Fatalf("a play naming a protected path must not load")
		}
		if CodeOf(err) != types.CodeRegistry007 {
			t.Fatalf("load code = %s, want %s (%v)", CodeOf(err), types.CodeRegistry007, err)
		}
		if ClassOf(err) != types.ErrClassPolicyRefused {
			t.Fatalf("load class = %q, want %q", ClassOf(err), types.ErrClassPolicyRefused)
		}
		if ReasonOf(err) != reasonDoNotTouchPath {
			t.Fatalf("load reason = %q, want %q", ReasonOf(err), reasonDoNotTouchPath)
		}
	})

	t.Run("protected-unit", func(t *testing.T) {
		path := play(`schema_version = 1
name = "protected-unit"
version = 1

[[task]]
name  = "reload"
tool  = "service.reload"
args  = { unit = "sshd.service" }
`)
		_, err := LoadPlay(path)
		if err == nil {
			t.Fatalf("a play naming a protected unit must not load")
		}
		if CodeOf(err) != types.CodeRegistry007 {
			t.Fatalf("load code = %s, want %s", CodeOf(err), types.CodeRegistry007)
		}
		if ClassOf(err) != types.ErrClassPolicyRefused {
			t.Fatalf("load class = %q, want %q", ClassOf(err), types.ErrClassPolicyRefused)
		}
		if ReasonOf(err) != reasonDoNotTouchUnit {
			t.Fatalf("load reason = %q, want %q", ReasonOf(err), reasonDoNotTouchUnit)
		}
	})

	t.Run("state-root-is-resolved-before-load", func(t *testing.T) {
		path := play(`schema_version = 1
name = "state-root"
version = 1

[[task]]
name  = "patch"
tool  = "file.patch"
args  = { path = "` + defaultStateRoot() + `/plays/thing.toml", patch = "@@ -1,1 +1,1 @@\n-a\n+b\n" }
`)
		_, err := LoadPlay(path)
		if err == nil {
			t.Fatalf("<state_root>/** must be enforced at load too")
		}
		if CodeOf(err) != types.CodeRegistry007 {
			t.Fatalf("load code = %s, want %s", CodeOf(err), types.CodeRegistry007)
		}
	})

	t.Run("an-unprotected-play-loads", func(t *testing.T) {
		conf := writeTempFile(t, root, "app.conf", ladderFixture)
		path := play(`schema_version = 1
name = "unprotected"
version = 3

[[task]]
name  = "patch"
tool  = "file.patch"
args  = { path = "` + conf + `", patch = "@@ -1,2 +1,2 @@\n listen = 8080\n-workers = 4\n+workers = 8\n" }
`)
		p, err := LoadPlay(path)
		if err != nil {
			t.Fatalf("an unprotected play must load: %v", err)
		}
		if p.Name != "unprotected" || p.Version != 3 {
			t.Fatalf("play = %+v", p)
		}
		// §3.7 defaults: max_runs 2, retries 0, on_fail abort, source
		// module-default, when/register empty; CheckMode is derived.
		if p.MaxRuns != 2 || p.Source != "module-default" || !p.CheckMode {
			t.Fatalf("defaults = %+v", p)
		}
		if len(p.Tasks) != 1 || p.Tasks[0].Retries != 0 || p.Tasks[0].OnFail != "abort" {
			t.Fatalf("task defaults = %+v", p.Tasks)
		}
	})
}

// ---------------------------------------------------------------------------
// test-only modules (registered after NewWith: they carry no committed schema)
// ---------------------------------------------------------------------------

type scopeTargetArgs struct {
	ScopeToken string `json:"scope_token,omitempty"`
}

type scopeTargetModule struct{}

var scopeTargetDescriptor = types.Descriptor{
	Name:        "test.scope_target",
	Version:     1,
	Schema:      schemagen.Generate("test.scope_target", 1, scopeTargetArgs{}),
	Scopes:      []string{"test:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
}

func (scopeTargetModule) Descriptor() types.Descriptor { return scopeTargetDescriptor }

func (scopeTargetModule) ProtectedTargets(args map[string]any) []protectedTarget {
	if s, ok := args["scope_token"].(string); ok && s != "" {
		return []protectedTarget{{Kind: "scope", Value: s}}
	}
	return nil
}

func (scopeTargetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{Empty: true, Summary: "scope target probe"}, nil
}

func (scopeTargetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{Changed: false, Output: map[string]any{}}, nil
}

func (scopeTargetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

type plainTargetArgs struct {
	File string `json:"file,omitempty"`
}

// plainTargetModule deliberately implements no ProtectedTargets: the args key
// scan must still find its `file` argument.
type plainTargetModule struct{}

var plainTargetDescriptor = types.Descriptor{
	Name:        "test.plain_target",
	Version:     1,
	Schema:      schemagen.Generate("test.plain_target", 1, plainTargetArgs{}),
	Scopes:      []string{"test:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
}

func (plainTargetModule) Descriptor() types.Descriptor { return plainTargetDescriptor }

func (plainTargetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{Empty: true, Summary: "plain target probe"}, nil
}

func (plainTargetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{Changed: false, Output: map[string]any{}}, nil
}

func (plainTargetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

// ---------------------------------------------------------------------------
// small assertions
// ---------------------------------------------------------------------------

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v (%d entries), want %v (%d entries)", what, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q (full set: %v)", what, i, got[i], want[i], got)
		}
	}
}

func assertContainsAll(t *testing.T, what string, got, want []string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is missing %q (set: %v)", what, w, got)
		}
	}
}

// randomSuffix keeps the play fixture filenames distinct without a second
// dependency.
func randomSuffix() string {
	return types.NewID(types.PEv)
}
