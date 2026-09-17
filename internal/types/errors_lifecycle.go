package types

// SPEC-12 error codes (TROUBLE-LIFECYCLE-001..017) — the cross-area seam. The
// lifecycle area lands with SPEC-12; the codes other areas raise (§4 of SPEC-08:
// a config refused at load, the spool budget, clock skew) are declared here so a
// foreign area never mints a code of its own.

const (
	CodeLifecycle001 ErrorCode = "TROUBLE-LIFECYCLE-001" // permanent: config file invalid
	CodeLifecycle002 ErrorCode = "TROUBLE-LIFECYCLE-002" // permanent: config precedence conflict
	CodeLifecycle003 ErrorCode = "TROUBLE-LIFECYCLE-003" // permanent: bind preflight failed
	CodeLifecycle004 ErrorCode = "TROUBLE-LIFECYCLE-004" // permanent: state root not writable
	CodeLifecycle005 ErrorCode = "TROUBLE-LIFECYCLE-005" // permanent: state root mode wrong
	CodeLifecycle006 ErrorCode = "TROUBLE-LIFECYCLE-006" // transient: unit install/repair failed
	CodeLifecycle007 ErrorCode = "TROUBLE-LIFECYCLE-007" // transient: watchdog ping missed
	CodeLifecycle008 ErrorCode = "TROUBLE-LIFECYCLE-008" // transient: heartbeat file stale
	CodeLifecycle009 ErrorCode = "TROUBLE-LIFECYCLE-009" // permanent: ledger sequence stall
	CodeLifecycle010 ErrorCode = "TROUBLE-LIFECYCLE-010" // transient: escalation hook failed
	CodeLifecycle011 ErrorCode = "TROUBLE-LIFECYCLE-011" // transient: upgrade park failed
	CodeLifecycle012 ErrorCode = "TROUBLE-LIFECYCLE-012" // permanent: schema_version downgrade
	CodeLifecycle013 ErrorCode = "TROUBLE-LIFECYCLE-013" // permanent: secret file permissions
	CodeLifecycle014 ErrorCode = "TROUBLE-LIFECYCLE-014" // permanent: forward protocol version
	CodeLifecycle015 ErrorCode = "TROUBLE-LIFECYCLE-015" // transient: spool budget exceeded
	CodeLifecycle016 ErrorCode = "TROUBLE-LIFECYCLE-016" // permanent: unit escalation wiring missing
	CodeLifecycle017 ErrorCode = "TROUBLE-LIFECYCLE-017" // transient: clock skew beyond tolerance
)

// LifecycleCodeClass is the SPEC-TYPES §5 class column for this area.
var LifecycleCodeClass = map[ErrorCode]ErrorClass{
	CodeLifecycle001: ErrClassPermanent,
	CodeLifecycle002: ErrClassPermanent,
	CodeLifecycle003: ErrClassPermanent,
	CodeLifecycle004: ErrClassPermanent,
	CodeLifecycle005: ErrClassPermanent,
	CodeLifecycle006: ErrClassTransient,
	CodeLifecycle007: ErrClassTransient,
	CodeLifecycle008: ErrClassTransient,
	CodeLifecycle009: ErrClassPermanent,
	CodeLifecycle010: ErrClassTransient,
	CodeLifecycle011: ErrClassTransient,
	CodeLifecycle012: ErrClassPermanent,
	CodeLifecycle013: ErrClassPermanent,
	CodeLifecycle014: ErrClassPermanent,
	CodeLifecycle015: ErrClassTransient,
	CodeLifecycle016: ErrClassPermanent,
	CodeLifecycle017: ErrClassTransient,
}

func init() {
	for code, class := range LifecycleCodeClass {
		CodeClass[code] = class
	}
}
