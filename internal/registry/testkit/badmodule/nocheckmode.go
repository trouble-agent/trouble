package badmodule

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// NoCheckArgs is the args schema of the check_mode-less negative module.
type NoCheckArgs struct {
	Path string `json:"path"`
}

// NoCheckMode is the deliberate defect of SPEC-06 §2.4: it declares
// Mutating=true with CheckMode=false, which no mutating module may do
// (AC-23 condition 2). registry.Register must refuse it with
// TROUBLE-REGISTRY-014 at registration, not at call time, so a module without a
// dry run can never reach a target.
type NoCheckMode struct{}

var noCheckModeDescriptor = types.Descriptor{
	Name:        "badmodule.nocheckmode",
	Version:     1,
	Schema:      schemagen.Generate("badmodule.nocheckmode", 1, NoCheckArgs{}),
	Scopes:      []string{"file:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   false,
	TimeoutS:    5,
	Mutating:    true,
}

// Descriptor implements types.Module.
func (m NoCheckMode) Descriptor() types.Descriptor { return noCheckModeDescriptor }

// Check, Apply and Verify are never reached: the module must be refused at
// registration. They refuse loudly rather than pretending to work, so a test that
// accidentally registered it would not silently pass.
func (m NoCheckMode) Check(context.Context, map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m NoCheckMode) Apply(context.Context, map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m NoCheckMode) Verify(context.Context, map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
