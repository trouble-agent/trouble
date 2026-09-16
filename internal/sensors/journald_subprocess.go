//go:build !journald_sdjournal

package sensors

// SPEC-03 §3.3: the v0.1 artifacts always use the supervised `journalctl -f`
// child. The sdjournal cgo variant lives behind the `journald_sdjournal` build
// tag (see journald_sdjournal.go) and is a v1.0 hand-off.

// journalBackend names the active journald reader so /health and the probe
// record never have to guess which one is running.
func journalBackend() string { return "subprocess" }
