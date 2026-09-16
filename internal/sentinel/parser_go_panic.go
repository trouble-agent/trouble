package sentinel

// Parser file: see parsers_table.go for the table assembly and collectors.go for
// the assembler. Each parser owns its start/continuation patterns (§3.5) and the
// mapping from assembled lines to a canonical event.

import (
	"regexp"
	"strings"
	"time"
)

// goPanicStart is `^panic: `, `^\s+panic: `, `^fatal error: `.
const goPanicStart = `^(?:\s*panic: |fatal error: )`

// goPanicContinuation is `^\s+\S` (indented), `^\S+\.go:\d+`, `^\t`,
// `^\S+\(0x`, `^\s*created by `.
//
// The last entry is an addition (documented deviation #5 in
// docs/sentinel-compat.md §5): a Go dump prints frames with no arguments as a
// bare `pkg.Func()` line, which none of the pinned five patterns match — and the
// pinned END rule would then close the event at the first frame, losing the
// stack. The added pattern matches only a whitespace-free, dotted, parenthesised
// symbol line, so ordinary log lines are unaffected.
var goPanicContinuation = []string{
	`^\s+\S`,
	`^\S+\.go:\d+`,
	`^\t`,
	`^\S+\(0x`,
	`^\s*created by `,
	`^[\w./*\[\]<>-]+(?:\.[\w]+)+(?:\(.*\))?$`,
}

// reGoFuncLine matches a Go stack function line: `pkg/path.func(args)`,
// `main.main()`, `created by pkg.fn in goroutine 1`.
var reGoFuncLine = regexp.MustCompile(`^(?:created by )?([\w./*\[\]<>-]+(?:\.[\w]+)*)(?:\(.*\))?(?: in goroutine \d+)?$`)

// reGoFileLine matches `/path/file.go:118 +0x1f` (and `file.go:118`).
var reGoFileLine = regexp.MustCompile(`^(\S+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)

// goPanicParser is the builtin go-panic entry of §3.5.
func goPanicParser() *parserDef {
	return &parserDef{
		Name:          "go-panic",
		Kind:          "multiline",
		Level:         "error",
		StartPattern:  goPanicStart,
		Continuation:  goPanicContinuation,
		FlushTimeout:  250 * time.Millisecond,
		MaxEventBytes: maxEventBytesDefault,
		SigFields:     []string{"message", "frame[0..2]", "panicking_function"},
		build:         buildGoPanicEvent,
	}
}

// buildGoPanicEvent maps assembled go-panic lines to an event: source order with
// the panicking frame last, culprit = the frame immediately above `panic:`.
func buildGoPanicEvent(lines []logLine) *rawEvent {
	if len(lines) == 0 {
		return nil
	}
	ev := &rawEvent{Level: "error", Platform: "go"}
	ev.Message = strings.TrimSpace(lines[0].Text)
	for _, l := range lines {
		t := strings.TrimSpace(l.Text)
		if strings.HasPrefix(t, "fatal error:") || strings.HasPrefix(t, "panic:") {
			ev.Level = "fatal"
			break
		}
	}
	// Pair a function line with the following file line into one frame.
	panicIdx := -1
	var pendingFn string
	for i, l := range lines {
		t := strings.TrimSpace(l.Text)
		if strings.HasPrefix(t, "panic:") {
			panicIdx = i
		}
		if m := reGoFileLine.FindStringSubmatch(t); m != nil {
			f := frame{File: m[1], Line: atoiSafe(m[2]), InApp: goInApp(m[1])}
			f.Function = pendingFn
			f.ContextLine = t
			pendingFn = ""
			ev.Frames = append(ev.Frames, f)
			continue
		}
		if m := reGoFuncLine.FindStringSubmatch(t); m != nil {
			fn := m[1]
			if strings.HasPrefix(t, "created by ") {
				// `created by` marks a goroutine's creator: it is a frame, but the
				// panic-relevant frames are the crash path. It is kept as a
				// pending function so the pairing stays positional.
				pendingFn = fn
				continue
			}
			pendingFn = fn
			continue
		}
		// An indented source context line belongs to the frame above it.
		if len(ev.Frames) > 0 && strings.HasPrefix(l.Text, "\t") && !reGoFileLine.MatchString(t) {
			ev.Frames[len(ev.Frames)-1].ContextLine = t
		}
	}
	if pendingFn != "" && len(ev.Frames) > 0 {
		ev.Frames[len(ev.Frames)-1].Function = pendingFn
	}
	// A Go dump lists goroutine 1 innermost-first; the canonical frame order is
	// oldest-first with the panicking frame last (§3.5's stack-order row), which
	// is also the order an SDK reports — that is what makes one digest across
	// both paths (§4.4).
	reverseFrames(ev.Frames)
	if len(ev.Frames) > 0 {
		// The frame immediately above `panic:` is the panicking function, and
		// after the reversal it is the last frame.
		last := ev.Frames[len(ev.Frames)-1]
		ev.Culprit = last.Function
		if ev.Culprit == "" {
			ev.Culprit = last.File
		}
	}
	if panicIdx >= 0 && len(ev.Frames) > 0 {
		ev.Culprit = ev.Frames[len(ev.Frames)-1].Function
	}
	if ev.Message == "" {
		ev.Message = "panic"
	}
	return ev
}

// reverseFrames flips a frame slice in place (Go dumps are innermost-first).
func reverseFrames(f []frame) {
	for i, j := 0, len(f)-1; i < j; i, j = i+1, j-1 {
		f[i], f[j] = f[j], f[i]
	}
}

// goInApp reports whether a Go file path is application code (not stdlib, not
// the runtime, not a vendored dependency).
func goInApp(path string) bool {
	switch {
	case strings.HasPrefix(path, "/usr/local/go/"),
		strings.HasPrefix(path, "/usr/lib/go"),
		strings.Contains(path, "/runtime/"),
		strings.HasSuffix(path, "/runtime/panic.go"),
		strings.Contains(path, "/vendor/"),
		strings.Contains(path, "/go/pkg/mod/"):
		return false
	}
	return true
}
