package types

// SPEC-03 error codes (SPEC-INDEX §3.5 range 001–025, SPEC-TYPES §5 classes).
// Declared in their own file so the sensor area and the scrub area never edit
// the same declaration block; the classes merge into CodeClass at init.

const (
	CodeSensors001 ErrorCode = "TROUBLE-SENSORS-001" // permanent: kernel < 5.15
	CodeSensors002 ErrorCode = "TROUBLE-SENSORS-002" // permanent: root + EINVAL on a legal trigger write
	CodeSensors003 ErrorCode = "TROUBLE-SENSORS-003" // permanent: EBUSY (already armed)
	CodeSensors004 ErrorCode = "TROUBLE-SENSORS-004" // permanent: POLLERR on an armed fd (source gone)
	CodeSensors005 ErrorCode = "TROUBLE-SENSORS-005" // permanent: arming denied for this uid
	CodeSensors006 ErrorCode = "TROUBLE-SENSORS-006" // permanent: journalctl not found
	CodeSensors007 ErrorCode = "TROUBLE-SENSORS-007" // transient: cursor invalid
	CodeSensors008 ErrorCode = "TROUBLE-SENSORS-008" // transient: journal child exited
	CodeSensors009 ErrorCode = "TROUBLE-SENSORS-009" // permanent: no journal read access
	CodeSensors010 ErrorCode = "TROUBLE-SENSORS-010" // transient: bounded journal queue overflow
	CodeSensors011 ErrorCode = "TROUBLE-SENSORS-011" // transient: D-Bus connect failed
	CodeSensors012 ErrorCode = "TROUBLE-SENSORS-012" // policy_refused: polkit refused manage-units
	CodeSensors013 ErrorCode = "TROUBLE-SENSORS-013" // permanent: configured manager not watched
	CodeSensors014 ErrorCode = "TROUBLE-SENSORS-014" // transient: NameOwnerChanged on systemd1
	CodeSensors015 ErrorCode = "TROUBLE-SENSORS-015" // transient: Subscribe/AddMatch failed on a reachable bus
	CodeSensors016 ErrorCode = "TROUBLE-SENSORS-016" // permanent: systemd-oomd absent/masked (no-op)
	CodeSensors017 ErrorCode = "TROUBLE-SENSORS-017" // permanent: rule file schema invalid
	CodeSensors018 ErrorCode = "TROUBLE-SENSORS-018" // permanent: condition expression invalid
	CodeSensors019 ErrorCode = "TROUBLE-SENSORS-019" // transient: hot-reload failed or exceeded 1s
	CodeSensors020 ErrorCode = "TROUBLE-SENSORS-020" // transient: inotify watch limit / ENOSPC
	CodeSensors021 ErrorCode = "TROUBLE-SENSORS-021" // transient: IN_Q_OVERFLOW
	CodeSensors022 ErrorCode = "TROUBLE-SENSORS-022" // permanent: statfs failed for a configured mount
	CodeSensors023 ErrorCode = "TROUBLE-SENSORS-023" // permanent: ListTimers reply could not be parsed
	CodeSensors024 ErrorCode = "TROUBLE-SENSORS-024" // transient: sensor last-success older than its stale threshold
	CodeSensors025 ErrorCode = "TROUBLE-SENSORS-025" // permanent: sensor disabled, host capability absent
)

// SensorCodeClass is the SPEC-03 §5 class table.
var SensorCodeClass = map[ErrorCode]ErrorClass{
	CodeSensors001: ErrClassPermanent,
	CodeSensors002: ErrClassPermanent,
	CodeSensors003: ErrClassPermanent,
	CodeSensors004: ErrClassPermanent,
	CodeSensors005: ErrClassPermanent,
	CodeSensors006: ErrClassPermanent,
	CodeSensors007: ErrClassTransient,
	CodeSensors008: ErrClassTransient,
	CodeSensors009: ErrClassPermanent,
	CodeSensors010: ErrClassTransient,
	CodeSensors011: ErrClassTransient,
	CodeSensors012: ErrClassPolicyRefused,
	CodeSensors013: ErrClassPermanent,
	CodeSensors014: ErrClassTransient,
	CodeSensors015: ErrClassTransient,
	CodeSensors016: ErrClassPermanent,
	CodeSensors017: ErrClassPermanent,
	CodeSensors018: ErrClassPermanent,
	CodeSensors019: ErrClassTransient,
	CodeSensors020: ErrClassTransient,
	CodeSensors021: ErrClassTransient,
	CodeSensors022: ErrClassPermanent,
	CodeSensors023: ErrClassPermanent,
	CodeSensors024: ErrClassTransient,
	CodeSensors025: ErrClassPermanent,
}

func init() {
	for code, class := range SensorCodeClass {
		CodeClass[code] = class
	}
}
