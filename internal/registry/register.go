package registry

import (
	"github.com/trouble-agent/trouble/internal/types"
)

// CapabilityProbeModule is the reserved, non-dispatchable audit subject of
// Registry.PolicyCheck (SPEC-06 §4.4).
const CapabilityProbeModule = "registry.capability-probe"

// shippedModules is the ONLY registration site for the v0.1 module set
// (SPEC-06 §3.8). A tool outside this table cannot be named at all.
func shippedModules() []types.Module {
	return []types.Module{
		configGetModule{}, configSetModule{}, configListModule{},
		serviceStatusModule{}, serviceReloadModule{}, serviceRestartModule{},
		fileReadModule{}, filePatchModule{},
		procTopModule{}, procConnectionsModule{},
		flowFileIssueModule{}, flowCreateTaskModule{}, flowCommentModule{},
	}
}

// ShippedDescriptors returns the descriptors of the shipped module set without
// building a Registry — the schema generator and the CLI inventory use it.
func ShippedDescriptors() []types.Descriptor {
	mods := shippedModules()
	out := make([]types.Descriptor, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.Descriptor())
	}
	return out
}

// ShippedModules exposes the shipped set (schema generation and conformance).
func ShippedModules() []types.Module { return shippedModules() }

func shippedByName() map[string]types.Descriptor {
	out := map[string]types.Descriptor{}
	for _, d := range ShippedDescriptors() {
		out[d.Name] = d
	}
	return out
}

// moduleEnv is everything a shipped module needs from the daemon: the resolved
// config, the wired collaborators and the registry itself (for replay-guard
// lookups). It is passed through the package-private envBinder extension, so the
// frozen types.Module surface never widens (SPEC-06 §2.1).
type moduleEnv struct {
	cfg  config
	deps RegistryDeps
	reg  *Registry
}

// envBinder is the package-private binding extension every shipped module
// implements.
type envBinder interface {
	bind(*moduleEnv)
}

// bindEnv wires the environment into the module set built by NewWith.
func bindEnv(mods []types.Module, env *moduleEnv) []types.Module {
	for _, m := range mods {
		if b, ok := m.(envBinder); ok {
			b.bind(env)
		}
	}
	return mods
}
