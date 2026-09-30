package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// StableHostID derives the host identity from /etc/machine-id when it exists
// (systemd's stable machine identity), hashed so the raw id is never written
// into a ledger record. Without it, the hostname is the last resort and it is
// marked as such by using its own bytes rather than pretending to be a
// derived id.
//
// It lives here — not in internal/app — because TWO processes must derive the
// SAME identity: the daemon at boot (internal/app) and the CLI verbs that
// build record-bearing planes out-of-band (`trouble sensors probe`,
// SPEC-03 §2). One derivation, two callers: a CLI-only re-derivation is how
// the two sides drift, the exact defect class TRBL-063 records for the
// dashboard token store path.
func StableHostID() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:8])
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		sum := sha256.Sum256([]byte("hostname:" + h))
		return hex.EncodeToString(sum[:8])
	}
	return "0000000000000000"
}
