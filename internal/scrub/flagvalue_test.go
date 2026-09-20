package scrub

import (
	"context"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §3.3 rule 9's explicit-path exemption, both directions, at the rule
// level and at the persistence boundary the daemon's argv scan is built on
// (SPEC-12 §2.5a: `--dashboard-token_file /path` rides argv; `--hub-token
// <token>` and `--dashboard-token_file <token>` do not).

func TestCLIFlagSecretExemptsExplicitPathValue(t *testing.T) {
	e := newTestEngine(t, "")
	tests := []struct {
		name string
		in   string
		out  string
	}{
		{
			name: "space form, absolute path",
			in:   `--dashboard-token_file /srv/trouble/dashboard-tokens.json`,
			out:  `--dashboard-token_file /srv/trouble/dashboard-tokens.json`,
		},
		{
			// an absolute path needs STRUCTURE (a separator or an extension),
			// not two of them: a single-segment store in / is a path too.
			name: "absolute single-segment path with an extension",
			in:   `--dashboard-token_file /tokens.json`,
			out:  `--dashboard-token_file /tokens.json`,
		},
		{
			// the shell idioms carry no structure requirement: no credential
			// alphabet this table knows begins with `~`.
			name: "home-relative value without an extension",
			in:   `--hub-token ~/hubtoken`,
			out:  `--hub-token ~/hubtoken`,
		},
		{
			name: "equals form, absolute path",
			in:   `--dashboard-token_file=/srv/trouble/dashboard-tokens.json`,
			out:  `--dashboard-token_file=/srv/trouble/dashboard-tokens.json`,
		},
		{
			name: "home-relative path",
			in:   `--hub-token ~/trouble/hub.token`,
			out:  `--hub-token ~/trouble/hub.token`,
		},
		{
			name: "dot-relative path",
			in:   `--hub-token ./tokens.json`,
			out:  `--hub-token ./tokens.json`,
		},
		{
			name: "parent-relative path",
			in:   `--hub-token ../tokens.json`,
			out:  `--hub-token ../tokens.json`,
		},
		{
			name: "quoted path keeps the quotes",
			in:   `--token_file "/srv/tokens.json"`,
			out:  `--token_file "/srv/tokens.json"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.ExemptValues()
			got, res := scrubString(t, e, types.TgEventMsg, "1", tt.in)
			if got != tt.out {
				t.Errorf("scrub(%q) = %q, want %q (a path-shaped value is left unchanged)", tt.in, got, tt.out)
			}
			if res.Redactions != 0 || len(res.ByRule) != 0 {
				t.Errorf("%s: by_rule = %s, want no rule to fire (this target carries no path rule)",
					tt.in, byRuleString(res.ByRule))
			}
			if e.ExemptValues() == before {
				t.Errorf("%s: exempt_values did not move: the exemption is counted, not silent", tt.in)
			}
		})
	}

	// The value the defect was reported with, in full (TRBL-026). `/home/user/…`
	// also matches the OPTIONAL home-root path rule 17, so the prefix is redacted
	// with the path marker (§3.5) — a different concern from this exemption, which
	// is about the REFUSAL. What matters here is that rule 9 did not fire and the
	// value was counted exempt; the boundary case below pins that it rides argv.
	before := e.ExemptValues()
	got, res := scrubString(t, e, types.TgEventMsg, "1",
		`--dashboard-token_file /home/user/.config/trouble/dashboard-tokens.json`)
	want := `--dashboard-token_file [REDACTED:path_home_root]/.config/trouble/dashboard-tokens.json`
	if got != want {
		t.Errorf("scrub(the reported store path) = %q, want %q", got, want)
	}
	if res.ByRule["cli_flag_secret"] != 0 {
		t.Errorf("cli_flag_secret fired on a token-store path: by_rule = %s", byRuleString(res.ByRule))
	}
	if e.ExemptValues() == before {
		t.Error("exempt_values did not move for the reported store path")
	}
}

// The other direction: the exemption is an allowlist of path SHAPES, so every
// value that is not an explicit path — a token, a bare file name, a base64 run
// that happens to start with a slash, a lone slash — is redacted exactly as
// before, whichever flag name it follows.
func TestCLIFlagSecretStillRefusesANonPathValue(t *testing.T) {
	e := newTestEngine(t, "")
	tests := []struct {
		name string
		in   string
	}{
		{"token after the key that carries a path", `--dashboard-token_file abcDEF123ghiJKL456mnoPQR`},
		{"token after its own key", `--hub-token abcDEF123ghiJKL456mnoPQR`},
		{"the normative P6 vector", `--api-key=abcdef123456`},
		{"bare name is not a path", `--token_file tokens.json`},
		{"path prefix, credential alphabet", `--token_file /9j4K+fg==`},
		{"a lone slash is not a path", `--token_file /`},
		// `/` IS in the standard-base64 alphabet, so an absolute value needs
		// path STRUCTURE: a slash-prefixed word with neither a separator nor an
		// extension is a credential shape, not a path (SPEC-02 §3.3 rule 9).
		{"slash-prefixed credential, no structure", `--token_file /abcDEF123ghiJKL456mnoPQR`},
		{"absolute single word, no separator or extension", `--hub-token /hub-token`},
		{"no separator at all", `--token hunter2`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.ExemptValues()
			got, res := scrubString(t, e, types.TgEventMsg, "1", tt.in)
			if !strings.Contains(got, "[REDACTED:") {
				t.Errorf("scrub(%q) = %q, want a redaction marker", tt.in, got)
			}
			if res.ByRule["cli_flag_secret"] != 1 {
				t.Errorf("scrub(%q): by_rule = %s, want cli_flag_secret=1", tt.in, byRuleString(res.ByRule))
			}
			if e.ExemptValues() != before {
				t.Errorf("scrub(%q): exempt_values moved for a non-path value", tt.in)
			}
		})
	}
}

// The assignment form is rule 7/8 territory and keeps its strict behaviour: only
// rule 9 carries the exemption, and a flag whose name ENDS at the sensitive word
// (`--token=<value>`) is still read as an assignment (§3.6 rule 5). That is what
// makes the two spellings of SPEC-12 §2.5a differ for `hub.token`:
// `--dashboard-token_file=<path>` is rule 9's own flag form and resolves, while
// `--hub-token=<path>` is `NAME=value` first and stays refused — an environment
// or config dump writes the same shape, so the assignment rules cannot carry a
// path exemption without losing the values they exist to catch.
func TestCLIFlagSecretAssignmentFormIsUnchanged(t *testing.T) {
	e := newTestEngine(t, "")
	got, res := scrubString(t, e, types.TgEventMsg, "1", `--token=/srv/tokens.json`)
	if got != `--token=[REDACTED:env_assign]` {
		t.Errorf("scrub = %q, want the env_assign marker (rule 7 has no path exemption)", got)
	}
	if res.ByRule["env_assign"] != 1 {
		t.Errorf("by_rule = %s, want env_assign=1", byRuleString(res.ByRule))
	}
	if err := MandatoryScan(context.Background(), []byte(`--token=/srv/tokens.json`)); err == nil {
		t.Error("the boundary accepted an assignment-form value: rules 7/8 are unchanged by design")
	}
	// A whole assignment written by an environment or a config snapshot is the
	// same shape and stays refused, which is why the exemption cannot live in
	// rule 7: the dump is not a command line.
	if err := MandatoryScan(context.Background(), []byte(`HUB_TOKEN=/srv/hub.token`)); err == nil {
		t.Error("the boundary accepted `HUB_TOKEN=/srv/hub.token`: an assignment is not a flag value")
	}
}

// The boundary re-scan is the scanner the daemon's argv control runs over
// /proc/self/cmdline (SPEC-12 §3.2 rule 2). Both directions land here, and the
// refusal keeps the same code and the same rule name.
func TestMandatoryScanArgvBothDirections(t *testing.T) {
	pathArgv := strings.Join([]string{
		"troubled",
		"--config", "/srv/trouble/config.toml",
		"--state_root", "/srv/trouble/state",
		"--dashboard-token_file", "/home/user/.config/trouble/dashboard-tokens.json",
	}, "\n")
	if err := MandatoryScan(context.Background(), []byte(pathArgv)); err != nil {
		t.Errorf("argv with a token-store PATH = %v, want nil (the daemon must be able to be pointed at a store)", err)
	}
	// The `=` spelling of a flag name that EXTENDS the sensitive word is rule 9's
	// own form (no assignment rule claims it), so it rides argv as well.
	if err := MandatoryScan(context.Background(), []byte(
		`--dashboard-token_file=/home/user/.config/trouble/dashboard-tokens.json`)); err != nil {
		t.Errorf("argv with an `=` token-store PATH = %v, want nil", err)
	}

	for _, tt := range []struct{ argv, wantRule string }{
		{"--hub-token abcDEF123ghiJKL456mnoPQR", "cli_flag_secret"},
		{"--dashboard-token_file abcDEF123ghiJKL456mnoPQR", "cli_flag_secret"},
		// `/` is in the base64 alphabet, so a slash-prefixed word with no path
		// structure is a credential here too (the exemption is structural).
		{"--hub-token /abcDEF123ghiJKL456mnoPQR", "cli_flag_secret"},
		// rule 7 reaches the `=` form first (the boundary reports the first
		// mandatory rule that matched, in §3.3 order); the refusal is what this
		// case pins, and it is the same code either way.
		{"--api-key=abcdef123456", "env_assign"},
		{"--hub-token=/srv/hub.token", "env_assign"},
	} {
		err := MandatoryScan(context.Background(), []byte(tt.argv))
		if err == nil {
			t.Errorf("argv %q = nil, want the boundary refusal", tt.argv)
			continue
		}
		if !BoundaryRefused(err) {
			t.Errorf("argv %q: error is not TROUBLE-SCRUB-008: %v", tt.argv, err)
		}
		if !strings.Contains(err.Error(), tt.wantRule) {
			t.Errorf("argv %q: refusal does not name %s: %v", tt.argv, tt.wantRule, err)
		}
	}
}

// A payload that carries a path-shaped flag value is a fixed point of the rule
// table (§6): the second pass redacts nothing and the boundary accepts the
// first pass's own output — which is what lets the value be persisted.
func TestCLIFlagSecretPathValueIsAFixedPoint(t *testing.T) {
	e := newTestEngine(t, "")
	in := `spawn: troubled --dashboard-token_file /srv/trouble/dashboard-tokens.json (state_root=/srv/trouble/state)`
	once, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if once != in {
		t.Fatalf("first pass = %q, want the input unchanged", once)
	}
	if res.Redactions != 0 {
		t.Fatalf("first pass redactions = %d, want 0", res.Redactions)
	}
	twice, res2 := scrubString(t, e, types.TgEventMsg, "1", once)
	if twice != once || res2.Redactions != 0 {
		t.Errorf("second pass = %q (redactions %d), want the first pass's output and 0", twice, res2.Redactions)
	}
	if err := MandatoryScan(context.Background(), []byte(once)); err != nil {
		t.Errorf("boundary refused the scrubbed payload: %v", err)
	}
}
