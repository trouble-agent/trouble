//go:build windows

package sentinel

import "os"

// platformFileIdentity is NOT APPLICABLE on Windows: there is no (dev, ino)
// pair to read (Go surfaces Win32FileAttributeData, not a Stat_t). The caller
// falls back to the modification time alone, which is a KNOWN WEAKER rotation
// detector — a replaced file that keeps its mtime is not detected
// (SPEC-12 §3.5a).
func platformFileIdentity(st os.FileInfo) (uint64, uint64, bool) { return 0, 0, false }
