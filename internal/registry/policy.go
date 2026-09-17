package registry

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// capabilityState is the host's polkit capability for the service verbs
// (SPEC-06 §3.10). It is a distinct third class on purpose: a missing policy is
// neither transient (retrying cannot fix it) nor permanent (nothing about the
// module is broken).
type capabilityState int

const (
	capUnknown capabilityState = iota
	capOK
	capMissingPolicy
)

// polkitRulesPath is the install target of contrib/polkit/49-trouble.rules.
const polkitRulesPath = "/etc/polkit-1/rules.d/49-trouble.rules"

// polkitProbe is the seam the boot probe and its tests share.
var polkitProbe = probePolkitAuthority

// PolicyCheck runs the non-mutating polkit probe and records its verdict as the
// reserved audit record (SPEC-06 §2.2, §4.4).
func (r *Registry) PolicyCheck(ctx context.Context) (types.VerifyResult, error) {
	budget := r.cfg.ProbeTimeout
	if budget <= 0 {
		budget = 2 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	unit := ""
	if len(r.cfg.ServiceUnits) > 0 {
		unit = normalizeUnit(r.cfg.ServiceUnits[0])
	}
	authorized, err := polkitProbe(pctx, r.cfg.DbusAddress, unit, "reload")

	r.capMu.Lock()
	r.capChecked = true
	r.capVerbs = []string{"reload", "restart"}
	r.capUnitsOK = append([]string(nil), r.cfg.ServiceUnits...)
	switch {
	case err != nil:
		r.capPresent = false
		r.capRulesOK = false
		r.capReason = "polkit probe failed: " + err.Error()
	case authorized:
		r.capPresent = true
		r.capRulesOK = true
		r.capReason = ""
	default:
		r.capPresent = false
		r.capRulesOK = false
		r.capReason = "polkit denied " + polkitAction + " for this user (install contrib/polkit/49-trouble.rules)"
	}
	_, statErr := os.Stat(polkitRulesPath)
	r.capRulesOK = statErr == nil
	vr := types.VerifyResult{
		OK:     r.capPresent,
		Method: "probe",
		Detail: map[string]any{
			"polkit_authorized":  r.capPresent,
			"polkit_rules_file":  polkitRulesPath,
			"rules_file_present": r.capRulesOK,
			"verbs_allowed":      r.capVerbs,
			"units_allowed":      r.capUnitsOK,
		},
	}
	r.capMu.Unlock()

	// The probe is the only way polkit state reaches the append-only ledger, so
	// it is recorded as a tool call (this area owns tool_call/play_run only).
	stages := []types.CallStage{
		{Stage: types.StageAuthorize, OK: true, Detail: "reserved audit subject"},
		{Stage: types.StageValidate, OK: true, Detail: "no args"},
		{Stage: types.StageDryRun, OK: true, Detail: fmt.Sprintf("authorized=%t", r.capPresent)},
	}
	payload := map[string]any{
		"module": CapabilityProbeModule,
		"mode":   types.ModeCheck,
		"stage":  stages,
		"result": map[string]any{"output": vr.Detail},
	}
	rec := types.Record{
		Kind:    types.KToolCall,
		Origin:  r.origin(),
		Actor:   r.deps.Actor,
		Payload: payload,
	}
	out, aerr := r.deps.Append(rec)
	if aerr != nil {
		return vr, wrapErr(types.CodeRegistry006, types.StageAudit, reasonAuditAppendFailed, aerr)
	}
	r.capMu.Lock()
	r.probeRecord = out.RecID
	r.capMu.Unlock()
	return vr, nil
}

func (r *Registry) capabilityState() capabilityState {
	r.capMu.RLock()
	defer r.capMu.RUnlock()
	if !r.capChecked {
		return capUnknown
	}
	if r.capPresent {
		return capOK
	}
	return capMissingPolicy
}

func (r *Registry) capabilityReason() string {
	r.capMu.RLock()
	defer r.capMu.RUnlock()
	if r.capReason == "" {
		return "unknown"
	}
	return r.capReason
}

// CapabilityReport is the CLI/dashboard view of the probe (trouble registry policy).
func (r *Registry) CapabilityReport() map[string]any {
	r.capMu.RLock()
	defer r.capMu.RUnlock()
	return map[string]any{
		"checked":            r.capChecked,
		"authorized":         r.capPresent,
		"rules_file":         polkitRulesPath,
		"rules_file_present": r.capRulesOK,
		"verbs_allowed":      append([]string(nil), r.capVerbs...),
		"units_allowed":      append([]string(nil), r.capUnitsOK...),
		"reason":             r.capReason,
		"probe_record":       r.probeRecord,
	}
}

// polkitAction is the only action trouble ever escalates for (SPEC-06 §3.10).
const polkitAction = "org.freedesktop.systemd1.manage-units"

// probePolkitAuthority calls org.freedesktop.PolicyKit1.Authority.CheckAuthorization
// with flags=0 (no interactive prompt): a genuinely non-mutating probe.
func probePolkitAuthority(ctx context.Context, address, unit, verb string) (bool, error) {
	conn, err := dialBus(address)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	type subject struct {
		Kind    string
		Details map[string]string
	}
	type actionInfo struct {
		ActionID string
		Details  map[string]string
	}
	subj := subject{Kind: "unix-process", Details: map[string]string{}}
	details := map[string]string{"verb": verb}
	if unit != "" {
		details["unit"] = unit
	}
	act := actionInfo{ActionID: polkitAction, Details: details}

	obj := conn.Object("org.freedesktop.PolicyKit1", "/org/freedesktop/PolicyKit1/Authority")
	call := obj.CallWithContext(ctx, "org.freedesktop.PolicyKit1.Authority.CheckAuthorization", 0,
		subj, act, uint32(0), "")
	if call.Err != nil {
		return false, call.Err
	}
	var authorized bool
	var challenge bool
	var detailsOut map[string]string
	if err := call.Store(&authorized, &challenge, &detailsOut); err != nil {
		return false, err
	}
	return authorized, nil
}

func dialBus(address string) (*dbus.Conn, error) {
	if address != "" {
		return dbus.Dial(address)
	}
	return dbus.SystemBus()
}
