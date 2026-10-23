// Package boardintegrity validates board rows against the git history.
//
// INT-BOARD-1: a board close must cite a sha that exists.
// Before this package, a row could close against a commit that was
// pushed then reset away — the board thought the defect was fixed
// while the code was still live.
package boardintegrity

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Row mirrors the JSONL schema used by .coding-hermes/board/tasks.jsonl.
// We only touch status, commit_hash, and id — nothing else.
type Row struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	CommitHash  string `json:"commit_hash"`
	Description string `json:"description,omitempty"`
}

// Result captures the outcome of validating one row.
type Result struct {
	RowID  string `json:"row_id"`
	SHA    string `json:"sha"`
	Exists bool   `json:"exists"`
	Branch string `json:"branch,omitempty"` // non-empty if SHA exists only on unmerged branches
	Msg    string `json:"msg,omitempty"`    // human-readable message on failure
}

// Validate reads the board JSONL and validates every completed row's
// commit_hash resolves to a real commit in the current git repository.
//
// It checks:
//  1. git cat-file -e <sha>^{commit} (current object database)
//  2. git branch --contains <sha> (exists on unmerged branch)
//  3. git reflog --all (was force-pushed away but once existed)
//
// path is the directory containing the repo (where git cat-file runs).
// boardPath is the path to tasks.jsonl (default: .coding-hermes/board/tasks.jsonl).
func Validate(path, boardPath string) ([]Result, error) {
	if boardPath == "" {
		boardPath = filepath.Join(path, ".coding-hermes", "board", "tasks.jsonl")
	}

	data, err := os.ReadFile(boardPath)
	if err != nil {
		return nil, fmt.Errorf("read board: %w", err)
	}

	var rows []Row
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("decode row %q: %w", line[:min(len(line), 80)], err)
		}
		rows = append(rows, r)
	}

	results := make([]Result, 0, len(rows))
	for _, row := range rows {
		if row.Status != "complete" || row.CommitHash == "" {
			continue
		}
		r := Result{
			RowID: row.ID,
			SHA:   row.CommitHash,
		}

		// Check 1: SHA resolves in current object database (HEAD + all branches)
		cmd := exec.Command("git", "cat-file", "-e", row.CommitHash+"^{commit}")
		cmd.Dir = path
		if err := cmd.Run(); err == nil {
			r.Exists = true
			results = append(results, r)
			continue
		}

		// Check 2: SHA exists on any unmerged branch (includes local/remote)
		cmd = exec.Command("git", "branch", "--contains", row.CommitHash)
		cmd.Dir = path
		out, err := cmd.CombinedOutput()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			r.Branch = lines[0] // first branch that contains it
			r.Exists = true     // exists but not on main
			r.Msg = fmt.Sprintf("SHA exists only on unmerged branch %s, not on main", strings.TrimSpace(lines[0]))
			results = append(results, r)
			continue
		}

		// Check 3: SHA was once valid but force-pushed away (reflog)
		cmd = exec.Command("git", "reflog", "show", "--all", "--format=%H", row.CommitHash)
		cmd.Dir = path
		reflogOut, err := cmd.CombinedOutput()
		if err == nil && len(strings.TrimSpace(string(reflogOut))) > 0 {
			r.Msg = "SHA exists only in reflog (force-pushed away; once existed)"
			results = append(results, r)
			continue
		}

		// SHA is truly unresolvable — phantom or truncated
		r.Exists = false
		r.Msg = fmt.Sprintf("SHA %s does not resolve to a commit in this repository", row.CommitHash)
		results = append(results, r)
	}

	return results, nil
}

// Summary prints a human-readable validation summary.
func Summary(results []Result) string {
	var lines []string
	total := len(results)
	unresolved := 0

	for _, r := range results {
		if r.Exists {
			if r.Branch != "" {
				lines = append(lines, fmt.Sprintf("  WARN  %s: %s (SHA %s)", r.RowID, r.Msg, r.SHA))
			} else {
				lines = append(lines, fmt.Sprintf("  OK    %s: SHA %s resolves", r.RowID, r.SHA))
			}
		} else {
			lines = append(lines, fmt.Sprintf("  FAIL  %s: %s (SHA %s)", r.RowID, r.Msg, r.SHA))
			unresolved++
		}
	}

	lines = append(lines, fmt.Sprintf("Total rows examined: %d", total))
	if unresolved > 0 {
		lines = append(lines, fmt.Sprintf("Unresolved closes: %d", unresolved))
	} else {
		lines = append(lines, "Unresolved closes: 0")
	}

	return strings.Join(lines, "\n")
}
