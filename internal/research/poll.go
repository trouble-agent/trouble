package research

// poll.go — queue polling (SPEC-07 §3.5).
//
// One poll loop per submission, owned by the research worker and cancelled with
// the rung context. The lab has no webhook push (measured), so polling is the
// only result channel, and its budget — not the lab's optimistic
// `estimated_time` — ends the loop.

import (
	"hash/fnv"
	"time"
)

// pollState is one submission's poll budget.
type pollState struct {
	SubmissionID string
	Interval     time.Duration
	Started      time.Time
	Requests     int
	MaxRequests  int
	Budget       time.Duration
	UnknownStat  int // polls whose status string trouble did not recognise
}

// pollParams are the resolved knobs.
type pollParams struct {
	Interval    time.Duration
	JitterPct   float64
	Timeout     time.Duration
	MaxRequests int
}

// newPollState seeds a poll budget. The interval's jitter is deterministic per
// submission id, so two pollers for two submissions de-synchronise while a
// replay of the same submission polls on the same schedule.
func newPollState(submissionID string, p pollParams, now time.Time) *pollState {
	return &pollState{
		SubmissionID: submissionID,
		Interval:     jitteredInterval(p.Interval, p.JitterPct, submissionID),
		Started:      now,
		MaxRequests:  p.MaxRequests,
		Budget:       p.Timeout,
	}
}

// jitteredInterval is interval × (1 + (hash16(id) mod 41 − 20)/100): ±20% in 1%
// steps, stable for one submission id.
func jitteredInterval(interval time.Duration, pct float64, id string) time.Duration {
	if interval <= 0 {
		return interval
	}
	if pct <= 0 {
		return interval
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	step := int64(h.Sum32()%41) - 20 // -20..+20
	// Scale the ±20 steps by the configured percentage.
	delta := (interval.Nanoseconds() * step * int64(pct*100)) / (20 * 100 * 100)
	return interval + time.Duration(delta)
}

// nextInterval returns the wait before the next poll: the base interval, ×1.5
// after two minutes of polling, capped at 60s. Long solves hold fewer requests.
func (p *pollState) nextInterval(now time.Time) time.Duration {
	d := p.Interval
	if now.Sub(p.Started) >= 2*time.Minute {
		d = time.Duration(float64(p.Interval) * 1.5)
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// exhausted reports whether the poll budget is spent. The budget is both a wall
// cap and a request cap: whichever comes first ends the loop.
func (p *pollState) exhausted(now time.Time) bool {
	if p.Requests >= p.MaxRequests {
		return true
	}
	return now.Sub(p.Started) >= p.Budget
}
