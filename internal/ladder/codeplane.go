package ladder

import (
	"encoding/json"

	"github.com/trouble-agent/trouble/internal/types"
)

// codeplane.go implements SPEC-05 §3.13a: the cross-plane bundle is carried on
// admission (AC-31), persisted on the incident record, re-filled by
// convergence on a sensor-born admission (SPEC-04 §3.9a) and copied out
// verbatim to the research request (SPEC-07 §3.10a) and the issue body
// (SPEC-09 §3.13a). The bundle is context, never evidence: it changes no rung,
// no gate and no verification input (§3.10 stays untouched).

// researchSubject is the §3.10a copy-out join: the Subject handed to the
// ResearchPort carries the persisted bundle at Context["codeplane"]
// byte-for-byte, and nothing else changes shape. A nil bundle leaves the map
// without the key (a system-only incident), so the absent-bundle contract is
// exactly "key absent".
func researchSubject(st *incState) Subject {
	ctx := map[string]any{"sig": st.Inc.Sig}
	if st.Inc.Codeplane != nil {
		ctx["codeplane"] = st.Inc.Codeplane
	}
	return Subject{Slug: st.Rule, Context: ctx}
}

// CodeplaneAccessor is the sentinel's convergence accessor (SPEC-04 §3.9a,
// `CodeplaneFor`): the open-group facts of an open group whose subject the
// sensor plane also observes, or ok=false. A nil dependency (or a false
// return) leaves a sensor-born admission with its own rule context only —
// the stale-entry edge case of §3.9a.
type CodeplaneAccessor interface {
	CodeplaneFor(sig types.Sig) (types.CodeplaneContext, bool)
}

// codeplaneForSensor resolves the bundle of a sensor-born admission: the
// sensor half is built here from the firing rule (Side/RuleID/Readings), the
// sentinel half arrives through the accessor and is copied in — the two planes
// write disjoint fields, so this is a join of disjoint halves, never a merge
// decision over the same field.
func CodeplaneForSensor(accessor CodeplaneAccessor, obs Observation) *types.CodeplaneContext {
	sentinelHalf, ok := accessor.CodeplaneFor(obs.Sig)
	if !ok {
		// No convergence-map hit: the rung runs unchanged (§3.13a).
		return nil
	}
	b := sentinelHalf
	b.Side = codeplaneSideSensor
	b.RuleID = obs.Rule
	b.Readings = sensorReadings(obs)
	return &b
}

const codeplaneSideSensor = "sensor"

// sensorReadings renders the stabilized reading set of the firing rule as
// `metric → value` strings (the sensor half of the bundle). The event's
// already-scrubbed detail is the authority; only string-renderable scalars are
// carried.
func sensorReadings(obs Observation) map[string]string {
	out := map[string]string{}
	if obs.Rule != "" {
		out["rule"] = obs.Rule
	}
	if v := scalarString(obs.Detail["value"]); v != "" {
		out["value"] = v
	}
	if v := scalarString(obs.Detail["unit"]); v != "" {
		out["unit"] = v
	}
	for _, k := range []string{"some_avg10", "some_avg60", "some_avg300", "full_avg10", "full_avg60", "full_avg300"} {
		if v := scalarString(obs.Detail[k]); v != "" {
			out[k] = v
		}
	}
	if v := scalarString(obs.Detail["unit"]); v != "" {
		out["systemd_unit"] = v
	}
	return out
}

// scalarString renders a JSON scalar (string / int / float / bool) as a
// string, or "" for anything else.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return itoaL(int64(t))
	case int64:
		return itoaL(t)
	case float64:
		return jsonFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// itoaL renders a small int without pulling fmt into the hot admission path.
func itoaL(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// jsonFloat renders a float the way Go's encoder would print a float64 scalar.
func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
