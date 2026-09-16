package sentinel

import (
	"regexp"
	"strings"
	"time"
)

// Parser file: py-traceback (§3.5).

// pyStart is `^Traceback \(most recent call last\):` or
// `^[A-Za-z_.]*\d*(Error|Exception|Warning|Exit|Fault): `.
const pyStart = `^(?:Traceback \(most recent call last\):|[A-Za-z_.]*\d*(?:Error|Exception|Warning|Exit|Fault): )`

// pyContinuation is the pinned continuation set of §3.5.
var pyContinuation = []string{
	`^  `,
	`^File "`,
	`^\s+\^`,
	`^raise `,
	`^During handling`,
	`^The above exception`,
	`^  \|`,
}

// rePyException matches the exception line `ExcClass: text`.
var rePyException = regexp.MustCompile(`^([A-Za-z_.]*\d*(?:Error|Exception|Warning|Exit|Fault))\s*:?\s*(.*)$`)

// rePyFile matches `  File "/path/file.py", line 118, in func`.
var rePyFile = regexp.MustCompile(`^\s*File "([^"]+)", line (\d+)(?:, in (\S+))?`)

// pyTracebackParser is the builtin py-traceback entry of §3.5.
func pyTracebackParser() *parserDef {
	return &parserDef{
		Name:          "py-traceback",
		Kind:          "multiline",
		Level:         "error",
		StartPattern:  pyStart,
		Continuation:  pyContinuation,
		FlushTimeout:  200 * time.Millisecond,
		MaxEventBytes: maxEventBytesDefault,
		SigFields:     []string{"exception_class", "frame_last.function", "frame_last.file"},
		build:         buildPyTracebackEvent,
	}
}

// buildPyTracebackEvent maps assembled traceback lines to an event: frames in
// source order (oldest first) with the raised frame last, culprit = the last
// `File "…"` frame's function, message = `ExcClass: text` of the exception line.
func buildPyTracebackEvent(lines []logLine) *rawEvent {
	if len(lines) == 0 {
		return nil
	}
	ev := &rawEvent{Level: "error", Platform: "python"}
	ev.Message = strings.TrimSpace(lines[0].Text)
	var pendingFile string
	var pendingLine int
	var pendingFunc string
	for _, l := range lines {
		t := strings.TrimRight(l.Text, " \t")
		if m := rePyFile.FindStringSubmatch(strings.TrimSpace(t)); m != nil {
			if pendingFile != "" {
				ev.Frames = append(ev.Frames, frame{File: pendingFile, Line: pendingLine, Function: pendingFunc, InApp: pyInApp(pendingFile)})
			}
			pendingFile, pendingLine, pendingFunc = m[1], atoiSafe(m[2]), m[3]
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(t), "^") && len(ev.Frames) > 0 {
			ev.Frames[len(ev.Frames)-1].ContextLine = strings.TrimSpace(t)
			continue
		}
		if m := rePyException.FindStringSubmatch(strings.TrimSpace(t)); m != nil {
			ev.ExcClass = m[1]
			ev.ExcValue = strings.TrimSpace(m[2])
			ev.Message = m[1]
			if m[2] != "" {
				ev.Message = m[1] + ": " + strings.TrimSpace(m[2])
			}
		}
	}
	if pendingFile != "" {
		ev.Frames = append(ev.Frames, frame{File: pendingFile, Line: pendingLine, Function: pendingFunc, InApp: pyInApp(pendingFile)})
	}
	if len(ev.Frames) > 0 {
		last := ev.Frames[len(ev.Frames)-1]
		ev.Culprit = last.Function
		if ev.Culprit == "" {
			ev.Culprit = last.File
		}
	}
	if ev.Message == "" {
		ev.Message = "python traceback"
	}
	return ev
}

// pyInApp reports whether a Python file path is application code.
func pyInApp(path string) bool {
	switch {
	case strings.Contains(path, "/site-packages/"),
		strings.Contains(path, "/dist-packages/"),
		strings.HasPrefix(path, "/usr/lib/python"),
		strings.Contains(path, "/lib/python3"):
		return false
	}
	return true
}
