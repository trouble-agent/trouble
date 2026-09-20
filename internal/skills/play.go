package skills

import (
	"encoding/json"

	"github.com/trouble-agent/trouble/internal/registry"
	"github.com/trouble-agent/trouble/internal/types"
)

// DecodePlay parses a play document through SPEC-06's own decoder, so the skills
// path and the registry path can never disagree about what a play is: the play
// schema is closed, one dialect, one loader.
func DecodePlay(b []byte) (types.Play, error) {
	play, err := registry.DecodePlay(b, "", nil)
	if err != nil {
		return types.Play{}, newErr(types.CodeSkills001, ReasonPlayMissing, "play decode: %v", err)
	}
	return play, nil
}

func jsonMarshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, newErr(types.CodeSkills014, ReasonStatsWrite, "encode: %v", err)
	}
	return raw, nil
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
