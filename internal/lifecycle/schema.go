package lifecycle

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// CheckSchemaCompat verifies the local ledger does not contain a schema version
// newer than this binary supports, and refuses forward protocol skew
// (SPEC-12 §3.6).
func CheckSchemaCompat(stateRoot string, maxSupported int) error {
	ledgerDir := filepath.Join(stateRoot, "ledger")
	maxSeen := 0
	_ = filepath.WalkDir(ledgerDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		m, err := maxSchemaVersion(path)
		if err != nil {
			return nil
		}
		if m > maxSeen {
			maxSeen = m
		}
		return nil
	})
	if maxSeen > maxSupported {
		return fmt.Errorf("%w: ledger schema_version %d > max_supported %d", types.CodeLifecycle012, maxSeen, maxSupported)
	}
	return nil
}

func maxSchemaVersion(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	max := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			SchemaVersion int `json:"schema_version"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.SchemaVersion > max {
			max = rec.SchemaVersion
		}
	}
	return max, sc.Err()
}

// CheckForwardProtocol returns TROUBLE-LIFECYCLE-014 for unsupported protocol.
func CheckForwardProtocol(protocolVersion int) error {
	if protocolVersion != 1 {
		return fmt.Errorf("%w: hub.protocol_version %d not supported", types.CodeLifecycle014, protocolVersion)
	}
	return nil
}

// TolerantUnmarshal parses a record preserving unknown payload keys.
func TolerantUnmarshal(line []byte, rec *types.Record) error {
	if err := json.Unmarshal(line, rec); err != nil {
		return err
	}
	return nil
}

func parseSchemaVersion(s string) (int, error) {
	return strconv.Atoi(s)
}
