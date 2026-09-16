// Command trouble is the operator CLI (SPEC-12 §2.1). Every verb here is an
// operator action, never a daemon action: the daemon owns the ledger, the
// dashboard and the loops, and the CLI either reads what the daemon wrote
// (config explain, topology, token list), renders what an installer needs
// (install), or runs out-of-band by construction (check-stall, escalate).
//
// Exit codes are part of the contract:
//
//	0   ok
//	8   liveness surface stale/unreadable   (TROUBLE-LIFECYCLE-008)
//	9   ledger sequence stall               (TROUBLE-LIFECYCLE-009)
//	13  a refusable condition (SPEC-12 §2.1: config, bind, state root, units)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

const usage = `trouble — incident ladder daemon control

usage:
  trouble install [--scope user|system] [--root DIR] [--check] [--dry-run] [--force]
  trouble upgrade [--to PATH] [--rollback] [--wait DURATION]
  trouble config explain [--key K] [--json]
  trouble topology [--json]
  trouble check-stall [--health-url URL] [--state-root DIR] [--json]
  trouble escalate --unit NAME
  trouble dashboard token create --label LABEL --scopes read[,write][,autonomy]
  trouble dashboard token rotate --label LABEL
  trouble dashboard token revoke --label LABEL
  trouble dashboard token list [--json]
  trouble --version

Config precedence is flag > env > file > default (SPEC-12 §3.1); every key has a
default and ` + "`trouble config explain`" + ` shows which source won.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "--version", "-version", "version":
		v, sha, bt, unstamped := lifecycle.VersionInfo()
		state := "stamped"
		if unstamped {
			state = "UNSTAMPED (degraded; trouble install refuses to enable the unit without --force)"
		}
		fmt.Printf("%s %s %s [%s]\n", v, sha, bt, state)
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "install":
		return cmdInstall(args[1:])
	case "upgrade":
		return cmdUpgrade(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "topology":
		return cmdTopology(args[1:])
	case "check-stall":
		return cmdCheckStall(args[1:])
	case "escalate":
		return cmdEscalate(args[1:])
	case "dashboard":
		return cmdDashboard(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// defaultConfigPath mirrors the config_path default (SPEC-12 §3.1): the resolved
// config may override it, and every verb accepts --config.
func defaultConfigPath() string {
	if v := os.Getenv("TROUBLE_CONFIG_PATH"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "trouble", "config.toml")
}

// resolve loads the resolved config for a verb, consuming --config so no verb's
// own flag set has to know about it. A config problem is exit 13.
func resolve(args []string) (lifecycle.Resolved, int) {
	cfgPath := defaultConfigPath()
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	res, err := lifecycle.Resolve(rest, os.Environ(), cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return lifecycle.Resolved{}, 13
	}
	return res, 0
}

func cmdConfig(args []string) int {
	if len(args) == 0 || args[0] != "explain" {
		fmt.Fprint(os.Stderr, "usage: trouble config explain [--key K] [--json]\n")
		return 2
	}
	fs := flag.NewFlagSet("config explain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	key := fs.String("key", "", "show these keys only (comma-separated)")
	asJSON := fs.Bool("json", false, "machine-readable ConfigValue[]")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	var keys []string
	if *key != "" {
		keys = strings.Split(*key, ",")
	}
	rows, err := lifecycle.Explain(res, keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config explain: %v\n", err)
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "config explain: %v\n", err)
			return 13
		}
		return 0
	}
	width := 0
	for _, r := range rows {
		if n := len(r.Key); n > width {
			width = n
		}
	}
	for _, r := range rows {
		val := fmt.Sprintf("%v", r.Value)
		if r.Redacted {
			val = "[REDACTED:config]"
		}
		fmt.Printf("%-*s  %-38s  %-8s  %s\n", width, r.Key, val, r.Source, r.SourceRef)
	}
	return 0
}

func cmdTopology(args []string) int {
	fs := flag.NewFlagSet("topology", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable TopologyDecision[]")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	rows := lifecycle.TopologyDecisions(res.Config)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "topology: %v\n", err)
			return 13
		}
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%s→%s  %s\n        keys: %s\n", r.From, r.To, r.Decision, strings.Join(r.ConfigKeys, ", "))
	}
	cfg := res.Config
	fmt.Printf("\ningest.bind=%s (zone %s) dashboard.bind=%s (zone %s)\n",
		cfg.Ingest.Bind, lifecycle.ZoneOf(cfg.Ingest.Bind, "ingest", lifecycle.AuthToken),
		cfg.Dashboard.Bind, lifecycle.ZoneOf(cfg.Dashboard.Bind, "dashboard", lifecycle.AuthLoopback))
	return 0
}

func cmdCheckStall(args []string) int {
	fs := flag.NewFlagSet("check-stall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	healthURL := fs.String("health-url", "", "override the health URL (default: dashboard.bind + /health.json)")
	stateRoot := fs.String("state-root", "", "override the state root")
	asJSON := fs.Bool("json", false, "machine-readable verdict")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	cfg := res.Config
	if *stateRoot != "" {
		cfg.StateRoot = *stateRoot
	}
	if *healthURL != "" {
		cfg.HealthURL = *healthURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verdict, err := lifecycle.StallCheck(ctx, cfg)
	if err != nil && verdict.Code == "" {
		fmt.Fprintf(os.Stderr, "check-stall: %v\n", err)
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(verdict)
	} else {
		fmt.Printf("%s class=%s seq_age=%.0fs breaches=%d exit=%d\n", verdict.Code, verdict.Class, verdict.SeqAgeS, verdict.BreachCount, verdict.Exit)
		if verdict.Reason != "" {
			fmt.Printf("reason: %s\n", verdict.Reason)
		}
	}
	return verdict.Exit
}

func cmdEscalate(args []string) int {
	fs := flag.NewFlagSet("escalate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	unit := fs.String("unit", "", "the failing unit name (systemd %i)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *unit == "" {
		fmt.Fprint(os.Stderr, "usage: trouble escalate --unit NAME\n")
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	if err := lifecycle.Escalate(context.Background(), res.Config, *unit); err != nil {
		fmt.Fprintf(os.Stderr, "escalate: %v\n", err)
		return 13
	}
	fmt.Printf("escalated %s\n", *unit)
	return 0
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scope := fs.String("scope", "", "user|system (default: from lifecycle.systemd_scope)")
	root := fs.String("root", "", "write the units into this unit root instead of the real one (tests)")
	checkOnly := fs.Bool("check", false, "verify the installation and the escalation wiring, write nothing")
	dryRun := fs.Bool("dry-run", false, "render and report, do not enable")
	force := fs.Bool("force", false, "install despite an unstamped build")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	rep, err := lifecycle.Install(res.Config, lifecycle.InstallOptions{
		Scope: *scope, Root: *root, Check: *checkOnly, DryRun: *dryRun, Force: *force,
	})
	for _, row := range rep.Rows {
		fmt.Fprintf(os.Stdout, "audit  %-28s %s\n", row.Key, row.SourceRef)
	}
	for _, u := range rep.Units {
		fmt.Fprintf(os.Stdout, "unit   %-28s -> %s\n", u, rep.UnitDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 13
	}
	if rep.Degraded != "" {
		fmt.Fprintf(os.Stderr, "install degraded: %s\n", rep.Degraded)
	}
	fmt.Printf("install ok: %d units, dir=%s\n", len(rep.Units), rep.UnitDir)
	return 0
}

func cmdUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	to := fs.String("to", "", "new binary path")
	rollback := fs.Bool("rollback", false, "restore backups/bin/<newest> and restart")
	wait := fs.Duration("wait", 0, "READY deadline after the restart (default: lifecycle.upgrade_ready_timeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	plan := lifecycle.UpgradeOptions{To: *to, Rollback: *rollback}
	if *wait > 0 {
		plan.Wait = types.Duration(wait.String())
	}
	if err := lifecycle.RunUpgrade(context.Background(), res.Config, plan); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade: %v\n", err)
		return 13
	}
	fmt.Printf("upgrade ok (to=%s rollback=%v)\n", *to, *rollback)
	return 0
}

func cmdDashboard(args []string) int {
	if len(args) < 2 || args[0] != "token" {
		fmt.Fprint(os.Stderr, "usage: trouble dashboard token create|rotate|revoke|list\n")
		return 2
	}
	verb, rest := args[1], args[2:]
	fs := flag.NewFlagSet("dashboard token "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	label := fs.String("label", "", "token label (becomes Actor.ID on writes, e.g. dash-write@laptop)")
	scopes := fs.String("scopes", "", "comma-separated: read,write,autonomy")
	asJSON := fs.Bool("json", false, "machine-readable output (list only)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	path := res.Config.Dashboard.TokenFile
	if path == "" {
		path = defaultTokenPath()
	}
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}

	if verb == "list" {
		return listTokens(path, *asJSON)
	}

	// The plaintext is shown exactly once, here: this is the only code path in
	// the repository that can mint a dashboard token (SPEC-10 §3.2).
	store, err := dashboard.LoadTokenStore(path, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard token: %v\n", err)
		return 13
	}
	switch verb {
	case "create":
		if *label == "" || *scopes == "" {
			fmt.Fprint(os.Stderr, "usage: trouble dashboard token create --label LABEL --scopes read[,write][,autonomy]\n")
			return 2
		}
		sc, err := parseScopes(*scopes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token create: %v\n", err)
			return 2
		}
		tok, plaintext, err := store.Mint(*label, sc, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token create: %v\n", err)
			return 13
		}
		if err := store.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token create: %v\n", err)
			return 13
		}
		fmt.Println(plaintext)
		fmt.Fprintf(os.Stderr, "token %q created with scopes %v; the plaintext above is shown once and is not recoverable\n", tok.ID, scopeStrings(tok.Scopes))
		return 0
	case "rotate":
		if *label == "" {
			fmt.Fprint(os.Stderr, "usage: trouble dashboard token rotate --label LABEL\n")
			return 2
		}
		tok, plaintext, err := store.Rotate(*label, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token rotate: %v\n", err)
			return 13
		}
		if err := store.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token rotate: %v\n", err)
			return 13
		}
		fmt.Println(plaintext)
		fmt.Fprintf(os.Stderr, "%q revoked, %q minted (no grace window)\n", *label, tok.ID)
		return 0
	case "revoke":
		if *label == "" {
			fmt.Fprint(os.Stderr, "usage: trouble dashboard token revoke --label LABEL\n")
			return 2
		}
		if err := store.Revoke(*label, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token revoke: %v\n", err)
			return 13
		}
		if err := store.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard token revoke: %v\n", err)
			return 13
		}
		fmt.Printf("revoked %s\n", *label)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown dashboard token verb %q\n", verb)
		return 2
	}
}

// tokenFileView is the on-disk wrapper (SPEC-10 §3.2). The CLI reads it for
// `list` because the store deliberately exposes no list API: the file is the
// audit record and reading it must not require a live server.
type tokenFileView struct {
	Version int           `json:"version"`
	Tokens  []types.Token `json:"tokens"`
}

func listTokens(path string, asJSON bool) int {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard token list: %v\n", err)
		return 13
	}
	var tf tokenFileView
	if err := json.Unmarshal(b, &tf); err != nil {
		fmt.Fprintf(os.Stderr, "dashboard token list: %v\n", err)
		return 13
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(tf.Tokens)
		return 0
	}
	for _, t := range tf.Tokens {
		state := "active"
		if t.Revoked {
			state = "revoked"
		}
		fmt.Printf("%-28s %-22s %-9s created=%s last_used=%s\n", t.ID, strings.Join(scopeStrings(t.Scopes), ","), state, t.CreatedTS, t.LastUsedTS)
	}
	return 0
}

func defaultTokenPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "trouble", "dashboard-tokens.json")
}

func parseScopes(in string) ([]types.Scope, error) {
	out := make([]types.Scope, 0, 3)
	for _, raw := range strings.Split(in, ",") {
		s := strings.TrimSpace(raw)
		switch types.Scope(s) {
		case types.ScopeRead, types.ScopeWrite, types.ScopeAutonomy:
			out = append(out, types.Scope(s))
		case "":
		default:
			return nil, fmt.Errorf("unknown scope %q (read|write|autonomy)", s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scopes given")
	}
	return out, nil
}

func scopeStrings(sc []types.Scope) []string {
	out := make([]string, 0, len(sc))
	for _, s := range sc {
		out = append(out, string(s))
	}
	return out
}
