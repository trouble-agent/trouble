package research

// config.go — the [research] table's resolution (SPEC-07 §4.3).
//
// Every key is defaulted and every default is generic; a key this file does not
// know is ignored rather than fatal, because strict config validation belongs to
// SPEC-12. Unknown drivers, negative bounds and unparsable durations are refused
// here: a config error must fail the boot, never a rung.

import (
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// resolveConfig applies the §4.3 defaults to a raw config map.
func resolveConfig(in map[string]any) (config, error) {
	c := defaultConfig()
	for k, v := range in {
		switch k {
		case "enabled":
			c.Enabled = asBool(v, c.Enabled)
		case "driver":
			c.Driver = asString(v, c.Driver)
		case "lab_url":
			c.LabURL = asString(v, c.LabURL)
		case "lab_data_dir":
			c.LabDataDir = asString(v, c.LabDataDir)
		case "corpus_roots":
			c.CorpusRoots = asStrings(v, c.CorpusRoots)
		case "corpus_glob":
			c.CorpusGlob = asString(v, c.CorpusGlob)
		case "corpus_max_files":
			c.CorpusMaxFiles = int(asInt64(v, int64(c.CorpusMaxFiles)))
		case "corpus_max_bytes_per_file":
			c.CorpusMaxBytesPerFile = asInt64(v, c.CorpusMaxBytesPerFile)
		case "corpus_timeout":
			c.CorpusTimeout = asDur(v, c.CorpusTimeout)
		case "connect_timeout":
			c.ConnectTimeout = asDur(v, c.ConnectTimeout)
		case "request_timeout":
			c.RequestTimeout = asDur(v, c.RequestTimeout)
		case "submit_timeout":
			c.SubmitTimeout = asDur(v, c.SubmitTimeout)
		case "poll_interval":
			c.PollInterval = asDur(v, c.PollInterval)
		case "poll_jitter_pct":
			c.PollJitterPct = asFloat(v, c.PollJitterPct)
		case "poll_timeout":
			c.PollTimeout = asDur(v, c.PollTimeout)
		case "poll_max_requests":
			c.PollMaxRequests = int(asInt64(v, int64(c.PollMaxRequests)))
		case "duplicate_recheck_interval":
			c.DuplicateRecheckInterval = asDur(v, c.DuplicateRecheckInterval)
		case "queue_depth_skip":
			c.QueueDepthSkip = asInt64(v, c.QueueDepthSkip)
		case "max_inflight":
			c.MaxInflight = int(asInt64(v, int64(c.MaxInflight)))
		case "cooldown_failures":
			c.CooldownFailures = int(asInt64(v, int64(c.CooldownFailures)))
		case "cooldown":
			c.Cooldown = asDur(v, c.Cooldown)
		case "health_probe_interval":
			c.HealthProbeInterval = asDur(v, c.HealthProbeInterval)
		case "capability_probe_interval":
			c.CapabilityProbeInterval = asDur(v, c.CapabilityProbeInterval)
		case "submit_fuse_400s":
			c.SubmitFuse400s = int(asInt64(v, int64(c.SubmitFuse400s)))
		case "brief_max_bytes":
			c.BriefMaxBytes = int(asInt64(v, int64(c.BriefMaxBytes)))
		case "prompt_brief_max_bytes":
			c.PromptBriefMaxBytes = int(asInt64(v, int64(c.PromptBriefMaxBytes)))
		case "prompt_max_bytes":
			c.PromptMaxBytes = int(asInt64(v, int64(c.PromptMaxBytes)))
		case "requests_per_day":
			c.RequestsPerDay = asInt64(v, c.RequestsPerDay)
		case "cadence":
			c.Cadence = asString(v, c.Cadence)
		case "cadence_ladder":
			c.CadenceLadder = asStrings(v, c.CadenceLadder)
		case "fallback_slug":
			c.FallbackSlug = asString(v, c.FallbackSlug)
		case "allow_unknown_class_submit":
			c.AllowUnknownClassSubmit = asBool(v, c.AllowUnknownClassSubmit)
		case "apply_brief_as_play":
			c.ApplyBriefAsPlay = asBool(v, c.ApplyBriefAsPlay)
		case "table":
			c.Table = asString(v, c.Table)
		case "unit_kind":
			c.UnitKind = asStringMap(v)
		case "container_markers":
			c.ContainerMarkers = asStrings(v, c.ContainerMarkers)
		case "agent_tokens_saved_in":
			c.AgentTokensSavedIn = asInt64(v, c.AgentTokensSavedIn)
		case "agent_tokens_saved_out":
			c.AgentTokensSavedOut = asInt64(v, c.AgentTokensSavedOut)
		case "webhook_url":
			c.WebhookURL = asString(v, c.WebhookURL)
		case "webhook_poll_url":
			c.WebhookPollURL = asString(v, c.WebhookPollURL)
		case "host_id":
			c.HostID = asString(v, c.HostID)
		case "daemon_version":
			c.DaemonVersion = asString(v, c.DaemonVersion)
		default:
			// An unknown key is ignored (SPEC-12 owns strict validation), so a
			// newer config file still loads on an older binary.
		}
	}
	if c.PollMaxRequests < 1 {
		return c, fmt.Errorf("research: poll_max_requests must be ≥ 1")
	}
	if c.MaxInflight < 1 {
		return c, fmt.Errorf("research: max_inflight must be ≥ 1")
	}
	if c.BriefMaxBytes < 0 || c.PromptBriefMaxBytes < 0 || c.PromptMaxBytes < 0 {
		return c, fmt.Errorf("research: byte caps must not be negative")
	}
	if len(c.CadenceLadder) == 0 {
		c.CadenceLadder = []string{c.Cadence}
	}
	if c.Driver == types.DriverOffByOne && c.LabURL == "" {
		return c, fmt.Errorf("research: driver off-by-one requires research.lab_url")
	}
	if c.CorpusTimeout < 0 || c.PollTimeout < 0 || c.PollInterval < 0 {
		return c, fmt.Errorf("research: durations must not be negative")
	}
	return c, nil
}

// ReasonOfCode maps a SPEC-07 §5 code onto its pinned degrade reason. It is the
// inverse of the §5 table and keeps reason strings out of call sites.
func ReasonOfCode(code types.ErrorCode) string {
	switch code {
	case types.CodeResearch001:
		return types.ResReasonLabUnreachable
	case types.CodeResearch002:
		return types.ResReasonStrictDecoder
	case types.CodeResearch004, types.CodeResearch010:
		return types.ResReasonSolverUnavailable
	case types.CodeResearch006:
		return types.ResReasonPollTimeout
	}
	return ""
}
