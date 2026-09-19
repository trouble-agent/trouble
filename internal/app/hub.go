package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/hub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// hub.go is the composition root's half of SPEC-13: it builds the light-hub
// runtime behind the resolved `[server]` profile and mounts the queue in front of
// the sentinel's ledger sink.
//
// Three properties are the reason this file is thin:
//
//   - STANDALONE IS UNTOUCHED. Nothing here runs under the default profile: no
//     connection, no goroutine, no state directory (§4.1 step 2). d.Hub is nil
//     and every call site first asks whether a runtime exists.
//   - THE QUEUE WRITES THROUGH THE SAME SINK. internal/hub's Appender seam is
//     mounted with the sentinel's own sink (`sentinelSink`), which is the path
//     that appends to the ledger AND hands a new group record to the ladder — so
//     the fan-out step of §4.2 ("local sensor (Route A direct)") is byte-identical
//     in both profiles. The profile changes the hop, not the writer.
//   - THE 200 IS BACKED BY THE LEDGER. hub.Door waits for the consumer's
//     append+fsync and returns the record the LEDGER wrote, which is what the
//     sentinel's `markFlushed` reads back (internal/sentinel/group.go). A refusal
//     (429 by the sentinel's own mapping) is what a caller sees when that cannot
//     happen, and a refusal is never a silent success.

// sentinelWriteSink is the sentinel's write seam as the composition root sees
// it: `Append` (the ledger write) plus the read surface the sentinel rebuilds
// its group index from. It mirrors internal/sentinel's unexported sink interface,
// which cannot be named from here; the compiler checks the satisfaction.
type sentinelWriteSink interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
	LastSeq() uint64
	ScanFrom(seq uint64, yield func(types.Record) bool) error
}

// hubSink mounts the light-hub queue in front of the standalone sink.
//
// It embeds the standalone sink so the read surface the sentinel also uses
// (`LastSeq`, `ScanFrom`) keeps reading the one ledger: the queue does not create
// a second read path, and the sentinel's watermark arithmetic is unchanged.
type hubSink struct {
	sentinelSink
	rt *hub.Runtime
}

// Append routes one record through the profile's plumbing.
//
// The runtime answers with a ledger record in every non-refusing mode:
//
//   - queue live → the consumer appended it (durable before the answer);
//   - degraded boot → the standalone in-process path, counted as a fallback;
//   - queue lost at runtime → an error, which the sentinel turns into 429 +
//     Retry-After (its own overload mapping) so a caller that discards on 429
//     discards rather than believing a 200 the daemon cannot honour.
func (h hubSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return h.rt.Ingest(ctx, d)
}

// hubRuntimeConfig assembles the runtime configuration from the resolved config,
// the profile gate and the daemon's own seams.
func hubRuntimeConfig(d *Daemon, hostID string, gate hub.ProfileGate, stateRoot string) hub.RuntimeConfig {
	sink := sentinelSink{L: d.Ledger, d: d}
	return hub.RuntimeConfig{
		Profile: gate.ProfileConfig(),
		Redis:   gate.Redis,
		Archive: gate.Archive,
		// The hub state tree lives under the daemon's RESOLVED state root — the
		// same absolute path the ledger was opened at (SPEC-13 §3.1: one state
		// root), never the raw configured string, which may be relative.
		StateRoot: stateRoot,
		HostID:    hostID,
		HubID:     gate.HubID,
		// The ledger writer and the in-process fallback are the SAME sink: the
		// queue is a hop in front of the writer, never a second writer.
		Ledger:     sink,
		Standalone: sink,
		AckCursor:  func() uint64 { return d.Ledger.Status().LastSeq },
		Streams:    d.hubStreams,
		Log:        func(format string, args ...any) { d.log.Warn(fmt.Sprintf(format, args...)) },
	}
}

// openHubRuntime runs SPEC-13 §4.1 steps 1-3 for a light-hub profile: the gate is
// already resolved, so this dials Redis, creates the group and wires the
// consumer. A refusal (002/003/005/015/016) is returned to the boot, which
// records it and exits 13.
func openHubRuntime(ctx context.Context, d *Daemon, hostID string, gate hub.ProfileGate, stateRoot string) (*hub.Runtime, error) {
	if !gate.Enabled {
		return nil, nil
	}
	return hub.Open(ctx, hubRuntimeConfig(d, hostID, gate, stateRoot))
}

// startHubRuntime starts the consumer, the reconnect supervisor and the archive
// timer, and writes the one lifecycle record that says which state the profile
// booted in. It is called AFTER the ladder exists and BEFORE the ingest listener
// serves a request (SPEC-13 §4.1 step 3: "the consumer goroutine starts before
// the ingestion listener accepts traffic, so no request can be 200-acked into a
// queue nobody drains").
func startHubRuntime(ctx context.Context, d *Daemon) error {
	if d.Hub == nil || !d.Hub.Enabled() {
		return nil
	}
	if err := d.Hub.Start(ctx); err != nil {
		return err
	}
	d.Hub.RecordBoot(ctx)
	return nil
}

// closeHubRuntime stops the consumer, the supervisor and the archive timer. It is
// called on the drain path, after ingest stops and before the ledger closes.
func closeHubRuntime(d *Daemon) {
	if d.Hub == nil {
		return
	}
	if err := d.Hub.Close(); err != nil {
		d.log.Warn("hub close", "err", err)
	}
}

// hubHealth reads the profile's stanza for the one health surface. The read is
// bounded: a Redis that hangs degrades the numbers, never the endpoint.
func hubHealth(d *Daemon) *types.HubStatus {
	if d == nil || d.Hub == nil || !d.Hub.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st := d.Hub.Status(ctx)
	return &st
}

// hubBootCodeOf maps an error to the TROUBLE-HUB code the boot record carries.
func hubBootCodeOf(err error) types.ErrorCode {
	if err == nil {
		return ""
	}
	if code := hub.CodeOf(err); code != "" {
		return code
	}
	var he *hub.Error
	if errors.As(err, &he) {
		return he.Code
	}
	return types.CodeHub003
}

// hubLedgerPath is where SPEC-12 §3.2 puts the ledger under a state root; the
// archival tier needs it explicitly because it lists generations itself.
func hubLedgerPath(stateRoot string) string { return filepath.Join(stateRoot, "ledger") }
