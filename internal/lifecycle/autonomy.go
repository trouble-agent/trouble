package lifecycle

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// GatesStore reads the current autonomy gates.
type GatesStore interface {
	Gates() types.AutonomyGates
}

// AutonomyDeps is the injected dependency set for SetAutonomy.
type AutonomyDeps struct {
	Gates  GatesStore
	Writer RecordWriter
}

// SetAutonomy writes a config record with the new gates and returns them.
func SetAutonomy(ctx context.Context, d AutonomyDeps, gates types.AutonomyGates, actor types.Actor) (types.AutonomyGates, error) {
	gates.ChangedBy = actor.ID
	gates.ChangedTS = types.NowUTC()
	_, err := d.Writer.Append(ctx, types.RecordDraft{
		Kind:  types.KConfig,
		Actor: actor,
		Payload: map[string]any{
			"autonomy": gates,
		},
	})
	if err != nil {
		return types.AutonomyGates{}, err
	}
	return gates, nil
}
