package skills

import (
	"context"

	"github.com/trouble-agent/trouble/internal/types"
)

// Skills is the composed package surface: one store, one puller, one promoter,
// one resolver and the registry hook. The composition root (cmd/troubled, owned
// by SPEC-12) builds it once at boot.
type Skills struct {
	Cfg        types.SkillsConfig
	Deps       Deps
	Store      *Store
	Puller     *Puller
	Promoter   *Promoter
	Resolver   *Resolver
	Authorizer *Authorizer
}

// New builds the package: config validation (so a credential-bearing source URL
// or an unknown approve policy is a boot rejection), the local store, the boot
// re-verification pass (§3.5) and the four collaborators.
func New(cfg types.SkillsConfig, deps Deps) (*Skills, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if deps.Clock == nil {
		deps.Clock = &systemClock{}
	}
	store, err := newStore(cfg, deps.StateRoot, deps.hostID(), deps.daemonVersion(), deps.Clock.Now)
	if err != nil {
		return nil, err
	}
	s := &Skills{
		Cfg:        cfg,
		Deps:       deps,
		Store:      store,
		Promoter:   NewPromoter(cfg, store, deps),
		Resolver:   NewResolver(cfg, store, deps),
		Authorizer: NewAuthorizer(cfg, store, deps),
	}
	puller, err := NewPuller(cfg, store, deps)
	if err != nil {
		return nil, err
	}
	s.Puller = puller
	// Boot re-verification: a tampered installed artifact is quarantined and
	// reported, never executed (§3.5). It never fails the boot.
	if _, err := puller.Reverify(context.Background()); err != nil {
		_, _ = deps.phaseRecord(context.Background(), PhaseRefused, "", "", map[string]any{
			"error_code": string(types.CodeSkills002), "reason": ReasonTampered,
			"count": 1, "detail": err.Error(),
		})
	}
	return s, nil
}

// Run is the pull loop (one goroutine: single-flight inside PullOnce).
func (s *Skills) Run(ctx context.Context) { s.Puller.Run(ctx) }

// PullOnce is one pull.
func (s *Skills) PullOnce(ctx context.Context) (pullReport, error) { return s.Puller.PullOnce(ctx) }

// Status is `trouble skills status --json`.
func (s *Skills) Status() types.SkillsStatus {
	return s.Store.Status(s.Puller.State())
}

// Close flushes local stats (§3.6: the crash-loss window is ≤ 5 s).
func (s *Skills) Close() error { return s.Store.Flush() }
