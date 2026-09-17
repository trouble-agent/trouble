package ledger

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLossWindowStatement: the §2.1 sentence appears verbatim in README.md,
// docs/operations.md, the `trouble --help` ledger section and GET /health.json
// (ledger_loss_window_ms). Comparisons are whitespace-normalized so a
// line-wrapped paragraph still has to carry the exact sentence.
func TestLossWindowStatement(t *testing.T) {
	want := normalizeWS(LossWindowStatement)
	root := filepath.Join("..", "..")
	for _, f := range []string{filepath.Join(root, "README.md"), filepath.Join(root, "docs", "operations.md")} {
		body, err := readWholeFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(normalizeWS(body), want) {
			t.Errorf("%s does not carry the verbatim loss-window statement", f)
		}
	}
	if !strings.Contains(normalizeWS(HelpLedgerSection()), want) {
		t.Errorf("HelpLedgerSection() does not carry the verbatim loss-window statement")
	}
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	fields := l.HealthLedgerFields()
	got, ok := fields["ledger_loss_window_ms"]
	if !ok {
		t.Fatalf("HealthLedgerFields() has no ledger_loss_window_ms")
	}
	if got != l.opts.Rotation.FsyncWindowMS {
		t.Errorf("ledger_loss_window_ms = %v, want %d", got, l.opts.Rotation.FsyncWindowMS)
	}
	if _, ok := fields["ledger_last_seq"]; !ok {
		t.Errorf("HealthLedgerFields() has no ledger_last_seq")
	}
}

// TestSqliteNotLinked: `go list -deps ./internal/ledger` contains no
// modernc.org/sqlite while the §3.8 triggers are un-tripped.
func TestSqliteNotLinked(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not on PATH")
	}
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	if l.SQLiteTierTripped() {
		t.Skip("a §3.8 trigger is tripped: the sqlite tier is legitimate")
	}
	out, err := exec.Command("go", "list", "-deps", "./internal/ledger").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v (%s)", err, out)
	}
	if strings.Contains(string(out), "modernc.org/sqlite") {
		t.Fatalf("modernc.org/sqlite is linked while no §3.8 trigger has fired")
	}
	_ = context.Background()
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func readWholeFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
