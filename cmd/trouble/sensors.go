package main

// `trouble sensors probe` — SPEC-03 §2 CLI verbs: "runs Probe, prints the
// capability_probe record it wrote". SPEC-03 §3.2 P9 adds the binding rule
// this verb exists for: the probe result selects a PSI mode and trigger
// arming is "never attempted speculatively again after the probe until the
// next boot or explicit `trouble sensors probe`" — this command IS the
// explicit probe. It therefore calls Probe alone: no Start, no sensor
// loops, no sampling goroutines (§4's steps 3-10 are the daemon's boot).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/sensors"
	"github.com/trouble-agent/trouble/internal/types"
)

// appendDerived publishes a derived value into the resolved key list the way
// the daemon's boot does (internal/app setResolved): replace an existing row,
// append when the key was not resolved. Source "derived" so `config explain`
// can tell it apart from a declared value.
func appendDerived(vals []types.ConfigValue, key, value string) []types.ConfigValue {
	row := types.ConfigValue{Key: key, Value: value, Source: "derived"}
	for i := range vals {
		if vals[i].Key == key {
			vals[i] = row
			return vals
		}
	}
	return append(vals, row)
}

// probeTimeout bounds one capability probe. The probe is a handful of
// host reads plus at most a few trigger-arm syscalls (SPEC-03 §3.2 P9:
// one real arm, the window ceiling walk 20s→10s→2s, one stall=0 probe);
// it has no business hanging, and a hung probe must not hang the CLI.
const probeTimeout = 30 * time.Second

// capture is the emit sink `sensors probe` builds its Sensors with: the
// probe's one capability_probe record is captured here instead of being
// written to a ledger, because the CLI never owns the daemon's ledger
// (SPEC-12 §2.1: every CLI verb is an operator action, never a daemon
// action). Exactly one record must arrive; more would mean the probe
// wrote past its contract.
type capture struct {
	records []types.Record
}

func (c *capture) emit(_ context.Context, d types.RecordDraft) (types.Record, error) {
	c.records = append(c.records, types.Record{
		Seq:           uint64(len(c.records) + 1),
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(time.Now().UTC()),
		Kind:          d.Kind,
		SchemaVersion: 1,
		Sig:           d.Sig,
		Inc:           d.Inc,
		Origin:        d.Origin,
		Actor:         d.Actor,
		Redactions:    d.Redactions,
		Payload:       d.Payload,
	})
	return c.records[len(c.records)-1], nil
}

func cmdSensors(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, sensorsUsage)
		return 2
	}
	switch args[0] {
	case "probe":
		return cmdSensorsProbe(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown sensors verb %q\n%s", args[0], sensorsUsage)
		return 2
	}
}

const sensorsUsage = "usage: trouble sensors probe [--json] [--timeout DURATION]\n" +
	"  runs the capability probe (SPEC-03 §3.2 P9) and prints the capability_probe\n" +
	"  record it wrote plus the resulting PSI mode. Exit 0 even when the probe\n" +
	"  reports a degraded capability (sampling-only, oomd absent); nonzero only\n" +
	"  when the probe itself fails.\n"

func cmdSensorsProbe(args []string) int {
	fs := flag.NewFlagSet("sensors probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "print the capability_probe record as one JSON line")
	timeout := fs.Duration("timeout", probeTimeout, "probe deadline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, code := resolve(fs.Args())
	if code != 0 {
		return code
	}
	// origin.host_id: the daemon derives it at boot and republishes it into
	// the resolved list (internal/app), because the subsystems read keys, not
	// the struct. A probe that runs out-of-band must carry the SAME identity
	// or its record cannot dedup against the daemon's ledger — so the CLI
	// runs the same derivation, through the same implementation
	// (lifecycle.StableHostID; a CLI-only re-derivation is how the two sides
	// drift, the defect class TRBL-063 records for the token store).
	hostID := res.Config.Origin.HostID
	if hostID == "" {
		hostID = lifecycle.StableHostID()
	}
	hubID := res.Config.Origin.HubID
	if hubID == "" {
		hubID = hostID // T1: a hub with zero satellites
	}
	res.Values = appendDerived(res.Values, "origin.host_id", hostID)
	res.Values = appendDerived(res.Values, "origin.hub_id", hubID)

	sink := &capture{}
	sn, err := sensors.New(res.Values, sink.emit, nil, time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sensors probe: %v\n", err)
		return 13
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := sn.Probe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sensors probe: %v\n", err)
		return 13
	}
	if len(sink.records) != 1 {
		fmt.Fprintf(os.Stderr, "sensors probe: the probe wrote %d records, want exactly 1\n", len(sink.records))
		return 13
	}
	rec := sink.records[0]

	// The record first (spec: "prints the capability_probe record it
	// wrote"), then a one-line mode summary for the operator. In --json mode
	// stdout is machine-readable ONLY — the record, one line — and the
	// summary goes to stderr (the same split every verb here draws).
	enc := json.NewEncoder(os.Stdout)
	if *asJSON {
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintf(os.Stderr, "sensors probe: %v\n", err)
			return 13
		}
		fmt.Fprintf(os.Stderr, "psi_mode=%v probe=%v\n", rec.Payload["psi_mode"], probeVerdict(rec.Payload))
		for _, c := range probeCodes(rec.Payload) {
			fmt.Fprintf(os.Stderr, "code: %s\n", c)
		}
		return 0
	}
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		fmt.Fprintf(os.Stderr, "sensors probe: %v\n", err)
		return 13
	}
	fmt.Printf("\npsi_mode=%v probe=%v\n", rec.Payload["psi_mode"], probeVerdict(rec.Payload))
	for _, c := range probeCodes(rec.Payload) {
		fmt.Printf("code: %s\n", c)
	}
	return 0
}

// probeVerdict renders the payload's own verdict ("ok" / the leading error
// code) so a degraded mode is named in words, never left to be inferred
// from a JSON field.
func probeVerdict(payload map[string]any) string {
	if code, _ := payload["error_code"].(string); code != "" {
		return code
	}
	if triggerArm, _ := payload["trigger_arm"].(string); triggerArm == "ok" {
		return "ok"
	}
	return "ok"
}

// probeCodes lists the payload's codes[] (each a TROUBLE-SENSORS-* name) so
// the degraded capabilities surface on the terminal, not only in the record.
func probeCodes(payload map[string]any) []string {
	raw, ok := payload["codes"].([]string)
	if !ok {
		return nil
	}
	return raw
}
