package types

// Spool kind tokens (§3.7 / §3.14): the SpoolEntry kind values shared by
// internal/issues (SPEC-09 §3.7) and internal/lifecycle.
const (
	SpoolForward = "forward"
	SpoolIssue   = "issue"
	SpoolSpawn   = "spawn"
	SpoolSkill   = "skill"
)

// Gap cause tokens owned by SPEC-09/SPEC-12 (the open set lives in §3.7).
const (
	CauseDriverDown    = "driver_down"
	CauseQueueOverflow = "queue_overflow"
)
