//go:build windows

package issues

import "os"

// platOwnerUID is NOT APPLICABLE on Windows: ownership there is ACL-based and
// Go surfaces Win32FileAttributeData, not a Stat_t, so there is no uid to
// compare against the daemon uid. The caller records the documented not-applicable
// result instead of inventing a uid; the credential-file ownership check is a
// KNOWN GAP on Windows (SPEC-12 §3.5a).
func platOwnerUID(st os.FileInfo) (int, bool) { return 0, false }
