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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

const usage = `trouble — incident ladder daemon control

usage:
  trouble install [--scope user|system] [--root DIR] [--check] [--dry-run] [--force]
  trouble upgrade [--to PATH|VERSION] [--rollback] [--wait DURATION]
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

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "--version", "-version", "version":
		v, sha, bt, unstamped := lifecycle.Version()
		state := "stamped"
		if unstamped {
			state = "UNSTAMPED (degraded; trouble install refuses to enable the unit)"
		}
		fmt.Fprintf(stdout, "%s %s %s [%s]\n", v, sha, bt, state)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "upgrade":
		return cmdUpgrade(args[1:], stdout, stderr)
	case "config":
		return cmdConfig(args[1:], stdout, stderr)
	case "topology":
		return cmdTopology(args[1:], stdout, stderr)
	case "check-stall":
		return cmdCheckStall(args[1:], stdout, stderr)
	case "escalate":
		return cmdEscalate(args[1:], stdout, stderr)
	case "dashboard":
		return cmdDashboard(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// resolve loads the resolved config for a verb. A config problem is exit 13
// (SPEC-12 §2.1: "Exit 13 on any refusable condition").
func resolve(args []string, stderr *os.File) (lifecycle.Resolved, int) {
	cfgPath := lifecycle.DefaultConfigPath()
	// --config is accepted by every verb and consumed here, so a verb's own
	// flags never have to know about it.
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
		fmt.Fprintf(stderr, "config: %v\n", err)
		return lifecycle.Resolved{}, 13
	}
	return res, 0
}

func cmdConfig(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "explain" {
		fmt.Fprint(stderr, "usage: trouble config explain [--key K] [--json]\n")
		return 2
	}
	fs := flag.NewFlagSet("config explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "show one key only")
	asJSON := fs.Bool("json", false, "machine-readable ConfigValue[]")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	var keys []string
	if *key != "" {
		keys = strings.Split(*key, ",")
	}
	rows, err := lifecycle.Explain(res, keys)
	if err != nil {
		fmt.Fprintf(stderr, "config explain: %v\n", err)
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "config explain: %v\n", err)
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
		fmt.Fprintf(stdout, "%-*s  %-40s  %-8s  %s\n", width, r.Key, val, r.Source, r.SourceRef)
	}
	return 0
}

func cmdTopology(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("topology", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "machine-readable TopologyDecision[]")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	rows := lifecycle.TopologyDecisions(res.Config)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "topology: %v\n", err)
			return 13
		}
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s→%s  %s\n        keys: %s\n", r.From, r.To, r.Decision, strings.Join(r.ConfigKeys, ", "))
	}
	fmt.Fprintf(stdout, "\ningest.bind=%s dashboard.bind=%s zone=%s\n",
		res.Config.IngestBind, res.Config.DashboardBind, lifecycle.ZoneOf(res.Config.IngestBind, "cli", lifecycle.AuthLoopbackDSN))
	return 0
}

func cmdCheckStall(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("check-stall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	healthURL := fs.String("health-url", "", "override the health URL (default: dashboard.bind + /health.json)")
	stateRoot := fs.String("state-root", "", "override the state root")
	asJSON := fs.Bool("json", false, "machine-readable verdict")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
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
		fmt.Fprintf(stderr, "check-stall: %v\n", err)
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(verdict)
	} else {
		fmt.Fprintf(stdout, "%s %s seq=%d seq_age=%.0fs breaches=%d\n",
			verdict.Code, verdict.Class, verdict.Seq, verdict.SeqAgeS, verdict.BreachCount)
		if verdict.Detail != "" {
			fmt.Fprintf(stdout, "detail: %s\n", verdict.Detail)
		}
	}
	return verdict.ExitCode()
}

func cmdEscalate(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("escalate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	unit := fs.String("unit", "", "the failing unit name (systemd %i)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *unit == "" {
		fmt.Fprint(stderr, "usage: trouble escalate --unit NAME\n")
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	if err := lifecycle.Escalate(context.Background(), res.Config, *unit); err != nil {
		fmt.Fprintf(stderr, "escalate: %v\n", err)
		return 13
	}
	fmt.Fprintf(stdout, "escalated %s\n", *unit)
	return 0
}

func cmdInstall(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("scope", "", "user|system (default: user when XDG_RUNTIME_DIR is set)")
	root := fs.String("root", "", "write the units into this unit root instead of the real one (tests)")
	checkOnly := fs.Bool("check", false, "verify the installation and the escalation wiring, write nothing")
	dryRun := fs.Bool("dry-run", false, "render and report, do not enable")
	force := fs.Bool("force", false, "install despite an unstamped build")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	cfg := res.Config
	switch *scope {
	case "":
	case "user":
		cfg.SystemdScope = "user"
	case "system":
		cfg.SystemdScope = "system"
	default:
		fmt.Fprintf(stderr, "install: unknown --scope %q\n", *scope)
		return 2
	}
	if *root != "" {
		cfg.UnitDirOverride = *root
	}
	plan, err := lifecycle.InstallPlan(cfg, lifecycle.InstallOptions{Check: *checkOnly, DryRun: *dryRun, Force: *force})
	if err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 13
	}
	for _, row := range plan.Rows {
		fmt.Fprintf(stdout, "%-28s %s\n", row.Key, row.Value)
	}
	if plan.Refused {
		fmt.Fprintf(stderr, "install refused: %s\n", plan.Reason)
		return 13
	}
	fmt.Fprintf(stdout, "install: %d units, scope=%s root=%s\n", len(plan.Units), cfg.SystemdScope, plan.UnitDir)
	return 0
}

func cmdUpgrade(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "new binary path or version")
	rollback := fs.Bool("rollback", false, "restore backups/bin/<previous> and restart")
	wait := fs.Duration("wait", 30*time.Second, "READY deadline after the restart")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	plan := lifecycle.UpgradePlan{To: *to, Rollback: *rollback, Wait: types.Duration(wait.String())}
	if err := lifecycle.Upgrade(ctx, res.Config, plan); err != nil {
		fmt.Fprintf(stderr, "upgrade: %v\n", err)
		return 13
	}
	fmt.Fprintf(stdout, "upgrade ok (to=%s rollback=%v wait=%s)\n", *to, *rollback, *wait)
	return 0
}

func cmdDashboard(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "token" {
		fmt.Fprint(stderr, "usage: trouble dashboard token create|rotate|revoke|list\n")
		return 2
	}
	if len(args) < 2 {
		fmt.Fprint(stderr, "usage: trouble dashboard token create|rotate|revoke|list\n")
		return 2
	}
	verb, rest := args[1], args[2:]
	fs := flag.NewFlagSet("dashboard token "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	label := fs.String("label", "", "token label (becomes Actor.ID on writes, e.g. dash-write@laptop)")
	scopes := fs.String("scopes", "", "comma-separated: read,write,autonomy")
	asJSON := fs.Bool("json", false, "machine-readable output (list only)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	res, code := resolve(fs.Args(), stderr)
	if code != 0 {
		return code
	}
	path := res.Config.Dashboard.TokenFile
	if path == "" {
		path = dashboard.DefaultTokenPath()
	}
	// The token store's plaintext is shown exactly once, here, and this is the
	// only place in the codebase that can mint one (SPEC-10 §3.2, CLI-only).
	store, err := dashboard.LoadTokenStoreForCLI(path, res.Config.ProjectKeys())
	if err != nil {
		fmt.Fprintf(stderr, "dashboard token: %v\n", err)
		return 13
	}
	switch verb {
	case "create":
		if *label == "" || *scopes == "" {
			fmt.Fprint(stderr, "usage: trouble dashboard token create --label LABEL --scopes read[,write][,autonomy]\n")
			return 2
		}
		sc, err := parseScopes(*scopes)
		if err != nil {
			fmt.Fprintf(stderr, "dashboard token create: %v\n", err)
			return 2
		}
		tok, plaintext, err := store.Mint(*label, sc)
		if err != nil {
			fmt.Fprintf(stderr, "dashboard token create: %v\n", err)
			return 13
		}
		fmt.Fprintf(stdout, "%s\n", plaintext)
		fmt.Fprintf(stderr, "token %q created with scopes %v; the plaintext above is shown once and is not recoverable\n", tok.ID, tok.Scopes)
		return 0
	case "rotate":
		if *label == "" {
			fmt.Fprint(stderr, "usage: trouble dashboard token rotate --label LABEL\n")
			return 2
		}
		tok, plaintext, err := store.Rotate(*label)
		if err != nil {
			fmt.Fprintf(stderr, "dashboard token rotate: %v\n", err)
			return 13
		}
		fmt.Fprintf(stdout, "%s\n", plaintext)
		fmt.Fprintf(stderr, "%q revoked, %q minted (no grace window)\n", *label, tok.ID)
		return 0
	case "revoke":
		if *label == "" {
			fmt.Fprint(stderr, "usage: trouble dashboard token revoke --label LABEL\n")
			return 2
		}
		if err := store.Revoke(*label); err != nil {
			fmt.Fprintf(stderr, "dashboard token revoke: %v\n", err)
			return 13
		}
		fmt.Fprintf(stdout, "revoked %s\n", *label)
		return 0
	case "list":
		toks := store.Tokens()
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			// Hash is part of the stored form; it is not a secret (it is the
			// digest, not the token) and the list is what makes an audit possible.
			_ = enc.Encode(toks)
			return 0
		}
		for _, t := range toks {
			state := "active"
			if t.Revoked {
				state = "revoked"
			}
			fmt.Fprintf(stdout, "%-28s %-22s %-9s created=%s last_used=%s\n", t.ID, strings.Join(scopeStrings(t.Scopes), ","), state, t.CreatedTS, t.LastUsedTS)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown dashboard token verb %q\n", verb)
		return 2
	}
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
