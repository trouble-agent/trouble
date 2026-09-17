package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestCheckSchemaCompatNewer(t *testing.T) {
	dir := t.TempDir()
	ledgerDir := filepath.Join(dir, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"seq":1,"rec_id":"ev_1","ts":"2026-09-16T00:00:00Z","kind":"event","schema_version":2,"sig":"","origin":{"host_id":"h","hub_id":"","source":""},"actor":{"kind":"daemon","id":"x"},"redactions":0,"payload":{}}
`
	if err := os.WriteFile(filepath.Join(ledgerDir, "2026-09-16.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckSchemaCompat(dir, 1)
	if err == nil {
		t.Fatal("expected schema compat error")
	}
	if !containsCode(err, types.CodeLifecycle012) {
		t.Errorf("expected 012, got %v", err)
	}
}

func TestCheckForwardProtocolUnsupported(t *testing.T) {
	if err := CheckForwardProtocol(2); err == nil {
		t.Fatal("expected protocol refusal")
	}
}

func containsCode(err error, code types.ErrorCode) bool {
	return err != nil && contains(err.Error(), string(code))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || containsAt(s, sub))
}

func containsAt(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
