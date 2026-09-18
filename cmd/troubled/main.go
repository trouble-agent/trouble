// Command troubled is the daemon (SPEC-12 §3, §4): one process per state root,
// the only ledger writer, the only dashboard server and the only thing that
// samples the host.
//
// It does not implement policy. Every decision lives in a subsystem package; this
// binary resolves the configuration, performs the boot sequence, starts the loops
// and drains — in that order, refusing to serve at the first failure.
//
// Its argv entry point does not adjudicate config keys either (SPEC-12 §2.5):
// every key the resolver registers is also a flag, and the registry — not this
// binary — is the whitelist that reads them. This file owns only the daemon's
// own three flags and the split between the two surfaces.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/totalwindupflightsystems/trouble/internal/app"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
)

func main() { os.Exit(run(os.Args[1:])) }

// daemonUsage is the daemon's real surface (SPEC-12 §2.5): the three flags this
// binary parses itself, plus the per-key surface it forwards. Go's flag package
// automessage is deliberately not used — it could only ever list the daemon's own
// flags and so contradicted the documented surface it was printed on behalf of.
const daemonUsage = `troubled — the incident-ladder daemon (SPEC-12 §3, §4)

usage:
  troubled [--config <path>] [-v] [--version] [--<key> <value>]...

the daemon's own flags:
  --config <path>   resolve against this config file. This is the operator
                    spelling of the config_path key; --config_path <path> is the
                    same selection in the mechanical form. The last one on argv
                    wins.
  -v                debug logging
  --version         print the version triple and exit
  -h, --help        this text

config keys (flag > env > file > default, SPEC-12 §3.1):
  every key the resolver registers is also a flag: a.b_c -> --a-b-c, with the
  value in the next argument or after '='. A flag beats the environment, the file
  and the default. An unknown key is refused by name (TROUBLE-LIFECYCLE-001) and
  the daemon exits 13. A single-dash token that is not -v or -h is refused for the
  same reason: the per-key surface is spelled with two dashes.

  'trouble config explain [--json]' lists every key and the source that won. Five
  keys cannot ride argv: projects, issues and skills are tables (a flag value is a
  scalar), and dashboard.token_file and hub.token are refused by the argv secret
  scan (TROUBLE-LIFECYCLE-013) because the flag NAME matches the mandatory
  cli_flag_secret rule. Set those from the file or the environment.

examples:
  troubled --config ~/.config/trouble/config.toml
  troubled --state_root ~/.local/state/trouble-scratch --ingest-bind 127.0.0.1:7645
`

// The daemon's own flag names, as they were accepted while the flag package
// parsed this surface (one or two leading dashes for each).
const (
	ownConfig  = "config"
	ownVerbose = "v"
	ownVersion = "version"
	ownHelp    = "help"
	ownHelpH   = "h"
)

// daemonArgs is what the daemon parses for itself out of argv. Every other
// argument is a config key flag and reaches the resolver unchanged.
type daemonArgs struct {
	config  string
	verbose bool
	version bool
	help    bool
}

// splitDaemonArgs partitions argv into the daemon's own flags and the config key
// flags that lifecycle.Resolve adjudicates on its own whitelist (SPEC-12 §2.5).
//
// A key flag is forwarded verbatim — never normalized, never filtered — because
// the registry, not this binary, decides whether it names a key: parseArgs in
// internal/lifecycle refuses an unknown key with TROUBLE-LIFECYCLE-001 and the
// daemon maps that refusal to exit 13. What this function owns is the other half
// of the split: a token that is neither one of the daemon's own flags nor a
// `--key` token is an ERROR named here, because argv is no longer parsed by the
// flag package and a silently ignored argument is how a typo turns into a scratch
// instance that ran on the wrong state root.
func splitDaemonArgs(argv []string) (daemonArgs, []string, error) {
	var own daemonArgs
	keys := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, hasValue, mine := ownFlag(arg)
		if !mine {
			if isShortToken(arg) {
				return daemonArgs{}, nil, fmt.Errorf("unknown flag %q: config key flags are spelled with two dashes (--<key>)", arg)
			}
			keys = append(keys, arg)
			continue
		}
		switch name {
		case ownConfig:
			if !hasValue {
				if i+1 == len(argv) {
					return daemonArgs{}, nil, fmt.Errorf("flag needs an argument: %s", arg)
				}
				i++
				value = argv[i]
			}
			own.config = value
		case ownVerbose, ownVersion, ownHelp, ownHelpH:
			b, err := boolValue(arg, value, hasValue)
			if err != nil {
				return daemonArgs{}, nil, err
			}
			switch name {
			case ownVerbose:
				own.verbose = b
			case ownVersion:
				own.version = b
			default:
				own.help = b
			}
		}
	}
	return own, keys, nil
}

// ownFlag reports whether arg spells one of the daemon's own flags, and splits
// the optional `=value` off it. Both the one- and two-dash spellings are
// accepted, exactly as the flag package accepted them before this row.
func ownFlag(arg string) (name, value string, hasValue, ok bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return "", "", false, false
	}
	body := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if body == "" {
		return "", "", false, false
	}
	name, value, hasValue = strings.Cut(body, "=")
	switch name {
	case ownConfig, ownVerbose, ownVersion, ownHelp, ownHelpH:
		return name, value, hasValue, true
	}
	return "", "", false, false
}

// isShortToken reports whether arg is a one-dash token. The per-key surface is
// spelled with two dashes (SPEC-12 §2.5), so a one-dash token has no other
// reading — and forwarding it would be silently ignored, because parseArgs only
// looks at `--` tokens.
func isShortToken(arg string) bool {
	return strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && arg != "-"
}

// boolValue reads the value of a boolean own flag the way the flag package did.
func boolValue(arg, value string, hasValue bool) (bool, error) {
	if !hasValue {
		return true, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid boolean value %q for %s", value, arg)
	}
	return b, nil
}

func run(argv []string) int {
	own, keys, err := splitDaemonArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "troubled: %v\n\n%s", err, daemonUsage)
		return 2
	}
	if own.help {
		fmt.Print(daemonUsage)
		return 0
	}
	if own.version {
		v, sha, bt, unstamped := lifecycle.VersionInfo()
		state := "stamped"
		if unstamped {
			state = "UNSTAMPED"
		}
		fmt.Printf("%s %s %s [%s]\n", v, sha, bt, state)
		return 0
	}

	level := slog.LevelInfo
	if own.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// The daemon's lifetime is the process's: SIGTERM/SIGINT cancel the context,
	// which is what starts the drain inside RunDaemon (SPEC-12 §4.2).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// `--config <path>` is re-emitted in the resolver's own spelling so the file
	// selector has exactly one shape downstream (internal/app splitConfigFlag).
	// It is APPENDED rather than prepended because that selector is last-one-wins
	// and the flag the operator typed must beat a --config_path further left.
	args := keys
	if own.config != "" {
		args = append(append(make([]string, 0, len(keys)+2), keys...), "--config", own.config)
	}

	_, err = app.RunDaemon(ctx, app.BootOptions{
		Args: args,
		Env:  os.Environ(),
		Log:  logger,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		logger.Error("boot refused", "err", err)
		// Exit 13 is the CLI's "refusable condition" code; the daemon uses it for
		// the same class so systemd's OnFailure sees one meaning per code.
		return 13
	}
	return 0
}
