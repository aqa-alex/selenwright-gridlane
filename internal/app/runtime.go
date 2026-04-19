package app

import (
	"fmt"

	"gridlane/internal/auth"
	"gridlane/internal/catalog"
	routerconfig "gridlane/internal/config"
	"gridlane/internal/health"
	"gridlane/internal/observe"
	"gridlane/internal/proxy"
)

// Runtime bundles the validated, immutable configuration artifacts produced
// from a single router.json load. It is re-created wholesale by
// ReloadingHandler on SIGHUP so in-flight requests observe a consistent view.
type Runtime struct {
	Catalog          *catalog.Catalog
	Auth             *auth.Policy
	Health           *health.Manager
	ProxyCredentials proxy.CredentialStore
	Metrics          *observe.Metrics
}

// NewRuntime loads the strict router.json, builds the catalog, resolves auth
// and backend credential secrets, and primes the health manager. Metrics is
// intentionally left nil — ReloadingHandler assigns its shared metrics
// instance on the next line so reload preserves counter continuity.
func NewRuntime(opts Options) (Runtime, error) {
	cfg, err := routerconfig.LoadFile(opts.ConfigPath)
	if err != nil {
		return Runtime{}, fmt.Errorf("load config: %w", err)
	}
	cat, err := catalog.New(cfg)
	if err != nil {
		return Runtime{}, fmt.Errorf("build catalog: %w", err)
	}
	policy, err := auth.NewPolicy(cfg, auth.EnvFileResolver{})
	if err != nil {
		return Runtime{}, fmt.Errorf("build auth policy: %w", err)
	}
	backendCredentials, err := proxy.NewCredentialStore(cfg.BackendPools, auth.EnvFileResolver{})
	if err != nil {
		return Runtime{}, fmt.Errorf("build backend credential store: %w", err)
	}
	return Runtime{
		Catalog:          cat,
		Auth:             policy,
		Health:           health.NewManager(cfg.BackendPools),
		ProxyCredentials: backendCredentials,
	}, nil
}
