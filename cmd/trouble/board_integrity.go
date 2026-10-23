package main

import (
	"fmt"
	"os"

	"github.com/trouble-agent/trouble/internal/boardintegrity"
)

// cmdBoard routes board sub-verbs (INT-BOARD-1).
func cmdBoard(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "board: no verb specified. Use 'board integrity' to validate board rows against git history.\n")
		return 2
	}
	switch args[0] {
	case "integrity":
		return cmdBoardIntegrity(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "board: unknown verb %q. Use 'board integrity'.\n", args[0])
		return 2
	}
}

func cmdBoardIntegrity(args []string) int {
	results, err := boardintegrity.Validate(".", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "board integrity: %v\n", err)
		return 2
	}

	fmt.Println(boardintegrity.Summary(results))

	for _, r := range results {
		if !r.Exists {
			return 1
		}
	}
	return 0
}
