package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

// ledger_cli_test.go mounts SPEC-01 §2.4's three operator verbs end-to-end
// through the CLI: status, verify, compact. Each test names the section row it
// pins. The build gates the acceptance this task was filed for: before the
// registration, every one of these exited 2 with `unknown command "ledger"`.
//
// Rules the tests honour (the same ones hub_cli_test.go states):
//
//   - NO LEDGER UNDER /tmp: ledger.Open refuses /tmp roots (SPEC-01 §3.9), so
//     every state root lives under ~/.local/state/trouble-test.
//   - A live writer is simulated by holding the ledger open IN THIS TEST
//     PROCESS (ledger.Open's flock on ledger/LOCK — the same §3.4 rule-6 guard
//     the daemon takes), then running the built binary, whose open must be
//     refused with TROUBLE-LEDGER-005.
func ledgerTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(troubleTestBase(t), "t100-")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeLedgerConfig writes a standalone config over base and returns its path.
func writeLedgerConfig(t *testing.T, base string) (string, string) {
	t.Helper()
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("state root: %v", err)
	}
	body := "state_root = \"" + stateRoot + "\"\n" +
		"[origin]\nhost_id = \"7f3a91c2d4e5b607\"\n" +
		"[server]\nprofile = \"standalone\"\n"
	path := filepath.Join(base, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return path, stateRoot
}

// ledgerMergeKey is the cross-source merge key for the fixture events, spelled
// the way internal/ledger.MergeKey defines it (§3.3).
func ledgerMergeKey() string {
	return ledger.MergeKey("resource_exhaustion", "io")
}

// writeDayFixture writes one valid closed-day part file: n event records,
// contiguous seq from 1, in the wire shape decodeRecord reads.
func writeDayFixture(t *testing.T, led, day string, n int) {
	t.Helper()
	var buf strings.Builder
	for i := 1; i <= n; i++ {
		rec := map[string]any{
			"rec_id":         fmt.Sprintf("ev_%024d", i),
			"seq":            i,
			"ts":             fmt.Sprintf("%sT00:00:%02d.000Z", day, i),
			"kind":           "event",
			"schema_version": ledger.SchemaVersionV1,
			"sig":            "psi:sha256v1:" + strings.Repeat("0", 40),
			"origin":         map[string]any{"host_id": "7f3a91c2d4e5b607", "source": "psi"},
			"actor":          map[string]any{"kind": "daemon", "id": "troubled", "version": "0.1.0"},
			"payload": map[string]any{
				"op":        "sample",
				"merge_key": ledgerMergeKey(),
				"subject":   "x",
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(led, day+".jsonl"), []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// holdLedgerOpen opens the ledger the way the daemon does and keeps it for the
// life of the test: the CLI's separate process then meets the §3.4 rule-6
// second-writer refusal exactly as it would against a live daemon.
func holdLedgerOpen(t *testing.T, ledRoot string) {
	t.Helper()
	l, err := ledger.Open(t.Context(), ledger.Options{
		Root:           ledRoot,
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         lifecycleTestActor(),
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         "7f3a91c2d4e5b607",
		Zone:           "loopback",
		AssertScrubbed: true,
	})
	if err != nil {
		t.Fatalf("open holder ledger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(t.Context()) })
}

// lifecycleTestActor is the holder's writer stamp (identity only; the CLI must
// never adopt it — the §5 code-005 refusal is the point).
func lifecycleTestActor() types.Actor {
	return types.Actor{
		Kind: types.ActorDaemon, ID: "trbl100-test-holder", Version: "0.1.0",
		GitSHA: "testsha", BuildTime: "2026-10-15T00:00:00.000Z",
	}
}

// ---------------------------------------------------------------------------
// SPEC-01 §2.4 — the verbs are REGISTERED (the filed defect: they exited 2
// `unknown command "ledger"`), usage names all three, unknown verbs stay exit 2.
// ---------------------------------------------------------------------------

func TestLedgerVerbsRegisteredAndUsageSpec01Section24(t *testing.T) {
	bin := buildTrouble(t)
	base := ledgerTestRoot(t)
	cfg, _ := writeLedgerConfig(t, base)

	// The filed defect, pinned: `trouble ledger <verb>` must not exit 2 with
	// "unknown command \"ledger\"" — the ledger word itself must be consumed.
	for _, args := range [][]string{
		{"ledger", "status"},
		{"ledger", "verify"},
		{"ledger", "compact", "--day", "2026-06-01"},
	} {
		code, _, errOut := runCLI(t, bin, cfg, args...)
		if code == 2 && strings.Contains(errOut, "unknown command \"ledger\"") {
			t.Fatalf("run(%v) hit the filed defect: %s", args, errOut)
		}
	}

	// Bare `ledger` and an unknown ledger verb are usage errors (exit 2) whose
	// usage names every verb the §2.4 table defines.
	for _, args := range [][]string{{"ledger"}, {"ledger", "frobnicate"}} {
		code, _, errOut := runCLI(t, bin, cfg, args...)
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2 (usage)", args, code)
		}
		for _, want := range []string{"ledger status", "ledger verify", "ledger compact"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("run(%v) usage %q does not name %q", args, errOut, want)
			}
		}
	}

	// `--help` names the ledger verb group too (the discovery surface).
	code, helpOut, _ := runCLI(t, bin, cfg, "--help")
	if code != 0 || !strings.Contains(helpOut, "trouble ledger status") {
		t.Errorf("--help exit=%d must name `trouble ledger status`:\n%s", code, helpOut)
	}

	// compact without the required --day is a usage error naming the flag.
	code, _, usageErr := runCLI(t, bin, cfg, "ledger", "compact")
	if code != 2 || !strings.Contains(usageErr, "--day") {
		t.Errorf("compact without --day = %d, want 2 naming --day: %s", code, usageErr)
	}
}

// ---------------------------------------------------------------------------
// SPEC-01 §2.4 row 1 — status: 0 ok (--json is a LedgerStatus with the §3.10
// field names), 1 degraded when a live writer holds LOCK.
// ---------------------------------------------------------------------------

func TestLedgerStatusOkAndWriterLiveDegradedSpec01Section24(t *testing.T) {
	bin := buildTrouble(t)
	base := ledgerTestRoot(t)
	cfg, stateRoot := writeLedgerConfig(t, base)
	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}

	// OK row: a ledger with content answers exit 0 and REAL numbers — the
	// fixture day's records must be visible in the §3.10 fields.
	writeDayFixture(t, led, "2026-06-01", 2)
	code, out, errOut := runCLI(t, bin, cfg, "ledger", "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit = %d, want 0 (SPEC-01 §2.4)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var st types.LedgerStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json is not LedgerStatus JSON: %v\n%s", err, out)
	}
	if st.LastSeq < 2 {
		t.Fatalf("status must report the fixture's records: last_seq=%d, want >= 2", st.LastSeq)
	}
	if st.LossWindowMS != ledger.DefaultFsyncWindowMS {
		t.Fatalf("loss_window_ms = %d, want the §2.5 default %d", st.LossWindowMS, ledger.DefaultFsyncWindowMS)
	}
	if st.DiskBudgetBytes != ledger.DefaultDiskBudgetBytes {
		t.Fatalf("disk_budget_bytes = %d, want the §2.5 default %d", st.DiskBudgetBytes, ledger.DefaultDiskBudgetBytes)
	}

	// Degraded row: a live writer (this process holds the seat) must produce
	// exit 1 with degraded:true — never a zero-filled healthy read.
	holdLedgerOpen(t, led)
	code, out, _ = runCLI(t, bin, cfg, "ledger", "status", "--json")
	if code != 1 {
		t.Fatalf("status with live writer exit = %d, want 1 (SPEC-01 §2.4 degraded)", code)
	}
	var dg struct {
		Degraded       bool   `json:"degraded"`
		DegradedReason string `json:"degraded_reason"`
	}
	if err := json.Unmarshal([]byte(out), &dg); err != nil {
		t.Fatalf("writer-live stanza is not JSON: %v\n%s", err, out)
	}
	if !dg.Degraded || !strings.Contains(dg.DegradedReason, "another writer holds") {
		t.Fatalf("writer-live stanza must name the LOCK: %+v", dg)
	}
	// The text form names the degradation too.
	tCode, textOut, tErr := runCLI(t, bin, cfg, "ledger", "status")
	if tCode != 1 || !strings.Contains(textOut, "another writer holds") {
		t.Fatalf("text status with live writer = %d, must name the held LOCK: %s (stderr: %s)", tCode, textOut, tErr)
	}
}

// ---------------------------------------------------------------------------
// SPEC-01 §2.4 row 2 — verify: 0 ok, 1 findings (a corrupt interior line is a
// finding; the report names the file), 2 unreadable when a live writer holds
// LOCK (the walk opens no lock, but reaching it needs the writer seat here).
// ---------------------------------------------------------------------------

func TestLedgerVerifyExitCodesSpec01Section24(t *testing.T) {
	bin := buildTrouble(t)

	// OK row: a healthy ledger verifies clean, exit 0.
	base := ledgerTestRoot(t)
	cfg, stateRoot := writeLedgerConfig(t, base)
	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}
	writeDayFixture(t, led, "2026-06-01", 3)
	code, out, errOut := runCLI(t, bin, cfg, "ledger", "verify", "--day", "2026-06-01", "--json")
	if code != 0 {
		t.Fatalf("verify exit = %d, want 0 (SPEC-01 §2.4)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var rep ledger.VerifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify --json is not VerifyReport JSON: %v\n%s", err, out)
	}
	if !rep.OK || rep.Lines != 3 || len(rep.Files) != 1 {
		t.Fatalf("healthy verify = ok=%v lines=%d files=%v, want ok 3 lines 1 file", rep.OK, rep.Lines, rep.Files)
	}

	// Findings row: a seq hole is a finding — exit 1, and the
	// report names the file (operations.md §2: "the report names the files").
	// Valid JSON avoids recovery healing it (tornShaped only triggers on
	// unparseable-but-truncated JSON); two valid records with a seq gap
	// guarantee a detectable seq hole (2..2).
	sec := ledgerMergeKey()
	seq1 := fmt.Sprintf("ev_%024d", 1)
	seq3 := fmt.Sprintf("ev_%024d", 3)
	// Two valid records — seq 1 and seq 3 — so Verify detects hole at seq 2.
	corruptContent := strings.Join([]string{
		`{"rec_id":"` + seq1 + `","seq":1,"ts":"2026-06-02T00:00:01.000Z","kind":"event","schema_version":1,"sig":"psi:sha256v1:` + strings.Repeat("0", 40) + `","origin":{"host_id":"7f3a91c2d4e5b607","source":"psi"},"actor":{"kind":"daemon","id":"troubled","version":"0.1.0"},"payload":{"op":"sample","merge_key":"` + sec + `","subject":"x"}}`,
		`{"rec_id":"` + seq3 + `","seq":3,"ts":"2026-06-02T00:00:03.000Z","kind":"event","schema_version":1,"sig":"psi:sha256v1:` + strings.Repeat("0", 40) + `","origin":{"host_id":"7f3a91c2d4e5b607","source":"psi"},"actor":{"kind":"daemon","id":"troubled","version":"0.1.0"},"payload":{"op":"sample","merge_key":"` + sec + `","subject":"x"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(led, "2026-06-02.jsonl"),
		[]byte(corruptContent), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	code, out, _ = runCLI(t, bin, cfg, "ledger", "verify", "--day", "2026-06-02")
	if code != 1 {
		t.Fatalf("verify on a corrupt day exit = %d, want 1 (SPEC-01 §2.4 findings)\nstdout=%s", code, out)
	}
	if !strings.Contains(out, "2026-06-02.jsonl") {
		t.Fatalf("findings report must name the file: %s", out)
	}

	// Unreadable row: a live writer makes the day unwalkable from a second
	// process — exit 2 with the LOCK named.
	holdLedgerOpen(t, led)
	code, out, _ = runCLI(t, bin, cfg, "ledger", "verify", "--json")
	if code != 2 {
		t.Fatalf("verify with live writer exit = %d, want 2 (SPEC-01 §2.4 unreadable)\nstdout=%s", code, out)
	}
	if !strings.Contains(out, "another writer holds") {
		t.Fatalf("unreadable stanza must name the LOCK: %s", out)
	}
}

// ---------------------------------------------------------------------------
// SPEC-01 §2.4 row 3 — compact: 0 ok (the --dry-run prints the
// CompactionResult with ToFile and byte deltas and writes NOTHING; a real
// compact produces the gen file and unlinks the part), 1 failed (the library's
// §5 codes named: 007 no files for the day, 009 retention blocked).
// ---------------------------------------------------------------------------

func TestLedgerCompactExitCodesAndDryRunSpec01Section24(t *testing.T) {
	bin := buildTrouble(t)
	base := ledgerTestRoot(t)
	cfg, stateRoot := writeLedgerConfig(t, base)
	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}

	// Failed row 1: a day with no files is a §5 code-007 refusal, exit 1.
	code, _, errOut := runCLI(t, bin, cfg, "ledger", "compact", "--day", "2030-01-01", "--dry-run", "--json")
	if code != 1 || !strings.Contains(errOut, "TROUBLE-LEDGER-007") {
		t.Fatalf("compact of an absent day = %d (%s), want 1 naming TROUBLE-LEDGER-007", code, errOut)
	}

	// Failed row 2: the only candidates are spine records (an all-event day
	// still inside retention_raw has nothing legal to reclaim) → §5 code 009,
	// exit 1. Counts are never sacrificed. The fresh day must NOT be the
	// writer's current file (that refusal is 007, already covered above), so
	// the fixture is dated ~48h ago while a distant day keeps the writer's
	// adoption away from it.
	writeDayFixture(t, led, "2099-01-01", 2)
	fresh := time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02")
	writeDayFixture(t, led, fresh, 2)
	code, _, errOut = runCLI(t, bin, cfg, "ledger", "compact", "--day", fresh, "--dry-run")
	if code != 1 || !strings.Contains(errOut, "TROUBLE-LEDGER-009") {
		t.Fatalf("compact of a fresh day = %d (%s), want 1 naming TROUBLE-LEDGER-009", code, errOut)
	}

	// OK dry-run row: a day past retention_raw folds cleanly. The result
	// carries ToFile and the byte deltas, dry_run=true — and the day itself is
	// untouched: no generation file exists after, and the compaction source
	// part is byte-identical. (The CLI's own OPEN writes the boot record and
	// touches HEAD/idx the way every daemon boot does — that is the ledger's
	// design, not the compaction writing; what §2.4 pins is that the DAY is
	// not rewritten and nothing named in the result appears.)
	writeDayFixture(t, led, "2026-06-01", 3)
	srcBefore, err := os.ReadFile(filepath.Join(led, "2026-06-01.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	code, out, errOut := runCLI(t, bin, cfg, "ledger", "compact", "--day", "2026-06-01", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dry-run compact exit = %d, want 0 (SPEC-01 §2.4)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var res types.CompactionResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("compact --json is not CompactionResult JSON: %v\n%s", err, out)
	}
	if !res.DryRun || res.ToFile == "" || res.LinesIn == 0 || res.BytesIn == 0 {
		t.Fatalf("dry-run result must carry dry_run, ToFile and deltas: %+v", res)
	}
	if _, serr := os.Stat(filepath.Join(led, res.ToFile)); !os.IsNotExist(serr) {
		t.Fatalf("dry-run must not produce the generation file %s (SPEC-01 §2.4: writes nothing): %v", res.ToFile, serr)
	}
	srcAfter, rerr := os.ReadFile(filepath.Join(led, "2026-06-01.jsonl"))
	if rerr != nil {
		t.Fatalf("compaction source vanished after a dry-run: %v", rerr)
	}
	if string(srcBefore) != string(srcAfter) {
		t.Fatalf("dry-run must leave the source part byte-identical (SPEC-01 §2.4)")
	}

	// OK real row: exit 0, the generation file exists, the raw part is gone,
	// and a follow-up verify stays ok (counts preserved by the aggregate).
	code, out, errOut = runCLI(t, bin, cfg, "ledger", "compact", "--day", "2026-06-01", "--json")
	if code != 0 {
		t.Fatalf("real compact exit = %d, want 0\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(led, res.ToFile)); err != nil {
		t.Fatalf("compacted generation %s missing after a real compact: %v", res.ToFile, err)
	}
	if _, err := os.Stat(filepath.Join(led, "2026-06-01.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("raw part must be unlinked after a real compact (never in place): %v", err)
	}
	code, out, _ = runCLI(t, bin, cfg, "ledger", "verify", "--day", "2026-06-01", "--json")
	if code != 0 {
		t.Fatalf("verify after compact exit = %d, want 0\nstdout=%s", code, out)
	}
	var vrep ledger.VerifyReport
	if err := json.Unmarshal([]byte(out), &vrep); err != nil {
		t.Fatalf("verify after compact: %v\n%s", err, out)
	}
	if vrep.Counts.Events != 3 {
		t.Fatalf("counts must be preserved across compaction: %+v", vrep.Counts)
	}
}

// ---------------------------------------------------------------------------
// Suite-wide codes: an unusable state root is the refusable condition (13),
// the same row every other verb prints.
// ---------------------------------------------------------------------------

func TestLedgerRefusesUnusableStateRootSpec12(t *testing.T) {
	bin := buildTrouble(t)
	base := ledgerTestRoot(t)
	cfg, stateRoot := writeLedgerConfig(t, base)
	// Break the state root: a missing root cannot be blessed by CheckStateRoot.
	if err := os.RemoveAll(stateRoot); err != nil {
		t.Fatalf("remove state root: %v", err)
	}
	for _, args := range [][]string{
		{"ledger", "status"},
		{"ledger", "verify"},
		{"ledger", "compact", "--day", "2026-06-01"},
	} {
		code, _, errOut := runCLI(t, bin, cfg, args...)
		if code != 13 {
			t.Errorf("run(%v) on a missing state root = %d, want 13\nstderr=%s", args, code, errOut)
		}
	}
}
