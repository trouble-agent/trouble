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
//     the daemon takes). Only a verb that must take the writer seat (compact)
//     is refused with TROUBLE-LEDGER-005; status and verify are read-only and
//     open no lock (SPEC-01 §2.4, docs/operations.md §2), so they must keep
//     answering while that writer is live.
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

// writeDayFixtureSeqs writes one closed-day part file carrying exactly the given
// seqs — so a fixture can hold a real seq hole — in the wire shape decodeRecord
// reads (same record shape as writeDayFixture).
func writeDayFixtureSeqs(t *testing.T, led, day string, seqs ...int) {
	t.Helper()
	var buf strings.Builder
	for _, seq := range seqs {
		rec := map[string]any{
			"rec_id":         fmt.Sprintf("ev_%024d", seq),
			"seq":            seq,
			"ts":             fmt.Sprintf("%sT00:00:%02d.000Z", day, seq),
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
// field names), 1 degraded when the INDEX is degraded (docs/operations.md §2's
// daily checks: `index.degraded=true` with a reason). Status opens no lock, so
// a live writer never turns it into a refusal: it answers with real numbers and
// the REAL writer pid read from ledger/LOCK, never its own pid (TRBL-104).
// ---------------------------------------------------------------------------

func TestLedgerStatusOkDegradedAndLiveWriterSpec01Section24(t *testing.T) {
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

	// Live-writer row: this process holds the writer seat. status opens no
	// lock (SPEC-01 §2.4), so it must still answer — exit 0, REAL numbers, and
	// writer_pid equal to the actual holder's pid, never the CLI's own.
	holdLedgerOpen(t, led)
	code, out, errOut = runCLI(t, bin, cfg, "ledger", "status", "--json")
	if code != 0 {
		t.Fatalf("status with a live writer exit = %d, want 0 (SPEC-01 §2.4: status opens no lock)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var live types.LedgerStatus
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		t.Fatalf("writer-live stanza is not LedgerStatus JSON: %v\n%s", err, out)
	}
	if live.WriterPID != os.Getpid() || !live.WriterLockHeld {
		t.Errorf("live writer status = writer_pid:%d writer_lock_held:%t, want pid %d and held=true", live.WriterPID, live.WriterLockHeld, os.Getpid())
	}
	if live.LastSeq < 2 {
		t.Errorf("writer-live status must report the real watermark: last_seq=%d, want >= 2", live.LastSeq)
	}
	// The text form answers the same way, naming the real writer pid.
	tCode, textOut, tErr := runCLI(t, bin, cfg, "ledger", "status")
	if tCode != 0 || !strings.Contains(textOut, fmt.Sprintf("writer: pid=%d", os.Getpid())) {
		t.Fatalf("text status with a live writer = %d, must name writer pid %d: %s (stderr: %s)",
			tCode, os.Getpid(), textOut, tErr)
	}

	// Degraded row: §2.4's exit 1 is the INDEX-degraded condition, not a live
	// writer. A fresh root whose day file carries a real seq hole (1 then 3)
	// makes ReadStatus report index.degraded / "seq_gaps_detected".
	base2 := ledgerTestRoot(t)
	cfg2, stateRoot2 := writeLedgerConfig(t, base2)
	led2 := filepath.Join(stateRoot2, "ledger")
	if err := os.MkdirAll(led2, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}
	writeDayFixtureSeqs(t, led2, "2026-06-05", 1, 3)
	code, out, _ = runCLI(t, bin, cfg2, "ledger", "status", "--json")
	if code != 1 {
		t.Fatalf("status on a holed ledger exit = %d, want 1 (SPEC-01 §2.4 degraded)\nstdout=%s", code, out)
	}
	var dg types.LedgerStatus
	if err := json.Unmarshal([]byte(out), &dg); err != nil {
		t.Fatalf("degraded stanza is not LedgerStatus JSON: %v\n%s", err, out)
	}
	if !dg.Index.Degraded || dg.Index.DegradedReason != "seq_gaps_detected" {
		t.Fatalf("degraded stanza must report index.degraded=true reason=seq_gaps_detected: %+v", dg.Index)
	}
}

// ---------------------------------------------------------------------------
// SPEC-01 §2.4 row 2 — verify: 0 ok, 1 findings (a corrupt interior line is a
// finding; the report names the file), 2 unreadable when the ledger ITSELF is
// unreadable (TROUBLE-LEDGER-012). The walk opens no lock, so a LIVE WRITER is
// not an unreadable ledger: verify still walks and reports (docs §2).
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

	// Live-writer row: SPEC-01 §2.4 defines verify as a read-only walk that
	// opens **no** lock, and docs/operations.md §2 promises it "is always safe
	// to run against a live ledger". A writer holding LOCK must therefore NOT
	// become a refusal: the walk still runs, and the seq hole in the
	// 2026-06-02 fixture is still the honest answer (findings, exit 1).
	holdLedgerOpen(t, led)
	code, out, liveErr := runCLI(t, bin, cfg, "ledger", "verify", "--json")
	if strings.Contains(out, "another writer holds") || strings.Contains(liveErr, "another writer holds") {
		t.Fatalf("verify refused on LOCK; §2.4 says the walk opens no lock: stdout=%s stderr=%s", out, liveErr)
	}
	if code == 2 {
		t.Fatalf("verify with a live writer = 2 (unreadable); a live writer is not an unreadable ledger. stderr=%s", liveErr)
	}
	if code != 1 {
		t.Fatalf("verify with a live writer exit = %d, want 1 (the 2026-06-02 hole is still a finding)\nstdout=%s", code, out)
	}

	// Unreadable row: §2.4's exit 2 is a ledger the walk genuinely cannot read
	// (TROUBLE-LEDGER-012), never a live writer. A fresh root whose ledger/
	// directory does not exist is the deterministic form.
	base2 := ledgerTestRoot(t)
	cfg2, _ := writeLedgerConfig(t, base2)
	code, out, unreadErr := runCLI(t, bin, cfg2, "ledger", "verify")
	if code != 2 {
		t.Fatalf("verify on an unreadable ledger exit = %d, want 2 (SPEC-01 §2.4 unreadable)\nstdout=%s\nstderr=%s", code, out, unreadErr)
	}
	if !strings.Contains(unreadErr, "TROUBLE-LEDGER-012") {
		t.Fatalf("unreadable stanza must name TROUBLE-LEDGER-012: %s", unreadErr)
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

	// Active-day 007 refusal must clean up the lock acquired by this CLI.
	// The existence probe is deliberate: wording/status alone cannot prove it.
	activeDay := time.Now().UTC().Format("2006-01-02")
	writeDayFixture(t, led, activeDay, 1)
	lockPath := filepath.Join(led, "LOCK")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("LOCK before active-day refusal exists: %v", err)
	}
	preCode, preStatus, preErr := runCLI(t, bin, cfg, "ledger", "status")
	if preCode != 0 {
		t.Fatalf("status before refusal: code=%d stdout=%s stderr=%s", preCode, preStatus, preErr)
	}
	code, _, errOut = runCLI(t, bin, cfg, "ledger", "compact", "--day", activeDay, "--dry-run")
	if code != 1 || !strings.Contains(errOut, "TROUBLE-LEDGER-007") {
		t.Fatalf("compact of active day = %d (%s), want 1 naming TROUBLE-LEDGER-007", code, errOut)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("LOCK after active-day refusal must not exist: %v", err)
	}
	postCode, postStatus, postErr := runCLI(t, bin, cfg, "ledger", "status")
	t.Logf("active-day refusal probe: refuser=TROUBLE-LEDGER-007 LOCK_before=false LOCK_after=false status_before=%q status_after=%q refusal_stderr=%q", preStatus, postStatus, errOut)
	if postCode != 0 || !strings.Contains(postStatus, "writer: pid=-1") {
		t.Fatalf("status after refusal: code=%d stdout=%s stderr=%s; want no writer", postCode, postStatus, postErr)
	}

	// A live writer's LOCK is not ours to remove. The refused CLI must leave
	// the file intact, and status must continue naming the actual holder.
	liveBase := ledgerTestRoot(t)
	liveCfg, liveStateRoot := writeLedgerConfig(t, liveBase)
	liveLedgerRoot := filepath.Join(liveStateRoot, "ledger")
	if err := os.MkdirAll(liveLedgerRoot, 0o700); err != nil {
		t.Fatalf("live ledger root: %v", err)
	}
	writeDayFixture(t, liveLedgerRoot, "2026-06-01", 1)
	holdLedgerOpen(t, liveLedgerRoot)
	liveLock := filepath.Join(liveLedgerRoot, "LOCK")
	if _, err := os.Stat(liveLock); err != nil {
		t.Fatalf("live writer LOCK before refused compact: %v", err)
	}
	liveBeforeCode, liveBefore, liveBeforeErr := runCLI(t, bin, liveCfg, "ledger", "status")
	liveCompactCode, liveCompactOut, liveCompactErr := runCLI(t, bin, liveCfg, "ledger", "compact", "--day", "2026-06-01", "--dry-run")
	liveAfterCode, liveAfter, liveAfterErr := runCLI(t, bin, liveCfg, "ledger", "status")
	if liveBeforeCode != 0 || liveAfterCode != 0 || liveCompactCode != 1 || !strings.Contains(liveCompactOut+liveCompactErr, "another writer holds") {
		t.Fatalf("live-lock refusal: before=%d/%s (%s), compact=%d/%s/%s, after=%d/%s (%s)", liveBeforeCode, liveBefore, liveBeforeErr, liveCompactCode, liveCompactOut, liveCompactErr, liveAfterCode, liveAfter, liveAfterErr)
	}
	if _, err := os.Stat(liveLock); err != nil {
		t.Fatalf("live writer LOCK was removed by refused compact: %v", err)
	}
	lockPayload, err := os.ReadFile(liveLock)
	if err != nil {
		t.Fatalf("read live LOCK after refused compact: %v", err)
	}
	var lockOwner struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(lockPayload, &lockOwner); err != nil {
		t.Fatalf("decode live LOCK after refused compact: %v", err)
	}
	if lockOwner.PID != os.Getpid() {
		t.Fatalf("LOCK payload after refused compact pid=%d, want still-live holder %d", lockOwner.PID, os.Getpid())
	}
	if !strings.Contains(liveBefore, fmt.Sprintf("writer: pid=%d", os.Getpid())) || !strings.Contains(liveAfter, fmt.Sprintf("writer: pid=%d", os.Getpid())) {
		t.Fatalf("status did not retain live writer identity: before=%s after=%s", liveBefore, liveAfter)
	}
	t.Logf("live-lock refusal probe: refuser=writer-held LOCK_before=true LOCK_after=true status_before=%q refusal=%q status_after=%q", strings.Split(strings.TrimSpace(liveBefore), "\n")[3], strings.TrimSpace(liveCompactOut), strings.Split(strings.TrimSpace(liveAfter), "\n")[3])

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

func TestLedgerStatusDoesNotReportDeadLockPID(t *testing.T) {
	bin := buildTrouble(t)
	base := ledgerTestRoot(t)
	cfg, stateRoot := writeLedgerConfig(t, base)
	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}
	const deadPID = 999999
	lockPath := filepath.Join(led, "LOCK")
	if err := os.WriteFile(lockPath, []byte("{\"pid\":999999,\"version\":\"test\"}\n"), 0o600); err != nil {
		t.Fatalf("write stale LOCK: %v", err)
	}
	code, out, errOut := runCLI(t, bin, cfg, "ledger", "status", "--json")
	if code != 0 {
		t.Fatalf("status with stale LOCK exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	var st types.LedgerStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status JSON: %v: %s", err, out)
	}
	if st.WriterPID != -1 || st.WriterLockHeld {
		t.Fatalf("unlocked stale LOCK status = writer_pid:%d writer_lock_held:%t; want -1/false", st.WriterPID, st.WriterLockHeld)
	}
	textCode, textOut, textErr := runCLI(t, bin, cfg, "ledger", "status")
	if textCode != 0 || strings.Contains(textOut, fmt.Sprintf("pid=%d", deadPID)) || !strings.Contains(textOut, "writer: pid=-1") {
		t.Fatalf("text status with stale LOCK: code=%d stdout=%s stderr=%s", textCode, textOut, textErr)
	}
	t.Logf("stale-pid status probe: LOCK_pid=%d JSON_writer_pid=%d text_writer=%q", deadPID, st.WriterPID, strings.Split(strings.TrimSpace(textOut), "\n")[3])

	// The same stale payload must not hide a kernel flock. This is the status
	// surface used by operators and must distinguish it from an unlocked stale file.
	liveBase := ledgerTestRoot(t)
	liveCfg, liveStateRoot := writeLedgerConfig(t, liveBase)
	liveRoot := filepath.Join(liveStateRoot, "ledger")
	if err := os.MkdirAll(liveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	holdLedgerOpen(t, liveRoot)
	if err := os.WriteFile(filepath.Join(liveRoot, "LOCK"), []byte(`{"pid":999999,"version":"stale"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = runCLI(t, bin, liveCfg, "ledger", "status", "--json")
	if code != 0 {
		t.Fatalf("status with live flock/stale payload: exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	var held types.LedgerStatus
	if err := json.Unmarshal([]byte(out), &held); err != nil {
		t.Fatalf("decode live-flock status: %v: %s", err, out)
	}
	if (held.WriterPID != -1 && held.WriterPID != os.Getpid()) || !held.WriterLockHeld {
		t.Fatalf("live flock with stale payload status = writer_pid:%d writer_lock_held:%t; want holder %d or unknown/-1 with held=true", held.WriterPID, held.WriterLockHeld, os.Getpid())
	}
	code, out, errOut = runCLI(t, bin, liveCfg, "ledger", "status")
	if code != 0 || !(strings.Contains(out, "writer: pid=-1 lock_held=true") || strings.Contains(out, fmt.Sprintf("writer: pid=%d lock_held=true", os.Getpid()))) || strings.Contains(out, "pid=999999") {
		t.Fatalf("text status hid live flock or exposed dead PID: exit=%d stdout=%s stderr=%s", code, out, errOut)
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
