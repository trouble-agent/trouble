package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// StateRoot is the validated state root path and ownership facts.
type StateRoot struct {
	Path string
	UID  int
	GID  int
	Mode os.FileMode
}

// CheckStateRoot validates the state root: owned, 0700, local fs, not under a
// forbidden root (SPEC-12 §3.2). Returns TROUBLE-LIFECYCLE-004 or 005.
func CheckStateRoot(cfg Config) (StateRoot, error) {
	root := cfg.StateRoot
	if root == "" {
		return StateRoot{}, fmt.Errorf("%w: state_root is empty", types.CodeLifecycle004)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return StateRoot{}, fmt.Errorf("%w: cannot resolve state_root %q: %v", types.CodeLifecycle004, root, err)
	}
	abs = filepath.Clean(abs)

	for _, forbidden := range cfg.FS.ForbiddenStateRoots {
		if forbidden == "" {
			continue
		}
		fAbs, err := filepath.Abs(forbidden)
		if err != nil {
			continue
		}
		if strings.HasPrefix(abs, filepath.Clean(fAbs)+string(filepath.Separator)) || abs == filepath.Clean(fAbs) {
			return StateRoot{}, fmt.Errorf("%w: state_root %q resolves under forbidden root %q", types.CodeLifecycle004, abs, forbidden)
		}
	}

	// Resolve symlinks and re-check forbidden roots.
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		for _, forbidden := range cfg.FS.ForbiddenStateRoots {
			if forbidden == "" {
				continue
			}
			fAbs, _ := filepath.Abs(forbidden)
			if strings.HasPrefix(resolved, filepath.Clean(fAbs)+string(filepath.Separator)) || resolved == filepath.Clean(fAbs) {
				return StateRoot{}, fmt.Errorf("%w: state_root symlink %q resolves under forbidden root %q", types.CodeLifecycle004, resolved, forbidden)
			}
		}
	}

	info, err := os.Stat(abs)
	if err != nil {
		return StateRoot{}, fmt.Errorf("%w: cannot stat state_root %q: %v", types.CodeLifecycle004, abs, err)
	}
	if !info.IsDir() {
		return StateRoot{}, fmt.Errorf("%w: state_root %q is not a directory", types.CodeLifecycle004, abs)
	}
	if info.Mode().Perm() != 0o700 {
		return StateRoot{}, fmt.Errorf("%w: state_root %q mode is %04o, want 0700", types.CodeLifecycle005, abs, info.Mode().Perm())
	}

	if isRemoteFS(abs, cfg.FS.RemoteTypes) {
		return StateRoot{}, fmt.Errorf("%w: state_root %q is on a remote filesystem", types.CodeLifecycle004, abs)
	}

	stat, ok := info.Sys().(*StatT)
	uid, gid := -1, -1
	if ok {
		uid = int(stat.Uid)
		gid = int(stat.Gid)
	}

	return StateRoot{Path: abs, UID: uid, GID: gid, Mode: info.Mode().Perm()}, nil
}

// CheckSecretFiles walks the declared 0600 set and reports every offender
// (SPEC-12 §3.2). Returns TROUBLE-LIFECYCLE-013.
func CheckSecretFiles(cfg Config) ([]types.ConfigValue, error) {
	var offenders []types.ConfigValue
	paths := secretPaths(cfg)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			// Missing file is not a mode offender.
			continue
		}
		mode := info.Mode().Perm()
		if mode != 0o600 {
			offenders = append(offenders, types.ConfigValue{
				Key:       secretKeyFor(p, cfg),
				Value:     fmt.Sprintf("mode=%04o", mode),
				Source:    "runtime",
				SourceRef: p,
			})
		}
	}
	return offenders, nil
}

func secretPaths(cfg Config) []string {
	var out []string
	out = append(out, cfg.Secrets.EnvironmentFile)
	if containsSecretInFile(cfg.ConfigPath) {
		out = append(out, cfg.ConfigPath)
	}
	out = append(out, filepath.Join(cfg.StateRoot, "heartbeat.json"))
	out = append(out, filepath.Join(cfg.StateRoot, "checker.state.json"))
	out = append(out, filepath.Join(cfg.StateRoot, "checker.alarm"))
	out = append(out, filepath.Join(cfg.StateRoot, "escalate.log"))
	out = append(out, filepath.Join(cfg.StateRoot, "spool"))
	out = append(out, filepath.Join(cfg.StateRoot, "backups"))
	return out
}

func containsSecretInFile(path string) bool {
	// Conservative: if the config path contains a redacted-class key, require 0600.
	// We cannot parse without knowing it; default to true for safety.
	return true
}

func secretKeyFor(path string, cfg Config) string {
	if path == cfg.Secrets.EnvironmentFile {
		return "secrets.environment_file"
	}
	if path == cfg.ConfigPath {
		return "config_path"
	}
	return "lifecycle.secret_file"
}
