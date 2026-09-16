package research

// driver_none.go — the disabled driver (SPEC-07 §2.2).
//
// `driver = "none"` is a deliberate configuration: every method fails with
// errDriverDisabled, the service writes exactly one `research` record with
// state "skipped" and skip_reason "driver_none" (TROUBLE-RESEARCH-008), and
// nothing opens a socket. It is the switch an operator flips to stop the lab
// conversation without stopping the rung.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// driverNone is the zero-behaviour driver.
type driverNone struct{}

// Name reports the driver's config value.
func (driverNone) Name() string { return types.DriverNone }

// Discover always fails: the driver is off.
func (driverNone) Discover(context.Context, types.DiscoverRequest) (types.DiscoverResponse, error) {
	return types.DiscoverResponse{}, errDriverDisabled
}

// Submit always fails: the driver is off.
func (driverNone) Submit(context.Context, types.SubmitRequest) (types.SubmitResponse, error) {
	return types.SubmitResponse{}, errDriverDisabled
}

// Poll always fails: the driver is off.
func (driverNone) Poll(context.Context, string) (types.QueueStatus, error) {
	return types.QueueStatus{}, errDriverDisabled
}

// Health always fails: the driver is off, and it makes no probe.
func (driverNone) Health(context.Context) (types.LabHealth, error) {
	return types.LabHealth{}, errDriverDisabled
}

// Stats always fails: the driver is off, and it makes no probe.
func (driverNone) Stats(context.Context) (types.LabHealth, error) {
	return types.LabHealth{}, errDriverDisabled
}
