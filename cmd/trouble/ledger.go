package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// cmdLedger mounts SPEC-01 §2.4's operator surface on the library machinery
// internal/ledger already ships and tests: Status (§3.10), Verify (§2.2
// read-only walk) and Compact / CompactDryRun (§3.7).
//
// Exit codes are the §2.4 contract, per verb:
//
//	status : 0 ok, 1 degraded
//	verify : 0 ok, 1 findings, 2 unreadable
//	compact: 0 ok, 1 failed (a --dry-run is the ok row and writes nothing)
//
// and the CLI's own usage/config failures keep the suite-wide codes
// (2 usage, 13 refusable condition), exactly as cmdHub does.
//
// One writer per ledger (§3.4 rule 6) shapes what a SEPARATE PROCESS can see.
// The CLI never opens a ledger whose writer seat is taken: ledger.Open refuses
// with TROUBLE-LEDGER-005 and the verbs render that refusal as the verb's own
// non-ok row — the second-writer fact IS the live-ness fact, and a stanza that
// says so beats printing a zero-filled status that reads like an empty ledger.
// `compact` is additionally refused up front of any generation rewrite: it must
// never race a live writer even if the seat looked free a moment earlier.
func cmdLedger(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, ledgerUsage)
		return 2
	}
	switch args[0] {
	case "status":
		return cmdLedgerStatus(args[1:])
	case "verify":
		return cmdLedgerVerify(args[1:])
	case "compact":
		return cmdLedgerCompact(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown ledger verb %q\n\n%s", args[0], ledgerUsage)
		return 2
	}
}

const ledgerUsage = `usage:
  trouble ledger status [--json]
  trouble ledger verify [--day YYYY-MM-DD] [--json]
  trouble ledger compact --day YYYY-MM-DD [--dry-run] [--json]

SPEC-01 §2.4. status/verify are read-only and open no lock; they walk files directly even when a writer is live.
compact rewrites one day into a new generation and is never in place — pass
--dry-run to print the CompactionResult (byte deltas, ToFile) and write nothing.
compact must not race a live writer and refuses when LOCK is held.
`

// ledgerContext is what every ledger verb resolves before it acts: the resolved
// config and the ledger root the daemon opens (SPEC-12 §3.2: the ledger lives
// at <state_root>/ledger, the same join internal/app and hub drain use).
type ledgerContext struct {
	res    lifecycle.Resolved
	root   string
	hostID string
}

// resolveLedgerContext resolves config + state root, printing the suite's
// config-refusal error and returning exit 13 on failure. A root the state-root
// check cannot bless is a refusable condition here too: every ledger verb names
// files under it, and guessing a different root would report on a ledger the
// daemon does not own.
//
// TRBL-111: if --config was given explicitly but the file does not exist, refuse
// instead of silently falling back to defaults. A reporting verb that cannot
// find its config must not open the default state root and write boot notes
// into a ledger the operator did not point it at.
func resolveLedgerContext(args []string) (ledgerContext, int) {
	if cfgPathOverride != "" {
		if _, err := os.Stat(cfgPathOverride); err != nil {
			fmt.Fprintf(os.Stderr, "ledger: config not found: %s\n", cfgPathOverride)
			return ledgerContext{}, 13
		}
	}
	res, code := resolve(args)
	if code != 0 {
		return ledgerContext{}, code
	}
	rootInfo, err := lifecycle.CheckStateRoot(res.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger: %v\n", err)
		return ledgerContext{}, 13
	}
	hostID := res.Config.Origin.HostID
	if hostID == "" {
		hostID = "unknown-host"
	}
	return ledgerContext{
		res:    res,
		root:   filepath.Join(rootInfo.Path, "ledger"),
		hostID: hostID,
	}, 0
}

// openLedgerForReading opens the ledger the way every non-daemon caller does
// (hub drain's options, the daemon's defaults): briefly the writer, released by
// the caller with Close. Options are the daemon's defaults on purpose — the CLI
// must not answer questions about a ledger whose rotation/retention shape it
// invented. A TROUBLE-LEDGER-005 refusal is the live-writer signal the verbs
// render through writerHeldExit.
func openLedgerForReading(lc ledgerContext) (*ledger.Ledger, error) {
	return ledger.Open(context.Background(), ledger.Options{
		Root:           lc.root,
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         lifecycle.Actor(types.ActorDaemon, "trouble-ledger-cli"),
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         lc.hostID,
		Zone:           "loopback",
		AssertScrubbed: true,
	})
}

// openLedger separates the two failure classes the §2.4 table cares about:
// writerHeld=true (another writer holds LOCK — the verb renders its own
// degraded/unreadable/failed row) and everything else (13, refusable).
func openLedger(lc ledgerContext) (led *ledger.Ledger, writerHeld bool, err error) {
	led, err = openLedgerForReading(lc)
	if err == nil {
		return led, false, nil
	}
	if ledger.CodeOf(err) == types.CodeLedger005 {
		return nil, true, err
	}
	return nil, false, err
}

// ---------------------------------------------------------------------------
// trouble ledger status — SPEC-01 §2.4 row 1 (0 ok, 1 degraded; --json prints
// LedgerStatus)
// ---------------------------------------------------------------------------

func cmdLedgerStatus(args []string) int {
	fs := flag.NewFlagSet("ledger status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable LedgerStatus")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lc, code := resolveLedgerContext(fs.Args())
	if code != 0 {
		return code
	}
	// SPEC-01 §2.4: status is read-only. Use the read-only path that opens
	// no locks, writes nothing, and reports the actual LOCK-file PID (not self).
	// ReadStatus walks files directly — it never takes the writer seat.
	st, err := ledger.ReadStatus(lc.root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger status: %v\n", err)
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			fmt.Fprintf(os.Stderr, "ledger status: %v\n", err)
			return 13
		}
	} else {
		printLedgerStatus(st)
	}
	if st.Index.Degraded {
		return 1
	}
	return 0
}

// printLedgerStatus renders the stanza for humans: the §3.10 fields, plus the
// index line whose degraded flag is the §2.4 exit-1 condition and whose §3.8
// trigger values §4.5 promises beside their thresholds. The JSON form is the
// machine contract.
func printLedgerStatus(st types.LedgerStatus) {
	fmt.Printf("ledger: last_seq=%d last_ts=%s stall_s=%.0f loss_window_ms=%d\n", st.LastSeq, st.LastTS, st.StallS, st.LossWindowMS)
	fmt.Printf("day:    %s gen=%d part=%d file=%s bytes=%d records=%d\n", st.Day, st.Gen, st.Part, st.File, st.Bytes, st.Records)
	fmt.Printf("queue:  depth=%d cap=%d backpressure_total=%d\n", st.QueueDepth, st.QueueCap, st.BackpressureTotal)
	fmt.Printf("writer: pid=%d version=%s\n", st.WriterPID, st.WriterVersion)
	fmt.Printf("disk:   bytes=%d budget=%d (%.1f%%)\n", st.DiskBytes, st.DiskBudgetBytes,
		100*float64(st.DiskBytes)/float64(max64(st.DiskBudgetBytes, 1)))
	fmt.Printf("index:  entries=%d groups=%d cold=%d torn=%d corrupt=%d degraded=%t\n",
		st.Index.Entries, st.Index.Groups, st.Index.GroupsCold, st.Index.TornLines,
		st.Index.CorruptLines, st.Index.Degraded)
	if r := st.Index.DegradedReason; r != "" {
		fmt.Printf("index degraded: %s\n", r)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// trouble ledger verify — SPEC-01 §2.4 row 2 (0 ok, 1 findings, 2 unreadable)
// ---------------------------------------------------------------------------

func cmdLedgerVerify(args []string) int {
	fs := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	day := fs.String("day", "", "verify this day only (default: every authoritative file)")
	asJSON := fs.Bool("json", false, "machine-readable VerifyReport")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lc, code := resolveLedgerContext(fs.Args())
	if code != 0 {
		return code
	}
	// SPEC-01 §2.4: verify is read-only — opens no lock. Use the read-only
	// path that walks files directly. This never takes the writer seat.
	rep, err := ledger.ReadVerify(lc.root, *day)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger verify: %v\n", err)
		if ledger.CodeOf(err) == types.CodeLedger012 {
			return 2
		}
		return 13
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "ledger verify: %v\n", err)
			return 13
		}
	} else {
		printVerifyReport(rep)
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// printVerifyReport renders the §2.2 walk for humans; the report names the
// files it walked, which is the operations.md §2 promise ("the report names the
// files").
func printVerifyReport(rep ledger.VerifyReport) {
	state := "ok"
	if !rep.OK {
		state = "FINDINGS"
	}
	fmt.Printf("verify %s: day=%s lines=%d files=%d\n", state, rep.Day, rep.Lines, len(rep.Files))
	for _, f := range rep.Files {
		fmt.Printf("  file: %s\n", f)
	}
	fmt.Printf("seq_holes=%d torn_lines=%d corrupt_lines=%d newer_schema=%d hash_mismatches=%d tombstone_mismatches=%d\n",
		len(rep.SeqHoles), rep.TornLines, rep.CorruptLines, rep.NewerSchema,
		rep.PayloadHashMismatches, rep.TombstoneMismatches)
	for _, h := range rep.SeqHoles {
		fmt.Printf("  seq hole: %d..%d\n", h.From, h.To)
	}
	for _, e := range rep.Errors {
		fmt.Printf("  error: %s\n", e)
	}
}

// ---------------------------------------------------------------------------
// trouble ledger compact — SPEC-01 §2.4 row 3 (0 ok, 1 failed; a --dry-run
// prints CompactionResult and writes nothing)
// ---------------------------------------------------------------------------

func cmdLedgerCompact(args []string) int {
	fs := flag.NewFlagSet("ledger compact", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	day := fs.String("day", "", "the day to compact, YYYY-MM-DD (required)")
	dryRun := fs.Bool("dry-run", false, "print the CompactionResult and write nothing")
	asJSON := fs.Bool("json", false, "machine-readable CompactionResult")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *day == "" {
		fmt.Fprint(os.Stderr, "usage: trouble ledger compact --day YYYY-MM-DD [--dry-run] [--json]\n")
		return 2
	}
	lc, code := resolveLedgerContext(fs.Args())
	if code != 0 {
		return code
	}
	led, held, err := openLedger(lc)
	if held {
		// Compaction must never race the daemon's writer: the open is refused
		// by the live LOCK, and a generation rewrite behind a running writer is
		// the one shape this surface must not attempt. The failed row is exit 1.
		printWriterHeld(os.Stdout, "ledger compact", lc, *asJSON)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledger compact: %v\n", err)
		return 13
	}
	defer func() { _ = led.Close(context.Background()) }()
	var res types.CompactionResult
	if *dryRun {
		res, err = led.CompactDryRun(context.Background(), *day, ledger.DefaultRetentionPolicy())
	} else {
		res, err = led.Compact(context.Background(), *day, ledger.DefaultRetentionPolicy())
	}
	if err != nil {
		// The library's own refusal carries the §5 code (007 no files for the
		// day / writer is on that file; 009 retention blocked). §2.4's failed
		// row is exit 1 for every one of them; the message names the code.
		// A dry-run that acquired this process's own writer seat must not leave
		// its PID in LOCK after a refusal. Never touch a different owner's LOCK:
		// the live-daemon refusal returned above before we owned the seat.
		if *dryRun && ledger.CodeOf(err) == types.CodeLedger007 {
			if closeErr := led.Close(context.Background()); closeErr != nil {
				fmt.Fprintf(os.Stderr, "ledger compact: close after refusal: %v\n", closeErr)
			}
			if cleanErr := ledger.RemoveLockIfOwned(lc.root, os.Getpid()); cleanErr != nil && !os.IsNotExist(cleanErr) {
				fmt.Fprintf(os.Stderr, "ledger compact: remove owned LOCK: %v\n", cleanErr)
			}
		}
		fmt.Fprintf(os.Stderr, "ledger compact: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "ledger compact: %v\n", err)
			return 13
		}
	} else {
		printCompactionResult(res)
	}
	return 0
}

// printCompactionResult renders the §3.10 outcome for humans: day, ToFile and
// the byte deltas the §2.4 row promises the dry-run shows.
func printCompactionResult(res types.CompactionResult) {
	mode := "compacted"
	if res.DryRun {
		mode = "dry-run"
	}
	fmt.Printf("compact %s: day=%s lines_in=%d lines_out=%d bytes_in=%d bytes_out=%d delta=%d\n",
		mode, res.Day, res.LinesIn, res.LinesOut, res.BytesIn, res.BytesOut, res.BytesOut-res.BytesIn)
	fmt.Printf("records_kept=%d aggregates=%d payloads_expired=%d payloads_dropped=%d elapsed_ms=%d\n",
		res.RecordsKept, res.Aggregates, res.PayloadsExpired, res.PayloadsDropped, res.ElapsedMS)
	if res.ToFile != "" {
		fmt.Printf("to_file: %s (gen %d -> %d)\n", res.ToFile, res.FromGen, res.ToGen)
	}
}

// printWriterHeld is the shared live-daemon stanza: it names the ledger and the
// LOCK the open was refused by (§3.4 rule 6), plus the remedy. The JSON form
// carries "degraded": true so a machine reader sees the same statement the exit
// code makes — a §2.4 row must never read as a healthy ledger that happens to
// be empty.
func printWriterHeld(w io.Writer, verb string, lc ledgerContext, asJSON bool) {
	reason := fmt.Sprintf("%s: another writer holds %s (the daemon is live; run this verb after it stops or on its host)", verb, filepath.Join(lc.root, "LOCK"))
	if asJSON {
		_, _ = fmt.Fprintf(w, "{\n  \"degraded\": true,\n  \"degraded_reason\": %q\n}\n", reason)
		return
	}
	_, _ = fmt.Fprintln(w, reason)
	_, _ = fmt.Fprintf(w, "degraded: writer-live (the exit code carries this verb's degraded/unreadable/failed row)\n")
}
