//go:build journald_sdjournal

// SPEC-03 §3.3 documents a cgo `sdjournal` variant as the *no-subprocess* seam:
// the same follower interface implemented against libsystemd's
// sd_journal_open/sd_journal_get_data instead of a `journalctl -f` child.
//
// It is off in every v0.1 artifact (the build tag must be set explicitly) and
// this file exists so the seam is real rather than described: the interface is
// fixed here, and v1.0 hands it to an implementation without touching any
// caller. Nothing in the default build sees this symbol.
package sensors

import "errors"

// sdjournalSource is the cgo reader's handle for one follow scope.
type sdjournalSource struct {
	scope  string
	cursor string
}

// journalBackend selects the child-process follower (false) or, under this
// build tag, the sdjournal reader (true).
func journalBackend() string { return "sdjournal" }

// openSdjournal is the placeholder for the v1.0 cgo implementation. It fails
// loudly rather than pretending a subprocess-free reader exists.
func openSdjournal(scope, cursor string) (*sdjournalSource, error) {
	return nil, errors.New("sensors: journald_sdjournal build tag set but the cgo sd_journal reader is a v1.0 hand-off; it is not implemented in v0.1")
}
