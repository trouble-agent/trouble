package app

import (
	"github.com/trouble-agent/trouble/internal/ladder"
	"github.com/trouble-agent/trouble/internal/llm"
)

// llmAgentPort builds the agent stage's LLM port from the `[llm]` table
// (SPEC-05 §4.3a, declared per SPEC-12 §3.1c).
//
// No table → no port: nil is returned as a NIL INTERFACE (never as a typed-nil
// *llm.Client, which would be non-nil to the ladder and panic on the first call),
// and the agent stage refuses with TROUBLE-LADDER-021 instead of inventing a
// completion. A declared table that cannot be built is returned as an error, and
// the caller records it with TROUBLE-LIFECYCLE-001 and refuses the boot: the
// operator wrote the table, so a silent fall back to "no chain" is exactly the
// failure SPEC-12 §3.1 refuses for a scalar key.
func llmAgentPort(d *Daemon) (ladder.AgentPort, error) {
	if d == nil || !d.Cfg.LLMTable.Declared() {
		return nil, nil
	}
	cfg, err := llm.LoadConfig([]byte(d.Cfg.LLMTable.Text))
	if err != nil {
		return nil, err
	}
	client, err := llm.New(cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// ladderSkillPort adapts the built library to the ladder's §2b port.
//
// The nil case is handled HERE rather than by assigning the pointer straight into
// the interface: a (*skills.Library)(nil) stored in an interface is non-nil, and
// the ladder would call it (SPEC-05 §2b's "no library is wired" path would never be
// taken).
func ladderSkillPort(subs *Subsystems) ladder.SkillLibrary {
	if subs == nil || subs.Library == nil {
		return nil
	}
	return subs.Library
}
