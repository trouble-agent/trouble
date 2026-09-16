package flow

// config.go — the [flow] table's defaults and validation (SPEC-08 §3.3, §3.7).
//
// A config refusal here is a boot failure that names its cross-area code
// (TROUBLE-LIFECYCLE-001 for the load-time refusals §4 lists), never a rung that
// degrades quietly: an unadmitted priority class or a /tmp worktree base is a
// silent lane doing nothing, which is the failure mode this whole subsystem
// exists to avoid.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// maxConcurrentHardCap is the §3.7 hard cap: a worktree is a full checkout, so
// the cap is a disk decision and is refused above four.
const maxConcurrentHardCap = 4

// applyFlowDefaults fills every defaulted key of §3.7.
func applyFlowDefaults(c types.FlowConfig) types.FlowConfig {
	if c.Driver == "" {
		c.Driver = types.FlowDriverBoard
	}
	if c.ReviewMode == "" {
		c.ReviewMode = types.FlowReviewAuto
	}
	if c.IDPrefix == "" {
		c.IDPrefix = string(types.PTsk)
	}
	if c.RegistrationProbeEvery == "" {
		c.RegistrationProbeEvery = types.Duration("5m")
	}
	if c.RegistrationStaleMax == "" {
		c.RegistrationStaleMax = types.Duration("1h")
	}
	if c.Router.DispatchPath == "" {
		c.Router.DispatchPath = "/dispatch"
	}
	if c.Router.TokenEnv == "" {
		c.Router.TokenEnv = "TROUBLE_ROUTER_TOKEN"
	}
	if c.Router.Mode == "" {
		c.Router.Mode = "http"
	}
	if c.Router.Timeout == "" {
		c.Router.Timeout = types.Duration("10s")
	}
	if c.Router.Retries == 0 {
		c.Router.Retries = 3
	}
	h := c.Hotfix
	if h.ForemanSpawn == "" {
		h.ForemanSpawn = types.FlowSpawnRouter
	}
	if h.PriorityClass == "" {
		h.PriorityClass = "hotfix"
	}
	if h.VerifyWindow == "" {
		h.VerifyWindow = types.Duration("10m")
	}
	if h.Promote == "" {
		h.Promote = types.FlowPromoteHuman
	}
	if h.MaxConcurrent == 0 {
		h.MaxConcurrent = 2
	}
	if h.MinFreeDiskGB == 0 {
		h.MinFreeDiskGB = 10
	}
	if h.WorktreeBase == "" {
		h.WorktreeBase = ".worktrees"
	}
	if h.LeaseTTL == "" {
		h.LeaseTTL = types.Duration("30m")
	}
	if h.SpawnAckTimeout == "" {
		h.SpawnAckTimeout = types.Duration("5s")
	}
	if h.SpawnWorktreeTimeout == "" {
		h.SpawnWorktreeTimeout = types.Duration("30s")
	}
	if h.MaxAttempts == 0 {
		h.MaxAttempts = 5
	}
	if h.MinSeverity == "" {
		h.MinSeverity = types.SevHigh
	}
	if h.MutexWait == "" {
		h.MutexWait = types.Duration("5s")
	}
	if len(h.CapabilityTags) == 0 {
		h.CapabilityTags = []string{"hotfix", "trouble"}
	}
	c.Hotfix = h
	return c
}

// validateFlowConfig refuses a config that cannot work (§4, §6.5).
func validateFlowConfig(c types.FlowConfig) error {
	switch c.Driver {
	case types.FlowDriverBoard, types.FlowDriverRouter, types.FlowDriverNone:
	default:
		return &flowError{Code: types.CodeFlow001, Msg: fmt.Sprintf("unknown flow driver %q", c.Driver)}
	}
	switch c.ReviewMode {
	case types.FlowReviewAuto, types.FlowReviewReview, types.FlowReviewNever:
	default:
		return &flowError{Code: types.CodeLifecycle001, Msg: fmt.Sprintf("unknown review_mode %q", c.ReviewMode)}
	}
	h := c.Hotfix
	if h.MaxConcurrent > maxConcurrentHardCap {
		return &flowError{Code: types.CodeLifecycle001,
			Msg: fmt.Sprintf("hotfix.max_concurrent=%d exceeds the hard cap %d (a worktree is a full checkout)", h.MaxConcurrent, maxConcurrentHardCap)}
	}
	if strings.HasPrefix(h.WorktreeBase, "/") || strings.HasPrefix(h.WorktreeBase, "..") ||
		strings.Contains(h.WorktreeBase, "..") || filepath.IsAbs(h.WorktreeBase) {
		return &flowError{Code: types.CodeLifecycle001,
			Msg: fmt.Sprintf("hotfix.worktree_base=%q must be a repo-relative path without ..", h.WorktreeBase)}
	}
	if strings.HasPrefix(h.WorktreeBase, "/tmp") {
		return &flowError{Code: types.CodeLifecycle001, Msg: "a worktree under /tmp is invisible to a PrivateTmp unit"}
	}
	if reviewModeNeverWithHotfix(h, c.ReviewMode) {
		return &flowError{Code: types.CodeLifecycle001, Msg: "review_mode=never contradicts hotfix.enabled=true"}
	}
	if h.ForemanSpawn != types.FlowSpawnRouter {
		return &flowError{Code: types.CodeLifecycle001, Msg: fmt.Sprintf("foreman_spawn=%q is not supported in v0.1", h.ForemanSpawn)}
	}
	switch h.Promote {
	case types.FlowPromoteHuman, types.FlowPromoteAuto:
	default:
		return &flowError{Code: types.CodeLifecycle001, Msg: fmt.Sprintf("unknown promote mode %q", h.Promote)}
	}
	return nil
}

// reviewModeNeverWithHotfix reports the one config contradiction §3.3 names.
func reviewModeNeverWithHotfix(h types.HotfixConfig, mode string) bool {
	return h.Enabled && mode == types.FlowReviewNever
}

// severityRank orders severities for the hot-fix threshold.
func severityRank(s types.Severity) int {
	switch s {
	case types.SevCritical:
		return 0
	case types.SevHigh:
		return 1
	case types.SevMedium:
		return 2
	case types.SevLow:
		return 3
	case types.SevInfo:
		return 4
	}
	return 9
}

// severityAtLeast reports severity ≥ threshold.
func severityAtLeast(s, threshold types.Severity) bool {
	return severityRank(s) <= severityRank(threshold)
}

// priorityOf and complexityOf are the §3.3 severity maps.
func priorityOf(s types.Severity) string {
	switch s {
	case types.SevCritical:
		return "P0"
	case types.SevHigh:
		return "P1"
	case types.SevMedium:
		return "P2"
	}
	return "P3"
}

func complexityOf(s types.Severity) string {
	switch s {
	case types.SevCritical:
		return "L"
	case types.SevHigh, types.SevMedium:
		return "M"
	}
	return "S"
}

// rowStatus is the review_mode → row status map (§3.3).
func rowStatus(mode string) string {
	if mode == types.FlowReviewReview {
		return "blocked"
	}
	return "todo"
}

// reviewMode resolves the review mode with precedence project > rule > global
// (§3.3). The rule's own setting is not modelled in the row here, so the project
// override and the global value are what this function applies.
func (f *Flow) reviewMode(proj types.FlowProject, inc types.Incident) string {
	// A critical incident is always reviewed: §3.3 makes that mandatory, and the
	// per-project switch the spec also names is not modelled in v0.1's frozen
	// FlowConfig (recorded deviation).
	if inc.Severity == types.SevCritical && f.cfg.ReviewMode == types.FlowReviewAuto {
		return types.FlowReviewReview
	}
	return f.cfg.ReviewMode
}
