package portability

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This test is the regression guard for the TRBL-002 build breakage: the daemon
// core used Linux-only syscalls (inotify, statfs(2), fdatasync(2), flock(2),
// syscall.Stat_t) in files with NO build constraint, so
// CGO_ENABLED=0 GOOS=windows/darwin go build failed with undefined symbols.
//
// A cross-compile in a unit test would be the strongest check but is not cheap,
// so this is a source invariant instead: any file that touches the Linux-only
// surface must carry a build constraint (a //go:build line or an OS/arch file
// suffix). It is hermetic (walks the tree, no network, no toolchain) and it
// fails on the exact defect class the row measured.

// linuxOnlyToken is one Linux-only (or unix-only) symbol and the constraint a
// file using it needs.
type linuxOnlyPattern struct {
	token  string
	reason string
}

var linuxOnlyPatterns = []linuxOnlyPattern{
	{`golang.org/x/sys/unix"`, "package golang.org/x/sys/unix does not exist for windows (and is a different surface on darwin)"},
	{"unix.IN_CREATE", "inotify(7) is Linux-only"},
	{"unix.InotifyInit1", "inotify(7) is Linux-only"},
	{"unix.Statfs_t", "statfs(2) is not in the windows syscall package"},
	{"unix.ST_RDONLY", "ST_RDONLY is a Linux statfs flag"},
	{"syscall.Fdatasync", "fdatasync(2) has no windows or darwin binding in package syscall"},
	{"syscall.Flock", "flock(2) does not exist on windows"},
	{"syscall.Stat_t", "syscall.Stat_t does not exist on windows"},
	{"syscall.Statfs_t", "syscall.Statfs_t does not exist on windows"},
	{"Setpgid:", "syscall.SysProcAttr has no Setpgid field on windows"},
}

// osSuffixes are the file-name suffixes that act as build constraints.
var osSuffixes = []string{
	"_linux", "_darwin", "_windows", "_freebsd", "_netbsd", "_openbsd",
	"_unix", "_bsd", "_amd64", "_arm64", "_386", "_arm",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the repo root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// constrained reports whether a file is excluded from some platforms: either an
// explicit //go:build / // +build line, or an OS/arch file-name suffix.
func constrained(name string, src []byte) bool {
	for _, line := range strings.Split(head(src, 2048), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//go:build ") || strings.HasPrefix(trimmed, "// +build ") {
			return true
		}
	}
	base := strings.TrimSuffix(name, ".go")
	for _, suf := range osSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

func head(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return string(b[:n])
}

// offenders returns the (path, token) pairs that violate the invariant.
func offenders(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // .git, .worktrees, .coding-hermes
			}
			switch d.Name() {
			case "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if constrained(d.Name(), src) {
			return nil
		}
		text := string(src)
		for _, p := range linuxOnlyPatterns {
			if strings.Contains(text, p.token) {
				rel, _ := filepath.Rel(root, path)
				out = append(out, rel+": uses "+p.token+" ("+p.reason+") with no build constraint")
			}
		}
		return nil
	})
	return out, err
}

// TestLinuxOnlyFilesAreBuildConstrained is the guard. It must FAIL on a tree
// where, say, internal/sensors/inotify.go uses unix.IN_CREATE unconstrained.
func TestLinuxOnlyFilesAreBuildConstrained(t *testing.T) {
	root := repoRoot(t)
	found, err := offenders(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, f := range found {
		t.Errorf("unconstrained Linux-only symbol: %s\n"+
			"fix: rename the file to <name>_linux.go, or add a //go:build line "+
			"(and a non-Linux counterpart so the package still compiles there)", f)
	}
}

// TestScannerIsNotVacuous proves the guard can fail: it must flag a synthetic
// unconstrained file that touches the Linux-only surface, and must NOT flag the
// same content once a //go:build line is present. Without this, a pattern list
// that silently stopped matching would leave the guard green forever.
func TestScannerIsNotVacuous(t *testing.T) {
	dir := t.TempDir()
	body := "package sensors\n\nvar bits = map[string]uint32{\"create\": unix.IN_CREATE}\n"
	if err := os.WriteFile(filepath.Join(dir, "inotify.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := offenders(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("scanner found nothing in an unconstrained file using unix.IN_CREATE: the guard is vacuous")
	}
	// Same content WITH an explicit constraint: now clean.
	if err := os.WriteFile(filepath.Join(dir, "inotify.go"), []byte("//go:build linux\n\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = offenders(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("scanner still flags an explicitly constrained file: %v", got)
	}
	// And the file-suffix form must count as a constraint too.
	if err := os.WriteFile(filepath.Join(dir, "inotify_linux.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = offenders(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("scanner flags a _linux.go file: %v", got)
	}
}
