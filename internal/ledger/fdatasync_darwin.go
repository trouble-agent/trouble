//go:build darwin

package ledger

import "os"

// darwin: fdatasync(2) exists in the Darwin kernel, but NEITHER Go's syscall
// package NOR golang.org/x/sys/unix exposes a binding for it (verified against
// the module cache: zsyscall_darwin_*.go carries no Fdatasync). The configured
// fdatasync=true therefore runs fsync(2) here.
//
// This is NOT a durability downgrade — fsync flushes data AND metadata, so it
// is at least as strong as fdatasync — it is the loss of the fdatasync
// optimisation (an append-only rotation pays a metadata flush too). Recorded
// here and in SPEC-12 §3.5a rather than left implicit.
func fdatasync(f *os.File, useFdatasync bool) error {
	return f.Sync()
}
