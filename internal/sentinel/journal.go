package sentinel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// journalSource supervises one `journalctl -f -o json` child (§3.5). The cursor
// is persisted per scope so a restart resumes where it stopped instead of
// replaying or skipping.
type journalSource struct {
	s          *Server
	unit       string
	cursorPath string
	cursor     string
	lastTS     string
	cmd        *exec.Cmd
}

func newJournalSource(s *Server, unit string) *journalSource {
	j := &journalSource{s: s, unit: unit}
	if s.cfg.SpoolDir != "" {
		j.cursorPath = filepath.Join(s.cfg.SpoolDir, "collectors", "journal-"+pathHash(unit)+".cursor")
		j.cursor, j.lastTS = readCursor(j.cursorPath)
	}
	return j
}

// Name is the source label ("journal:<unit>").
func (j *journalSource) Name() string { return "journal:" + j.unit }

// readCursor returns (cursor, last-ts) from a persisted cursor state file.
func readCursor(path string) (string, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	cursor := parts[0]
	last := ""
	if len(parts) > 1 {
		last = strings.TrimSpace(parts[1])
	}
	return cursor, last
}

// writeCursor persists the cursor plus the last observed timestamp (0600).
func writeCursor(path, cursor, lastTS string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cursor+"\n"+lastTS+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Lines spawns the child and streams its JSON lines.
func (j *journalSource) Lines(ctx context.Context) (<-chan logLine, <-chan error, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return nil, nil, err
	}
	args := []string{"-f", "-o", "json", "-u", j.unit}
	if j.cursor != "" {
		if !validJournalCursor(j.cursor) {
			// A malformed cursor is a documented dropout, never "no errors":
			// fall back to --since with the last timestamp and rescan.
			j.s.markGap("cursor_invalid", j.Name(), -1)
			if j.lastTS != "" {
				args = append(args, "--since", j.lastTS)
			}
		} else {
			args = append(args, "--after-cursor", j.cursor)
		}
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	j.cmd = cmd
	out := make(chan logLine, 256)
	errs := make(chan error, 4)
	go func() {
		defer close(out)
		defer close(errs)
		defer func() {
			if j.cursor != "" {
				_ = writeCursor(j.cursorPath, j.cursor, j.lastTS)
			}
		}()
		br := bufio.NewReaderSize(stdout, 256*1024)
		for {
			line, rerr := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				if ev, ok := parseJournalLine(line); ok {
					j.cursor = ev.cursor
					if ev.realTS != "" {
						j.lastTS = ev.realTS
					}
					select {
					case out <- ev.line:
					case <-ctx.Done():
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
						return
					}
					continue
				}
			}
			if rerr != nil {
				_ = cmd.Wait()
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			select {
			case errs <- err:
			default:
			}
		}
	}()
	return out, errs, nil
}

// journalEvent is one parsed journald JSON line.
type journalEvent struct {
	line   logLine
	cursor string
	realTS string
}

// parseJournalLine decodes journald's `-o json` record.
func parseJournalLine(b []byte) (journalEvent, bool) {
	var raw struct {
		Message    any    `json:"MESSAGE"`
		RealTime   string `json:"__REALTIME_TIMESTAMP"`
		Unit       string `json:"_SYSTEMD_UNIT"`
		Cursor     string `json:"__CURSOR"`
		Priority   string `json:"PRIORITY"`
		Identifier string `json:"SYSLOG_IDENTIFIER"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return journalEvent{}, false
	}
	msg := ""
	switch m := raw.Message.(type) {
	case string:
		msg = m
	case []any:
		// journald encodes non-UTF-8 MESSAGE values as an array of numbers.
		buf := make([]byte, 0, len(m))
		for _, v := range m {
			if f, ok := v.(float64); ok {
				buf = append(buf, byte(f))
			}
		}
		msg = string(buf)
	default:
		return journalEvent{}, false
	}
	ev := journalEvent{
		line:   logLine{Text: msg, Source: "journal:" + raw.Unit, Unit: raw.Unit},
		cursor: raw.Cursor,
		realTS: raw.RealTime,
	}
	if us, err := strconv.ParseInt(raw.RealTime, 10, 64); err == nil && us > 0 {
		ev.line.RealTS = time.Unix(us/1_000_000, (us%1_000_000)*1000).UTC()
		ev.line.TS = ev.line.RealTS
	}
	return ev, true
}

// validJournalCursor accepts journald's cursor shape: "s=<hex>;i=<hex>;b=<hex>;m=<hex>;t=<hex>;x=<hex>".
func validJournalCursor(c string) bool {
	if !strings.Contains(c, ";") {
		return false
	}
	fields := 0
	for _, part := range strings.Split(c, ";") {
		if part == "" {
			continue
		}
		if !strings.Contains(part, "=") {
			return false
		}
		fields++
	}
	return fields > 0
}

// Close kills the child if it is still running.
func (j *journalSource) Close() error {
	if j.cmd != nil && j.cmd.Process != nil {
		_ = j.cmd.Process.Kill()
	}
	if j.cursor != "" {
		return writeCursor(j.cursorPath, j.cursor, j.lastTS)
	}
	return nil
}
