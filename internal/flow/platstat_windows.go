//go:build windows

package flow

import "os"

// platFileID is NOT APPLICABLE on Windows: there is no (device, inode) pair in
// syscall.Stat_t — Windows identifies files by file index on a volume and Go
// surfaces Win32FileAttributeData, not a Stat_t. The documented result is
// (0, 0), which makes the board writer's stamp comparison fall back to
// (size, mtime) alone. This is a KNOWN WEAKER change detector on Windows: a
// same-size same-mtime replacement of the board file is not detected by the
// stamp, and the reader path must rely on re-reading the file (SPEC-12 §3.5a).
func platFileID(st os.FileInfo) (uint64, uint64) { return 0, 0 }
