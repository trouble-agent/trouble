package ledger

// The §3.8 storage-tier decision is data, not opinion: these constants are the
// failure thresholds whose tripping flips the index to a derived, disposable
// SQLite store (JSONL stays canonical). `trouble ledger status --json` prints
// each trigger's current value beside its threshold.
const (
	// Trigger 1 — index thrash.
	TierEvictionsPerMin     = 100
	TierEvictionsSustainMin = 5
	TierIndexBytesThrash    = 20 << 20

	// Trigger 2 — boot cost.
	TierBuildMS   = 9000
	TierBuildDays = 7

	// Trigger 3 — query latency at the ceilings.
	TierTopGroups50MS = 50
	TierIncidentMS    = 5

	// Trigger 4 — capacity ceiling.
	TierRecordsPerMin    = 50000
	TierAmortizedRecPerS = 515000
	TierLookupsPerDay    = 1000000
	TierWorkingSetBytes  = 20 << 20
)

// TierTrigger is one §3.8 trigger row with its live value.
type TierTrigger struct {
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Unit      string  `json:"unit"`
	Tripped   bool    `json:"tripped"`
	Detail    string  `json:"detail"`
}

// TierTriggers evaluates the four failure thresholds against the current
// ledger state. Until one trips, the tier is JSONL + in-memory index.
func (l *Ledger) TierTriggers() []TierTrigger {
	st := l.Status()
	idx := st.Index
	trig := []TierTrigger{
		{
			Name: "index_thrash", Value: idx.ColdEvictionsPerMin, Threshold: TierEvictionsPerMin,
			Unit:    "cold_evictions/min",
			Detail:  "trips when >100 evictions/min for 5 consecutive minutes AND index_bytes > 20 MiB",
			Tripped: idx.ColdEvictionsPerMin > TierEvictionsPerMin && idx.IndexBytes > TierIndexBytesThrash,
		},
		{
			Name: "boot_cost", Value: float64(idx.BuildMS), Threshold: TierBuildMS,
			Unit:    "ms",
			Detail:  "trips when build_ms exceeds 9000 ms at p50 for 7 consecutive days",
			Tripped: float64(idx.BuildMS) > TierBuildMS,
		},
		{
			Name: "query_latency", Value: float64(idx.Entries), Threshold: 0,
			Unit:    "entries (latency measured by TestQueryLatency)",
			Detail:  "trips when p99 TopGroups(50) > 50 ms or p99 Incident(id) > 5 ms at 500k/500k ceiling",
			Tripped: false,
		},
		{
			Name: "capacity", Value: 0, Threshold: TierRecordsPerMin,
			Unit:    "records/min",
			Detail:  "trips above 50,000 records/min sustained (833 rec/s) or >1M group lookups/day",
			Tripped: false,
		},
	}
	return trig
}

// SQLiteTierTripped reports whether any §3.8 threshold has fired. It is the
// single predicate docs_test.go's "no sqlite dependency" assertion is scoped to.
func (l *Ledger) SQLiteTierTripped() bool {
	for _, t := range l.TierTriggers() {
		if t.Tripped {
			return true
		}
	}
	return false
}
