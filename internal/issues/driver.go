package issues

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Writer is SPEC-01's single-writer append port (§2). A *ledger.Ledger satisfies
// it directly, so the composition root wires the ledger with no adapter.
type Writer interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// Scanner is SPEC-01's sequential record reader (§2.4). The desk uses it once at
// boot to rebuild the anchor index and the cap counters, which is what makes a
// restart incapable of lifting a cap or duplicating an issue.
type Scanner interface {
	ScanFrom(seq uint64, yield func(types.Record) bool) error
}

// Scrubber is SPEC-02's scrub port (§2). A *scrub.Engine satisfies it directly.
// Every body and every spool payload passes through it before it is called out
// or persisted; a refusal refuses the operation.
type Scrubber interface {
	ScrubBytes(ctx context.Context, target types.ScrubTarget, projectID string, b []byte) ([]byte, types.ScrubResult, error)
}

// Clock is the dual clock of SPEC-12 §2 (SPEC-INDEX §6.5): monotonic for every
// window in this spec, wall for persisted timestamps. A nil Clock means the
// system clock.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
}

// DriverFactory builds one driver. It receives the desk (for callbacks a driver
// may need) and the shared HTTP client; the transport is the stdlib's only.
type DriverFactory func(cfg types.IssueDriverConfig, d *Desk, hc *http.Client) (types.IssueDriver, error)

var (
	drvMu       sync.RWMutex
	drvRegistry = map[string]DriverFactory{}
)

// Register wires a driver name to its factory. A name that is not registered is
// a boot-time config rejection (§2.2): internal/issues never sees it.
func Register(name string, f DriverFactory) {
	drvMu.Lock()
	defer drvMu.Unlock()
	drvRegistry[name] = f
}

// Registered lists the driver names known to this build, sorted.
func Registered() []string {
	drvMu.RLock()
	defer drvMu.RUnlock()
	out := make([]string, 0, len(drvRegistry))
	for name := range drvRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func factoryFor(name string) (DriverFactory, bool) {
	drvMu.RLock()
	defer drvMu.RUnlock()
	f, ok := drvRegistry[name]
	return f, ok
}

func init() {
	Register("github", newGitHubDriver)
	Register("duckbrain", newDuckbrainDriver)
}

// systemClock is the Clock used when the desk is built without one.
type systemClock struct{ t0 time.Time }

func (c *systemClock) Now() time.Time {
	if c.t0.IsZero() {
		c.t0 = time.Now()
	}
	return time.Now()
}

func (c *systemClock) Monotonic() time.Duration {
	if c.t0.IsZero() {
		c.t0 = time.Now()
	}
	return time.Since(c.t0)
}

// ReasonHealthProbe is the detail line of a failed probe.
// bootProbeFailures is the consecutive-failure count a failed boot probe implies.
func (d *Desk) bootProbeFailures() int {
	if d.cfg.FailAfterProbes > 1 {
		return d.cfg.FailAfterProbes
	}
	return 1
}

func orProbeError(err error, h types.DriverHealth) error {
	if err != nil {
		return err
	}
	return newErr(types.CodeIssues009, ReasonDegraded, 0, true, "boot probe degraded: %s", h.Detail)
}

func (d *Desk) hostID() string {
	if d.deps.HostID != "" {
		return d.deps.HostID
	}
	return "unknown"
}
