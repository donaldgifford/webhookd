// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/observability"
)

// ProviderFactory builds a Provider from the system-level dependencies
// every provider needs at construction time. Each integration package
// registers exactly one factory at `init()` time keyed by the
// provider's Name(), per ADR-0010. main.go calls
// `Registry.Build(deps)` to resolve `cfg.EnabledProviders` into
// concrete providers — there is no longer a hardcoded construction
// site that knows about each provider individually.
//
// The deps struct is intentionally narrow: factories that need
// provider-specific config slice into `deps.Config.JSM`,
// `deps.Config.GitHub`, etc. themselves. Metrics is optional; nil is
// honored by every provider's nil-safe metric helpers.
type ProviderFactory func(deps ProviderDeps) (Provider, error)

// ProviderDeps is the construction-time dependency bundle handed to
// every ProviderFactory. Adding a field is a registry-wide migration
// because every factory has to accept the new shape — keep this
// minimal.
type ProviderDeps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Metrics *observability.Metrics
}

// Registry maps provider names to their factories. The zero value is
// not usable; construct via NewRegistry. Goroutine-safe — protected by
// an internal mutex so concurrent init() registration is safe.
type Registry struct {
	mu        sync.Mutex
	providers map[string]ProviderFactory
}

// NewRegistry returns an empty Registry. Tests use this to opt out of
// the package-level DefaultRegistry and exercise a single integration
// in isolation.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]ProviderFactory{}}
}

// DefaultRegistry is the package-level registry that init()-registered
// providers populate. Production wiring resolves through here; tests
// that want isolation construct their own via NewRegistry.
//
//nolint:gochecknoglobals // Standard registry pattern; see ADR-0010.
var DefaultRegistry = NewRegistry()

// RegisterProvider registers a factory in the DefaultRegistry.
// Calling from an integration package's init() is the production
// pattern (ADR-0010 §Decision). Duplicate registrations panic at
// startup — silent override would be worse than a clear failure.
func RegisterProvider(name string, factory ProviderFactory) {
	DefaultRegistry.Register(name, factory)
}

// Register adds a factory to this Registry. See RegisterProvider for
// the package-level convenience wrapper.
func (r *Registry) Register(name string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[name]; dup {
		panic(fmt.Sprintf("webhook: provider %q registered twice", name))
	}
	r.providers[name] = factory
}

// Build resolves cfg.EnabledProviders against r and constructs each
// via its factory in the order they appear in the allow-list. Returns
// an error if any enabled provider isn't registered, or if any
// factory itself returns an error. Producing a deterministic ordering
// matters because the dispatcher uses the first-registered name for
// routing.
func (r *Registry) Build(deps ProviderDeps) ([]Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	enabled := deps.Config.EnabledProviders
	out := make([]Provider, 0, len(enabled))
	for _, name := range enabled {
		f, ok := r.providers[name]
		if !ok {
			return nil, fmt.Errorf("webhook: provider %q enabled but not registered", name)
		}
		prov, err := f(deps)
		if err != nil {
			return nil, fmt.Errorf("webhook: provider %q factory: %w", name, err)
		}
		out = append(out, prov)
	}
	return out, nil
}
