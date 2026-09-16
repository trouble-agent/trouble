package types

// SPEC-08 error codes (TROUBLE-FLOW-001..019) and their classes, per SPEC-08 §5.
// Declared in their own file so the flow area never edits another area's
// declaration block; the classes merge into CodeClass at init.

const (
	CodeFlow001 ErrorCode = "TROUBLE-FLOW-001" // permanent: flow driver disabled by config
	CodeFlow002 ErrorCode = "TROUBLE-FLOW-002" // transient: board row append failed
	CodeFlow003 ErrorCode = "TROUBLE-FLOW-003" // permanent: post-append validation failed
	CodeFlow004 ErrorCode = "TROUBLE-FLOW-004" // permanent: board id conflict
	CodeFlow005 ErrorCode = "TROUBLE-FLOW-005" // transient: task-router dispatch failed
	CodeFlow006 ErrorCode = "TROUBLE-FLOW-006" // permanent: hot-fix lane disabled
	CodeFlow007 ErrorCode = "TROUBLE-FLOW-007" // permanent: repo not allowed
	CodeFlow008 ErrorCode = "TROUBLE-FLOW-008" // permanent: below the hot-fix threshold
	CodeFlow009 ErrorCode = "TROUBLE-FLOW-009" // permanent: one-fix-per-sig lease held
	CodeFlow010 ErrorCode = "TROUBLE-FLOW-010" // transient: router_spawn call failed
	CodeFlow011 ErrorCode = "TROUBLE-FLOW-011" // transient: spawn pending after the retry budget
	CodeFlow012 ErrorCode = "TROUBLE-FLOW-012" // permanent: disk gate refused
	CodeFlow013 ErrorCode = "TROUBLE-FLOW-013" // transient: worktree mutex timed out
	CodeFlow014 ErrorCode = "TROUBLE-FLOW-014" // permanent: repo is worktree-exempt
	CodeFlow015 ErrorCode = "TROUBLE-FLOW-015" // permanent: verify window failed
	CodeFlow016 ErrorCode = "TROUBLE-FLOW-016" // permanent: promotion denied by policy
	CodeFlow017 ErrorCode = "TROUBLE-FLOW-017" // permanent: rollback refused
	CodeFlow018 ErrorCode = "TROUBLE-FLOW-018" // permanent: project not registered/enabled
	CodeFlow019 ErrorCode = "TROUBLE-FLOW-019" // permanent: comment/cross-ref conflict
)

// FlowCodeClass is the SPEC-08 §5 class column.
var FlowCodeClass = map[ErrorCode]ErrorClass{
	CodeFlow001: ErrClassPermanent,
	CodeFlow002: ErrClassTransient,
	CodeFlow003: ErrClassPermanent,
	CodeFlow004: ErrClassPermanent,
	CodeFlow005: ErrClassTransient,
	CodeFlow006: ErrClassPermanent,
	CodeFlow007: ErrClassPermanent,
	CodeFlow008: ErrClassPermanent,
	CodeFlow009: ErrClassPermanent,
	CodeFlow010: ErrClassTransient,
	CodeFlow011: ErrClassTransient,
	CodeFlow012: ErrClassPermanent,
	CodeFlow013: ErrClassTransient,
	CodeFlow014: ErrClassPermanent,
	CodeFlow015: ErrClassPermanent,
	CodeFlow016: ErrClassPermanent,
	CodeFlow017: ErrClassPermanent,
	CodeFlow018: ErrClassPermanent,
	CodeFlow019: ErrClassPermanent,
}

func init() {
	for code, class := range FlowCodeClass {
		CodeClass[code] = class
	}
}
