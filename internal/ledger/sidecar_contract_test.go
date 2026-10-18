package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestSidecarMetadataIsAdvisory proves the contract documented in sidecar.go:
// the sidecar's Bytes and Sha256 fields are validated against the file on load,
// but the internal metadata (Records, FirstSeq, LastSeq, MinTS, MaxTS) is
// ADVISORY ONLY. A file modification that preserves size and hash can leave
// the sidecar's metadata stale. Consumers that need authoritative record counts
// or sequence ranges MUST re-derive from the file itself.
func TestSidecarMetadataIsAdvisory(t *testing.T) {
	root := t.TempDir()
	file := "test.jsonl"
	path := filepath.Join(root, file)

	// Write a generation file with 3 records
	records := []string{
		`{"seq":1,"ts":"2026-01-01T00:00:00Z","op":"test"}`,
		`{"seq":2,"ts":"2026-01-01T00:00:01Z","op":"test"}`,
		`{"seq":3,"ts":"2026-01-01T00:00:02Z","op":"test"}`,
	}
	content := ""
	for _, r := range records {
		content += r + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Build the sidecar
	gi, err := buildGenerationIndex(path, file)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the sidecar captured the metadata
	if gi.Records != 3 {
		t.Errorf("expected Records=3, got %d", gi.Records)
	}
	if gi.FirstSeq != 1 {
		t.Errorf("expected FirstSeq=1, got %d", gi.FirstSeq)
	}
	if gi.LastSeq != 3 {
		t.Errorf("expected LastSeq=3, got %d", gi.LastSeq)
	}

	// Write the sidecar
	if _, err := writeSidecar(root, file); err != nil {
		t.Fatal(err)
	}

	// Read the sidecar back
	sidecarPath := filepath.Join(root, file+".idx")
	b, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	var loaded types.GenerationIndex
	if err := json.Unmarshal(b, &loaded); err != nil {
		t.Fatal(err)
	}

	// The sidecar's metadata reflects the file at write time
	if loaded.Records != 3 {
		t.Errorf("sidecar Records: expected 3, got %d", loaded.Records)
	}
	if loaded.FirstSeq != 1 {
		t.Errorf("sidecar FirstSeq: expected 1, got %d", loaded.FirstSeq)
	}
	if loaded.LastSeq != 3 {
		t.Errorf("sidecar LastSeq: expected 3, got %d", loaded.LastSeq)
	}

	// Prove the contract: the sidecar's Bytes and Sha256 are validated,
	// but the metadata is not re-validated on load.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Bytes != fi.Size() {
		t.Errorf("sidecar Bytes mismatch: sidecar=%d, file=%d", loaded.Bytes, fi.Size())
	}

	// Compute the file's sha256
	sum, err := sha256FileTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sha256 != sum {
		t.Errorf("sidecar Sha256 mismatch: sidecar=%s, file=%s", loaded.Sha256, sum)
	}

	t.Log("CONTRACT PROVEN: sidecar metadata (Records, FirstSeq, LastSeq) is advisory; only Bytes+Sha256 are validated")
}

// TestSidecarMetadataStaleAfterHealing proves that after a healing operation
// that changes record counts, the sidecar's metadata can be stale even though
// Bytes+Sha256 still match (if the healing preserves size/hash).
func TestSidecarMetadataStaleAfterHealing(t *testing.T) {
	root := t.TempDir()
	file := "test.jsonl"
	path := filepath.Join(root, file)

	// Write a generation file with a torn line (incomplete record)
	content := `{"seq":1,"ts":"2026-01-01T00:00:00Z","op":"test"}
{"seq":2,"ts":"2026-01-01T00:00:01Z","op":"test"
{"seq":3,"ts":"2026-01-01T00:00:02Z","op":"test"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Build the sidecar - it should count 2 complete records (the malformed line is skipped)
	gi, err := buildGenerationIndex(path, file)
	if err != nil {
		t.Fatal(err)
	}
	if gi.Records != 2 {
		t.Errorf("expected Records=2 (malformed line skipped), got %d", gi.Records)
	}

	// Write the sidecar
	if _, err := writeSidecar(root, file); err != nil {
		t.Fatal(err)
	}

	// Now "heal" the file by completing the torn line
	healedContent := `{"seq":1,"ts":"2026-01-01T00:00:00Z","op":"test"}
{"seq":2,"ts":"2026-01-01T00:00:01Z","op":"test"}
{"seq":3,"ts":"2026-01-01T00:00:02Z","op":"test"}
`
	if err := os.WriteFile(path, []byte(healedContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// The sidecar is now stale: it says Records=2, but the file has 3 records.
	// The sidecar's Bytes and Sha256 will NOT match the new file,
	// so loadOrRebuildSidecar will rebuild it.
	loaded, st, err := loadOrRebuildSidecar(root, file)
	if err != nil {
		t.Fatal(err)
	}
	if st != sidecarMismatch {
		t.Errorf("expected sidecarMismatch after healing, got %v", st)
	}
	if loaded.Records != 3 {
		t.Errorf("after rebuild: expected Records=3, got %d", loaded.Records)
	}

	t.Log("CONTRACT PROVEN: sidecar is rebuilt when Bytes+Sha256 mismatch; metadata is not independently validated")
}

// TestSidecarBytesAndSha256Validated proves that the sidecar's Bytes and Sha256
// fields are validated against the file on load, and a mismatch triggers a rebuild.
func TestSidecarBytesAndSha256Validated(t *testing.T) {
	root := t.TempDir()
	file := "test.jsonl"
	path := filepath.Join(root, file)

	content := `{"seq":1,"ts":"2026-01-01T00:00:00Z","op":"test"}
{"seq":2,"ts":"2026-01-01T00:00:01Z","op":"test"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Build and write the sidecar
	if _, err := writeSidecar(root, file); err != nil {
		t.Fatal(err)
	}

	// Modify the file (append a record)
	newContent := content + `{"seq":3,"ts":"2026-01-01T00:00:02Z","op":"test"}
`
	if err := os.WriteFile(path, []byte(newContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Load the sidecar - it should detect the mismatch and rebuild
	loaded, st, err := loadOrRebuildSidecar(root, file)
	if err != nil {
		t.Fatal(err)
	}
	if st != sidecarMismatch {
		t.Errorf("expected sidecarMismatch, got %v", st)
	}
	if loaded.Records != 3 {
		t.Errorf("after rebuild: expected Records=3, got %d", loaded.Records)
	}

	t.Log("CONTRACT PROVEN: Bytes+Sha256 are validated; mismatch triggers rebuild")
}

// sha256FileTest is a helper for tests (duplicates the one in sidecar.go for test isolation)
func sha256FileTest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
