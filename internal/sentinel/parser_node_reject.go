package sentinel

import (
	"regexp"
	"strings"
	"time"
)

// Parser file: node-reject (§3.5).

// nodeStart is `^UnhandledPromiseRejection`, `^[A-Za-z_$][\w$]*Error: `,
// `^Error: `, `^node:internal/`.
const nodeStart = `^(?:UnhandledPromiseRejection|[A-Za-z_$][\w$]*Error: |Error: |node:internal/)`

// nodeContinuation is the pinned continuation set of §3.5.
var nodeContinuation = []string{
	`^    at `,
	`^\s*\[cause\]: `,
	`^Caused by: `,
	`^\s+code: `,
	`^\s+at `,
	`^\s*\^`,
}

// reNodeFrame matches `    at func (/path/file.js:12:34)` and
// `    at /path/file.js:12:34` (a module frame).
var reNodeFrame = regexp.MustCompile(`^\s*at (?:async )?(.*?) \((.*?):(\d+):(\d+)\)\s*$|^\s*at (?:async )?(.*?):(\d+):(\d+)\s*$`)

// reNodeFrameFile matches `    at func (file:line:col)` (node's file:line form
// without a path, e.g. `at Object.<anonymous> (/app/x.js:1:1)`).
var reNodeFrameFile = regexp.MustCompile(`^\s*at (?:async )?(\S*) \((.*):(\d+):(\d+)\)`)

// nodeRejectParser is the builtin node-reject entry of §3.5.
func nodeRejectParser() *parserDef {
	return &parserDef{
		Name:          "node-reject",
		Kind:          "multiline",
		Level:         "error",
		StartPattern:  nodeStart,
		Continuation:  nodeContinuation,
		FlushTimeout:  150 * time.Millisecond,
		MaxEventBytes: maxEventBytesDefault,
		SigFields:     []string{"message", "frame[0].function", "frame[0].file"},
		build:         buildNodeRejectEvent,
	}
}

// buildNodeRejectEvent maps assembled Node rejection lines to an event: message
// = first line, culprit = the first `at` frame's function (or `<module>`),
// frames in source order.
func buildNodeRejectEvent(lines []logLine) *rawEvent {
	if len(lines) == 0 {
		return nil
	}
	ev := &rawEvent{Level: "error", Platform: "javascript"}
	ev.Message = strings.TrimSpace(lines[0].Text)
	for _, l := range lines {
		t := strings.TrimRight(l.Text, " \t")
		if !strings.Contains(t, "at ") && !strings.Contains(t, "Caused by:") && !strings.Contains(t, "code:") {
			continue
		}
		trimmed := strings.TrimSpace(t)
		if !strings.HasPrefix(trimmed, "at ") && !strings.Contains(trimmed, "at ") {
			continue
		}
		f, ok := parseNodeFrame(trimmed)
		if !ok {
			continue
		}
		ev.Frames = append(ev.Frames, f)
	}
	if len(ev.Frames) > 0 {
		ev.Culprit = ev.Frames[0].Function
		if ev.Culprit == "" {
			ev.Culprit = "<module>"
		}
	}
	if ev.Message == "" {
		ev.Message = "unhandled rejection"
	}
	return ev
}

// parseNodeFrame parses one `at …` frame line.
func parseNodeFrame(line string) (frame, bool) {
	// `at func (file:line:col)` / `at func (file:///path:line:col)`
	if m := reNodeFrameFile.FindStringSubmatch(line); m != nil {
		return frame{
			Function:    m[1],
			File:        m[2],
			Line:        atoiSafe(m[3]),
			InApp:       nodeInApp(m[2]),
			ContextLine: line,
		}, true
	}
	// `at file:line:col` (a module-level frame with no function name)
	t := strings.TrimSpace(strings.TrimPrefix(line, "at "))
	if i := strings.LastIndexByte(t, ':'); i > 0 {
		j := strings.LastIndexByte(t[:i], ':')
		if j > 0 {
			file := t[:j]
			ln := t[j+1 : i]
			if ln != "" && allDigits(ln) {
				return frame{File: file, Line: atoiSafe(ln), InApp: nodeInApp(file), ContextLine: line}, true
			}
		}
	}
	return frame{}, false
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// nodeInApp reports whether a JS file path is application code.
func nodeInApp(path string) bool {
	switch {
	case strings.HasPrefix(path, "node:"),
		strings.Contains(path, "/node_modules/"),
		strings.HasPrefix(path, "internal/"):
		return false
	}
	return true
}

// unused guard so regexp stays imported if the frame regexps change.
var _ = regexp.MustCompile
