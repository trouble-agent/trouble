package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExpandTokenPath resolves a leading `~` in a `dashboard.token_file` path
// against the current user's home directory (SPEC-10 §3.2/§3.4, whose shipped
// default is "~/.config/trouble/dashboard-tokens.json").
//
// Exactly the two forms the spec's default uses are expanded: "~/" and the bare
// "~". Every other input is returned byte-for-byte unchanged — absolute paths,
// relative paths, and any path whose "~" is not at the front are left alone.
// The `~user` form is deliberately NOT resolved: §3.2 defines the store as the
// current user's own file (mode 0600, "ownership = the daemon's user"), and
// taking it as a literal directory name is what made a missing store look like a
// valid empty one in the first place.
//
// An unexpandable tilde path is an ERROR, never a fall-back to the literal
// string: os.Stat("~/…") always fails with ENOENT, which the store's fail-open
// branch reads as "no credentials yet" — a valid empty store that 401s every
// presented token (TROUBLE-DASHBOARD-002) with no signal at all.
func ExpandTokenPath(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		// Includes the empty path: an empty token_file keeps its existing
		// meaning (Config.normalize fills the default before we get here).
		return p, nil
	}
	rest := strings.TrimPrefix(p, "~")
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("path %q: only a leading ~/ (or ~) may be expanded, not %q", p, "~"+rest)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("path %q: home directory unknown: %w", p, err)
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return filepath.Clean(home), nil
	}
	return filepath.Join(home, rest), nil
}

// DefaultTokenPath is the SPEC-10 §3.4 default `dashboard.token_file`, resolved
// to an absolute path: `~/.config/trouble/dashboard-tokens.json` under the
// current user's home directory.
//
// It is the single expression both sides of the store boundary resolve through.
// The daemon reaches the default through Config.normalize()/DefaultConfig(); the
// operator CLI has to name the same file when no config declares one, and it
// used to name it with os.UserConfigDir() instead — which honours
// XDG_CONFIG_HOME, while the shipped default's documented and daemon-reachable
// form is `~/`. On a host with XDG_CONFIG_HOME set, the CLI therefore minted
// into `$XDG_CONFIG_HOME/trouble/dashboard-tokens.json` while the daemon read
// `$HOME/.config/trouble/dashboard-tokens.json`: the mint succeeded, printed a
// token, and that token 401'd everywhere — TROUBLE-DASHBOARD-002's signature,
// by a second route. ONE function for the default means the two can no longer
// drift; the checked-in default string itself stays the spec's `~/` form.
func DefaultTokenPath() (string, error) {
	return ExpandTokenPath(DefaultConfig().TokenFile)
}
