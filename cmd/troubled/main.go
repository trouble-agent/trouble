// Command troubled is the daemon (SPEC-12 §3, §4): one process per state root,
// the only ledger writer, the only dashboard server and the only thing that
// samples the host.
//
// It does not implement policy. Every decision lives in a subsystem package; this
// binary resolves the configuration, performs the boot sequence, starts the loops
// and drains — in that order, refusing to serve at the first failure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/totalwindupflightsystems/trouble/internal/app"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
)

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("troubled", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/trouble/config.toml)")
	verbose := fs.Bool("v", false, "debug logging")
	version := fs.Bool("version", false, "print the version triple and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *version {
		v, sha, bt, unstamped := lifecycle.VersionInfo()
		state := "stamped"
		if unstamped {
			state = "UNSTAMPED"
		}
		fmt.Printf("%s %s %s [%s]\n", v, sha, bt, state)
		return 0
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// The daemon's lifetime is the process's: SIGTERM/SIGINT cancel the context,
	// which is what starts the drain inside RunDaemon (SPEC-12 §4.2).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	args := fs.Args()
	if *configPath != "" {
		args = append([]string{"--config", *configPath}, args...)
	}

	_, err := app.RunDaemon(ctx, app.BootOptions{
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
